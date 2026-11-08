// Package web is the browser face of kipp: Go templates, htmx and a few
// stylesheets, all embedded, every action going through api.Service.
package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
	"github.com/ndthuan/kipp/internal/webhook"
)

//go:embed templates/*.html
var templateFS embed.FS

// staticFS is what the pages load. Not all of it was written here:
// htmx.min.js is htmx's own build. THIRD_PARTY.md says where it comes from
// and under which licence.
//
//go:embed static
var staticFS embed.FS

// maxForm is the most a form may send, but for the secrets form.
const maxForm = 64 << 10

// maxSecretForm is the most the secrets form may send: a file of
// config.MaxSecretFile bytes, or a value of config.MaxSecret bytes each of
// which a browser may percent-encode as three, and room for the other
// fields. A file a little over its bound is read and refused by name.
const maxSecretForm = config.MaxSecretFile + 3*config.MaxSecret + maxForm

// csp lets a page load only what this server serves: no inline script or
// style, nothing from another origin, no frame around it.
const csp = "default-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// Site is the handler.
type Site struct {
	api     *api.API
	auth    *auth.Service
	st      *store.Store
	version string
	pages   map[string]*template.Template // by file name without .html
	assets  map[string]asset              // by path under static/
	// forms are the one-time values of the forms that show a value once.
	forms *oneTimeForms
	// signIn is auth's Login; a test puts its own in its place.
	signIn func(ctx context.Context, name, pw string, at auth.Attempt) (*store.Session, string, error)
	// apps makes GitHub Apps from a manifest; nil when the server is not
	// set up for it.
	apps      *AppFlow
	appStates *appStates
	now       func() time.Time
}

type asset struct {
	data []byte
	etag string
}

// New builds the site over the API.
func New(a *api.API, au *auth.Service, st *store.Store, version string, opts ...Option) http.Handler {
	return newSite(a, au, st, version, opts...).handler()
}

func newSite(a *api.API, au *auth.Service, st *store.Store, version string, opts ...Option) *Site {
	assets := loadAssets()
	s := &Site{api: a, auth: au, st: st, version: version, pages: parsePages(assets, a.Zone()), assets: assets, forms: newOneTimeForms(time.Now), signIn: au.Login,
		appStates: newAppStates(time.Now), now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s
}

// parsePages gives every page its own copy of the layout, so that each
// can define "content" without meeting another's. Their times are shown in
// zone, the server's.
func parsePages(assets map[string]asset, zone *time.Location) map[string]*template.Template {
	// static is a file's address with its content in it, so a new kipp's
	// pages ask for new addresses and a cache in between, which may keep
	// a file longer than it was told to, has nothing old to answer with.
	static := func(name string) (string, error) {
		a, ok := assets[name]
		if !ok {
			return "", fmt.Errorf("no static file %q", name)
		}
		return "/static/" + name + "?v=" + a.version(), nil
	}
	funcs := template.FuncMap{"static": static, "dag": dagOf, "ago": ago, "clock": clockIn(zone), "zoned": zoned, "since": since, "join": join, "cancelOf": cancelOf, "took": took, "millis": millis, "lines": lines, "buildPath": buildPath, "crumb": crumb,
		"userPath": userPath, "secretPath": secretPath, "lasts": lasts, "expired": expired, "tokenList": tokenList, "tokenForm": tokenForm, "secretName": webhook.SecretName, "placePath": placePath, "bytes": byteSize, "scopePath": scopePath, "scopeField": scopeField, "metricsPath": metricsPath}
	layout := template.Must(template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html"))
	files, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		panic(err)
	}
	pages := map[string]*template.Template{}
	for _, file := range files {
		if name := strings.TrimSuffix(path.Base(file), ".html"); name != "layout" {
			pages[name] = template.Must(template.Must(layout.Clone()).ParseFS(templateFS, file))
		}
	}
	return pages
}

func loadAssets() map[string]asset {
	assets := map[string]asset{}
	err := fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := staticFS.ReadFile(p)
		assets[strings.TrimPrefix(p, "static/")] = newAsset(data)
		return err
	})
	if err != nil {
		panic(err)
	}
	return assets
}

func newAsset(data []byte) asset {
	sum := sha256.Sum256(data)
	return asset{data: data, etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
}

// version is the part of the tag that names the content.
func (a asset) version() string { return strings.Trim(a.etag, `"`) }

func (s *Site) handler() http.Handler {
	pages := http.NewServeMux()
	pages.HandleFunc("POST /logout", s.withUser(s.logout))
	pages.HandleFunc("GET /{$}", s.withUser(s.jobs))
	pages.HandleFunc("GET /jobs/{name}", s.withUser(s.job))
	pages.HandleFunc("POST /jobs/{name}/build", s.withUser(s.build))
	pages.HandleFunc("POST /jobs/{name}/scan", s.withUser(s.scan))
	pages.HandleFunc("GET /jobs/{name}/{number}", s.withUser(s.buildPage))
	pages.HandleFunc("POST /jobs/{name}/{number}/cancel", s.withUser(s.cancel))
	pages.HandleFunc("POST /jobs/{name}/{number}/again", s.withUser(s.again))
	pages.HandleFunc("POST /jobs/{name}/{number}/keep", s.withUser(s.keep(false)))
	pages.HandleFunc("POST /jobs/{name}/{number}/unkeep", s.withUser(s.keep(true)))
	pages.HandleFunc("GET /builds/{id}", s.withUser(s.buildByID))
	pages.HandleFunc("GET /queue", s.withUser(s.queue))
	pages.HandleFunc("GET /metrics", s.withUser(s.metricsIndex))
	pages.HandleFunc("GET /metrics/{store}", s.withUser(s.metricsStore))
	s.accountRoutes(pages)
	s.adminRoutes(pages)
	pages.HandleFunc("/", s.withUser(func(w http.ResponseWriter, r *http.Request, _ *store.User) { s.fail(w, r, api.ErrNotFound) }))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /static/{file...}", s.asset)
	// A browser asks for this one on its own, whatever the page names.
	mux.HandleFunc("GET /favicon.ico", s.icon)
	// Login stands outside the session check, so a form left open in one
	// tab still works after a login in another.
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	// The form's token is copied into the header before the API's
	// middleware looks for it there; what that refuses is a page here.
	mux.Handle("/", csrfFromForm(s.api.AuthenticateWith(s.fail, pages)))
	// The cross-origin check is what stands between another site and the
	// login form, which has no session to hold a CSRF token.
	return secure(http.NewCrossOriginProtection().Handler(limitForms(mux)))
}

// limitForms caps what a request may send at maxForm, or maxSecretForm for
// the one form that carries a secret's value.
func limitForms(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := int64(maxForm)
		if r.Method == http.MethodPost && r.URL.Path == "/admin/secrets" {
			limit = maxSecretForm
		}
		r2 := *r
		r2.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, &r2)
	})
}

func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		// The login page answers 200 to anyone, and a crawler that finds a
		// kipp behind a public name should keep none of it.
		h.Set("X-Robots-Tag", "noindex, nofollow")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// asset serves an embedded file. Its tag is its content, so a browser asks
// each time and downloads only what a new kipp changed.
func (s *Site) asset(w http.ResponseWriter, r *http.Request) { s.serveAsset(w, r, r.PathValue("file")) }

func (s *Site) icon(w http.ResponseWriter, r *http.Request) { s.serveAsset(w, r, "favicon.svg") }

func (s *Site) serveAsset(w http.ResponseWriter, r *http.Request, name string) {
	a, ok := s.assets[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("ETag", a.etag)
	w.Header().Set("Cache-Control", "no-cache")
	if r.URL.Query().Get("v") == a.version() {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(a.data))
}

// csrfFromForm lets a plain form carry the CSRF token as a field; it is
// copied into the header the API's middleware checks.
func csrfFromForm(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// PostFormValue reads a multipart form too, which is how a file comes.
		if r.Method == http.MethodPost && r.Header.Get(api.CSRFHeader) == "" {
			if v := r.PostFormValue(config.CSRFField); v != "" {
				r.Header.Set(api.CSRFHeader, v)
			}
		}
		next.ServeHTTP(w, r)
	})
}

type pageHandler func(w http.ResponseWriter, r *http.Request, u *store.User)

// withUser lets a browser session through and sends anything else to the
// login form. An API token is not a session.
func (s *Site) withUser(h pageHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := api.UserFrom(r.Context())
		switch {
		case u != nil && api.SessionFrom(r.Context()) != nil:
			h(w, r, u)
		case r.Header.Get("HX-Request") == "true":
			// htmx would follow a redirect and swap the login form into the page.
			w.Header().Set("HX-Redirect", loginFor(r))
			w.WriteHeader(http.StatusUnauthorized)
		default:
			redirect(w, r, loginFor(r), http.StatusSeeOther)
		}
	}
}

// loginFor is the login form that leads back to the page r asked for. A
// request that is not a GET asked for no page: its path would be a 404 or
// a 405 after the login, so that one leads to the front page.
func loginFor(r *http.Request) string {
	if r.Method != http.MethodGet {
		return "/login"
	}
	return "/login?next=" + url.QueryEscape(localPath(r.URL.RequestURI()))
}

// page is what the layout needs, with one page's own Data.
type page struct {
	Title     string
	User      *store.User
	CSRF      string
	HXHeaders string // JSON for htmx's hx-headers
	Version   string
	Theme     string // "light" or "dark" when the reader picked one, else the system's
	Banner    string // shown on every page when not empty
	Quiet     bool   // the server is quiet before a restart: every page says so
	// Upgrade is the upgrade as an admin's pages show it, nil for anyone
	// else; render reads it unless the page has it already.
	Upgrade *upgradeView
	Error   string
	Data    any
	// Here is the href of the group the page is in, "/" for every job;
	// Job is the job it is about. The header marks both.
	Here, Job string
	Nav       *navData
	// Queue is what the header's queue link counts: what the page has
	// already, or what render reads.
	Queue *api.QueueView
	// groups are the jobs when the page has them already.
	groups []*group
	// oneTime: the page has a form whose answer shows a value once, and
	// render gives it Once, the form's one-time value.
	oneTime bool
	Once    string
}

// render executes the whole page before it writes any of it, so that a
// template that fails is a 500 and never half a page.
func (s *Site) render(w http.ResponseWriter, r *http.Request, name string, code int, p page) {
	if sess := api.SessionFrom(r.Context()); sess != nil {
		p.CSRF = sess.CSRF
		if p.oneTime {
			once, err := s.forms.give(sess.ID)
			if err != nil {
				s.fail(w, r, err)
				return
			}
			p.Once = once
		}
		headers, _ := json.Marshal(map[string]string{api.CSRFHeader: sess.CSRF})
		p.HXHeaders = string(headers)
	}
	p.User = api.UserFrom(r.Context())
	p.Version = s.version
	p.Theme = themeOf(r)
	if msg := s.api.Service().ReloadError(p.User); msg != "" {
		p.Banner = msg
	}
	p.Quiet = p.User != nil && s.api.Service().Quieting()
	if p.Upgrade == nil {
		p.Upgrade = s.upgradeViewOf(r, p.User)
	}
	p.Nav = s.nav(r, p)
	if p.User != nil && p.Queue == nil {
		// A queue that cannot be read costs the header its link, not the page.
		q, err := s.api.Service().Queue(r.Context(), p.User)
		if err != nil && r.Context().Err() == nil {
			slog.Error("web: the header's queue", "err", err)
		}
		p.Queue = q
	}
	var buf bytes.Buffer
	if err := s.pages[name].ExecuteTemplate(&buf, "layout.html", p); err != nil {
		slog.Error("web: render", "page", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	_, _ = buf.WriteTo(w)
}

// themeCookie holds the theme a reader picked in the footer. It is the
// browser's to keep and only a look, so it carries nothing to guard: theme.js
// writes it and the page is drawn in it, so the first paint is already right.
const themeCookie = "kipp_theme"

// themeOf is the theme r's cookie picks, or "" for the system's.
func themeOf(r *http.Request) string {
	if c, err := r.Cookie(themeCookie); err == nil && (c.Value == "light" || c.Value == "dark") {
		return c.Value
	}
	return ""
}

// nav is the header's menus for a page a user asked for. A refresh by
// htmx swaps in a part of the page below the header, so it gets none and
// is spared listing every job; a list that fails costs the menus, not
// the page.
func (s *Site) nav(r *http.Request, p page) *navData {
	if p.User == nil || r.Header.Get("HX-Request") == "true" {
		return nil
	}
	all := p.groups
	if all == nil {
		jobs, err := s.api.Service().Jobs(r.Context(), p.User)
		if err != nil {
			if r.Context().Err() == nil {
				slog.Error("web: the header's jobs", "err", err)
			}
			return nil
		}
		all = groups(jobs)
	}
	return navOf(all, p.Here, p.Job)
}

// fail answers with the error's page. Only what the user got wrong is
// shown; what went wrong inside the server goes to the log. A change that
// needs the password again is answered with the form that asks for it.
func (s *Site) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, api.ErrSudoRequired) {
		s.askSudo(w, r)
		return
	}
	code, msg := refusal(w, err)
	if code == http.StatusInternalServerError && r.Context().Err() == nil { // a client that left is not the server's failure
		slog.Error("web", "method", r.Method, "path", r.URL.Path, "err", err)
	}
	s.render(w, r, "error", code, page{Title: http.StatusText(code), Error: msg})
}

// refusal is the status and the words err is answered with. A password
// that was not looked at, throttled or busy, also says when to try again.
func refusal(w http.ResponseWriter, err error) (int, string) {
	var throttled *auth.ThrottledError
	switch {
	case errors.Is(err, api.ErrCSRF):
		return http.StatusForbidden, "This form is out of date. Go back, load the page again and send it once more."
	case errors.Is(err, api.ErrNotFound):
		return http.StatusNotFound, "There is nothing here, or nothing you may see."
	case errors.Is(err, api.ErrForbidden):
		return http.StatusForbidden, "You are not allowed to do that."
	case errors.Is(err, api.ErrWrongPassword):
		return http.StatusUnauthorized, "Wrong password."
	case errors.Is(err, api.ErrConflict):
		return http.StatusConflict, plainly(err)
	case errors.Is(err, api.ErrQuiet):
		retryAfter(w, time.Minute)
		return http.StatusServiceUnavailable, "kipp is quiet: it is about to restart and starts no build. Try again in a minute."
	case errors.Is(err, api.ErrBadInput):
		return http.StatusBadRequest, plainly(err)
	case errors.As(err, &throttled):
		retryAfter(w, throttled.RetryAfter)
		return http.StatusTooManyRequests, "Too many wrong passwords for this name. Try again in " + inWords(throttled.RetryAfter) + "."
	case errors.Is(err, auth.ErrBusy):
		retryAfter(w, busyWait)
		return http.StatusServiceUnavailable, "Too many passwords are being checked at once. Wait a moment and try again."
	}
	return http.StatusInternalServerError, "Something went wrong on the server. Its log has the details."
}

// busyWait is how long a password check that found no place is told to
// wait: the time it waited for one.
const busyWait = 2 * time.Second

func retryAfter(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
}

// plainly is a refusal's reason as a person reads it: without the name of
// its kind in front, which is for the code.
func plainly(err error) string {
	msg := err.Error()
	for _, kind := range []error{api.ErrBadInput, api.ErrConflict} {
		msg = strings.TrimPrefix(msg, kind.Error()+": ")
	}
	return msg
}

type loginData struct {
	Next string
	// Name is the name a refused login was sent with, kept in the form.
	Name string
}

// localPath is next when it is a path on this site, and the front page
// otherwise: a login must never send its user to another site.
func localPath(next string) string {
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.Contains(next, `\`) {
		return "/"
	}
	return next
}

// redirect sends the browser to to, a path of this site, and is the one
// way the site redirects. It takes to apart and puts it together again
// from its segments and its query, each escaped: whatever reaches it, the
// address it sends names a page of this site and no other. A to that is
// not a path of this site leads to the front page.
func redirect(w http.ResponseWriter, r *http.Request, to string, status int) {
	u, err := url.Parse(localPath(to))
	if err != nil { // localPath parsed it already
		u = &url.URL{Path: "/"}
	}
	var at strings.Builder
	// The escaped path keeps an escaped slash inside its segment.
	for _, seg := range strings.Split(u.EscapedPath(), "/")[1:] {
		plain, err := url.PathUnescape(seg)
		if err != nil {
			plain = seg
		}
		at.WriteString("/" + url.PathEscape(plain))
	}
	if at.Len() == 0 {
		at.WriteString("/")
	}
	query := u.Query()
	sep := "?"
	for _, k := range slices.Sorted(maps.Keys(query)) {
		for _, v := range query[k] {
			at.WriteString(sep + url.QueryEscape(k) + "=" + url.QueryEscape(v))
			sep = "&"
		}
	}
	http.Redirect(w, r, at.String(), status)
}

func (s *Site) loginPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "login", http.StatusOK, page{Title: "Log in", Data: loginData{Next: localPath(r.URL.Query().Get("next"))}})
}

func (s *Site) login(w http.ResponseWriter, r *http.Request) {
	next := localPath(r.PostFormValue("next"))
	sess, device, err := s.signIn(r.Context(), r.PostFormValue("name"), r.PostFormValue("password"), s.api.Attempt(r))
	if err != nil {
		s.loginRefused(w, r, next, err)
		return
	}
	s.endHeldSession(r)
	for _, c := range s.api.LoginCookies(r, sess) {
		http.SetCookie(w, c)
	}
	http.SetCookie(w, s.api.DeviceCookieFor(r, device))
	redirect(w, r, next, http.StatusSeeOther)
}

// loginRefused answers a login that did not succeed. A password that was
// not looked at, throttled or busy, brings the form back to be sent again
// later, and says when.
func (s *Site) loginRefused(w http.ResponseWriter, r *http.Request, next string, err error) {
	again := func(code int, wait time.Duration, msg string) {
		retryAfter(w, wait)
		s.render(w, r, "login", code, page{Title: "Log in", Error: msg, Data: loginData{Next: next, Name: r.PostFormValue("name")}})
	}
	var throttled *auth.ThrottledError
	switch {
	case errors.Is(err, auth.ErrBadLogin):
		s.render(w, r, "login", http.StatusUnauthorized, page{Title: "Log in", Error: "Wrong name or password.", Data: loginData{Next: next, Name: r.PostFormValue("name")}})
	case errors.As(err, &throttled):
		again(http.StatusTooManyRequests, throttled.RetryAfter, "Too many wrong passwords for this name. Try again in "+inWords(throttled.RetryAfter)+".")
	case errors.Is(err, auth.ErrBusy):
		again(http.StatusServiceUnavailable, busyWait, "Too many logins at once. Wait a moment and try again.")
	default:
		s.fail(w, r, err)
	}
}

// inWords is a wait as a person says it: in seconds up to a minute, in
// minutes after, rounded up so that it is never too early.
func inWords(d time.Duration) string {
	if d <= time.Minute {
		return plural(int(math.Ceil(d.Seconds())), "second")
	}
	return plural(int(math.Ceil(d.Minutes())), "minute")
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}

// endHeldSession deletes the sessions the browser came with, which the
// new cookie replaces: left alone they would stay good until they expired.
// The login has succeeded either way.
func (s *Site) endHeldSession(r *http.Request) {
	for _, id := range s.api.HeldSessions(r) {
		if err := s.st.DeleteSession(r.Context(), id); err != nil {
			slog.Warn("web: the session a login replaced was not deleted", "err", err)
		}
	}
}

func (s *Site) logout(w http.ResponseWriter, r *http.Request, _ *store.User) {
	if err := s.st.DeleteSession(r.Context(), api.SessionFrom(r.Context()).ID); err != nil {
		s.fail(w, r, err)
		return
	}
	for _, c := range s.api.LogoutCookies(r) {
		http.SetCookie(w, c)
	}
	redirect(w, r, "/login", http.StatusSeeOther)
}
