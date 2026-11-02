package metrics

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/config"
)

// kippMetrics writes a KIPP_METRICS file of the store site remembering
// coverage total 87.19 from site-deploy #123, with the guards, and gives
// its path.
func kippMetrics(t *testing.T, guards ...config.Guard) string {
	t.Helper()
	data, err := Baseline("site", []Remembered{{Metric: "coverage", Item: "total", Unit: "%", Value: 87.19,
		From: From{Job: "site-deploy", Number: 123, At: time.Now()}}}, guards)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "metrics-measure.json")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func check(t *testing.T, kippMetrics string, args ...string) (int, string) {
	t.Helper()
	var out bytes.Buffer
	code := CheckCommand(args, func(k string) string {
		if k == "KIPP_METRICS" {
			return kippMetrics
		}
		return ""
	}, &out)
	return code, out.String()
}

var totalGuard = config.Guard{Metric: "coverage", Item: "total", NoLower: true, Tolerance: 0.01, OnFall: "failed"}

func TestMetricsCheckExitsOneWhenAGuardFailsAndPrintsTheLine(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "metrics-coverage.json", `{"metrics": [{"name": "coverage", "unit": "%", "values": {"total": 86.90}}]}`)
	code, out := check(t, kippMetrics(t, totalGuard), filepath.Join(dir, "metrics-*.json"))
	want := "kipp: metrics guard: coverage total 87.19 -> 86.90 (-0.29), remembered from site-deploy #123, tolerance 0.01: failed\n"
	if code != CheckFailed || out != want {
		t.Fatalf("%d %q", code, out)
	}
}

func TestMetricsCheckExitsZeroOnAnUnstableFallAndSaysIt(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "m.json", `{"metrics": [{"name": "coverage", "values": {"total": 80}}]}`)
	g := totalGuard
	g.OnFall = "unstable"
	t.Chdir(dir)
	if code, out := check(t, kippMetrics(t, g), "m.json"); code != CheckHeld || !strings.HasSuffix(out, ": unstable\n") {
		t.Fatalf("%d %q", code, out)
	}
}

func TestMetricsCheckExitsTwoWithoutKippMetrics(t *testing.T) {
	code, out := check(t, "", "m.json")
	if code != CheckNoMetrics || !strings.Contains(out, "KIPP_METRICS is not set") {
		t.Fatalf("%d %q", code, out)
	}
	if code, out = check(t, filepath.Join(t.TempDir(), "gone.json"), "m.json"); code != CheckNoMetrics || !strings.Contains(out, "KIPP_METRICS") {
		t.Fatalf("an unreadable one: %d %q", code, out)
	}
	if code, out = check(t, kippMetrics(t, totalGuard)); code != CheckNoMetrics || !strings.Contains(out, "usage: kipp metrics check <glob>") {
		t.Fatalf("no glob: %d %q", code, out)
	}
}

func TestMetricsCheckExitsThreeWithNoGuard(t *testing.T) {
	if code, out := check(t, kippMetrics(t), "m.json"); code != CheckNoGuard || out != "kipp: metrics check: the stage declares no guard\n" {
		t.Fatalf("%d %q", code, out)
	}
}

func TestMetricsCheckWithNoReportFailsAsMissing(t *testing.T) {
	code, out := check(t, kippMetrics(t, totalGuard), filepath.Join(t.TempDir(), "m.json"))
	if code != CheckFailed || !strings.Contains(out, "kipp: metrics: m.json: matched nothing\n") ||
		!strings.HasSuffix(out, "kipp: metrics guard: coverage total 87.19 -> missing, remembered from site-deploy #123, tolerance 0.01: failed\n") {
		t.Fatalf("%d %q", code, out)
	}
}

func TestTheGuardLineSaysEachSide(t *testing.T) {
	from := &From{Job: "site-deploy", Number: 123}
	for _, c := range []struct {
		r    GuardResult
		want string
	}{
		{GuardResult{Metric: "coverage", Item: "total", Before: new(87.19), After: new(86.9), Tolerance: 0.01, Outcome: "failed", Verdict: Fell, From: from},
			"kipp: metrics guard: coverage total 87.19 -> 86.90 (-0.29), remembered from site-deploy #123, tolerance 0.01: failed"},
		{GuardResult{Metric: "coverage", Item: "total", Before: new(80.0), After: new(81.5), Outcome: "held", Verdict: Held, From: from},
			"kipp: metrics guard: coverage total 80.00 -> 81.50 (+1.50), remembered from site-deploy #123, tolerance 0: held"},
		// Two decimals would say the change is none.
		{GuardResult{Metric: "coverage", Item: "total", Before: new(87.19), After: new(87.188), Outcome: "failed", Verdict: Fell, From: from},
			"kipp: metrics guard: coverage total 87.190 -> 87.188 (-0.002), remembered from site-deploy #123, tolerance 0: failed"},
		{GuardResult{Metric: "coverage", Item: "total", After: new(86.9), Outcome: "held", Verdict: NoBaseline},
			"kipp: metrics guard: coverage total no baseline -> 86.90, tolerance 0: held"},
		{GuardResult{Metric: "coverage", Item: "total", Before: new(87.19), Outcome: "failed", Verdict: Missing, From: from},
			"kipp: metrics guard: coverage total 87.19 -> missing, remembered from site-deploy #123, tolerance 0: failed"},
		{GuardResult{Metric: "coverage", Item: "total", Outcome: "held", Verdict: NotReported},
			"kipp: metrics guard: coverage total no baseline -> not reported, tolerance 0: held"},
		// A baseline written by hand that names no build.
		{GuardResult{Metric: "coverage", Item: "total", Before: new(1.0), After: new(1.0), Outcome: "held", Verdict: Held},
			"kipp: metrics guard: coverage total 1.00 -> 1.00 (+0.00), tolerance 0: held"},
	} {
		if got := c.r.Line(); got != c.want {
			t.Errorf("got  %s\nwant %s", got, c.want)
		}
	}
}

func TestAFallIsBelowTheRememberedValueLessTheTolerance(t *testing.T) {
	base := File{Metrics: []FileMetric{{Name: "coverage", Values: map[string]float64{"total": 87.19}}}}
	for _, c := range []struct {
		value, tolerance float64
		want             string
	}{
		{87.19, 0, Held}, {87.2, 0, Held}, {87.18, 0, Fell},
		{87.18, 0.01, Held}, {87.17, 0.01, Fell},
	} {
		g := FileGuard{Metric: "coverage", Item: "total", NoLower: true, Tolerance: c.tolerance, Required: true}
		rs := Check(g, []Metric{{Name: "coverage", Values: map[string]float64{"total": c.value}}}, base)
		if len(rs) != 1 || rs[0].Verdict != c.want {
			t.Errorf("%v tolerance %v: %+v, want %s", c.value, c.tolerance, rs, c.want)
		}
	}
	if !fell(0.3, 0.1+0.2-1e-6, 0) || fell(0.3, 0.1+0.2, 0) {
		t.Fatal("rounding of a sum is not a fall, a real difference is")
	}
}

func TestAPatternThatMatchesNoItemIsOneMissingResult(t *testing.T) {
	g := FileGuard{Metric: "coverage", Item: "lib/*", NoLower: true, Required: true}
	rs := Check(g, []Metric{{Name: "coverage", Values: map[string]float64{"src/a": 1}}}, File{})
	if len(rs) != 1 || rs[0].Item != "lib/*" || rs[0].Verdict != Missing || rs[0].Outcome != OutcomeFailed {
		t.Fatalf("%+v", rs)
	}
}

func TestTheWorstOutcomeIsFailedThenUnstable(t *testing.T) {
	r := func(o string) GuardResult { return GuardResult{Outcome: o} }
	for _, c := range []struct {
		rs   []GuardResult
		want string
	}{
		{nil, OutcomeHeld},
		{[]GuardResult{r(OutcomeHeld), r(OutcomeUnstable)}, OutcomeUnstable},
		{[]GuardResult{r(OutcomeUnstable), r(OutcomeFailed), r(OutcomeHeld)}, OutcomeFailed},
	} {
		if got := Worst(c.rs); got != c.want {
			t.Errorf("%v: %s, want %s", c.rs, got, c.want)
		}
	}
}

func TestARelativeGlobIsReadFromTheWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "out/m-1.json", `{"metrics": [{"name": "coverage", "values": {"total": 90}}]}`)
	t.Chdir(dir)
	for _, glob := range []string{"out/m-*.json", "./out/m-*.json"} {
		code, out := check(t, kippMetrics(t, totalGuard), glob)
		if code != CheckHeld || !slices.Contains(strings.Split(out, "\n"),
			"kipp: metrics guard: coverage total 87.19 -> 90.00 (+2.81), remembered from site-deploy #123, tolerance 0.01: held") {
			t.Fatalf("%s: %d %q", glob, code, out)
		}
	}
}
