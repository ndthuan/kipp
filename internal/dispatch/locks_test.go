package dispatch

import (
	"context"
	"strings"
	"testing"

	"github.com/ndthuan/kipp/internal/store"
)

func (h *harness) lockWaitOf(t *testing.T, id int64, stage string) (StageLockWait, bool) {
	t.Helper()
	for _, r := range h.queue(t).Running {
		if r.Build.ID == id {
			w, ok := r.LockWaits[stage]
			return w, ok
		}
	}
	return StageLockWait{}, false
}

func (h *harness) untilLockWait(t *testing.T, id int64, stage, lock string, blocker int64) {
	t.Helper()
	eventually(t, "stage "+stage+" waits for lock "+lock, func() bool {
		w, ok := h.lockWaitOf(t, id, stage)
		return ok && w.Lock == lock && w.Blocker != nil && w.Blocker.ID == blocker && h.waitingOf(t, id)[stage] == LockWaitLine(lock)
	})
}

func (h *harness) assertAfter(t *testing.T, earlier int64, earlierStage string, later int64, laterStage string) {
	t.Helper()
	e, l := h.stages(t, earlier)[earlierStage], h.stages(t, later)[laterStage]
	if e.FinishedAt == nil || l.StartedAt == nil || l.StartedAt.Before(*e.FinishedAt) {
		t.Fatalf("%s started %v, before %s ended %v", laterStage, l.StartedAt, earlierStage, e.FinishedAt)
	}
}

func (h *harness) assertStatus(t *testing.T, id int64, want store.Status) *store.Build {
	t.Helper()
	b := h.wait(t, id)
	if b.Status != want {
		t.Fatalf("%s #%d is %s, want %s", b.Job, b.Number, b.Status, want)
	}
	return b
}

func (h *harness) assertNoLocks(t *testing.T) {
	t.Helper()
	h.d.mu.Lock()
	defer h.d.mu.Unlock()
	if len(h.d.locks) != 0 || len(h.d.lockWanted) != 0 || len(h.d.lockWaits) != 0 {
		t.Fatalf("left behind: locks %v, wanted %v, waits %v", h.d.locks, h.d.lockWanted, h.d.lockWaits)
	}
}

func TestStagesOfTwoJobsLockingOneNameNeverOverlapAndTheirOtherStagesRunBeside(t *testing.T) {
	hold, open := gated(t)
	beside, openBeside := gated(t)
	h := newHarness(t, map[string]string{
		"a": "name: a\nstages:\n  deploy: {run: '" + hold + "', locks: [x]}\n  free: {run: '" + beside + "'}\n",
		"b": "name: b\nstages:\n  deploy: {run: 'true', locks: [x]}\n  free: {run: '" + beside + "'}\n",
	})
	a := h.trigger(t, "a")
	h.untilStage(t, a.ID, "deploy", store.StageRunning)
	b := h.trigger(t, "b")
	h.untilLockWait(t, b.ID, "deploy", "x", a.ID)
	h.untilStage(t, b.ID, "free", store.StageRunning)
	h.untilStage(t, a.ID, "free", store.StageRunning)
	open()
	h.untilStage(t, b.ID, "deploy", store.StageSuccess)
	openBeside()
	h.assertStatus(t, a.ID, store.Success)
	h.assertStatus(t, b.ID, store.Success)
	h.assertAfter(t, a.ID, "deploy", b.ID, "deploy")
	if got := h.log(t, b, "deploy"); !strings.Contains(got, "kipp: waiting for lock x\n") {
		t.Fatalf("the log of the stage that waited: %q", got)
	}
	h.assertNoLocks(t)
}

func TestAJobLockHoldsBackAStageLockOfTheSameName(t *testing.T) {
	hold, open := gated(t)
	h := newHarness(t, map[string]string{
		"job":   "name: job\nlocks: [x]\nstages:\n  s: {run: '" + hold + "'}\n",
		"stage": "name: stage\nstages:\n  s: {run: 'true', locks: [x]}\n",
	})
	job := h.trigger(t, "job")
	h.untilRunning(t, job.ID)
	stage := h.trigger(t, "stage")
	h.untilLockWait(t, stage.ID, "s", "x", job.ID)
	open()
	j := h.assertStatus(t, job.ID, store.Success)
	h.assertStatus(t, stage.ID, store.Success)
	if s := h.stages(t, stage.ID)["s"]; s.StartedAt.Before(*j.FinishedAt) {
		t.Fatalf("the stage started %v, before the build that held the lock ended %v", s.StartedAt, j.FinishedAt)
	}
}

func TestAStageLockHoldsBackAQueuedBuildThatLocksTheSameName(t *testing.T) {
	hold, open := gated(t)
	h := newHarness(t, map[string]string{
		"stage": "name: stage\nstages:\n  s: {run: '" + hold + "', locks: [x]}\n",
		"job":   "name: job\nlocks: [x]\nstages:\n  s: {run: 'true'}\n",
	})
	stage := h.trigger(t, "stage")
	h.untilStage(t, stage.ID, "s", store.StageRunning)
	job := h.trigger(t, "job")
	h.queueUntil(t, "the queued build waits for the lock the stage holds", func(q *Queue) bool {
		return len(q.Queued) == 1 && q.Queued[0].Waiting == WaitLock && q.Queued[0].Lock == "x" && q.Queued[0].Blocker != nil && q.Queued[0].Blocker.ID == stage.ID
	})
	open()
	h.assertStatus(t, stage.ID, store.Success)
	j := h.assertStatus(t, job.ID, store.Success)
	if s := h.stages(t, stage.ID)["s"]; j.StartedAt.Before(*s.FinishedAt) {
		t.Fatalf("the build started %v, before the stage that held the lock ended %v", j.StartedAt, s.FinishedAt)
	}
}

func TestAStageLockIsGivenBackWhenTheStageFailsWhileItsBuildRunsOn(t *testing.T) {
	hold, open := gated(t)
	h := newHarness(t, map[string]string{
		"f": "name: f\nstages:\n  bad: {run: 'exit 1', locks: [x]}\n  slow: {run: '" + hold + "'}\n",
		"g": "name: g\nstages:\n  s: {run: 'true', locks: [x]}\n",
	})
	f := h.trigger(t, "f")
	h.untilStage(t, f.ID, "bad", store.StageFailed)
	h.assertStatus(t, h.trigger(t, "g").ID, store.Success)
	open()
	h.assertStatus(t, f.ID, store.Failed)
	h.assertNoLocks(t)
}

func TestCancellingAStageThatWaitsForALockEndsTheWaitAtOnce(t *testing.T) {
	hold, _ := gated(t)
	h := newHarness(t, map[string]string{
		"f": "name: f\nstages:\n  s: {run: '" + hold + "', locks: [x]}\n",
		"g": "name: g\nstages:\n  s: {run: 'echo never', locks: [x]}\n",
		"h": "name: h\nstages:\n  s: {run: 'true', locks: [x]}\n",
	})
	f := h.trigger(t, "f")
	h.untilStage(t, f.ID, "s", store.StageRunning)
	g := h.trigger(t, "g")
	h.untilLockWait(t, g.ID, "s", "x", f.ID)
	if err := h.d.Cancel(context.Background(), g.ID, "test"); err != nil {
		t.Fatal(err)
	}
	gb := h.assertStatus(t, g.ID, store.Cancelled)
	if s := h.stages(t, g.ID)["s"]; s.Status != store.StageCancelled || strings.Contains(h.log(t, gb, "s"), "never") {
		t.Fatalf("the waiting stage %s ran or did not end cancelled", s.Status)
	}
	if err := h.d.Cancel(context.Background(), f.ID, "test"); err != nil {
		t.Fatal(err)
	}
	h.assertStatus(t, f.ID, store.Cancelled)
	h.assertStatus(t, h.trigger(t, "h").ID, store.Success)
	h.assertNoLocks(t)
}

func TestAStageLockIsGivenBackWhenTheJobsTimeoutPasses(t *testing.T) {
	hold, _ := gated(t)
	h := newHarness(t, map[string]string{
		"f": "name: f\ntimeout: 1s\nstages:\n  s: {run: '" + hold + "', locks: [x]}\n",
		"g": "name: g\nstages:\n  s: {run: 'true', locks: [x]}\n",
	})
	f := h.trigger(t, "f")
	h.untilStage(t, f.ID, "s", store.StageRunning)
	g := h.trigger(t, "g")
	h.assertStatus(t, f.ID, store.Timeout)
	h.assertStatus(t, g.ID, store.Success)
	h.assertNoLocks(t)
}

func TestAStageThatWaitsForALockHoldsNoExecutor(t *testing.T) {
	hold, open := gated(t)
	h := newHarness(t, map[string]string{
		"f":     "name: f\nstages:\n  s: {run: '" + hold + "', locks: [x]}\n",
		"g":     "name: g\nstages:\n  s: {run: 'true', locks: [x]}\n",
		"other": "name: other\nstages:\n  s: {run: 'true'}\n",
	}, func(o *Options) { o.Executors = 2 })
	f := h.trigger(t, "f")
	h.untilStage(t, f.ID, "s", store.StageRunning)
	g := h.trigger(t, "g")
	h.untilLockWait(t, g.ID, "s", "x", f.ID)
	eventually(t, "only the stage that runs holds an executor", func() bool { return h.builtInBusy() == 1 })
	h.assertStatus(t, h.trigger(t, "other").ID, store.Success)
	if _, waits := h.lockWaitOf(t, g.ID, "s"); !waits {
		t.Fatal("the stage stopped waiting before the lock was given back")
	}
	open()
	h.assertStatus(t, f.ID, store.Success)
	h.assertStatus(t, g.ID, store.Success)
}

func TestAStageThatWaitsForALockTakesItBeforeALaterQueuedBuild(t *testing.T) {
	hold, open := gated(t)
	second, openSecond := gated(t)
	h := newHarness(t, map[string]string{
		"f":   "name: f\nstages:\n  s: {run: '" + hold + "', locks: [x]}\n",
		"g":   "name: g\nstages:\n  s: {run: '" + second + "', locks: [x]}\n",
		"job": "name: job\nlocks: [x]\nstages:\n  s: {run: 'true'}\n",
	})
	f := h.trigger(t, "f")
	h.untilStage(t, f.ID, "s", store.StageRunning)
	g := h.trigger(t, "g")
	h.untilLockWait(t, g.ID, "s", "x", f.ID)
	job := h.trigger(t, "job")
	h.queueUntil(t, "the queued build waits for the lock", func(q *Queue) bool {
		return len(q.Queued) == 1 && q.Queued[0].Waiting == WaitLock && q.Queued[0].Lock == "x"
	})
	open()
	h.assertStatus(t, f.ID, store.Success)
	eventually(t, "the waiting stage took the lock", func() bool {
		_, waits := h.lockWaitOf(t, g.ID, "s")
		return !waits && h.stages(t, g.ID)["s"].Status == store.StageRunning
	})
	h.queueUntil(t, "the queued build now waits for the stage", func(q *Queue) bool {
		return len(q.Queued) == 1 && q.Queued[0].Blocker != nil && q.Queued[0].Blocker.ID == g.ID
	})
	openSecond()
	h.assertStatus(t, g.ID, store.Success)
	j := h.assertStatus(t, job.ID, store.Success)
	if s := h.stages(t, g.ID)["s"]; j.StartedAt.Before(*s.FinishedAt) {
		t.Fatalf("the queued build started %v, before the waiting stage ended %v", j.StartedAt, s.FinishedAt)
	}
}

func TestAStageLockingWhatItsBuildHoldsAsAJobLockRunsAtOnce(t *testing.T) {
	h := newHarness(t, map[string]string{
		"j": "name: j\nlocks: [x]\nstages:\n  s: {run: 'echo ran', locks: [x]}\n",
	})
	b := h.assertStatus(t, h.trigger(t, "j").ID, store.Success)
	if got := h.log(t, b, "s"); got != "ran\n" {
		t.Fatalf("the stage waited or did not run: %q", got)
	}
	h.assertNoLocks(t)
}

func TestParallelStagesOfOneBuildLockingOneNameTakeTurns(t *testing.T) {
	hold, open := gated(t)
	turn := "mkdir \"$KIPP_WORKSPACE/held\" || exit 1; " + hold + "; rmdir \"$KIPP_WORKSPACE/held\""
	h := newHarness(t, map[string]string{
		"j": "name: j\nstages:\n  a: {run: '" + turn + "', locks: [x]}\n  b: {run: '" + turn + "', locks: [x]}\n",
	})
	b := h.trigger(t, "j")
	eventually(t, "one stage runs and the other waits for it", func() bool {
		for _, waiting := range []string{"a", "b"} {
			if w, ok := h.lockWaitOf(t, b.ID, waiting); ok && w.Blocker != nil && w.Blocker.ID == b.ID {
				return true
			}
		}
		return false
	})
	open()
	h.assertStatus(t, b.ID, store.Success)
	h.assertNoLocks(t)
}

func TestTwoBuildsThatWouldWaitForEachOthersJobLocksFailTheStageThatClosesTheCircle(t *testing.T) {
	hold, open := gated(t)
	h := newHarness(t, map[string]string{
		"p": "name: p\nlocks: [p]\nstages:\n  s: {run: 'true', locks: [q]}\n",
		"q": "name: q\nlocks: [q]\nstages:\n  first: {run: '" + hold + "'}\n  s: {needs: [first], run: 'echo never', locks: [p]}\n",
	})
	q := h.trigger(t, "q")
	h.untilStage(t, q.ID, "first", store.StageRunning)
	p := h.trigger(t, "p")
	h.untilLockWait(t, p.ID, "s", "q", q.ID)
	open()
	qb := h.assertStatus(t, q.ID, store.Failed)
	if got := h.log(t, qb, "s"); !strings.Contains(got, "lock p is held by p #1, which waits for a lock this build holds") || strings.Contains(got, "never") {
		t.Fatalf("the stage that closed the circle: %q", got)
	}
	h.assertStatus(t, p.ID, store.Success)
	h.assertNoLocks(t)
}

func TestAStagesFileLockOutsideTheGrantFailsTheBuildBeforeAnyStage(t *testing.T) {
	file := "stages:\n  s: {run: 'echo ran', locks: [production]}\n"
	h := newHarness(t, map[string]string{
		"src":     "name: src\nsource: {repo: o/r, ref: main, path: kipp.yml}\n",
		"granted": "name: granted\nstage_locks: [production]\nsource: {repo: o/r, ref: main, path: kipp.yml}\n",
	}, withRepoFile(file))
	b := h.assertStatus(t, h.trigger(t, "src").ID, store.Failed)
	if got := h.log(t, b, "prepare"); !strings.Contains(got, "stage s: lock production is not one the job grants") {
		t.Fatalf("prepare: %q", got)
	}
	h.assertStatus(t, h.trigger(t, "granted").ID, store.Success)
}
