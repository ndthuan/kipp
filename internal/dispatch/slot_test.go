package dispatch

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/agenthub"
	"github.com/ndthuan/kipp/internal/store"
)

// A build whose stages wait to be placed on an agent holds no --builds slot,
// so the server's other jobs start while they wait.

const waitForSlotLine = "waiting for a free build slot"

// quickPlaces looks at a stage that waits for a place often enough to keep
// the tests short.
func quickPlaces(t *testing.T) {
	t.Helper()
	old := placeEvery
	placeEvery = 20 * time.Millisecond
	t.Cleanup(func() { placeEvery = old })
}

// idle is an agent that is known to the hub and not online: a stage that
// asks for it waits.
func idleAgent(t *testing.T) *agents {
	t.Helper()
	return newAgents(t, agenthub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
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

func (h *harness) slotsTaken() int {
	h.d.mu.Lock()
	defer h.d.mu.Unlock()
	return len(h.d.slots)
}

func TestBuildsThatOnlyWaitForAnAgentLeaveTheBuildsSlotsToOtherJobs(t *testing.T) {
	quickPlaces(t)
	ag := idleAgent(t)
	ag.runBox(t.TempDir())
	hold, open := gated(t)
	h := newHarness(t, map[string]string{
		"par":   "name: par\nconcurrency: parallel\nstages:\n  s: {runs_on: linux, run: '" + hold + "'}\n",
		"local": "name: local\nstages:\n  s: {run: echo local}\n",
	}, ag.opts, func(o *Options) { o.Executors, o.Builds = 4, 2 })
	var par []*store.Build
	for range 3 {
		par = append(par, h.trigger(t, "par"))
	}
	eventually(t, "one stage runs on the agent and two builds wait for it", func() bool {
		waiting := 0
		for _, b := range par {
			if len(h.waitingOf(t, b.ID)) == 1 {
				waiting++
			}
		}
		return waiting == 2 && h.slotsTaken() == 1
	})
	// With the slots as today, two of the three builds would hold both of
	// them and this build would wait as WaitExecutor.
	if b := h.wait(t, h.trigger(t, "local").ID); b.Status != store.Success {
		t.Fatalf("%s", b.Status)
	}
	open()
	for _, b := range par {
		if got := h.wait(t, b.ID); got.Status != store.Success {
			t.Fatalf("%s", got.Status)
		}
	}
	if n := h.slotsTaken(); n != 0 {
		t.Fatalf("%d slots still taken", n)
	}
}

func TestAStageWhosePlaceIsFreeWaitsForABuildsSlotAndSaysSo(t *testing.T) {
	quickPlaces(t)
	ag := idleAgent(t)
	hold, open := gated(t)
	h := newHarness(t, map[string]string{
		"remote": "name: remote\nstages:\n  s: {runs_on: linux, run: echo on-the-agent}\n",
		"hold":   "name: hold\nstages:\n  s: {run: '" + hold + "'}\n",
	}, ag.opts, func(o *Options) { o.Executors, o.Builds = 4, 1 })
	remote := h.trigger(t, "remote")
	eventually(t, "the remote stage waits for the agent, holding no slot", func() bool {
		return len(h.waitingOf(t, remote.ID)) == 1 && h.slotsTaken() == 0
	})
	holder := h.trigger(t, "hold")
	h.untilRunning(t, holder.ID)
	eventually(t, "the holder runs", func() bool { return h.slotsTaken() == 1 })
	ag.runBox(t.TempDir())
	eventually(t, "the stage waits for a slot", func() bool {
		return h.waitingOf(t, remote.ID)["s"] == waitForSlotLine
	})
	if got := h.log(t, remote, "s"); !strings.Contains(got, "kipp: "+waitForSlotLine+"\n") || strings.Contains(got, "on-the-agent") {
		t.Fatalf("log %q", got)
	}
	// The agent's executor is not held for a stage that cannot start.
	if !ag.hub.Free(agenthub.Who{Name: "box"}) {
		t.Fatal("the agent is held for a stage that waits for a slot")
	}
	open()
	b := h.wait(t, remote.ID)
	if got := h.log(t, b, "s"); b.Status != store.Success || !strings.Contains(got, "on-the-agent") {
		t.Fatalf("%s: %q", b.Status, got)
	}
	h.wait(t, holder.ID)
}

// A started build whose stage waits for a slot gets the first one that
// frees, not a build that was queued behind it.
func TestAStageWaitingForASlotIsNotStarvedByQueuedBuilds(t *testing.T) {
	quickPlaces(t)
	ag := idleAgent(t)
	hold, openHold := gated(t)
	stay, openStay := gated(t)
	h := newHarness(t, map[string]string{
		"remote": "name: remote\nstages:\n  s: {runs_on: linux, run: '" + stay + "'}\n",
		"hold":   "name: hold\nstages:\n  s: {run: '" + hold + "'}\n",
		"quick":  "name: quick\nconcurrency: parallel\nstages:\n  s: {run: 'true'}\n",
	}, ag.opts, func(o *Options) { o.Executors, o.Builds = 4, 1 })
	remote := h.trigger(t, "remote")
	eventually(t, "the remote stage waits, holding no slot", func() bool {
		return len(h.waitingOf(t, remote.ID)) == 1 && h.slotsTaken() == 0
	})
	holder := h.trigger(t, "hold")
	// The holder lets go of the slot between its prepare and its stage, and a
	// build queued in that gap would take it.
	eventually(t, "the holder's stage holds the slot", func() bool {
		return h.stages(t, holder.ID)["s"] != nil && h.stages(t, holder.ID)["s"].Status == store.StageRunning && h.slotsTaken() == 1
	})
	var quick []*store.Build
	for range 4 {
		quick = append(quick, h.trigger(t, "quick"))
	}
	ag.runBox(t.TempDir())
	eventually(t, "the stage waits for a slot", func() bool {
		return h.waitingOf(t, remote.ID)["s"] == waitForSlotLine
	})
	openHold()
	eventually(t, "the stage starts on the agent", func() bool {
		return h.stages(t, remote.ID)["s"].RanOn == "box" && len(h.waitingOf(t, remote.ID)) == 0
	})
	// While the stage runs on the one slot, every build queued behind it still waits.
	if q := h.queue(t); len(q.Queued) != len(quick) {
		t.Fatalf("%d builds queued, want %d: the stage was passed over", len(q.Queued), len(quick))
	}
	openStay()
	h.wait(t, remote.ID)
	for _, b := range quick {
		h.wait(t, b.ID)
	}
}

// What a build holds besides the slot, its place among the running builds of
// its job and its locks, it keeps while it holds no slot.
func TestABuildWaitingForAnAgentKeepsItsPlaceAndItsLocks(t *testing.T) {
	quickPlaces(t)
	ag := idleAgent(t)
	h := newHarness(t, map[string]string{
		"one":    "name: one\nstages:\n  s: {runs_on: linux, run: echo one}\n",
		"locked": "name: locked\nlocks: [shared]\nstages:\n  s: {runs_on: linux, run: echo locked}\n",
		"other":  "name: other\nconcurrency: parallel\nlocks: [shared]\nstages:\n  s: {run: echo other}\n",
	}, ag.opts, func(o *Options) { o.Executors, o.Builds = 4, 4 })
	first := h.trigger(t, "one")
	second := h.trigger(t, "one")
	locked := h.trigger(t, "locked")
	eventually(t, "the first and the locked build wait for the agent, holding no slot", func() bool {
		return len(h.waitingOf(t, first.ID)) == 1 && len(h.waitingOf(t, locked.ID)) == 1 && h.slotsTaken() == 0
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

func TestCancellingABuildThatHoldsNoSlotGivesBackNothing(t *testing.T) {
	quickPlaces(t)
	ag := idleAgent(t)
	h := newHarness(t, map[string]string{
		"remote": "name: remote\nstages:\n  s: {runs_on: linux, run: echo never}\n",
		"local":  "name: local\nstages:\n  s: {run: echo local}\n",
	}, ag.opts, func(o *Options) { o.Executors, o.Builds = 4, 1 })
	remote := h.trigger(t, "remote")
	eventually(t, "the stage waits, holding no slot", func() bool {
		return len(h.waitingOf(t, remote.ID)) == 1 && h.slotsTaken() == 0
	})
	if err := h.d.Cancel(context.Background(), remote.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if b := h.wait(t, remote.ID); b.Status != store.Cancelled {
		t.Fatalf("%s", b.Status)
	}
	if n := h.slotsTaken(); n != 0 {
		t.Fatalf("%d slots taken after the cancel", n)
	}
	// The one slot is still there to take, and only once.
	for range 2 {
		if b := h.wait(t, h.trigger(t, "local").ID); b.Status != store.Success {
			t.Fatalf("%s", b.Status)
		}
	}
	if n := h.slotsTaken(); n != 0 {
		t.Fatalf("%d slots taken at the end", n)
	}
}

// A build counts once however many of its stages run at once.
func TestParallelStagesOfOneBuildHoldOneSlot(t *testing.T) {
	hold, open := gated(t)
	h := newHarness(t, map[string]string{
		"a": "name: a\nstages:\n  x: {run: '" + hold + "'}\n  y: {run: '" + hold + "'}\n",
	}, func(o *Options) { o.Executors, o.Builds = 4, 4 })
	b := h.trigger(t, "a")
	eventually(t, "both stages run", func() bool {
		st := h.stages(t, b.ID)
		return len(st) == 2 && st["x"].Status == store.StageRunning && st["y"].Status == store.StageRunning
	})
	if n := h.slotsTaken(); n != 1 {
		t.Fatalf("%d slots taken by one build", n)
	}
	open()
	h.wait(t, b.ID)
	if n := h.slotsTaken(); n != 0 {
		t.Fatalf("%d slots taken at the end", n)
	}
}
