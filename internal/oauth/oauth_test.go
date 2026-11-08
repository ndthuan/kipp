package oauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/store"
)

const (
	base     = "https://ci.example.com"
	verifier = "a-code-verifier-of-at-least-43-characters-in-all"
	callback = "https://claude.example.net/callback"
)

var challenge = func() string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}()

type fixture struct {
	t     *testing.T
	st    *store.Store
	srv   *Server
	mux   *http.ServeMux
	user  *store.User
	clock *atomic.Int64 // unix nanoseconds
}

func (f *fixture) now() time.Time { return time.Unix(0, f.clock.Load()) }

func (f *fixture) advance(d time.Duration) { f.clock.Add(int64(d)) }

func open(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	must(t, err)
	t.Cleanup(func() { must(t, st.Close()) })
	return st
}

func newFixture(t *testing.T, tune ...func(*Config)) *fixture {
	t.Helper()
	f := &fixture{t: t, st: open(t), mux: http.NewServeMux(), clock: &atomic.Int64{}}
	f.clock.Store(time.Now().UnixNano())
	// Every request is a client of its own, but for a test about the limit.
	var seq atomic.Int64
	cfg := Config{Store: f.st, BaseURL: base, ResourcePath: "/mcp", Logger: slog.New(slog.DiscardHandler), Now: f.now,
		Client: func(*http.Request) string { return strconv.FormatInt(seq.Add(1), 10) }}
	for _, tn := range tune {
		tn(&cfg)
	}
	f.srv = New(cfg)
	f.srv.Register(f.mux)
	var err error
	f.user, err = f.st.CreateUser(context.Background(), "ann", "h", false)
	must(t, err)
	return f
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

func (f *fixture) do(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec
}

func (f *fixture) form(path string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return f.do(req)
}

func (f *fixture) registerJSON(body any) *httptest.ResponseRecorder {
	b, err := json.Marshal(body)
	must(f.t, err)
	req := httptest.NewRequest(http.MethodPost, "/oauth/register", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	return f.do(req)
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("%d %q: %v", rec.Code, rec.Body.String(), err)
	}
	return m
}

// register makes a client the way an app does and returns its id.
func (f *fixture) register(redirects ...string) string {
	f.t.Helper()
	if len(redirects) == 0 {
		redirects = []string{callback}
	}
	rec := f.registerJSON(map[string]any{"redirect_uris": redirects, "client_name": "Claude"})
	eq(f.t, "register status", rec.Code, http.StatusCreated)
	return decode(f.t, rec)["client_id"].(string)
}

func (f *fixture) query(client string, extra url.Values) url.Values {
	q := url.Values{"response_type": {"code"}, "client_id": {client}, "redirect_uri": {callback}, "state": {"st"},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "scope": {"kipp:read kipp:build"}}
	for k, v := range extra {
		if len(v) == 1 && v[0] == "" {
			q.Del(k)
			continue
		}
		q[k] = v
	}
	return q
}

// authorize approves a request and returns the code.
func (f *fixture) authorize(client string, build bool) string {
	f.t.Helper()
	req, aerr := f.srv.ParseAuthorize(context.Background(), f.query(client, nil))
	if aerr != nil {
		f.t.Fatal(aerr)
	}
	to, err := f.srv.Approve(req, f.user, build)
	must(f.t, err)
	u, err := url.Parse(to)
	must(f.t, err)
	eq(f.t, "state", u.Query().Get("state"), "st")
	eq(f.t, "iss", u.Query().Get("iss"), base)
	return u.Query().Get("code")
}

func (f *fixture) tokenForm(client, code string, extra url.Values) url.Values {
	v := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier}, "client_id": {client}, "redirect_uri": {callback}, "resource": {base + "/mcp"}}
	for k, e := range extra {
		if len(e) == 1 && e[0] == "" {
			v.Del(k)
			continue
		}
		v[k] = e
	}
	return v
}

// signIn runs the whole code flow and returns the token response.
func (f *fixture) signIn(client string, build bool) map[string]any {
	f.t.Helper()
	rec := f.form("/oauth/token", f.tokenForm(client, f.authorize(client, build), nil))
	eq(f.t, "token status", rec.Code, http.StatusOK)
	return decode(f.t, rec)
}

func (f *fixture) refused(rec *httptest.ResponseRecorder, status int, code string) {
	f.t.Helper()
	eq(f.t, "status "+rec.Body.String(), rec.Code, status)
	eq(f.t, "error", decode(f.t, rec)["error"], any(code))
}

func TestTheDiscoveryDocumentsNameTheServerAndItsEndpoints(t *testing.T) {
	f := newFixture(t)
	for _, p := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		rec := f.do(httptest.NewRequest(http.MethodGet, p, nil))
		eq(t, p+" status", rec.Code, http.StatusOK)
		m := decode(t, rec)
		eq(t, "resource", m["resource"], any(base+"/mcp"))
		eq(t, "authorization_servers", m["authorization_servers"].([]any)[0], any(base))
		eq(t, "bearer", m["bearer_methods_supported"].([]any)[0], any("header"))
		eq(t, "scopes", len(m["scopes_supported"].([]any)), 2)
	}
	rec := f.do(httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil))
	m := decode(t, rec)
	for k, want := range map[string]any{
		"issuer": base, "authorization_endpoint": base + "/oauth/authorize", "token_endpoint": base + "/oauth/token",
		"registration_endpoint": base + "/oauth/register", "revocation_endpoint": base + "/oauth/revoke",
		"client_id_metadata_document_supported": true, "authorization_response_iss_parameter_supported": true,
	} {
		eq(t, k, m[k], want)
	}
	for k, want := range map[string]string{"code_challenge_methods_supported": "S256", "token_endpoint_auth_methods_supported": "none", "response_types_supported": "code"} {
		got := m[k].([]any)
		eq(t, k, len(got), 1)
		eq(t, k, got[0], any(want))
	}
	eq(t, "grants", len(m["grant_types_supported"].([]any)), 2)
	// No OpenID Connect.
	eq(t, "openid-configuration", f.do(httptest.NewRequest(http.MethodGet, "/.well-known/openid-configuration", nil)).Code, http.StatusNotFound)
}

func TestTheBaseURLIsAnHttpsOriginOrOneOnThisMachine(t *testing.T) {
	for in, want := range map[string]string{
		"https://ci.example.com": "https://ci.example.com", "https://CI.example.com/": "https://ci.example.com",
		"https://ci.example.com:8443": "https://ci.example.com:8443", "http://localhost:8085": "http://localhost:8085",
		"http://127.0.0.1:8085": "http://127.0.0.1:8085", "http://[::1]:8085": "http://[::1]:8085",
	} {
		got, err := ParseBaseURL(in)
		must(t, err)
		eq(t, in, got, want)
	}
	for _, in := range []string{"http://ci.example.com", "ci.example.com", "https://ci.example.com/kipp", "https://u:p@ci.example.com",
		"https://ci.example.com?x=1", "ftp://localhost", "http://192.168.1.5:8085", "", "https://"} {
		if got, err := ParseBaseURL(in); err == nil {
			t.Errorf("%q is accepted as %q", in, got)
		}
	}
}

func TestRegistrationTakesHttpsAndLoopbackRedirectsAndNothingElse(t *testing.T) {
	f := newFixture(t)
	good := f.registerJSON(map[string]any{"redirect_uris": []string{"https://a.example/cb", "http://127.0.0.1:5000/cb", "http://localhost/cb", "http://[::1]:7/cb"},
		"client_name": "  Claude\u0000 Code  ", "token_endpoint_auth_method": "client_secret_post", "grant_types": []string{"authorization_code", "refresh_token"}})
	eq(t, "status", good.Code, http.StatusCreated)
	m := decode(t, good)
	eq(t, "name", m["client_name"], any("Claude Code"))
	eq(t, "auth method", m["token_endpoint_auth_method"], any("none"))
	if m["client_id"] == "" || m["client_secret"] != nil {
		t.Fatalf("%v", m)
	}
	eq(t, "no-store", good.Header().Get("Cache-Control"), "no-store")
	c, err := f.st.OAuthClient(context.Background(), m["client_id"].(string))
	must(t, err)
	eq(t, "stored uris", len(c.RedirectURIs), 4)

	for _, bad := range []any{
		map[string]any{},
		map[string]any{"redirect_uris": []string{}},
		map[string]any{"redirect_uris": []string{"http://example.com/cb"}},
		map[string]any{"redirect_uris": []string{"http://127.0.0.1.example.com/cb"}},
		map[string]any{"redirect_uris": []string{"javascript:alert(1)"}},
		map[string]any{"redirect_uris": []string{"claude://callback"}},
		map[string]any{"redirect_uris": []string{"https://a.example/cb#frag"}},
		map[string]any{"redirect_uris": []string{"https://user:pw@a.example/cb"}},
		map[string]any{"redirect_uris": []string{"/relative"}},
		map[string]any{"redirect_uris": []string{"https://a.example/cb"}, "grant_types": []string{"client_credentials"}},
		map[string]any{"redirect_uris": []string{"https://a.example/cb"}, "grant_types": []string{"refresh_token"}},
		map[string]any{"redirect_uris": []string{"https://a.example/cb"}, "response_types": []string{"token"}},
	} {
		rec := f.registerJSON(bad)
		eq(t, "status for "+rec.Body.String(), rec.Code, http.StatusBadRequest)
	}
	notJSON := f.form("/oauth/register", url.Values{"redirect_uris": {"https://a.example/cb"}})
	eq(t, "a form", notJSON.Code, http.StatusBadRequest)
	big := httptest.NewRequest(http.MethodPost, "/oauth/register", strings.NewReader(`{"client_name":"`+strings.Repeat("x", maxBody)+`"}`))
	eq(t, "a body over the cap", f.do(big).Code, http.StatusBadRequest)
}

func TestRegistrationsAreSlowedPerClientAddressLikeWrongPasswords(t *testing.T) {
	f := newFixture(t, func(c *Config) {
		c.Client = func(r *http.Request) string { return r.Header.Get("X-Client") }
	})
	post := func(client string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]any{"redirect_uris": []string{callback}})
		req := httptest.NewRequest(http.MethodPost, "/oauth/register", bytes.NewReader(b))
		req.Header.Set("X-Client", client)
		return f.do(req)
	}
	for range 5 {
		eq(t, "registration", post("1.2.3.4").Code, http.StatusCreated)
	}
	rec := post("1.2.3.4")
	f.refused(rec, http.StatusTooManyRequests, "temporarily_unavailable")
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}
	eq(t, "another client", post("5.6.7.8").Code, http.StatusCreated)
	f.advance(time.Hour)
	eq(t, "after the wait", post("1.2.3.4").Code, http.StatusCreated)
}

func TestTheTableOfRegisteredClientsIsBounded(t *testing.T) {
	f := newFixture(t)
	for i := range maxClients {
		_, err := f.st.SaveOAuthClient(context.Background(), &store.OAuthClient{ID: "c" + string(rune('a'+i%26)) + strings.Repeat("x", i/26), Name: "n", RedirectURIs: []string{callback}}, f.now())
		must(t, err)
	}
	f.refused(f.registerJSON(map[string]any{"redirect_uris": []string{callback}}), http.StatusServiceUnavailable, "temporarily_unavailable")
}

func TestAnAuthorizationRequestIsHeldToTheClientsAddressesAndToPKCE(t *testing.T) {
	f := newFixture(t)
	id := f.register(callback, "http://127.0.0.1:3000/cb")
	parse := func(extra url.Values) (*AuthRequest, *AuthError) {
		return f.srv.ParseAuthorize(context.Background(), f.query(id, extra))
	}
	req, aerr := parse(nil)
	if aerr != nil {
		t.Fatal(aerr)
	}
	eq(t, "client", req.Client.Name, "Claude")
	eq(t, "resource defaults to the audience", req.Resource, base+"/mcp")
	eq(t, "build asked", req.WantsBuild(), true)

	// A request whose address is not the client's is shown to the user, not
	// answered at that address.
	for name, extra := range map[string]url.Values{
		"another address":      {"redirect_uri": {"https://evil.example/callback"}},
		"a prefix of one":      {"redirect_uri": {callback + "/more"}},
		"a different path":     {"redirect_uri": {"https://claude.example.net/other"}},
		"a different scheme":   {"redirect_uri": {"http://claude.example.net/callback"}},
		"no address":           {"redirect_uri": {""}},
		"loopback, other path": {"redirect_uri": {"http://127.0.0.1:9/other"}},
		"loopback, other host": {"redirect_uri": {"http://localhost:3000/cb"}},
		"a query added":        {"redirect_uri": {callback + "?a=b"}},
		"a repeated address":   {"redirect_uri": {callback, "https://evil.example/callback"}},
	} {
		if _, e := parse(extra); e == nil || e.RedirectURI != "" {
			t.Errorf("%s: %+v", name, e)
		}
	}
	if _, e := f.srv.ParseAuthorize(context.Background(), f.query("nobody", nil)); e == nil || e.RedirectURI != "" {
		t.Errorf("an unknown client: %+v", e)
	}
	// A loopback address takes any port.
	if r, e := parse(url.Values{"redirect_uri": {"http://127.0.0.1:54321/cb"}}); e != nil || r.RedirectURI != "http://127.0.0.1:54321/cb" {
		t.Errorf("loopback with another port: %+v", e)
	}

}

func TestTheRestOfAnAuthorizationRequestIsAnswerToTheClientAtItsOwnAddress(t *testing.T) {
	f := newFixture(t)
	id := f.register(callback, "http://127.0.0.1:3000/cb")
	parse := func(extra url.Values) (*AuthRequest, *AuthError) {
		return f.srv.ParseAuthorize(context.Background(), f.query(id, extra))
	}
	for name, tc := range map[string]struct {
		extra url.Values
		code  string
	}{
		"no PKCE":               {url.Values{"code_challenge": {""}, "code_challenge_method": {""}}, "invalid_request"},
		"plain PKCE":            {url.Values{"code_challenge_method": {"plain"}}, "invalid_request"},
		"no method":             {url.Values{"code_challenge_method": {""}}, "invalid_request"},
		"a challenge too short": {url.Values{"code_challenge": {"abc"}}, "invalid_request"},
		"a token response":      {url.Values{"response_type": {"token"}}, "unsupported_response_type"},
		"another resource":      {url.Values{"resource": {"https://other.example.com/mcp"}}, "invalid_target"},
		"the origin only":       {url.Values{"resource": {base}}, "invalid_target"},
		"two resources":         {url.Values{"resource": {base + "/mcp", base + "/mcp"}}, "invalid_request"},
		"an unknown scope":      {url.Values{"scope": {"kipp:read admin"}}, "invalid_scope"},
	} {
		_, e := parse(tc.extra)
		if e == nil || e.Code != tc.code {
			t.Errorf("%s: %+v", name, e)
			continue
		}
		if tc.code != "invalid_request" || name != "two resources" {
			eq(t, name+": redirect", e.RedirectURI, callback)
			eq(t, name+": state", e.State, "st")
		}
	}
	// What is allowed.
	if r, e := parse(url.Values{"resource": {"HTTPS://CI.EXAMPLE.COM/mcp"}, "scope": {""}}); e != nil || r.WantsBuild() || len(r.Scopes) != 1 {
		t.Errorf("a resource in another case and no scope: %+v %+v", r, e)
	}
	// The error goes back with the state and the issuer.
	_, e := parse(url.Values{"code_challenge_method": {"plain"}})
	to, err := url.Parse(f.srv.ErrorRedirect(e))
	must(t, err)
	eq(t, "error", to.Query().Get("error"), "invalid_request")
	eq(t, "state", to.Query().Get("state"), "st")
	eq(t, "iss", to.Query().Get("iss"), base)
	eq(t, "host", to.Host, "claude.example.net")
}

func TestDenyTellsTheClientAccessWasDenied(t *testing.T) {
	f := newFixture(t)
	id := f.register()
	req, e := f.srv.ParseAuthorize(context.Background(), f.query(id, nil))
	if e != nil {
		t.Fatal(e)
	}
	to, err := url.Parse(f.srv.Deny(req))
	must(t, err)
	eq(t, "error", to.Query().Get("error"), "access_denied")
	eq(t, "state", to.Query().Get("state"), "st")
	if to.Query().Get("code") != "" {
		t.Fatal("a code with a denial")
	}
}

func TestACodeIsExchangedOnceForTokensOfTheScopesApproved(t *testing.T) {
	f := newFixture(t)
	id := f.register()
	m := f.signIn(id, true)
	eq(t, "type", m["token_type"], any("Bearer"))
	eq(t, "scope", m["scope"], any("kipp:read kipp:build"))
	eq(t, "expires_in", m["expires_in"], any(float64(3600)))
	access, refresh := m["access_token"].(string), m["refresh_token"].(string)
	if !strings.HasPrefix(access, "kippo_") || !strings.HasPrefix(refresh, "kippr_") || strings.HasPrefix(access, "kipp_") {
		t.Fatalf("%q %q", access, refresh)
	}
	v, err := f.srv.Verify(context.Background(), access)
	must(t, err)
	eq(t, "user", v.User.Name, "ann")
	eq(t, "build", v.Has(ScopeBuild), true)

	// Unticking build leaves read alone.
	m = f.signIn(id, false)
	eq(t, "scope", m["scope"], any("kipp:read"))
	v, err = f.srv.Verify(context.Background(), m["access_token"].(string))
	must(t, err)
	eq(t, "build", v.Has(ScopeBuild), false)

	// The response is not to be stored.
	rec := f.form("/oauth/token", f.tokenForm(id, f.authorize(id, true), nil))
	eq(t, "no-store", rec.Header().Get("Cache-Control"), "no-store")
	eq(t, "pragma", rec.Header().Get("Pragma"), "no-cache")
}

func TestOnlyAnAppThatAskedForBuildCanBeGivenIt(t *testing.T) {
	f := newFixture(t)
	id := f.register()
	req, e := f.srv.ParseAuthorize(context.Background(), f.query(id, url.Values{"scope": {"kipp:read"}}))
	if e != nil {
		t.Fatal(e)
	}
	to, err := f.srv.Approve(req, f.user, true) // the box is ticked in a form the app never asked about
	must(t, err)
	u, _ := url.Parse(to)
	m := decode(t, f.form("/oauth/token", f.tokenForm(id, u.Query().Get("code"), nil)))
	eq(t, "scope", m["scope"], any("kipp:read"))
}

func TestACodeIsRefusedWhenItIsUsedAgainWrongOrLate(t *testing.T) {
	f := newFixture(t)
	id := f.register()
	other := f.register("https://other.example/cb")

	// Used twice: the second is refused and the grant the first made ends.
	code := f.authorize(id, true)
	first := decode(t, f.form("/oauth/token", f.tokenForm(id, code, nil)))
	access := first["access_token"].(string)
	if _, err := f.srv.Verify(context.Background(), access); err != nil {
		t.Fatal(err)
	}
	f.refused(f.form("/oauth/token", f.tokenForm(id, code, nil)), http.StatusBadRequest, "invalid_grant")
	if _, err := f.srv.Verify(context.Background(), access); err == nil {
		t.Fatal("the token of a code used twice still works")
	}
	if gs, _ := f.srv.Grants(context.Background(), f.user.ID); len(gs) != 0 {
		t.Fatalf("grants remain: %+v", gs)
	}

	// Expired.
	code = f.authorize(id, true)
	f.advance(CodeTTL + time.Second)
	f.refused(f.form("/oauth/token", f.tokenForm(id, code, nil)), http.StatusBadRequest, "invalid_grant")

	// Wrong verifier, and the code is spent by the guess.
	code = f.authorize(id, true)
	f.refused(f.form("/oauth/token", f.tokenForm(id, code, url.Values{"code_verifier": {strings.Repeat("b", 43)}})), http.StatusBadRequest, "invalid_grant")
	f.refused(f.form("/oauth/token", f.tokenForm(id, code, nil)), http.StatusBadRequest, "invalid_grant")

	// A verifier that is not in the alphabet, or too short.
	for _, v := range []string{"short", verifier + "!", strings.Repeat("a", 129)} {
		f.refused(f.form("/oauth/token", f.tokenForm(id, f.authorize(id, true), url.Values{"code_verifier": {v}})), http.StatusBadRequest, "invalid_grant")
	}

	// Another redirect_uri, another client, another resource.
	f.refused(f.form("/oauth/token", f.tokenForm(id, f.authorize(id, true), url.Values{"redirect_uri": {"https://other.example/cb"}})), http.StatusBadRequest, "invalid_grant")
	f.refused(f.form("/oauth/token", f.tokenForm(other, f.authorize(id, true), nil)), http.StatusBadRequest, "invalid_grant")
	f.refused(f.form("/oauth/token", f.tokenForm(id, f.authorize(id, true), url.Values{"resource": {"https://other.example.com/mcp"}})), http.StatusBadRequest, "invalid_target")

	// Missing pieces, unknown codes and grant types.
	f.refused(f.form("/oauth/token", f.tokenForm(id, f.authorize(id, true), url.Values{"code_verifier": {""}})), http.StatusBadRequest, "invalid_request")
	f.refused(f.form("/oauth/token", f.tokenForm(id, "not-a-code", nil)), http.StatusBadRequest, "invalid_grant")
	f.refused(f.form("/oauth/token", url.Values{"grant_type": {"password"}}), http.StatusBadRequest, "unsupported_grant_type")
	f.refused(f.form("/oauth/token", url.Values{}), http.StatusBadRequest, "invalid_request")
	if gs, _ := f.srv.Grants(context.Background(), f.user.ID); len(gs) != 0 {
		t.Fatalf("a refused exchange made grants: %+v", gs)
	}
}

func TestTheTokenEndpointReadsTheBodyAlone(t *testing.T) {
	f := newFixture(t)
	id := f.register()
	form := f.tokenForm(id, f.authorize(id, true), nil)
	req := httptest.NewRequest(http.MethodPost, "/oauth/token?"+form.Encode(), nil)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	f.refused(f.do(req), http.StatusBadRequest, "invalid_request")
	jsonBody := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(`{"grant_type":"authorization_code"}`))
	jsonBody.Header.Set("Content-Type", "application/json")
	f.refused(f.do(jsonBody), http.StatusBadRequest, "invalid_request")
	eq(t, "GET", f.do(httptest.NewRequest(http.MethodGet, "/oauth/token", nil)).Code, http.StatusMethodNotAllowed)
}

func (f *fixture) refreshForm(client, token string) url.Values {
	return url.Values{"grant_type": {"refresh_token"}, "refresh_token": {token}, "client_id": {client}}
}

func TestARefreshTokenIsReplacedByEachUseAndAUsedOneEndsTheGrant(t *testing.T) {
	f := newFixture(t)
	id := f.register()
	other := f.register("https://other.example/cb")
	m := f.signIn(id, true)
	r1, a1 := m["refresh_token"].(string), m["access_token"].(string)

	f.advance(10 * time.Minute)
	rec := f.form("/oauth/token", f.refreshForm(id, r1))
	eq(t, "refresh status "+rec.Body.String(), rec.Code, http.StatusOK)
	n := decode(t, rec)
	r2, a2 := n["refresh_token"].(string), n["access_token"].(string)
	if r2 == r1 || a2 == a1 {
		t.Fatal("a refresh handed back the old tokens")
	}
	eq(t, "scope kept", n["scope"], any("kipp:read kipp:build"))
	if _, err := f.srv.Verify(context.Background(), a2); err != nil {
		t.Fatal(err)
	}

	// Another client's use of the token is refused and costs nothing.
	f.refused(f.form("/oauth/token", f.refreshForm(other, r2)), http.StatusBadRequest, "invalid_grant")
	if _, err := f.srv.Verify(context.Background(), a2); err != nil {
		t.Fatalf("another client's attempt ended the grant: %v", err)
	}
	f.refused(f.form("/oauth/token", f.refreshForm(id, "kippr_unknown")), http.StatusBadRequest, "invalid_grant")
	f.refused(f.form("/oauth/token", f.refreshForm(id, a2)), http.StatusBadRequest, "invalid_grant") // an access token
	f.refused(f.form("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {id}}), http.StatusBadRequest, "invalid_request")
	bad := f.refreshForm(id, r2)
	bad.Set("resource", "https://other.example.com/mcp")
	f.refused(f.form("/oauth/token", bad), http.StatusBadRequest, "invalid_target")

	// The old refresh token again: refused, and the grant, with the newest
	// tokens, is gone.
	f.refused(f.form("/oauth/token", f.refreshForm(id, r1)), http.StatusBadRequest, "invalid_grant")
	for _, tok := range []string{a1, a2} {
		if _, err := f.srv.Verify(context.Background(), tok); err == nil {
			t.Fatalf("%s still works after a refresh token was reused", tok[:8])
		}
	}
	f.refused(f.form("/oauth/token", f.refreshForm(id, r2)), http.StatusBadRequest, "invalid_grant")
	if gs, _ := f.srv.Grants(context.Background(), f.user.ID); len(gs) != 0 {
		t.Fatalf("grants remain: %+v", gs)
	}
	if !audited(t, f.st, "oauth.refresh.reuse") {
		t.Fatal("the reuse is not in the audit")
	}
}

func TestATokenLastsAnHourARefreshTokenThirtyDaysAndTheConsentNinety(t *testing.T) {
	f := newFixture(t)
	id := f.register()
	m := f.signIn(id, true)
	access, refresh := m["access_token"].(string), m["refresh_token"].(string)
	f.advance(AccessTTL - time.Minute)
	if _, err := f.srv.Verify(context.Background(), access); err != nil {
		t.Fatal(err)
	}
	f.advance(2 * time.Minute)
	if _, err := f.srv.Verify(context.Background(), access); err == nil {
		t.Fatal("an access token past its hour")
	}
	// Used every 29 days, the refresh token goes on, to the 90th day.
	for range 3 {
		f.advance(29 * 24 * time.Hour)
		rec := f.form("/oauth/token", f.refreshForm(id, refresh))
		eq(t, "refresh "+rec.Body.String(), rec.Code, http.StatusOK)
		refresh = decode(t, rec)["refresh_token"].(string)
	}
	f.advance(29 * 24 * time.Hour) // day 87 + 29 > 90
	f.refused(f.form("/oauth/token", f.refreshForm(id, refresh)), http.StatusBadRequest, "invalid_grant")

	// Not used for 31 days.
	m = f.signIn(id, true)
	f.advance(31 * 24 * time.Hour)
	f.refused(f.form("/oauth/token", f.refreshForm(id, m["refresh_token"].(string))), http.StatusBadRequest, "invalid_grant")
}

func TestATokenIsGoodForThisServersResourceAlone(t *testing.T) {
	f := newFixture(t)
	id := f.register()
	access := f.signIn(id, true)["access_token"].(string)
	// The same database, served at another address: its tokens were made
	// for the first.
	moved := New(Config{Store: f.st, BaseURL: "https://other.example.com", ResourcePath: "/mcp", Logger: slog.New(slog.DiscardHandler), Now: f.now})
	if _, err := moved.Verify(context.Background(), access); err == nil {
		t.Fatal("a token for another resource is accepted")
	}
	for _, tok := range []string{"", "kipp_" + strings.Repeat("a", 43), "kippa_x", "kippr_x", "kippo_unknown", AccessPrefix} {
		if _, err := f.srv.Verify(context.Background(), tok); err == nil {
			t.Errorf("%q is accepted", tok)
		}
	}
	// A refresh token is not an access token.
	m := f.signIn(id, true)
	if _, err := f.srv.Verify(context.Background(), m["refresh_token"].(string)); err == nil {
		t.Fatal("a refresh token as an access token")
	}
}

func TestRevokingEitherTokenEndsTheWholeGrant(t *testing.T) {
	f := newFixture(t)
	id := f.register()
	other := f.register("https://other.example/cb")
	for _, which := range []string{"access_token", "refresh_token"} {
		m := f.signIn(id, true)
		// Another client cannot revoke it, and an unknown token is no error.
		eq(t, "other client", f.form("/oauth/revoke", url.Values{"token": {m[which].(string)}, "client_id": {other}}).Code, http.StatusOK)
		if _, err := f.srv.Verify(context.Background(), m["access_token"].(string)); err != nil {
			t.Fatalf("another client revoked the grant: %v", err)
		}
		eq(t, "unknown", f.form("/oauth/revoke", url.Values{"token": {"kippo_unknown"}}).Code, http.StatusOK)
		eq(t, "not ours", f.form("/oauth/revoke", url.Values{"token": {"whatever"}}).Code, http.StatusOK)
		rec := f.form("/oauth/revoke", url.Values{"token": {m[which].(string)}, "client_id": {id}, "token_type_hint": {"refresh_token"}})
		eq(t, which, rec.Code, http.StatusOK)
		if _, err := f.srv.Verify(context.Background(), m["access_token"].(string)); err == nil {
			t.Fatalf("revoking the %s left the access token", which)
		}
		f.refused(f.form("/oauth/token", f.refreshForm(id, m["refresh_token"].(string))), http.StatusBadRequest, "invalid_grant")
	}
	f.refused(f.form("/oauth/revoke", url.Values{}), http.StatusBadRequest, "invalid_request")
	if !audited(t, f.st, "oauth.grant.revoke") {
		t.Fatal("the revocation is not in the audit")
	}
}

func TestAUserRevokesTheirOwnGrantAndNoOnesElse(t *testing.T) {
	f := newFixture(t)
	id := f.register()
	access := f.signIn(id, true)["access_token"].(string)
	gs, err := f.srv.Grants(context.Background(), f.user.ID)
	must(t, err)
	eq(t, "grants", len(gs), 1)
	eq(t, "client name", gs[0].ClientName, "Claude")
	bob, err := f.st.CreateUser(context.Background(), "bob", "h", false)
	must(t, err)
	if err := f.srv.Revoke(context.Background(), "bob", bob, gs[0].ID); err == nil {
		t.Fatal("bob revoked ann's grant")
	}
	if _, err := f.srv.Verify(context.Background(), access); err != nil {
		t.Fatal(err)
	}
	must(t, f.srv.Revoke(context.Background(), "ann", f.user, gs[0].ID))
	if _, err := f.srv.Verify(context.Background(), access); err == nil {
		t.Fatal("a revoked grant's token works")
	}
}

func TestGrantsAreAuditedWithoutSecrets(t *testing.T) {
	f := newFixture(t)
	id := f.register()
	m := f.signIn(id, true)
	rows, err := f.st.AuditBefore(context.Background(), 0, 50)
	must(t, err)
	seen := map[string]bool{}
	for _, a := range rows {
		seen[a.Action] = true
		for _, secret := range []string{m["access_token"].(string), m["refresh_token"].(string), verifier} {
			if strings.Contains(a.Detail+a.Target+a.Actor, secret) {
				t.Fatalf("the audit holds a secret: %+v", a)
			}
		}
	}
	if !seen["oauth.client.register"] || !seen["oauth.grant.approve"] {
		t.Fatalf("audit actions: %v", seen)
	}
}

func audited(t *testing.T, st *store.Store, action string) bool {
	t.Helper()
	rows, err := st.AuditBefore(context.Background(), 0, 100)
	must(t, err)
	for _, a := range rows {
		if a.Action == action {
			return true
		}
	}
	return false
}

// A body that is not read from the address, and not larger than the cap.
func TestTheEndpointsCapWhatTheyRead(t *testing.T) {
	f := newFixture(t)
	big := strings.Repeat("a", maxBody+1)
	for _, p := range []string{"/oauth/token", "/oauth/revoke"} {
		req := httptest.NewRequest(http.MethodPost, p, io.NopCloser(strings.NewReader("token="+big)))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		eq(t, p, f.do(req).Code, http.StatusBadRequest)
	}
}
