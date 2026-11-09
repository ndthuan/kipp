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

func (s *Site) runnerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/runners", s.withUser(s.runners))
	mux.HandleFunc("POST /admin/runners/engines", s.withUser(s.addEngine))
	mux.HandleFunc("POST /admin/runners/engines/{name}/enable", s.withUser(s.setEngine(true)))
	mux.HandleFunc("POST /admin/runners/engines/{name}/disable", s.withUser(s.setEngine(false)))
	mux.HandleFunc("GET /admin/runners/engines/{name}/delete", s.withUser(s.confirmDeleteEngine))
	mux.HandleFunc("POST /admin/runners/engines/{name}/delete", s.withUser(s.deleteEngine))
	mux.HandleFunc("POST /admin/runners/runners", s.withUser(s.oneTime(s.addRunner)))
	mux.HandleFunc("POST /admin/runners/runners/{name}/enable", s.withUser(s.setRunner(true)))
	mux.HandleFunc("POST /admin/runners/runners/{name}/disable", s.withUser(s.setRunner(false)))
	mux.HandleFunc("GET /admin/runners/runners/{name}/token", s.withUser(s.confirmRunnerToken))
	mux.HandleFunc("POST /admin/runners/runners/{name}/token", s.withUser(s.oneTime(s.runnerToken)))
	mux.HandleFunc("GET /admin/runners/runners/{name}/delete", s.withUser(s.confirmDeleteRunner))
	mux.HandleFunc("POST /admin/runners/runners/{name}/delete", s.withUser(s.deleteRunner))
}

type runnersData struct {
	Engines []enginepool.EngineStatus
	Runners []api.RunnerInfo
	// ServerVersion is the server's: a runner of another is refused, and
	// its row says so.
	ServerVersion string
	// Engine and Runner are what a refused form sent, to fill it again.
	Engine store.Engine
	Runner api.RunnerInfo
	// Refused names the form that was refused, "engine" or "runner": it comes
	// back open, with the error inside it.
	Refused string
	Notice  string
}

// OSArch is where a runner said it runs, as os/arch.
func (runnersData) OSArch(a api.RunnerInfo) string {
	if a.OS == "" {
		return ""
	}
	return a.OS + "/" + a.Arch
}

func (s *Site) runners(w http.ResponseWriter, r *http.Request, u *store.User) {
	s.runnersPage(w, r, u, http.StatusOK, runnersData{Engine: store.Engine{Max: 1}, Runner: api.RunnerInfo{Executors: 1}}, "")
}

func (s *Site) runnersPage(w http.ResponseWriter, r *http.Request, u *store.User, code int, d runnersData, msg string) {
	var err error
	if d.Engines, err = s.api.Service().Engines(r.Context(), u); err != nil {
		s.fail(w, r, err)
		return
	}
	if d.Runners, err = s.api.Service().Runners(r.Context(), u); err != nil {
		s.fail(w, r, err)
		return
	}
	d.ServerVersion = s.version
	if code == http.StatusOK {
		d.Notice = noticeOf(r)
	}
	s.render(w, r, "runners", code, page{Title: "Runners", Here: "/admin/runners", Error: msg, Data: d, oneTime: true})
}

// refusedForm is the page again with what was refused, or the error page
// for anything else.
func (s *Site) refusedForm(w http.ResponseWriter, r *http.Request, u *store.User, d runnersData, err error) {
	if errors.Is(err, api.ErrBadInput) || errors.Is(err, api.ErrConflict) {
		s.runnersPage(w, r, u, http.StatusBadRequest, d, plainly(err))
		return
	}
	s.fail(w, r, err)
}

func (s *Site) addEngine(w http.ResponseWriter, r *http.Request, u *store.User) {
	e := store.Engine{Name: r.PostFormValue("name"), URL: r.PostFormValue("url"), SSHKey: r.PostFormValue("ssh_key")}
	d := runnersData{Engine: e, Runner: api.RunnerInfo{Executors: 1}, Refused: "engine"}
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
	done(w, r, "/admin/runners", "engine-added")
}

func (s *Site) setEngine(on bool) pageHandler {
	return func(w http.ResponseWriter, r *http.Request, u *store.User) {
		if err := s.api.Service().SetEngineEnabled(r.Context(), u, api.SessionFrom(r.Context()), r.PathValue("name"), on); err != nil {
			s.fail(w, r, err)
			return
		}
		done(w, r, "/admin/runners", "engine-set")
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
	s.confirm(w, r, "/admin/runners", confirmData{
		Heading: "Remove the engine " + name + "?",
		Lines:   []string{line, "A stage whose labels no other engine carries fails until one is added again."},
		Button:  "Remove " + name, Back: "/admin/runners",
	})
}

func (s *Site) deleteEngine(w http.ResponseWriter, r *http.Request, u *store.User) {
	if err := s.api.Service().DeleteEngine(r.Context(), u, api.SessionFrom(r.Context()), r.PathValue("name")); err != nil {
		s.fail(w, r, err)
		return
	}
	done(w, r, "/admin/runners", "engine-gone")
}

func (s *Site) addRunner(w http.ResponseWriter, r *http.Request, u *store.User) {
	name := r.PostFormValue("name")
	d := runnersData{Engine: store.Engine{Max: 1}, Runner: api.RunnerInfo{Name: name, Executors: 1}, Refused: "runner"}
	executors, err := strconv.Atoi(r.PostFormValue("executors"))
	if err != nil {
		s.refusedForm(w, r, u, d, errors.Join(api.ErrBadInput, errors.New("executors: a whole number")))
		return
	}
	d.Runner.Executors = executors
	labels, err := api.ParseLabels(r.PostFormValue("labels"))
	if err != nil {
		s.refusedForm(w, r, u, d, err)
		return
	}
	d.Runner.Labels = labels
	plain, err := s.api.Service().AddRunner(r.Context(), u, api.SessionFrom(r.Context()), name, labels, executors)
	if err != nil {
		s.refusedForm(w, r, u, d, err)
		return
	}
	s.once(w, r, "/admin/runners", onceData{
		Heading: "Token for the runner " + name, Value: plain, Back: "/admin/runners", BackLabel: "Back to Runners",
		Lead: "Start the runner with it, on its own machine: kipp runner --url <this server> --workdir <dir>, the token in KIPP_RUNNER_TOKEN. It is shown this once.",
	})
}

func (s *Site) setRunner(on bool) pageHandler {
	return func(w http.ResponseWriter, r *http.Request, u *store.User) {
		if err := s.api.Service().SetRunnerEnabled(r.Context(), u, api.SessionFrom(r.Context()), r.PathValue("name"), on); err != nil {
			s.fail(w, r, err)
			return
		}
		done(w, r, "/admin/runners", "runner-set")
	}
}

func (s *Site) confirmRunnerToken(w http.ResponseWriter, r *http.Request, u *store.User) {
	name := r.PathValue("name")
	if !s.hasRunner(w, r, u, name) {
		return
	}
	s.confirm(w, r, "/admin/runners", confirmData{
		Heading: "A new token for the runner " + name + "?",
		Lines:   []string{"The token it holds stops working at once: the runner is refused until it is started with the new one."},
		Button:  "Make a new token", Back: "/admin/runners", oneTime: true,
	})
}

func (s *Site) runnerToken(w http.ResponseWriter, r *http.Request, u *store.User) {
	name := r.PathValue("name")
	plain, err := s.api.Service().NewRunnerToken(r.Context(), u, api.SessionFrom(r.Context()), name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.once(w, r, "/admin/runners", onceData{
		Heading: "New token for the runner " + name, Value: plain, Back: "/admin/runners", BackLabel: "Back to Runners",
		Lead: "Start the runner again with it, in KIPP_RUNNER_TOKEN. It is shown this once.",
	})
}

func (s *Site) confirmDeleteRunner(w http.ResponseWriter, r *http.Request, u *store.User) {
	name := r.PathValue("name")
	if !s.hasRunner(w, r, u, name) {
		return
	}
	s.confirm(w, r, "/admin/runners", confirmData{
		Heading: "Remove the runner " + name + "?",
		Lines:   []string{"What it runs fails, and its token is refused. To stop it taking work and let what it runs finish, disable it instead."},
		Button:  "Remove " + name, Back: "/admin/runners",
	})
}

func (s *Site) deleteRunner(w http.ResponseWriter, r *http.Request, u *store.User) {
	if err := s.api.Service().DeleteRunner(r.Context(), u, api.SessionFrom(r.Context()), r.PathValue("name")); err != nil {
		s.fail(w, r, err)
		return
	}
	done(w, r, "/admin/runners", "runner-gone")
}

// hasRunner reports whether the runner is there, and answers the request
// when it is not.
func (s *Site) hasRunner(w http.ResponseWriter, r *http.Request, u *store.User, name string) bool {
	as, err := s.api.Service().Runners(r.Context(), u)
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

// placePath is where an engine's or a runner's actions are.
func placePath(kind, name string) string {
	return "/admin/runners/" + kind + "/" + url.PathEscape(name)
}
