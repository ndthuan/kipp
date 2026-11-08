package web

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/mcp"
	"github.com/ndthuan/kipp/internal/oauth"
)

const (
	codeVerifier = "a-code-verifier-of-at-least-43-characters-in-all"
	appCallback  = "https://claude.example.net/callback"
)

var codeChallenge = func() string {
	sum := sha256.Sum256([]byte(codeVerifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}()

// oauthSite is site() with the sign-in for MCP clients on, and /mcp
// beside it, on a server of its own whose address is the issuer. The
// helpers of env work on it as on the other.
type oauthSite struct {
	*env
	oauth *oauth.Server
}

func newOAuthSite(t *testing.T, fetch *http.Client) *oauthSite {
	t.Helper()
	base := site(t)
	var h http.Handler
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	o := oauth.New(oauth.Config{Store: base.st, BaseURL: srv.URL, ResourcePath: mcp.Path, Logger: slog.New(slog.DiscardHandler), Fetch: fetch})
	a := base.site.api
	pages := newSite(a, base.site.auth, base.st, "t", WithOAuth(o))
	m := mcp.New(mcp.Config{Service: a.Service(), Auth: base.site.auth, Verify: mcp.OAuthTokens(o, mcp.APITokens(base.site.auth)),
		ResourceMetadataURL: o.ResourceMetadataURL(), Version: "t", Logger: slog.New(slog.DiscardHandler)})
	mux := http.NewServeMux()
	mux.Handle("/api/", a.Handler())
	mux.Handle(mcp.Path, m.Handler())
	o.Register(mux)
	mux.Handle("/", pages.handler())
	h = mux
	return &oauthSite{env: &env{srv: srv, st: base.st, d: base.d, site: pages}, oauth: o}
}

func (o *oauthSite) register(t *testing.T, redirect string) string {
	t.Helper()
	body := fmt.Sprintf(`{"redirect_uris": [%q], "client_name": "Claude <b>Code</b>"}`, redirect)
	res, err := o.srv.Client().Post(o.srv.URL+"/oauth/register", "application/json", strings.NewReader(body))
	must(t, err)
	defer func() { _ = res.Body.Close() }()
	eq(t, "register", res.StatusCode, http.StatusCreated)
	var m struct {
		ClientID string `json:"client_id"`
	}
	must(t, json.NewDecoder(res.Body).Decode(&m))
	return m.ClientID
}

// authorizeQuery is the query of an app's authorization request.
func (o *oauthSite) authorizeQuery(client string, extra url.Values) url.Values {
	q := url.Values{"response_type": {"code"}, "client_id": {client}, "redirect_uri": {appCallback}, "state": {"xyz"},
		"code_challenge": {codeChallenge}, "code_challenge_method": {"S256"}, "scope": {"kipp:read kipp:build"}, "resource": {o.srv.URL + "/mcp"}}
	for k, v := range extra {
		if len(v) == 1 && v[0] == "" {
			q.Del(k)
			continue
		}
		q[k] = v
	}
	return q
}

// consent posts the consent form of the page the query leads to, as the
// browser of c would, with the fields given beside the form's own.
func (o *oauthSite) consent(t *testing.T, c *http.Client, q url.Values, decision string, build bool) *http.Response {
	t.Helper()
	status, page := get(t, c, o.srv.URL+"/oauth/authorize?"+q.Encode())
	eq(t, "consent page", status, http.StatusOK)
	form := url.Values{"csrf": {csrfOf(t, page)}, "decision": {decision}}
	for _, k := range consentFields {
		if v := hiddenValue(t, page, k); v != "" {
			form.Set(k, v)
		}
	}
	if build {
		form.Set("build", "on")
	}
	return post(t, c, o.srv.URL+"/oauth/authorize", form)
}

// hiddenValue is the value of the hidden input called name.
func hiddenValue(t *testing.T, page, name string) string {
	t.Helper()
	_, rest, ok := strings.Cut(page, `<input type="hidden" name="`+name+`" value="`)
	if !ok {
		return ""
	}
	v, _, _ := strings.Cut(rest, `"`)
	return strings.NewReplacer("&amp;", "&", "&#34;", `"`, "&lt;", "<", "&gt;", ">", "&#39;", "'", "&#43;", "+").Replace(v)
}

func codeOf(t *testing.T, res *http.Response) string {
	t.Helper()
	eq(t, "redirect", res.StatusCode, http.StatusSeeOther)
	to, err := url.Parse(res.Header.Get("Location"))
	must(t, err)
	eq(t, "redirected to the app", to.Scheme+"://"+to.Host+to.Path, appCallback)
	eq(t, "state", to.Query().Get("state"), "xyz")
	if !strings.HasPrefix(to.Query().Get("iss"), "https://127.0.0.1:") {
		t.Fatalf("iss %q", to.Query().Get("iss"))
	}
	return to.Query().Get("code")
}

func (o *oauthSite) exchange(t *testing.T, client, code string) map[string]any {
	t.Helper()
	res, err := o.srv.Client().PostForm(o.srv.URL+"/oauth/token", url.Values{"grant_type": {"authorization_code"}, "code": {code},
		"code_verifier": {codeVerifier}, "client_id": {client}, "redirect_uri": {appCallback}, "resource": {o.srv.URL + "/mcp"}})
	must(t, err)
	defer func() { _ = res.Body.Close() }()
	eq(t, "token status", res.StatusCode, http.StatusOK)
	var m map[string]any
	must(t, json.NewDecoder(res.Body).Decode(&m))
	return m
}

// rpc posts one JSON-RPC call to /mcp with the bearer token.
func (o *oauthSite) rpc(t *testing.T, token, method string, params string) (*http.Response, string) {
	t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":%s}`, method, params)
	req, err := http.NewRequest(http.MethodPost, o.srv.URL+mcp.Path, strings.NewReader(body))
	must(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := o.srv.Client().Do(req)
	must(t, err)
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	must(t, err)
	return res, string(b)
}

func toolCall(name, job string) string {
	return fmt.Sprintf(`{"name":%q,"arguments":{"job":%q}}`, name, job)
}

// An app is connected from the first request to the first call of /mcp,
// and the user types the password once: the login that leads to the
// consent page counts as having typed it.
func TestAnAppIsConnectedWithOnePasswordAndItsTokensWorkOnMcp(t *testing.T) {
	o := newOAuthSite(t, nil)
	client := o.register(t, appCallback)
	q := o.authorizeQuery(client, nil)
	authorize := "/oauth/authorize?" + q.Encode()

	// Nobody is logged in: the login leads back, and no code is given.
	c := o.anon(t)
	res, err := c.Get(o.srv.URL + authorize)
	must(t, err)
	_ = res.Body.Close()
	eq(t, "to the login", res.StatusCode, http.StatusSeeOther)
	login := res.Header.Get("Location")
	if !strings.HasPrefix(login, "/login?next=") {
		t.Fatalf("login at %q", login)
	}
	next, err := url.Parse(login)
	must(t, err)
	res = post(t, c, o.srv.URL+"/login", url.Values{"name": {"bob"}, "password": {"pw"}, "next": {next.Query().Get("next")}})
	eq(t, "login", res.StatusCode, http.StatusSeeOther)
	eq(t, "back to the consent page", res.Header.Get("Location"), authorize)

	// The page: who, what, where it goes, and nothing from the app unescaped.
	status, page := get(t, c, o.srv.URL+authorize)
	eq(t, "consent", status, http.StatusOK)
	contains(t, page, "Connect Claude &lt;b&gt;Code&lt;/b&gt;", "claude.example.net", "<strong>bob</strong>", `name="build" checked`, `value="approve"`, `value="deny"`)
	lacks(t, page, "<b>Code</b>")
	selfContained(t, page)

	// Approve at once: no password is asked.
	code := codeOf(t, o.consent(t, c, q, "approve", true))
	if code == "" {
		t.Fatal("no code")
	}
	m := o.exchange(t, client, code)
	access := m["access_token"].(string)
	eq(t, "scope", m["scope"], any("kipp:read kipp:build"))

	// /mcp takes it and answers as bob.
	res2, body := o.rpc(t, access, "tools/call", toolCall("get_job", "smoke"))
	eq(t, "get_job "+body, res2.StatusCode, http.StatusOK)
	contains(t, body, "smoke")
	res2, _ = o.rpc(t, access, "tools/call", `{"name":"overview"}`)
	eq(t, "overview", res2.StatusCode, http.StatusOK)
	res2, body = o.rpc(t, access, "tools/list", "{}")
	contains(t, body, "run_build")
	eq(t, "tools/list", res2.StatusCode, http.StatusOK)
}

func TestApprovingNeedsThePasswordWhenTheSessionIsNotInSudoAndNothingIsGivenBeforeIt(t *testing.T) {
	o := newOAuthSite(t, nil)
	client := o.register(t, appCallback)
	q := o.authorizeQuery(client, nil)
	c := o.login(t, "bob") // a login that did not lead to the consent page

	res := o.consent(t, c, q, "approve", true)
	eq(t, "without sudo", res.StatusCode, http.StatusForbidden)
	status, _ := get(t, c, o.srv.URL+"/oauth/authorize?"+q.Encode())
	eq(t, "page", status, http.StatusOK)
	_, page := postBody(t, c, o.srv.URL+"/oauth/authorize", url.Values{"csrf": {o.token(t, c)}, "decision": {"approve"}, "client_id": {client},
		"redirect_uri": {appCallback}, "code_challenge": {codeChallenge}, "code_challenge_method": {"S256"}, "response_type": {"code"}, "state": {"xyz"}})
	contains(t, page, "Confirm your password", "let an app in", `action="/sudo"`)
	// The form leads back to the same request, as the app made it.
	_, back, _ := strings.Cut(page, `name="next" value="`)
	back, _, _ = strings.Cut(back, `"`)
	contains(t, back, "/oauth/authorize?", "client_id="+client)

	// The password, and the page it leads to approves.
	res = post(t, c, o.srv.URL+"/sudo", url.Values{"csrf": {o.token(t, c)}, "password": {"pw"}, "next": {authorizeLead(q)}})
	eq(t, "sudo", res.StatusCode, http.StatusSeeOther)
	if code := codeOf(t, o.consent(t, c, q, "approve", false)); code == "" {
		t.Fatal("no code after the password")
	}
}

func authorizeLead(q url.Values) string { return "/oauth/authorize?" + q.Encode() }

func TestDenyingNeedsNoPasswordAndTellsTheApp(t *testing.T) {
	o := newOAuthSite(t, nil)
	client := o.register(t, appCallback)
	c := o.login(t, "bob")
	res := o.consent(t, c, o.authorizeQuery(client, nil), "deny", false)
	eq(t, "status", res.StatusCode, http.StatusSeeOther)
	to, err := url.Parse(res.Header.Get("Location"))
	must(t, err)
	eq(t, "app", to.Host, "claude.example.net")
	eq(t, "error", to.Query().Get("error"), "access_denied")
	eq(t, "state", to.Query().Get("state"), "xyz")
	eq(t, "no code", to.Query().Get("code"), "")
	gs, err := o.oauth.Grants(context.Background(), 2)
	must(t, err)
	eq(t, "grants after a denial", len(gs), 0)
}

func TestTheConsentFormNeedsTheCSRFTokenAndAKnownDecision(t *testing.T) {
	o := newOAuthSite(t, nil)
	client := o.register(t, appCallback)
	q := o.authorizeQuery(client, nil)
	c := o.login(t, "bob")
	o.sudo(t, c)
	form := url.Values{"decision": {"approve"}}
	for k := range q {
		form.Set(k, q.Get(k))
	}
	res := post(t, c, o.srv.URL+"/oauth/authorize", form)
	eq(t, "no csrf", res.StatusCode, http.StatusForbidden)
	form.Set("csrf", o.token(t, c))
	form.Set("decision", "maybe")
	eq(t, "unknown decision", post(t, c, o.srv.URL+"/oauth/authorize", form).StatusCode, http.StatusBadRequest)
	form.Del("decision")
	eq(t, "no decision", post(t, c, o.srv.URL+"/oauth/authorize", form).StatusCode, http.StatusBadRequest)
}

func TestTheConsentPageLetsTheBrowserFollowTheRedirectToTheAppAlone(t *testing.T) {
	o := newOAuthSite(t, nil)
	c := o.login(t, "bob")
	for redirect, want := range map[string]string{
		"https://claude.example.net/callback": "form-action 'self' https://claude.example.net;",
		"http://127.0.0.1:5000/cb":            "form-action 'self' http://127.0.0.1:*;",
		"http://localhost/cb":                 "form-action 'self' http://localhost:*;",
		"http://[::1]:5000/cb":                "form-action 'self' http://[::1]:*;",
		"https://app.example:8443/cb":         "form-action 'self' https://app.example:8443;",
	} {
		client := o.register(t, redirect)
		q := o.authorizeQuery(client, url.Values{"redirect_uri": {redirect}})
		res, err := c.Get(o.srv.URL + authorizeLead(q))
		must(t, err)
		_ = res.Body.Close()
		eq(t, redirect+" status", res.StatusCode, http.StatusOK)
		if got := res.Header.Get("Content-Security-Policy"); !strings.Contains(got, want) || !strings.Contains(got, "default-src 'self'") {
			t.Errorf("%s: %q", redirect, got)
		}
	}
}

// A policy is never built from an address an app registered beyond the few
// characters a host has.
func TestTheFormActionIsBuiltFromAHostAndNothingElse(t *testing.T) {
	for in, want := range map[string]string{
		"https://claude.example.net/cb":       "'self' https://claude.example.net",
		"https://a.example;script-src */cb":   "'self'",
		"https://a.example'unsafe-inline'/cb": "'self'",
		"https://a.example,b/cb":              "'self'",
		"https://a.example:8443/cb":           "'self' https://a.example:8443",
		"http://127.0.0.1:99/cb":              "'self' http://127.0.0.1:*",
		"https://[2001:db8::1]:443/cb":        "'self' https://[2001:db8::1]:443",
		"https://exa mple.com/cb":             "'self'",
		"%zz":                                 "'self'",
	} {
		eq(t, in, formAction(in), want)
	}
}

func TestARequestTheAppGotWrongIsShownOrAnsweredByWhoseAddressIsRight(t *testing.T) {
	o := newOAuthSite(t, nil)
	client := o.register(t, appCallback)
	c := o.login(t, "bob")
	get := func(extra url.Values) (*http.Response, string) {
		req, err := http.NewRequest(http.MethodGet, o.srv.URL+authorizeLead(o.authorizeQuery(client, extra)), nil)
		must(t, err)
		return do(t, c, req)
	}
	// An address that is not the app's: a page, never a redirect.
	for what, extra := range map[string]url.Values{
		"a strange address": {"redirect_uri": {"https://evil.example/callback"}},
		"no address":        {"redirect_uri": {""}},
		"an unknown client": {"client_id": {"nobody"}},
		"a plain-http one":  {"client_id": {"http://claude.example.net/client.json"}},
	} {
		res, body := get(extra)
		eq(t, what, res.StatusCode, http.StatusBadRequest)
		eq(t, what+": no redirect", res.Header.Get("Location"), "")
		contains(t, body, "This app cannot be connected")
	}
	// The rest go back to the app, with the state.
	for what, extra := range map[string]url.Values{
		"no PKCE":          {"code_challenge": {""}, "code_challenge_method": {""}},
		"plain PKCE":       {"code_challenge_method": {"plain"}},
		"another resource": {"resource": {"https://other.example.com/mcp"}},
		"a wrong scope":    {"scope": {"admin"}},
	} {
		res, _ := get(extra)
		eq(t, what, res.StatusCode, http.StatusSeeOther)
		to, err := url.Parse(res.Header.Get("Location"))
		must(t, err)
		eq(t, what+": host", to.Host, "claude.example.net")
		eq(t, what+": state", to.Query().Get("state"), "xyz")
		if to.Query().Get("error") == "" || to.Query().Get("code") != "" {
			t.Errorf("%s: %s", what, to)
		}
	}
}

func TestAReadOnlyGrantCannotBuildAndAGrantWithBuildStillIsHeldToTheUsersGrants(t *testing.T) {
	o := newOAuthSite(t, nil)
	client := o.register(t, appCallback)
	grant := func(user string, build bool) string {
		c := o.login(t, user)
		o.sudo(t, c)
		return o.exchange(t, client, codeOf(t, o.consent(t, c, o.authorizeQuery(client, nil), "approve", build)))["access_token"].(string)
	}
	readOnly := grant("bob", false)
	res, _ := o.rpc(t, readOnly, "tools/call", toolCall("get_job", "smoke"))
	eq(t, "a read-only grant reads", res.StatusCode, http.StatusOK)
	res, body := o.rpc(t, readOnly, "tools/call", toolCall("run_build", "smoke"))
	eq(t, "run_build "+body, res.StatusCode, http.StatusForbidden)
	want := `Bearer error="insufficient_scope", scope="kipp:build", resource_metadata="` + o.srv.URL + `/.well-known/oauth-protected-resource/mcp"`
	eq(t, "challenge", res.Header.Get("WWW-Authenticate"), want)
	res, _ = o.rpc(t, readOnly, "tools/call", toolCall("cancel_build", "smoke"))
	eq(t, "cancel_build", res.StatusCode, http.StatusForbidden)

	// With build, bob runs what bob may: smoke, not slow (view only).
	withBuild := grant("bob", true)
	res, body = o.rpc(t, withBuild, "tools/call", toolCall("run_build", "smoke"))
	eq(t, "run_build smoke", res.StatusCode, http.StatusOK)
	lacks(t, body, `"isError":true`)
	res, body = o.rpc(t, withBuild, "tools/call", toolCall("run_build", "slow"))
	eq(t, "run_build slow", res.StatusCode, http.StatusOK)
	contains(t, body, `"isError":true`, "not allowed")
	// alice may view smoke only, and builds on none.
	alice := func() string {
		c := o.login(t, "alice")
		o.sudo(t, c)
		return o.exchange(t, client, codeOf(t, o.consent(t, c, o.authorizeQuery(client, nil), "approve", true)))["access_token"].(string)
	}()
	_, body = o.rpc(t, alice, "tools/call", toolCall("run_build", "smoke"))
	contains(t, body, `"isError":true`, "not allowed")
}

func TestAnOAuthTokenIsRefusedOutsideMcpAndAnAPITokenStillWorksOnIt(t *testing.T) {
	o := newOAuthSite(t, nil)
	client := o.register(t, appCallback)
	c := o.login(t, "admin")
	o.sudo(t, c)
	access := o.exchange(t, client, codeOf(t, o.consent(t, c, o.authorizeQuery(client, nil), "approve", true)))["access_token"].(string)

	for _, path := range []string{"/api/v1/jobs", "/api/v1/me", "/api/v1/queue", "/", "/jobs", "/account", "/admin/users"} {
		req, err := http.NewRequest(http.MethodGet, o.srv.URL+path, nil)
		must(t, err)
		req.Header.Set("Authorization", "Bearer "+access)
		res, body := do(t, o.anon(t), req)
		if res.StatusCode == http.StatusOK || strings.Contains(body, "smoke") {
			t.Errorf("%s answered %d to an OAuth token", path, res.StatusCode)
		}
	}
	req, err := http.NewRequest(http.MethodGet, o.srv.URL+"/api/v1/jobs", nil)
	must(t, err)
	req.Header.Set("Authorization", "Bearer "+access)
	res, _ := do(t, o.anon(t), req)
	eq(t, "api with an OAuth token", res.StatusCode, http.StatusUnauthorized)

	// An API token on /mcp, as before.
	plain, hash, err := auth.NewToken()
	must(t, err)
	admin, err := o.st.UserByName(context.Background(), "admin")
	must(t, err)
	_, err = o.st.CreateToken(context.Background(), admin.ID, "ci", hash)
	must(t, err)
	res2, _ := o.rpc(t, plain, "tools/call", toolCall("get_job", "smoke"))
	eq(t, "api token on mcp", res2.StatusCode, http.StatusOK)
	res2, _ = o.rpc(t, "", "tools/list", "{}")
	eq(t, "no token", res2.StatusCode, http.StatusUnauthorized)
	contains(t, res2.Header.Get("WWW-Authenticate"), `resource_metadata="`+o.srv.URL+`/.well-known/oauth-protected-resource/mcp"`)
}

// The wire of a whole sign-in: an access token, its refresh, the rotation,
// the reuse that ends the grant, and the revocation.
func TestRefreshingRotatesTheTokenAndReusingAnOldOneEndsTheGrantOnMcp(t *testing.T) {
	o := newOAuthSite(t, nil)
	client := o.register(t, appCallback)
	c := o.login(t, "bob")
	o.sudo(t, c)
	first := o.exchange(t, client, codeOf(t, o.consent(t, c, o.authorizeQuery(client, nil), "approve", true)))
	refresh := func(token string) (int, map[string]any) {
		res, err := o.srv.Client().PostForm(o.srv.URL+"/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {token}, "client_id": {client}})
		must(t, err)
		defer func() { _ = res.Body.Close() }()
		var m map[string]any
		must(t, json.NewDecoder(res.Body).Decode(&m))
		return res.StatusCode, m
	}
	code, second := refresh(first["refresh_token"].(string))
	eq(t, "refresh", code, http.StatusOK)
	res, _ := o.rpc(t, second["access_token"].(string), "tools/list", "{}")
	eq(t, "new access token", res.StatusCode, http.StatusOK)
	code, _ = refresh(first["refresh_token"].(string))
	eq(t, "the old refresh token", code, http.StatusBadRequest)
	res, _ = o.rpc(t, second["access_token"].(string), "tools/list", "{}")
	eq(t, "after the reuse", res.StatusCode, http.StatusUnauthorized)
	res, _ = o.rpc(t, first["access_token"].(string), "tools/list", "{}")
	eq(t, "the first access token after the reuse", res.StatusCode, http.StatusUnauthorized)
}

func TestAClientNamedByItsDocumentSignsInThroughTheSameFlow(t *testing.T) {
	var doc string
	cimd := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, doc) }))
	t.Cleanup(cimd.Close)
	id := cimd.URL + "/client.json"
	doc = fmt.Sprintf(`{"client_id": %q, "client_name": "Claude", "redirect_uris": [%q], "token_endpoint_auth_method": "none"}`, id, appCallback)
	o := newOAuthSite(t, cimd.Client())
	c := o.login(t, "bob")
	o.sudo(t, c)
	q := o.authorizeQuery(id, nil)
	_, page := get(t, c, o.srv.URL+authorizeLead(q))
	contains(t, page, "Connect Claude")
	m := o.exchange(t, id, codeOf(t, o.consent(t, c, q, "approve", true)))
	res, _ := o.rpc(t, m["access_token"].(string), "tools/list", "{}")
	eq(t, "tools/list", res.StatusCode, http.StatusOK)

	// A document that is not the client's own is no client.
	doc = fmt.Sprintf(`{"client_id": "%s/other.json", "client_name": "Claude", "redirect_uris": [%q]}`, cimd.URL, appCallback)
	other := cimd.URL + "/second.json"
	status, body := get(t, c, o.srv.URL+authorizeLead(o.authorizeQuery(other, nil)))
	eq(t, "a document for another id", status, http.StatusBadRequest)
	contains(t, body, "cannot be connected")
}

func (o *oauthSite) connect(t *testing.T, user string) (*http.Client, string) {
	t.Helper()
	client := o.register(t, appCallback)
	c := o.login(t, user)
	o.sudo(t, c)
	access := o.exchange(t, client, codeOf(t, o.consent(t, c, o.authorizeQuery(client, nil), "approve", true)))["access_token"].(string)
	return c, access
}

func TestTheAccountPageListsConnectedAppsAndRevokesThem(t *testing.T) {
	o := newOAuthSite(t, nil)
	bob, access := o.connect(t, "bob")
	_, page := get(t, bob, o.srv.URL+"/account")
	contains(t, page, "Connected apps", "Claude &lt;b&gt;Code&lt;/b&gt;", "read, and start and cancel builds", "Disconnect", `href="/account/apps/1/revoke"`)
	lacks(t, page, "<b>Code</b>")

	// Another user's grant is not there to revoke.
	alice := o.login(t, "alice")
	status, _ := get(t, alice, o.srv.URL+"/account/apps/1/revoke")
	eq(t, "another user's confirm page", status, http.StatusNotFound)
	res, _ := o.form(t, alice, "/account/apps/1/revoke", url.Values{})
	eq(t, "another user's revoke", res.StatusCode, http.StatusNotFound)
	if r, _ := o.rpc(t, access, "tools/list", "{}"); r.StatusCode != http.StatusOK {
		t.Fatal("alice ended bob's grant")
	}

	status, page = get(t, bob, o.srv.URL+"/account/apps/1/revoke")
	eq(t, "confirm", status, http.StatusOK)
	contains(t, page, "Disconnect", "Claude", `action="/account/apps/1/revoke"`)
	res, _ = o.form(t, bob, "/account/apps/1/revoke", url.Values{})
	eq(t, "revoke", res.StatusCode, http.StatusSeeOther)
	eq(t, "back", res.Header.Get("Location"), "/account?done=app-revoked")
	if r, _ := o.rpc(t, access, "tools/list", "{}"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a disconnected app's token: %d", r.StatusCode)
	}
	_, page = get(t, bob, o.srv.URL+"/account?done=app-revoked")
	contains(t, page, "No app is connected", "The app is disconnected")
}

func TestAnAdminSeesAndRevokesAUsersConnectedApps(t *testing.T) {
	o := newOAuthSite(t, nil)
	_, access := o.connect(t, "bob")
	admin := o.login(t, "admin")
	_, page := get(t, admin, o.srv.URL+"/admin/users/bob")
	contains(t, page, "Connected apps", "Claude &lt;b&gt;Code&lt;/b&gt;", `href="/admin/users/bob/apps/1/revoke"`)
	// A user who is no admin sees nothing of it.
	status, _ := get(t, o.login(t, "alice"), o.srv.URL+"/admin/users/bob/apps/1/revoke")
	eq(t, "alice", status, http.StatusForbidden)
	status, page = get(t, admin, o.srv.URL+"/admin/users/bob/apps/1/revoke")
	eq(t, "confirm", status, http.StatusOK)
	contains(t, page, "Disconnect bob")
	res, _ := o.form(t, admin, "/admin/users/bob/apps/1/revoke", url.Values{})
	eq(t, "revoke", res.StatusCode, http.StatusSeeOther)
	if r, _ := o.rpc(t, access, "tools/list", "{}"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after the admin revoked: %d", r.StatusCode)
	}
	rows, err := o.st.AuditBefore(context.Background(), 0, 20)
	must(t, err)
	found := false
	for _, a := range rows {
		found = found || (a.Action == "oauth.grant.revoke" && a.Actor == "admin" && a.Target == "bob")
	}
	if !found {
		t.Fatalf("the admin's revocation is not in the audit: %+v", rows)
	}
}

func TestANewPasswordAndTheDeletionOfTheUserEndTheirApps(t *testing.T) {
	o := newOAuthSite(t, nil)
	bob, access := o.connect(t, "bob")
	res, _ := o.form(t, bob, "/account/password", url.Values{"current": {"pw"}, "new": {strongPassword}, "again": {strongPassword}})
	eq(t, "password change", res.StatusCode, http.StatusSeeOther)
	if r, _ := o.rpc(t, access, "tools/list", "{}"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after bob changed the password: %d", r.StatusCode)
	}

	_, access = o.connect(t, "alice")
	admin := o.login(t, "admin")
	o.sudo(t, admin)
	res, _ = o.form(t, admin, "/admin/users/alice/password", url.Values{})
	eq(t, "reset", res.StatusCode, http.StatusOK)
	if r, _ := o.rpc(t, access, "tools/list", "{}"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after the reset: %d", r.StatusCode)
	}

	carol, err := o.st.CreateUser(context.Background(), "carol", hashOnce(), false)
	must(t, err)
	_, access = o.connect(t, "carol")
	res, _ = o.form(t, admin, "/admin/users/carol/delete", url.Values{})
	eq(t, "delete", res.StatusCode, http.StatusSeeOther)
	if r, _ := o.rpc(t, access, "tools/list", "{}"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after the user was deleted: %d", r.StatusCode)
	}
	gs, err := o.oauth.Grants(context.Background(), carol.ID)
	must(t, err)
	eq(t, "grants of a deleted user", len(gs), 0)
}

func TestAGrantTheUserLosesTheGrantsBeneathStopsAtOnce(t *testing.T) {
	o := newOAuthSite(t, nil)
	_, access := o.connect(t, "bob")
	_, body := o.rpc(t, access, "tools/call", toolCall("get_job", "smoke"))
	lacks(t, body, `"isError":true`)
	bob, err := o.st.UserByName(context.Background(), "bob")
	must(t, err)
	must(t, o.st.Grant(context.Background(), bob.ID, "smoke", nil))
	_, body = o.rpc(t, access, "tools/call", toolCall("get_job", "smoke"))
	contains(t, body, `"isError":true`, "not found")
}

// Without the option none of the pages is there.
func TestWithoutOAuthTheConsentPageAndTheAppsListAreNotThere(t *testing.T) {
	e := site(t)
	c := e.login(t, "admin")
	for _, p := range []string{"/oauth/authorize", "/account/apps/1/revoke", "/admin/users/bob/apps/1/revoke"} {
		status, _ := get(t, c, e.srv.URL+p)
		eq(t, p, status, http.StatusNotFound)
	}
	_, page := get(t, c, e.srv.URL+"/account")
	lacks(t, page, "Connected apps")
	_, page = get(t, c, e.srv.URL+"/admin/users/bob")
	lacks(t, page, "Connected apps")
}
