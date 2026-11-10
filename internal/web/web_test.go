package web

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/cachesrv"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/dispatch"
	"github.com/ndthuan/kipp/internal/overview"
	"github.com/ndthuan/kipp/internal/runner"
	"github.com/ndthuan/kipp/internal/store"
	"github.com/ndthuan/kipp/internal/waitfor"
)

// htmxSHA256 is static/htmx.min.js as published in the npm package
// htmx.org@2.0.11 (dist/htmx.min.js).
const htmxSHA256 = "d6fdc75f204e6bdefa99b69bf1e6d4ac69b8a364f77929f45c13476b4000f717"

// gateEnv names the file a build of the job "hooked" waits for before its
// hook ends; a stage inherits the server's environment.
const gateEnv = "KIPP_TEST_HOOK_GATE"

var jobFiles = map[string]string{
	"_vars.yml":  "global: {REGION: eu}\njobs:\n  'ask': {BANNER: '<b>hi</b>'}\n",
	"hooked.yml": "name: hooked\nstages:\n  s: {run: echo stage}\non_success: {run: 'while [ ! -e \"$" + gateEnv + "\" ]; do sleep 0.02; done; echo hook done'}\n",
	"ask.yml":    "name: ask\nparams:\n  REQUEST: {type: text, default: \"\\nline <one>\\nline two\"}\n  WHO: {default: world, description: \"Who to greet, <one> name.\\nEmpty is world.\"}\nstages:\n  s: {run: 'true'}\n",
	"smoke.yml":  "name: smoke\ngroup: backend\nparams:\n  TIER: {type: choice, options: [quick, full]}\nstages:\n  lint: {run: echo lint}\n  unit: {run: echo unit}\n  smoke: {needs: [lint, unit], run: echo smoke}\n",
	"slow.yml":   "name: slow\nconcurrency: drop\nstages:\n  s: {run: 'echo started; sleep 30'}\n",
	"quoted.yml": "name: quoted\ndescription: 'ends \"the title\" data-x=\"1\" & <b>bold</b>'\nstages:\n  s: {run: 'true'}\n",
	"shaky.yml": `name: shaky
stages:
  lint:
    run: |
      printf '%s' '<testsuite name="s"><testcase name="fine"/></testsuite>' > r.xml
      echo warned
      exit 4
    junit: r.xml
    unstable_exit: [4]
  unit: {needs: [lint], run: echo unit}
`,
	"nightly.yml": `name: nightly
description: "<script>alert('job')</script>"
triggers: {cron: '0 3 * * *'}
params:
  DRY: {type: bool, default: "true"}
  NOTE: {default: hello}
on_failure: {run: echo hooked}
stages:
  report:
    run: |
      printf '%s' '<testsuite name="s"><testcase name="fine" time="0.5"/><testcase name="&lt;b&gt;bad&lt;/b&gt;"><failure message="&lt;script&gt;boom&lt;/script&gt;"/></testcase><testcase name="later"><skipped/></testcase></testsuite>' > r.xml
      exit 1
    junit: r.xml
`,
}

type env struct {
	srv  *httptest.Server
	st   *store.Store
	d    *dispatch.Dispatcher
	site *Site
	// reloadErr is why the last reload was refused, as serve would say it.
	reloadErr atomic.Pointer[string]
}

var hashOnce = sync.OnceValue(func() string {
	h, err := auth.HashPassword("pw")
	if err != nil {
		panic(err)
	}
	return h
})

// site is a server with three users, all with the password "pw": admin,
// bob (build on smoke, view on slow) and alice (view on smoke, with a token).
func site(t *testing.T, tune ...func(*api.Deps)) *env {
	t.Helper()
	dir := t.TempDir()
	jobs := filepath.Join(dir, "jobs")
	must(t, os.MkdirAll(jobs, 0o750))
	for name, body := range jobFiles {
		must(t, os.WriteFile(filepath.Join(jobs, name), []byte(body), 0o600))
	}
	snap, err := config.Load(jobs)
	must(t, err)
	st, err := store.Open(filepath.Join(dir, "db"))
	must(t, err)
	ctx, stop := context.WithCancel(context.Background())
	quiet := slog.New(slog.DiscardHandler)
	d := dispatch.New(st, runner.Local{}, dispatch.Options{DataDir: dir, Logger: quiet, PassEnv: append([]string{gateEnv}, dispatch.DefaultPassEnv...)})
	d.SetSnapshot(snap)
	stopped := make(chan struct{})
	go func() { d.Run(ctx); close(stopped) }()
	au := auth.New(st, time.Hour, []byte("an obvious test device key 32 b."))
	e := &env{st: st, d: d}
	e.reloadErr.Store(new(""))
	deps := api.Deps{Auth: au, Dispatch: d, Store: st, Version: "t", Logger: quiet, Listen: "127.0.0.1:0", FlakyFlips: overview.DefaultFlips,
		Reload: func(context.Context) error { return nil }, ReloadError: func() string { return *e.reloadErr.Load() },
		// What is sealed is never the value, so that a page that showed
		// it would be caught by looking for the value.
		Seal:       func(string, []byte) ([]byte, error) { return []byte("the-sealed-bytes"), nil },
		ServerInfo: serverInfoNow,
		Cache:      cachesrv.New(cachesrv.Options{Dir: t.TempDir(), Store: st})}
	for _, f := range tune {
		f(&deps)
	}
	a := api.New(deps)
	e.site = newSite(a, au, st, deps.Version)
	mux := http.NewServeMux()
	mux.Handle("/api/", a.Handler())
	mux.Handle("/", e.site.handler())
	// TLS: a cookie jar sends a Secure cookie to https only.
	e.srv = httptest.NewTLSServer(mux)
	t.Cleanup(func() { stop(); <-stopped; e.srv.Close(); _ = st.Close() })

	_, err = st.CreateUser(ctx, "admin", hashOnce(), true)
	must(t, err)
	bob, err := st.CreateUser(ctx, "bob", hashOnce(), false)
	must(t, err)
	must(t, st.Grant(ctx, bob.ID, "smoke", []string{"view", "build"}))
	must(t, st.Grant(ctx, bob.ID, "slow", []string{"view"}))
	alice, err := st.CreateUser(ctx, "alice", hashOnce(), false)
	must(t, err)
	must(t, st.Grant(ctx, alice.ID, "smoke", []string{"view"}))
	_, err = st.CreateToken(ctx, alice.ID, "laptop-token", "hash-of-the-token")
	must(t, err)
	must(t, st.PutSecret(ctx, "DEPLOY_KEY", []byte("sealed")))
	return e
}

// anon is a browser with no session: it keeps cookies and follows nothing.
func (e *env) anon(t *testing.T) *http.Client {
	t.Helper()
	c := *e.srv.Client() // a copy: the server hands every caller the same client
	jar, err := cookiejar.New(nil)
	must(t, err)
	c.Jar = jar
	c.Timeout = waitfor.Safety
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

func (e *env) login(t *testing.T, name string) *http.Client {
	t.Helper()
	c := e.anon(t)
	res := post(t, c, e.srv.URL+"/login", url.Values{"name": {name}, "password": {"pw"}})
	eq(t, "login as "+name, res.StatusCode, http.StatusSeeOther)
	return c
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func eq[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
}

func contains(t *testing.T, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Fatalf("%q is not in:\n%s", sub, s)
		}
	}
}

func lacks(t *testing.T, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			t.Fatalf("%q is in:\n%s", sub, s)
		}
	}
}

func do(t *testing.T, c *http.Client, req *http.Request) (*http.Response, string) {
	t.Helper()
	res, err := c.Do(req)
	must(t, err)
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	must(t, err)
	return res, string(body)
}

func get(t *testing.T, c *http.Client, u string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, u, nil)
	must(t, err)
	res, body := do(t, c, req)
	return res.StatusCode, body
}

func post(t *testing.T, c *http.Client, u string, form url.Values) *http.Response {
	t.Helper()
	res, _ := postBody(t, c, u, form)
	return res
}

func postBody(t *testing.T, c *http.Client, u string, form url.Values) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, u, strings.NewReader(form.Encode()))
	must(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return do(t, c, req)
}

func csrfOf(t *testing.T, page string) string {
	t.Helper()
	_, rest, ok := strings.Cut(page, `name="`+config.CSRFField+`" value="`)
	if !ok {
		t.Fatalf("no csrf field in:\n%s", page)
	}
	token, _, _ := strings.Cut(rest, `"`)
	return token
}

// token is the session's CSRF token, read off a page as a browser would.
func (e *env) token(t *testing.T, c *http.Client) string {
	t.Helper()
	_, page := get(t, c, e.srv.URL+"/")
	return csrfOf(t, page)
}

// build triggers a build through the form and returns its id.
func (e *env) build(t *testing.T, c *http.Client, job string, params url.Values) int64 {
	t.Helper()
	form := url.Values{"csrf": {e.token(t, c)}}
	maps.Copy(form, params)
	res := post(t, c, e.srv.URL+"/jobs/"+job+"/build", form)
	eq(t, "trigger "+job, res.StatusCode, http.StatusSeeOther)
	at := strings.Split(res.Header.Get("Location"), "/") // /jobs/<job>/<number>
	if len(at) != 4 {
		t.Fatalf("trigger %s led to %q", job, res.Header.Get("Location"))
	}
	n, err := strconv.Atoi(at[3])
	must(t, err)
	b, err := e.st.BuildByNumber(context.Background(), at[2], n)
	must(t, err)
	return b.ID
}

// at is the address of the build's page.
func (e *env) at(t *testing.T, id int64) string {
	t.Helper()
	b, err := e.st.Build(context.Background(), id)
	must(t, err)
	return buildPath(b)
}

func (e *env) wait(t *testing.T, id int64) *store.Build {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitfor.Safety)
	defer cancel()
	b, err := e.d.Wait(ctx, id)
	must(t, err)
	return b
}

// eventually polls for what the dispatcher writes after a build's status
// is final: the tests and the hook's log.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	waitfor.Until(t, what, ok)
}

func TestALoginLeadsBackToThePageAskedForAndThePagesShowTheBuild(t *testing.T) {
	e := site(t)
	c := e.anon(t)
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/jobs/smoke?x=1", nil)
	res, _ := do(t, c, req)
	eq(t, "anonymous", res.StatusCode, http.StatusSeeOther)
	eq(t, "to login", res.Header.Get("Location"), "/login?next=%2Fjobs%2Fsmoke%3Fx%3D1")

	res, page := postBody(t, c, e.srv.URL+"/login", url.Values{"name": {"admin"}, "password": {"wrong"}})
	eq(t, "wrong password", res.StatusCode, http.StatusUnauthorized)
	contains(t, page, "Wrong name or password.")
	res = post(t, c, e.srv.URL+"/login", url.Values{"name": {"admin"}, "password": {"pw"}})
	eq(t, "login", res.StatusCode, http.StatusSeeOther)
	eq(t, "after login", res.Header.Get("Location"), "/")
	cookie := res.Cookies()[0]
	if cookie.Name != api.SessionCookie || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" {
		t.Fatalf("session cookie: %+v", cookie)
	}

	code, page := get(t, c, e.srv.URL+"/")
	eq(t, "jobs", code, http.StatusOK)
	contains(t, page, "backend", `href="/jobs/smoke"`, "0 3 * * *")

	code, page = get(t, c, e.srv.URL+"/jobs/smoke")
	eq(t, "job", code, http.StatusOK)
	contains(t, page, `name="TIER"`, `<option value="full">`)
	csrf := csrfOf(t, page)

	res = post(t, c, e.srv.URL+"/jobs/smoke/build", url.Values{"TIER": {"full"}, "csrf": {csrf}})
	eq(t, "build", res.StatusCode, http.StatusSeeOther)
	eq(t, "build location", res.Header.Get("Location"), "/jobs/smoke/1")
	code, page = get(t, c, e.srv.URL+"/jobs/smoke/1")
	eq(t, "build page", code, http.StatusOK)
	contains(t, page, `<svg class="dag"`, `data-stage="smoke"`, `data-log="/api/v1/builds/1/stages/lint/log"`, "TIER=full", `src="/static/build.js?v=`)

	if b := e.wait(t, 1); b.Status != store.Success {
		t.Fatalf("build: %s", b.Status)
	}
	code, page = get(t, c, e.srv.URL+"/jobs/smoke")
	eq(t, "job with a build", code, http.StatusOK)
	contains(t, page, `href="/jobs/smoke/1"`, `class="status success"`)
	code, page = get(t, c, e.srv.URL+"/jobs")
	eq(t, "jobs with a build", code, http.StatusOK)
	contains(t, page, `href="/jobs/smoke/1"`, `class="status success"`)
}

func TestFormPostNeedsTheCSRFField(t *testing.T) {
	e := site(t)
	c := e.login(t, "admin")
	csrf := e.token(t, c)

	res := post(t, c, e.srv.URL+"/jobs/smoke/build", url.Values{"TIER": {"full"}, "csrf": {csrf}})
	eq(t, "form with the field", res.StatusCode, http.StatusSeeOther)
	res = post(t, c, e.srv.URL+"/jobs/smoke/build", url.Values{"TIER": {"full"}})
	eq(t, "form without the field", res.StatusCode, http.StatusForbidden)
	res = post(t, c, e.srv.URL+"/jobs/smoke/build", url.Values{"TIER": {"full"}, "csrf": {"guess"}})
	eq(t, "form with a wrong field", res.StatusCode, http.StatusForbidden)

	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/jobs/smoke/build", strings.NewReader("TIER=quick"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(api.CSRFHeader, csrf)
	res, _ = do(t, c, req)
	eq(t, "htmx's header", res.StatusCode, http.StatusSeeOther)

	builds, err := e.st.Builds(context.Background(), "smoke", 10)
	must(t, err)
	eq(t, "builds triggered", len(builds), 2)
}

func TestOnlyAPathOnThisSiteIsALocalPath(t *testing.T) {
	for _, c := range [][2]string{
		{"", "/"},
		{"/", "/"},
		{"/jobs/smoke?x=1", "/jobs/smoke?x=1"},
		{"/jobs/smoke#frag", "/jobs/smoke#frag"},
		{"/%2Fevil.example", "/%2Fevil.example"},
		{"/login?next=//evil.example", "/login?next=//evil.example"},
		{"//evil.example", "/"},
		{"///evil.example", "/"},
		{`/\evil.example`, "/"},
		{`/jobs/\evil.example`, "/"},
		{"https://evil.example/", "/"},
		{"evil.example", "/"},
		{"javascript:alert(1)", "/"},
		{"/\t/evil.example", "/"},
		{"/\n/evil.example", "/"},
		{" /jobs", "/"},
	} {
		eq(t, strconv.Quote(c[0]), localPath(c[0]), c[1])
	}
}

// Every redirect is put together again from escaped parts, so what it
// sends is a path of this site even when what it was given was not.
func TestARedirectNamesAPageOfThisSiteAndNoOther(t *testing.T) {
	for _, c := range [][2]string{
		{"/", "/"},
		{"/jobs/site-smoke/12", "/jobs/site-smoke/12"},
		{"/jobs/a b/3?y=<b>&x=1", "/jobs/a%20b/3?x=1&y=%3Cb%3E"},
		{"/jobs/a%2Fb/3", "/jobs/a%2Fb/3"},
		{"/%2F%2Fevil.example", "/%2F%2Fevil.example"},
		{"/login?next=%2Fjobs%2Fsite-smoke", "/login?next=%2Fjobs%2Fsite-smoke"},
		{"/jobs/", "/jobs/"},
		{"//evil.example", "/"},
		{"///evil.example", "/"},
		{`/\evil.example`, "/"},
		{"https://evil.example/", "/"},
		{"javascript:alert(1)", "/"},
	} {
		w := httptest.NewRecorder()
		redirect(w, httptest.NewRequest(http.MethodGet, "/", nil), c[0], http.StatusSeeOther)
		eq(t, strconv.Quote(c[0]), w.Header().Get("Location"), c[1])
		eq(t, strconv.Quote(c[0])+" status", w.Code, http.StatusSeeOther)
	}
}

func TestLoginRedirectsOnlyToALocalPath(t *testing.T) {
	e := site(t)
	for next, want := range map[string]string{"/jobs/smoke": "/jobs/smoke", "//evil.example/x": "/", `/\evil.example`: "/", "https://evil.example": "/"} {
		c := e.anon(t)
		code, page := get(t, c, e.srv.URL+"/login?next="+url.QueryEscape(next))
		eq(t, "login page", code, http.StatusOK)
		contains(t, page, `name="next" value="`+want+`"`)
		res := post(t, c, e.srv.URL+"/login", url.Values{"name": {"admin"}, "password": {"pw"}, "next": {next}})
		eq(t, "login", res.StatusCode, http.StatusSeeOther)
		eq(t, "redirect for "+next, res.Header.Get("Location"), want)
	}
}

func TestLoginTakesThePasswordFromTheBodyOnly(t *testing.T) {
	e := site(t)
	res := post(t, e.anon(t), e.srv.URL+"/login?name=admin&password=pw", url.Values{})
	eq(t, "credentials in the URL", res.StatusCode, http.StatusUnauthorized)
}

func TestAFormOverTheLimitIsNotRead(t *testing.T) {
	e := site(t)
	body := "pad=" + strings.Repeat("x", maxForm) + "&name=admin&password=pw"
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, _ := do(t, e.anon(t), req)
	eq(t, "a login too large to read", res.StatusCode, http.StatusUnauthorized)
}

func TestLoginThatFailsInsideIsNotAWrongPassword(t *testing.T) {
	e := site(t)
	c := e.anon(t)
	must(t, e.st.Close())
	res, page := postBody(t, c, e.srv.URL+"/login", url.Values{"name": {"admin"}, "password": {"pw"}})
	eq(t, "a store that is down", res.StatusCode, http.StatusInternalServerError)
	lacks(t, page, "Wrong name or password", "sql", "closed")
	eq(t, "no cookie", len(res.Cookies()), 0)
}

// A browser too old to send Sec-Fetch-Site still sends Origin, which is
// then held against the Host.
func TestCrossSiteLoginIsRefused(t *testing.T) {
	e := site(t)
	form := url.Values{"name": {"admin"}, "password": {"pw"}}
	for _, c := range []struct {
		header, value string
		want          int
	}{
		{"Sec-Fetch-Site", "cross-site", http.StatusForbidden},
		{"Sec-Fetch-Site", "same-origin", http.StatusSeeOther},
		{"Origin", "https://evil.example", http.StatusForbidden},
		{"Origin", e.srv.URL, http.StatusSeeOther},
	} {
		req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set(c.header, c.value)
		res, _ := do(t, e.anon(t), req)
		eq(t, c.header+": "+c.value, res.StatusCode, c.want)
	}
}

// cookieNamed is the cookie c's jar holds for u by name.
func cookieNamed(t *testing.T, c *http.Client, u *url.URL, name string) *http.Cookie {
	t.Helper()
	for _, k := range c.Jar.Cookies(u) {
		if k.Name == name {
			return k
		}
	}
	t.Fatalf("no cookie %s in %+v", name, c.Jar.Cookies(u))
	return nil
}

func TestLogoutEndsTheSession(t *testing.T) {
	e := site(t)
	c := e.login(t, "admin")
	csrf := e.token(t, c)
	res := post(t, c, e.srv.URL+"/logout", url.Values{})
	eq(t, "logout without csrf", res.StatusCode, http.StatusForbidden)
	u, _ := url.Parse(e.srv.URL)
	session := cookieNamed(t, c, u, api.SessionCookie).Value

	res = post(t, c, e.srv.URL+"/logout", url.Values{"csrf": {csrf}})
	eq(t, "logout", res.StatusCode, http.StatusSeeOther)
	eq(t, "to login", res.Header.Get("Location"), "/login")
	gone := res.Cookies()[0]
	if gone.Name != api.SessionCookie || gone.MaxAge >= 0 || gone.SameSite != http.SameSiteLaxMode {
		t.Fatalf("logout cookie: %+v", gone)
	}
	// The device cookie stays: logging out does not make the browser a
	// stranger to the name it logged in as.
	left := c.Jar.Cookies(u)
	if len(left) != 1 || left[0].Name != api.DeviceCookie {
		t.Fatalf("cookies left: %+v", left)
	}
	if _, err := e.st.Session(context.Background(), session); err == nil {
		t.Fatal("the session is still in the store")
	}
}

// htmx would follow a redirect and swap the login form into the page, so
// it is told where to go in a header. After the login the reader is back
// on the page they asked for; a path that only takes a POST is no page.
func TestWithoutASessionTheLoginComesBackToAPageAndNeverToAPostOnlyPath(t *testing.T) {
	e := site(t)
	for _, c := range []struct {
		method, path string
		htmx         bool
		want         string
	}{
		{"GET", "/jobs/smoke/1?x=1", false, "/login?next=%2Fjobs%2Fsmoke%2F1%3Fx%3D1"},
		{"GET", "/jobs/smoke/1?x=1", true, "/login?next=%2Fjobs%2Fsmoke%2F1%3Fx%3D1"},
		{"GET", "/builds/1?x=1", false, "/login?next=%2Fbuilds%2F1%3Fx%3D1"},
		{"POST", "/jobs/smoke/build", false, "/login"},
		{"POST", "/jobs/smoke/1/cancel", true, "/login"},
		{"POST", "/logout", false, "/login"},
	} {
		req, _ := http.NewRequest(c.method, e.srv.URL+c.path, nil)
		header, status := "Location", http.StatusSeeOther
		if c.htmx {
			req.Header.Set("HX-Request", "true")
			header, status = "HX-Redirect", http.StatusUnauthorized
		}
		res, _ := do(t, e.anon(t), req)
		what := c.method + " " + c.path + " " + header
		eq(t, what+" status", res.StatusCode, status)
		eq(t, what, res.Header.Get(header), c.want)
	}
	// The mux redirects such a path away before any page sees it; were it
	// to arrive, the login would still lead nowhere but here.
	elsewhere := httptest.NewRequest(http.MethodGet, "/", nil)
	elsewhere.URL.Path = "//evil.example/"
	eq(t, "a path that reads as another site", loginFor(elsewhere), "/login?next=%2F")
}

func TestATokenIsNotABrowserSession(t *testing.T) {
	e := site(t)
	plain, hash, err := auth.NewToken()
	must(t, err)
	admin, err := e.st.UserByName(context.Background(), "admin")
	must(t, err)
	_, err = e.st.CreateToken(context.Background(), admin.ID, "ci", hash)
	must(t, err)
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/", nil)
	req.Header.Set("Authorization", "Bearer "+plain)
	res, _ := do(t, e.anon(t), req)
	eq(t, "a token on a page", res.StatusCode, http.StatusSeeOther)
}

var (
	localRef  = regexp.MustCompile(`(?:src|href|action)="([^"]*)"`)
	inlineTag = regexp.MustCompile(`<script(?:>|\s[^>]*>)|<style|\sstyle=|\son[a-z]+=`)
)

// selfContained fails if the page loads anything from another origin or
// carries a script or a style the CSP would refuse.
func selfContained(t *testing.T, page string) {
	t.Helper()
	for _, m := range localRef.FindAllStringSubmatch(page, -1) {
		if m[1] != "#main" && !strings.HasPrefix(m[1], "#log-") && (!strings.HasPrefix(m[1], "/") || strings.HasPrefix(m[1], "//")) {
			t.Fatalf("not a local reference: %s", m[0])
		}
	}
	for _, tag := range inlineTag.FindAllString(page, -1) {
		if !strings.HasPrefix(tag, "<script src=") {
			t.Fatalf("inline script or style: %s", tag)
		}
	}
}

// The groups are the header's menus, one per group, each listing its jobs;
// the name of the site is the way back to all of them, and no page above
// the list is called Jobs.
func TestTheHeaderHasAMenuForEachGroupAndTheJobsPageShowsOnlyTheOnePicked(t *testing.T) {
	e := site(t)
	c := e.login(t, "admin")

	code, page := get(t, c, e.srv.URL+"/jobs")
	eq(t, "all", code, http.StatusOK)
	contains(t, page, `<a class="brand" href="/">kipp</a>`, `<a class="navlink" href="/jobs" aria-current="page">Jobs</a>`,
		`<nav class="groups" id="groups" aria-label="Groups">`,
		`<summary>backend</summary>`, `<a class="menu-all" href="/jobs?group=backend">All in backend</a>`,
		`<summary>No group</summary>`, `<a class="menu-all" href="/jobs?only=ungrouped">All in No group</a>`,
		`hx-get="/jobs"`, `class="link" href="/jobs/smoke"`, `class="link" href="/jobs/slow"`)
	lacks(t, page, `class="tabs"`, `<h1>Jobs</h1>`, `>All</a>`, `href="/?`)

	_, page = get(t, c, e.srv.URL+"/jobs?group=backend")
	contains(t, page, `<a class="brand" href="/">kipp</a>`, `<a class="navlink" href="/jobs">Jobs</a>`, `<summary aria-current="true">backend</summary>`,
		`<a class="menu-all" href="/jobs?group=backend" aria-current="page">All in backend</a>`,
		`hx-get="/jobs?group=backend"`, `class="link" href="/jobs/smoke"`)
	lacks(t, page, `class="link" href="/jobs/slow"`)

	_, page = get(t, c, e.srv.URL+"/jobs?only=ungrouped")
	contains(t, page, `<summary aria-current="true">No group</summary>`, `hx-get="/jobs?only=ungrouped"`, `class="link" href="/jobs/slow"`)
	lacks(t, page, `class="link" href="/jobs/smoke"`)

	// A group nobody has, or one this user may not see, is the whole list.
	_, page = get(t, c, e.srv.URL+"/jobs?group=nowhere")
	contains(t, page, `<a class="navlink" href="/jobs" aria-current="page">Jobs</a>`, `hx-get="/jobs"`, `class="link" href="/jobs/smoke"`, `class="link" href="/jobs/slow"`)

	// A job's page and its builds' are in its group's menu, the job marked.
	_, page = get(t, c, e.srv.URL+"/jobs/smoke")
	contains(t, page, `<a class="brand" href="/">kipp</a>`, `<summary aria-current="true">backend</summary>`,
		`<a href="/jobs/smoke" aria-current="page">`)

	// The overview is the logo's page, and no menu is its own.
	_, page = get(t, c, e.srv.URL+"/")
	contains(t, page, `<a class="brand" href="/" aria-current="page">kipp</a>`, `<a class="navlink" href="/jobs">Jobs</a>`, `<a class="menu-all" href="/jobs?group=backend">All in backend</a>`)
	lacks(t, page, `<summary aria-current="true">`)

	// The removed jobs have a menu of their own, last.
	e.wait(t, e.build(t, c, "smoke", nil))
	e.drop("smoke")
	_, page = get(t, c, e.srv.URL+"/jobs?only=removed")
	contains(t, page, `<summary aria-current="true">Removed</summary>`, `class="link" href="/jobs/smoke"`)
	lacks(t, page, `class="link" href="/jobs/slow"`)
	eq(t, "removed is the last menu", strings.LastIndex(page, `<details class="group drop">`), strings.Index(page, `<details class="group drop">`+"\n"+`<summary aria-current="true">Removed`))

	// One group is still a menu: alice sees hers and its one job.
	_, page = get(t, e.login(t, "alice"), e.srv.URL+"/jobs")
	contains(t, page, `<summary>Removed</summary>`, `<a class="menu-all" href="/jobs?only=removed">All in Removed</a>`, `<a href="/jobs/smoke">`)
	eq(t, "alice's menus", strings.Count(page, `<details class="group drop">`), 1)
}

// A refresh swaps in a part of the page, never the header, so it is not
// asked for the menus.
func TestARefreshIsNotGivenTheHeadersMenus(t *testing.T) {
	e := site(t)
	c := e.login(t, "admin")
	for path, id := range map[string]string{"/": `id="overview"`, "/jobs": `id="jobs"`} {
		req := mustGet(t, e.srv.URL+path)
		req.Header.Set("HX-Request", "true")
		_, page := do(t, c, req)
		contains(t, page, id)
		lacks(t, page, `class="groups"`)
	}
}

// Admin sits with the user's name, and only for an admin.

// The page is as wide as the header: no measure of its own, and the same
// side padding as the header's at every width. Past --page both keep their
// content in one centred column, so a wide window does not stretch a row
// of the jobs table beyond what an eye can follow.
func TestThePageIsAsWideAsTheHeader(t *testing.T) {
	data, err := staticFS.ReadFile("static/kipp.css")
	must(t, err)
	shorthand := regexp.MustCompile(`(?m)^\s*padding:`)
	padded := 0
	for _, r := range cssRules(string(data)) {
		main, top := r.names("main"), r.names(".top")
		switch {
		case main && strings.Contains(r.decls, "max-width"):
			t.Fatalf("main has a width of its own: %s", r.text)
		case (main || top) && shorthand.MatchString(r.decls):
			t.Fatalf("a shorthand padding hides the sides: %s", r.text)
		case (main || top) && strings.Contains(r.decls, "padding-inline"):
			checkSharedSides(t, r)
			padded++
		}
	}
	if padded == 0 {
		t.Fatal("main and .top are padded nowhere")
	}
}

// A rule that pads main or .top pads both, and stops their content at --page.
func checkSharedSides(t *testing.T, r cssRule) {
	t.Helper()
	if !r.names("main") || !r.names(".top") {
		t.Fatalf("main and .top are padded apart: %s", r.text)
	}
	if !strings.Contains(r.decls, "(100% - var(--page)) / 2") {
		t.Fatalf("main and .top grow without end on a wide window: %s", r.text)
	}
}

// The theme a reader picked is a cookie the page is drawn with, so the
// first paint is already in it; anything but light or dark is the
// system's, and the picker's button says which one holds.
func TestThePageIsDrawnInTheThemeItsCookiePicks(t *testing.T) {
	e := site(t)
	for path, c := range map[string]*http.Client{"/login": e.anon(t), "/jobs/smoke": e.login(t, "admin")} {
		for value, want := range map[string]string{
			"":                    "system",
			"dark":                "dark",
			"light":               "light",
			"sepia":               "system",
			`dark"><script>x=1//`: "system",
		} {
			req := mustGet(t, e.srv.URL+path)
			if value != "" {
				// A browser sends the name and the value, never the attributes.
				req.Header.Add("Cookie", themeCookie+"="+url.QueryEscape(value))
			}
			res, page := do(t, c, req)
			eq(t, path+" "+value, res.StatusCode, http.StatusOK)
			if want == "system" {
				contains(t, page, "<html lang=\"en\">\n")
			} else {
				contains(t, page, `<html lang="en" data-theme="`+want+`">`)
			}
			contains(t, page, `<details class="theme drop" hidden>`+"\n"+`<summary aria-label="Theme" title="Theme" data-now="`+want+`">`)
			// It is the last thing at the top right, after the user menu when
			// there is one: one button whose menu holds the three picks, each
			// its icon and its name as text.
			_, top, _ := strings.Cut(page, `<div class="who">`)
			top, _, _ = strings.Cut(top, "</header>")
			eq(t, path+" picker at the top right", strings.Count(top, `data-theme-pick="`), 3)
			contains(t, strings.TrimSpace(top), "</ul>\n</details>\n</div>")
			contains(t, page, `data-theme-pick="`+want+`" aria-pressed="true"`)
			for _, name := range []string{"System", "Light", "Dark"} {
				contains(t, top, `<svg class="icon" viewBox="0 0 24 24" aria-hidden="true">`)
				contains(t, top, "</svg>"+name+"</button>")
			}
			_, foot, _ := strings.Cut(page, "<footer")
			lacks(t, foot, "theme")
			eq(t, value+" one pressed", strings.Count(page, `aria-pressed="true"`), 1)
			eq(t, value+" three picks", strings.Count(page, `data-theme-pick="`), 3)
			lacks(t, page, "<script>x=1")
			selfContained(t, page)
		}
	}
}

// cssRule is one rule of a stylesheet as the tests read it.
type cssRule struct{ text, selectors, decls string }

func cssRules(css string) []cssRule {
	var out []cssRule
	for _, m := range regexp.MustCompile(`(?m)^\s*([^{}\n]+)\{([^}]*)\}`).FindAllStringSubmatch(css, -1) {
		out = append(out, cssRule{m[0], m[1], m[2]})
	}
	return out
}

// names says whether the rule's selector list holds the selector.
func (r cssRule) names(selector string) bool {
	for sel := range strings.SplitSeq(r.selectors, ",") {
		if strings.TrimSpace(sel) == selector {
			return true
		}
	}
	return false
}

func TestTheLoginFormStandsInTheMiddleOfThePage(t *testing.T) {
	e := site(t)
	res, err := e.srv.Client().Get(e.srv.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	page, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(page), `<form class="panel stack narrow middle" method="post" action="/login">`) {
		t.Fatalf("the login form is not marked middle:\n%s", page)
	}
	css, err := staticFS.ReadFile("static/kipp.css")
	if err != nil {
		t.Fatal(err)
	}
	// main is a column as tall as the window; auto margins take what is left.
	if !strings.Contains(string(css), ".middle { margin: auto; }") {
		t.Fatal("kipp.css has no .middle rule")
	}
}

func TestEveryAnswerCarriesTheSecurityHeadersAndLoadsNothingFromElsewhere(t *testing.T) {
	e := site(t)
	c := e.login(t, "admin")
	check := func(what string, h http.Header, cache string) {
		t.Helper()
		eq(t, what+" CSP", h.Get("Content-Security-Policy"), "default-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		eq(t, what+" nosniff", h.Get("X-Content-Type-Options"), "nosniff")
		eq(t, what+" referrer", h.Get("Referrer-Policy"), "same-origin")
		eq(t, what+" robots", h.Get("X-Robots-Tag"), "noindex, nofollow")
		eq(t, what+" cache", h.Get("Cache-Control"), cache)
	}
	for _, path := range []string{"/", "/jobs/smoke", "/jobs/none", "/admin", "/login", "/nowhere", "/account", "/admin/users", "/admin/users/bob", "/admin/secrets", "/admin/server", "/admin/users/bob/delete"} {
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+path, nil)
		res, page := do(t, c, req)
		check(path, res.Header, "no-store")
		eq(t, path+" type", res.Header.Get("Content-Type"), "text/html; charset=utf-8")
		selfContained(t, page)
	}
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/", nil)
	res, _ := do(t, e.anon(t), req)
	check("redirect", res.Header, "no-store")
	req, _ = http.NewRequest(http.MethodGet, e.srv.URL+"/static/kipp.css", nil)
	res, _ = do(t, e.anon(t), req)
	check("static", res.Header, "no-cache")
}

// The job list is one line a job: what a job is for is said on its own
// page, and on the list only when the pointer rests on its name.
func TestTheJobListKeepsEachJobToOneLine(t *testing.T) {
	e := site(t)
	c := e.login(t, "admin")
	_, page := get(t, c, e.srv.URL+"/jobs")
	contains(t, page, `<a class="link" href="/jobs/nightly" title="&lt;script&gt;alert(&#39;job&#39;)&lt;/script&gt;">nightly</a>`)
	// A quote in a description cannot end the attribute it is in.
	contains(t, page, `<a class="link" href="/jobs/quoted" title="ends &#34;the title&#34; data-x=&#34;1&#34; &amp; &lt;b&gt;bold&lt;/b&gt;">quoted</a>`)
	lacks(t, page, `data-x="1"`, "<b>bold")
	_, jobs, _ := strings.Cut(page, `<div id="jobs"`)
	lacks(t, jobs, `<div class="sub">`)
	_, page = get(t, c, e.srv.URL+"/jobs/nightly")
	contains(t, page, `<p class="lead">&lt;script&gt;alert(&#39;job&#39;)&lt;/script&gt;</p>`)
}

func TestWhatARepoWritesIsEscaped(t *testing.T) {
	e := site(t)
	c := e.login(t, "admin")
	for _, path := range []string{"/jobs", "/jobs/nightly"} {
		code, page := get(t, c, e.srv.URL+path)
		eq(t, path, code, http.StatusOK)
		contains(t, page, "&lt;script&gt;alert(&#39;job&#39;)&lt;/script&gt;")
		lacks(t, page, "<script>alert")
	}

	id := e.build(t, c, "nightly", nil)
	eq(t, "status", e.wait(t, id).Status, store.Failed)
	eventually(t, "the tests are stored", func() bool {
		tests, err := e.st.Tests(context.Background(), id)
		return err == nil && len(tests) == 3
	})
	for _, path := range []string{e.at(t, id), "/jobs/nightly"} {
		code, page := get(t, c, e.srv.URL+path)
		eq(t, path, code, http.StatusOK)
		contains(t, page, "&lt;b&gt;bad&lt;/b&gt;")
		lacks(t, page, "<b>bad", "<script>boom")
		selfContained(t, page)
	}
	_, page := get(t, c, e.srv.URL+e.at(t, id))
	contains(t, page, "&lt;script&gt;boom&lt;/script&gt;")

	svg := drawn(t, []*store.Stage{{Name: `<script>x</script>`, Status: `"><script>`}}, nil)
	lacks(t, svg, "<script>")
	contains(t, svg, "&lt;script&gt;")
}

func TestBuildPageShowsTestsLogsAndHistory(t *testing.T) {
	e := site(t)
	c := e.login(t, "admin")
	id := e.build(t, c, "nightly", url.Values{"NOTE": {"hi"}, "DRY": {"false", "true"}})
	path := e.at(t, id)
	eq(t, "status", e.wait(t, id).Status, store.Failed)
	var page string
	eventually(t, "the hook has run", func() bool {
		_, page = get(t, c, e.srv.URL+path)
		return strings.Contains(page, "/stages/hooks/log")
	})
	contains(t, page,
		`class="status failed"`, "<span>exit ", "DRY=true", "NOTE=hi",
		"1 failed", "1 passed", "1 skipped",
		// The page is for the test that failed; the rest are counted, and one click away.
		`<details class="results" id="results" open>`, "&lt;b&gt;bad", `href="`+path+`?results=all"`, "Show all 3</a>",
		`<details class="log" id="log-report" open>`, `data-log="/api/v1/builds/1/stages/report/log"`,
		`<details class="log" id="log-hooks">`,
	)
	lacks(t, page, "/stages/clone/log", "/stages/prepare/log", `href="`+path+`/cancel"`)
	lacks(t, page, `data-label="Case">fine`, "500ms")
	_, all := get(t, c, e.srv.URL+path+"?results=all")
	contains(t, all, `<details class="results" id="results" open>`, `data-label="Case">fine`, "500ms", `href="`+path+`"`, "Show only the 1 that failed</a>")
	if strings.Index(all, "&lt;b&gt;bad") > strings.Index(all, `data-label="Case">fine`) {
		t.Fatal("the failed test should come first")
	}

	id2 := e.build(t, c, "nightly", nil)
	e.wait(t, id2)
	eventually(t, "the second build's tests are stored", func() bool {
		tests, err := e.st.Tests(context.Background(), id2)
		return err == nil && len(tests) == 3
	})
	must(t, e.st.AddActivity(context.Background(), "nightly", "cron skipped: <still> running", time.Now()))
	code, page := get(t, c, e.srv.URL+"/jobs/nightly")
	eq(t, "job", code, http.StatusOK)
	contains(t, page,
		`<input type="hidden" name="DRY" value="false">`, `name="DRY" value="true" checked`, `name="NOTE" value="hello"`,
		`class="dot failed" title="#2 failed"`, `class="dot failed" title="#1 failed"`,
		"cron skipped: &lt;still&gt; running", "0 3 * * *",
		// Three tests ran; the page is for the one that failed, and says how to see the rest.
		"1 of 3 cases failed in the last 2 builds",
		// A test failed, so the section starts open; the rest are fetched into its box, or by the plain link.
		`<details class="results" id="results" open>`, `<summary><h2>Results</h2>`,
		`<a class="link" href="/jobs/nightly?results=all" hx-get="/jobs/nightly?results=all" hx-select="#results-box" hx-target="#results-box" hx-swap="outerHTML">Show all 3</a>`,
		`<div class="scroll tall">`,
	)
	lacks(t, page, `class="dot skipped"`, ">fine<")
	_, page = get(t, c, e.srv.URL+"/jobs/nightly?results=all")
	contains(t, page, `class="dot failed" title="#2 failed"`, `class="dot passed" title="#1 passed"`, `class="dot skipped"`, ">fine<",
		`<details class="results" id="results" open>`,
		`<a class="link" href="/jobs/nightly" hx-get="/jobs/nightly" hx-select="#results-box" hx-target="#results-box" hx-swap="outerHTML">Show only the 1 that failed</a>`)
	// Every row is inside the box the control swaps.
	_, box, _ := strings.Cut(page, `<div id="results-box"`)
	box, _, _ = strings.Cut(box, "</details>")
	eq(t, "rows in the box", strings.Count(box, `<td class="dots" data-label="History">`), 3)
}

// visibleText is what a person reads of a page: its text and the titles
// and labels it shows, without its markup, scripts or addresses.
func visibleText(page string) string {
	page = regexp.MustCompile(`(?s)<(script|style)[^>]*>.*?</(script|style)>`).ReplaceAllString(page, " ")
	var shown []string
	for _, m := range regexp.MustCompile(`(?:title|aria-label|placeholder)="([^"]*)"`).FindAllStringSubmatch(page, -1) {
		shown = append(shown, m[1])
	}
	return regexp.MustCompile(`(?s)<[^>]*>`).ReplaceAllString(page, " ") + " " + strings.Join(shown, " ")
}

func TestTheBuildAndJobPagesSayResultsAndNoVisibleTests(t *testing.T) {
	e := site(t)
	c := e.login(t, "admin")
	id := e.build(t, c, "nightly", nil)
	e.wait(t, id)
	eventually(t, "the cases are stored", func() bool {
		cases, err := e.st.Tests(context.Background(), id)
		return err == nil && len(cases) == 3
	})
	word := regexp.MustCompile(`(?i)\btests?\b`)
	for _, path := range []string{e.at(t, id), e.at(t, id) + "?results=all", "/jobs/nightly", "/jobs/nightly?results=all"} {
		_, page := get(t, c, e.srv.URL+path)
		contains(t, page, "<h2>Results</h2>")
		if found := word.FindAllString(visibleText(page), -1); len(found) > 0 {
			t.Errorf("%s shows %q", path, found)
		}
	}
}

func TestTheTestsBoxScrollsInsideItselfUnderAHeaderThatStaysAndFadesWhereThereIsMore(t *testing.T) {
	data, err := staticFS.ReadFile("static/kipp.css")
	must(t, err)
	css := string(data)
	contains(t, css,
		".tall { max-height: 60vh; overflow-y: auto; }",
		".tall th { position: sticky; top: 0; }",
		// The fade takes its own room after the last row, so at the end of
		// the list, and under a short one, it lies over nothing.
		".tall::after {", "position: sticky;", "bottom: 0;", "pointer-events: none;",
		"linear-gradient(to bottom, transparent, var(--bg))",
	)
}

func TestAJobWhoseTestsAllPassListsNoneOfThemUntilAsked(t *testing.T) {
	e := site(t)
	c := e.login(t, "admin")
	id := e.build(t, c, "shaky", nil)
	e.wait(t, id)
	eventually(t, "the build's tests are stored", func() bool {
		tests, err := e.st.Tests(context.Background(), id)
		return err == nil && len(tests) > 0
	})
	_, page := get(t, c, e.srv.URL+"/jobs/shaky")
	// No test failed: the section is one closed line.
	contains(t, page, "No case failed in the last build", `<details class="results" id="results">`,
		`href="/jobs/shaky?results=all" hx-get="/jobs/shaky?results=all" hx-select="#results-box" hx-target="#results-box" hx-swap="outerHTML">Show all 1</a>`)
	lacks(t, page, `class="dot`, `<details class="results" id="results" open>`)
	_, page = get(t, c, e.srv.URL+"/jobs/shaky?results=all")
	contains(t, page, `class="dot passed"`, `<details class="results" id="results" open>`, ">Hide them</a>")
}

func TestTheJobsTableGivesItsLastThreeColumnsRoomAndKeepsThemWhole(t *testing.T) {
	e := site(t)
	_, page := get(t, e.login(t, "admin"), e.srv.URL+"/jobs")
	contains(t, page, `<table class="jobs stacked">`)
	css, err := staticFS.ReadFile("static/kipp.css")
	must(t, err)
	contains(t, string(css),
		".jobs th:not(:first-child), .jobs td:not(:first-child) { white-space: nowrap; }",
		".jobs th:nth-child(1) { width: 34%; }",
		".jobs th:nth-child(2) { width: 28%; }",
		".jobs th:nth-child(3) { width: 20%; }",
		".jobs th:nth-child(4) { width: 18%; }")
	lacks(t, string(css), "width: 1%")
}

func TestTheJobListShowsADotForEachOfTheLastFiveFinishedBuilds(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	statuses := []store.Status{store.Failed, store.Success, store.Success, store.Unstable, store.Cancelled, store.Timeout, store.Failed}
	for _, st := range statuses {
		must(t, e.st.CreateBuild(context.Background(), &store.Build{Job: "smoke", Status: st, Trigger: "cli"}))
	}
	_, page := get(t, admin, e.srv.URL+"/jobs")
	_, row, _ := strings.Cut(page, `<a class="link" href="/jobs/smoke"`)
	row, _, _ = strings.Cut(row, "</tr>")
	_, cell, _ := strings.Cut(row, "</td>")
	cell, _, _ = strings.Cut(cell, "</td>")
	var got []string
	for _, m := range regexp.MustCompile(`<a class="dot ([a-z]+)" href="(/jobs/smoke/\d+)" title="(#\d+ [a-z]+)"></a>`).FindAllStringSubmatch(cell, -1) {
		got = append(got, m[1]+" "+m[2]+" "+m[3])
	}
	want := []string{
		"failed /jobs/smoke/7 #7 failed",
		"timeout /jobs/smoke/6 #6 timeout",
		"cancelled /jobs/smoke/5 #5 cancelled",
		"unstable /jobs/smoke/4 #4 unstable",
		"success /jobs/smoke/3 #3 success",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("dots in the Last build cell:\n got %q\nwant %q\ncell %s", got, want, cell)
	}
	css, err := staticFS.ReadFile("static/kipp.css")
	must(t, err)
	contains(t, string(css),
		".dot.success { background-color: var(--ok); }",
		".dot.unstable, .dot.timeout, .dot.lost { background-color: var(--warn); }")
}

func TestAJobWithTwoBuildsHasTwoDotsAndOneNeverBuiltHasNone(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	for range 2 {
		must(t, e.st.CreateBuild(context.Background(), &store.Build{Job: "smoke", Status: store.Success, Trigger: "cli"}))
	}
	_, page := get(t, admin, e.srv.URL+"/jobs")
	eq(t, "dots", strings.Count(page, `<a class="dot `), 2)
	contains(t, page, "never built")
}

func TestHistoryRowsAlignTestsToBuilds(t *testing.T) {
	rows := historyRows([]store.TestRow{
		{BuildNumber: 3, Suite: "s", Name: "a", Status: "passed"},
		{BuildNumber: 3, Suite: "s", Name: "new", Status: "failed"},
		{BuildNumber: 3, Suite: "s", Name: "new", Status: "passed"},
		{BuildNumber: 2, Suite: "s", Name: "a", Status: "failed"},
		{BuildNumber: 1, Suite: "s", Name: "a", Status: "passed"},
		{BuildNumber: 1, Suite: "s", Name: "old", Status: "skipped"},
	})
	eq(t, "rows", len(rows), 3)
	want := map[string][]dot{
		"a":   {{3, "passed"}, {2, "failed"}, {1, "passed"}},
		"new": {{3, "failed"}, {2, ""}, {1, ""}},
		"old": {{3, ""}, {2, ""}, {1, "skipped"}},
	}
	for _, r := range rows {
		eq(t, r.Name+" dots", len(r.Dots), 3)
		for i, d := range r.Dots {
			eq(t, r.Name+" dot "+strconv.Itoa(i), d, want[r.Name][i])
		}
	}
}

func TestARunningBuildRefreshesAndCanBeCancelled(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	id := e.build(t, admin, "slow", nil)
	path := e.at(t, id)
	eventually(t, "the build runs", func() bool {
		b, err := e.st.Build(context.Background(), id)
		return err == nil && b.Status == store.Running
	})
	code, page := get(t, admin, e.srv.URL+path)
	eq(t, "running build", code, http.StatusOK)
	contains(t, page, `hx-get="`+path+`"`, `hx-select="#live"`, `hx-trigger="every 2s"`, `href="`+path+`/cancel"`, `hx-headers="{&#34;X-CSRF-Token&#34;:&#34;`+csrfOf(t, page)+`&#34;}"`)

	// bob may view slow and nothing more.
	bob := e.login(t, "bob")
	code, page = get(t, bob, e.srv.URL+path)
	eq(t, "bob sees it", code, http.StatusOK)
	lacks(t, page, "/cancel")
	res, page := postBody(t, bob, e.srv.URL+path+"/cancel", url.Values{"csrf": {e.token(t, bob)}})
	eq(t, "bob cancels", res.StatusCode, http.StatusForbidden)
	contains(t, page, "You are not allowed to do that.")
	code, page = get(t, bob, e.srv.URL+"/jobs/slow")
	eq(t, "bob sees the job", code, http.StatusOK)
	lacks(t, page, `action="/jobs/slow/build"`)

	res, page = postBody(t, admin, e.srv.URL+"/jobs/slow/build", url.Values{"csrf": {e.token(t, admin)}})
	eq(t, "a dropped trigger", res.StatusCode, http.StatusConflict)
	contains(t, page, "the job is running or already queued")

	res = post(t, admin, e.srv.URL+path+"/cancel", url.Values{"csrf": {e.token(t, admin)}})
	eq(t, "cancel", res.StatusCode, http.StatusSeeOther)
	eq(t, "back to the build", res.Header.Get("Location"), path)
	eq(t, "status", e.wait(t, id).Status, store.Cancelled)
	res, page = postBody(t, admin, e.srv.URL+path+"/cancel", url.Values{"csrf": {e.token(t, admin)}})
	eq(t, "cancel again", res.StatusCode, http.StatusConflict)
	contains(t, page, "can no longer be cancelled")
}

func TestABadParamIsA400PageAndAnUnknownPathA404(t *testing.T) {
	e := site(t)
	c := e.login(t, "admin")
	res, page := postBody(t, c, e.srv.URL+"/jobs/smoke/build", url.Values{"csrf": {e.token(t, c)}, "TIER": {"<b>none</b>"}})
	eq(t, "a bad param", res.StatusCode, http.StatusBadRequest)
	contains(t, page, "&lt;b&gt;none&lt;/b&gt;")
	for _, path := range []string{"/builds/x", "/builds/99", "/nowhere", "/jobs/smoke/extra"} {
		code, _ := get(t, c, e.srv.URL+path)
		eq(t, path, code, http.StatusNotFound)
	}

	must(t, e.st.Close())
	code, page := get(t, c, e.srv.URL+"/static/kipp.css")
	eq(t, "static without the store", code, http.StatusOK)
	lacks(t, page, "<html")
}

func TestAServerErrorSaysNothingOfItsCause(t *testing.T) {
	e := site(t)
	rec := httptest.NewRecorder()
	e.site.fail(rec, httptest.NewRequest(http.MethodGet, "/", nil), os.ErrDeadlineExceeded)
	eq(t, "status", rec.Code, http.StatusInternalServerError)
	contains(t, rec.Body.String(), "Something went wrong")
	lacks(t, rec.Body.String(), os.ErrDeadlineExceeded.Error())
}

func TestAHiddenJobLooksLikeAMissingOne(t *testing.T) {
	e := site(t)
	id := e.build(t, e.login(t, "admin"), "slow", nil)
	alice := e.login(t, "alice")
	for hidden, missing := range map[string]string{
		"/jobs/slow":                           "/jobs/none",
		e.at(t, id):                            "/jobs/none/1",
		"/builds/" + strconv.FormatInt(id, 10): "/builds/99",
	} {
		codeHidden, pageHidden := get(t, alice, e.srv.URL+hidden)
		codeMissing, pageMissing := get(t, alice, e.srv.URL+missing)
		eq(t, hidden, codeHidden, http.StatusNotFound)
		eq(t, missing, codeMissing, http.StatusNotFound)
		eq(t, hidden+" against "+missing, pageHidden, pageMissing)
	}
	res, page := postBody(t, alice, e.srv.URL+"/jobs/slow/build", url.Values{"csrf": {e.token(t, alice)}})
	eq(t, "building a hidden job", res.StatusCode, http.StatusNotFound)
	lacks(t, page, "slow")
	_, page = get(t, alice, e.srv.URL+"/")
	contains(t, page, `href="/jobs/smoke"`)
	lacks(t, page, "slow", "nightly", `href="/admin"`)
}

func TestTheBannerShowsWhenSet(t *testing.T) {
	e := site(t)
	for banner, want := range map[string]bool{"": false, "reload failed: <jobs/x.yml>": true} {
		rec := httptest.NewRecorder()
		e.site.render(rec, httptest.NewRequest(http.MethodGet, "/login", nil), "login", http.StatusOK, page{Title: "Log in", Banner: banner, Data: loginData{Next: "/"}})
		eq(t, "status", rec.Code, http.StatusOK)
		eq(t, "banner "+strconv.Quote(banner), strings.Contains(rec.Body.String(), `role="alert"`), want)
		lacks(t, rec.Body.String(), "<jobs/x.yml>")
	}
}

// Every template is a page some test above asks for, so that one that does
// not execute fails a test. A new template fails here until it is listed.
func TestEveryTemplateIsAPage(t *testing.T) {
	tested := map[string]bool{"login": true, "jobs": true, "job": true, "build": true, "error": true, "queue": true,
		"account": true, "sudo": true, "once": true, "confirm": true, "users": true, "user": true, "secrets": true, "server": true, "runners": true,
		"metrics": true, "metrics_store": true, "cache": true, "apps": true, "appgo": true, "overview": true, "authorize": true}
	files, err := templateFS.ReadDir("templates")
	must(t, err)
	e := site(t)
	eq(t, "pages", len(e.site.pages), len(files)-1)
	for _, f := range files {
		name := strings.TrimSuffix(f.Name(), ".html")
		if name == "layout" {
			continue
		}
		if e.site.pages[name] == nil || !tested[name] {
			t.Fatalf("template %s is not a page with a test", f.Name())
		}
	}
}

func TestAPageThatDoesNotExecuteIsA500(t *testing.T) {
	e := site(t)
	rec := httptest.NewRecorder()
	e.site.render(rec, httptest.NewRequest(http.MethodGet, "/", nil), "jobs", http.StatusOK, page{Data: "not the jobs data"})
	eq(t, "status", rec.Code, http.StatusInternalServerError)
	lacks(t, rec.Body.String(), "<html")
}

func TestDAGSVGLaysOutByDepth(t *testing.T) {
	stages := []*store.Stage{{Name: "lint", Status: store.StageSuccess}, {Name: "unit", Status: store.StageRunning}, {Name: "smoke", Status: store.StagePending}}
	svg := drawn(t, stages, map[string][]string{"lint": nil, "unit": nil, "smoke": {"lint", "unit"}})
	if !strings.Contains(svg, `class="stage success"`) || !strings.Contains(svg, `class="stage running"`) || strings.Count(svg, "<line") != 2 {
		t.Fatalf("%s", svg)
	}
	lint, smoke := strings.Index(svg, `data-stage="lint"`), strings.Index(svg, `data-stage="smoke"`)
	if lint < 0 || smoke < 0 || lint > smoke {
		t.Fatal("roots should come first")
	}
	contains(t, svg, `<rect x="0" y="0"`, `<rect x="0" y="52"`, `<rect x="220" y="0"`, `viewBox="0 0 380 88"`)
}

func TestABuildPageDrawsTheEdgesTheBuildRanWithAfterAReload(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	id := e.build(t, admin, "smoke", nil)
	eq(t, "status", e.wait(t, id).Status, store.Success)
	jobs := filepath.Join(t.TempDir(), "jobs")
	must(t, os.MkdirAll(jobs, 0o750))
	changed := "name: smoke\nstages:\n  lint: {run: echo lint}\n  unit: {run: echo unit}\n  smoke: {run: echo smoke}\n  deploy: {needs: [smoke], run: echo deploy}\n"
	must(t, os.WriteFile(filepath.Join(jobs, "smoke.yml"), []byte(changed), 0o600))
	snap, err := config.Load(jobs)
	must(t, err)
	e.d.SetSnapshot(snap)
	code, page := get(t, admin, e.srv.URL+e.at(t, id))
	eq(t, "the build's page", code, http.StatusOK)
	eq(t, "edges", strings.Count(page, "<line"), 2)
	lacks(t, page, `data-stage="deploy"`)
}

// The needs are the build's own. One that names no stage of the build
// still draws no line, so that a row that cannot be read as a graph never
// costs the page its picture.
func TestDAGSVGLeavesOutANeedThatIsNotAStage(t *testing.T) {
	stages := []*store.Stage{{Name: "a", Status: store.StageSuccess}, {Name: "b", Status: store.StageFailed}}
	svg := drawn(t, stages, map[string][]string{"b": {"a", "gone"}, "other": nil})
	contains(t, svg, `data-stage="a"`, `data-stage="b"`)
	lacks(t, svg, "gone", "other")
	eq(t, "edges", strings.Count(svg, "<line"), 1)
	svg = drawn(t, stages, nil)
	contains(t, svg, `data-stage="a"`, `data-stage="b"`)
	eq(t, "no stages", drawn(t, nil, nil), "")

	long := drawn(t, []*store.Stage{{Name: "integration-tests-postgres-17", Status: store.StagePending}}, nil)
	contains(t, long, "<title>integration-tests-postgres-17: pending</title>", ">integration-tests-...</text>")
}

func TestAPageNamesItsStaticFilesByTheirContentSoACacheNeverServesAnOldOne(t *testing.T) {
	e := site(t)
	c := e.anon(t)
	_, page := get(t, c, e.srv.URL+"/login")
	refs := regexp.MustCompile(`(?:href|src)="(/static/[a-z.]+\?v=([0-9a-f]{16}))"`).FindAllStringSubmatch(page, -1)
	eq(t, "the layout's seven files, each with its content's name", len(refs), 7)
	lacks(t, page, `/static/kipp.css"`, `/static/htmx.min.js"`)
	for _, m := range refs {
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+m[1], nil)
		res, _ := do(t, c, req)
		eq(t, m[1], res.StatusCode, http.StatusOK)
		eq(t, m[1]+" is its content's name", res.Header.Get("ETag"), `"`+m[2]+`"`)
		eq(t, m[1]+" may be kept for good", res.Header.Get("Cache-Control"), "public, max-age=31536000, immutable")
	}
	// Any other name for the file may be another content tomorrow.
	for _, path := range []string{"/static/kipp.css", "/static/kipp.css?v=0000000000000000"} {
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+path, nil)
		res, _ := do(t, c, req)
		eq(t, path, res.Header.Get("Cache-Control"), "no-cache")
	}
}

func TestStaticFilesAreServedWithTheirTypesAndAnETag(t *testing.T) {
	e := site(t)
	c := e.anon(t)
	served := map[string][]string{
		"text/javascript; charset=utf-8": {"/static/htmx.min.js", "/static/build.js", "/static/theme.js"},
		"image/svg+xml":                  {"/static/favicon.svg", "/favicon.ico"},
		"text/css; charset=utf-8":        {"/static/tokens.css", "/static/kipp.css", "/static/base.css"},
	}
	bodies := map[string]string{}
	for want, paths := range served {
		for _, path := range paths {
			req, _ := http.NewRequest(http.MethodGet, e.srv.URL+path, nil)
			res, body := do(t, c, req)
			eq(t, path, res.StatusCode, http.StatusOK)
			eq(t, path+" type", res.Header.Get("Content-Type"), want)
			bodies[path] = body

			req.Header.Set("If-None-Match", res.Header.Get("ETag"))
			res, _ = do(t, c, req)
			eq(t, path+" unchanged", res.StatusCode, http.StatusNotModified)
		}
	}
	sum := sha256.Sum256([]byte(bodies["/static/htmx.min.js"]))
	eq(t, "htmx.min.js is htmx.org@2.0.11", hex.EncodeToString(sum[:]), htmxSHA256)
	contains(t, bodies["/static/htmx.min.js"], `version:"2.0.11"`)

	for _, path := range []string{"/static/none.css", "/static/"} {
		code, _ := get(t, c, e.srv.URL+path)
		eq(t, path, code, http.StatusNotFound)
	}
	// The type is the system's: no stylesheet asks for a file of its own.
	for _, css := range []string{"/static/tokens.css", "/static/base.css", "/static/kipp.css"} {
		lacks(t, bodies[css], "@font-face", "url(")
	}
}

var (
	declared = regexp.MustCompile(`(?m)^\s*(--[a-z0-9-]+):`)
	used     = regexp.MustCompile(`var\((--[a-z0-9-]+)`)
)

// The tokens are plain custom properties a browser reads as they are.
func TestTokensArePlainCustomProperties(t *testing.T) {
	data, err := staticFS.ReadFile("static/tokens.css")
	must(t, err)
	css := string(data)
	lacks(t, css, "@theme", "-*:")
	contains(t, css, ":root {", "--ok-soft: light-dark(#e5f5ea, #112b1b);", "--font-ui: system-ui,")
	eq(t, "braces", strings.Count(css, "{"), strings.Count(css, "}"))
}

// Each colour role has a light and a dark value, and the page wears the
// one the system asks for unless the reader picked one: color-scheme is
// what light-dark() reads, so the system's choice is followed the moment
// it changes, with no script.
func TestEveryColourRoleHasALightAndADarkValueAndThePageFollowsTheSystem(t *testing.T) {
	data, err := staticFS.ReadFile("static/tokens.css")
	must(t, err)
	css := string(data)
	contains(t, css,
		"  color-scheme: light dark;",
		`:root[data-theme="light"] { color-scheme: light; }`,
		`:root[data-theme="dark"] { color-scheme: dark; }`)
	lacks(t, css, "prefers-color-scheme")
	for _, m := range regexp.MustCompile(`(?m)^\s*(--[a-z-]+):\s*([^;]*(?:#[0-9a-f]{3,8}|rgb\()[^;]*);`).FindAllStringSubmatch(css, -1) {
		if !strings.HasPrefix(m[2], "light-dark(") && !strings.Contains(m[2], " light-dark(") {
			t.Errorf("%s has one value for both themes: %s", m[1], m[2])
		}
	}
}

// Every pair of roles a page puts one on the other holds WCAG 2.2's
// contrast: 4.5:1 for text, 3:1 for an outline, a mark or a focus ring,
// in the light theme and in the dark one.
func TestTheColourRolesHoldTheirContrast(t *testing.T) {
	data, err := staticFS.ReadFile("static/tokens.css")
	must(t, err)
	themes := map[string]map[string]string{"light": {}, "dark": {}}
	for _, m := range regexp.MustCompile(`(?m)^\s*--([a-z-]+):\s*light-dark\((#[0-9a-f]{6}),\s*(#[0-9a-f]{6})\);`).FindAllStringSubmatch(string(data), -1) {
		themes["light"][m[1]], themes["dark"][m[1]] = m[2], m[3]
	}
	text := [][2]string{
		{"fg", "bg"}, {"fg", "bg-raised"}, {"fg", "bg-sunken"},
		{"fg-muted", "bg"}, {"fg-muted", "bg-raised"}, {"fg-muted", "bg-sunken"},
		{"fg-subtle", "bg"}, {"fg-subtle", "bg-raised"}, {"fg-subtle", "bg-sunken"},
		{"accent-text", "bg"}, {"accent-text", "bg-raised"},
		{"on-primary", "primary"}, {"on-primary", "primary-hover"},
		{"on-bad", "bad"}, {"on-bad", "bad-hover"},
		{"on-accent-soft", "accent-soft"}, {"on-ok-soft", "ok-soft"}, {"on-warn-soft", "warn-soft"},
		{"on-bad-soft", "bad-soft"}, {"on-busy-soft", "busy-soft"}, {"on-busy-soft", "bg-raised"},
	}
	// A log's colours go on its sunken ground as text; its background colours
	// carry the default text.
	for _, name := range ansiColours {
		text = append(text, [2]string{"ansi-" + name, "bg-sunken"}, [2]string{"fg", "ansi-bg-" + name})
	}
	marks := [][2]string{
		{"line-strong", "bg"}, {"line-strong", "bg-sunken"}, {"focus", "bg"}, {"focus", "bg-raised"},
		{"accent", "bg-raised"}, {"ok", "bg"}, {"warn", "bg"}, {"bad", "bg"}, {"busy", "bg"},
		{"ok", "ok-soft"}, {"warn", "warn-soft"}, {"bad", "bad-soft"}, {"busy", "busy-soft"},
		// The graphs' marks on their cards.
		{"ok", "bg-raised"}, {"warn", "bg-raised"}, {"bad", "bg-raised"}, {"busy", "bg-raised"}, {"line-strong", "bg-raised"},
	}
	for theme, role := range themes {
		holdContrast(t, theme, role, 4.5, text)
		holdContrast(t, theme, role, 3, marks)
	}
}

// ansiColours are the 16 colours a build's output can use, as build.js names
// them in its classes.
var ansiColours = func() []string {
	var names []string
	for _, suffix := range []string{"", "-bright"} {
		for _, c := range []string{"black", "red", "green", "yellow", "blue", "magenta", "cyan", "white"} {
			names = append(names, c+suffix)
		}
	}
	return names
}()

// A colour a build printed is painted from the roles: every class build.js
// can give a span has a rule in the stylesheet that uses its own role, and
// the roles are in both themes.
func TestTheLogsColourClassesPaintInTheirRoles(t *testing.T) {
	css, err := staticFS.ReadFile("static/kipp.css")
	must(t, err)
	tokens, err := staticFS.ReadFile("static/tokens.css")
	must(t, err)
	for _, name := range ansiColours {
		contains(t, string(css),
			".log pre .ansi-fg-"+name+" { color: var(--ansi-"+name+"); }",
			".log pre .ansi-bg-"+name+" { background-color: var(--ansi-bg-"+name+"); }")
		contains(t, string(tokens), "--ansi-"+name+": light-dark(", "--ansi-bg-"+name+": light-dark(")
	}
	contains(t, string(css),
		".log pre .ansi-b { font-weight: var(--weight-bold); }",
		".log pre .ansi-dim { opacity: 0.7; }",
		".log pre .ansi-i { font-style: italic; }",
		".log pre .ansi-u { text-decoration: underline; }")
}

// holdContrast fails each pair of one theme's roles under the ratio.
func holdContrast(t *testing.T, theme string, role map[string]string, want float64, pairs [][2]string) {
	t.Helper()
	for _, p := range pairs {
		fg, bg := role[p[0]], role[p[1]]
		if fg == "" || bg == "" {
			t.Fatalf("%s on %s: not a colour role of tokens.css", p[0], p[1])
		}
		if got := contrast(fg, bg); got < want {
			t.Errorf("%s: %s on %s: %.2f:1, want %.1f:1", theme, p[0], p[1], got, want)
		}
	}
}

// contrast is WCAG's ratio between two #rrggbb colours.
func contrast(a, b string) float64 {
	lum := func(hex string) float64 {
		var rgb [3]float64
		for i := range rgb {
			v, _ := strconv.ParseUint(hex[1+2*i:3+2*i], 16, 8)
			c := float64(v) / 255
			if c <= 0.04045 {
				rgb[i] = c / 12.92
			} else {
				rgb[i] = math.Pow((c+0.055)/1.055, 2.4)
			}
		}
		return 0.2126*rgb[0] + 0.7152*rgb[1] + 0.0722*rgb[2]
	}
	hi, lo := lum(a), lum(b)
	if lo > hi {
		hi, lo = lo, hi
	}
	return (hi + 0.05) / (lo + 0.05)
}

// The pages' rules paint only in the tokens' roles and set type only from
// its scale.
func TestPageRulesUseOnlyTheTokens(t *testing.T) {
	data, err := staticFS.ReadFile("static/kipp.css")
	must(t, err)
	css := string(data)
	if m := regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b|\b(?:rgba?|hsla?|oklch|oklab|color-mix)\(`).FindString(css); m != "" {
		t.Fatalf("a colour outside the roles: %s", m)
	}
	for _, m := range regexp.MustCompile(`font-size:\s*([^;]+);`).FindAllStringSubmatch(css, -1) {
		if !strings.HasPrefix(m[1], "var(--size-") {
			t.Fatalf("a size outside the scale: %s", m[0])
		}
	}
	theme, err := staticFS.ReadFile("static/tokens.css")
	must(t, err)
	known := map[string]bool{}
	for _, m := range declared.FindAllStringSubmatch(string(theme), -1) {
		known[m[1]] = true
	}
	for _, m := range used.FindAllStringSubmatch(css, -1) {
		if !known[m[1]] {
			t.Fatalf("%s is not a token", m[1])
		}
	}
}

// A refusal inside the session check is a page like any other error's,
// never the API's JSON.
func TestAStaleFormIsAnsweredWithAPage(t *testing.T) {
	e := site(t)
	c := e.login(t, "admin")
	for what, form := range map[string]url.Values{"no token": {"TIER": {"full"}}, "a stale token": {"TIER": {"full"}, config.CSRFField: {"from-another-login"}}} {
		res, page := postBody(t, c, e.srv.URL+"/jobs/smoke/build", form)
		eq(t, what, res.StatusCode, http.StatusForbidden)
		eq(t, what+" type", res.Header.Get("Content-Type"), "text/html; charset=utf-8")
		contains(t, page, "<html", "<h1>Forbidden</h1>", `role="alert"`, "This form is out of date")
		lacks(t, page, `{"error"`)
		selfContained(t, page)
	}
	builds, err := e.st.Builds(context.Background(), "smoke", 10)
	must(t, err)
	eq(t, "builds triggered", len(builds), 0)
}

// logged collects what the site writes to the default logger.
func logged(t *testing.T) *bytes.Buffer {
	t.Helper()
	var mu sync.Mutex
	buf := &bytes.Buffer{}
	before := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}), nil)))
	t.Cleanup(func() { slog.SetDefault(before) })
	return buf
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestAStoreThatFailsInTheSessionCheckIsThe500PageAndIsLogged(t *testing.T) {
	e := site(t)
	c := e.login(t, "admin")
	log := logged(t)
	must(t, e.st.Close())
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/jobs/smoke", nil)
	res, page := do(t, c, req)
	eq(t, "status", res.StatusCode, http.StatusInternalServerError)
	eq(t, "type", res.Header.Get("Content-Type"), "text/html; charset=utf-8")
	contains(t, page, "<html", "Something went wrong on the server.")
	lacks(t, page, `{"error"`, "sql", "closed")
	contains(t, log.String(), "path=/jobs/smoke", "closed")
}

func TestALoginEndsTheSessionTheBrowserAlreadyHeld(t *testing.T) {
	e := site(t)
	c := e.login(t, "bob")
	u, _ := url.Parse(e.srv.URL)
	first := c.Jar.Cookies(u)[0].Value

	res := post(t, c, e.srv.URL+"/login", url.Values{"name": {"bob"}, "password": {"wrong"}})
	eq(t, "a login that fails", res.StatusCode, http.StatusUnauthorized)
	if _, err := e.st.Session(context.Background(), first); err != nil {
		t.Fatalf("a failed login ended the session held: %v", err)
	}

	res = post(t, c, e.srv.URL+"/login", url.Values{"name": {"admin"}, "password": {"pw"}})
	eq(t, "a second login", res.StatusCode, http.StatusSeeOther)
	second := c.Jar.Cookies(u)[0].Value
	if second == first {
		t.Fatal("the second login kept the first session")
	}
	if _, err := e.st.Session(context.Background(), first); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the first session is still in the store: %v", err)
	}
	code, page := get(t, c, e.srv.URL+"/admin/users/alice")
	eq(t, "the new session works", code, http.StatusOK)
	contains(t, page, "laptop-token")
}

// A status this code does not know is drawn with the failures, where it
// is seen, and not above them.
func TestAnUnknownTestStatusRanksWithFailed(t *testing.T) {
	tests := []store.Test{{Name: "p", Status: "passed"}, {Name: "f", Status: "failed"}, {Name: "s", Status: "skipped"}, {Name: "e", Status: "error"}}
	slices.SortStableFunc(tests, func(a, b store.Test) int { return cmp.Compare(testRank(a.Status), testRank(b.Status)) })
	var order []string
	for _, tc := range tests {
		order = append(order, tc.Name)
	}
	eq(t, "order", strings.Join(order, " "), "f e s p")
}

func TestEveryPageNamesItsIcon(t *testing.T) {
	e := site(t)
	for _, c := range []*http.Client{e.anon(t), e.login(t, "admin")} {
		_, page := get(t, c, e.srv.URL+"/login")
		contains(t, page, `<link rel="icon" href="/static/favicon.svg?v=`, `" type="image/svg+xml">`)
	}
}

// The browser cannot be run here; this holds the script to the two things
// the page relies on. A stream the browser gave up on (readyState CLOSED)
// must be forgotten, or opening the log again would do nothing.
func TestTheLogScriptForgetsAStreamTheBrowserClosed(t *testing.T) {
	data, err := staticFS.ReadFile("static/build.js")
	must(t, err)
	js := string(data)
	_, handler, found := strings.Cut(js, "es.onerror = ")
	if !found {
		t.Fatal("build.js has no onerror handler")
	}
	handler, _, _ = strings.Cut(handler, "};")
	contains(t, handler, "es.readyState === EventSource.CLOSED", "pre.stream = null")
	lacks(t, handler, "pre.dataset.done")
	// While the page still refreshes, an open log with no stream gets one.
	_, refresh, _ := strings.Cut(js, `"htmx:afterSwap"`)
	contains(t, refresh, `document.querySelectorAll("details.log[open]").forEach(follow);`)
}

// The page stops refreshing by the rule the logs end by: the build has
// ended and the dispatcher has let go of it, hook and tests written.
func TestABuildPageRefreshesUntilItsHookIsDoneAndNotAfter(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "gate")
	t.Setenv(gateEnv, gate)
	e := site(t)
	c := e.login(t, "admin")
	id := e.build(t, c, "hooked", nil)
	path := e.srv.URL + e.at(t, id)
	eventually(t, "the build's status is final", func() bool {
		b, err := e.st.Build(context.Background(), id)
		return err == nil && b.Status == store.Success
	})
	_, page := get(t, c, path)
	contains(t, page, `class="status success"`, `hx-trigger="every 2s"`)
	lacks(t, page, "/cancel")

	must(t, os.WriteFile(gate, nil, 0o600))
	e.wait(t, id)
	_, page = get(t, c, path)
	contains(t, page, `class="status success"`, `id="log-hooks"`)
	// The header's queue link follows the queue; the build's part of the page is still.
	lacks(t, page, `hx-select="#live"`, `hx-trigger="every 2s"`)
}

func TestAStaleJobSetIsToldToEveryoneLoggedInAndExplainedToAdmins(t *testing.T) {
	e := site(t)
	admin, bob := e.login(t, "admin"), e.login(t, "bob")
	_, page := get(t, bob, e.srv.URL+"/")
	lacks(t, page, `role="alert"`)

	e.reloadErr.Store(new("slow.yml: stage s: run is empty"))
	_, page = get(t, admin, e.srv.URL+"/")
	contains(t, page, `role="alert"`, "slow.yml: stage s: run is empty")
	for _, path := range []string{"/", "/jobs/smoke", "/nowhere"} {
		_, page = get(t, bob, e.srv.URL+path)
		contains(t, page, `<p class="notice notice-error" role="alert">`+api.StaleJobs+`</p>`)
		lacks(t, page, "slow.yml", "run is empty")
	}
	_, page = get(t, e.anon(t), e.srv.URL+"/login")
	lacks(t, page, `role="alert"`, "reload")
}

// From a build the way back is its job, and from a job its group: the
// page's heading is that trail, ending with the page itself.
func TestAJobAndABuildSayWhereTheyAreByGroupThenJob(t *testing.T) {
	e := site(t)
	c := e.login(t, "admin")
	const sep = `<span aria-hidden="true">/</span>`
	_, page := get(t, c, e.srv.URL+"/jobs/smoke")
	contains(t, page, `<h1 class="crumbs"><a class="link-plain" href="/jobs?group=backend">backend</a>`+sep+`<span>smoke</span></h1>`)
	lacks(t, page, `<h1>smoke</h1>`, `aria-label="Breadcrumb"`)
	_, page = get(t, c, e.srv.URL+"/jobs/slow")
	contains(t, page, `<h1 class="crumbs"><a class="link-plain" href="/jobs?only=ungrouped">No group</a>`+sep+`<span>slow</span></h1>`)

	id := e.build(t, c, "smoke", nil)
	e.wait(t, id)
	_, page = get(t, c, e.srv.URL+e.at(t, id))
	contains(t, page, `<h1 class="crumbs"><a class="link-plain" href="/jobs?group=backend">backend</a>`+sep+`<a class="link-plain" href="/jobs/smoke">smoke</a>`+sep+`<span>#1</span> <span class="status success">success</span></h1>`)

	e.drop("smoke")
	_, page = get(t, c, e.srv.URL+e.at(t, id))
	contains(t, page, `<h1 class="crumbs"><a class="link-plain" href="/jobs?only=removed">Removed</a>`+sep+`<a class="link-plain" href="/jobs/smoke">smoke</a>`+sep+`<span>#1</span>`)
	_, page = get(t, c, e.srv.URL+"/jobs/smoke")
	contains(t, page, `<h1 class="crumbs"><a class="link-plain" href="/jobs?only=removed">Removed</a>`+sep+`<span>smoke</span></h1>`)
}

// When a job builds by itself is said apart from the prose around it.
func TestAJobsScheduleAndNextBuildStandOut(t *testing.T) {
	e := site(t)
	_, page := get(t, e.login(t, "admin"), e.srv.URL+"/jobs/nightly")
	contains(t, page, `<dl class="facts sched">`,
		`<div><dt>Runs on</dt><dd><code>0 3 * * *</code></dd></div>`,
		`<div><dt>Time zone</dt><dd>`,
		`<div><dt>Next build</dt><dd>`)
	lacks(t, page, "next at")
	data, err := staticFS.ReadFile("static/kipp.css")
	must(t, err)
	contains(t, rule(t, string(data), ".sched dd"), "background-color: var(--accent-soft);")
}

// A job's page lists the variables the job files give it, with where each
// came from, and puts the job file's env over the ones _vars.yml gives.
func TestAJobsPageListsTheVariablesTheFilesGiveItAndWhereFrom(t *testing.T) {
	e := site(t)
	_, page := get(t, e.login(t, "admin"), e.srv.URL+"/jobs/ask")
	contains(t, page, `<h2>Variables</h2>`,
		`<tr><td class="lead" data-label="Name"><code>BANNER</code></td><td data-label="Value"><code>&lt;b&gt;hi&lt;/b&gt;</code></td><td data-label="From">_vars.yml jobs ask</td></tr>`,
		`<tr><td class="lead" data-label="Name"><code>REGION</code></td><td data-label="Value"><code>eu</code></td><td data-label="From">_vars.yml global</td></tr>`)
	lacks(t, page, "<b>hi</b>")
}

// The Build button is in the heading's row. A job with params opens its
// form from there; one without builds on the click.
func TestTheBuildButtonSitsInTheHeadingsRow(t *testing.T) {
	e := site(t)
	c := e.login(t, "admin")
	_, page := get(t, c, e.srv.URL+"/jobs/hooked")
	contains(t, page, `<div class="head">`+"\n"+`<h1 class="crumbs">`,
		`</h1>`+"\n"+`<form class="build" method="post" action="/jobs/hooked/build">`)
	lacks(t, page, `<details class="build drop">`)

	_, page = get(t, c, e.srv.URL+"/jobs/ask")
	contains(t, page, `</h1>`+"\n"+`<details class="build drop">`+"\n"+`<summary class="action action-main">Build</summary>`,
		`<form class="panel stack pop" method="post" action="/jobs/ask/build">`,
		`<button class="action action-main">Start the build</button>`)

	// The build's own page has its Cancel in the same place.
	id := e.build(t, c, "slow", nil)
	_, page = get(t, c, e.srv.URL+e.at(t, id))
	contains(t, page, `</p>`+"\n"+`</div>`+"\n"+`<a class="build action action-small" href="/jobs/slow/1/cancel">Cancel...</a>`)
	post(t, c, e.srv.URL+"/jobs/slow/1/cancel", url.Values{"csrf": {csrfOf(t, page)}})
	e.wait(t, id)
}

func TestAParamsDescriptionStandsUnderItsLabelOnTheBuildForm(t *testing.T) {
	e := site(t)
	c := e.login(t, "admin")
	_, page := get(t, c, e.srv.URL+"/jobs/ask")
	contains(t, page, "<label class=\"entry-label\" for=\"param-WHO\">WHO</label>\n<p class=\"entry-help\" id=\"hint-WHO\">Who to greet, &lt;one&gt; name.\nEmpty is world.</p>",
		`<input class="entry" id="param-WHO" name="WHO" value="world" aria-describedby="hint-WHO">`)
	if strings.Contains(page, "hint-REQUEST") {
		t.Fatal("a param with no description has a hint")
	}
}

func TestATextParamIsATextareaAndItsLinesArriveAsTheyWereTyped(t *testing.T) {
	e := site(t)
	c := e.login(t, "admin")
	_, page := get(t, c, e.srv.URL+"/jobs/ask")
	// The newline after the tag is the one a browser drops, so the
	// default's own first newline stays.
	contains(t, page, "<textarea class=\"entry\" id=\"param-REQUEST\" name=\"REQUEST\" rows=\"8\">\n\nline &lt;one&gt;\nline two</textarea>", `<input class="entry" id="param-WHO" name="WHO" value="world" aria-describedby="hint-WHO">`)

	// A browser sends every line break of a form as CR LF.
	id := e.build(t, c, "ask", url.Values{"REQUEST": {"\r\nfirst\r\n  second\r\n\r\nlast\r\n"}})
	b, err := e.st.Build(context.Background(), id)
	must(t, err)
	eq(t, "the text", b.Params["REQUEST"], "\nfirst\n  second\n\nlast\n")
	// On the build's page a value of several lines keeps them, the first
	// too when it is empty: a browser drops one newline after the tag.
	e.wait(t, id)
	_, page = get(t, c, e.srv.URL+e.at(t, id))
	contains(t, page, "<code>REQUEST</code><pre>\n\nfirst\n  second\n\nlast\n</pre>", "<code>WHO=world</code>")

	res, page := postBody(t, c, e.srv.URL+"/jobs/ask/build", url.Values{config.CSRFField: {e.token(t, c)}, "WHO": {"two\r\nlines"}})
	eq(t, "a string of two lines", res.StatusCode, http.StatusBadRequest)
	contains(t, page, "one line")
}

// drop takes a job out of the job set, as a reload after its file was removed does.
func (e *env) drop(name string) {
	snap := e.d.Snapshot()
	jobs := maps.Clone(snap.Jobs)
	delete(jobs, name)
	e.d.SetSnapshot(&config.Snapshot{Jobs: jobs, Names: slices.DeleteFunc(slices.Clone(snap.Names), func(n string) bool { return n == name })})
}

func TestARemovedJobKeepsItsHistoryOnThePagesAndNothingToBuild(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	e.wait(t, e.build(t, admin, "smoke", nil))
	e.drop("smoke")
	e.drop("ask") // never built: gone altogether

	for _, name := range []string{"admin", "alice"} {
		c := e.login(t, name)
		code, page := get(t, c, e.srv.URL+"/jobs/smoke")
		eq(t, name+": the removed job's page", code, http.StatusOK)
		contains(t, page, "removed from the job files", `href="/jobs/smoke/1"`)
		lacks(t, page, `action="/jobs/smoke/build"`, `class="facts sched"`)
		code, page = get(t, c, e.srv.URL+"/jobs")
		eq(t, name+": the jobs", code, http.StatusOK)
		contains(t, page, "Removed from the job files", `href="/jobs/smoke"`)
		lacks(t, page, `href="/jobs/ask"`)
		code, page = get(t, c, e.srv.URL+"/jobs/smoke/1")
		eq(t, name+": the build of the removed job", code, http.StatusOK)
		contains(t, page, `href="/jobs/smoke"`)
	}
	res, _ := postBody(t, admin, e.srv.URL+"/jobs/smoke/build", url.Values{"csrf": {e.token(t, admin)}})
	eq(t, "building a removed job", res.StatusCode, http.StatusNotFound)

	// bob has no grant on nightly: removed or not, it does not exist for him.
	e.wait(t, e.build(t, admin, "nightly", nil))
	e.drop("nightly")
	bob := e.login(t, "bob")
	codeHidden, pageHidden := get(t, bob, e.srv.URL+"/jobs/nightly")
	_, pageMissing := get(t, bob, e.srv.URL+"/jobs/none")
	eq(t, "a removed job one may not view", codeHidden, http.StatusNotFound)
	eq(t, "the page of a removed job one may not view", pageHidden, pageMissing)
	_, page := get(t, bob, e.srv.URL+"/")
	lacks(t, page, "nightly")
}

func TestALoginThatFindsTheServerBusyIsToldToComeBackAndIsNotAWrongPassword(t *testing.T) {
	e := site(t)
	e.site.signIn = func(context.Context, string, string, auth.Attempt) (*store.Session, string, error) {
		return nil, "", auth.ErrBusy
	}
	res, page := postBody(t, e.anon(t), e.srv.URL+"/login", url.Values{"name": {"admin"}, "password": {"pw"}, "next": {"/jobs/smoke"}})
	eq(t, "a login with no slot", res.StatusCode, http.StatusServiceUnavailable)
	eq(t, "Retry-After", res.Header.Get("Retry-After"), "2")
	contains(t, page, "Too many logins at once", "try again", `name="password"`, `name="next" value="/jobs/smoke"`)
	lacks(t, page, "Wrong name or password")
	eq(t, "no cookie", len(res.Cookies()), 0)
}

func TestAThrottledLoginIs429AndSaysWhenToTryAgain(t *testing.T) {
	e := site(t)
	for _, c := range []struct {
		wait         time.Duration
		after, words string
	}{
		{2 * time.Second, "2", "Try again in 2 seconds."},
		{1500 * time.Millisecond, "2", "Try again in 2 seconds."},
		{time.Second, "1", "Try again in 1 second."},
		{64 * time.Second, "64", "Try again in 2 minutes."},
		{15 * time.Minute, "900", "Try again in 15 minutes."},
	} {
		e.site.signIn = func(context.Context, string, string, auth.Attempt) (*store.Session, string, error) {
			return nil, "", &auth.ThrottledError{RetryAfter: c.wait}
		}
		res, page := postBody(t, e.anon(t), e.srv.URL+"/login", url.Values{"name": {"admin"}, "password": {"pw"}, "next": {"/jobs/smoke"}})
		eq(t, "a throttled login", res.StatusCode, http.StatusTooManyRequests)
		eq(t, "Retry-After", res.Header.Get("Retry-After"), c.after)
		contains(t, page, "Too many wrong passwords for this name.", c.words, `name="password"`, `name="next" value="/jobs/smoke"`)
		lacks(t, page, "Wrong name or password")
		eq(t, "no cookie", len(res.Cookies()), 0)
	}
}

// The browser a user has logged in with keeps logging in while strangers
// have run the name's shared count out; a browser that never logged in as
// the name waits.
func TestTheBrowserThatLoggedInBeforeStillLogsInWhenTheNameIsThrottled(t *testing.T) {
	e := site(t)
	owner := e.login(t, "admin")
	u, err := url.Parse(e.srv.URL)
	must(t, err)
	device := cookieNamed(t, owner, u, api.DeviceCookie)
	stranger := e.anon(t)
	for range 5 {
		eq(t, "a stranger's wrong password", post(t, stranger, e.srv.URL+"/login", url.Values{"name": {"admin"}, "password": {"wrong"}}).StatusCode, http.StatusUnauthorized)
	}
	res := post(t, stranger, e.srv.URL+"/login", url.Values{"name": {"admin"}, "password": {"pw"}})
	eq(t, "the right password from a stranger, throttled", res.StatusCode, http.StatusTooManyRequests)
	// The name's wait is two seconds from the last wrong password, and the
	// hashing of the five before it can take one of them on a loaded machine.
	if got := res.Header.Get("Retry-After"); got != "2" && got != "1" {
		t.Fatalf("Retry-After %q, want 2 or 1", got)
	}
	// A device cookie made for another user lets nobody past.
	bob := e.login(t, "bob")
	eq(t, "bob's browser, as admin", post(t, bob, e.srv.URL+"/login", url.Values{"name": {"admin"}, "password": {"pw"}}).StatusCode, http.StatusTooManyRequests)

	res = post(t, owner, e.srv.URL+"/login", url.Values{"name": {"admin"}, "password": {"pw"}})
	eq(t, "the owner's own browser", res.StatusCode, http.StatusSeeOther)
	for _, c := range res.Cookies() {
		if c.Name == api.DeviceCookie && c.Value != device.Value {
			t.Fatal("a device cookie that was still good was replaced")
		}
	}
}

func TestEveryLogOnABuildPageHasItsDownloadAndAPlaceToSayItIsNotAllShown(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	id := strconv.FormatInt(e.wait(t, e.build(t, admin, "smoke", nil)).ID, 10)
	_, page := get(t, admin, e.srv.URL+"/jobs/smoke/1")
	for _, stage := range []string{"lint", "unit", "smoke"} {
		contains(t, page, `href="/api/v1/builds/`+id+`/stages/`+stage+`/log?download=true"`)
	}
	eq(t, "places to say a log is cut", strings.Count(page, "data-cut hidden"), 3)
	// The link is the session's: a browser that follows it gets the file.
	res, body := do(t, admin, mustGet(t, e.srv.URL+"/api/v1/builds/"+id+"/stages/lint/log?download=true"))
	eq(t, "the download", res.StatusCode, http.StatusOK)
	eq(t, "the download's body", body, "lint\n")
	contains(t, res.Header.Get("Content-Disposition"), `filename="smoke-1-lint.log"`)

	js, err := staticFS.ReadFile("static/build.js")
	must(t, err)
	contains(t, string(js), `fetch(pre.dataset.log, { method: "HEAD" })`, `"X-Kipp-Size"`, "const TAIL = 1 << 20;", "const MAX_LINES = 5000;")
}

func mustGet(t *testing.T, u string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, u, nil)
	must(t, err)
	return req
}

func TestADisabledJobIsShownAsDisabledWithNothingToBuild(t *testing.T) {
	e := site(t)
	snap := e.d.Snapshot()
	jobs := maps.Clone(snap.Jobs)
	off := *jobs["nightly"]
	off.Disabled = true
	jobs["nightly"] = &off
	e.d.SetSnapshot(&config.Snapshot{Jobs: jobs, Names: snap.Names})

	admin := e.login(t, "admin")
	code, page := get(t, admin, e.srv.URL+"/jobs/nightly")
	eq(t, "the disabled job's page", code, http.StatusOK)
	contains(t, page, "This job is disabled")
	lacks(t, page, `action="/jobs/nightly/build"`, `class="facts sched"`)
	_, page = get(t, admin, e.srv.URL+"/jobs")
	eq(t, "jobs marked disabled in the list", strings.Count(page, `<span class="tag">disabled</span>`), 1)
	res, page := postBody(t, admin, e.srv.URL+"/jobs/nightly/build", url.Values{"csrf": {e.token(t, admin)}})
	eq(t, "building a disabled job", res.StatusCode, http.StatusConflict)
	contains(t, page, "the job is disabled")
}

func TestABuildStartedFromTheFormIsRecordedAsTheWebsAndShownSo(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	b := e.wait(t, e.build(t, admin, "smoke", nil))
	eq(t, "the trigger of a build from the form", b.Trigger, "web")
	_, page := get(t, admin, e.srv.URL+"/jobs/smoke")
	contains(t, page, `<td data-label="Trigger">web <span class="sub">by admin</span></td>`)
	_, page = get(t, admin, e.srv.URL+e.at(t, b.ID))
	contains(t, page, `<p class="meta"><span>web by admin</span>`)
}

func TestAnUnstableBuildAndStageAreShownAsUnstableOnEveryPage(t *testing.T) {
	e := site(t)
	c := e.login(t, "admin")
	id := e.build(t, c, "shaky", nil)
	path := e.at(t, id)
	eq(t, "status", e.wait(t, id).Status, store.Unstable)

	_, page := get(t, c, e.srv.URL+path)
	contains(t, page,
		`<span>#1</span> <span class="status unstable">unstable</span></h1>`,
		`<g class="stage unstable" data-stage="lint">`, `<g class="stage success" data-stage="unit">`,
		"<td class=\"lead\" data-label=\"Stage\"><a class=\"link\" href=\"#log-lint\">lint</a></td>\n<td class=\"lead\" data-label=\"Status\"><span class=\"status unstable\">unstable</span></td>\n<td data-label=\"Exit\">4</td>",
		"<td class=\"lead\" data-label=\"Stage\"><a class=\"link\" href=\"#log-unit\">unit</a></td>\n<td class=\"lead\" data-label=\"Status\"><span class=\"status success\">success</span></td>",
		// What made the build unstable is the log a reader came for.
		`<details class="log" id="log-lint" open>`, `<details class="log" id="log-unit">`,
		// Nothing failed: the tests are a count, folded, and none is listed.
		`<details class="results" id="results">`, `<span class="status success">1 passed</span>`, "Show all 1</a>",
	)
	// The build is over: nothing to cancel and nothing more to wait for.
	// (The one hx-get left is the link that loads the tests, on a click.)
	lacks(t, page, `href="`+path+`/cancel"`, `hx-trigger="every 2s"`, `hx-select="#live"`)

	_, page = get(t, c, e.srv.URL+"/jobs")
	contains(t, page, `<a class="link" href="`+path+`">#1</a> <span class="status unstable">unstable</span>`)

	_, page = get(t, c, e.srv.URL+"/jobs/shaky?results=all")
	contains(t, page,
		`<td class="lead" data-label="Build"><a class="link" href="`+path+`">#1</a></td>`+"\n"+`<td class="lead" data-label="Status"><span class="status unstable">unstable</span></td>`,
		// A dot is a test's own outcome, whatever its build's was.
		`<span class="dot passed" title="#1 passed"></span>`,
	)
	lacks(t, page, "dot unstable")
}

// rule is the declarations of the one rule of the stylesheet whose
// selector list names the selector.
func rule(t *testing.T, css, selector string) string {
	t.Helper()
	for _, m := range regexp.MustCompile(`(?m)^\s*([^{}\n]+)\{([^}]*)\}`).FindAllStringSubmatch(css, -1) {
		for sel := range strings.SplitSeq(m[1], ",") {
			if strings.TrimSpace(sel) == selector {
				return m[2]
			}
		}
	}
	t.Fatalf("kipp.css has no rule for %s", selector)
	return ""
}

func TestUnstableIsPaintedInTheWarningRole(t *testing.T) {
	data, err := staticFS.ReadFile("static/kipp.css")
	must(t, err)
	css := string(data)
	contains(t, rule(t, css, ".status.unstable"),
		"background-color: var(--warn-soft);", "color: var(--on-warn-soft);", "border-color: var(--warn);")
	contains(t, rule(t, css, ".dag .unstable rect"), "fill: var(--warn-soft);", "stroke: var(--warn);")
	// The roles its neighbours are painted in stay theirs.
	contains(t, rule(t, css, ".status.success"), "var(--ok)")
	contains(t, rule(t, css, ".status.failed"), "var(--bad)")
	contains(t, rule(t, css, ".dag .failed rect"), "var(--bad)")
}

func TestDAGSVGMarksAnUnstableStage(t *testing.T) {
	stages := []*store.Stage{{Name: "lint", Status: store.StageUnstable}, {Name: "unit", Status: store.StageSuccess}}
	svg := drawn(t, stages, map[string][]string{"lint": nil, "unit": {"lint"}})
	contains(t, svg, `<g class="stage unstable" data-stage="lint">`, `<g class="stage success" data-stage="unit">`)
	eq(t, "edges", strings.Count(svg, "<line"), 1)
}

// A node's colour is not the only thing that says how its stage ended: the
// title every node carries says it in a word.
func TestEveryDAGNodeSaysItsStatusInItsTitle(t *testing.T) {
	statuses := []store.StageStatus{store.StagePending, store.StageRunning, store.StageSuccess, store.StageUnstable, store.StageFailed, store.StageSkipped, store.StageCancelled}
	var stages []*store.Stage
	for _, st := range statuses {
		stages = append(stages, &store.Stage{Name: "s-" + string(st), Status: st})
	}
	svg := drawn(t, stages, nil)
	for _, st := range statuses {
		contains(t, svg, `<g class="stage `+string(st)+`" data-stage="s-`+string(st)+`"><title>s-`+string(st)+`: `+string(st)+`</title>`)
	}
}

// An unstable node differs from its neighbours by more than its colour,
// and by another mark than the dashes of a stage that did not run.
func TestAnUnstableDAGNodeIsMarkedByMoreThanItsColour(t *testing.T) {
	data, err := staticFS.ReadFile("static/kipp.css")
	must(t, err)
	css := string(data)
	width := regexp.MustCompile(`stroke-width:\s*([^;]+);`)
	plain, unstable := width.FindStringSubmatch(rule(t, css, ".dag rect")), width.FindStringSubmatch(rule(t, css, ".dag .unstable rect"))
	if plain == nil || unstable == nil || plain[1] == unstable[1] {
		t.Fatalf("stroke width of a node %v, of an unstable one %v: they must differ", plain, unstable)
	}
	lacks(t, rule(t, css, ".dag .unstable rect"), "dasharray")
	for _, other := range []string{".dag .success rect", ".dag .failed rect", ".dag .running rect", ".dag .skipped rect"} {
		lacks(t, rule(t, css, other), "stroke-width")
	}
}

// A stage that starts and ends unstable between two refreshes of the page
// has its log opened by the script, as one that failed has: the summary
// names it among the logs worth reading, and the script opens those.
func TestTheLogScriptOpensTheLogOfAStageThatEndedUnstable(t *testing.T) {
	data, err := staticFS.ReadFile("static/build.js")
	must(t, err)
	_, refresh, _ := strings.Cut(string(data), `"htmx:afterSwap"`)
	contains(t, refresh, `live.dataset.open`)

	e := site(t)
	c := e.login(t, "admin")
	id := e.build(t, c, "shaky", nil)
	eq(t, "status", e.wait(t, id).Status, store.Unstable)
	_, page := get(t, c, e.srv.URL+e.at(t, id))
	contains(t, page, `data-open="lint "`)
}

func TestAJobsPageLeadsToItsOlderBuildsAPageAtATime(t *testing.T) {
	e := site(t)
	c := e.login(t, "admin")
	for range 52 {
		must(t, e.st.CreateBuild(context.Background(), &store.Build{Job: "smoke", Status: store.Success, Trigger: "cli"}))
	}
	_, page := get(t, c, e.srv.URL+"/jobs/smoke")
	eq(t, "builds on the first page", strings.Count(page, `<td class="lead" data-label="Build"><a class="link" href="/jobs/smoke/`), 50)
	contains(t, page, ">#52</a>", ">#3</a>", `<a class="link" href="/jobs/smoke?before=3">Older builds</a>`)
	lacks(t, page, ">#2</a>", "Newest builds")
	code, page := get(t, c, e.srv.URL+"/jobs/smoke?before=3")
	eq(t, "older page", code, http.StatusOK)
	contains(t, page, ">#2</a>", ">#1</a>", `<a class="link" href="/jobs/smoke">Newest builds</a>`)
	lacks(t, page, ">#3</a>", "Older builds")
	code, _ = get(t, c, e.srv.URL+"/jobs/smoke?before=x")
	eq(t, "a before that is no number", code, http.StatusBadRequest)
}

func TestABrowserOnTheServersMachineLogsInOverPlainHTTPAndAProxiedOneNeverByThatCookie(t *testing.T) {
	e := site(t)
	plain := httptest.NewServer(e.srv.Config.Handler)
	t.Cleanup(plain.Close)
	c := &http.Client{Timeout: waitfor.Safety, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	login := func(hdr ...string) []*http.Cookie {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, plain.URL+"/login", strings.NewReader(url.Values{"name": {"admin"}, "password": {"pw"}}.Encode()))
		must(t, err)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", plain.URL) // as a browser showing the form at this address sends it
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		res, _ := do(t, c, req)
		eq(t, "login", res.StatusCode, http.StatusSeeOther)
		return res.Cookies()
	}
	page := func(cookie *http.Cookie, hdr ...string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, plain.URL+"/", nil)
		must(t, err)
		req.AddCookie(cookie)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		res, _ := do(t, c, req)
		return res
	}

	cookies := login()
	local := cookies[0]
	if local.Name != api.LocalSessionCookie || local.Secure || !local.HttpOnly {
		t.Fatalf("a login straight from this machine got %+v, want the cookie a browser keeps over plain HTTP", local)
	}
	eq(t, "the page, straight from this machine", page(local).StatusCode, http.StatusOK)
	// The same cookie through a proxy is nobody's: the reader is sent to log in.
	eq(t, "the page, through a proxy", page(local, "X-Forwarded-Proto", "https").StatusCode, http.StatusSeeOther)

	device := cookies[len(cookies)-1]
	if device.Name != api.LocalDeviceCookie || device.Secure || !device.HttpOnly {
		t.Fatalf("a login straight from this machine got the device cookie %+v", device)
	}

	// A login that came through a proxy gets the Secure cookies and no other.
	cookies = login("X-Forwarded-Proto", "https")
	if len(cookies) != 2 || cookies[0].Name != api.SessionCookie || !cookies[0].Secure ||
		cookies[1].Name != api.DeviceCookie || !cookies[1].Secure {
		t.Fatalf("a proxied login got %+v", cookies)
	}

	// Logging out ends the session and both cookies.
	csrf := csrfOf(t, func() string {
		req, _ := http.NewRequest(http.MethodGet, plain.URL+"/", nil)
		req.AddCookie(local)
		_, body := do(t, c, req)
		return body
	}())
	req, err := http.NewRequest(http.MethodPost, plain.URL+"/logout", strings.NewReader(url.Values{"csrf": {csrf}}.Encode()))
	must(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", plain.URL)
	req.AddCookie(local)
	res, _ := do(t, c, req)
	eq(t, "logout", res.StatusCode, http.StatusSeeOther)
	var ended []string
	for _, gone := range res.Cookies() {
		if gone.MaxAge < 0 {
			ended = append(ended, gone.Name)
		}
	}
	slices.Sort(ended)
	eq(t, "cookies ended", strings.Join(ended, " "), api.SessionCookie+" "+api.LocalSessionCookie)
	eq(t, "the page after logout", page(local).StatusCode, http.StatusSeeOther)
}

// A build is read by its job and its number there, the two things its page
// shows. The id is the database's, and an address that was handed out with
// it still arrives.
func TestABuildsPageIsUnderItsJobByNumberAndTheOldAddressLeadsThere(t *testing.T) {
	e := site(t)
	c := e.login(t, "admin")
	e.wait(t, e.build(t, c, "nightly", nil))
	form := url.Values{"csrf": {e.token(t, c)}, "TIER": {"full"}}
	res := post(t, c, e.srv.URL+"/jobs/smoke/build", form)
	eq(t, "a build's own address", res.Header.Get("Location"), "/jobs/smoke/1")
	e.wait(t, 2)

	code, page := get(t, c, e.srv.URL+"/jobs/smoke/1")
	eq(t, "by job and number", code, http.StatusOK)
	contains(t, page, "TIER=full", `data-log="/api/v1/builds/2/stages/lint/log"`)
	lacks(t, page, `"/builds/`)

	req := mustGet(t, e.srv.URL+"/builds/2?results=all")
	res, _ = do(t, c, req)
	eq(t, "the old address", res.StatusCode, http.StatusMovedPermanently)
	eq(t, "the old address leads to", res.Header.Get("Location"), "/jobs/smoke/1?results=all")

	for _, path := range []string{"/jobs/smoke/2", "/jobs/smoke/0", "/jobs/smoke/x", "/jobs/smoke/1/extra", "/jobs/none/1"} {
		code, _ := get(t, c, e.srv.URL+path)
		eq(t, path, code, http.StatusNotFound)
	}
}

func TestABuildIsCancelledAtItsOwnAddress(t *testing.T) {
	e := site(t)
	c := e.login(t, "admin")
	id := e.build(t, c, "slow", nil)
	_, page := get(t, c, e.srv.URL+"/jobs/slow/1")
	contains(t, page, `href="/jobs/slow/1/cancel"`)
	res := post(t, c, e.srv.URL+"/jobs/slow/1/cancel", url.Values{"csrf": {csrfOf(t, page)}})
	eq(t, "cancel", res.StatusCode, http.StatusSeeOther)
	eq(t, "cancel leads back", res.Header.Get("Location"), "/jobs/slow/1")
	if b := e.wait(t, id); b.Status != store.Cancelled {
		t.Fatalf("build: %s", b.Status)
	}
}

// A job that names its zone says so, and its next build is shown in it,
// with the zone's name beside the time.
func TestAJobsNextBuildIsShownInTheZoneItsCronIsReadIn(t *testing.T) {
	e := site(t)
	jobs := t.TempDir()
	must(t, os.WriteFile(filepath.Join(jobs, "tokyo.yml"), []byte("name: tokyo\ntimezone: Asia/Tokyo\ntriggers: {cron: '0 9 * * *'}\nstages:\n  s: {run: 'true'}\n"), 0o600))
	snap, err := config.Load(jobs)
	must(t, err)
	e.d.SetSnapshot(snap)
	_, page := get(t, e.login(t, "admin"), e.srv.URL+"/jobs/tokyo")
	contains(t, page, `<div><dt>Time zone</dt><dd>Asia/Tokyo</dd></div>`, ` 09:00:00 JST</dd></div>`)
}

func TestThePagesShowTimesInTheServersZone(t *testing.T) {
	at := time.Date(2026, 10, 11, 2, 0, 0, 0, time.UTC)
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	must(t, err)
	eq(t, "a time", clockIn(tokyo)(at), "2026-10-11 11:00:00")
	eq(t, "a pointer to one", clockIn(tokyo)(&at), "2026-10-11 11:00:00")
	eq(t, "none", clockIn(tokyo)((*time.Time)(nil)), "")
}

// lockJobs adds two jobs that share a lock: hold keeps it for 30s, and
// wait needs it.
func (e *env) lockJobs(t *testing.T) {
	t.Helper()
	jobs := t.TempDir()
	for name, body := range jobFiles {
		must(t, os.WriteFile(filepath.Join(jobs, name), []byte(body), 0o600))
	}
	must(t, os.WriteFile(filepath.Join(jobs, "hold.yml"), []byte("name: hold\nlocks: [k]\nstages:\n  s: {run: 'sleep 30'}\n"), 0o600))
	must(t, os.WriteFile(filepath.Join(jobs, "wait.yml"), []byte("name: wait\nlocks: [k]\nstages:\n  s: {run: 'true'}\n"), 0o600))
	snap, err := config.Load(jobs)
	must(t, err)
	e.d.SetSnapshot(snap)
}

// queued waits until the dispatcher has n builds running and m queued.
func (e *env) queued(t *testing.T, running, queued int) {
	t.Helper()
	timeout := time.After(waitfor.Safety)
	for {
		changes := e.d.Changes()
		q, err := e.d.Queue(context.Background())
		must(t, err)
		if len(q.Running) == running && len(q.Queued) == queued && (running == 0 || len(q.Running[0].Stages) > 0) {
			return
		}
		select {
		case <-changes:
		case <-timeout:
			t.Fatalf("never %d running and %d queued: %+v", running, queued, q)
		}
	}
}

func TestTheQueuePageSaysWhatRunsAndWhyTheRestWaits(t *testing.T) {
	e := site(t)
	e.lockJobs(t)
	c := e.login(t, "admin")
	hold := e.build(t, c, "hold", nil)
	e.queued(t, 1, 0)
	e.build(t, c, "wait", nil)
	e.queued(t, 1, 1)
	_, page := get(t, c, e.srv.URL+"/queue")
	contains(t, page, `<section id="queue" class="stack" hx-get="/queue" hx-select="#queue" hx-target="this" hx-swap="outerHTML" hx-trigger="kipp-changed from:body">`,
		`1 of 4 build slots busy`,
		`<a class="link" href="/jobs/hold/1">hold #1</a>`,
		`<span class="status running">running</span> <span class="sub">s</span>`,
		`<time class="since" datetime="`,
		`<span class="status queued">queued</span> <span class="sub">waiting for lock <code>k</code>, held by <a class="link" href="/jobs/hold/1">hold #1</a></span>`,
		`href="/jobs/wait/1/cancel?next=`, `href="/jobs/hold/1/cancel?next=`,
		`<script src="/static/queue.js?v=`)
	contains(t, page, `id="queue-badge"`,
		`aria-label="1 running, 1 queued" title="1 running, 1 queued"`,
		`<span class="qb qb-running"><svg class="icon" viewBox="0 0 24 24" aria-hidden="true">`,
		`</svg>1</span><span class="qb qb-queued"><svg class="icon" viewBox="0 0 24 24" aria-hidden="true">`,
		`</svg>1</span></a>`)
	lacks(t, page, `1 running, 1 queued</a>`)

	// bob may view wait and nothing of hold: the lock is named, its holder is not.
	ctx := context.Background()
	bob, err := e.st.UserByName(ctx, "bob")
	must(t, err)
	must(t, e.st.Grant(ctx, bob.ID, "wait", []string{"view"}))
	_, page = get(t, e.login(t, "bob"), e.srv.URL+"/queue")
	contains(t, page, `waiting for lock <code>k</code>, held by another build`,
		`aria-label="0 running, 1 queued"`, `<span class="qb qb-queued">`)
	lacks(t, page, `qb-running`, "hold", `/cancel"`, "executors")

	must(t, e.d.Cancel(ctx, hold, "test"))
	e.queued(t, 0, 0)
	_, page = get(t, c, e.srv.URL+"/queue")
	contains(t, page, `Nothing runs and nothing waits.`, `>idle</a>`)
}

func TestTheJobListShowsTheQueueAboveItAndBothFollowItsEvents(t *testing.T) {
	e := site(t)
	e.lockJobs(t)
	c := e.login(t, "admin")
	_, page := get(t, c, e.srv.URL+"/jobs")
	contains(t, page, `<section id="queue"`, `hx-trigger="kipp-changed from:body, every 60s"`)
	lacks(t, page, `every 10s`, "Nothing runs and nothing waits.")
	if strings.Index(page, `id="queue"`) > strings.Index(page, `id="jobs"`) {
		t.Fatal("the queue is below the jobs")
	}
	hold := e.build(t, c, "hold", nil)
	e.queued(t, 1, 0)
	_, page = get(t, c, e.srv.URL+"/jobs")
	contains(t, page, `hold #1`)
	must(t, e.d.Cancel(context.Background(), hold, "test"))
}

func TestTheQueueLinkIsInEveryPagesHeader(t *testing.T) {
	e := site(t)
	c := e.login(t, "admin")
	for _, path := range []string{"/", "/jobs/smoke", "/admin/users", "/account"} {
		_, page := get(t, c, e.srv.URL+path)
		contains(t, page, `<a id="queue-badge" class="link-plain queue-badge idle" href="/queue" hx-get="/queue" hx-select="#queue-badge" hx-target="this" hx-swap="outerHTML" hx-trigger="kipp-changed from:body">idle</a>`)
	}
	_, page := get(t, e.anon(t), e.srv.URL+"/login")
	lacks(t, page, "queue-badge", "queue.js")
}

// drawn is the stages as a build's page draws them.
func drawn(t *testing.T, stages []*store.Stage, needs map[string][]string) string {
	t.Helper()
	var out bytes.Buffer
	must(t, parsePages(loadAssets(), time.UTC)["build"].ExecuteTemplate(&out, "dag", dagOf(stages, needs)))
	return out.String()
}

// A build's heading is one line of facts under its name. A build of one
// stage has no drawing and no table of its stages, which would only say
// again what the line says; the line gives the stage's exit code.
func TestABuildOfOneStageIsHeadedByOneLineOfFacts(t *testing.T) {
	e := site(t)
	jobs := t.TempDir()
	must(t, os.WriteFile(filepath.Join(jobs, "solo.yml"), []byte("name: solo\nstages:\n  s: {run: 'echo out; exit 3'}\n"), 0o600))
	snap, err := config.Load(jobs)
	must(t, err)
	e.d.SetSnapshot(snap)
	c := e.login(t, "admin")
	id := e.build(t, c, "solo", nil)
	eq(t, "status", e.wait(t, id).Status, store.Failed)
	_, page := get(t, c, e.srv.URL+e.at(t, id))
	contains(t, page, `<p class="meta"><span>web by admin</span><span>started `, "<span>exit 3</span></p>",
		`<details class="log" id="log-s" open>`, `data-open="s "`, `<input type="checkbox" data-times checked> Timestamps`, `data-problem="-1">Previous problem</button>`, `data-problem="1">Next problem</button>`, `/log?download=true&amp;timestamps=true">with times</a>`)
	lacks(t, page, `<dl class="facts">`, `<svg class="dag"`, "<th>Stage</th>", "Queued")
}

// A build whose stages all went well opens the log of its last one: what
// a reader comes to see is how it ended.
func TestABuildThatWentWellOpensTheLogOfItsLastStage(t *testing.T) {
	e := site(t)
	c := e.login(t, "admin")
	id := e.build(t, c, "smoke", url.Values{"TIER": {"quick"}})
	eq(t, "status", e.wait(t, id).Status, store.Success)
	_, page := get(t, c, e.srv.URL+e.at(t, id))
	contains(t, page, `<svg class="dag"`, "<th>Stage</th>", `<p class="meta">`,
		`<details class="log" id="log-lint">`, `<details class="log" id="log-unit">`, `<details class="log" id="log-smoke" open>`)
}

func TestAFinishedBuildHasABuildAgainButtonForWhoMayBuild(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	id := e.build(t, admin, "smoke", nil)
	e.wait(t, id)
	path := e.at(t, id)
	_, page := get(t, admin, e.srv.URL+path)
	contains(t, page, `action="`+path+`/again"`, "Build again")

	// alice may view smoke and nothing more.
	alice := e.login(t, "alice")
	_, page = get(t, alice, e.srv.URL+path)
	lacks(t, page, "/again")
	res, page := postBody(t, alice, e.srv.URL+path+"/again", url.Values{"csrf": {e.token(t, alice)}})
	eq(t, "alice builds again", res.StatusCode, http.StatusForbidden)
	contains(t, page, "You are not allowed to do that.")

	res = post(t, admin, e.srv.URL+path+"/again", url.Values{"csrf": {e.token(t, admin)}})
	eq(t, "build again", res.StatusCode, http.StatusSeeOther)
	eq(t, "to the new build", res.Header.Get("Location"), "/jobs/smoke/2")
	b, err := e.st.BuildByNumber(context.Background(), "smoke", 2)
	must(t, err)
	eq(t, "trigger", b.Trigger, "web")
	eq(t, "trigger detail", b.TriggerDetail, "again #1")
	eq(t, "started by", b.StartedBy, "admin")
	e.wait(t, b.ID)
}

func TestARunningBuildHasNoBuildAgainButtonAndRefusesIt(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	id := e.build(t, admin, "slow", nil)
	path := e.at(t, id)
	eventually(t, "the build runs", func() bool {
		b, err := e.st.Build(context.Background(), id)
		return err == nil && b.Status == store.Running
	})
	_, page := get(t, admin, e.srv.URL+path)
	lacks(t, page, "/again")
	res, page := postBody(t, admin, e.srv.URL+path+"/again", url.Values{"csrf": {e.token(t, admin)}})
	eq(t, "build again", res.StatusCode, http.StatusConflict)
	contains(t, page, "has not ended")
	must(t, e.d.Cancel(context.Background(), id, "test"))
	e.wait(t, id)
}

func TestAFinishedBuildCanBeKeptAndStoppedFromItsPageByWhoMayBuild(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	id := e.build(t, admin, "smoke", nil)
	e.wait(t, id)
	path := e.at(t, id)
	_, page := get(t, admin, e.srv.URL+path)
	contains(t, page, `action="`+path+`/keep"`, ">Keep<")
	lacks(t, page, "Stop keeping", "kept by")

	// alice may view smoke and nothing more.
	alice := e.login(t, "alice")
	_, page = get(t, alice, e.srv.URL+path)
	lacks(t, page, `action="`+path+`/keep"`, "Stop keeping")
	res, page := postBody(t, alice, e.srv.URL+path+"/keep", url.Values{"csrf": {e.token(t, alice)}})
	eq(t, "alice keeps", res.StatusCode, http.StatusForbidden)
	contains(t, page, "You are not allowed to do that.")

	res = post(t, admin, e.srv.URL+path+"/keep", url.Values{"csrf": {e.token(t, admin)}})
	eq(t, "keep", res.StatusCode, http.StatusSeeOther)
	eq(t, "back to the build", res.Header.Get("Location"), path)
	b, err := e.st.Build(context.Background(), id)
	must(t, err)
	eq(t, "kept by", b.KeptBy, "admin")
	_, page = get(t, admin, e.srv.URL+path)
	contains(t, page, "kept by admin", `action="`+path+`/unkeep"`, "Stop keeping")
	lacks(t, page, `action="`+path+`/keep"`)
	_, page = get(t, alice, e.srv.URL+path)
	contains(t, page, "kept by admin")
	lacks(t, page, "Stop keeping")

	res = post(t, admin, e.srv.URL+path+"/unkeep", url.Values{"csrf": {e.token(t, admin)}})
	eq(t, "stop keeping", res.StatusCode, http.StatusSeeOther)
	b, err = e.st.Build(context.Background(), id)
	must(t, err)
	eq(t, "kept after", b.KeptAt == nil, true)
}

func TestAKeptBuildHasATagInTheJobsBuildList(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	first := e.build(t, admin, "smoke", nil)
	e.wait(t, first)
	second := e.build(t, admin, "smoke", nil)
	e.wait(t, second)
	must(t, e.st.KeepBuild(context.Background(), first, "admin", time.Now()))
	_, page := get(t, admin, e.srv.URL+"/jobs/smoke")
	eq(t, "one kept tag", strings.Count(page, `<span class="tag">kept</span>`), 1)
	_, row, _ := strings.Cut(page, `href="`+e.at(t, first)+`"`)
	row, _, _ = strings.Cut(row, "</tr>")
	contains(t, row, `<span class="tag">kept</span>`)
}

func TestARunningBuildHasNoKeepButtonAndRefusesIt(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	id := e.build(t, admin, "slow", nil)
	path := e.at(t, id)
	eventually(t, "the build runs", func() bool {
		b, err := e.st.Build(context.Background(), id)
		return err == nil && b.Status == store.Running
	})
	_, page := get(t, admin, e.srv.URL+path)
	lacks(t, page, "/keep")
	res, page := postBody(t, admin, e.srv.URL+path+"/keep", url.Values{"csrf": {e.token(t, admin)}})
	eq(t, "keep", res.StatusCode, http.StatusConflict)
	contains(t, page, "has not ended")
	must(t, e.d.Cancel(context.Background(), id, "test"))
	e.wait(t, id)
}
