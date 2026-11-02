package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/dispatch"
)

type queueAnswer struct {
	Executors int `json:"executors"`
	Busy      int `json:"busy"`
	Running   []struct {
		ID        int64     `json:"id"`
		Job       string    `json:"job"`
		Number    int       `json:"number"`
		StartedAt time.Time `json:"started_at"`
		Stages    []string  `json:"stages"`
	} `json:"running"`
	Queued []struct {
		ID      int64  `json:"id"`
		Job     string `json:"job"`
		Number  int    `json:"number"`
		Waiting string `json:"waiting"`
		Lock    string `json:"lock"`
		Blocker *struct {
			ID     int64  `json:"id"`
			Job    string `json:"job"`
			Number int    `json:"number"`
			Status string `json:"status"`
		} `json:"blocker"`
	} `json:"queued"`
}

func (e *env) queue(t *testing.T, tok string) queueAnswer {
	t.Helper()
	text, _ := e.call(t, 200, tok, "GET", "/api/v1/queue", "")
	var q queueAnswer
	if err := json.Unmarshal([]byte(text), &q); err != nil {
		t.Fatalf("%v: %s", err, text)
	}
	return q
}

func TestTheQueueAnswersWhatRunsAndWhyTheRestWaits(t *testing.T) {
	e := newEnv(t)
	first := e.started(t, "slow")
	second := e.trigger(t, "slow", "")
	var q queueAnswer
	eventually(t, func() bool { q = e.queue(t, e.admin); return len(q.Queued) == 1 })
	eq(t, "executors", q.Executors, 4)
	eq(t, "running", len(q.Running), 1)
	r := q.Running[0]
	eq(t, "running build", r.Job+" "+strconv.FormatInt(r.ID, 10), "slow "+first)
	eq(t, "its stages", strings.Join(r.Stages, ","), "s")
	if r.StartedAt.IsZero() {
		t.Error("a running build with no start")
	}
	w := q.Queued[0]
	eq(t, "queued build", strconv.FormatInt(w.ID, 10), second)
	eq(t, "why", w.Waiting, "job")
	if w.Blocker == nil || strconv.FormatInt(w.Blocker.ID, 10) != first || w.Blocker.Status != "running" {
		t.Fatalf("blocker %+v", w.Blocker)
	}
	// The viewer may see smoke only, and nothing of slow is in its queue.
	if v := e.queue(t, e.viewer); len(v.Running)+len(v.Queued) != 0 {
		t.Fatalf("the viewer sees %+v", v)
	}
	e.call(t, 202, e.admin, "POST", "/api/v1/builds/"+second+"/cancel", "")
	e.call(t, 202, e.admin, "POST", "/api/v1/builds/"+first+"/cancel", "")
}

func TestABuildThatHoldsALockIsNamedOnlyToWhoeverMayViewIt(t *testing.T) {
	e := newEnv(t)
	e.writeJob(t, "holder", "name: holder\nlocks: [k]\nstages:\n  s: {run: 'echo started; sleep 30'}\n")
	e.writeJob(t, "waiter", "name: waiter\nlocks: [k]\nstages:\n  s: {run: 'true'}\n")
	only := token(t, e.st, "only-waiter", false, map[string][]string{"waiter": {"view"}})
	held := e.started(t, "holder")
	waiting := e.trigger(t, "waiter", "")
	var q queueAnswer
	eventually(t, func() bool { q = e.queue(t, only); return len(q.Queued) == 1 })
	eq(t, "running builds it sees", len(q.Running), 0)
	eq(t, "why", q.Queued[0].Waiting, "lock")
	eq(t, "lock", q.Queued[0].Lock, "k")
	if q.Queued[0].Blocker != nil {
		t.Fatalf("a build it may not view is named: %+v", q.Queued[0].Blocker)
	}
	if q = e.queue(t, e.admin); q.Queued[0].Blocker == nil || q.Queued[0].Blocker.Job != "holder" {
		t.Fatalf("the admin is not told who holds the lock: %+v", q.Queued[0])
	}
	e.call(t, 202, e.admin, "POST", "/api/v1/builds/"+waiting+"/cancel", "")
	e.call(t, 202, e.admin, "POST", "/api/v1/builds/"+held+"/cancel", "")
}

func TestOnlyAnAdminIsToldHowBusyTheExecutorsAre(t *testing.T) {
	e := newEnv(t)
	for tok, want := range map[string]bool{e.admin: true, e.viewer: false} {
		q := e.object(t, 200, tok, "GET", "/api/v1/queue", "")
		_, executors := q["executors"].(float64)
		_, busy := q["busy"].(float64)
		eq(t, fmt.Sprintf("executors and busy in %v", q), executors && busy, want)
		eq(t, fmt.Sprintf("either in %v", q), executors || busy, want)
	}
}

// A build that waits for an executor, or for nothing, says to anyone but an
// admin only that it waits to start: either would tell whether builds of
// jobs the user may not view hold every executor.
func TestToAllButAnAdminABuildWaitingForAnExecutorWaitsToStart(t *testing.T) {
	v := &QueueView{Executors: 4, Busy: 4, Queued: []QueuedView{
		{Waiting: dispatch.WaitExecutor}, {Waiting: dispatch.WaitNothing}, {Waiting: dispatch.WaitLock, Lock: "k"},
		{Waiting: dispatch.WaitJob}, {Waiting: dispatch.WaitGone},
	}}
	hideCapacity(v)
	eq(t, "executors", v.Executors, 0)
	eq(t, "busy", v.Busy, 0)
	var got []string
	for _, w := range v.Queued {
		got = append(got, string(w.Waiting))
	}
	eq(t, "why each waits", strings.Join(got, " "), "start start lock job gone")
}

// events opens the queue's event stream as tok and sends a line down
// lines for each line it reads.
func (e *env) events(t *testing.T, tok string) <-chan string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req := e.request(ctx, t, tok, "GET", "/api/v1/queue/events", nil, "Accept", sseAccept)
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	res, err := client.Do(req)
	must(t, err)
	eq(t, "status", res.StatusCode, 200)
	eq(t, "content type", res.Header.Get("Content-Type"), sseAccept)
	lines := make(chan string, 100)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	t.Cleanup(func() { cancel(); _ = res.Body.Close() })
	return lines
}

// announced waits for an event called changed on lines, or for none in quiet.
func announced(lines <-chan string, within time.Duration) bool {
	deadline := time.After(within)
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				return false
			}
			if l == "event: changed" {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

func TestTheQueuesEventsTellEachUserOfChangesToWhatTheyMaySee(t *testing.T) {
	e := newEnv(t)
	admin, viewer := e.events(t, e.admin), e.events(t, e.viewer)
	id := e.trigger(t, "slow", "")
	if !announced(admin, 5*time.Second) {
		t.Fatal("the admin was not told of a build of slow")
	}
	if announced(viewer, time.Second) {
		t.Fatal("the viewer was told of a build of a job it may not see")
	}
	e.call(t, 202, e.admin, "POST", "/api/v1/builds/"+id+"/cancel", "")
	if !announced(admin, 5*time.Second) {
		t.Fatal("the admin was not told the build was cancelled")
	}
	e.finished(t, e.trigger(t, "smoke", `{"params":{"TIER":"quick"}}`))
	if !announced(viewer, 5*time.Second) {
		t.Fatal("the viewer was not told of a build of smoke")
	}
}

func TestTheQueuesEventStreamKeepsItselfAlive(t *testing.T) {
	e := newEnv(t, func(a *API) { a.keepAlive = 50 * time.Millisecond })
	lines := e.events(t, e.admin)
	select {
	case l := <-lines:
		if !strings.HasPrefix(l, ":") {
			t.Fatalf("the first line of a quiet stream is %q", l)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a quiet stream said nothing")
	}
}
