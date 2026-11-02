// Command coverage turns a Go coverage profile into a kipp metrics report
// and guards the total against a baseline, as a kipp stage's metrics guard
// with no_lower does. CI runs it; the kipp binary does not contain it.
//
//	coverage report -profile coverage.out -o coverage.json
//	coverage guard -base base.json -head coverage.json [-tolerance 0.1] [-summary file.md]
//
// The report is the metric coverage in %, one item per package (its import
// path without the module's) and the item total. guard prints one line in
// the words of a kipp guard, appends a table of what changed to -summary
// (GitHub's job summary), and exits 1 when the total fell further than the
// tolerance. A -base that is not there is no baseline, which passes.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
)

const (
	metricName = "coverage"
	totalItem  = "total"
)

func main() {
	if len(os.Args) < 2 {
		fail(errors.New("usage: coverage report|guard [flags]"))
	}
	switch os.Args[1] {
	case "report":
		fail(report(os.Args[2:]))
	case "guard":
		fail(guardCmd(os.Args[2:]))
	default:
		fail(fmt.Errorf("unknown command %q: report or guard", os.Args[1]))
	}
}

func fail(err error) {
	if err == nil {
		return
	}
	if _, ok := errors.AsType[errFell](err); !ok {
		fmt.Fprintln(os.Stderr, "coverage:", err)
	}
	os.Exit(1)
}

// errFell is a guard that failed; its line is already printed.
type errFell struct{}

func (errFell) Error() string { return "coverage fell" }

func report(args []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	profilePath := fs.String("profile", "coverage.out", "the coverage profile go test wrote")
	out := fs.String("o", "coverage.json", "where the kipp metrics report goes")
	module := fs.String("module", "github.com/ndthuan/kipp", "the module path, left out of each package's item")
	if err := fs.Parse(args); err != nil {
		return err
	}
	f, err := os.Open(*profilePath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	r, err := parseProfile(f, *module)
	if err != nil {
		return fmt.Errorf("%s: %w", *profilePath, err)
	}
	w, err := os.Create(*out)
	if err != nil {
		return err
	}
	return errors.Join(writeMetrics(w, r), w.Close())
}

func guardCmd(args []string) error {
	fs := flag.NewFlagSet("guard", flag.ContinueOnError)
	basePath := fs.String("base", "", "the report the head is held to; one that is not there is no baseline")
	headPath := fs.String("head", "coverage.json", "the report of this run")
	tolerance := fs.Float64("tolerance", 0.1, "how far the total may fall, in percentage points")
	summaryPath := fs.String("summary", "", "a Markdown file the table of changes is appended to")
	if err := fs.Parse(args); err != nil {
		return err
	}
	head, err := readMetrics(*headPath)
	if err != nil {
		return err
	}
	var base map[string]float64
	if *basePath != "" {
		if base, err = readMetrics(*basePath); errors.Is(err, os.ErrNotExist) {
			base, err = nil, nil
		}
		if err != nil {
			return err
		}
	}
	line, fell := guard(base, head, *tolerance)
	if _, err := io.WriteString(os.Stdout, line); err != nil {
		return err
	}
	if *summaryPath != "" {
		f, err := os.OpenFile(*summaryPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, err = io.WriteString(f, summary(base, head))
		if err := errors.Join(err, f.Close()); err != nil {
			return err
		}
	}
	if fell {
		return errFell{}
	}
	return nil
}

// counts is how many statements a package has and how many of them ran.
type counts struct{ stmts, covered int }

// parseProfile reads a profile go test -coverprofile wrote, by package. A
// block named by more than one line (go test merges profiles) counts once,
// covered when any of its lines ran it.
func parseProfile(r io.Reader, module string) (map[string]counts, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	if !sc.Scan() || !strings.HasPrefix(sc.Text(), "mode: ") {
		return nil, errors.New("not a coverage profile: no mode line")
	}
	type block struct{ stmts, hits int }
	blocks := map[string]block{}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		key, stmts, hits, err := parseBlock(line)
		if err != nil {
			return nil, err
		}
		b := blocks[key]
		b.stmts, b.hits = stmts, max(b.hits, hits)
		blocks[key] = b
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	out := map[string]counts{}
	for key, b := range blocks {
		file, _, _ := strings.Cut(key, ":")
		pkg := strings.TrimPrefix(strings.TrimPrefix(path.Dir(file), module), "/")
		c := out[pkg]
		c.stmts += b.stmts
		if b.hits > 0 {
			c.covered += b.stmts
		}
		out[pkg] = c
	}
	return out, nil
}

// parseBlock reads "file:start,end statements count".
func parseBlock(line string) (key string, stmts, hits int, err error) {
	f := strings.Fields(line)
	if len(f) != 3 || !strings.Contains(f[0], ":") {
		return "", 0, 0, fmt.Errorf("not a profile block: %q", line)
	}
	if stmts, err = strconv.Atoi(f[1]); err != nil {
		return "", 0, 0, fmt.Errorf("statements in %q: %w", line, err)
	}
	if hits, err = strconv.Atoi(f[2]); err != nil {
		return "", 0, 0, fmt.Errorf("count in %q: %w", line, err)
	}
	return f[0], stmts, hits, nil
}

func percent(c counts) float64 {
	if c.stmts == 0 {
		return 0
	}
	return math.Round(float64(c.covered)/float64(c.stmts)*10000) / 100
}

type metricsFile struct {
	Metrics []metric `json:"metrics"`
}

type metric struct {
	Name     string             `json:"name"`
	Unit     string             `json:"unit"`
	Complete bool               `json:"complete"`
	Values   map[string]float64 `json:"values"`
}

// writeMetrics writes the report in the shape a kipp stage's metrics file
// has, so that a kipp job can keep and guard it too.
func writeMetrics(w io.Writer, byPkg map[string]counts) error {
	values := map[string]float64{}
	var all counts
	for pkg, c := range byPkg {
		values[pkg] = percent(c)
		all.stmts += c.stmts
		all.covered += c.covered
	}
	values[totalItem] = percent(all)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(metricsFile{Metrics: []metric{{Name: metricName, Unit: "%", Complete: true, Values: values}}})
}

func readMetrics(file string) (map[string]float64, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var m metricsFile
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	for _, x := range m.Metrics {
		if x.Name == metricName {
			return x.Values, nil
		}
	}
	return nil, fmt.Errorf("%s: no metric %q", file, metricName)
}

// guard is the line a kipp guard would print of the total against the
// baseline, and whether it fell.
func guard(base, head map[string]float64, tolerance float64) (string, bool) {
	now, ok := head[totalItem]
	if !ok {
		return fmt.Sprintf("coverage %s: missing from the report, failed\n", totalItem), true
	}
	was, ok := base[totalItem]
	if !ok {
		return fmt.Sprintf("coverage %s %.2f: no baseline, passed\n", totalItem, now), false
	}
	fell := now < was-tolerance-1e-9
	verdict := "passed"
	if fell {
		verdict = "failed"
	}
	return fmt.Sprintf("coverage %s %.2f -> %.2f (%+.2f), tolerance %.2f: %s\n", totalItem, was, now, now-was, tolerance, verdict), fell
}

type change struct {
	item       string
	was, now   float64
	hasWas     bool
	hasNow     bool
	difference float64
}

// summary is a Markdown table of the total and of each package whose
// coverage changed, the largest fall first.
func summary(base, head map[string]float64) string {
	var rows []change
	for _, item := range unionKeys(base, head) {
		was, hasWas := base[item]
		now, hasNow := head[item]
		if item != totalItem && hasWas && hasNow && was == now {
			continue
		}
		rows = append(rows, change{item: item, was: was, now: now, hasWas: hasWas, hasNow: hasNow, difference: now - was})
	}
	slices.SortFunc(rows, func(a, b change) int {
		if a.item == totalItem || b.item == totalItem {
			return boolCmp(b.item == totalItem, a.item == totalItem)
		}
		if a.hasWas && a.hasNow && b.hasWas && b.hasNow && a.difference != b.difference {
			return cmpFloat(a.difference, b.difference)
		}
		return strings.Compare(a.item, b.item)
	})
	var b strings.Builder
	b.WriteString("### Coverage\n\n")
	if base == nil {
		b.WriteString("No baseline: the newest green run on main kept no report.\n\n")
	}
	b.WriteString("| Package | Baseline % | This run % | Change |\n|---|---:|---:|---:|\n")
	for _, r := range rows {
		b.WriteString("| " + r.item + " | " + cell(r.was, r.hasWas) + " | " + cell(r.now, r.hasNow) + " | " + changeCell(r) + " |\n")
	}
	return b.String()
}

func cell(v float64, ok bool) string {
	if !ok {
		return ""
	}
	return fmt.Sprintf("%.2f", v)
}

func changeCell(r change) string {
	switch {
	case !r.hasWas:
		return "new"
	case !r.hasNow:
		return "gone"
	}
	return fmt.Sprintf("%+.2f", r.difference)
}

func unionKeys(a, b map[string]float64) []string {
	var out []string
	for k := range a {
		out = append(out, k)
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

func boolCmp(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
