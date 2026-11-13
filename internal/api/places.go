package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/enginepool"
	"github.com/ndthuan/kipp/internal/runnerhub"
	"github.com/ndthuan/kipp/internal/store"
)

// RunnerInfo is what an admin sees of a standing runner: its record and,
// once it has connected, what the server sees of it now. Never its token.
type RunnerInfo struct {
	Name      string
	Labels    []string
	Executors int
	Enabled   bool
	Online    bool
	Since     time.Time // online or offline since; zero when never seen by this server
	Version   string
	OS, Arch  string
	Stages    []string // what it runs or has been given: job#number/stage
	// Running is Stages by build, and Busy how many there are.
	Running    []runnerhub.StageRef
	Busy       int
	LastSeenAt *time.Time
}

// BuiltInInfo is what an admin sees of the built-in runner, the server's
// own executors.
type BuiltInInfo struct {
	// Executors is its limit, and Busy how many of them stages and prepares
	// hold. RunsStages is false on a server of --executors 0, whose limit
	// counts prepares alone.
	Executors, Busy int
	RunsStages      bool
	// Stages is what it runs now: job#number/stage.
	Stages []string
}

// BuiltIn is the built-in runner as it is now, for an admin.
func (s *Service) BuiltIn(ctx context.Context, u *store.User) (*BuiltInInfo, error) {
	if !u.Admin {
		return nil, ErrForbidden
	}
	q, err := s.deps.Dispatch.Queue(ctx)
	if err != nil {
		return nil, err
	}
	info := &BuiltInInfo{Executors: q.Executors, Busy: q.Busy, RunsStages: q.RunsStages, Stages: []string{}}
	for _, r := range q.Running {
		for _, st := range r.Stages {
			_, waits := r.Waiting[st]
			_, away := r.Remote[st]
			if !waits && !away {
				info.Stages = append(info.Stages, fmt.Sprintf("%s#%d/%s", r.Build.Job, r.Build.Number, st))
			}
		}
	}
	return info, nil
}

// Engines are the Docker engines by name, for an admin; a token may read
// them. Each is its record, and what the server sees of it now once it
// has read it.
func (s *Service) Engines(ctx context.Context, u *store.User) ([]enginepool.EngineStatus, error) {
	if !u.Admin {
		return nil, ErrForbidden
	}
	rows, err := s.deps.Store.Engines(ctx)
	if err != nil {
		return nil, err
	}
	live := map[string]enginepool.EngineStatus{}
	if s.deps.Engines != nil {
		for _, e := range s.deps.Engines() {
			live[e.Name] = e
		}
	}
	out := make([]enginepool.EngineStatus, 0, len(rows))
	for _, e := range rows {
		st := live[e.Name]
		st.Name, st.Kind, st.URL, st.Labels, st.Max, st.Enabled = e.Name, e.Kind, enginepool.Redact(e.URL), e.Labels, e.Max, e.Enabled
		if st.Running == nil {
			st.Running = []string{}
		}
		out = append(out, st)
	}
	return out, nil
}

// Runners are the standing runners by name, for an admin; a token may read
// them.
func (s *Service) Runners(ctx context.Context, u *store.User) ([]RunnerInfo, error) {
	if !u.Admin {
		return nil, ErrForbidden
	}
	rows, err := s.deps.Store.Runners(ctx)
	if err != nil {
		return nil, err
	}
	live := map[string]runnerhub.Status{}
	if s.deps.Runners != nil {
		for _, a := range s.deps.Runners() {
			if a.Build == 0 { // a container's runner is its engine's
				live[a.Name] = a
			}
		}
	}
	out := make([]RunnerInfo, 0, len(rows))
	for _, a := range rows {
		info := RunnerInfo{Name: a.Name, Labels: a.Labels, Executors: a.Executors, Enabled: a.Enabled,
			Version: a.Version, OS: a.OS, Arch: a.Arch, LastSeenAt: a.LastSeenAt, Stages: []string{}}
		if l, ok := live[a.Name]; ok {
			info.Online, info.Since, info.Stages = l.Online, l.Since, append(info.Stages, l.Stages...)
			info.Running, info.Busy = l.Running, l.Busy
			if l.Version != "" {
				info.Version, info.OS, info.Arch = l.Version, l.OS, l.Arch
			}
		}
		out = append(out, info)
	}
	return out, nil
}

// placesChanged has the server read the engines and runners again, so that
// a change made here takes effect now and not at the next reload.
func (s *Service) placesChanged(ctx context.Context) {
	if s.deps.PlacesChanged != nil {
		s.deps.PlacesChanged(ctx)
	}
}

// AddEngine registers an engine. Its URL is checked, never dialled: an
// engine that is asleep is one a stage waits for, not a mistake.
func (s *Service) AddEngine(ctx context.Context, u *store.User, sess *store.Session, e store.Engine) error {
	if err := administers(u, sess); err != nil {
		return err
	}
	e.Kind, e.Enabled = "docker", true
	if err := enginepool.Check(&e); err != nil {
		return fmt.Errorf("%w: %w", ErrBadInput, err)
	}
	if _, err := s.deps.Store.Engine(ctx, e.Name); err == nil {
		return fmt.Errorf("%w: an engine called %s is there already", ErrConflict, e.Name)
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if err := s.deps.Store.PutEngine(ctx, e); err != nil {
		return err
	}
	s.record(ctx, u.Name, "engine.add", e.Name, EngineDetail(&e))
	s.placesChanged(ctx)
	return nil
}

// SetEngineEnabled enables an engine or disables it: a disabled one takes
// no new container, and the ones it runs finish.
func (s *Service) SetEngineEnabled(ctx context.Context, u *store.User, sess *store.Session, name string, on bool) error {
	if err := administers(u, sess); err != nil {
		return err
	}
	e, err := s.deps.Store.Engine(ctx, name)
	if err != nil {
		return missing(err)
	}
	e.Enabled = on
	if err := s.deps.Store.PutEngine(ctx, *e); err != nil {
		return err
	}
	s.record(ctx, u.Name, "engine.set", name, EngineDetail(e))
	s.placesChanged(ctx)
	return nil
}

// DeleteEngine removes an engine; the containers it runs finish.
func (s *Service) DeleteEngine(ctx context.Context, u *store.User, sess *store.Session, name string) error {
	if err := administers(u, sess); err != nil {
		return err
	}
	if err := s.deps.Store.DeleteEngine(ctx, name); err != nil {
		return missing(err)
	}
	s.record(ctx, u.Name, "engine.rm", name, "")
	s.placesChanged(ctx)
	return nil
}

// AddRunner registers a standing runner and returns its token, the one time
// it is seen.
func (s *Service) AddRunner(ctx context.Context, u *store.User, sess *store.Session, name string, labels []string, executors int) (string, error) {
	if err := administers(u, sess); err != nil {
		return "", err
	}
	if err := runnerhub.CheckRunner(name, labels, executors); err != nil {
		return "", fmt.Errorf("%w: %w", ErrBadInput, err)
	}
	if _, err := s.deps.Store.Runner(ctx, name); err == nil {
		return "", fmt.Errorf("%w: a runner called %s is there already", ErrConflict, name)
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	plain, hash, err := auth.NewRunnerToken()
	if err != nil {
		return "", err
	}
	a := store.Runner{Name: name, Labels: labels, Executors: executors, Enabled: true, TokenHash: hash}
	if err := s.deps.Store.PutRunner(ctx, a); err != nil {
		return "", err
	}
	s.record(ctx, u.Name, "runner.add", name, RunnerDetail(&a))
	s.placesChanged(ctx)
	return plain, nil
}

// SetRunnerEnabled enables a standing runner or drains it: a disabled one
// takes nothing new, and what it runs finishes.
func (s *Service) SetRunnerEnabled(ctx context.Context, u *store.User, sess *store.Session, name string, on bool) error {
	if err := administers(u, sess); err != nil {
		return err
	}
	a, err := s.deps.Store.Runner(ctx, name)
	if err != nil {
		return missing(err)
	}
	a.Enabled = on
	if err := s.deps.Store.PutRunner(ctx, *a); err != nil {
		return err
	}
	s.record(ctx, u.Name, "runner.set", name, RunnerDetail(a))
	s.placesChanged(ctx)
	return nil
}

// NewRunnerToken gives a standing runner a new token, the one time it is
// seen; the old one dies, and the runner that held it is refused.
func (s *Service) NewRunnerToken(ctx context.Context, u *store.User, sess *store.Session, name string) (string, error) {
	if err := administers(u, sess); err != nil {
		return "", err
	}
	a, err := s.deps.Store.Runner(ctx, name)
	if err != nil {
		return "", missing(err)
	}
	plain, hash, err := auth.NewRunnerToken()
	if err != nil {
		return "", err
	}
	a.TokenHash = hash
	if err := s.deps.Store.PutRunner(ctx, *a); err != nil {
		return "", err
	}
	s.record(ctx, u.Name, "runner.token", name, "the old token no longer works")
	return plain, nil
}

// DeleteRunner removes a standing runner: what it runs fails, and its token
// is refused.
func (s *Service) DeleteRunner(ctx context.Context, u *store.User, sess *store.Session, name string) error {
	if err := administers(u, sess); err != nil {
		return err
	}
	if err := s.deps.Store.DeleteRunner(ctx, name); err != nil {
		return missing(err)
	}
	s.record(ctx, u.Name, "runner.rm", name, "")
	s.placesChanged(ctx)
	return nil
}

// EngineDetail is what the audit says of an engine, from the pages and the
// command line alike: never a password, which the URL never holds.
func EngineDetail(e *store.Engine) string {
	return fmt.Sprintf("%s, labels %s, max %d, %s", enginepool.Redact(e.URL), strings.Join(e.Labels, ","), e.Max, enabled(e.Enabled))
}

// RunnerDetail is what the audit says of a runner: never its token.
func RunnerDetail(a *store.Runner) string {
	return fmt.Sprintf("labels %s, executors %d, %s", strings.Join(a.Labels, ","), a.Executors, enabled(a.Enabled))
}

func enabled(on bool) string {
	if on {
		return "enabled"
	}
	return "disabled"
}

// ParseLabels reads a form's comma-separated labels.
func ParseLabels(list string) ([]string, error) {
	labels, err := config.ParseLabels(list)
	if err != nil {
		return nil, fmt.Errorf("%w: labels: %w", ErrBadInput, err)
	}
	return labels, nil
}

func (a *API) engines(w http.ResponseWriter, r *http.Request, u *store.User) {
	es, err := a.svc.Engines(r.Context(), u)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := make([]engineV1, 0, len(es))
	for _, e := range es {
		out = append(out, toEngineV1(e))
	}
	writeList(w, out, "")
}

func (a *API) runners(w http.ResponseWriter, r *http.Request, u *store.User) {
	as, err := a.svc.Runners(r.Context(), u)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	builds, err := a.svc.BuildsByID(r.Context(), stageBuildIDs(as))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := make([]runnerV1, 0, len(as))
	for _, ag := range as {
		out = append(out, toRunnerV1(ag, builds))
	}
	writeList(w, out, "")
}
