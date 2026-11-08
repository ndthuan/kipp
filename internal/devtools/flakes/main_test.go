package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stream builds go test -json output from events.
func stream(t *testing.T, evs ...event) string {
	t.Helper()
	var b strings.Builder
	for _, ev := range evs {
		line, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// runOf is one run of a test: its start, what it printed and its result.
func runOf(test, result string, out ...string) []event {
	const pkg = "k/a"
	evs := []event{{Action: "run", Package: pkg, Test: test}}
	for _, o := range out {
		evs = append(evs, event{Action: "output", Package: pkg, Test: test, Output: o})
	}
	return append(evs, event{Action: "output", Package: pkg, Test: test, Output: "--- " + strings.ToUpper(result) + ": " + test + " (0.00s)\n"},
		event{Action: result, Package: pkg, Test: test})
}

func seed(s string) event {
	const pkg = "k/a"
	return event{Action: "output", Package: pkg, Output: "-test.shuffle " + s + "\n"}
}

func join(parts ...[]event) []event {
	var all []event
	for _, p := range parts {
		all = append(all, p...)
	}
	return all
}

func parseStream(t *testing.T, evs []event) []finding {
	t.Helper()
	got, err := parse(strings.NewReader(stream(t, evs...)), "4")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestACleanRunHasNoFindingAndSaysSo(t *testing.T) {
	evs := join([]event{seed("1")}, runOf("TestA", "pass"), runOf("TestA", "pass"),
		[]event{{Action: "pass", Package: "k/a"}})
	if got := parseStream(t, evs); len(got) != 0 {
		t.Fatalf("findings in a clean run: %+v", got)
	}
	if md := render(nil); !strings.Contains(md, "No test failed") {
		t.Fatalf("a clean summary does not say so: %q", md)
	}
}

func TestATestThatFailsSometimesIsFlakyWithTheSeedAndTheFirstFailure(t *testing.T) {
	evs := join([]event{seed("4242")},
		runOf("TestA", "pass"),
		runOf("TestA", "fail", "    a_test.go:9: got 409, want 503\n"),
		runOf("TestA", "pass"),
		runOf("TestA", "fail", "    a_test.go:9: a later failure\n"),
		[]event{{Action: "fail", Package: "k/a"}})
	got := parseStream(t, evs)
	if len(got) != 1 {
		t.Fatalf("findings = %+v, want one", got)
	}
	f := got[0]
	if f.pkg != "k/a" || f.test != "TestA" || f.cpu != "4" || f.failed != 2 || f.runs != 4 || f.seed != "4242" {
		t.Errorf("finding = %+v", f)
	}
	if len(f.excerpt) != 1 || !strings.Contains(f.excerpt[0], "got 409, want 503") {
		t.Errorf("excerpt = %q, want the first failure's lines without the frames", f.excerpt)
	}
	md := render(got)
	for _, want := range []string{"| k/a | TestA | flaky | 2 of 4 | 4 | 4242 |", "got 409, want 503"} {
		if !strings.Contains(md, want) {
			t.Errorf("summary lacks %q:\n%s", want, md)
		}
	}
}

func TestATestThatFailsEveryRunIsReportedAsFailingNotFlaky(t *testing.T) {
	evs := join([]event{seed("7")},
		runOf("TestA", "fail", "    a_test.go:3: always\n"),
		runOf("TestA", "fail", "    a_test.go:3: always\n"),
		[]event{{Action: "fail", Package: "k/a"}})
	md := render(parseStream(t, evs))
	if !strings.Contains(md, "fails every run | 2 of 2") || strings.Contains(md, "flaky") {
		t.Fatalf("summary:\n%s", md)
	}
}

func TestAFailingSubtestIsTheFindingNotItsParent(t *testing.T) {
	evs := join([]event{seed("1")},
		runOf("TestA/sub", "fail", "    a_test.go:3: boom\n"),
		runOf("TestA", "fail"),
		runOf("TestA/other", "pass"),
		[]event{{Action: "fail", Package: "k/a"}})
	got := parseStream(t, evs)
	if len(got) != 1 || got[0].test != "TestA/sub" {
		t.Fatalf("findings = %+v, want TestA/sub alone", got)
	}
}

func TestAPanicNamesTheTestThatPanickedAndItsFirstLines(t *testing.T) {
	evs := join([]event{seed("99")},
		runOf("TestOK", "pass"),
		runOf("TestP", "fail",
			"panic: assignment to entry in nil map [recovered, repanicked]\n", "\n", "goroutine 8 [running]:\n"),
		[]event{{Action: "fail", Package: "k/a"}})
	got := parseStream(t, evs)
	if len(got) != 1 || got[0].test != "TestP" || got[0].seed != "99" {
		t.Fatalf("findings = %+v, want TestP", got)
	}
	if len(got[0].excerpt) != 2 || !strings.HasPrefix(got[0].excerpt[0], "panic: assignment") {
		t.Errorf("excerpt = %q, want the panic line and the goroutine line", got[0].excerpt)
	}
}

func TestAPackageThatDoesNotBuildIsReportedWithTheCompilerOutput(t *testing.T) {
	evs := []event{
		{Action: "build-output", ImportPath: "k/b.test", Output: "# k/b\n"},
		{Action: "build-output", ImportPath: "k/b.test", Output: "b/b_test.go:3:33: expected operand, found 'EOF'\n"},
		{Action: "build-fail", ImportPath: "k/b.test"},
		{Action: "start", Package: "k/b"},
		{Action: "output", Package: "k/b", Output: "FAIL\tk/b [setup failed]\n"},
		{Action: "fail", Package: "k/b", FailedBuild: "k/b.test"},
	}
	got := parseStream(t, evs)
	if len(got) != 1 || got[0].pkg != "k/b" || got[0].test != "(build)" {
		t.Fatalf("findings = %+v, want a build failure of k/b", got)
	}
	if md := render(got); !strings.Contains(md, "expected operand") {
		t.Errorf("summary lacks the compiler's line:\n%s", md)
	}
}

func TestAPackageThatDiesOutsideATestIsReportedAsThePackage(t *testing.T) {
	evs := join([]event{seed("5"), {Action: "output", Package: "k/a", Output: "FAIL\tk/a\t1.0s\n"}},
		[]event{{Action: "fail", Package: "k/a"}})
	got := parseStream(t, evs)
	if len(got) != 1 || got[0].test != "(package)" {
		t.Fatalf("findings = %+v, want the package", got)
	}
}

func TestATableCellCannotBreakTheTable(t *testing.T) {
	f := finding{pkg: "k/a", test: "TestA", cpu: "1", failed: 1, runs: 2, excerpt: []string{"a | b <script>"}}
	row := strings.Split(render([]finding{f}), "\n")[4]
	if strings.Count(row, "|") != 8 || strings.Contains(row, "<script>") {
		t.Fatalf("row = %q", row)
	}
}

func TestOutputThatIsNotGoTestJSONIsRefused(t *testing.T) {
	for name, in := range map[string]string{"empty": "", "text": "ok  \tk/a\t0.1s\n"} {
		if _, err := parse(strings.NewReader(in), "1"); err == nil {
			t.Errorf("%s: parsed without an error", name)
		}
	}
}

func TestRunWritesTheSummaryAndFailsWhenATestFailed(t *testing.T) {
	dir := t.TempDir()
	clean := filepath.Join(dir, "clean.json")
	bad := filepath.Join(dir, "bad.json")
	summary := filepath.Join(dir, "summary.md")
	write := func(path string, evs ...event) {
		if err := os.WriteFile(path, []byte(stream(t, evs...)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(clean, join(runOf("TestA", "pass"), []event{{Action: "pass", Package: "k/a"}})...)
	write(bad, join(runOf("TestA", "fail", "x\n"), []event{{Action: "fail", Package: "k/a"}})...)

	var out strings.Builder
	if err := run([]string{"-summary", summary, "1=" + clean}, &out); err != nil {
		t.Fatalf("clean run: %v", err)
	}
	out.Reset()
	err := run([]string{"-summary", summary, "1=" + clean, "4=" + bad}, &out)
	if _, ok := errors.AsType[failures](err); !ok {
		t.Fatalf("err = %v, want failures", err)
	}
	got, rerr := os.ReadFile(summary)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if strings.Count(string(got), "## Flake hunt") != 2 || !strings.Contains(string(got), "| k/a | TestA |") {
		t.Errorf("summary:\n%s", got)
	}
	if err := run([]string{"nonsense"}, &out); err == nil {
		t.Error("an argument without a cpu value was accepted")
	}
}
