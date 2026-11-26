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
	"github.com/ndthuan/kipp/internal/oauth"
	"github.com/ndthuan/kipp/internal/storage"
	"github.com/ndthuan/kipp/internal/store"
	"github.com/ndthuan/kipp/internal/webhook"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

const maxForm = 64 << 10

const maxSecretForm = config.MaxSecretFile + 3*config.MaxSecret + maxForm

const csp = "default-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// Site is the handler.
type Site struct {
	api       *api.API
	auth      *auth.Service
	st        *store.Store
	version   string
	pages     map[string]*template.Template
	assets    map[string]asset
	forms     *oneTimeForms
	signIn    func(ctx context.Context, name, pw string, at auth.Attempt) (*store.Session, string, error)
	apps      *AppFlow
	appStates *appStates
	oauth     *oauth.Server
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

func parsePages(assets map[string]asset, zone *time.Location) map[string]*template.Template {
	static := func(name string) (string, error) {
		a, ok := assets[name]
		if !ok {
			return "", fmt.Errorf("no static file %q", name)
		}
		return "/static/" + name + "?v=" + a.version(), nil
	}
	funcs := template.FuncMap{"static": static, "dag": dagOf, "ago": ago, "clock": clockIn(zone), "when": whenIn(zone, time.Now), "zoned": zoned, "elapsed": elapsedIn(zone, time.Now), "since": sinceIn(zone, time.Now), "past": pastIn(zone, time.Now), "join": join, "localStages": localStages, "placeLines": placeLines, "loadText": loadText, "nowOf": nowOf, "endedAt": endedAt, "placeWait": placeWait, "cancelOf": cancelOf, "exitText": exitText, "took": took, "millis": millis, "lines": lines, "buildPath": buildPath, "whyRow": whyRow, "commitShort": commitShort, "crumb": crumb,
		"userPath": userPath, "secretPath": secretPath, "lasts": lasts, "expired": expired, "tokenList": tokenList, "appList": appList, "appScopes": appScopes, "tokenForm": tokenForm, "secretName": webhook.SecretName, "placePath": placePath, "bytes": byteSize, "scopePath": scopePath, "scopeField": scopeField, "metricsPath": metricsPath, "plural": plural, "percent": percent, "nameRule": nameRuleOf,
		"removePath": removePath, "partLabel": partLabel, "compactTook": compactTook, "rows": rowsText, "lineNo": lineNo}
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

func (a asset) version() string { return strings.Trim(a.etag, `"`) }

func (s *Site) handler() http.Handler {
	pages := http.NewServeMux()
	pages.HandleFunc("POST /logout", s.withUser(s.logout))
	pages.HandleFunc("GET /{$}", s.withUser(s.home))
	pages.HandleFunc("GET /overview/tabs/{name}", s.withUser(s.overviewTabPanel))
	pages.HandleFunc("GET /overview/charts", s.withUser(s.overviewCharts))
	pages.HandleFunc("GET /jobs", s.withUser(s.jobs))
	pages.HandleFunc("GET /jobs/{name}", s.withUser(s.job))
	pages.HandleFunc("POST /jobs/{name}/build", s.withUser(s.build))
	pages.HandleFunc("POST /jobs/{name}/scan", s.withUser(s.scan))
	pages.HandleFunc("GET /jobs/{name}/{number}", s.withUser(s.buildPage))
	pages.HandleFunc("GET /jobs/{name}/{number}/cancel", s.withUser(s.confirmCancel))
	pages.HandleFunc("POST /jobs/{name}/{number}/cancel", s.withUser(s.cancel))
	pages.HandleFunc("POST /jobs/{name}/{number}/again", s.withUser(s.again))
	pages.HandleFunc("POST /jobs/{name}/{number}/keep", s.withUser(s.keep(false)))
	pages.HandleFunc("POST /jobs/{name}/{number}/unkeep", s.withUser(s.keep(true)))
	pages.HandleFunc("GET /builds/{id}", s.withUser(s.buildByID))
	pages.HandleFunc("GET /queue", s.withUser(s.queue))
	pages.HandleFunc("GET /metrics", s.withUser(s.metricsIndex))
	pages.HandleFunc("GET /metrics/{store}", s.withUser(s.metricsStore))
	s.accountRoutes(pages)
	s.jobEditRoutes(pages)
	s.adminRoutes(pages)
	if s.oauth != nil {
		s.oauthRoutes(pages)
	}
	pages.HandleFunc("/", s.withUser(func(w http.ResponseWriter, r *http.Request, _ *store.User) { s.fail(w, r, api.ErrNotFound) }))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /static/{file...}", s.asset)
	mux.HandleFunc("GET /favicon.ico", s.icon)
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.Handle("/", csrfFromForm(s.api.AuthenticateWith(s.fail, pages)))
	return secure(http.NewCrossOriginProtection().Handler(limitForms(mux)))
}

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
		h.Set("X-Robots-Tag", "noindex, nofollow")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

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

func csrfFromForm(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.Header.Get(api.CSRFHeader) == "" {
			if v := r.PostFormValue(config.CSRFField); v != "" {
				r.Header.Set(api.CSRFHeader, v)
			}
		}
		next.ServeHTTP(w, r)
	})
}

type pageHandler func(w http.ResponseWriter, r *http.Request, u *store.User)

func (s *Site) withUser(h pageHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := api.UserFrom(r.Context())
		switch {
		case u != nil && api.SessionFrom(r.Context()) != nil:
			h(w, r, u)
		case r.Header.Get("HX-Request") == "true":
			w.Header().Set("HX-Redirect", loginFor(r))
			w.WriteHeader(http.StatusUnauthorized)
		default:
			redirect(w, r, loginFor(r), http.StatusSeeOther)
		}
	}
}

func loginFor(r *http.Request) string {
	if r.Method != http.MethodGet {
		return "/login"
	}
	return "/login?next=" + url.QueryEscape(localPath(r.URL.RequestURI()))
}

type page struct {
	Title     string
	User      *store.User
	CSRF      string
	HXHeaders string
	Version   string
	Theme     string
	Banner    string
	Quiet     bool
	SelfCheck *storage.Check
	Upgrade   *upgradeView
	Error     string
	Data      any
	Fill      filled
	Here, Job string
	Nav       *navData
	Queue     *api.QueueView
	groups    []*group
	oneTime   bool
	Once      string
}

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
	s.notices(&p)
	if p.Upgrade == nil {
		p.Upgrade = s.upgradeViewOf(r, p.User)
	}
	p.Nav = s.nav(r, p)
	if p.User != nil && p.Queue == nil {
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

func (s *Site) fragment(w http.ResponseWriter, page, name string, data any) {
	var buf bytes.Buffer
	if err := s.pages[page].ExecuteTemplate(&buf, name, data); err != nil {
		slog.Error("web: render", "fragment", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

func (s *Site) notices(p *page) {
	if msg := s.api.Service().ReloadError(p.User); msg != "" {
		p.Banner = msg
	}
	p.Quiet = p.User != nil && s.api.Service().Quieting()
	if p.User != nil && p.User.Admin {
		p.SelfCheck = s.api.Service().SelfCheckFailed()
	}
}

const themeCookie = "kipp_theme"

func themeOf(r *http.Request) string {
	if c, err := r.Cookie(themeCookie); err == nil && (c.Value == "light" || c.Value == "dark") {
		return c.Value
	}
	return ""
}

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

func (s *Site) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, api.ErrSudoRequired) {
		s.askSudo(w, r)
		return
	}
	code, msg := refusal(w, err)
	if code == http.StatusInternalServerError && r.Context().Err() == nil {
		slog.Error("web", "method", r.Method, "path", r.URL.Path, "err", err)
	}
	s.errorPage(w, r, code, http.StatusText(code), msg)
}

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
	if code, msg := jobRefusal(err); code != 0 {
		return code, msg
	}
	return http.StatusInternalServerError, "Something went wrong on the server. Its log has the details."
}

const busyWait = 2 * time.Second

func retryAfter(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
}

func plainly(err error) string {
	msg := err.Error()
	for _, kind := range []error{api.ErrBadInput, api.ErrConflict} {
		msg = strings.TrimPrefix(msg, kind.Error()+": ")
	}
	return msg
}

type loginData struct {
	Next string
	Name string
}

func localPath(next string) string {
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.Contains(next, `\`) {
		return "/"
	}
	return next
}

func nextOr(next, fallback string) string {
	if p := localPath(next); p != "/" || next == "/" {
		return p
	}
	return fallback
}

func redirect(w http.ResponseWriter, r *http.Request, to string, status int) {
	u, err := url.Parse(localPath(to))
	if err != nil {
		u = &url.URL{Path: "/"}
	}
	var at strings.Builder
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
	s.sudoForConsent(r, next, sess)
	http.SetCookie(w, api.SessionCookieFor(sess))
	http.SetCookie(w, api.DeviceCookieFor(device))
	redirect(w, r, next, http.StatusSeeOther)
}

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
	http.SetCookie(w, api.ExpiredSessionCookie())
	redirect(w, r, "/login", http.StatusSeeOther)
}
