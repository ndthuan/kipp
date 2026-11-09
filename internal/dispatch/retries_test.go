package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ndthuan/kipp/internal/runnerhub"
	"github.com/ndthuan/kipp/internal/store"
)

// flaky is a command that fails with code until it has run times times,
// counting its runs in a file of its directory.
func flaky(times int, code int) string {
	return "n=$(cat runs 2>/dev/null || echo 0); n=$((n+1)); echo $n > runs; echo run $n; [ $n -ge " + strconv.Itoa(times) + " ] || exit " + strconv.Itoa(code)
}

func tryLines(log string) int { return strings.Count(log, "kipp: try ") }

func TestAStageThatFailsTwiceThenSucceedsEndsSuccessWithThreeTries(t *testing.T) {
	h := newHarness(t, map[string]string{"r": "name: r\nstages:\n  s: {run: '" + flaky(3, 7) + "', retries: 2}\n  t: {needs: [s], run: echo after}\n"})
	b := h.wait(t, h.trigger(t, "r").ID)
	st := h.stages(t, b.ID)
	log := h.log(t, b, "s")
	if b.Status != store.Success || st["s"].Status != store.StageSuccess || st["s"].Tries != 3 {
		t.Fatalf("build %s, stage %+v\n%s", b.Status, st["s"], log)
	}
	want := "run 1\nkipp: try 2 of 3\nrun 2\nkipp: try 3 of 3\nrun 3\n"
	if log != want {
		t.Fatalf("log %q, want %q", log, want)
	}
	if st["s"].ExitCode == nil || *st["s"].ExitCode != 0 {
		t.Fatalf("exit code %v is the last try's", st["s"].ExitCode)
	}
	if st["t"].Tries != 1 {
		t.Fatalf("a stage that ran once: %d tries", st["t"].Tries)
	}
}

func TestAStageThatAlwaysFailsRunsRetriesPlusOneTimesAndEndsWithTheLastExitCode(t *testing.T) {
	h := newHarness(t, map[string]string{"r": "name: r\nstages:\n  s: {run: 'n=$(cat runs 2>/dev/null || echo 0); n=$((n+1)); echo $n > runs; exit $((10+n))', retries: 2}\n"})
	b := h.wait(t, h.trigger(t, "r").ID)
	s := h.stages(t, b.ID)["s"]
	if b.Status != store.Failed || s.Status != store.StageFailed || s.Tries != 3 || s.ExitCode == nil || *s.ExitCode != 13 {
		t.Fatalf("build %s, stage %+v", b.Status, s)
	}
	if got := tryLines(h.log(t, b, "s")); got != 2 {
		t.Fatalf("%d try lines, want 2", got)
	}
}

func TestAStageWithNoRetriesRunsOnceAndSaysNothingOfTries(t *testing.T) {
	h := newHarness(t, map[string]string{"r": "name: r\nstages:\n  s: {run: 'echo once; exit 3'}\n"})
	b := h.wait(t, h.trigger(t, "r").ID)
	if s := h.stages(t, b.ID)["s"]; s.Tries != 1 || s.Status != store.StageFailed {
		t.Fatalf("%+v", s)
	}
	if got := h.log(t, b, "s"); got != "once\n" {
		t.Fatalf("log %q", got)
	}
}

func TestAnUnstableExitCodeIsNotRetried(t *testing.T) {
	h := newHarness(t, map[string]string{"r": "name: r\nstages:\n  s: {run: 'echo ran; exit 4', unstable_exit: [4], retries: 3}\n"})
	b := h.wait(t, h.trigger(t, "r").ID)
	if s := h.stages(t, b.ID)["s"]; s.Status != store.StageUnstable || s.Tries != 1 {
		t.Fatalf("%+v", s)
	}
	if got := h.log(t, b, "s"); got != "ran\n" {
		t.Fatalf("log %q", got)
	}
}

func TestACancelDuringATryStartsNoOther(t *testing.T) {
	runs := filepath.Join(t.TempDir(), "runs")
	h := newHarness(t, map[string]string{"r": "name: r\nstages:\n  s: {run: 'echo x >> " + runs + "; sleep 30', retries: 3}\n"})
	b := h.trigger(t, "r")
	// The shell creates the file before it writes the line, and a cancel
	// between the two would leave the try unrecorded.
	eventually(t, "the first try began", func() bool { data, _ := os.ReadFile(filepath.Clean(runs)); return len(data) > 0 })
	if err := h.d.Cancel(context.Background(), b.ID, "ada"); err != nil {
		t.Fatal(err)
	}
	// The build is not busy once it ends: no try can start after Wait.
	b = h.wait(t, b.ID)
	data, _ := os.ReadFile(filepath.Clean(runs))
	if b.Status != store.Cancelled || strings.Count(string(data), "x") != 1 {
		t.Fatalf("build %s, runs %q", b.Status, data)
	}
}

func TestAStageOnAnRunnerIsRetriedByTheRunnerAndSaysHowManyTries(t *testing.T) {
	ag := newRunners(t, runnerhub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
	ag.runBox(t.TempDir())
	h := newHarness(t, map[string]string{"r": "name: r\nstages:\n  s: {runs_on: linux, run: '" + flaky(3, 7) + "', retries: 2}\n"}, ag.opts)
	b := h.wait(t, h.trigger(t, "r").ID)
	s := h.stages(t, b.ID)["s"]
	log := h.log(t, b, "s")
	if b.Status != store.Success || s.Tries != 3 {
		t.Fatalf("build %s, stage %+v\n%s", b.Status, s, log)
	}
	if want := "run 1\nkipp: try 2 of 3\nrun 2\nkipp: try 3 of 3\nrun 3\n"; log != want {
		t.Fatalf("log %q, want %q", log, want)
	}
}

func TestAStageOnAnRunnerThatAlwaysFailsEndsWithTheLastExitCode(t *testing.T) {
	ag := newRunners(t, runnerhub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
	ag.runBox(t.TempDir())
	h := newHarness(t, map[string]string{"r": "name: r\nstages:\n  s: {runs_on: linux, run: 'n=$(cat runs 2>/dev/null || echo 0); n=$((n+1)); echo $n > runs; exit $((10+n))', retries: 1}\n"}, ag.opts)
	b := h.wait(t, h.trigger(t, "r").ID)
	s := h.stages(t, b.ID)["s"]
	if s.Status != store.StageFailed || s.Tries != 2 || s.ExitCode == nil || *s.ExitCode != 12 {
		t.Fatalf("%+v", s)
	}
}

func TestAnUnstableExitCodeOnAnRunnerStageWithNoCacheEndsUnstableAndIsNotRetried(t *testing.T) {
	ag := newRunners(t, runnerhub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
	ag.runBox(t.TempDir())
	h := newHarness(t, map[string]string{"r": "name: r\nstages:\n  s: {runs_on: linux, run: 'echo ran; exit 4', unstable_exit: [4], retries: 3}\n"}, ag.opts)
	b := h.wait(t, h.trigger(t, "r").ID)
	s := h.stages(t, b.ID)["s"]
	if s.Status != store.StageUnstable || s.Tries != 1 || h.log(t, b, "s") != "ran\n" {
		t.Fatalf("%+v\n%s", s, h.log(t, b, "s"))
	}
}

func TestACancelDuringATryOnAnRunnerStartsNoOther(t *testing.T) {
	ag := newRunners(t, runnerhub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
	ag.runBox(t.TempDir())
	runs := filepath.Join(t.TempDir(), "runs")
	h := newHarness(t, map[string]string{"r": "name: r\nstages:\n  s: {runs_on: linux, run: 'echo x >> " + runs + "; sleep 30', retries: 3}\n"}, ag.opts)
	b := h.trigger(t, "r")
	eventually(t, "the first try began", func() bool { _, err := os.Stat(runs); return err == nil })
	if err := h.d.Cancel(context.Background(), b.ID, "ada"); err != nil {
		t.Fatal(err)
	}
	b = h.wait(t, b.ID)
	data, _ := os.ReadFile(filepath.Clean(runs))
	if b.Status != store.Cancelled || strings.Count(string(data), "x") != 1 {
		t.Fatalf("build %s, runs %q", b.Status, data)
	}
}
