// Package dispatch turns triggers into builds and runs them: the queue, the
// concurrency rules, the named locks, the executor limit and the lifecycle
// of one build from queued to a terminal status.
package dispatch

import (
	"cmp"
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

// rescan is how often Run looks at the queue with nothing waking it.
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
	// ErrQuiet is any trigger while the server is quiet: it is draining
	// before a restart and starts no build (see SetQuiet).
	ErrQuiet = errors.New("kipp is quiet: restarting once running builds end")
)

// Options configure a Dispatcher. Zero values take the defaults noted.
type Options struct {
	DataDir string // logs/ and workspace/ live under it
	// Executors is how many stages the server runs itself at once; default
	// 4. RunsNoStages is a server that runs none (--executors 0): a stage
	// with no runs_on goes where DefaultRunsOn says, or fails.
	Executors    int
	RunsNoStages bool
	// Builds is how many builds with something running run at once (see
	// joinSlot), the one limit that also counts their stages on runners;
	// default Executors, or 4 for a server that runs no stage itself.
	Builds        int
	DefaultRunsOn []string
	// Hub is the runners, and Engines the Docker engines, a stage with
	// runs_on is placed on; nil is none.
	Hub     *runnerhub.Hub
	Engines EngineSource
	// RunnerEnv names the server's variables a stage on a runner is also
	// given (exact names); PassEnv is for the server's own stages only.
	RunnerEnv []string
	Shell     string // runner.Local's shell, default sh
	BaseURL   string // KIPP_URL for stages
	// TimeZone is the zone a cron is read in when its job names none; nil
	// is the host's.
	TimeZone *time.Location
	// Secrets opens the named secrets, each as its kind keeps it.
	Secrets func(ctx context.Context, names []string) (map[string]Secret, error)
	Clone   func(ctx context.Context, dir, repo, ref string, out io.Writer) error
	// Head is the commit a clone in dir checked out; default the host's
	// git.
	Head func(ctx context.Context, dir string) (string, error)
	// Merge checks a pull request's head merged into the tip of its base
	// branch, or into baseCommit when that is set, out into dir, and says
	// the two commits; ErrNoCleanMerge when they conflict. Default the
	// host's git.
	Merge  func(ctx context.Context, dir, repo, base, baseCommit, head string, out io.Writer) (baseSHA, headSHA string, err error)
	Retain func(ctx context.Context, st *store.Store, job *config.Job, logsDir string, now time.Time) error
	Logger *slog.Logger
	// PassEnv names the server's variables a stage and a hook are given
	// (PassEnv the function reads it); nil is DefaultPassEnv.
	PassEnv []string
	// ArtifactsMax is the most every build's artifacts come to, in bytes;
	// past it the oldest builds' go once a build has ended. Zero is no
	// limit.
	ArtifactsMax int64
	// Cache is the keyed caches a stage with cache_scopes restores from and
	// saves to; nil is none, and such a stage fails saying so. CacheURL is
	// where a stage on the server reaches it (a loopback address), and
	// RunnerURL where a stage on a runner does: that one must be set by the
	// operator, since a default would be the address the runners may not
	// reach.
	Cache     *cachesrv.Service
	CacheURL  string
	RunnerURL string
	// BuildTokens makes the KIPP_TOKEN every build is given, a token of
	// the build with the grants its job's permissions: name; nil gives
	// none.
	BuildTokens *auth.BuildTokens
	// Status says a state of a child's build to the host of its repo. It
	// is called for the children of a multibranch job that names a token,
	// and its error is a line in the build's hooks log and nothing more.
	// PublicURL is --base-url as the operator gave it, empty without one: a
	// status then carries no link to the build.
	Status    func(ctx context.Context, job *config.Job, s CommitStatus) error
	PublicURL string
}

// TriggerRequest asks for a build.
type TriggerRequest struct {
	Job    string
	Kind   string // cron, api, web, webhook, scan or after
	Detail string // a webhook's commit, or the upstream build as job#number
	Branch string // the branch a webhook's commit was pushed to
	Tag    string // the tag a webhook's push named, for a push of a tag
	// DefaultBranch is the repo's default branch, as a webhook's push named it.
	DefaultBranch string
	// UpstreamStatus is how the upstream build ended, for an after trigger.
	UpstreamStatus store.Status
	Params         map[string]string
	By             string
	// Commit, BuiltCommit and BaseCommit are what Again carries from the
	// build it repeats: the commit a push named, the commit that build
	// checked out, and the base a pull request was merged into.
	Commit, BuiltCommit, BaseCommit string
}

// Dispatcher owns the queue and the running builds.
type Dispatcher struct {
	st   *store.Store
	r    runner.Runner
	opts Options
	log  *slog.Logger
	// bin is KIPP_BIN for the stages the server runs: its own binary.
	bin string

	slots chan struct{} // one token per build with something running; see joinSlot
	local chan struct{} // one token per stage the server runs itself
	wake  chan struct{}
	quit  chan struct{}  // closed when Run's ctx ends
	wg    sync.WaitGroup // the build goroutines; only Run adds and waits

	// queueMu serialises every decision about a queued build (drop or
	// create, start, cancel), so that two of them never act on the same
	// stale read of the store. It is held across those short store calls,
	// never across a stage, and is always taken before mu.
	queueMu sync.Mutex
	// trimMu is held while the artifacts over ArtifactsMax are removed.
	trimMu sync.Mutex

	// mu guards the fields below and each run's fields. It is never held
	// across a store call or a command.
	mu      sync.Mutex
	snap    *config.Snapshot
	running map[int64]*run              // by build id, from reserved to released
	ending  map[int64]bool              // queued builds being ended, until released
	locks   map[string]int64            // lock name to the build holding it
	done    map[int64][]chan struct{}   // waiters per build
	waiting map[int64]map[string]string // by build and stage: what a stage waits for to be placed
	changes chan struct{}               // closed and replaced at every change; see Changes
	// slotWaiters is how many started stages have a free place and wait for
	// a slot. While there is one, no queued build takes a slot, so that the
	// stage gets the next to free before a build that started later.
	slotWaiters int
	// quiet is the time the server went quiet, zero while it is not; and
	// skipped the jobs whose cron fired meanwhile, which run once when it
	// ends. Both are memory only: a restart ends quiet.
	quiet   time.Time
	skipped map[string]bool
	// quietEnded is told, once, each time quiet is turned off.
	quietEnded func()
}

// run is a build between leaving the queue and releasing what it holds: its
// place among the running builds and the job's locks, which it keeps until
// its hook is done, and a slot, which it holds only while something of it
// runs (see joinSlot).
type run struct {
	build   *store.Build
	job     *config.Job
	cancel  context.CancelCauseFunc // with a cancelledBy when somebody asked for it
	settled bool                    // its final status is decided; Cancel comes too late
	slot    bool                    // it holds a slot
	users   int                     // what runs of it: its prepare, and each stage running; it holds the slot while above 0
}

// cancelledBy is why a build's context ended when a user cancelled it: who.
type cancelledBy string

func (c cancelledBy) Error() string { return "cancelled by " + string(c) }

// childGone is why a build's context ended when its child of a multibranch
// job went: a closed pull request or a deleted branch has nobody left to
// read what the build would say.
type childGone string

func (c childGone) Error() string { return "cancelled: " + string(c) }

// goneWhy is why the job builds no more because its child went, "" for a
// job that is not a gone child.
func goneWhy(job *config.Job) string {
	switch {
	case job == nil || job.Origin == nil || job.Origin.State != config.ChildGone:
		return ""
	case job.Origin.Kind == config.KindBranch:
		return "branch " + job.Origin.Branch + " is gone"
	}
	return fmt.Sprintf("pull request #%d is closed", job.Origin.PR)
}

// timedOut is why a build's context ended when the job's timeout passed.
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
	if opts.Builds <= 0 {
		opts.Builds = cmp.Or(opts.Executors, 4)
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
	return &Dispatcher{
		st: st, r: r, opts: opts, log: opts.Logger, bin: executable(),
		slots: make(chan struct{}, opts.Builds), local: make(chan struct{}, opts.Executors),
		wake: make(chan struct{}, 1), quit: make(chan struct{}), waiting: map[int64]map[string]string{},
		snap:    &config.Snapshot{Jobs: map[string]*config.Job{}},
		running: map[int64]*run{}, ending: map[int64]bool{}, locks: map[string]int64{},
		done: map[int64][]chan struct{}{}, changes: make(chan struct{}),
	}
}

// Builds is how many builds with something running run at once.
func (d *Dispatcher) Builds() int { return cap(d.slots) }

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

// activityLine is a line to add to a job's activity.
type activityLine struct{ job, line string }

// cancelGoneLocked tells every running build whose child s says is gone to
// stop, and returns the activity line of each. One whose final status is
// decided is left to end.
func (d *Dispatcher) cancelGoneLocked(s *config.Snapshot) []activityLine {
	var out []activityLine
	for _, r := range d.running {
		why := goneWhy(s.Jobs[r.build.Job])
		if why == "" || r.settled {
			continue
		}
		r.cancel(childGone(why))
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

// kick wakes the loop: something changed that may let a queued build
// start, and so is a change to announce. mu must not be held.
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
	if d.refusedQuiet(req) {
		return nil, ErrQuiet
	}
	snap := d.Snapshot()
	job := snap.Jobs[req.Job]
	if job == nil {
		return nil, fmt.Errorf("%w: %s", ErrUnknownJob, req.Job)
	}
	if job.IsOwner() {
		return nil, ErrOwnerJob
	}
	if job.IsParent() && !job.Disabled {
		// A multibranch job builds through the child of its default branch.
		if job = snap.DefaultChild(job.Name); job == nil {
			return nil, fmt.Errorf("%w: %s", ErrNoDefaultChild, req.Job)
		}
	}
	if job.Disabled {
		return nil, ErrDisabled
	}
	params, err := job.ResolveParams(req.Params)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadParams, err)
	}
	d.queueMu.Lock()
	defer d.queueMu.Unlock()
	why, err := d.dropReason(ctx, job, req.Kind)
	if err != nil {
		return nil, err
	}
	if why != "" {
		line := fmt.Sprintf("dropped a %s trigger: %s", req.Kind, why)
		if err := d.st.AddActivity(ctx, job.Name, line, time.Now()); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w (%s)", ErrDropped, why)
	}
	b := &store.Build{Job: job.Name, Status: store.Queued, Trigger: req.Kind, TriggerDetail: req.Detail, TriggerBranch: req.Branch, TriggerDefaultBranch: req.DefaultBranch, TriggerTag: req.Tag, UpstreamStatus: req.UpstreamStatus, Params: params, StartedBy: req.By, ConfigHash: snap.Hash,
		PushedCommit: req.Commit, BuiltCommit: req.BuiltCommit, BaseCommit: req.BaseCommit}
	ofChild(b, job)
	if err := d.st.CreateBuild(ctx, b); err != nil {
		return nil, err
	}
	if err := d.supersede(ctx, job, b); err != nil {
		return nil, err
	}
	d.kick()
	return b, nil
}

// supersede, for a supersede job, cancels the other builds of b's job and
// of b's ref (the same branch, the same tag, or none for both) that are
// still queued, and says on the job's activity which build took their
// place. A running build is left to end. A job serves many branches and
// tags, so a push to one never cancels the queued build of another.
func (d *Dispatcher) supersede(ctx context.Context, job *config.Job, b *store.Build) error {
	if job.Concurrency != config.Supersede {
		return nil
	}
	queued, err := d.st.BuildsWithStatus(ctx, store.Queued)
	if err != nil {
		return err
	}
	for _, old := range queued {
		if old.Job != b.Job || old.ID == b.ID || old.TriggerBranch != b.TriggerBranch || old.TriggerTag != b.TriggerTag {
			continue
		}
		if err := d.st.SetBuildStatus(ctx, old.ID, store.Cancelled, time.Now()); err != nil {
			return err
		}
		d.release(old.ID)
		line := fmt.Sprintf("build #%d superseded by #%d", old.Number, b.Number)
		if err := d.st.AddActivity(ctx, b.Job, line, time.Now()); err != nil {
			return err
		}
	}
	return nil
}

// dropReason is non-empty when the job's rule refuses this trigger now:
// a drop job with a build running or queued, or a cron fire with a build
// already queued (one in the queue is enough).
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
	if stopped, err := d.cancelRunning(id, by); stopped || err != nil {
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

// cancelRunning reports whether the build was running and has been told to
// stop. A build whose final status is decided cannot be stopped any more.
func (d *Dispatcher) cancelRunning(id int64, by string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r := d.running[id]
	if r == nil {
		return false, nil
	}
	if r.settled {
		return false, fmt.Errorf("build #%d %w: it is already finishing", r.build.Number, ErrTooLate)
	}
	r.cancel(cancelledBy(by))
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
	// What the stages reaped above were given: a crash leaves the files of
	// the secrets that were in use.
	if rmErr := os.RemoveAll(d.secretFilesDir()); rmErr != nil {
		d.log.Warn("leftover secret files could not be removed", "err", rmErr)
	}
	if rmErr := os.RemoveAll(d.stageTmpDir()); rmErr != nil {
		d.log.Warn("leftover KIPP_TMP directories could not be removed", "err", rmErr)
	}
	return err
}

// lostLine ends the log of a stage its server did not see the end of.
const lostLine = "kipp: the server stopped while this ran"

// reapLost kills what the stages of a dead server left running (a crash
// leaves all of it) and says in each stage's log why the log ends there.
// It returns once every group is gone: a build of the same job, or one
// holding the same lock, must not start beside what the lost one still
// runs. A group that cannot be shown to be its stage's is left running and
// named in a warning, and so is a stage with no group recorded. The hook of a lost build is not looked for.
func (d *Dispatcher) reapLost(stages []store.LostStage) {
	var wg sync.WaitGroup
	for _, s := range stages {
		if s.Pgid <= 0 {
			// The server died between starting the stage and writing its
			// group down, so there is nothing to look for or to kill by.
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
			// The line is kipp's own and holds nothing of the build's to mask.
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

// schedule starts every queued build whose rules allow it, oldest first.
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

// cancelQueued takes off the queue a build that has no reason left to run.
func (d *Dispatcher) cancelQueued(ctx context.Context, b *store.Build, why string) {
	d.endQueued(ctx, b, store.Cancelled, fmt.Sprintf("#%d cancelled before it started: %s", b.Number, why))
}

// endQueued ends a queued build in status and says line on its job's activity.
func (d *Dispatcher) endQueued(ctx context.Context, b *store.Build, status store.Status, line string) {
	now := time.Now()
	// Busy until the line is written: Wait answers a terminal build that is
	// not busy, and the line is part of what it ended with.
	d.mu.Lock()
	d.ending[b.ID] = true
	d.mu.Unlock()
	if err := d.st.SetBuildStatus(ctx, b.ID, status, now); err != nil {
		d.log.Error("ending a queued build", "build", b.ID, "status", status, "err", err)
		d.mu.Lock()
		delete(d.ending, b.ID)
		d.mu.Unlock()
		return // still queued: the next pass tries again, and the line below is written once
	}
	if err := d.st.AddActivity(ctx, b.Job, line, now); err != nil {
		d.log.Error("recording a queued build's end", "build", b.ID, "err", err)
	}
	d.release(b.ID)
}

// start takes the build off the queue if its rules allow it now. The build
// is running in the store before its goroutine exists, so the next pass
// over the queue cannot start it twice.
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
	// Under mu: Queue reads the build of a run.
	d.mu.Lock()
	b.Status, b.StartedAt = store.Running, &now
	d.mu.Unlock()
	d.changed()
	d.wg.Go(func() { d.runBuild(runCtx, r) })
}

// reserve applies the concurrency rule, the named locks and the builds
// limit, in that order, and takes what the build needs when all three allow
// it. The slot it takes is for the build's prepare.
func (d *Dispatcher) reserve(ctx context.Context, b *store.Build, job *config.Job) (context.Context, *run) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.mayStartLocked(b, job) || d.slotWaiters > 0 {
		return nil, nil
	}
	select {
	case d.slots <- struct{}{}:
	default:
		return nil, nil
	}
	for _, l := range job.Locks {
		d.locks[l] = b.ID
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	r := &run{build: b, job: job, cancel: cancel, slot: true, users: 1}
	d.running[b.ID] = r
	return runCtx, r
}

func (d *Dispatcher) mayStartLocked(b *store.Build, job *config.Job) bool {
	if job.Concurrency != config.Parallel {
		for _, r := range d.running {
			if r.build.Job == b.Job {
				return false
			}
		}
	}
	for _, l := range job.Locks {
		if _, held := d.locks[l]; held {
			return false
		}
	}
	return true
}

// joinSlot makes something of the build run: its first takes a slot, and
// the others share it, so a build counts once however many stages it runs
// at once, and counts for nothing while all it does is wait for a place or
// for a slot. With wait, it blocks until a slot is free, and a blocked
// caller is served before any build that tries later; without, it reports
// false when none is. It reports false too when ctx ends first. Each true
// is for one leaveSlot.
func (d *Dispatcher) joinSlot(ctx context.Context, id int64, wait bool) bool {
	d.mu.Lock()
	r := d.running[id]
	if r == nil {
		d.mu.Unlock()
		return false
	}
	if r.users > 0 {
		r.users++
		d.mu.Unlock()
		return true
	}
	select {
	case d.slots <- struct{}{}:
		r.slot, r.users = true, 1
		d.mu.Unlock()
		return true
	default:
	}
	d.mu.Unlock()
	if !wait {
		return false
	}
	select {
	case d.slots <- struct{}{}:
	case <-ctx.Done():
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if r.slot {
		<-d.slots // another stage of the build took one meanwhile
	}
	r.slot = true
	r.users++
	return true
}

// leaveSlot ends what joinSlot made run, gives the slot back when nothing
// else of the build runs, and wakes the loop. The hook runs outside the
// limit, and the job's locks and its place among the running builds stay
// the build's until release.
func (d *Dispatcher) leaveSlot(id int64) {
	d.mu.Lock()
	if r := d.running[id]; r != nil && r.users > 0 {
		if r.users--; r.users == 0 && r.slot {
			r.slot = false
			<-d.slots
		}
	}
	d.mu.Unlock()
	d.kick()
}

// slotWaiter records that a started stage waits for a slot, or no longer does.
func (d *Dispatcher) slotWaiter(on bool) {
	d.mu.Lock()
	if on {
		d.slotWaiters++
	} else {
		d.slotWaiters--
	}
	d.mu.Unlock()
	d.kick()
}

// release gives back what a build still holds, and wakes its waiters and the loop.
func (d *Dispatcher) release(id int64) {
	d.mu.Lock()
	delete(d.ending, id)
	if r := d.running[id]; r != nil {
		for _, l := range r.job.Locks {
			delete(d.locks, l)
		}
		r.cancel(nil)
		delete(d.running, id)
		if r.slot {
			<-d.slots
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
		// Subscribe before reading, so a finish in between is not missed;
		// look for the run after reading, so a start in between is not either.
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

// subscribe returns a channel closed when the build is released.
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
