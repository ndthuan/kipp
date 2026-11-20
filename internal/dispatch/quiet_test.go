package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/store"
)

func TestQuietRefusesEveryTriggerAndSaysWhy(t *testing.T) {
	h := newHarness(t, map[string]string{"a": "name: a\nstages:\n  s: {run: 'true'}\n"})
	ctx := context.Background()
	st, err := h.d.SetQuiet(ctx, true)
	if err != nil || !st.On || st.Since.IsZero() {
		t.Fatalf("%+v %v", st, err)
	}
	for _, kind := range []string{"api", "web", "webhook", "scan", "after", "cron"} {
		_, err := h.d.Trigger(ctx, TriggerRequest{Job: "a", Kind: kind, By: "test"})
		if !errors.Is(err, ErrQuiet) {
			t.Errorf("a %s trigger: %v, want ErrQuiet", kind, err)
		}
	}
	if err := ErrQuiet; err.Error() != "kipp is quiet: restarting once running builds end" {
		t.Errorf("the message is %q", err)
	}
	if st, _ = h.d.SetQuiet(ctx, false); st.On {
		t.Fatal("still quiet")
	}
	if b := h.trigger(t, "a"); h.wait(t, b.ID).Status != store.Success {
		t.Fatal("a trigger after quiet did not build")
	}
}

func TestQuietLetsRunningAndQueuedBuildsEnd(t *testing.T) {
	block, open := gated(t)
	h := newHarness(t, map[string]string{"a": "name: a\nstages:\n  s: {run: '" + block + "'}\n"})
	ctx := context.Background()
	first := h.trigger(t, "a")
	h.untilRunning(t, first.ID)
	second := h.trigger(t, "a") // accepted before quiet: it waits for the first
	st, err := h.d.SetQuiet(ctx, true)
	if err != nil || st.Running != 1 || st.Queued != 1 {
		t.Fatalf("%+v %v, want 1 running and 1 queued", st, err)
	}
	if _, err := h.d.Trigger(ctx, TriggerRequest{Job: "a", Kind: "api", By: "test"}); !errors.Is(err, ErrQuiet) {
		t.Fatalf("%v", err)
	}
	open()
	for _, id := range []int64{first.ID, second.ID} {
		if b := h.wait(t, id); b.Status != store.Success {
			t.Fatalf("#%d ended %v: quiet must not cut a build short", b.Number, b.Status)
		}
	}
	eventually(t, "nothing is left", func() bool {
		st, _ := h.d.QuietState(ctx)
		return st.On && st.Running == 0 && st.Queued == 0
	})
}

func TestACronFireRefusedDuringQuietRunsOnceWhenItEnds(t *testing.T) {
	h := newHarness(t, map[string]string{"c": "name: c\ntriggers: {cron: '* * * * *'}\nstages:\n  s: {run: 'true'}\n"})
	ctx := context.Background()
	if _, err := h.d.SetQuiet(ctx, true); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i := range 3 { // three fires in one quiet: one build after it
		h.d.Tick(ctx, now.Add(time.Duration(i)*time.Minute))
	}
	if builds, _ := h.st.BuildsWithStatus(ctx, store.Queued, store.Running, store.Success); len(builds) != 0 {
		t.Fatalf("%d builds during quiet", len(builds))
	}
	if _, err := h.d.SetQuiet(ctx, false); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the kept fire ran", func() bool {
		builds, _ := h.st.BuildsWithStatus(ctx, store.Success)
		return len(builds) == 1 && builds[0].Trigger == "cron"
	})
	// A second fire would have been queued by now, and a pass over the queue
	// begun after it would have run it.
	h.d.schedule(ctx)
	if builds, _ := h.st.BuildsWithStatus(ctx, store.Queued, store.Running, store.Success); len(builds) != 1 {
		t.Fatalf("%d builds, want the one", len(builds))
	}
	// Quiet turned on twice or off twice changes nothing.
	for _, on := range []bool{false, true, true, false} {
		if _, err := h.d.SetQuiet(ctx, on); err != nil {
			t.Fatal(err)
		}
	}
	if h.d.Quiet() {
		t.Fatal("still quiet")
	}
}

func upAndDown(block string) map[string]string {
	return map[string]string{
		"up":   "name: up\nstages:\n  s: {run: '" + block + "'}\n",
		"down": "name: down\ntriggers: {after: [up]}\nstages:\n  s: {run: 'echo from=$KIPP_UPSTREAM_BUILD'}\n",
	}
}

// endUpstreamDuringQuiet runs up, makes the server quiet while it runs, and
// lets it end: its after trigger comes while quiet.
func (h *harness) endUpstreamDuringQuiet(t *testing.T, open func()) *store.Build {
	t.Helper()
	ctx := context.Background()
	up := h.trigger(t, "up")
	h.untilRunning(t, up.ID)
	if _, err := h.d.SetQuiet(ctx, true); err != nil {
		t.Fatal(err)
	}
	open()
	if b := h.wait(t, up.ID); b.Status != store.Success {
		t.Fatalf("up ended %v", b.Status)
	}
	if downs, _ := h.st.Builds(ctx, "down", 5); len(downs) != 0 {
		t.Fatalf("down was queued while quiet: %+v", downs)
	}
	return up
}

func (h *harness) downAfter(t *testing.T, up *store.Build) {
	t.Helper()
	var downs []*store.Build
	eventually(t, "down was triggered", func() bool {
		downs, _ = h.st.Builds(context.Background(), "down", 5)
		return len(downs) > 0
	})
	if len(downs) != 1 {
		t.Fatalf("%d builds of down, want one", len(downs))
	}
	down := h.wait(t, downs[0].ID)
	want := fmt.Sprintf("up#%d", up.Number)
	if down.Status != store.Success || down.Trigger != "after" || down.TriggerDetail != want || down.UpstreamStatus != store.Success {
		t.Fatalf("down %+v, want a green after trigger from %s", down, want)
	}
	if !strings.Contains(h.log(t, down, "s"), "from="+want) {
		t.Fatalf("down's log %q", h.log(t, down, "s"))
	}
}

func TestAnAfterTriggerRefusedDuringQuietRunsAfterTheRestart(t *testing.T) {
	block, open := gated(t)
	h := newHarness(t, upAndDown(block))
	up := h.endUpstreamDuringQuiet(t, open)
	h2, _ := h.restart(t)
	h2.downAfter(t, up)
	if pending, err := h2.st.PendingAfters(context.Background()); err != nil || len(pending) != 0 {
		t.Fatalf("still kept %+v %v", pending, err)
	}
}

func TestAnAfterTriggerRefusedDuringQuietRunsOnceWhenItEnds(t *testing.T) {
	block, open := gated(t)
	h := newHarness(t, upAndDown(block))
	ctx := context.Background()
	up := h.endUpstreamDuringQuiet(t, open)
	if _, err := h.d.SetQuiet(ctx, false); err != nil {
		t.Fatal(err)
	}
	h.downAfter(t, up)
	h.d.TriggerKeptAfters(ctx)
	if downs, _ := h.st.Builds(ctx, "down", 5); len(downs) != 1 {
		t.Fatalf("%d builds of down, want the one", len(downs))
	}
}

func TestAKeptAfterTriggerStaysKeptWhileTheServerIsQuietAgain(t *testing.T) {
	block, open := gated(t)
	h := newHarness(t, upAndDown(block))
	ctx := context.Background()
	h.endUpstreamDuringQuiet(t, open)
	h.d.TriggerKeptAfters(ctx)
	if pending, err := h.st.PendingAfters(ctx); err != nil || len(pending) != 1 || pending[0].Job != "down" {
		t.Fatalf("kept %+v %v, want down still kept", pending, err)
	}
	if downs, _ := h.st.Builds(ctx, "down", 5); len(downs) != 0 {
		t.Fatalf("down was queued while quiet: %+v", downs)
	}
}

func TestACronFireRefusedDuringQuietRunsOnceAfterARestart(t *testing.T) {
	h := newHarness(t, map[string]string{"c": "name: c\ntriggers: {cron: '* * * * *'}\nstages:\n  s: {run: 'true'}\n"})
	ctx := context.Background()
	if _, err := h.d.SetQuiet(ctx, true); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i := range 3 {
		h.d.Tick(ctx, now.Add(time.Duration(i)*time.Minute))
	}
	again, _ := h.restart(t)
	eventually(t, "the kept fire ran", func() bool {
		builds, _ := h.st.BuildsWithStatus(ctx, store.Success)
		return len(builds) == 1 && builds[0].Trigger == "cron"
	})
	again.d.schedule(ctx)
	if builds, _ := h.st.BuildsWithStatus(ctx, store.Queued, store.Running, store.Success); len(builds) != 1 {
		t.Fatalf("%d builds, want the one", len(builds))
	}
	if kept, _ := h.st.PendingCrons(ctx); len(kept) != 0 {
		t.Fatalf("kept after running: %v", kept)
	}
}
