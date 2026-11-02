package web

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/enginepool"
	"github.com/ndthuan/kipp/internal/store"
)

func (s *Site) agentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/agents", s.withUser(s.agents))
	mux.HandleFunc("POST /admin/agents/engines", s.withUser(s.addEngine))
	mux.HandleFunc("POST /admin/agents/engines/{name}/enable", s.withUser(s.setEngine(true)))
	mux.HandleFunc("POST /admin/agents/engines/{name}/disable", s.withUser(s.setEngine(false)))
	mux.HandleFunc("GET /admin/agents/engines/{name}/delete", s.withUser(s.confirmDeleteEngine))
	mux.HandleFunc("POST /admin/agents/engines/{name}/delete", s.withUser(s.deleteEngine))
	mux.HandleFunc("POST /admin/agents/agents", s.withUser(s.oneTime(s.addAgent)))
	mux.HandleFunc("POST /admin/agents/agents/{name}/enable", s.withUser(s.setAgent(true)))
	mux.HandleFunc("POST /admin/agents/agents/{name}/disable", s.withUser(s.setAgent(false)))
	mux.HandleFunc("GET /admin/agents/agents/{name}/token", s.withUser(s.confirmAgentToken))
	mux.HandleFunc("POST /admin/agents/agents/{name}/token", s.withUser(s.oneTime(s.agentToken)))
	mux.HandleFunc("GET /admin/agents/agents/{name}/delete", s.withUser(s.confirmDeleteAgent))
	mux.HandleFunc("POST /admin/agents/agents/{name}/delete", s.withUser(s.deleteAgent))
}

type agentsData struct {
	Engines []enginepool.EngineStatus
	Agents  []api.AgentInfo
	// ServerVersion is the server's: an agent of another is refused, and
	// its row says so.
	ServerVersion string
	// Engine and Agent are what a refused form sent, to fill it again.
	Engine store.Engine
	Agent  api.AgentInfo
	// Refused names the form that was refused, "engine" or "agent": it comes
	// back open, with the error inside it.
	Refused string
	Notice  string
}

// OSArch is where an agent said it runs, as os/arch.
func (agentsData) OSArch(a api.AgentInfo) string {
	if a.OS == "" {
		return ""
	}
	return a.OS + "/" + a.Arch
}

func (s *Site) agents(w http.ResponseWriter, r *http.Request, u *store.User) {
	s.agentsPage(w, r, u, http.StatusOK, agentsData{Engine: store.Engine{Max: 1}, Agent: api.AgentInfo{Executors: 1}}, "")
}

func (s *Site) agentsPage(w http.ResponseWriter, r *http.Request, u *store.User, code int, d agentsData, msg string) {
	var err error
	if d.Engines, err = s.api.Service().Engines(r.Context(), u); err != nil {
		s.fail(w, r, err)
		return
	}
	if d.Agents, err = s.api.Service().Agents(r.Context(), u); err != nil {
		s.fail(w, r, err)
		return
	}
	d.ServerVersion = s.version
	if code == http.StatusOK {
		d.Notice = noticeOf(r)
	}
	s.render(w, r, "agents", code, page{Title: "Agents", Here: "/admin/agents", Error: msg, Data: d, oneTime: true})
}

// refusedForm is the page again with what was refused, or the error page
// for anything else.
func (s *Site) refusedForm(w http.ResponseWriter, r *http.Request, u *store.User, d agentsData, err error) {
	if errors.Is(err, api.ErrBadInput) || errors.Is(err, api.ErrConflict) {
		s.agentsPage(w, r, u, http.StatusBadRequest, d, plainly(err))
		return
	}
	s.fail(w, r, err)
}

func (s *Site) addEngine(w http.ResponseWriter, r *http.Request, u *store.User) {
	e := store.Engine{Name: r.PostFormValue("name"), URL: r.PostFormValue("url"), SSHKey: r.PostFormValue("ssh_key")}
	d := agentsData{Engine: e, Agent: api.AgentInfo{Executors: 1}, Refused: "engine"}
	max, err := strconv.Atoi(r.PostFormValue("max"))
	if err != nil {
		s.refusedForm(w, r, u, d, errors.Join(api.ErrBadInput, errors.New("max: a whole number")))
		return
	}
	d.Engine.Max = max
	if e.Labels, err = api.ParseLabels(r.PostFormValue("labels")); err != nil {
		s.refusedForm(w, r, u, d, err)
		return
	}
	e.Max = max
	if err := s.api.Service().AddEngine(r.Context(), u, api.SessionFrom(r.Context()), e); err != nil {
		d.Engine.Labels = e.Labels
		s.refusedForm(w, r, u, d, err)
		return
	}
	done(w, r, "/admin/agents", "engine-added")
}

func (s *Site) setEngine(on bool) pageHandler {
	return func(w http.ResponseWriter, r *http.Request, u *store.User) {
		if err := s.api.Service().SetEngineEnabled(r.Context(), u, api.SessionFrom(r.Context()), r.PathValue("name"), on); err != nil {
			s.fail(w, r, err)
			return
		}
		done(w, r, "/admin/agents", "engine-set")
	}
}

func (s *Site) confirmDeleteEngine(w http.ResponseWriter, r *http.Request, u *store.User) {
	name := r.PathValue("name")
	es, err := s.api.Service().Engines(r.Context(), u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	line := "It runs no container now."
	found := false
	for _, e := range es {
		if e.Name == name {
			found = true
			if len(e.Running) > 0 {
				line = "The containers it runs finish; it takes no new one."
			}
		}
	}
	if !found {
		s.fail(w, r, api.ErrNotFound)
		return
	}
	s.confirm(w, r, "/admin/agents", confirmData{
		Heading: "Remove the engine " + name + "?",
		Lines:   []string{line, "A stage whose labels no other engine carries fails until one is added again."},
		Button:  "Remove " + name, Back: "/admin/agents",
	})
}

func (s *Site) deleteEngine(w http.ResponseWriter, r *http.Request, u *store.User) {
	if err := s.api.Service().DeleteEngine(r.Context(), u, api.SessionFrom(r.Context()), r.PathValue("name")); err != nil {
		s.fail(w, r, err)
		return
	}
	done(w, r, "/admin/agents", "engine-gone")
}

func (s *Site) addAgent(w http.ResponseWriter, r *http.Request, u *store.User) {
	name := r.PostFormValue("name")
	d := agentsData{Engine: store.Engine{Max: 1}, Agent: api.AgentInfo{Name: name, Executors: 1}, Refused: "agent"}
	executors, err := strconv.Atoi(r.PostFormValue("executors"))
	if err != nil {
		s.refusedForm(w, r, u, d, errors.Join(api.ErrBadInput, errors.New("executors: a whole number")))
		return
	}
	d.Agent.Executors = executors
	labels, err := api.ParseLabels(r.PostFormValue("labels"))
	if err != nil {
		s.refusedForm(w, r, u, d, err)
		return
	}
	d.Agent.Labels = labels
	plain, err := s.api.Service().AddAgent(r.Context(), u, api.SessionFrom(r.Context()), name, labels, executors)
	if err != nil {
		s.refusedForm(w, r, u, d, err)
		return
	}
	s.once(w, r, "/admin/agents", onceData{
		Heading: "Token for the agent " + name, Value: plain, Back: "/admin/agents", BackLabel: "Back to Agents",
		Lead: "Start the agent with it, on its own machine: kipp agent --url <this server> --workdir <dir>, the token in KIPP_AGENT_TOKEN. It is shown this once.",
	})
}

func (s *Site) setAgent(on bool) pageHandler {
	return func(w http.ResponseWriter, r *http.Request, u *store.User) {
		if err := s.api.Service().SetAgentEnabled(r.Context(), u, api.SessionFrom(r.Context()), r.PathValue("name"), on); err != nil {
			s.fail(w, r, err)
			return
		}
		done(w, r, "/admin/agents", "agent-set")
	}
}

func (s *Site) confirmAgentToken(w http.ResponseWriter, r *http.Request, u *store.User) {
	name := r.PathValue("name")
	if !s.hasAgent(w, r, u, name) {
		return
	}
	s.confirm(w, r, "/admin/agents", confirmData{
		Heading: "A new token for the agent " + name + "?",
		Lines:   []string{"The token it holds stops working at once: the agent is refused until it is started with the new one."},
		Button:  "Make a new token", Back: "/admin/agents", oneTime: true,
	})
}

func (s *Site) agentToken(w http.ResponseWriter, r *http.Request, u *store.User) {
	name := r.PathValue("name")
	plain, err := s.api.Service().NewAgentToken(r.Context(), u, api.SessionFrom(r.Context()), name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.once(w, r, "/admin/agents", onceData{
		Heading: "New token for the agent " + name, Value: plain, Back: "/admin/agents", BackLabel: "Back to Agents",
		Lead: "Start the agent again with it, in KIPP_AGENT_TOKEN. It is shown this once.",
	})
}

func (s *Site) confirmDeleteAgent(w http.ResponseWriter, r *http.Request, u *store.User) {
	name := r.PathValue("name")
	if !s.hasAgent(w, r, u, name) {
		return
	}
	s.confirm(w, r, "/admin/agents", confirmData{
		Heading: "Remove the agent " + name + "?",
		Lines:   []string{"What it runs fails, and its token is refused. To stop it taking work and let what it runs finish, disable it instead."},
		Button:  "Remove " + name, Back: "/admin/agents",
	})
}

func (s *Site) deleteAgent(w http.ResponseWriter, r *http.Request, u *store.User) {
	if err := s.api.Service().DeleteAgent(r.Context(), u, api.SessionFrom(r.Context()), r.PathValue("name")); err != nil {
		s.fail(w, r, err)
		return
	}
	done(w, r, "/admin/agents", "agent-gone")
}

// hasAgent reports whether the agent is there, and answers the request
// when it is not.
func (s *Site) hasAgent(w http.ResponseWriter, r *http.Request, u *store.User, name string) bool {
	as, err := s.api.Service().Agents(r.Context(), u)
	if err != nil {
		s.fail(w, r, err)
		return false
	}
	for _, a := range as {
		if a.Name == name {
			return true
		}
	}
	s.fail(w, r, api.ErrNotFound)
	return false
}

// placePath is where an engine's or an agent's actions are.
func placePath(kind, name string) string { return "/admin/agents/" + kind + "/" + url.PathEscape(name) }
