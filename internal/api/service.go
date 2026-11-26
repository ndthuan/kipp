// Package api is the one way into kipp: the JSON API, and the Service the
// web UI calls. A script reads everything a browser can. Credentials and
// secrets are the exception on the way in: they change only from a
// browser session in sudo mode or from the CLI on the server's machine,
// never with an API token, so that a token that leaks cannot make another
// credential, grant admin or set a secret. Jobs are not part of that
// exception: an admin's token changes them where the server allows edits,
// and a job reads the secrets it names, so an admin's token is to be kept
// as the secrets are.
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
	"github.com/ndthuan/kipp/internal/jobstore"
	"github.com/ndthuan/kipp/internal/runnerhub"
	"github.com/ndthuan/kipp/internal/sched"
	"github.com/ndthuan/kipp/internal/storage"
	"github.com/ndthuan/kipp/internal/store"
	"github.com/ndthuan/kipp/internal/upgrade/engine"
)

// Errors a Service call answers with. Anything else is the server's fault.
var (
	ErrNotFound  = errors.New("not found")
	ErrForbidden = errors.New("forbidden")
	// ErrSelfStart is a build's token starting a build of its own job, or
	// of the multibranch job it is a child of, which builds its default
	// branch's child: a build cannot start itself again. It is an
	// ErrForbidden.
	ErrSelfStart = fmt.Errorf("%w: a build cannot start its own job", ErrForbidden)
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
	jobViewActivity = 20
	activityLines   = 50
	maxRows         = 500
)

// Deps is what the API needs.
type Deps struct {
	Auth          *auth.Service
	Dispatch      *dispatch.Dispatcher
	Store         *store.Store
	Reload        func(ctx context.Context) error
	ReloadError   func() string
	Version       string
	Logger        *slog.Logger
	ClientIP      ClientIP
	Seal          func(name string, value []byte) ([]byte, error)
	Scanner       *index.Scanner
	ServerInfo    func() ServerInfo
	Engines       func() []enginepool.EngineStatus
	Runners       func() []runnerhub.Status
	PlacesChanged func(ctx context.Context)
	Listen        string
	Cache         *cachesrv.Service
	Upgrade       *engine.Engine
	Storage       *storage.Service
	Restart       func()
	FlakyFlips    int
	FlakyQuiet    int
	Jobs          *jobstore.Jobs
}

// API holds the handlers and the Service.
type API struct {
	deps      Deps
	svc       *Service
	started   time.Time
	wait      time.Duration
	logPoll   time.Duration
	keepAlive time.Duration
	recheck   time.Duration
	loopback  bool
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
	deps    *Deps
	streams streams
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
	Recent                   []*store.Build
	Removed                  bool
	Disabled                 bool
	Children                 int
	Worst                    store.Status
	Running                  bool
}

// Status is the status the job's row shows: its last build's, or for a
// multibranch parent that has none of its own, the worst of its children's.
func (j JobSummary) Status() store.Status {
	if j.Children > 0 && j.Last == nil {
		return j.Worst
	}
	if j.Last == nil {
		return ""
	}
	return j.Last.Status
}

// JobView is one job's page. A Removed job has no Job: what is left of it
// is its builds and its activity, to read until retention takes them.
type JobView struct {
	Name          string
	Job           *config.Job
	Removed       bool
	Builds        []*store.Build
	Activity      []store.Activity
	Next          *time.Time
	Zone          *time.Location
	Before, Older int
	OlderActivity bool
	Parent        string
	Children      []ChildView
	Source        jobstore.Origin
	EditWaits     bool
	Folder        string
	Owner         *index.OwnerStatus
	Repos         []RepoView
	Scan          *index.ScanStatus
	Vars          []config.Var
	VarsErr       string
}

// BuildView is one build's page. Needs is what each of the build's stages
// waited for when it ran; for a build that has recorded no stage yet, it is
// what the job plans today. Complete says the build has ended and nothing
// more will be written for it: no stage, test, or line of a log.
type BuildView struct {
	Build     *store.Build
	Stages    []*store.Stage
	Needs     map[string][]string
	Complete  bool
	Group     string
	Removed   bool
	Waiting   map[string]string
	LockWaits map[string]dispatch.StageLockWait
}

func (s *Service) buildable(ctx context.Context, u *store.User, name string) error {
	if err := s.allowed(ctx, u, name, "view", ErrNotFound); err != nil {
		return err
	}
	job := s.deps.Dispatch.Snapshot().Jobs[name]
	if job == nil {
		return ErrNotFound
	}
	if own := u.Build; own != nil && (name == own.Job || name == own.Parent) {
		return ErrSelfStart
	}
	if err := s.allowed(ctx, u, name, "build", ErrForbidden); err != nil {
		return err
	}
	if job.Disabled {
		return fmt.Errorf("%w: %w", ErrConflict, dispatch.ErrDisabled)
	}
	return nil
}

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

func (s *Service) allowed(ctx context.Context, u *store.User, name, action string, refusal error) error {
	ok, err := s.deps.Auth.Can(ctx, u, name, action)
	if err == nil && !ok {
		err = refusal
	}
	return err
}

func (s *Service) complete(b *store.Build) bool {
	return b.Status.Terminal() && !s.deps.Dispatch.Busy(b.ID)
}

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

func missing(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

func rows(asked, fallback int) int {
	if asked <= 0 {
		return fallback
	}
	return min(asked, maxRows)
}

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

func childOfJob(snap *config.Snapshot, parents map[string]string) func(string) bool {
	return func(n string) bool {
		j := snap.Jobs[n]
		return j != nil && (j.Parent != "" || j.Folder != "") || snap.Jobs[parents[n]] != nil
	}
}

func removedWithBuilds(snap *config.Snapshot, last map[string]*store.Build, underParent func(string) bool) []string {
	return slices.DeleteFunc(slices.Sorted(maps.Keys(last)), func(n string) bool { return snap.Jobs[n] != nil || underParent(n) })
}

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
	underParent := childOfJob(snap, parents)
	removed := removedWithBuilds(snap, last, underParent)
	files := slices.DeleteFunc(slices.Clone(snap.Names), underParent)
	names, err := s.deps.Auth.Visible(ctx, u, slices.Concat(files, removed))
	if err != nil {
		return nil, err
	}
	active, err := s.deps.Store.ActiveJobs(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]JobSummary, 0, len(names))
	for _, n := range names {
		row := JobSummary{Name: n, Last: last[n], Recent: recent[n], Removed: true, Running: active[n]}
		if j := snap.Jobs[n]; j != nil {
			row = JobSummary{Name: n, Group: j.Group, Description: j.Description, Cron: j.Triggers.Cron, Next: nextFire(j, now, s.deps.Dispatch.Zone(j)), Last: last[n], Recent: recent[n], Disabled: j.Disabled}
			row.Children, row.Worst, row.Running = familyOf(snap, n, j, last, active)
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
	if s.deps.Jobs != nil {
		v.Source, v.EditWaits = s.deps.Jobs.Where(name)
	}
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

func (s *Service) waits(ctx context.Context, u *store.User, v *BuildView) error {
	id := v.Build.ID
	v.Waiting, v.LockWaits = s.deps.Dispatch.Waiting(id), s.deps.Dispatch.LockWaits(id)
	var jobs []string
	for _, lw := range v.LockWaits {
		if lw.Blocker != nil {
			jobs = append(jobs, lw.Blocker.Job)
		}
	}
	visible, err := s.deps.Auth.Visible(ctx, u, jobs)
	if err != nil {
		return err
	}
	view := map[string]bool{}
	for _, n := range visible {
		view[n] = true
	}
	v.LockWaits = visibleLockWaits(v.LockWaits, view)
	if !u.Admin {
		v.Waiting = hideWaits(v.Waiting, v.LockWaits)
	}
	return nil
}

// Build is one build with its stages and the needs between them.
func (s *Service) Build(ctx context.Context, u *store.User, id int64) (*BuildView, error) {
	b, err := s.build(ctx, u, id, "view")
	if err != nil {
		return nil, err
	}
	complete := s.complete(b)
	stages, err := s.deps.Store.Stages(ctx, id)
	if err != nil {
		return nil, err
	}
	needs := make(map[string][]string, len(stages))
	for _, st := range stages {
		needs[st.Name] = st.Needs
	}
	v := &BuildView{Build: b, Stages: stages, Needs: needs, Complete: complete, Removed: true}
	if !b.Status.Terminal() {
		if err := s.waits(ctx, u, v); err != nil {
			return nil, err
		}
	}
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
// builds nothing itself, and one for a tags' child, which a push builds.
func (s *Service) Trigger(ctx context.Context, u *store.User, name string, params map[string]string, kind string) (*store.Build, error) {
	if err := s.buildable(ctx, u, name); err != nil {
		return nil, err
	}
	b, err := s.deps.Dispatch.Trigger(ctx, dispatch.TriggerRequest{Job: name, Kind: kind, Params: params, By: u.Name})
	switch {
	case errors.Is(err, dispatch.ErrBadParams):
		return nil, fmt.Errorf("%w: %w", ErrBadInput, err)
	case errors.Is(err, dispatch.ErrDropped), errors.Is(err, dispatch.ErrDisabled), errors.Is(err, dispatch.ErrOwnerJob), errors.Is(err, dispatch.ErrNoTag):
		return nil, fmt.Errorf("%w: %w", ErrConflict, err)
	case errors.Is(err, dispatch.ErrUnknownJob):
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
