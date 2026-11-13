package dispatch

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/runnerhub"
	"github.com/ndthuan/kipp/internal/store"
)

// The built-in runner is the server's own executors: its stages and every
// build's prepare count against its limit, and nothing counts builds across
// runners.

// quickPlaces looks at a stage that waits for a place often enough to keep
// the tests short.
func quickPlaces(t *testing.T) {
	t.Helper()
	old := placeEvery
	placeEvery = 20 * time.Millisecond
	t.Cleanup(func() { placeEvery = old })
}

// idle is a runner that is known to the hub and not online: a stage that
// asks for it waits.
func idleRunner(t *testing.T) *runners {
	t.Helper()
	return newRunners(t, runnerhub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
}

func (h *harness) queue(t *testing.T) *Queue {
	t.Helper()
	q, err := h.d.Queue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return q
}

// waitingOf is what the stages of the running build wait for, if it runs.
func (h *harness) waitingOf(t *testing.T, id int64) map[string]string {
	t.Helper()
	for _, r := range h.queue(t).Running {
		if r.Build.ID == id {
			return r.Waiting
		}
	}
	return nil
}

// builtInBusy is how many of the built-in runner's executors are taken.
func (h *harness) builtInBusy() int {
	busy, _ := h.d.local.load()
	return busy
}

// untilLocalStageRuns waits until the stage of a build runs on the server
// and holds one of its executors, its prepare's given back.
func (h *harness) untilLocalStageRuns(t *testing.T, id int64, stage string, busy int) {
	t.Helper()
	eventually(t, "the stage of the build runs on the server", func() bool {
		s := h.stages(t, id)[stage]
		return s != nil && s.Status == store.StageRunning && h.builtInBusy() == busy
	})
}

func TestAStageOnARunnerStartsWhileTheBuiltInRunnerIsFull(t *testing.T) {
	quickPlaces(t)
	ag := idleRunner(t)
	hold, open := gated(t)
	h := newHarness(t, map[string]string{
		"remote": "name: remote\nstages:\n  s: {runs_on: linux, run: echo on-the-runner}\n",
		"hold":   "name: hold\nstages:\n  s: {run: '" + hold + "'}\n",
	}, ag.opts, func(o *Options) { o.Executors = 1 })
	remote := h.trigger(t, "remote")
	eventually(t, "the remote stage waits for the runner, holding no executor", func() bool {
		return len(h.waitingOf(t, remote.ID)) == 1 && h.builtInBusy() == 0
	})
	holder := h.trigger(t, "hold")
	h.untilLocalStageRuns(t, holder.ID, "s", 1)
	ag.runBox(t.TempDir())
	if b := h.wait(t, remote.ID); b.Status != store.Success {
		t.Fatalf("the stage on the runner: %s", b.Status)
	}
	open()
	h.wait(t, holder.ID)
}

func TestAPrepareWaitsWhileTheBuiltInRunnerIsFull(t *testing.T) {
	hold, open := gated(t)
	h := newHarness(t, map[string]string{
		"hold":  "name: hold\nstages:\n  s: {run: '" + hold + "'}\n",
		"other": "name: other\nstages:\n  s: {run: 'true'}\n",
	}, func(o *Options) { o.Executors = 1 })
	holder := h.trigger(t, "hold")
	h.untilLocalStageRuns(t, holder.ID, "s", 1)
	other := h.trigger(t, "other")
	eventually(t, "the other build waits for the built-in runner", func() bool {
		q := h.queue(t)
		return len(q.Queued) == 1 && q.Queued[0].Build.ID == other.ID && q.Queued[0].Waiting == WaitExecutor
	})
	open()
	if b := h.wait(t, other.ID); b.Status != store.Success {
		t.Fatalf("%s", b.Status)
	}
	h.wait(t, holder.ID)
	if n := h.builtInBusy(); n != 0 {
		t.Fatalf("%d executors still taken", n)
	}
}

// Many builds whose stages wait for a runner hold nothing of the server's
// once prepared, so the server's own jobs still start.
func TestBuildsThatOnlyWaitForARunnerLeaveTheBuiltInRunnerFree(t *testing.T) {
	quickPlaces(t)
	ag := idleRunner(t)
	h := newHarness(t, map[string]string{
		"par":   "name: par\nconcurrency: parallel\nstages:\n  s: {runs_on: linux, run: echo remote}\n",
		"local": "name: local\nstages:\n  s: {run: echo local}\n",
	}, ag.opts, func(o *Options) { o.Executors = 1 })
	var par []*store.Build
	for range 3 {
		par = append(par, h.trigger(t, "par"))
	}
	eventually(t, "the three builds wait for the runner", func() bool {
		waiting := 0
		for _, b := range par {
			if len(h.waitingOf(t, b.ID)) == 1 {
				waiting++
			}
		}
		return waiting == 3 && h.builtInBusy() == 0
	})
	if b := h.wait(t, h.trigger(t, "local").ID); b.Status != store.Success {
		t.Fatalf("%s", b.Status)
	}
	ag.runBox(t.TempDir())
	for _, b := range par {
		if got := h.wait(t, b.ID); got.Status != store.Success {
			t.Fatalf("%s", got.Status)
		}
	}
}

// A started build whose stage waits for the built-in runner gets the first
// executor that frees, not a build queued behind it.
func TestAStageWaitingForTheBuiltInRunnerIsServedBeforeQueuedBuilds(t *testing.T) {
	first, openFirst := gated(t)
	second, openSecond := gated(t)
	// Whichever stage runs first waits on the first gate, the other on the
	// second: they run one at a time on the one executor.
	run := fmt.Sprintf("if mkdir %s 2>/dev/null; then %s; else %s; fi", filepath.Join(t.TempDir(), "first"), first, second)
	h := newHarness(t, map[string]string{
		"two":   "name: two\nstages:\n  x: {run: '" + run + "'}\n  y: {run: '" + run + "'}\n",
		"quick": "name: quick\nconcurrency: parallel\nstages:\n  s: {run: 'true'}\n",
	}, func(o *Options) { o.Executors = 1 })
	two := h.trigger(t, "two")
	eventually(t, "one stage runs and the other waits for the executor", func() bool {
		st := h.stages(t, two.ID)
		running := 0
		for _, s := range st {
			if s.Status == store.StageRunning {
				running++
			}
		}
		return len(st) == 2 && running == 1 && h.d.local.waiting() == 1
	})
	var quick []*store.Build
	for range 3 {
		quick = append(quick, h.trigger(t, "quick"))
	}
	openFirst()
	eventually(t, "the first stage ended and the second runs on the executor", func() bool {
		st := h.stages(t, two.ID)
		x, y := st["x"].Status, st["y"].Status
		return ((x == store.StageSuccess && y == store.StageRunning) || (y == store.StageSuccess && x == store.StageRunning)) && h.builtInBusy() == 1
	})
	if q := h.queue(t); len(q.Queued) != len(quick) {
		t.Fatalf("%d builds queued, want %d: the stage was passed over", len(q.Queued), len(quick))
	}
	openSecond()
	h.wait(t, two.ID)
	for _, b := range quick {
		h.wait(t, b.ID)
	}
}

// What a build holds besides an executor, its place among the running builds
// of its job and its locks, it keeps while its stage waits for a runner.
func TestABuildWaitingForARunnerKeepsItsPlaceAndItsLocks(t *testing.T) {
	quickPlaces(t)
	ag := idleRunner(t)
	h := newHarness(t, map[string]string{
		"one":    "name: one\nstages:\n  s: {runs_on: linux, run: echo one}\n",
		"locked": "name: locked\nlocks: [shared]\nstages:\n  s: {runs_on: linux, run: echo locked}\n",
		"other":  "name: other\nconcurrency: parallel\nlocks: [shared]\nstages:\n  s: {run: echo other}\n",
	}, ag.opts)
	first := h.trigger(t, "one")
	second := h.trigger(t, "one")
	locked := h.trigger(t, "locked")
	eventually(t, "the first and the locked build wait for the runner, holding no executor", func() bool {
		return len(h.waitingOf(t, first.ID)) == 1 && len(h.waitingOf(t, locked.ID)) == 1 && h.builtInBusy() == 0
	})
	other := h.trigger(t, "other")
	eventually(t, "the second build and the other job wait for what is held", func() bool {
		var seen int
		for _, w := range h.queue(t).Queued {
			switch {
			case w.Build.ID == second.ID && w.Waiting == WaitJob && w.Blocker.ID == first.ID:
				seen++
			case w.Build.ID == other.ID && w.Waiting == WaitLock && w.Lock == "shared":
				seen++
			}
		}
		return seen == 2
	})
	ag.runBox(t.TempDir())
	for _, b := range []*store.Build{first, second, locked, other} {
		if got := h.wait(t, b.ID); got.Status != store.Success {
			t.Fatalf("%s: %s", got.Job, got.Status)
		}
	}
}

func TestCancellingABuildThatWaitsForARunnerGivesBackNothing(t *testing.T) {
	quickPlaces(t)
	ag := idleRunner(t)
	h := newHarness(t, map[string]string{
		"remote": "name: remote\nstages:\n  s: {runs_on: linux, run: echo never}\n",
		"local":  "name: local\nstages:\n  s: {run: echo local}\n",
	}, ag.opts, func(o *Options) { o.Executors = 1 })
	remote := h.trigger(t, "remote")
	eventually(t, "the stage waits, holding no executor", func() bool {
		return len(h.waitingOf(t, remote.ID)) == 1 && h.builtInBusy() == 0
	})
	if err := h.d.Cancel(context.Background(), remote.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if b := h.wait(t, remote.ID); b.Status != store.Cancelled {
		t.Fatalf("%s", b.Status)
	}
	for range 2 {
		if b := h.wait(t, h.trigger(t, "local").ID); b.Status != store.Success {
			t.Fatalf("%s", b.Status)
		}
	}
	if n := h.builtInBusy(); n != 0 {
		t.Fatalf("%d executors taken at the end", n)
	}
}

// Each stage on the server takes an executor of its own: the built-in runner
// counts stages, not builds.
func TestParallelStagesOfOneBuildTakeAnExecutorEach(t *testing.T) {
	hold, open := gated(t)
	h := newHarness(t, map[string]string{
		"a": "name: a\nstages:\n  x: {run: '" + hold + "'}\n  y: {run: '" + hold + "'}\n",
	}, func(o *Options) { o.Executors = 4 })
	b := h.trigger(t, "a")
	eventually(t, "both stages run, each on an executor", func() bool {
		st := h.stages(t, b.ID)
		return len(st) == 2 && st["x"].Status == store.StageRunning && st["y"].Status == store.StageRunning && h.builtInBusy() == 2
	})
	if q := h.queue(t); q.Executors != 4 || q.Busy != 2 {
		t.Fatalf("the queue says %d/%d", q.Busy, q.Executors)
	}
	open()
	h.wait(t, b.ID)
	if n := h.builtInBusy(); n != 0 {
		t.Fatalf("%d executors taken at the end", n)
	}
}

// A server that runs no stage of its own still prepares its builds, a few
// at once.
func TestAServerOfNoExecutorsStillPreparesBuilds(t *testing.T) {
	ag := idleRunner(t)
	ag.runBox(t.TempDir())
	h := newHarness(t, map[string]string{
		"a": "name: a\nstages:\n  s: {run: echo away}\n",
	}, ag.opts, func(o *Options) { o.RunsNoStages, o.DefaultRunsOn = true, []string{"linux"} })
	if b := h.wait(t, h.trigger(t, "a").ID); b.Status != store.Success {
		t.Fatalf("%s", b.Status)
	}
	if q := h.queue(t); q.Executors != NoStagesPrepares || q.RunsStages {
		t.Fatalf("the queue says %d executors, runs stages %v", q.Executors, q.RunsStages)
	}
}
