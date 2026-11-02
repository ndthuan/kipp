package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ndthuan/kipp/internal/agenthub"
	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/enginepool"
	"github.com/ndthuan/kipp/internal/store"
)

// AgentInfo is what an admin sees of a standing agent: its record and,
// once it has connected, what the server sees of it now. Never its token.
type AgentInfo struct {
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
	Running    []agenthub.StageRef
	Busy       int
	LastSeenAt *time.Time
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

// Agents are the standing agents by name, for an admin; a token may read
// them.
func (s *Service) Agents(ctx context.Context, u *store.User) ([]AgentInfo, error) {
	if !u.Admin {
		return nil, ErrForbidden
	}
	rows, err := s.deps.Store.Agents(ctx)
	if err != nil {
		return nil, err
	}
	live := map[string]agenthub.Status{}
	if s.deps.Agents != nil {
		for _, a := range s.deps.Agents() {
			if a.Build == 0 { // a container's agent is its engine's
				live[a.Name] = a
			}
		}
	}
	out := make([]AgentInfo, 0, len(rows))
	for _, a := range rows {
		info := AgentInfo{Name: a.Name, Labels: a.Labels, Executors: a.Executors, Enabled: a.Enabled,
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

// placesChanged has the server read the engines and agents again, so that
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

// AddAgent registers a standing agent and returns its token, the one time
// it is seen.
func (s *Service) AddAgent(ctx context.Context, u *store.User, sess *store.Session, name string, labels []string, executors int) (string, error) {
	if err := administers(u, sess); err != nil {
		return "", err
	}
	if err := agenthub.CheckAgent(name, labels, executors); err != nil {
		return "", fmt.Errorf("%w: %w", ErrBadInput, err)
	}
	if _, err := s.deps.Store.Agent(ctx, name); err == nil {
		return "", fmt.Errorf("%w: an agent called %s is there already", ErrConflict, name)
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	plain, hash, err := auth.NewAgentToken()
	if err != nil {
		return "", err
	}
	a := store.Agent{Name: name, Labels: labels, Executors: executors, Enabled: true, TokenHash: hash}
	if err := s.deps.Store.PutAgent(ctx, a); err != nil {
		return "", err
	}
	s.record(ctx, u.Name, "agent.add", name, AgentDetail(&a))
	s.placesChanged(ctx)
	return plain, nil
}

// SetAgentEnabled enables a standing agent or drains it: a disabled one
// takes nothing new, and what it runs finishes.
func (s *Service) SetAgentEnabled(ctx context.Context, u *store.User, sess *store.Session, name string, on bool) error {
	if err := administers(u, sess); err != nil {
		return err
	}
	a, err := s.deps.Store.Agent(ctx, name)
	if err != nil {
		return missing(err)
	}
	a.Enabled = on
	if err := s.deps.Store.PutAgent(ctx, *a); err != nil {
		return err
	}
	s.record(ctx, u.Name, "agent.set", name, AgentDetail(a))
	s.placesChanged(ctx)
	return nil
}

// NewAgentToken gives a standing agent a new token, the one time it is
// seen; the old one dies, and the agent that held it is refused.
func (s *Service) NewAgentToken(ctx context.Context, u *store.User, sess *store.Session, name string) (string, error) {
	if err := administers(u, sess); err != nil {
		return "", err
	}
	a, err := s.deps.Store.Agent(ctx, name)
	if err != nil {
		return "", missing(err)
	}
	plain, hash, err := auth.NewAgentToken()
	if err != nil {
		return "", err
	}
	a.TokenHash = hash
	if err := s.deps.Store.PutAgent(ctx, *a); err != nil {
		return "", err
	}
	s.record(ctx, u.Name, "agent.token", name, "the old token no longer works")
	return plain, nil
}

// DeleteAgent removes a standing agent: what it runs fails, and its token
// is refused.
func (s *Service) DeleteAgent(ctx context.Context, u *store.User, sess *store.Session, name string) error {
	if err := administers(u, sess); err != nil {
		return err
	}
	if err := s.deps.Store.DeleteAgent(ctx, name); err != nil {
		return missing(err)
	}
	s.record(ctx, u.Name, "agent.rm", name, "")
	s.placesChanged(ctx)
	return nil
}

// EngineDetail is what the audit says of an engine, from the pages and the
// command line alike: never a password, which the URL never holds.
func EngineDetail(e *store.Engine) string {
	return fmt.Sprintf("%s, labels %s, max %d, %s", enginepool.Redact(e.URL), strings.Join(e.Labels, ","), e.Max, enabled(e.Enabled))
}

// AgentDetail is what the audit says of an agent: never its token.
func AgentDetail(a *store.Agent) string {
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

func (a *API) agents(w http.ResponseWriter, r *http.Request, u *store.User) {
	as, err := a.svc.Agents(r.Context(), u)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	builds, err := a.svc.BuildsByID(r.Context(), stageBuildIDs(as))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := make([]agentV1, 0, len(as))
	for _, ag := range as {
		out = append(out, toAgentV1(ag, builds))
	}
	writeList(w, out, "")
}
