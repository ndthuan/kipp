package dispatch

import (
	"context"
	"strings"
	"testing"

	"github.com/ndthuan/kipp/internal/agenthub"
	"github.com/ndthuan/kipp/internal/store"
)

// junitRun writes a JUnit file with the given number of failed cases and one
// that passed, then exits with code.
func junitRun(failed int, code string) string {
	xml := `<testsuite name="g"><testcase name="ok"/>`
	for range failed {
		xml += `<testcase name="bad"><failure message="no"/></testcase>`
	}
	xml += `</testsuite>`
	return "mkdir -p r; echo '" + xml + "' > r/x.xml; exit " + code
}

func testsFailedJob(run, extra string) string {
	return "name: t\nstages:\n  a:\n    run: |\n      " + run + "\n    junit: r/*.xml\n" + extra +
		"  b: {needs: [a], run: 'echo after-a'}\n"
}

func TestTestsFailedDecidesAStageEndFromItsJunitReport(t *testing.T) {
	cases := []struct {
		name, run, extra string
		want             store.StageStatus
		buildWant        store.Status
		line             string
	}{
		{"success with a failed case ends unstable", junitRun(2, "0"), "    tests_failed: unstable\n",
			store.StageUnstable, store.Unstable, "kipp: 2 failed test cases, the stage ends unstable (tests_failed)"},
		{"success with a failed case ends failed", junitRun(1, "0"), "    tests_failed: failed\n",
			store.StageFailed, store.Failed, "kipp: 1 failed test case, the stage ends failed (tests_failed)"},
		{"unstable stays unstable", junitRun(1, "4"), "    unstable_exit: [4]\n    tests_failed: unstable\n",
			store.StageUnstable, store.Unstable, "kipp: 1 failed test case, the stage ends unstable (tests_failed)"},
		{"unstable becomes failed", junitRun(1, "4"), "    unstable_exit: [4]\n    tests_failed: failed\n",
			store.StageFailed, store.Failed, "the stage ends failed (tests_failed)"},
		{"no failed case changes nothing", junitRun(0, "0"), "    tests_failed: failed\n",
			store.StageSuccess, store.Success, ""},
		{"a stage that failed is unchanged", junitRun(1, "1"), "    tests_failed: unstable\n",
			store.StageFailed, store.Failed, ""},
		{"absent key changes nothing", junitRun(3, "0"), "",
			store.StageSuccess, store.Success, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, map[string]string{"t": testsFailedJob(c.run, c.extra)})
			b := h.wait(t, h.trigger(t, "t").ID)
			a := h.stages(t, b.ID)["a"]
			if a.Status != c.want || b.Status != c.buildWant {
				t.Fatalf("stage %s build %s: %s", a.Status, b.Status, h.log(t, b, "a"))
			}
			if log := h.log(t, b, "a"); c.line != "" && !strings.Contains(log, c.line) {
				t.Fatalf("no %q in %q", c.line, log)
			} else if c.line == "" && strings.Contains(log, "tests_failed") {
				t.Fatalf("a line about tests_failed: %q", log)
			}
		})
	}
}

// What follows a stage sees the status tests_failed gave it.
func TestWhatNeedsAStageSeesTheStatusTestsFailedGaveIt(t *testing.T) {
	h := newHarness(t, map[string]string{"t": `name: t
stages:
  a:
    run: |
      ` + junitRun(1, "0") + `
    junit: r/*.xml
    tests_failed: failed
  b: {needs: [a], when: {needs_status: failure}, run: 'echo saw-failed'}
`})
	b := h.wait(t, h.trigger(t, "t").ID)
	if st := h.stages(t, b.ID); st["a"].Status != store.StageFailed || st["b"].Status != store.StageSuccess {
		t.Fatalf("a %s b %s", st["a"].Status, st["b"].Status)
	}
}

func TestTestsFailedReadsTheReportOfAStageOnAnAgent(t *testing.T) {
	ag := newAgents(t, agenthub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
	ag.runBox(t.TempDir())
	h := newHarness(t, map[string]string{"t": `name: t
stages:
  s:
    runs_on: linux
    workdir: src
    junit: "r/*.xml"
    tests_failed: unstable
    run: |
      mkdir -p r
      echo '<testsuite name="s"><testcase name="bad"><failure message="no"/></testcase></testsuite>' > r/t.xml
`}, ag.opts)
	b := h.wait(t, h.trigger(t, "t").ID)
	s := h.stages(t, b.ID)["s"]
	if s.Status != store.StageUnstable || b.Status != store.Unstable {
		t.Fatalf("stage %s build %s: %s", s.Status, b.Status, h.log(t, b, "s"))
	}
	if tests, err := h.st.Tests(context.Background(), b.ID); err != nil || len(tests) != 1 {
		t.Fatalf("%v %+v", err, tests)
	}
}
