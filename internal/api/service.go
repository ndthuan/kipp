// Package api is the one way into kipp: the JSON API, and the Service the
// web UI calls. A script reads everything a browser can. Credentials and
// secrets are the exception on the way in: they change only from a
// browser session in sudo mode or from the CLI on the server's machine,
// never with an API token, so that a token that leaks cannot make another
// credential, grant admin or set a secret.
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/cachesrv"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/dispatch"
	"github.com/ndthuan/kipp/internal/enginepool"
	"github.com/ndthuan/kipp/internal/index"
	"github.com/ndthuan/kipp/internal/runnerhub"
	"github.com/ndthuan/kipp/internal/sched"
	"github.com/ndthuan/kipp/internal/store"
	"github.com/ndthuan/kipp/internal/upgrade/engine"
)

// Errors a Service call answers with. Anything else is the server's fault.
var (
	ErrNotFound  = errors.New("not found")
	ErrForbidden = errors.New("forbidden")
	ErrBadInput  = errors.New("bad input")
	ErrConflict  = errors.New("conflict")
	// ErrNoScanner is a scan asked of a server that has none; it is an
	// ErrConflict.
	ErrNoScanner = errors.New("this server does not scan")
	// ErrQuiet is a trigger refused because the server is draining before a
	// restart: 503, to be sent again once it is back.
	ErrQuiet = dispatch.ErrQuiet
	// ErrSudoRequired refuses a change to credentials or secrets from a
	// session that has not given its password in the last SudoFor.
	ErrSudoRequired = errors.New("this change needs your password again")
	// ErrWrongPassword is a password check that failed. It is ErrBadInput
	// too.
	ErrWrongPassword = fmt.Errorf("%w: wrong password", ErrBadInput)
)

// JobsRefused is a reload that read the job files and refused them: the
// one failure of a reload that is the files' and not the server's.
type JobsRefused struct{ Err error }

func (e *JobsRefused) Error() string { return e.Err.Error() }
func (e *JobsRefused) Unwrap() error { return e.Err }

const (
	waitAtMost = 60 * time.Second
	logPoll    = 500 * time.Millisecond
	keepAlive  = 15 * time.Second

	defaultBuilds   = 50
	defaultCache    = 100
	defaultTests    = 100
	defaultHistory  = 20
	jobViewActivity = 20 // lines of activity in a JobView
	activityLines   = 50 // lines Activity returns
	maxRows         = 500
)

// Deps is what the API needs.
type Deps struct {
	Auth     *auth.Service
	Dispatch *dispatch.Dispatcher
	Store    *store.Store
	Reload   func(ctx context.Context) error
	// ReloadError is why the last reload was refused, empty once one has
	// been taken. It may be nil.
	ReloadError func() string
	Version     string
	Logger      *slog.Logger
	// ClientIP is where a login's client address is read behind a proxy;
	// the zero value reads none.
	ClientIP ClientIP
	// Seal seals a secret's value under its name with the data
	// directory's key. Without it a secret is not set.
	Seal func(name string, value []byte) ([]byte, error)
	// Scanner scans the multibranch jobs; it may be nil, and a scan is
	// then refused.
	Scanner *index.Scanner
	// ServerInfo is what the server runs with, as of the call. It may be
	// nil.
	ServerInfo func() ServerInfo
	// Engines and Runners are the places a stage may run on as the server
	// sees them now, and PlacesChanged has it read their records again; each
	// may be nil.
	Engines       func() []enginepool.EngineStatus
	Runners       func() []runnerhub.Status
	PlacesChanged func(ctx context.Context)
	// Listen is the address the server listens on. The cookies of a
	// browser on the server's own machine (LocalSessionCookie) are set
	// and read only when it is a loopback one.
	Listen string
	// Cache is the keyed caches the server keeps; an admin sees and
	// clears them. Without it there are none to see.
	Cache *cachesrv.Service
	// Upgrade upgrades the server; without it the routes answer that this
	// server does not.
	Upgrade *engine.Engine
	// FlakyFlips is how many changes of colour in a job's last builds make
	// it flaky on the overview: 0 for no such section. kipp serve sets it
	// (default overview.DefaultFlips).
	FlakyFlips int
}

// API holds the handlers and the Service.
type API struct {
	deps      Deps
	svc       *Service
	started   time.Time
	wait      time.Duration // the long-poll's limit
	logPoll   time.Duration // how often a stream looks for more
	keepAlive time.Duration // a stream this quiet says it is alive
	recheck   time.Duration // how often a stream is authorised again
	loopback  bool          // Deps.Listen is a loopback address
	streams   streams
}

// New wires an API. Without a Reload, a reload is refused, not a crash.
func New(d Deps) *API {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Reload == nil {
		d.Reload = func(context.Context) error { return errors.New("reload is not configured") }
	}
	if d.ReloadError == nil {
		d.ReloadError = func() string { return "" }
	}
	if d.Seal == nil {
		d.Seal = func(string, []byte) ([]byte, error) { return nil, errors.New("secrets are not configured") }
	}
	if d.ServerInfo == nil {
		d.ServerInfo = func() ServerInfo { return ServerInfo{} }
	}
	a := &API{deps: d, started: time.Now(), wait: waitAtMost, logPoll: logPoll, keepAlive: keepAlive, recheck: recheckEvery, loopback: loopbackListen(d.Listen)}
	a.svc = &Service{deps: &a.deps}
	return a
}

// Zone is the server's zone: what a job with none of its own reads its
// cron in, and what the pages show times in.
func (a *API) Zone() *time.Location { return a.deps.Dispatch.Zone(nil) }

// Service answers for one user with permissions applied.
type Service struct {
	deps *Deps // the API's own, so that the two never differ
}

// Service is the permission-checked operations.
func (a *API) Service() *Service { return a.svc }

// StaleJobs is all that anyone but an admin is told of a refused reload.
const StaleJobs = "The jobs shown may be out of date: the last reload of the job files was refused, and an admin can see why."

// ReloadError says that the job set is not the one on disk, to whoever is
// logged in. An admin is told why; the reason names job files, so the rest
// get StaleJobs: a job one may not view does not exist.
func (s *Service) ReloadError(u *store.User) string {
	why := s.deps.ReloadError()
	switch {
	case u == nil || why == "":
		return ""
	case u.Admin:
		return why
	}
	return StaleJobs
}

// JobSummary is a row of the job list. Removed is a job whose file is gone
// and whose builds are not yet: it has a name and a last build, no more.
type JobSummary struct {
	Name, Group, Description string
	Cron                     string
	Next                     *time.Time
	Last                     *store.Build
	// Recent is the newest finished builds, newest first, for the dots
	// beside Last.
	Recent   []*store.Build
	Removed  bool
	Disabled bool
	// Children is how many live children a multibranch job has, and Worst
	// the worst of their last builds' statuses, empty before any built.
	Children int
	Worst    store.Status
}

// JobView is one job's page. A Removed job has no Job: what is left of it
// is its builds and its activity, to read until retention takes them.
type JobView struct {
	Name     string
	Job      *config.Job
	Removed  bool
	Builds   []*store.Build
	Activity []store.Activity
	Next     *time.Time
	// Zone is the zone the job's cron is read in, and Next is in it.
	Zone *time.Location
	// Before is the build number the page was asked to end before, 0 for
	// the newest builds; Older is the one to ask for to read on, 0 when
	// Builds reaches the job's first.
	Before, Older int
	// OlderActivity says the job has lines of activity beyond Activity.
	OlderActivity bool
	// Parent is the multibranch job of a child, and Children a multibranch
	// job's: the ones it builds, then the gone ones, then the pull
	// requests from forks it lists and does not build.
	Parent   string
	Children []ChildView
	// Folder is the owner job a repo's job belongs to, and Owner and Repos
	// an owner job's own page: how its scanning stands, and its repos.
	Folder string
	Owner  *index.OwnerStatus
	Repos  []RepoView
	// Scan is how a multibranch job's scanning stands; nil for any other
	// job, and when the server has no scanner.
	Scan *index.ScanStatus
	// Vars are the variables the job files give it, by name, each with
	// where it came from; params, secrets and what a stages file adds are
	// not among them. VarsErr is why they could not be made.
	Vars    []config.Var
	VarsErr string
}

// BuildView is one build's page. Needs is what each of the build's stages
// waited for when it ran; for a build that has recorded no stage yet, it is
// what the job plans today. Complete says the build has ended and nothing
// more will be written for it: no stage, test, or line of a log.
type BuildView struct {
	Build    *store.Build
	Stages   []*store.Stage
	Needs    map[string][]string
	Complete bool
	// Group is the job's group as its file says now, and Removed a job
	// whose file is gone: where the build's page says the build belongs.
	Group   string
	Removed bool
}

// buildable is nil if u may build the job: otherwise ErrNotFound (not
// viewable, or not in the job files), ErrForbidden (viewable, building
// refused) or, for a disabled job, ErrConflict. View is asked before the
// job is looked up, so that nothing on the way, a failing store included,
// answers differently for a hidden job and a missing one.
func (s *Service) buildable(ctx context.Context, u *store.User, name string) error {
	if err := s.allowed(ctx, u, name, "view", ErrNotFound); err != nil {
		return err
	}
	job := s.deps.Dispatch.Snapshot().Jobs[name]
	if job == nil {
		return ErrNotFound
	}
	if err := s.allowed(ctx, u, name, "build", ErrForbidden); err != nil {
		return err
	}
	if job.Disabled {
		return fmt.Errorf("%w: %w", ErrConflict, dispatch.ErrDisabled)
	}
	return nil
}

// history is the job for a call that only reads its past, which outlives
// the job's file: a job the snapshot no longer has is answered for too, as
// nil, for as long as the store holds a build of it. Whoever may not view
// the name gets ErrNotFound before anything is looked up, as in buildable.
func (s *Service) history(ctx context.Context, u *store.User, name string) (*config.Job, error) {
	if err := s.allowed(ctx, u, name, "view", ErrNotFound); err != nil {
		return nil, err
	}
	if job := s.deps.Dispatch.Snapshot().Jobs[name]; job != nil {
		return job, nil
	}
	left, err := s.deps.Store.Builds(ctx, name, 1)
	if err == nil && len(left) == 0 {
		err = ErrNotFound
	}
	return nil, err
}

// allowed is nil when u may do action on the job, refusal when not.
func (s *Service) allowed(ctx context.Context, u *store.User, name, action string, refusal error) error {
	ok, err := s.deps.Auth.Can(ctx, u, name, action)
	if err == nil && !ok {
		err = refusal
	}
	return err
}

// complete reports whether b has had everything written that ever will
// be: its status is terminal and the dispatcher has let go of it. The
// status alone does not say, for a junit note and the hook's output are
// written after it. b must have been read before this is asked; what is
// read after it then holds everything.
func (s *Service) complete(b *store.Build) bool {
	return b.Status.Terminal() && !s.deps.Dispatch.Busy(b.ID)
}

// build is a build u may do action on. A build of a hidden job is hidden.
// The permission is on the build's job by name, whether or not the job
// files still hold it: a build outlives its job's file, and while it runs
// somebody must be able to see it and stop it.
func (s *Service) build(ctx context.Context, u *store.User, id int64, action string) (*store.Build, error) {
	b, err := s.deps.Store.Build(ctx, id)
	if err != nil {
		return nil, missing(err)
	}
	if err := s.allowed(ctx, u, b.Job, "view", ErrNotFound); err != nil {
		return nil, err
	}
	if action != "view" {
		if err := s.allowed(ctx, u, b.Job, action, ErrForbidden); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// missing turns the store's not-found into this package's.
func missing(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

// rows is how many rows to ask the store for: never none, never all.
func rows(asked, fallback int) int {
	if asked <= 0 {
		return fallback
	}
	return min(asked, maxRows)
}

// nextFire is when the job's cron fires next, in the zone it is read in:
// never, for a disabled job.
func nextFire(job *config.Job, now time.Time, zone *time.Location) *time.Time {
	if job.Triggers.Cron == "" || job.Disabled {
		return nil
	}
	s, err := sched.Parse(job.Triggers.Cron, job.Name)
	if err != nil {
		return nil
	}
	n := s.Next(now.In(zone))
	if n.IsZero() {
		return nil
	}
	return &n
}

// recentDots is how many finished builds the jobs list draws a dot for.
const recentDots = 5

// Jobs lists what u may view: the jobs of the job files in name order,
// then, in name order too, the removed ones that still have a build.
func (s *Service) Jobs(ctx context.Context, u *store.User) ([]JobSummary, error) {
	snap := s.deps.Dispatch.Snapshot()
	last, err := s.deps.Store.LastBuilds(ctx)
	if err != nil {
		return nil, err
	}
	recent, err := s.deps.Store.RecentFinished(ctx, recentDots)
	if err != nil {
		return nil, err
	}
	parents, err := s.deps.Store.Parents(ctx)
	if err != nil {
		return nil, err
	}
	// A child is listed under its parent, while the parent is a job.
	underParent := func(n string) bool {
		j := snap.Jobs[n]
		return j != nil && (j.Parent != "" || j.Folder != "") || snap.Jobs[parents[n]] != nil
	}
	removed := slices.DeleteFunc(slices.Sorted(maps.Keys(last)), func(n string) bool { return snap.Jobs[n] != nil || underParent(n) })
	files := slices.DeleteFunc(slices.Clone(snap.Names), underParent)
	names, err := s.deps.Auth.Visible(ctx, u, slices.Concat(files, removed))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]JobSummary, 0, len(names))
	for _, n := range names {
		row := JobSummary{Name: n, Last: last[n], Recent: recent[n], Removed: true}
		if j := snap.Jobs[n]; j != nil {
			row = JobSummary{Name: n, Group: j.Group, Description: j.Description, Cron: j.Triggers.Cron, Next: nextFire(j, now, s.deps.Dispatch.Zone(j)), Last: last[n], Recent: recent[n], Disabled: j.Disabled}
			if j.IsOwner() {
				row.Children, row.Worst = reposOf(snap, n, last)
			} else {
				row.Children, row.Worst = childrenOf(snap, n, last)
			}
		}
		out = append(out, row)
	}
	return out, nil
}

// Job is one job with a page of its builds: the newest, or with before
// those numbered below it.
func (s *Service) Job(ctx context.Context, u *store.User, name string, before int) (*JobView, error) {
	job, err := s.history(ctx, u, name)
	if err != nil {
		return nil, err
	}
	v := &JobView{Name: name, Job: job, Removed: job == nil, Before: before}
	// One more than the page shows says whether there is a page after it.
	if v.Builds, err = s.page(ctx, name, before, defaultBuilds+1); err != nil {
		return nil, err
	}
	if len(v.Builds) > defaultBuilds {
		v.Builds = v.Builds[:defaultBuilds]
		v.Older = v.Builds[defaultBuilds-1].Number
	}
	if v.Activity, err = s.deps.Store.Activity(ctx, name, jobViewActivity+1); err != nil {
		return nil, err
	}
	v.Activity, v.OlderActivity = trim(v.Activity, jobViewActivity)
	if job != nil {
		v.Zone = s.deps.Dispatch.Zone(job)
		v.Next = nextFire(job, time.Now(), v.Zone)
		v.Vars, v.VarsErr = fileVars(s.deps.Dispatch.Snapshot().Vars, job)
	}
	if err := s.family(ctx, v); err != nil {
		return v, err
	}
	if sc := s.deps.Scanner; sc != nil && job != nil && job.Multibranch != nil && job.Parent == "" {
		st := sc.Status(name)
		v.Scan = &st
	}
	if job != nil {
		v.Folder = job.Folder
		if err := s.owner(ctx, v, job); err != nil {
			return v, err
		}
	}
	return v, nil
}

// owner fills in the page of an owner job: its scan and its repos.
func (s *Service) owner(ctx context.Context, v *JobView, job *config.Job) error {
	sc := s.deps.Scanner
	if !job.IsOwner() || sc == nil {
		return nil
	}
	v.Owner = sc.Owner(job.Name)
	if v.Owner == nil {
		return nil
	}
	last, err := s.deps.Store.LastBuilds(ctx)
	if err != nil {
		return err
	}
	snap := s.deps.Dispatch.Snapshot()
	for _, r := range v.Owner.Repos {
		row := RepoView{RepoStatus: r}
		row.Children, row.Worst = childrenOf(snap, r.Node, last)
		v.Repos = append(v.Repos, row)
	}
	return nil
}

// ScanNow has the multibranch job's repo read at once, as u with the
// grant to build it. The scan runs on; its outcome is the job's page.
func (s *Service) ScanNow(ctx context.Context, u *store.User, name string) error {
	if err := s.buildable(ctx, u, name); err != nil {
		return err
	}
	if s.deps.Scanner == nil {
		return fmt.Errorf("%w: %w", ErrConflict, ErrNoScanner)
	}
	switch err := s.deps.Scanner.Start(name); {
	case errors.Is(err, index.ErrNotMultibranch):
		return ErrNotFound
	case errors.Is(err, index.ErrNoToken), errors.Is(err, index.ErrScanning):
		return fmt.Errorf("%w: %w", ErrConflict, err)
	default:
		return err
	}
}

// ScanStatus is how the scan of a multibranch job stands; nil on a server
// that has no scanner.
func (s *Service) ScanStatus(name string) *index.ScanStatus {
	if s.deps.Scanner == nil {
		return nil
	}
	st := s.deps.Scanner.Status(name)
	return &st
}

// Builds of a job, newest first: 50 unless limit says otherwise, 500 at
// most, and with before only those numbered below it. More says there are
// builds beyond the page.
func (s *Service) Builds(ctx context.Context, u *store.User, name string, before, limit int) (page []*store.Build, more bool, err error) {
	if _, err := s.history(ctx, u, name); err != nil {
		return nil, false, err
	}
	n := rows(limit, defaultBuilds)
	found, err := s.page(ctx, name, before, n+1)
	if err != nil {
		return nil, false, err
	}
	page, more = trim(found, n)
	return page, more, nil
}

func (s *Service) page(ctx context.Context, name string, before, limit int) ([]*store.Build, error) {
	if before > 0 {
		return s.deps.Store.BuildsBefore(ctx, name, before, limit)
	}
	return s.deps.Store.Builds(ctx, name, limit)
}

// Build is one build with its stages and the needs between them.
func (s *Service) Build(ctx context.Context, u *store.User, id int64) (*BuildView, error) {
	b, err := s.build(ctx, u, id, "view")
	if err != nil {
		return nil, err
	}
	complete := s.complete(b) // asked before the stages are read, so that they are final when it says so
	stages, err := s.deps.Store.Stages(ctx, id)
	if err != nil {
		return nil, err
	}
	needs := make(map[string][]string, len(stages))
	for _, st := range stages {
		needs[st.Name] = st.Needs
	}
	v := &BuildView{Build: b, Stages: stages, Needs: needs, Complete: complete, Removed: true}
	// A job whose file is gone plans nothing: its queued build never starts.
	if job := s.deps.Dispatch.Snapshot().Jobs[b.Job]; job != nil {
		v.Group, v.Removed = job.Group, false
		if len(stages) == 0 && !b.Status.Terminal() {
			v.Needs = job.Needs()
		}
	}
	return v, nil
}

// BuildNumbered is the job's build with that number, as u may see it. A
// hidden job has no builds to ask about, so the refusal comes first.
func (s *Service) BuildNumbered(ctx context.Context, u *store.User, job string, number int) (*store.Build, error) {
	if err := s.allowed(ctx, u, job, "view", ErrNotFound); err != nil {
		return nil, err
	}
	b, err := s.deps.Store.BuildByNumber(ctx, job, number)
	if err != nil {
		return nil, missing(err)
	}
	return b, nil
}

// Trigger starts a build as u. kind is recorded as the build's trigger.
// Params the job refuses are ErrBadInput; a trigger the job's concurrency
// rule drops is ErrConflict, and dispatch.ErrDropped too; so is one for a
// disabled job, and dispatch.ErrDisabled; so is one for an owner job, which
// builds nothing itself.
func (s *Service) Trigger(ctx context.Context, u *store.User, name string, params map[string]string, kind string) (*store.Build, error) {
	if err := s.buildable(ctx, u, name); err != nil {
		return nil, err
	}
	b, err := s.deps.Dispatch.Trigger(ctx, dispatch.TriggerRequest{Job: name, Kind: kind, Params: params, By: u.Name})
	switch {
	case errors.Is(err, dispatch.ErrBadParams):
		return nil, fmt.Errorf("%w: %w", ErrBadInput, err)
	case errors.Is(err, dispatch.ErrDropped), errors.Is(err, dispatch.ErrDisabled), errors.Is(err, dispatch.ErrOwnerJob):
		return nil, fmt.Errorf("%w: %w", ErrConflict, err)
	case errors.Is(err, dispatch.ErrUnknownJob): // a reload removed it meanwhile
		return nil, ErrNotFound
	}
	return b, err
}

// Again queues a build like build id as u, as kind (web or api): same
// params, same checkout. A build that has not ended, a disabled job, a
// dropped trigger and an owner job are ErrConflict as for Trigger; a job a
// reload removed is ErrNotFound.
func (s *Service) Again(ctx context.Context, u *store.User, id int64, kind string) (*store.Build, error) {
	o, err := s.build(ctx, u, id, "build")
	if err != nil {
		return nil, err
	}
	if err := s.buildable(ctx, u, o.Job); err != nil {
		return nil, err
	}
	b, err := s.deps.Dispatch.Again(ctx, id, kind, u.Name)
	switch {
	case errors.Is(err, dispatch.ErrBadParams):
		return nil, fmt.Errorf("%w: %w", ErrBadInput, err)
	case errors.Is(err, dispatch.ErrNotEnded), errors.Is(err, dispatch.ErrDropped), errors.Is(err, dispatch.ErrDisabled), errors.Is(err, dispatch.ErrOwnerJob):
		return nil, fmt.Errorf("%w: %w", ErrConflict, err)
	case errors.Is(err, dispatch.ErrUnknownJob):
		return nil, ErrNotFound
	}
	return b, missing(err)
}

// Keep keeps build id out of retention as u, or with keep false puts it back
// under its job's rules. A build that has not ended is ErrConflict; asking
// for what already holds changes nothing and says nothing in the activity.
func (s *Service) Keep(ctx context.Context, u *store.User, id int64, keep bool) error {
	b, err := s.build(ctx, u, id, "build")
	if err != nil {
		return err
	}
	if !b.Status.Terminal() {
		return fmt.Errorf("%w: build #%d has not ended: it is %s", ErrConflict, b.Number, b.Status)
	}
	if keep == (b.KeptAt != nil) {
		return nil
	}
	now := time.Now()
	line := fmt.Sprintf("#%d kept by %s", b.Number, u.Name)
	if keep {
		err = s.deps.Store.KeepBuild(ctx, id, u.Name, now)
	} else {
		line = fmt.Sprintf("#%d no longer kept, by %s", b.Number, u.Name)
		err = s.deps.Store.UnkeepBuild(ctx, id)
	}
	if err != nil {
		return missing(err)
	}
	if err := s.deps.Store.AddActivity(ctx, b.Job, line, now); err != nil {
		s.deps.Logger.Error("recording a build kept", "job", b.Job, "number", b.Number, "err", err)
	}
	return nil
}

// Cancel stops a build as u. One that has ended, or is past stopping, is
// ErrConflict.
func (s *Service) Cancel(ctx context.Context, u *store.User, id int64) error {
	if _, err := s.build(ctx, u, id, "cancel"); err != nil {
		return err
	}
	err := s.deps.Dispatch.Cancel(ctx, id, u.Name)
	if errors.Is(err, dispatch.ErrTooLate) {
		return fmt.Errorf("%w: %w", ErrConflict, err)
	}
	return missing(err)
}

// Tests of a build.
func (s *Service) Tests(ctx context.Context, u *store.User, id int64) ([]store.Test, error) {
	if _, err := s.build(ctx, u, id, "view"); err != nil {
		return nil, err
	}
	return s.deps.Store.Tests(ctx, id)
}

// TestsPage is a page of a build's test cases, in the order stored: 100
// unless limit says otherwise, 500 at most, after the case with id after.
// More says there are cases beyond the page.
func (s *Service) TestsPage(ctx context.Context, u *store.User, id, after int64, limit int) (page []store.Test, more bool, err error) {
	if _, err := s.build(ctx, u, id, "view"); err != nil {
		return nil, false, err
	}
	n := rows(limit, defaultTests)
	found, err := s.deps.Store.TestsAfter(ctx, id, after, n+1)
	if err != nil {
		return nil, false, err
	}
	page, more = trim(found, n)
	return page, more, nil
}

// GuardResults of a build: what each guard of its stages found.
func (s *Service) GuardResults(ctx context.Context, u *store.User, id int64) ([]store.GuardRow, error) {
	if _, err := s.build(ctx, u, id, "view"); err != nil {
		return nil, err
	}
	return s.deps.Store.GuardResults(ctx, id)
}

// TestHistory of a job's last builds: 20 unless builds says otherwise.
func (s *Service) TestHistory(ctx context.Context, u *store.User, name string, builds int) ([]store.TestRow, error) {
	if _, err := s.history(ctx, u, name); err != nil {
		return nil, err
	}
	return s.deps.Store.TestHistory(ctx, name, rows(builds, defaultHistory))
}

// Activity of a job, newest first: 50 lines unless limit says otherwise, 500
// at most, and with before only the lines older than the one with that id.
// More says there are lines beyond the page.
func (s *Service) Activity(ctx context.Context, u *store.User, name string, before int64, limit int) (page []store.Activity, more bool, err error) {
	if _, err := s.history(ctx, u, name); err != nil {
		return nil, false, err
	}
	n := rows(limit, activityLines)
	found, err := s.deps.Store.ActivityBefore(ctx, name, before, n+1)
	if err != nil {
		return nil, false, err
	}
	page, more = trim(found, n)
	return page, more, nil
}

// LogPath is a stage's log file, with the build, for whoever streams it.
// A stage that is not a name config would take is ErrNotFound: the path is
// always a file directly inside the build's own log directory. The file need not
// exist yet.
func (s *Service) LogPath(ctx context.Context, u *store.User, id int64, stage string) (string, *store.Build, error) {
	b, err := s.build(ctx, u, id, "view")
	if err != nil {
		return "", nil, err
	}
	if !config.ValidName(stage) {
		return "", nil, ErrNotFound
	}
	return s.deps.Dispatch.LogPath(b.Job, b.Number, stage), b, nil
}

// serverLogs are the consoles a build has besides its stages' own.
var serverLogs = []string{config.CloneLog, config.PrepareLog, config.HooksLog}

// HasStage is nil when the build has a log of that name: a stage, planned
// or run, or one of the three the server writes itself. ErrNotFound when it
// has none.
func (s *Service) HasStage(ctx context.Context, u *store.User, id int64, stage string) error {
	v, err := s.Build(ctx, u, id)
	if err != nil {
		return err
	}
	if _, ok := v.Needs[stage]; !ok && !slices.Contains(serverLogs, stage) {
		return ErrNotFound
	}
	return nil
}

// Wait blocks until the build has ended or ctx's deadline passes, then
// returns it as it is. A deadline is how long to wait, not a failure.
func (s *Service) Wait(ctx context.Context, u *store.User, id int64) (*BuildView, error) {
	if _, err := s.build(ctx, u, id, "view"); err != nil {
		return nil, err
	}
	if _, err := s.deps.Dispatch.Wait(ctx, id); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return nil, missing(err)
	}
	return s.Build(context.WithoutCancel(ctx), u, id)
}

// BuildsByID are the builds with the ids, which an answer refers to; one
// that retention has removed is not among them.
func (s *Service) BuildsByID(ctx context.Context, ids []int64) (map[int64]*store.Build, error) {
	out := make(map[int64]*store.Build, len(ids))
	for _, id := range ids {
		if out[id] != nil || id == 0 {
			continue
		}
		b, err := s.deps.Store.Build(ctx, id)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return nil, err
		default:
			out[id] = b
		}
	}
	return out, nil
}

// fileVars are the variables _vars.yml and the job file give job, the
// file's env: over the ones _vars.yml gives, by name.
func fileVars(vars *config.Vars, job *config.Job) ([]config.Var, string) {
	got, err := vars.For(job)
	if err != nil {
		return nil, err.Error()
	}
	got = slices.DeleteFunc(got, func(v config.Var) bool { _, own := job.Env[v.Name]; return own })
	for k, val := range job.Env {
		got = append(got, config.Var{Name: k, Value: val, Source: "job file"})
	}
	slices.SortFunc(got, func(a, b config.Var) int { return strings.Compare(a.Name, b.Name) })
	return got, ""
}
