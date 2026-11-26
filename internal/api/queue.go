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

const queueSettle = 250 * time.Millisecond

// QueueView is the queue as one user may see it: the builds of the jobs
// the user may view, and of the builds that hold them back only those the
// user may view as well.
type QueueView struct {
	Running         []RunningView
	Queued          []QueuedView
	Executors, Busy int
	Runners         []RunnerLoad
}

// The kinds of RunnerLoad.
const (
	KindBuiltIn = "built-in"
	KindRunner  = "runner"
	KindEngine  = "engine"
)

// The states of RunnerLoad.
const (
	StateOnline   = "online"
	StateOffline  = "offline"
	StateDisabled = "disabled"
)

// RunnerLoad is how busy one place that runs stages is: the built-in
// runner (the server's own executors), a standing runner or an engine.
type RunnerLoad struct {
	Name, Kind   string
	Busy, Limit  int
	State        string
	PreparesOnly bool
	Weight       int
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
	Waiting   map[string]string
	LockWaits map[string]dispatch.StageLockWait
	Remote    map[string]string
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
	v := &QueueView{Executors: q.Executors, Busy: q.Busy, Runners: s.loads(q), Running: []RunningView{}, Queued: []QueuedView{}}
	for _, r := range q.Running {
		if p.view[r.Build.Job] {
			v.Running = append(v.Running, RunningView{Build: r.Build, Stages: r.Stages, CanCancel: p.cancel[r.Build.Job], Waiting: r.Waiting, Remote: r.Remote,
				LockWaits: visibleLockWaits(r.LockWaits, p.view)})
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

// EndedLimit is how many ended builds the pages that list them show.
const EndedLimit = 10

// NewestEnded is the EndedLimit builds that ended last among those of the
// jobs u may view, the newest end first. The web pages show it with the
// queue; the API does not.
func (s *Service) NewestEnded(ctx context.Context, u *store.User) ([]*store.Build, error) {
	jobs, err := s.deps.Store.EndedJobs(ctx)
	if err != nil {
		return nil, err
	}
	visible, err := s.deps.Auth.Visible(ctx, u, jobs)
	if err != nil {
		return nil, err
	}
	return s.deps.Store.NewestEnded(ctx, visible, EndedLimit)
}

func (s *Service) loads(q *dispatch.Queue) []RunnerLoad {
	out := []RunnerLoad{{Name: KindBuiltIn, Kind: KindBuiltIn, Busy: q.Busy, Limit: q.Executors, State: StateOnline, PreparesOnly: !q.RunsStages}}
	if s.deps.Runners != nil {
		for _, r := range s.deps.Runners() {
			if r.Build == 0 {
				out = append(out, RunnerLoad{Name: r.Name, Kind: KindRunner, Busy: r.Busy, Limit: r.Executors, State: placeState(r.Enabled, r.Online), Weight: r.Weight})
			}
		}
	}
	if s.deps.Engines != nil {
		for _, e := range s.deps.Engines() {
			out = append(out, RunnerLoad{Name: e.Name, Kind: KindEngine, Busy: len(e.Running), Limit: e.Max, State: placeState(e.Enabled, e.Reachable)})
		}
	}
	return out
}

func placeState(enabled, online bool) string {
	switch {
	case !enabled:
		return StateDisabled
	case !online:
		return StateOffline
	}
	return StateOnline
}

// WaitPlace is what a stage waiting to be placed is said to wait for, to
// whoever is not an admin.
const WaitPlace = "waiting for a place to run"

func hideCapacity(v *QueueView) {
	v.Executors, v.Busy, v.Runners = 0, 0, nil
	for i, r := range v.Running {
		if r.Remote != nil {
			hidden := map[string]string{}
			for stage := range r.Remote {
				hidden[stage] = ""
			}
			v.Running[i].Remote = hidden
		}
		v.Running[i].Waiting = hideWaits(r.Waiting, r.LockWaits)
	}
	for i, w := range v.Queued {
		if w.Waiting == dispatch.WaitExecutor || w.Waiting == dispatch.WaitNothing {
			v.Queued[i].Waiting = WaitStart
		}
	}
}

func hideWaits(w map[string]string, locks map[string]dispatch.StageLockWait) map[string]string {
	if w == nil {
		return nil
	}
	hidden := make(map[string]string, len(w))
	for stage, line := range w {
		hidden[stage] = WaitPlace
		if _, lock := locks[stage]; lock {
			hidden[stage] = line
		}
	}
	return hidden
}

func visibleLockWaits(w map[string]dispatch.StageLockWait, view map[string]bool) map[string]dispatch.StageLockWait {
	if w == nil {
		return nil
	}
	out := make(map[string]dispatch.StageLockWait, len(w))
	for stage, lw := range w {
		if lw.Blocker != nil && !view[lw.Blocker.Job] {
			lw.Blocker = nil
		}
		out[stage] = lw
	}
	return out
}

type queuePerms struct{ view, cancel map[string]bool }

func (s *Service) queuePerms(ctx context.Context, u *store.User, q *dispatch.Queue) (queuePerms, error) {
	var names []string
	for _, r := range q.Running {
		names = append(names, r.Build.Job)
		for _, lw := range r.LockWaits {
			if lw.Blocker != nil {
				names = append(names, lw.Blocker.Job)
			}
		}
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

func (v *QueueView) signature() string {
	var b strings.Builder
	for _, l := range v.Runners {
		fmt.Fprintf(&b, "p%s:%d/%d:%s:%d;", l.Name, l.Busy, l.Limit, l.State, l.Weight)
	}
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

type queueStream struct {
	a       *API
	u       *store.User
	changes <-chan struct{}
	last    string
}

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

func (s *queueStream) reread(ctx context.Context) (bool, error) {
	if !sleepCtx(ctx, queueSettle) {
		return false, ctx.Err()
	}
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
