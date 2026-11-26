package web

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/github"
	"github.com/ndthuan/kipp/internal/store"
	"github.com/ndthuan/kipp/internal/webhook"
)

// AppFlow is what the page that makes a GitHub App from a manifest needs
// of the server. Without it the page says it is off.
type AppFlow struct {
	DataDir string
	BaseURL string
	API     string
	WebBase string
}

// WithApps turns on the page that makes GitHub Apps.
func WithApps(f AppFlow) Option {
	return func(s *Site) {
		f.BaseURL = strings.TrimRight(f.BaseURL, "/")
		if f.WebBase == "" {
			f.WebBase = github.WebBase(f.API)
		}
		f.WebBase = strings.TrimRight(f.WebBase, "/")
		s.apps = &f
	}
}

// Option changes a Site.
type Option func(*Site)

const (
	appStateLife = time.Hour
	appStateMax  = 64
)

type appStates struct {
	mu  sync.Mutex
	now func() time.Time
	by  map[string]appState
}

type appState struct {
	user int64
	name string
	org  string
	at   time.Time
}

func newAppStates(now func() time.Time) *appStates {
	return &appStates{now: now, by: map[string]appState{}}
}

func (a *appStates) give(user int64, name, org string) (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	state := hex.EncodeToString(b)
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	for k, v := range a.by {
		if now.Sub(v.at) > appStateLife {
			delete(a.by, k)
		}
	}
	if len(a.by) >= appStateMax {
		return "", fmt.Errorf("%w: too many Apps are being made at once; wait and try again", api.ErrConflict)
	}
	a.by[state] = appState{user: user, name: name, org: org, at: now}
	return state, nil
}

func (a *appStates) take(state string, user int64) (appState, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, ok := a.by[state]
	if !ok || st.user != user || a.now().Sub(st.at) > appStateLife {
		return appState{}, false
	}
	delete(a.by, state)
	return st, true
}

type appRecord struct {
	Name    string    `json:"name"`
	ID      int64     `json:"id"`
	Slug    string    `json:"slug"`
	HTMLURL string    `json:"html_url"`
	Org     string    `json:"org,omitempty"`
	Made    time.Time `json:"made"`
}

var orgRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)

func (s *Site) appRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/apps", s.withUser(s.appsList))
	mux.HandleFunc("POST /admin/apps", s.withUser(s.startApp))
	mux.HandleFunc("GET /admin/apps/created", s.withUser(s.appCreated))
}

type appView struct {
	Name                              string
	ID                                int64
	MadeHere                          bool
	Jobs                              []string
	KeyFile, Secret, Line, InstallURL string
	Warnings                          []string
}

type appsData struct {
	On           bool
	BaseURL      string
	Apps         []appView
	Notice       string
	WebhookRoute string
}

func (f *AppFlow) keyFile(name string) string {
	return filepath.Join(f.DataDir, "apps", name+".pem")
}

func (f *AppFlow) record(name string) string {
	return filepath.Join(f.DataDir, "apps", name+".json")
}

func (f *AppFlow) records() (map[string]appRecord, error) {
	dir := filepath.Join(f.DataDir, "apps")
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	out := map[string]appRecord{}
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return out, nil
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	for _, file := range files {
		raw, err := root.ReadFile(filepath.Base(file))
		if err != nil {
			return nil, err
		}
		var rec appRecord
		if json.Unmarshal(raw, &rec) != nil || config.CheckAppName(rec.Name) != nil {
			continue
		}
		out[rec.Name] = rec
	}
	return out, nil
}

func appViews(made map[string]appRecord, keyFile func(string) string, uses []api.AppUse) []appView {
	rows := map[string]*appView{}
	for name, rec := range made {
		rows[name] = &appView{Name: name, ID: rec.ID, MadeHere: true, KeyFile: keyFile(name), Secret: webhook.AppSecretName(name),
			InstallURL: strings.TrimRight(rec.HTMLURL, "/") + "/installations/new"}
	}
	for _, use := range uses {
		v := rows[use.Name]
		if v == nil {
			v = &appView{Name: use.Name, ID: use.ID, KeyFile: use.KeyFile, Secret: use.Secret}
			rows[use.Name] = v
		}
		v.Jobs = use.Jobs
		v.Warnings = useWarnings(v, use)
	}
	out := make([]appView, 0, len(rows))
	for _, v := range rows {
		v.Line = fmt.Sprintf("owner: {login: <the account>, app: {id: %d, key_file: %s}}", v.ID, v.KeyFile)
		out = append(out, *v)
	}
	slices.SortFunc(out, func(a, b appView) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func useWarnings(v *appView, use api.AppUse) []string {
	var out []string
	if v.MadeHere && use.ID != v.ID {
		out = append(out, fmt.Sprintf("the job files give it the ID %d, and GitHub gave the App made here %d", use.ID, v.ID))
	}
	if v.MadeHere && use.KeyFile != v.KeyFile {
		out = append(out, "the job files name the key file "+use.KeyFile+", not the one made here, "+v.KeyFile)
	}
	if use.KeyProblem != "" {
		out = append(out, use.KeyProblem)
	}
	if !use.SecretSet {
		out = append(out, use.Secret+" is not set: kipp refuses the App's webhook deliveries until it is")
	}
	return out
}

func (s *Site) appsList(w http.ResponseWriter, r *http.Request, u *store.User) {
	s.appsPage(w, r, u, http.StatusOK, fillOf(r, makeAppRoute))
}

const makeAppRoute = "POST /admin/apps"

func (s *Site) appsPage(w http.ResponseWriter, r *http.Request, u *store.User, code int, fill filled) {
	if !u.Admin {
		s.fail(w, r, api.ErrForbidden)
		return
	}
	d := appsData{}
	if code == http.StatusOK {
		d.Notice = noticeOf(r)
	}
	uses, err := s.api.Service().OwnerApps(r.Context(), u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	made, keyFile := map[string]appRecord{}, func(string) string { return "" }
	if s.apps != nil {
		d.On, d.BaseURL = true, s.apps.BaseURL
		if made, err = s.apps.records(); err != nil {
			s.fail(w, r, err)
			return
		}
		keyFile = s.apps.keyFile
	}
	d.Apps = appViews(made, keyFile, uses)
	s.render(w, r, "apps", code, page{Title: "GitHub Apps", Here: "/admin/apps", Data: d, Fill: fill})
}

func (f *AppFlow) manifest(name string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"name":            name,
		"url":             f.BaseURL,
		"hook_attributes": map[string]any{"url": webhook.AppURL(f.BaseURL, name), "active": true},
		"redirect_url":    f.BaseURL + "/admin/apps/created",
		"public":          false,
		"default_permissions": map[string]string{
			"metadata": "read", "contents": "read", "pull_requests": "read", "statuses": "write",
		},
		"default_events": []string{"push", "pull_request"},
	})
}

type appGoData struct {
	Action, Manifest, Name string
}

func appCSP(webBase string) string {
	origin := webBase
	if u, err := url.Parse(webBase); err == nil && u.Host != "" {
		origin = u.Scheme + "://" + u.Host
	}
	return strings.Replace(csp, "form-action 'self'", "form-action 'self' "+origin, 1)
}

func (s *Site) startApp(w http.ResponseWriter, r *http.Request, u *store.User) {
	if err := s.api.Service().AdministersApps(u, api.SessionFrom(r.Context())); err != nil {
		s.fail(w, r, err)
		return
	}
	name := strings.ToLower(strings.TrimSpace(r.PostFormValue("name")))
	org := strings.TrimSpace(r.PostFormValue("org"))
	refuse := func(msg string) {
		s.appsPage(w, r, u, http.StatusBadRequest, filledAs(makeAppRoute, url.Values{"name": {name}, "org": {org}}, "").saying(msg))
	}
	switch {
	case s.apps == nil:
		refuse("This kipp was not started with a data directory for Apps.")
		return
	case s.apps.BaseURL == "":
		refuse("GitHub must reach this server to send the App's webhook and to lead you back: start kipp with --base-url set to its public address.")
		return
	}
	if err := config.CheckAppName(name); err != nil {
		refuse("Name " + err.Error())
		return
	}
	if org != "" && !orgRe.MatchString(org) {
		refuse("The organization is a GitHub login: letters, digits and -.")
		return
	}
	if _, err := os.Lstat(s.apps.keyFile(name)); err == nil {
		refuse("There is a key for an App called " + name + " already; kipp does not replace a key. Pick another name.")
		return
	}
	manifest, err := s.apps.manifest(name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	state, err := s.appStates.give(u.ID, name, org)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	target := s.apps.WebBase + "/settings/apps/new"
	if org != "" {
		target = s.apps.WebBase + "/organizations/" + url.PathEscape(org) + "/settings/apps/new"
	}
	w.Header().Set("Content-Security-Policy", appCSP(s.apps.WebBase))
	s.render(w, r, "appgo", http.StatusOK, page{Title: "Make a GitHub App", Here: "/admin/apps",
		Data: appGoData{Action: target + "?state=" + url.QueryEscape(state), Manifest: string(manifest), Name: name}})
}

func (s *Site) appCreated(w http.ResponseWriter, r *http.Request, u *store.User) {
	svc := s.api.Service()
	sess := api.SessionFrom(r.Context())
	if err := svc.AdministersApps(u, sess); err != nil {
		s.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	code := q.Get("code")
	st, ok := s.appStates.take(q.Get("state"), u.ID)
	if s.apps == nil || !ok || code == "" {
		s.fail(w, r, fmt.Errorf("%w: this page is not one GitHub led you to from a form here, or it was used already. Start again from the Apps page", api.ErrBadInput))
		return
	}
	if _, err := os.Lstat(s.apps.keyFile(st.name)); err == nil {
		s.fail(w, r, fmt.Errorf("%w: there is a key for an App called %s already", api.ErrConflict, st.name))
		return
	}
	app, err := github.ConvertManifest(r.Context(), s.apps.API, code)
	if err != nil {
		s.fail(w, r, fmt.Errorf("%w: GitHub did not hand the App over (%w). Start again from the Apps page", api.ErrBadInput, err))
		return
	}
	lost := func(why error) {
		slog.Error("web: an App was made at GitHub and not kept", "app", st.name, "id", app.ID, "err", why)
		s.fail(w, r, fmt.Errorf("%w: GitHub made the App %s (%s) but kipp could not keep it: %w. Delete that App at GitHub and start again", api.ErrConflict, st.name, app.HTMLURL, why))
	}
	if err := svc.SetSecret(r.Context(), u, sess, webhook.AppSecretName(st.name), app.WebhookSecret); err != nil {
		lost(err)
		return
	}
	if err := s.apps.writeKey(st.name, app.PEM); err != nil {
		lost(err)
		return
	}
	rec, _ := json.Marshal(appRecord{Name: st.name, ID: app.ID, Slug: app.Slug, HTMLURL: app.HTMLURL, Org: st.org, Made: s.now().UTC()})
	if err := os.WriteFile(s.apps.record(st.name), rec, 0o600); err != nil {
		lost(err)
		return
	}
	svc.RecordApp(r.Context(), u, st.name, fmt.Sprintf("id=%d slug=%s", app.ID, app.Slug))
	done(w, r, "/admin/apps", "app-created")
}

func (f *AppFlow) writeKey(name, pem string) (err error) {
	dir := filepath.Join(f.DataDir, "apps")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(f.keyFile(name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(f.keyFile(name))
		}
	}()
	if _, err = file.WriteString(pem); err != nil {
		_ = file.Close()
		return errors.New("the key was not written")
	}
	return file.Close()
}
