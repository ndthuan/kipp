package index

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/dispatch"
	"github.com/ndthuan/kipp/internal/github"
	"github.com/ndthuan/kipp/internal/store"
)

const (
	maxScans   = 2
	scanTick   = time.Minute
	noScanText = "no scan, no statuses: no token"
)

var (
	// ErrNoToken is a scan of a job that names no token: kipp has nothing
	// to call GitHub with.
	ErrNoToken = errors.New(noScanText)
	// ErrScanning is a scan asked for while one of the same job runs.
	ErrScanning = errors.New("a scan of this job is already running")
	// ErrNotMultibranch is a scan of a name that is no multibranch job,
	// nor an owner job.
	ErrNotMultibranch = errors.New("not a multibranch or owner job")
)

// ScanStatus is what is known of a multibranch job's scanning, for its
// page and the API: whether it can scan at all, and how the last one went.
type ScanStatus struct {
	Token   bool
	Every   time.Duration
	Running bool
	At      time.Time
	Result  string
	Err     string
}

// Ended is when the last scan ended, nil before the first.
func (s ScanStatus) Ended() *time.Time {
	if s.At.IsZero() {
		return nil
	}
	return &s.At
}

// AppClient is the client of a GitHub App's installation on the account
// called login. Its errors never hold the App's key.
type AppClient func(ctx context.Context, app *config.App, login string) (*github.Client, error)

func clientFor(ctx context.Context, api string, token func(context.Context, string) (string, error), app AppClient, a *config.App, tokenName, login string) (*github.Client, error) {
	if a == nil {
		tok, err := token(ctx, tokenName)
		if err != nil {
			return nil, err
		}
		return github.New(api, tok), nil
	}
	if app == nil {
		return nil, fmt.Errorf("this server cannot sign in as the GitHub App %s", a.Name)
	}
	return app(ctx, a, login)
}

// ScannerOptions are what a Scanner is given.
type ScannerOptions struct {
	API     string
	Token   func(ctx context.Context, name string) (string, error)
	Quiet   func() bool
	App     AppClient
	Trigger func(ctx context.Context, req dispatch.TriggerRequest) (*store.Build, error)
	Hook    func(ctx context.Context, owner string) HookPlan
	Log     *slog.Logger
}

// Scanner scans the multibranch jobs: once at start and at every reload,
// then every multibranch.scan, and when it is asked to. At most two scans
// run at once, and never two of the same job.
type Scanner struct {
	ix   *Index
	opts ScannerOptions
	api  string
	base context.Context
	now  func() time.Time
	sem  chan struct{}
	wg   sync.WaitGroup

	mu     sync.Mutex
	state  map[string]*scanState
	warned map[string]bool
}

type scanState struct {
	status     ScanStatus
	due        time.Time
	again      bool
	publicOnly bool
}

// NewScanner is a scanner of the index's jobs. Its scans end with base.
func NewScanner(base context.Context, ix *Index, opts ScannerOptions) *Scanner {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	return &Scanner{ix: ix, opts: opts, api: opts.API, base: base, now: time.Now, sem: make(chan struct{}, maxScans),
		state: map[string]*scanState{}, warned: map[string]bool{}}
}

// Wait returns once every scan that has started has ended.
func (s *Scanner) Wait() { s.wg.Wait() }

// Run looks at the clock until ctx ends, and starts the scans that are due.
func (s *Scanner) Run(ctx context.Context) {
	t := time.NewTicker(scanTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.tick(s.now())
		}
	}
}

// Reload is called with every new job set: the jobs are scanned now, and
// the ones that left are forgotten.
func (s *Scanner) Reload() {
	live := map[string]bool{}
	for _, name := range append(s.ix.Parents(), s.ix.Owners()...) {
		live[name] = true
	}
	s.mu.Lock()
	for name, st := range s.state {
		if !live[name] {
			delete(s.state, name)
			continue
		}
		st.due = time.Time{}
	}
	s.mu.Unlock()
	s.warnOverlaps()
	s.tick(s.now())
}

func (s *Scanner) tick(now time.Time) {
	for _, name := range append(s.ix.Parents(), s.ix.Owners()...) {
		if every, ok := s.interval(name); ok {
			s.start(name, every, now)
		}
	}
}

func (s *Scanner) interval(name string) (time.Duration, bool) {
	if o := s.ix.Owner(name); o != nil {
		return o.Scan, o.Scan > 0
	}
	m := s.ix.Multibranch(name)
	if m == nil {
		return 0, false
	}
	return m.Scan, m.Scan > 0 && m.Token != ""
}

func (s *Scanner) start(name string, every time.Duration, now time.Time) {
	s.mu.Lock()
	st := s.stateLocked(name)
	due := !st.status.Running && !now.Before(st.due)
	if due {
		st.due = now.Add(every)
		st.status.Running = true
	}
	s.mu.Unlock()
	if due {
		s.wg.Go(func() { _ = s.run(s.base, name) })
	}
}

func (s *Scanner) stateLocked(name string) *scanState {
	st := s.state[name]
	if st == nil {
		st = &scanState{}
		s.state[name] = st
	}
	return st
}

// Status is what is known of the job's scanning.
func (s *Scanner) Status(name string) ScanStatus {
	s.mu.Lock()
	var out ScanStatus
	if st := s.state[name]; st != nil {
		out = st.status
	}
	s.mu.Unlock()
	if o := s.ix.Owner(name); o != nil {
		out.Token, out.Every = true, o.Scan
	} else if m := s.ix.Multibranch(name); m != nil {
		out.Token = m.Authenticated()
		if slices.Contains(s.ix.Parents(), name) {
			out.Every = m.Scan
		}
	}
	return out
}

func (s *Scanner) claim(name string) error {
	if s.ix.Owner(name) == nil {
		switch m := s.ix.Multibranch(name); {
		case m == nil:
			return fmt.Errorf("%w: %s", ErrNotMultibranch, name)
		case !m.Authenticated():
			return ErrNoToken
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stateLocked(name)
	if st.status.Running {
		return ErrScanning
	}
	st.status.Running = true
	return nil
}

// Start scans the job in the background: "Scan now". It returns at once,
// and the outcome is the job's Status.
func (s *Scanner) Start(name string) error {
	if err := s.claim(name); err != nil {
		return err
	}
	s.wg.Go(func() { _ = s.run(s.base, name) })
	return nil
}

// Rescan asks for a scan of the job soon: now, or as soon as the one that
// runs ends, so that what the event it answers changed is read by a scan
// that began after it. It returns at once.
func (s *Scanner) Rescan(name string) error {
	if s.ix.Owner(name) == nil && s.ix.Multibranch(name) == nil {
		return fmt.Errorf("%w: %s", ErrNotMultibranch, name)
	}
	s.mu.Lock()
	st := s.stateLocked(name)
	if st.status.Running {
		st.again = true
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	if err := s.Start(name); err != nil && !errors.Is(err, ErrScanning) {
		return err
	} else if errors.Is(err, ErrScanning) {
		s.mu.Lock()
		s.stateLocked(name).again = true
		s.mu.Unlock()
	}
	return nil
}

// Scan scans the job and returns once it has: the error is the scan's
// too, as the job's Status has it.
func (s *Scanner) Scan(ctx context.Context, name string) error {
	if err := s.claim(name); err != nil {
		return err
	}
	return s.run(ctx, name)
}

func (s *Scanner) run(ctx context.Context, name string) error {
	var result string
	var err error
	select {
	case s.sem <- struct{}{}:
		result, err = s.scan(ctx, name)
		<-s.sem
	case <-ctx.Done():
		err = ctx.Err()
	}
	s.mu.Lock()
	st := s.stateLocked(name)
	st.status.Running, st.status.At, st.status.Result, st.status.Err = false, s.now(), result, ""
	if err != nil {
		st.status.Err = err.Error()
	}
	again := st.again
	st.again = false
	s.mu.Unlock()
	if err != nil {
		s.opts.Log.Warn("scan", "job", name, "err", err)
	}
	if again && ctx.Err() == nil {
		_ = s.Start(name)
	}
	return err
}

func (s *Scanner) scan(ctx context.Context, name string) (string, error) {
	if s.opts.Quiet != nil && s.opts.Quiet() {
		return "skipped: kipp is quiet", nil
	}
	if o := s.ix.Owner(name); o != nil {
		return s.scanOwner(ctx, name, o)
	}
	m := s.ix.Multibranch(name)
	if m == nil {
		return "", fmt.Errorf("%w: %s", ErrNotMultibranch, name)
	}
	gh, err := clientFor(ctx, s.api, s.opts.Token, s.opts.App, m.App, m.Token, repoOwner(m.Repo))
	if err != nil {
		return "", err
	}
	res, err := s.scanRepo(ctx, name, gh)
	return res.text(), err
}

type repoScan struct {
	summary         string
	queued, dropped int
}

func (r repoScan) text() string {
	out := fmt.Sprintf("%s, %d queued", r.summary, r.queued)
	if r.dropped > 0 {
		out += fmt.Sprintf(", %d dropped by the job's rule", r.dropped)
	}
	return out
}

func (s *Scanner) scanRepo(ctx context.Context, name string, gh *github.Client) (repoScan, error) {
	rep, err := s.ix.Scan(ctx, name, gh)
	if err != nil {
		return repoScan{}, err
	}
	res := repoScan{summary: rep.Summary}
	var errs []error
	for _, req := range rep.Builds {
		switch _, err := s.opts.Trigger(ctx, req); {
		case errors.Is(err, dispatch.ErrDropped), errors.Is(err, dispatch.ErrDisabled):
			res.dropped++
		case err != nil:
			errs = append(errs, fmt.Errorf("queueing %s: %w", req.Job, err))
		default:
			res.queued++
		}
	}
	return res, errors.Join(errs...)
}

func repoOwner(repo string) string {
	owner, _, _ := strings.Cut(repo, "/")
	return owner
}
