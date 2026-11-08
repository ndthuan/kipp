// Command flakes turns the output of go test -json, from runs that repeat
// and shuffle a suite, into a Markdown table of the tests that failed: the
// flake-hunt workflow appends it to the job summary and goes red when the
// table has a row.
//
// go test -json does not say which -cpu value a result belongs to, so each
// -cpu value is run apart and its output is named with it: flakes
// -summary "$GITHUB_STEP_SUMMARY" 1=cpu1.json 4=cpu4.json.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"os"
	"sort"
	"strings"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		if _, ok := errors.AsType[failures](err); !ok {
			fmt.Fprintln(os.Stderr, "flakes:", err)
		}
		os.Exit(1)
	}
}

// failures is the error of a run in which a test failed; its table is
// already written.
type failures int

func (n failures) Error() string { return fmt.Sprintf("%d failing tests", int(n)) }

func run(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("flakes", flag.ContinueOnError)
	summaryPath := fs.String("summary", "", "a Markdown file the table is appended to")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("usage: flakes [-summary file] <cpu>=<file of go test -json output> and more")
	}
	var all []finding
	for _, arg := range fs.Args() {
		cpu, path, ok := strings.Cut(arg, "=")
		if !ok || cpu == "" || path == "" {
			return fmt.Errorf("%q is not <cpu>=<file>", arg)
		}
		found, err := readFile(path, cpu)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		all = append(all, found...)
	}
	sortFindings(all)
	md := render(all)
	if _, err := io.WriteString(stdout, md); err != nil {
		return err
	}
	if *summaryPath != "" {
		if err := appendFile(*summaryPath, md); err != nil {
			return err
		}
	}
	if len(all) > 0 {
		return failures(len(all))
	}
	return nil
}

func readFile(path, cpu string) ([]finding, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return parse(f, cpu)
}

func appendFile(path, s string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.WriteString(f, s)
	return errors.Join(err, f.Close())
}

// event is one line of go test -json.
type event struct {
	Action      string
	Package     string
	Test        string
	Output      string
	ImportPath  string
	FailedBuild string
}

// finding is one failing test, or one package that failed without a test
// to blame, under one -cpu value.
type finding struct {
	pkg, test, cpu string
	failed, runs   int
	seed           string
	excerpt        []string
}

type tally struct {
	failed, runs int
	excerpt      []string
	current      []string
}

type pkgState struct {
	seed        string
	tests       map[string]*tally
	failed      int
	failedBuild bool
	build       []string
	output      []string
}

func (p *pkgState) test(name string) *tally {
	t := p.tests[name]
	if t == nil {
		t = &tally{}
		p.tests[name] = t
	}
	return t
}

// parse reads go test -json output of one -cpu value.
func parse(r io.Reader, cpu string) ([]finding, error) {
	dec := json.NewDecoder(bufio.NewReaderSize(r, 1<<20))
	pkgs := packages{}
	seen := 0
	for {
		var ev event
		if err := dec.Decode(&ev); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("not go test -json output: %w", err)
		}
		seen++
		pkgs.add(ev)
	}
	if seen == 0 {
		return nil, errors.New("no go test -json output: the run did not start")
	}
	var out []finding
	for name, p := range pkgs {
		out = append(out, p.findings(name, cpu)...)
	}
	return out, nil
}

type packages map[string]*pkgState

func (pkgs packages) state(name string) *pkgState {
	p := pkgs[name]
	if p == nil {
		p = &pkgState{tests: map[string]*tally{}}
		pkgs[name] = p
	}
	return p
}

func (pkgs packages) add(ev event) {
	switch {
	case ev.Action == "build-output":
		// Build events name an import path of the form "pkg.test" or
		// "pkg [pkg.test]", not a package.
		p := pkgs.state(buildPackage(ev.ImportPath))
		p.build = append(p.build, lines(ev.Output)...)
	case ev.Action == "build-fail":
		pkgs.state(buildPackage(ev.ImportPath))
	case ev.Package != "":
		apply(pkgs.state(ev.Package), ev)
	}
}

func buildPackage(importPath string) string {
	name, _, _ := strings.Cut(importPath, " ")
	return strings.TrimSuffix(name, ".test")
}

func apply(p *pkgState, ev event) {
	if ev.Test == "" {
		applyPackage(p, ev)
		return
	}
	t := p.test(ev.Test)
	switch ev.Action {
	case "run":
		t.current = nil
	case "output":
		if !isFrame(ev.Output) {
			t.current = append(t.current, lines(ev.Output)...)
		}
	case "pass":
		t.runs++
	case "fail":
		t.runs++
		t.failed++
		if t.excerpt == nil {
			t.excerpt = t.current
		}
	}
}

func applyPackage(p *pkgState, ev event) {
	switch ev.Action {
	case "output":
		if seed, ok := strings.CutPrefix(ev.Output, "-test.shuffle "); ok {
			p.seed = strings.TrimSpace(seed)
			return
		}
		p.output = append(p.output, lines(ev.Output)...)
	case "fail":
		p.failed++
		p.failedBuild = p.failedBuild || ev.FailedBuild != ""
	}
}

// isFrame reports a line go test prints around a test rather than from it.
func isFrame(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "=== ") || strings.HasPrefix(s, "--- ")
}

func lines(s string) []string {
	var out []string
	for l := range strings.SplitSeq(s, "\n") {
		if l = strings.TrimRight(l, " \t\r"); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func (p *pkgState) findings(pkg, cpu string) []finding {
	var failing []string
	for name, t := range p.tests {
		if t.failed > 0 {
			failing = append(failing, name)
		}
	}
	var out []finding
	for _, name := range failing {
		// A parent fails when a subtest does; the subtest is the finding.
		if hasFailingChild(name, failing) {
			continue
		}
		t := p.tests[name]
		out = append(out, finding{pkg: pkg, test: name, cpu: cpu, failed: t.failed, runs: t.runs, seed: p.seed, excerpt: t.excerpt})
	}
	if len(out) == 0 && p.failed > 0 {
		out = append(out, p.packageFinding(pkg, cpu))
	}
	return out
}

func hasFailingChild(name string, failing []string) bool {
	for _, other := range failing {
		if strings.HasPrefix(other, name+"/") {
			return true
		}
	}
	return false
}

// packageFinding is a package that failed with no failing test: it did not
// build, or the binary died or exited outside any test.
func (p *pkgState) packageFinding(pkg, cpu string) finding {
	f := finding{pkg: pkg, test: "(package)", cpu: cpu, failed: p.failed, runs: p.failed, seed: p.seed, excerpt: p.output}
	if p.failedBuild || len(p.build) > 0 {
		f.test, f.excerpt = "(build)", p.build
	}
	return f
}

func sortFindings(fs []finding) {
	sort.Slice(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if a.pkg != b.pkg {
			return a.pkg < b.pkg
		}
		if a.test != b.test {
			return a.test < b.test
		}
		return a.cpu < b.cpu
	})
}

const (
	excerptLines = 6
	excerptWidth = 160
)

func render(fs []finding) string {
	var b strings.Builder
	b.WriteString("## Flake hunt\n\n")
	if len(fs) == 0 {
		b.WriteString("No test failed.\n\n")
		return b.String()
	}
	b.WriteString("| Package | Test | Verdict | Failed runs | -cpu | Shuffle seed | First lines of the failure |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- | --- |\n")
	for _, f := range fs {
		verdict := "flaky"
		if f.failed == f.runs {
			verdict = "fails every run"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %d of %d | %s | %s | %s |\n",
			cell(f.pkg), cell(f.test), verdict, f.failed, f.runs, cell(f.cpu), cell(f.seed), excerptCell(f.excerpt))
	}
	b.WriteString("\nA test listed here is fixed at its root, never retried.\n\n")
	return b.String()
}

func excerptCell(ex []string) string {
	var parts []string
	for _, l := range ex[:min(len(ex), excerptLines)] {
		if len(l) > excerptWidth {
			l = l[:excerptWidth] + "..."
		}
		parts = append(parts, cell(strings.TrimSpace(l)))
	}
	return strings.Join(parts, "<br>")
}

// cell makes s safe inside a table cell: no pipe ends it and no tag opens.
func cell(s string) string {
	return strings.ReplaceAll(html.EscapeString(s), "|", "&#124;")
}
