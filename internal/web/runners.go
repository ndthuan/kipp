package web

import (
	"fmt"
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
	mux.HandleFunc(editEngineRoute, s.withUser(s.editEngine))
	mux.HandleFunc("POST /admin/runners/engines/{name}/enable", s.withUser(s.setEngine(true)))
	mux.HandleFunc("POST /admin/runners/engines/{name}/disable", s.withUser(s.setEngine(false)))
	mux.HandleFunc("GET /admin/runners/engines/{name}/delete", s.withUser(s.confirmDeleteEngine))
	mux.HandleFunc("POST /admin/runners/engines/{name}/delete", s.withUser(s.deleteEngine))
	mux.HandleFunc("POST /admin/runners/runners", s.withUser(s.oneTime(s.addRunner)))
	mux.HandleFunc(editRunnerRoute, s.withUser(s.editRunner))
	mux.HandleFunc(builtInRoute, s.withUser(s.setBuiltIn))
	mux.HandleFunc("POST /admin/runners/runners/{name}/enable", s.withUser(s.setRunner(true)))
	mux.HandleFunc("POST /admin/runners/runners/{name}/disable", s.withUser(s.setRunner(false)))
	mux.HandleFunc("GET /admin/runners/runners/{name}/token", s.withUser(s.confirmRunnerToken))
	mux.HandleFunc("POST /admin/runners/runners/{name}/token", s.withUser(s.oneTime(s.runnerToken)))
	mux.HandleFunc("GET /admin/runners/runners/{name}/delete", s.withUser(s.confirmDeleteRunner))
	mux.HandleFunc("POST /admin/runners/runners/{name}/delete", s.withUser(s.deleteRunner))
}

type runnersData struct {
	BuiltIn *api.BuiltInInfo
	Engines []enginepool.EngineStatus
	Runners []api.RunnerInfo
	// ServerVersion is the server's: a runner of another is refused, and
	// its row says so.
	ServerVersion string
	Notice        string
}

// The routes of the runners page's forms: the two that add, and the rows'.
const (
	addEngineRoute  = "POST /admin/runners/engines"
	addRunnerRoute  = "POST /admin/runners/runners"
	editEngineRoute = "POST /admin/runners/engines/{name}"
	editRunnerRoute = "POST /admin/runners/runners/{name}"
	builtInRoute    = "POST /admin/runners/built-in"
)

// OSArch is where a runner said it runs, as os/arch.
func (runnersData) OSArch(a api.RunnerInfo) string {
	if a.OS == "" {
		return ""
	}
	return a.OS + "/" + a.Arch
}

func (s *Site) runners(w http.ResponseWriter, r *http.Request, u *store.User) {
	s.runnersPage(w, r, u, http.StatusOK, fillOf(r, addEngineRoute, addRunnerRoute, editEngineRoute, editRunnerRoute, builtInRoute))
}

// runnersPage is the runners page with fill's form open and filled.
func (s *Site) runnersPage(w http.ResponseWriter, r *http.Request, u *store.User, code int, fill filled) {
	var d runnersData
	var err error
	if d.BuiltIn, err = s.api.Service().BuiltIn(r.Context(), u); err != nil {
		s.fail(w, r, err)
		return
	}
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
	s.render(w, r, "runners", code, page{Title: "Runners", Here: "/admin/runners", Data: d, Fill: fill, oneTime: true})
}

// refusedForm is the page again with the form of route as it was sent, or
// the error page for anything else. A name taken is a 400 here, as it has
// always been, not the 409 of the other forms.
func (s *Site) refusedForm(w http.ResponseWriter, r *http.Request, u *store.User, route string, err error) {
	s.refusedAs(w, r, err, func(_ int, msg string) {
		s.runnersPage(w, r, u, http.StatusBadRequest, filledAs(route, r.PostForm, r.PathValue("name")).saying(msg))
	})
}

// wholeNumber is the form's field read as a whole number.
func wholeNumber(r *http.Request, field string) (int, error) {
	n, err := strconv.Atoi(r.PostFormValue(field))
	if err != nil {
		return 0, fmt.Errorf("%w: %s: a whole number", api.ErrBadInput, field)
	}
	return n, nil
}

// editEngine changes an engine's labels and limit, from its row.
func (s *Site) editEngine(w http.ResponseWriter, r *http.Request, u *store.User) {
	max, err := wholeNumber(r, "max")
	if err != nil {
		s.refusedForm(w, r, u, editEngineRoute, err)
		return
	}
	labels, err := api.ParseLabels(r.PostFormValue("labels"))
	if err != nil {
		s.refusedForm(w, r, u, editEngineRoute, err)
		return
	}
	if err := s.api.Service().SetEngine(r.Context(), u, api.SessionFrom(r.Context()), r.PathValue("name"), api.EngineEdit{Labels: labels, Max: &max}); err != nil {
		s.refusedForm(w, r, u, editEngineRoute, err)
		return
	}
	done(w, r, "/admin/runners", "engine-set")
}

// editRunner changes a standing runner's labels and executors, from its
// row, through the checks kipp runner set makes.
func (s *Site) editRunner(w http.ResponseWriter, r *http.Request, u *store.User) {
	executors, err := wholeNumber(r, "executors")
	if err != nil {
		s.refusedForm(w, r, u, editRunnerRoute, err)
		return
	}
	labels, err := api.ParseLabels(r.PostFormValue("labels"))
	if err != nil {
		s.refusedForm(w, r, u, editRunnerRoute, err)
		return
	}
	if err := s.api.Service().SetRunner(r.Context(), u, api.SessionFrom(r.Context()), r.PathValue("name"), api.RunnerEdit{Labels: labels, Executors: &executors}); err != nil {
		s.refusedForm(w, r, u, editRunnerRoute, err)
		return
	}
	done(w, r, "/admin/runners", "runner-set")
}

// setBuiltIn changes the built-in runner's limit.
func (s *Site) setBuiltIn(w http.ResponseWriter, r *http.Request, u *store.User) {
	executors, err := wholeNumber(r, "executors")
	if err == nil {
		err = s.api.Service().SetBuiltInExecutors(r.Context(), u, api.SessionFrom(r.Context()), executors)
	}
	if err != nil {
		s.refusedForm(w, r, u, builtInRoute, err)
		return
	}
	done(w, r, "/admin/runners", "builtin-set")
}

func (s *Site) addEngine(w http.ResponseWriter, r *http.Request, u *store.User) {
	e := store.Engine{Name: r.PostFormValue("name"), URL: r.PostFormValue("url"), SSHKey: r.PostFormValue("ssh_key")}
	max, err := strconv.Atoi(r.PostFormValue("max"))
	if err != nil {
		s.refusedForm(w, r, u, addEngineRoute, fmt.Errorf("%w: max: a whole number", api.ErrBadInput))
		return
	}
	if e.Labels, err = api.ParseLabels(r.PostFormValue("labels")); err != nil {
		s.refusedForm(w, r, u, addEngineRoute, err)
		return
	}
	e.Max = max
	if err := s.api.Service().AddEngine(r.Context(), u, api.SessionFrom(r.Context()), e); err != nil {
		s.refusedForm(w, r, u, addEngineRoute, err)
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
	executors, err := strconv.Atoi(r.PostFormValue("executors"))
	if err != nil {
		s.refusedForm(w, r, u, addRunnerRoute, fmt.Errorf("%w: executors: a whole number", api.ErrBadInput))
		return
	}
	labels, err := api.ParseLabels(r.PostFormValue("labels"))
	if err != nil {
		s.refusedForm(w, r, u, addRunnerRoute, err)
		return
	}
	plain, err := s.api.Service().AddRunner(r.Context(), u, api.SessionFrom(r.Context()), name, labels, executors)
	if err != nil {
		s.refusedForm(w, r, u, addRunnerRoute, err)
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
