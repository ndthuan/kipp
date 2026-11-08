package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/agenthub"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/metrics"
	"github.com/ndthuan/kipp/internal/store"
)

// remembered is what the store remembers of one metric, item to value.
func (h *harness) remembered(t *testing.T, storeName, metric string) map[string]float64 {
	t.Helper()
	rs, err := h.st.RememberedValues(context.Background(), storeName, metric)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, r := range rs {
		out[r.Item] = r.Value
	}
	return out
}

// reported is what the build reported of one metric, item to value.
func (h *harness) reported(t *testing.T, id int64, metric string) map[string]float64 {
	t.Helper()
	rows, err := h.st.BuildMetrics(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, r := range rows {
		if r.Metric == metric {
			out[r.Item] = r.Value
		}
	}
	return out
}

func (h *harness) push(t *testing.T, job, branch string) *store.Build {
	t.Helper()
	b, err := h.d.Trigger(context.Background(), TriggerRequest{Job: job, Kind: "webhook", Detail: "abc123", Branch: branch, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	return h.wait(t, b.ID)
}

// readBaseline reads the KIPP_METRICS a stage copied to baseline.json in dir.
func readBaseline(t *testing.T, dir string) metrics.File {
	t.Helper()
	data, err := fs.ReadFile(os.DirFS(dir), "baseline.json")
	if err != nil {
		t.Fatal(err)
	}
	var f metrics.File
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("%v: %s", err, data)
	}
	return f
}

// report is a file of one metric in kipp's report format, for a stage to
// copy where its metrics.file finds it.
func report(t *testing.T, name, unit string, complete bool, values map[string]float64) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{"metrics": []any{map[string]any{"name": name, "unit": unit, "complete": complete, "values": values}}})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// copyTo is a stage's run that copies a report to name.
func copyTo(report, name string) string { return "cp " + report + " " + name }

func TestAStageSeesWhatItsStoreRemembersInKippMetrics(t *testing.T) {
	out := t.TempDir()
	h := newHarness(t, map[string]string{"site-smoke": `name: site-smoke
metrics: {store: site}
stages:
  plan:
    metrics: {}
    run: 'cp "$KIPP_METRICS" ` + out + `/baseline.json && test -x "$KIPP_BIN" && echo "bin $KIPP_BIN"'
  other: {run: 'echo "[${KIPP_METRICS-unset}]"'}
`})
	at := time.Date(2026, 10, 29, 3, 0, 0, 0, time.UTC)
	if err := h.st.Remember(context.Background(), "site", store.Origin{BuildID: 7, Job: "site-deploy", Number: 123}, at,
		[]store.Metric{{Name: "coverage", Unit: "%", Values: map[string]float64{"total": 87.19}}}); err != nil {
		t.Fatal(err)
	}
	b := h.wait(t, h.trigger(t, "site-smoke").ID)
	if b.Status != store.Success {
		t.Fatalf("%s: %s", b.Status, h.log(t, b, "plan"))
	}
	f := readBaseline(t, out)
	if f.Store != "site" || len(f.Metrics) != 1 || f.Metrics[0].Values["total"] != 87.19 || f.Metrics[0].Unit != "%" || len(f.Guards) != 0 {
		t.Fatalf("%+v", f)
	}
	if from := f.Metrics[0].From["total"]; from.Job != "site-deploy" || from.Number != 123 || !from.At.Equal(at) {
		t.Fatalf("%+v", from)
	}
	if exe, _ := os.Executable(); !strings.Contains(h.log(t, b, "plan"), "bin "+exe) {
		t.Fatalf("KIPP_BIN is not kipp's own executable: %q", h.log(t, b, "plan"))
	}
	if got := h.log(t, b, "other"); got != "[unset]\n" {
		t.Fatalf("a stage that declares no metrics sees %q", got)
	}
}

func TestAStageOnAnAgentSeesKippMetricsAndItsReportComesBack(t *testing.T) {
	ag := newAgents(t, agenthub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
	ag.runBox(t.TempDir())
	h := newHarness(t, map[string]string{"site-smoke": `name: site-smoke
metrics: {store: site}
stages:
  measure:
    runs_on: linux
    workdir: src
    metrics: {file: "out/metrics-*.json"}
    run: |
      test -x "$KIPP_BIN" && echo "bin ok"
      cat "$KIPP_METRICS"
      mkdir -p out
      ` + copyTo(report(t, "seconds", "s", true, map[string]float64{"a": 1.5, "b": 2}), "out/metrics-1.json") + `
  local:
    metrics: {file: m.json}
    run: ` + copyTo(report(t, "coverage", "%", false, map[string]float64{"total": 90}), "m.json") + `
`}, ag.opts)
	if err := h.st.Remember(context.Background(), "site", store.Origin{Job: "cli"}, time.Now(),
		[]store.Metric{{Name: "seconds", Values: map[string]float64{"a": 3}}}); err != nil {
		t.Fatal(err)
	}
	b := h.wait(t, h.trigger(t, "site-smoke").ID)
	log := h.log(t, b, "measure")
	if b.Status != store.Success || !strings.Contains(log, "bin ok") || !strings.Contains(log, `"store":"site"`) || !strings.Contains(log, `"values":{"a":3}`) {
		t.Fatalf("%s: %s", b.Status, log)
	}
	if got := h.reported(t, b.ID, "seconds"); got["a"] != 1.5 || got["b"] != 2 || len(got) != 2 {
		t.Fatalf("the agent's report: %v (%s)", got, log)
	}
	if got := h.reported(t, b.ID, "coverage"); got["total"] != 90 {
		t.Fatalf("the server's report: %v (%s)", got, h.log(t, b, "local"))
	}
	if got := h.remembered(t, "site", "seconds"); len(got) != 2 || got["a"] != 1.5 {
		t.Fatalf("remembered %v", got)
	}
}

func TestEveryBuildRecordsHowLongEachStageTook(t *testing.T) {
	h := newHarness(t, map[string]string{"site-smoke": "name: site-smoke\nstages:\n  slow: {run: sleep 0.3}\n  quick: {needs: [slow], run: 'true'}\n  never: {needs: [quick], when: {triggers: [cron]}, run: 'true'}\n"})
	b := h.wait(t, h.trigger(t, "site-smoke").ID)
	got := h.reported(t, b.ID, config.StageSeconds)
	if len(got) != 2 || got["slow"] < 0.25 || got["slow"] > 10 || got["quick"] >= got["slow"] {
		t.Fatalf("%v", got)
	}
	if rows, _ := h.st.BuildMetrics(context.Background(), b.ID); rows[0].Unit != "s" || rows[0].Store != "site-smoke" {
		t.Fatalf("%+v", rows)
	}
	if got := h.remembered(t, "site-smoke", config.StageSeconds); len(got) != 2 {
		t.Fatalf("remembered %v", got)
	}
}

func TestEveryJunitCaseIsAlsoACaseSecondsMetric(t *testing.T) {
	h := newHarness(t, map[string]string{"site-smoke": `name: site-smoke
stages:
  check:
    junit: "r/*.xml"
    run: |
      mkdir -p r
      echo '<testsuite name="site"><testcase name="home loads" time="1.5"/><testcase name="later" time="2"><skipped/></testcase></testsuite>' > r/a.xml
`})
	b := h.wait(t, h.trigger(t, "site-smoke").ID)
	if got := h.reported(t, b.ID, config.CaseSeconds); len(got) != 1 || got["site / home loads"] != 1.5 {
		t.Fatalf("%v", got)
	}
	if got := h.remembered(t, "site-smoke", config.CaseSeconds); got["site / home loads"] != 1.5 {
		t.Fatalf("%v", got)
	}
}

func TestABranchBuildGoesIntoHistoryAndIsNotRemembered(t *testing.T) {
	h := newHarness(t, map[string]string{"site-smoke": "name: site-smoke\nstages:\n  s: {metrics: {file: m.json}, run: " +
		copyTo(report(t, "coverage", "%", true, map[string]float64{"total": 80}), "m.json") + "}\n"})
	b := h.push(t, "site-smoke", "fix-1")
	if got := h.reported(t, b.ID, "coverage"); got["total"] != 80 {
		t.Fatalf("%s: %v %s", b.Status, got, h.log(t, b, "s"))
	}
	if got := h.remembered(t, "site-smoke", ""); len(got) != 0 {
		t.Fatalf("a branch build was remembered: %v", got)
	}
	main := h.push(t, "site-smoke", "main")
	if got := h.remembered(t, "site-smoke", "coverage"); got["total"] != 80 || main.Status != store.Success {
		t.Fatalf("%v", got)
	}
}

func TestACancelledBuildRemembersNothing(t *testing.T) {
	gate, open := gated(t)
	h := newHarness(t, map[string]string{"site-smoke": `name: site-smoke
metrics:
  remember: [{when: {status: [success, unstable, failed]}}]
stages:
  a: {metrics: {file: m.json}, run: ` + copyTo(report(t, "seconds", "s", false, map[string]float64{"a": 1}), "m.json") + `}
  b: {needs: [a], run: "` + gate + `"}
`})
	defer open()
	b := h.trigger(t, "site-smoke")
	eventually(t, "b runs", func() bool { s := h.stages(t, b.ID)["b"]; return s != nil && s.Status == store.StageRunning })
	if err := h.d.Cancel(context.Background(), b.ID, "someone"); err != nil {
		t.Fatal(err)
	}
	if b = h.wait(t, b.ID); b.Status != store.Cancelled {
		t.Fatal(b.Status)
	}
	if got := h.reported(t, b.ID, "seconds"); got["a"] != 1 {
		t.Fatalf("the report of the stage that ran is not in history: %v", got)
	}
	if got := h.remembered(t, "site-smoke", ""); len(got) != 0 {
		t.Fatalf("a cancelled build was remembered: %v", got)
	}
}

func TestACompleteReportOfABuildThatDoesNotRememberForgetsNothing(t *testing.T) {
	h := newHarness(t, map[string]string{"site-smoke": "name: site-smoke\nstages:\n  s: {metrics: {file: m.json}, run: " +
		copyTo(report(t, "seconds", "s", true, map[string]float64{"a": 9}), "m.json") + "}\n"})
	if err := h.st.Remember(context.Background(), "site-smoke", store.Origin{Job: "cli"}, time.Now(),
		[]store.Metric{{Name: "seconds", Values: map[string]float64{"a": 1, "b": 2}}}); err != nil {
		t.Fatal(err)
	}
	h.push(t, "site-smoke", "fix-1")
	if got := h.remembered(t, "site-smoke", "seconds"); len(got) != 2 || got["a"] != 1 || got["b"] != 2 {
		t.Fatalf("%v", got)
	}
}

// Two jobs that share a store end at the same moment: each one's items
// are all there, and an item both report holds one of the two values.
func TestTwoJobsRememberIntoOneStoreAtOnce(t *testing.T) {
	gate, open := gated(t)
	job := func(name, prefix string, v float64) string {
		items := map[string]float64{"shared": v}
		for i := range 200 {
			items[fmt.Sprintf("%s%d", prefix, i)] = v
		}
		return "name: " + name + "\nmetrics: {store: site}\nstages:\n  s: {metrics: {file: m.json}, run: '" + gate + "; " +
			copyTo(report(t, "seconds", "s", false, items), "m.json") + "'}\n"
	}
	h := newHarness(t, map[string]string{"site-smoke": job("site-smoke", "a", 1), "site-deploy": job("site-deploy", "b", 2)})
	first, second := h.trigger(t, "site-smoke"), h.trigger(t, "site-deploy")
	h.untilRunning(t, first.ID)
	h.untilRunning(t, second.ID)
	open()
	first, second = h.wait(t, first.ID), h.wait(t, second.ID)
	if first.Status != store.Success || second.Status != store.Success {
		t.Fatalf("%s %s: %s", first.Status, second.Status, h.log(t, first, "s"))
	}
	got := h.remembered(t, "site", "seconds")
	if len(got) != 401 || (got["shared"] != 1 && got["shared"] != 2) || got["a0"] != 1 || got["b199"] != 2 {
		t.Fatalf("%d items, shared %v", len(got), got["shared"])
	}
	if got := h.remembered(t, "site", config.StageSeconds); len(got) != 1 {
		t.Fatalf("both jobs' stage s is one item: %v", got)
	}
}

func TestASecretInAnItemIsMasked(t *testing.T) {
	out := t.TempDir()
	h := newHarness(t, map[string]string{"site-smoke": `name: site-smoke
secrets: [TOKEN]
stages:
  measure:
    metrics: {file: m.json}
    junit: r.xml
    run: |
      printf '{"metrics": [{"name": "seconds", "unit": "%s", "values": {"call %s": 1}}]}' "$TOKEN" "$TOKEN" > m.json
      echo "<testsuite name=\"s\"><testcase name=\"with $TOKEN\" time=\"1\"/></testsuite>" > r.xml
  read: {needs: [measure], metrics: {}, run: 'cp "$KIPP_METRICS" ` + out + `/baseline.json'}
`})
	b := h.wait(t, h.trigger(t, "site-smoke").ID)
	if got := h.reported(t, b.ID, "seconds"); got["call ***"] != 1 || len(got) != 1 {
		t.Fatalf("history %v: %s", got, h.log(t, b, "measure"))
	}
	if got := h.reported(t, b.ID, config.CaseSeconds); got["s / with ***"] != 1 {
		t.Fatalf("history %v", got)
	}
	rs, err := h.st.RememberedValues(context.Background(), "site-smoke", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs {
		if strings.Contains(r.Item+r.Unit, "value-of-TOKEN") {
			t.Fatalf("remembered %+v", r)
		}
	}
	h.wait(t, h.trigger(t, "site-smoke").ID)
	data, err := fs.ReadFile(os.DirFS(out), "baseline.json")
	if err != nil || strings.Contains(string(data), "value-of-TOKEN") || !strings.Contains(string(data), "call ***") {
		t.Fatalf("%v: %s", err, data)
	}
}

func TestAMissingReportIsALineAndNotAFailure(t *testing.T) {
	h := newHarness(t, map[string]string{"site-smoke": `name: site-smoke
stages:
  a: {metrics: {file: "metrics-*.json"}, run: echo ran}
  b:
    metrics: {file: m.json}
    run: "echo '{' > m.json"
`})
	b := h.wait(t, h.trigger(t, "site-smoke").ID)
	if b.Status != store.Success {
		t.Fatal(b.Status)
	}
	if got := h.log(t, b, "a"); got != "ran\nkipp: metrics: metrics-*.json: matched nothing\n" {
		t.Fatalf("%q", got)
	}
	if got := h.log(t, b, "b"); !strings.Contains(got, "kipp: metrics: m.json: not a report") {
		t.Fatalf("%q", got)
	}
}
