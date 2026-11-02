package dispatch

import (
	"context"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/agenthub"
	"github.com/ndthuan/kipp/internal/metrics"
	"github.com/ndthuan/kipp/internal/store"
)

// TestMain makes this test binary answer as kipp does when a stage runs
// "$KIPP_BIN" metrics check: KIPP_BIN is the server's own executable,
// which in these tests is this binary.
func TestMain(m *testing.M) {
	// The test binary stands in for kipp inside a stage. It exits past the
	// coverage hooks of a -coverprofile build, which would print a warning
	// about GOCOVERDIR into the stage's log the tests read.
	if len(os.Args) > 2 && os.Args[1] == "metrics" && os.Args[2] == "check" {
		syscall.Exit(metrics.CheckCommand(os.Args[3:], os.Getenv, os.Stdout))
	}
	os.Exit(m.Run())
}

// rememberCoverage has the store site remember coverage, as from
// site-deploy #123.
func (h *harness) rememberCoverage(t *testing.T, metric string, values map[string]float64) {
	t.Helper()
	if err := h.st.Remember(context.Background(), "site", store.Origin{BuildID: 99, Job: "site-deploy", Number: 123}, time.Now(),
		[]store.Metric{{Name: metric, Unit: "%", Values: values}}); err != nil {
		t.Fatal(err)
	}
}

// guardedJob is site-smoke with one stage, measure, that copies the
// report to m.json and has guard, and a stage after it.
func guardedJob(t *testing.T, run, guard string) map[string]string {
	t.Helper()
	return map[string]string{"site-smoke": `name: site-smoke
metrics: {store: site}
stages:
  measure:
    metrics:
      file: m.json
      guard:
        - ` + guard + `
    run: |
      ` + run + `
  after: {needs: [measure], run: echo after}
`}
}

// guardLines are the guard's lines of the stage's log, in order.
func guardLines(log string) []string {
	var out []string
	for line := range strings.SplitSeq(log, "\n") {
		if strings.HasPrefix(line, metrics.LinePrefix) {
			out = append(out, line)
		}
	}
	return out
}

func coverage(t *testing.T, total float64) string {
	return copyTo(report(t, "coverage", "%", false, map[string]float64{"total": total}), "m.json")
}

func TestAFallBeyondToleranceFailsTheStage(t *testing.T) {
	h := newHarness(t, guardedJob(t, coverage(t, 86.90), "{metric: coverage, item: total, no_lower: true, tolerance: 0.01}"))
	h.rememberCoverage(t, "coverage", map[string]float64{"total": 87.19})
	b := h.wait(t, h.trigger(t, "site-smoke").ID)
	stages := h.stages(t, b.ID)
	log := h.log(t, b, "measure")
	if b.Status != store.Failed || stages["measure"].Status != store.StageFailed || *stages["measure"].ExitCode != 0 || stages["after"].Status != store.StageSkipped {
		t.Fatalf("%s %+v: %s", b.Status, stages["measure"], log)
	}
	want := "kipp: metrics guard: coverage total 87.19 -> 86.90 (-0.29), remembered from site-deploy #123, tolerance 0.01: failed"
	if got := guardLines(log); len(got) != 1 || got[0] != want {
		t.Fatalf("%q, want %q", got, want)
	}
	// A failed build does not remember what fell.
	if got := h.remembered(t, "site", "coverage"); got["total"] != 87.19 {
		t.Fatalf("remembered %v", got)
	}
}

func TestAFallWithinToleranceHolds(t *testing.T) {
	h := newHarness(t, guardedJob(t, coverage(t, 87.18), "{metric: coverage, item: total, no_lower: true, tolerance: 0.01}"))
	h.rememberCoverage(t, "coverage", map[string]float64{"total": 87.19})
	b := h.wait(t, h.trigger(t, "site-smoke").ID)
	log := h.log(t, b, "measure")
	want := "kipp: metrics guard: coverage total 87.19 -> 87.18 (-0.01), remembered from site-deploy #123, tolerance 0.01: held"
	if got := guardLines(log); b.Status != store.Success || len(got) != 1 || got[0] != want {
		t.Fatalf("%s %q: %s", b.Status, got, log)
	}
}

func TestOnFallUnstableEndsTheStageUnstableAndWhatNeedsItRuns(t *testing.T) {
	h := newHarness(t, guardedJob(t, coverage(t, 80), "{metric: coverage, item: total, no_lower: true, on_fall: unstable}"))
	h.rememberCoverage(t, "coverage", map[string]float64{"total": 87.19})
	b := h.wait(t, h.trigger(t, "site-smoke").ID)
	stages := h.stages(t, b.ID)
	if b.Status != store.Unstable || stages["measure"].Status != store.StageUnstable || stages["after"].Status != store.StageSuccess {
		t.Fatalf("%s %s %s: %s", b.Status, stages["measure"].Status, stages["after"].Status, h.log(t, b, "measure"))
	}
	if got := guardLines(h.log(t, b, "measure")); len(got) != 1 || !strings.HasSuffix(got[0], "(-7.19), remembered from site-deploy #123, tolerance 0: unstable") {
		t.Fatalf("%q", got)
	}
}

func TestAnItemWithNoBaselinePassesAndSaysSo(t *testing.T) {
	h := newHarness(t, guardedJob(t, coverage(t, 86.9), "{metric: coverage, item: total, no_lower: true}"))
	b := h.wait(t, h.trigger(t, "site-smoke").ID)
	want := "kipp: metrics guard: coverage total no baseline -> 86.90, tolerance 0: held"
	if got := guardLines(h.log(t, b, "measure")); b.Status != store.Success || len(got) != 1 || got[0] != want {
		t.Fatalf("%s %q", b.Status, got)
	}
}

func TestAGuardedItemTheReportLacksFailsAsMissing(t *testing.T) {
	run := copyTo(report(t, "coverage", "%", false, map[string]float64{"other": 90}), "m.json")
	h := newHarness(t, guardedJob(t, run, "{metric: coverage, item: total, no_lower: true, on_fall: unstable}"))
	h.rememberCoverage(t, "coverage", map[string]float64{"total": 87.19})
	b := h.wait(t, h.trigger(t, "site-smoke").ID)
	want := "kipp: metrics guard: coverage total 87.19 -> missing, remembered from site-deploy #123, tolerance 0: unstable"
	if got := guardLines(h.log(t, b, "measure")); b.Status != store.Unstable || len(got) != 1 || got[0] != want {
		t.Fatalf("%s %q", b.Status, got)
	}
}

func TestAMissingReportFileFailsEveryGuardOfTheStage(t *testing.T) {
	for name, run := range map[string]string{
		"no file":       "echo no report",
		"not parseable": "echo '{not json' > m.json",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, map[string]string{"site-smoke": `name: site-smoke
metrics: {store: site}
stages:
  measure:
    metrics:
      file: m.json
      guard:
        - {metric: coverage, item: total, no_lower: true}
        - {metric: seconds, item: "unit *", no_lower: true}
    run: ` + run + `
`})
			b := h.wait(t, h.trigger(t, "site-smoke").ID)
			want := []string{
				"kipp: metrics guard: coverage total no baseline -> missing, tolerance 0: failed",
				"kipp: metrics guard: seconds unit * no baseline -> missing, tolerance 0: failed",
			}
			if got := guardLines(h.log(t, b, "measure")); b.Status != store.Failed || !slices.Equal(got, want) {
				t.Fatalf("%s %q: %s", b.Status, got, h.log(t, b, "measure"))
			}
		})
	}
}

func TestAPatternGuardsEveryMatchingItem(t *testing.T) {
	run := copyTo(report(t, "covered_lines", "lines", false, map[string]float64{"lib/a.mjs": 10, "lib/b.mjs": 4, "lib/d.mjs": 3, "src/c.mjs": 0}), "m.json")
	h := newHarness(t, guardedJob(t, run, `{metric: covered_lines, item: "lib/*.mjs", no_lower: true}`))
	h.rememberCoverage(t, "covered_lines", map[string]float64{"lib/a.mjs": 10, "lib/b.mjs": 5, "lib/e.mjs": 1, "src/c.mjs": 1})
	b := h.wait(t, h.trigger(t, "site-smoke").ID)
	want := []string{
		"kipp: metrics guard: covered_lines lib/a.mjs 10.00 -> 10.00 (+0.00), remembered from site-deploy #123, tolerance 0: held",
		"kipp: metrics guard: covered_lines lib/b.mjs 5.00 -> 4.00 (-1.00), remembered from site-deploy #123, tolerance 0: failed",
		"kipp: metrics guard: covered_lines lib/d.mjs no baseline -> 3.00, tolerance 0: held",
		"kipp: metrics guard: covered_lines lib/e.mjs 1.00 -> missing, remembered from site-deploy #123, tolerance 0: failed",
	}
	if got := guardLines(h.log(t, b, "measure")); b.Status != store.Failed || !slices.Equal(got, want) {
		t.Fatalf("%s\n%s", b.Status, strings.Join(got, "\n"))
	}
	rows, err := h.st.GuardResults(context.Background(), b.ID)
	if err != nil || len(rows) != 4 || rows[1].Item != "lib/b.mjs" || *rows[1].Remembered != 5 || *rows[1].Reported != 4 ||
		rows[1].Verdict != metrics.Fell || rows[1].Outcome != "failed" || rows[1].FromJob != "site-deploy" || rows[1].FromNumber != 123 || rows[1].Stage != "measure" {
		t.Fatalf("%v %+v", err, rows)
	}
	if rows[2].Remembered != nil || rows[3].Reported != nil {
		t.Fatalf("a side with no value is kept as none: %+v %+v", rows[2], rows[3])
	}
}

func TestAStageThatAlreadyFailedIsNotGuarded(t *testing.T) {
	h := newHarness(t, guardedJob(t, coverage(t, 50)+"; exit 1", "{metric: coverage, item: total, no_lower: true, on_fall: unstable}"))
	h.rememberCoverage(t, "coverage", map[string]float64{"total": 87.19})
	b := h.wait(t, h.trigger(t, "site-smoke").ID)
	rows, err := h.st.GuardResults(context.Background(), b.ID)
	if got := guardLines(h.log(t, b, "measure")); b.Status != store.Failed || len(got) != 0 || err != nil || len(rows) != 0 {
		t.Fatalf("%s %q %v %v", b.Status, got, err, rows)
	}
	// It still reported.
	if got := h.reported(t, b.ID, "coverage"); got["total"] != 50 {
		t.Fatalf("%v", got)
	}
}

func TestRequiredFalsePassesAnUnreportedItem(t *testing.T) {
	h := newHarness(t, guardedJob(t, "echo nothing measured this time", "{metric: coverage, item: total, no_lower: true, required: false}"))
	h.rememberCoverage(t, "coverage", map[string]float64{"total": 87.19})
	b := h.wait(t, h.trigger(t, "site-smoke").ID)
	want := "kipp: metrics guard: coverage total 87.19 -> not reported, remembered from site-deploy #123, tolerance 0: held"
	if got := guardLines(h.log(t, b, "measure")); b.Status != store.Success || len(got) != 1 || got[0] != want {
		t.Fatalf("%s %q", b.Status, got)
	}
}

func TestKippMetricsCarriesTheStagesGuardsWithTheirDefaults(t *testing.T) {
	out := t.TempDir()
	h := newHarness(t, guardedJob(t, `cp "$KIPP_METRICS" `+out+`/baseline.json; `+coverage(t, 90), `{metric: coverage, item: "t*", no_lower: true, tolerance: 0.5}`))
	h.wait(t, h.trigger(t, "site-smoke").ID)
	f := readBaseline(t, out)
	want := metrics.FileGuard{Metric: "coverage", Item: "t*", NoLower: true, Tolerance: 0.5, OnFall: "failed", Required: true}
	if len(f.Guards) != 1 || f.Guards[0] != want {
		t.Fatalf("%+v", f.Guards)
	}
}

func TestMetricsCheckExitsOneOnAFallInsideTheStage(t *testing.T) {
	// A script that merges only when the check holds never merges.
	run := coverage(t, 86.90) + `
      if "$KIPP_BIN" metrics check m.json; then echo merged; else echo "check said $?, not merged"; fi`
	h := newHarness(t, guardedJob(t, run, "{metric: coverage, item: total, no_lower: true, tolerance: 0.01}"))
	h.rememberCoverage(t, "coverage", map[string]float64{"total": 87.19})
	b := h.wait(t, h.trigger(t, "site-smoke").ID)
	log := h.log(t, b, "measure")
	line := "kipp: metrics guard: coverage total 87.19 -> 86.90 (-0.29), remembered from site-deploy #123, tolerance 0.01: failed"
	if !strings.Contains(log, line+"\ncheck said 1, not merged\n") || slices.Contains(strings.Split(log, "\n"), "merged") {
		t.Fatalf("%s", log)
	}
	// The guard after the stage runs all the same.
	if got := guardLines(log); b.Status != store.Failed || !slices.Equal(got, []string{line, line}) {
		t.Fatalf("%s %q", b.Status, got)
	}
}

func TestMetricsCheckInsideAStageWithNoGuardExitsThree(t *testing.T) {
	h := newHarness(t, map[string]string{"site-smoke": `name: site-smoke
stages:
  measure:
    metrics: {}
    run: '"$KIPP_BIN" metrics check m.json; echo "check said $?"'
`})
	b := h.wait(t, h.trigger(t, "site-smoke").ID)
	if log := h.log(t, b, "measure"); b.Status != store.Success || !strings.HasSuffix(log, "the stage declares no guard\ncheck said 3\n") {
		t.Fatalf("%s: %s", b.Status, log)
	}
}

// verdictCase is a report and a guard, and how the build ends.
type verdictCase struct {
	name, values, guard string
	want                store.Status
}

func TestAStageThatCallsMetricsCheckSeesTheSameVerdictAsTheGuardAfterIt(t *testing.T) {
	ag := newAgents(t, agenthub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
	ag.runBox(t.TempDir())
	cases := []verdictCase{
		{"held", `{"total": 87.19, "lib/a.mjs": 3}`, `{metric: coverage, item: total, no_lower: true}`, store.Success},
		{"within tolerance", `{"total": 87.18}`, `{metric: coverage, item: total, no_lower: true, tolerance: 0.01}`, store.Success},
		{"fell", `{"total": 86.9}`, `{metric: coverage, item: total, no_lower: true, tolerance: 0.01}`, store.Failed},
		{"fell unstable", `{"total": 86.9}`, `{metric: coverage, item: total, no_lower: true, on_fall: unstable}`, store.Unstable},
		{"missing", `{"other": 1}`, `{metric: coverage, item: total, no_lower: true}`, store.Failed},
		{"not reported", `{"other": 1}`, `{metric: coverage, item: total, no_lower: true, required: false}`, store.Success},
		{"pattern", `{"lib/b.mjs": 1}`, `{metric: coverage, item: "lib/*", no_lower: true}`, store.Failed},
	}
	for where, runsOn := range map[string]string{"server": "", "agent": "runs_on: linux"} {
		for _, c := range cases {
			t.Run(where+"/"+c.name, func(t *testing.T) { sameVerdict(t, ag, runsOn, c) })
		}
	}
}

// sameVerdict runs c in a stage that calls metrics check and prints what
// it said, on the server or on the agent runsOn names, and compares that
// with the lines of the guard after the stage.
func sameVerdict(t *testing.T, ag *agents, runsOn string, c verdictCase) {
	t.Helper()
	h := newHarness(t, map[string]string{"site-smoke": `name: site-smoke
metrics: {store: site}
stages:
  measure:
    ` + runsOn + `
    metrics:
      file: m.json
      guard: [` + c.guard + `]
    run: |
      echo '{"metrics": [{"name": "coverage", "values": ` + c.values + `}]}' > m.json
      "$KIPP_BIN" metrics check "$PWD/m.json" > inside.txt
      echo "inside $?"
      sed 's/^/said: /' inside.txt
`}, ag.opts)
	h.rememberCoverage(t, "coverage", map[string]float64{"total": 87.19, "lib/a.mjs": 2})
	b := h.wait(t, h.trigger(t, "site-smoke").ID)
	log := h.log(t, b, "measure")
	var inside []string
	for line := range strings.SplitSeq(log, "\n") {
		if l, ok := strings.CutPrefix(line, "said: "); ok {
			inside = append(inside, l)
		}
	}
	after := guardLines(log)
	if b.Status != c.want || len(after) == 0 || !slices.Equal(inside, after) {
		t.Fatalf("%s, want %s\ninside %q\nafter  %q\n%s", b.Status, c.want, inside, after, log)
	}
	code := "inside 0\n"
	if c.want == store.Failed {
		code = "inside 1\n"
	}
	if !strings.Contains(log, code) {
		t.Fatalf("want %s: %s", code, log)
	}
}
