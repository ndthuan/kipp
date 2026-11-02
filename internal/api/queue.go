package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/ndthuan/kipp/internal/dispatch"
	"github.com/ndthuan/kipp/internal/store"
)

// queueSettle is how long the event stream lets a change settle before it
// reads the queue again: a build that starts is several changes in a row,
// and one event says all of them.
const queueSettle = 250 * time.Millisecond

// QueueView is the queue as one user may see it: the builds of the jobs
// the user may view, and of the builds that hold them back only those the
// user may view as well.
type QueueView struct {
	Running []RunningView
	Queued  []QueuedView
	// Executors and Busy are told to an admin alone, and are 0 to anyone
	// else.
	Executors, Busy int
}

// WaitStart is why a build waits, to anyone but an admin, when it waits
// for an executor or for nothing: which of the two it is tells whether the
// builds of other jobs, ones the user may not view, hold every executor.
const WaitStart dispatch.WaitReason = "start"

// RunningView is a running build and the stages it runs now.
type RunningView struct {
	Build     *store.Build
	Stages    []string
	CanCancel bool
	// Waiting is what each stage that waits to be placed waits for: the
	// line its log has for an admin, and only that it waits for anyone else,
	// to whom the names and the state of the places are not told.
	Waiting map[string]string
}

// QueuedView is a queued build and why it waits. Blocker is nil when the
// build it waits for is one the user may not view.
type QueuedView struct {
	Build     *store.Build
	Waiting   dispatch.WaitReason
	Lock      string
	Blocker   *store.Build
	CanCancel bool
}

// Queue is what runs and what waits, as u may see it.
func (s *Service) Queue(ctx context.Context, u *store.User) (*QueueView, error) {
	q, err := s.deps.Dispatch.Queue(ctx)
	if err != nil {
		return nil, err
	}
	p, err := s.queuePerms(ctx, u, q)
	if err != nil {
		return nil, err
	}
	v := &QueueView{Executors: q.Executors, Busy: q.Busy, Running: []RunningView{}, Queued: []QueuedView{}}
	for _, r := range q.Running {
		if p.view[r.Build.Job] {
			v.Running = append(v.Running, RunningView{Build: r.Build, Stages: r.Stages, CanCancel: p.cancel[r.Build.Job], Waiting: r.Waiting})
		}
	}
	for _, w := range q.Queued {
		if !p.view[w.Build.Job] {
			continue
		}
		qv := QueuedView{Build: w.Build, Waiting: w.Waiting, Lock: w.Lock, CanCancel: p.cancel[w.Build.Job]}
		if w.Blocker != nil && p.view[w.Blocker.Job] {
			qv.Blocker = w.Blocker
		}
		v.Queued = append(v.Queued, qv)
	}
	if !u.Admin {
		hideCapacity(v)
	}
	return v, nil
}

// WaitPlace is what a stage waiting to be placed is said to wait for, to
// whoever is not an admin.
const WaitPlace = "waiting for a place to run"

// hideCapacity takes out of v what tells how busy the server is.
func hideCapacity(v *QueueView) {
	v.Executors, v.Busy = 0, 0
	for i, r := range v.Running {
		if r.Waiting != nil {
			hidden := map[string]string{}
			for stage := range r.Waiting {
				hidden[stage] = WaitPlace
			}
			v.Running[i].Waiting = hidden
		}
	}
	for i, w := range v.Queued {
		if w.Waiting == dispatch.WaitExecutor || w.Waiting == dispatch.WaitNothing {
			v.Queued[i].Waiting = WaitStart
		}
	}
}

// queuePerms is what u may do with each job the queue names.
type queuePerms struct{ view, cancel map[string]bool }

func (s *Service) queuePerms(ctx context.Context, u *store.User, q *dispatch.Queue) (queuePerms, error) {
	var names []string
	for _, r := range q.Running {
		names = append(names, r.Build.Job)
	}
	for _, w := range q.Queued {
		names = append(names, w.Build.Job)
		if w.Blocker != nil {
			names = append(names, w.Blocker.Job)
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	p := queuePerms{view: map[string]bool{}, cancel: map[string]bool{}}
	visible, err := s.deps.Auth.Visible(ctx, u, names)
	if err != nil {
		return p, err
	}
	for _, n := range visible {
		p.view[n] = true
		if p.cancel[n], err = s.deps.Auth.Can(ctx, u, n, "cancel"); err != nil {
			return p, err
		}
	}
	return p, nil
}

// signature is what of the view a page shows that can change: a new one
// is what the event stream tells about.
func (v *QueueView) signature() string {
	var b strings.Builder
	for _, r := range v.Running {
		fmt.Fprintf(&b, "r%d:%s:%v;", r.Build.ID, strings.Join(r.Stages, ","), r.Waiting)
	}
	for _, w := range v.Queued {
		blocker := int64(0)
		if w.Blocker != nil {
			blocker = w.Blocker.ID
		}
		fmt.Fprintf(&b, "q%d:%s:%s:%d;", w.Build.ID, w.Waiting, w.Lock, blocker)
	}
	return b.String()
}

func (a *API) queue(w http.ResponseWriter, r *http.Request, u *store.User) {
	v, err := a.svc.Queue(r.Context(), u)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toQueueV1(v))
}

// queueEvents sends an event called changed each time the queue u may see
// changes, and nothing else: the client reads the queue again when told.
// A change u may not see is not told, so a user who may view one job does
// not learn when the others build.
func (a *API) queueEvents(w http.ResponseWriter, r *http.Request, u *store.User) {
	ctx, done, err := a.openStream(r, u, nil)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	defer done()
	s := &queueStream{a: a, u: u, changes: a.deps.Dispatch.Changes()}
	v, err := a.svc.Queue(ctx, u)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	s.last = v.signature()
	sealed(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flush := http.NewResponseController(w).Flush
	if flush() != nil {
		return
	}
	alive := time.NewTicker(a.keepAlive)
	defer alive.Stop()
	for {
		line, ok := s.next(ctx, alive.C)
		if !ok {
			return
		}
		if _, err := w.Write([]byte(line)); err != nil || flush() != nil {
			return
		}
	}
}

// queueStream is one client's stream of the queue's events.
type queueStream struct {
	a       *API
	u       *store.User
	changes <-chan struct{}
	last    string // the signature of the queue the client was last told of
}

// next is the next thing to write: the event, or a line that keeps a quiet
// stream alive, which a proxy would otherwise close. It is false once the
// client has left or the queue cannot be read.
func (s *queueStream) next(ctx context.Context, alive <-chan time.Time) (string, bool) {
	for {
		select {
		case <-ctx.Done():
			return "", false
		case <-alive:
			return keepAliveComment, true
		case <-s.changes:
			changed, err := s.reread(ctx)
			if err != nil {
				return "", false
			}
			if changed {
				return "event: changed\ndata: \n\n", true
			}
		}
	}
}

// reread lets a change settle, reads the queue again and reports whether
// what the client may see of it is not what it was told of last.
func (s *queueStream) reread(ctx context.Context) (bool, error) {
	if !sleepCtx(ctx, queueSettle) {
		return false, ctx.Err()
	}
	// Asked for before the queue is read, so that no change between the
	// two goes untold.
	s.changes = s.a.deps.Dispatch.Changes()
	v, err := s.a.svc.Queue(ctx, s.u)
	if err != nil {
		if ctx.Err() == nil {
			s.a.deps.Logger.Error("api: the queue's event stream", "err", err)
		}
		return false, err
	}
	sig := v.signature()
	if sig == s.last {
		return false, nil
	}
	s.last = sig
	return true, nil
}

// sleepCtx waits for d, and reports false if ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
