package dispatch

import (
	"context"
	"errors"
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
