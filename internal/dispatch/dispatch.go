// Package dispatch turns triggers into builds and runs them: the queue, the
// concurrency rules, the named locks, the executor limit and the lifecycle
// of one build from queued to a terminal status.
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/cachesrv"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/runner"
	"github.com/ndthuan/kipp/internal/runnerhub"
	"github.com/ndthuan/kipp/internal/store"
)

var rescan = time.Second

// Errors a trigger or a cancel can come back with. Any other error is the
// store's.
var (
	ErrUnknownJob = errors.New("no such job")
	ErrDisabled   = errors.New("the job is disabled")
	ErrDropped    = errors.New("dropped: the job is running or already queued")
	ErrBadParams  = errors.New("params refused")
	ErrTooLate    = errors.New("can no longer be cancelled")
	// ErrNoDefaultChild is a multibranch job asked to build before kipp
	// has heard of its default branch: a push to it, or a scan, makes the
	// child.
	ErrNoDefaultChild = errors.New("the multibranch job has no child of its default branch yet; a push to that branch makes it")
	// ErrOwnerJob is an owner job asked to build: it builds nothing itself,
	// and each of its repos has a job that does.
	ErrOwnerJob = errors.New("an owner job builds nothing itself; build one of its repos")
	// ErrNoTag is the tags' child of a multibranch job asked to build with
	// no tag: a push of a tag builds it, and Build again rebuilds one.
	ErrNoTag = errors.New("this job builds pushed tags: a tag's push starts its builds, and Build again on one rebuilds that tag")
	// ErrQuiet is any trigger while the server is quiet: it is draining
	// before a restart and starts no build (see SetQuiet).
	ErrQuiet = errors.New("kipp is quiet: restarting once running builds end")
)

// Options configure a Dispatcher. Zero values take the defaults noted.
type Options struct {
	DataDir       string
	Executors     int
	RunsNoStages  bool
	DefaultRunsOn []string
	Hub           *runnerhub.Hub
	Engines       EngineSource
	RunnerEnv     []string
	Shell         string
	BaseURL       string
	TimeZone      *time.Location
	Secrets       func(ctx context.Context, names []string) (map[string]Secret, error)
	Clone         func(ctx context.Context, dir, repo, ref string, out io.Writer) error
	Head          func(ctx context.Context, dir string) (string, error)
	Merge         func(ctx context.Context, dir, repo, base, baseCommit, head string, out io.Writer) (baseSHA, headSHA string, err error)
	Retain        func(ctx context.Context, st *store.Store, job *config.Job, logsDir string, now time.Time) error
	Logger        *slog.Logger
	PassEnv       []string
	ArtifactsMax  int64
	Cache         *cachesrv.Service
	CacheURL      string
	RunnerURL     string
	BuildTokens   *auth.BuildTokens
	Status        func(ctx context.Context, job *config.Job, s CommitStatus) error
	PublicURL     string
	Now           func() time.Time
}

// TriggerRequest asks for a build.
type TriggerRequest struct {
	Job                             string
	Kind                            string
	Detail                          string
	Branch                          string
	Tag                             string
	DefaultBranch                   string
	UpstreamStatus                  store.Status
	UpstreamCommit, UpstreamTag     string
	Params                          map[string]string
	By                              string
	Commit, BuiltCommit, BaseCommit string
	RerunOf                         int
}

// Dispatcher owns the queue and the running builds.
type Dispatcher struct {
	st   *store.Store
	r    runner.Runner
	opts Options
	log  *slog.Logger
	bin  string

	local *limit
	wake  chan struct{}
	quit  chan struct{}
	wg    sync.WaitGroup

	queueMu      sync.Mutex
	trimMu       sync.Mutex
	keptAftersMu sync.Mutex
	keptCronsMu  sync.Mutex

	mu         sync.Mutex
	snap       *config.Snapshot
	running    map[int64]*run
	ending     map[int64]bool
	locks      map[string]lockHolder
	done       map[int64][]chan struct{}
	waiting    map[int64]map[string]string
	changes    chan struct{}
	lockWaits  map[int64]map[string]stageLockWait
	lockWanted map[string]int
	lockFreed  chan struct{}
	saying     map[int64]chan struct{}
	quiet      time.Time
	stopping   bool
	quietEnded func()
	chosen     map[int64]bool
	now        func() time.Time
}

type run struct {
	build     *store.Build
	job       *config.Job
	cancel    context.CancelCauseFunc
	settled   bool
	stopped   bool
	preparing bool
}

func (r *run) stop(cause error) {
	r.stopped = true
	r.cancel(cause)
}

type cancelledBy string

func (c cancelledBy) Error() string { return "cancelled by " + string(c) }

type supersededBy struct {
	number int
	commit string
}

func (s supersededBy) Error() string { return fmt.Sprintf("cancelled by build #%d", s.number) }

type childGone string

func (c childGone) Error() string { return "cancelled: " + string(c) }

func goneWhy(job *config.Job) string {
	switch {
	case job == nil || job.Origin == nil || job.Origin.State != config.ChildGone:
		return ""
	case job.Origin.Kind == config.KindBranch:
		return "branch " + job.Origin.Branch + " is gone"
	case job.Origin.Kind == config.KindTag:
		return "the repo's tags are no longer built"
	}
	return fmt.Sprintf("pull request #%d is closed", job.Origin.PR)
}

type timedOut time.Duration

func (t timedOut) Error() string {
	return "the job's timeout of " + time.Duration(t).String() + " passed"
}

// New makes a Dispatcher; call Recover, then Run in a goroutine.
func New(st *store.Store, r runner.Runner, opts Options) *Dispatcher {
	switch {
	case opts.RunsNoStages:
		opts.Executors = 0
	case opts.Executors <= 0:
		opts.Executors = 4
	}
	if opts.Secrets == nil {
		opts.Secrets = func(context.Context, []string) (map[string]Secret, error) { return map[string]Secret{}, nil }
	}
	if opts.Clone == nil {
		opts.Clone = gitClone
	}
	if opts.Head == nil {
		opts.Head = gitHead
	}
	if opts.Merge == nil {
		opts.Merge = gitMerge
	}
	if opts.Retain == nil {
		opts.Retain = func(context.Context, *store.Store, *config.Job, string, time.Time) error { return nil }
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Dispatcher{
		st: st, r: r, opts: opts, log: opts.Logger, bin: executable(),
		local: newLimit(opts.Executors),
		wake:  make(chan struct{}, 1), quit: make(chan struct{}), waiting: map[int64]map[string]string{}, now: opts.Now,
		snap:    &config.Snapshot{Jobs: map[string]*config.Job{}},
		running: map[int64]*run{}, ending: map[int64]bool{}, locks: map[string]lockHolder{},
		lockWaits: map[int64]map[string]stageLockWait{}, lockWanted: map[string]int{}, lockFreed: make(chan struct{}),
		done: map[int64][]chan struct{}{}, changes: make(chan struct{}), saying: map[int64]chan struct{}{},
	}
}

// SetExecutors changes the built-in runner's limit while the server runs:
// what runs past a lowered one finishes, and nothing new starts until it
// is under. A server that runs no stage of its own keeps running none.
func (d *Dispatcher) SetExecutors(n int) {
	if n < 1 || !d.local.runsStages() {
		return
	}
	d.local.set(n)
	d.changed()
	d.kick()
}

// Executors is the built-in runner's limit now; 0 on a server that runs no
// stage of its own.
func (d *Dispatcher) Executors() int {
	if !d.local.runsStages() {
		return 0
	}
	_, n := d.local.load()
	return n
}

// Zone is the zone the job's cron is read in; for a nil job it is the
// server's own, which is also the zone of what the pages show.
func (d *Dispatcher) Zone(job *config.Job) *time.Location { return job.Zone(d.opts.TimeZone) }

// SetSnapshot replaces the job set; running builds keep the job they
// started with, but one whose child the new set says is gone is cancelled.
func (d *Dispatcher) SetSnapshot(s *config.Snapshot) {
	d.mu.Lock()
	d.snap = s
	cancelled := d.cancelGoneLocked(s)
	d.mu.Unlock()
	now := time.Now()
	for _, c := range cancelled {
		if err := d.st.AddActivity(context.Background(), c.job, c.line, now); err != nil {
			d.log.Error("recording a cancel", "job", c.job, "err", err)
		}
	}
	d.kick()
}

type activityLine struct{ job, line string }

func (d *Dispatcher) cancelGoneLocked(s *config.Snapshot) []activityLine {
	var out []activityLine
	for _, r := range d.running {
		why := goneWhy(s.Jobs[r.build.Job])
		if why == "" || r.settled {
			continue
		}
		r.stop(childGone(why))
		out = append(out, activityLine{r.build.Job, fmt.Sprintf("#%d cancelled: %s", r.build.Number, why)})
	}
	return out
}

// Snapshot is the current job set.
func (d *Dispatcher) Snapshot() *config.Snapshot {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.snap
}

// LogsDir holds every build's logs, as <job>/<number>/<stage>.log.
func (d *Dispatcher) LogsDir() string { return filepath.Join(d.opts.DataDir, "logs") }

// LogPath is where a stage's console lives.
func (d *Dispatcher) LogPath(job string, number int, stage string) string {
	return filepath.Join(d.LogsDir(), job, strconv.Itoa(number), stage+".log")
}

func (d *Dispatcher) kick() {
	d.changed()
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Trigger creates a queued build, or drops the request by the job's rule.
// A disabled job is ErrDisabled whoever asks: a caller that should ignore
// its trigger without a word does not ask.
func (d *Dispatcher) Trigger(ctx context.Context, req TriggerRequest) (*store.Build, error) {
	b, err := d.trigger(ctx, req)
	if errors.Is(err, ErrQuiet) && req.Kind == "cron" {
		d.keepCron(ctx, req.Job)
	}
	return b, err
}

func (d *Dispatcher) trigger(ctx context.Context, req TriggerRequest) (*store.Build, error) {
	if d.refusedQuiet() {
		return nil, ErrQuiet
	}
	snap := d.Snapshot()
	job, err := buildable(snap, req)
	if err != nil {
		return nil, err
	}
	params, err := job.ResolveParams(req.Params)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadParams, err)
	}
	b, said, err := d.enqueue(ctx, snap, job, req, params)
	if said != nil {
		d.send(ctx, b, job, &buildEnv{}, "pending", b.Number, store.Queued)
		said()
	}
	return b, err
}

func (d *Dispatcher) enqueue(ctx context.Context, snap *config.Snapshot, job *config.Job, req TriggerRequest, params map[string]string) (*store.Build, func(), error) {
	d.queueMu.Lock()
	defer d.queueMu.Unlock()
	why, err := d.dropReason(ctx, job, req.Kind)
	if err != nil {
		return nil, nil, err
	}
	if why != "" {
		line := fmt.Sprintf("dropped a %s trigger: %s", req.Kind, why)
		if err := d.st.AddActivity(ctx, job.Name, line, time.Now()); err != nil {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("%w (%s)", ErrDropped, why)
	}
	b := &store.Build{Job: job.Name, Status: store.Queued, Trigger: req.Kind, TriggerDetail: req.Detail, TriggerBranch: req.Branch, TriggerDefaultBranch: req.DefaultBranch, TriggerTag: req.Tag, UpstreamStatus: req.UpstreamStatus, UpstreamCommit: req.UpstreamCommit, UpstreamTag: req.UpstreamTag, Params: params, StartedBy: req.By, ConfigHash: snap.Hash,
		PushedCommit: req.Commit, BuiltCommit: req.BuiltCommit, BaseCommit: req.BaseCommit, RerunOf: req.RerunOf}
	ofChild(b, job)
	if err := d.st.CreateBuild(ctx, b); err != nil {
		return nil, nil, err
	}
	sameCommit, err := d.supersede(ctx, job, b)
	if err != nil {
		return nil, nil, err
	}
	var said func()
	if sameCommit && d.reports(b, job) {
		said = d.sayingQueued(b.ID)
	}
	d.kick()
	return b, said, nil
}

func buildable(snap *config.Snapshot, req TriggerRequest) (*config.Job, error) {
	job := snap.Jobs[req.Job]
	if job == nil {
		return nil, fmt.Errorf("%w: %s", ErrUnknownJob, req.Job)
	}
	if job.IsOwner() {
		return nil, ErrOwnerJob
	}
	if job.IsParent() && !job.Disabled {
		if job = snap.DefaultChild(job.Name); job == nil {
			return nil, fmt.Errorf("%w: %s", ErrNoDefaultChild, req.Job)
		}
	}
	switch {
	case job.Disabled:
		return nil, ErrDisabled
	case job.BuildsTags() && req.Tag == "":
		return nil, ErrNoTag
	}
	return job, nil
}

func (d *Dispatcher) supersede(ctx context.Context, job *config.Job, b *store.Build) (sameCommit bool, err error) {
	if job.Concurrency != config.Supersede {
		return false, nil
	}
	active, err := d.st.BuildsWithStatus(ctx, store.Queued, store.Running)
	if err != nil {
		return false, err
	}
	commit := d.statusCommit(b)
	for _, old := range active {
		if old.Job != b.Job || old.ID == b.ID || old.TriggerBranch != b.TriggerBranch || old.TriggerTag != b.TriggerTag {
			continue
		}
		replaced, err := d.replace(ctx, old, b)
		if err != nil {
			return false, err
		}
		if !replaced {
			continue
		}
		sameCommit = sameCommit || (commit != "" && d.statusCommit(old) == commit)
		line := fmt.Sprintf("build #%d superseded by #%d", old.Number, b.Number)
		if err := d.st.AddActivity(ctx, b.Job, line, time.Now()); err != nil {
			return false, err
		}
	}
	return sameCommit, nil
}

func (d *Dispatcher) replace(ctx context.Context, old, by *store.Build) (bool, error) {
	if old.Status == store.Running {
		stopped, err := d.cancelRunning(old.ID, supersededBy{by.Number, d.statusCommit(by)})
		if errors.Is(err, ErrTooLate) {
			return false, nil
		}
		return stopped, err
	}
	if err := d.st.SetBuildStatus(ctx, old.ID, store.Cancelled, time.Now()); err != nil {
		return false, err
	}
	d.release(old.ID)
	return true, nil
}

func (d *Dispatcher) dropReason(ctx context.Context, job *config.Job, kind string) (string, error) {
	if job.Concurrency == config.Parallel || (job.Concurrency != config.Drop && kind != "cron") {
		return "", nil
	}
	active, err := d.st.BuildsWithStatus(ctx, store.Queued, store.Running)
	if err != nil {
		return "", err
	}
	for _, b := range active {
		if b.Job == job.Name && (job.Concurrency == config.Drop || b.Status == store.Queued) {
			return fmt.Sprintf("#%d is %s", b.Number, b.Status), nil
		}
	}
	return "", nil
}

// Cancel stops a running build or takes a queued one off the queue, for
// by, whom the logs of the stages it stops then name. A build that has
// ended, or whose final status is decided, is ErrTooLate.
func (d *Dispatcher) Cancel(ctx context.Context, id int64, by string) error {
	d.queueMu.Lock()
	defer d.queueMu.Unlock()
	if stopped, err := d.cancelRunning(id, cancelledBy(by)); stopped || err != nil {
		return err
	}
	b, err := d.st.Build(ctx, id)
	if err != nil {
		return err
	}
	if b.Status != store.Queued {
		return fmt.Errorf("build #%d %w: it is %s", b.Number, ErrTooLate, b.Status)
	}
	if err := d.st.SetBuildStatus(ctx, id, store.Cancelled, time.Now()); err != nil {
		return err
	}
	d.release(id)
	return nil
}

func (d *Dispatcher) cancelRunning(id int64, cause error) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r := d.running[id]
	if r == nil {
		return false, nil
	}
	if r.settled {
		return false, fmt.Errorf("build #%d %w: it is already finishing", r.build.Number, ErrTooLate)
	}
	r.stop(cause)
	return true, nil
}

// Recover is run once at start: what was running belonged to a dead
// process, and so did every workspace (a queued build has none yet). The
// error is the store's. A workspace that will not go is only logged: it is
// no reason to stay down.
func (d *Dispatcher) Recover(ctx context.Context) error {
	n, stages, err := d.st.MarkRunningLost(ctx, time.Now())
	if n > 0 {
		d.log.Warn("builds left running by the last process are lost", "count", n)
	}
	d.reapLost(stages)
	if rmErr := os.RemoveAll(filepath.Join(d.opts.DataDir, "workspace")); rmErr != nil {
		d.log.Warn("a leftover workspace could not be removed", "err", rmErr)
	}
	if rmErr := os.RemoveAll(d.secretFilesDir()); rmErr != nil {
		d.log.Warn("leftover secret files could not be removed", "err", rmErr)
	}
	if rmErr := os.RemoveAll(d.stageTmpDir()); rmErr != nil {
		d.log.Warn("leftover KIPP_TMP directories could not be removed", "err", rmErr)
	}
	return err
}

const lostLine = "kipp: the server stopped while this ran"

func (d *Dispatcher) reapLost(stages []store.LostStage) {
	var wg sync.WaitGroup
	for _, s := range stages {
		if s.Pgid <= 0 {
			d.log.Warn("a stage of the last process may still be running: no process group was recorded for it",
				"job", s.Job, "number", s.Number, "stage", s.Name)
		}
		wg.Go(func() {
			line := lostLine
			killed, err := d.r.Reap(runner.Group{Pgid: s.Pgid, Born: s.Born})
			switch {
			case err != nil:
				d.log.Warn("a stage of the last process may still be running: it was not killed",
					"job", s.Job, "number", s.Number, "stage", s.Name, "pgid", s.Pgid, "err", err)
			case killed:
				line += "; what it had left running was killed when the server started again"
			}
			d.note(&store.Build{Job: s.Job, Number: s.Number}, s.Name, &buildEnv{}, line)
		})
	}
	wg.Wait()
}

// Run schedules queued builds until ctx ends, then returns once the builds
// it started have ended: as lost, their stages and hooks killed. It wakes on
// every trigger and finish, and once a second regardless. Call it once.
func (d *Dispatcher) Run(ctx context.Context) {
	t := time.NewTicker(rescan)
	defer t.Stop()
	defer d.wg.Wait()
	for {
		d.schedule(ctx)
		select {
		case <-ctx.Done():
			close(d.quit)
			return
		case <-d.wake:
		case <-t.C:
		}
	}
}

func (d *Dispatcher) schedule(ctx context.Context) {
	d.queueMu.Lock()
	defer d.queueMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	queued, err := d.st.BuildsWithStatus(ctx, store.Queued)
	if err != nil {
		if ctx.Err() == nil {
			d.log.Error("reading the queue", "err", err)
		}
		return
	}
	snap := d.Snapshot()
	for _, b := range queued {
		switch job := snap.Jobs[b.Job]; {
		case job == nil:
			d.failQueued(ctx, b, "the job no longer exists")
		case goneWhy(job) != "":
			d.cancelQueued(ctx, b, goneWhy(job))
		case job.Disabled:
			d.failQueued(ctx, b, ErrDisabled.Error())
		default:
			d.start(ctx, b, job)
		}
	}
}

func (d *Dispatcher) failQueued(ctx context.Context, b *store.Build, why string) {
	d.endQueued(ctx, b, store.Failed, fmt.Sprintf("#%d failed before it started: %s", b.Number, why))
}

func (d *Dispatcher) cancelQueued(ctx context.Context, b *store.Build, why string) {
	d.endQueued(ctx, b, store.Cancelled, fmt.Sprintf("#%d cancelled before it started: %s", b.Number, why))
}

func (d *Dispatcher) endQueued(ctx context.Context, b *store.Build, status store.Status, line string) {
	now := time.Now()
	d.mu.Lock()
	d.ending[b.ID] = true
	d.mu.Unlock()
	if err := d.st.SetBuildStatus(ctx, b.ID, status, now); err != nil {
		d.log.Error("ending a queued build", "build", b.ID, "status", status, "err", err)
		d.mu.Lock()
		delete(d.ending, b.ID)
		d.mu.Unlock()
		return
	}
	if err := d.st.AddActivity(ctx, b.Job, line, now); err != nil {
		d.log.Error("recording a queued build's end", "build", b.ID, "err", err)
	}
	d.release(b.ID)
}

func (d *Dispatcher) start(ctx context.Context, b *store.Build, job *config.Job) {
	runCtx, r := d.reserve(ctx, b, job)
	if r == nil {
		return
	}
	now := time.Now()
	if err := d.st.SetBuildStatus(ctx, b.ID, store.Running, now); err != nil {
		d.log.Error("marking running", "build", b.ID, "err", err)
		d.release(b.ID)
		return
	}
	d.mu.Lock()
	b.Status, b.StartedAt = store.Running, &now
	d.mu.Unlock()
	d.changed()
	d.wg.Go(func() { d.runBuild(runCtx, r) })
}

func (d *Dispatcher) reserve(ctx context.Context, b *store.Build, job *config.Job) (context.Context, *run) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.mayStartLocked(b, job) || !d.local.tryTake() {
		return nil, nil
	}
	for _, l := range job.Locks {
		d.locks[l] = lockHolder{build: b.ID}
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	r := &run{build: b, job: job, cancel: cancel, preparing: true}
	d.running[b.ID] = r
	return runCtx, r
}

func (d *Dispatcher) mayStartLocked(b *store.Build, job *config.Job) bool {
	if d.stopping {
		return false
	}
	if job.Concurrency != config.Parallel {
		for _, r := range d.running {
			if r.build.Job == b.Job {
				return false
			}
		}
	}
	for _, l := range job.Locks {
		if _, held := d.locks[l]; held || d.lockWanted[l] > 0 {
			return false
		}
	}
	return true
}

func (d *Dispatcher) prepared(id int64) {
	d.mu.Lock()
	if r := d.running[id]; r != nil && r.preparing {
		r.preparing = false
		d.local.give()
	}
	d.mu.Unlock()
	d.kick()
}

func (d *Dispatcher) release(id int64) {
	d.mu.Lock()
	delete(d.ending, id)
	if r := d.running[id]; r != nil {
		for _, l := range r.job.Locks {
			if d.locks[l] == (lockHolder{build: id}) {
				delete(d.locks, l)
			}
		}
		d.lockFreedLocked()
		r.cancel(nil)
		delete(d.running, id)
		if r.preparing {
			d.local.give()
		}
	}
	for _, ch := range d.done[id] {
		close(ch)
	}
	delete(d.done, id)
	d.mu.Unlock()
	d.kick()
}

// Wait returns the build once it is terminal and its hook and downstream
// triggers are done, or ctx's error.
func (d *Dispatcher) Wait(ctx context.Context, id int64) (*store.Build, error) {
	for {
		ch := d.subscribe(id)
		b, err := d.st.Build(ctx, id)
		if err != nil || (b.Status.Terminal() && !d.Busy(id)) {
			d.unsubscribe(id, ch)
			return b, err
		}
		select {
		case <-ctx.Done():
			d.unsubscribe(id, ch)
			return nil, ctx.Err()
		case <-ch:
		}
	}
}

func (d *Dispatcher) subscribe(id int64) chan struct{} {
	ch := make(chan struct{})
	d.mu.Lock()
	defer d.mu.Unlock()
	d.done[id] = append(d.done[id], ch)
	return ch
}

// Busy is true from the moment a build leaves the queue until its hook and
// downstream triggers are done. A build's logs are complete once its status
// is terminal and, asked after that, it is not busy: the junit notes and the
// hook's output are written after the status.
func (d *Dispatcher) Busy(id int64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.running[id] != nil || d.ending[id]
}

func (d *Dispatcher) unsubscribe(id int64, ch chan struct{}) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if i := slices.Index(d.done[id], ch); i >= 0 {
		d.done[id] = slices.Delete(d.done[id], i, i+1)
	}
	if len(d.done[id]) == 0 {
		delete(d.done, id)
	}
}
