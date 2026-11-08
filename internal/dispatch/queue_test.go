package dispatch

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/store"
	"github.com/ndthuan/kipp/internal/waitfor"
)

// queueUntil reads the queue until ok holds of it, each time the dispatcher
// announces a change.
func (h *harness) queueUntil(t *testing.T, what string, ok func(*Queue) bool) *Queue {
	t.Helper()
	timeout := time.After(waitfor.Safety)
	for {
		changes := h.d.Changes()
		q, err := h.d.Queue(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if ok(q) {
			return q
		}
		select {
		case <-changes:
		case <-timeout:
			t.Fatalf("%s: never; the queue was %+v", what, q)
		}
	}
}

func TestTheQueueSaysWhatRunsAndWhyEachQueuedBuildWaits(t *testing.T) {
	h := newHarness(t, map[string]string{
		"a": "name: a\nlocks: [checkout]\nstages:\n  s: {run: sleep 30}\n",
		"b": "name: b\nconcurrency: parallel\nlocks: [checkout]\nstages:\n  s: {run: 'true'}\n",
		"c": "name: c\nstages:\n  s: {run: 'true'}\n",
	}, func(o *Options) { o.Executors = 1 })
	first := h.trigger(t, "a")
	h.queueUntil(t, "a #1 runs its stage", func(q *Queue) bool {
		return len(q.Running) == 1 && slices.Equal(q.Running[0].Stages, []string{"s"})
	})
	second, locked, starved := h.trigger(t, "a"), h.trigger(t, "b"), h.trigger(t, "c")
	third := h.trigger(t, "a")
	q := h.queueUntil(t, "four wait", func(q *Queue) bool { return len(q.Queued) == 4 })
	if q.Executors != 1 || q.Busy != 1 || q.Running[0].Build.ID != first.ID {
		t.Fatalf("executors %d, busy %d, running %+v", q.Executors, q.Busy, q.Running[0].Build)
	}
	want := []struct {
		id      int64
		why     WaitReason
		lock    string
		blocker int64
	}{
		{second.ID, WaitJob, "", first.ID},
		{locked.ID, WaitLock, "checkout", first.ID},
		{starved.ID, WaitExecutor, "", 0},
		{third.ID, WaitJob, "", second.ID},
	}
	for i, w := range want {
		got := q.Queued[i]
		blocker := int64(0)
		if got.Blocker != nil {
			blocker = got.Blocker.ID
		}
		if got.Build.ID != w.id || got.Waiting != w.why || got.Lock != w.lock || blocker != w.blocker {
			t.Errorf("queued %d: build %d waits for %q (lock %q, blocker %d), want %d %q (%q, %d)",
				i, got.Build.ID, got.Waiting, got.Lock, blocker, w.id, w.why, w.lock, w.blocker)
		}
	}
	for _, b := range []*store.Build{third, starved, locked, second, first} {
		if err := h.d.Cancel(context.Background(), b.ID, "test"); err != nil {
			t.Fatal(err)
		}
	}
	h.queueUntil(t, "nothing is left", func(q *Queue) bool { return len(q.Running)+len(q.Queued) == 0 })
}

// changed waits for ch to close, as it does at the next change.
func changed(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(waitfor.Safety):
		t.Fatalf("%s was not announced", what)
	}
}

func TestEveryChangeToTheQueueIsAnnounced(t *testing.T) {
	h := newHarness(t, map[string]string{
		"a": "name: a\nstages:\n  s: {run: sleep 0.2}\n  t: {needs: [s], run: 'true'}\n",
	})
	ch := h.d.Changes()
	select {
	case <-ch:
		t.Fatal("announced with nothing changed")
	case <-time.After(50 * time.Millisecond):
	}
	b := h.trigger(t, "a")
	changed(t, "a trigger", ch)
	h.queueUntil(t, "a starts", func(q *Queue) bool { return len(q.Running) == 1 })
	ch = h.d.Changes()
	changed(t, "a stage starting or ending", ch)
	ch = h.d.Changes()
	h.wait(t, b.ID)
	select {
	case <-ch:
	default:
		t.Fatal("the build ended unannounced")
	}
}
