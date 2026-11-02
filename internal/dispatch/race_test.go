package dispatch

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/ndthuan/kipp/internal/store"
)

// racers is how many triggers each test below sends at the same moment.
const racers = 50

// triggerAtOnce sends every request from its own goroutine, all released
// together, and returns the builds made and how many were dropped. Any
// other error fails the test.
func triggerAtOnce(t *testing.T, h *harness, reqs []TriggerRequest) (made []*store.Build, dropped int) {
	t.Helper()
	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
	)
	start := make(chan struct{})
	for _, req := range reqs {
		wg.Go(func() {
			<-start
			b, err := h.d.Trigger(context.Background(), req)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				made = append(made, b)
			case errors.Is(err, ErrDropped):
				dropped++
			default:
				errs = append(errs, err)
			}
		})
	}
	close(start)
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	return made, dropped
}

func same(req TriggerRequest, n int) []TriggerRequest {
	reqs := make([]TriggerRequest, n)
	for i := range reqs {
		reqs[i] = req
	}
	return reqs
}

func TestTriggersThatRaceOnADropJobMakeOneBuild(t *testing.T) {
	block, open := gated(t)
	h := newHarness(t, map[string]string{"d": "name: d\nconcurrency: drop\nstages:\n  s: {run: '" + block + "'}\n"})
	made, dropped := triggerAtOnce(t, h, same(TriggerRequest{Job: "d", Kind: "api", By: "test"}, racers))
	if len(made) != 1 || dropped != racers-1 {
		t.Fatalf("%d builds made and %d dropped, want 1 and %d", len(made), dropped, racers-1)
	}
	if all, _ := h.st.Builds(context.Background(), "d", racers); len(all) != 1 {
		t.Fatalf("%d builds in the store, want 1", len(all))
	}
	open()
	if b := h.wait(t, made[0].ID); b.Status != store.Success {
		t.Fatalf("%v", b.Status)
	}
}

func TestCronFiresThatRaceBehindARunningBuildQueueOne(t *testing.T) {
	block, open := gated(t)
	h := newHarness(t, map[string]string{"c": "name: c\nstages:\n  s: {run: '" + block + "'}\n"})
	first := h.trigger(t, "c")
	h.untilRunning(t, first.ID)
	made, dropped := triggerAtOnce(t, h, same(TriggerRequest{Job: "c", Kind: "cron"}, racers))
	if len(made) != 1 || dropped != racers-1 {
		t.Fatalf("%d builds made and %d dropped, want 1 and %d", len(made), dropped, racers-1)
	}
	if queued, _ := h.st.BuildsWithStatus(context.Background(), store.Queued); len(queued) != 1 {
		t.Fatalf("%d builds queued, want 1", len(queued))
	}
	open()
	for _, id := range []int64{first.ID, made[0].ID} {
		if b := h.wait(t, id); b.Status != store.Success {
			t.Fatalf("#%d %v", b.Number, b.Status)
		}
	}
}

func TestTriggersThatRaceOnALockRunOneBuildAtATime(t *testing.T) {
	h := newHarness(t, map[string]string{
		"x": "name: x\nlocks: [shared]\nstages:\n  s: {run: sleep 0.02}\n",
		"y": "name: y\nconcurrency: parallel\nlocks: [shared]\nstages:\n  s: {run: sleep 0.02}\n",
	}, func(o *Options) { o.Executors = 8 })
	reqs := append(same(TriggerRequest{Job: "x", Kind: "api", By: "test"}, racers/2),
		same(TriggerRequest{Job: "y", Kind: "api", By: "test"}, racers/2)...)
	made, dropped := triggerAtOnce(t, h, reqs)
	if len(made) != racers || dropped != 0 {
		t.Fatalf("%d builds made and %d dropped, want %d and 0", len(made), dropped, racers)
	}
	var builds []*store.Build
	for _, m := range made {
		b := h.wait(t, m.ID)
		if b.Status != store.Success {
			t.Fatalf("%s#%d %v", b.Job, b.Number, b.Status)
		}
		builds = append(builds, b)
	}
	if n := mostAtOnce(builds); n != 1 {
		t.Fatalf("%d builds held the lock at once", n)
	}
}
