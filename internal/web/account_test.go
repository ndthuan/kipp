package web

import (
	"context"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/store"
)

// strongPassword is one a person may choose: long enough, on no list,
// without a user's name.
const strongPassword = "an obvious test passphrase, long"

// sudo gives c's session leave to change credentials, as the password
// form does.
func (e *env) sudo(t *testing.T, c *http.Client) {
	t.Helper()
	res := post(t, c, e.srv.URL+"/sudo", url.Values{"csrf": {e.token(t, c)}, "password": {"pw"}, "next": {"/account"}})
	eq(t, "sudo", res.StatusCode, http.StatusSeeOther)
}

// form posts fields with c's CSRF token. A form whose answer shows a
// value once also carries a one-time value from the page it is on, as a
// browser's would, unless fields has one; a page c may not see has none.
func (e *env) form(t *testing.T, c *http.Client, path string, fields url.Values) (*http.Response, string) {
	t.Helper()
	all := url.Values{"csrf": {e.token(t, c)}}
	if from := oneTimeFormPage(path); from != "" && fields.Get("once") == "" {
		if _, page := get(t, c, e.srv.URL+from); strings.Contains(page, `name="once"`) {
			all.Set("once", oneTimeOf(t, page))
		}
	}
	maps.Copy(all, fields)
	return postBody(t, c, e.srv.URL+path, all)
}

// oneTimeFormPage is the page the form posted to path is on when its
// answer shows a value once, and "" for any other form.
func oneTimeFormPage(path string) string {
	switch {
	case path == "/account/tokens" || path == "/admin/users":
		return strings.TrimSuffix(path, "/tokens")
	case strings.HasPrefix(path, "/admin/users/") && strings.HasSuffix(path, "/tokens"):
		return strings.TrimSuffix(path, "/tokens")
	case strings.HasPrefix(path, "/admin/users/") && strings.HasSuffix(path, "/password"):
		return path
	case path == "/admin/runners/runners":
		return "/admin/runners"
	case strings.HasPrefix(path, "/admin/runners/runners/") && strings.HasSuffix(path, "/token"):
		return path
	}
	return ""
}

// oneTimeOf is the one-time value of the form on page.
func oneTimeOf(t *testing.T, page string) string {
	t.Helper()
	_, rest, ok := strings.Cut(page, `name="once" value="`)
	if !ok {
		t.Fatalf("no one-time value in:\n%s", page)
	}
	v, _, _ := strings.Cut(rest, `"`)
	return v
}

func (e *env) user(t *testing.T, name string) *store.User {
	t.Helper()
	u, err := e.st.UserByName(context.Background(), name)
	must(t, err)
	return u
}

func (e *env) tokens(t *testing.T, name string) []*store.Token {
	t.Helper()
	tokens, err := e.st.Tokens(context.Background(), e.user(t, name).ID)
	must(t, err)
	return tokens
}

// onceValue is the value a page shows once.
func onceValue(t *testing.T, page string) string {
	t.Helper()
	_, rest, ok := strings.Cut(page, `<code id="once-value">`)
	if !ok {
		t.Fatalf("no value shown once in:\n%s", page)
	}
	v, _, _ := strings.Cut(rest, "</code>")
	return v
}

// A form whose answer shows a token or a password once makes it once: sent
// again, by a reload or the back button, it is answered that it was sent
// already, and nothing is made or shown a second time.
func TestAFormThatShowsAValueOnceMakesItOnceAndSentAgainSaysItWasSent(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	e.sudo(t, admin)
	count := map[string]func() int{
		"tokens of admin": func() int { return len(e.tokens(t, "admin")) },
		"users":           func() int { us, err := e.st.Users(context.Background()); must(t, err); return len(us) },
		"tokens of bob":   func() int { return len(e.tokens(t, "bob")) },
		"bob's hash":      func() int { return len(e.user(t, "bob").Hash) + int(e.user(t, "bob").DeviceEpoch) },
	}
	for _, c := range []struct {
		page, action, counted string
		fields                url.Values
	}{
		{"/account", "/account/tokens", "tokens of admin", url.Values{"label": {"ci"}, "days": {"30"}}},
		{"/admin/users", "/admin/users", "users", url.Values{"name": {"carol"}}},
		{"/admin/users/bob", "/admin/users/bob/tokens", "tokens of bob", url.Values{"label": {"runner"}, "days": {"30"}}},
		{"/admin/users/bob/password", "/admin/users/bob/password", "bob's hash", url.Values{}},
	} {
		_, page := get(t, admin, e.srv.URL+c.page)
		c.fields.Set("once", oneTimeOf(t, page))
		res, page := e.form(t, admin, c.action, c.fields)
		eq(t, c.action+" sent", res.StatusCode, http.StatusOK)
		onceValue(t, page)
		before := count[c.counted]()
		res, page = e.form(t, admin, c.action, c.fields)
		eq(t, c.action+" sent again", res.StatusCode, http.StatusConflict)
		contains(t, page, "This form was sent already")
		lacks(t, page, `id="once-value"`)
		eq(t, c.counted+" after "+c.action+" sent again", count[c.counted](), before)
	}
}

// A form refused for what it holds made nothing: its one-time value is
// good for sending it again, corrected.
func TestAFormRefusedForItsFieldsMaySendItsOneTimeValueAgain(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	e.sudo(t, admin)
	_, page := get(t, admin, e.srv.URL+"/account")
	once := oneTimeOf(t, page)
	res, _ := e.form(t, admin, "/account/tokens", url.Values{"label": {"ci"}, "days": {"7"}, "once": {once}})
	eq(t, "a refused form", res.StatusCode, http.StatusBadRequest)
	res, _ = e.form(t, admin, "/account/tokens", url.Values{"label": {"ci"}, "days": {"30"}, "once": {once}})
	eq(t, "sent again, corrected", res.StatusCode, http.StatusOK)
	res, page = e.form(t, admin, "/account/tokens", url.Values{"label": {"ci"}, "days": {"30"}, "once": {"made-up"}})
	eq(t, "a one-time value never given", res.StatusCode, http.StatusConflict)
	contains(t, page, "This form was sent already")
}

func TestTheAccountPageShowsTheUsersOwnTokensAndHowManyOtherSessionsTheyHave(t *testing.T) {
	e := site(t)
	alice := e.login(t, "alice")
	code, page := get(t, alice, e.srv.URL+"/account")
	eq(t, "account", code, http.StatusOK)
	contains(t, page, "<h1>Account", "laptop-token", `href="/account/tokens/`, `action="/account/password"`,
		`autocomplete="current-password"`, `autocomplete="new-password"`, `action="/account/tokens"`, "You are logged in here only.")
	lacks(t, page, "hash-of-the-token")
	selfContained(t, page)
	for _, days := range api.TokenDays {
		contains(t, page, `<option value="`+strconv.Itoa(days)+`"`)
	}

	e.login(t, "alice")
	e.login(t, "alice")
	_, page = get(t, alice, e.srv.URL+"/account")
	contains(t, page, "2 other sessions", `action="/account/sessions/end"`)
	_, page = get(t, e.login(t, "bob"), e.srv.URL+"/account")
	lacks(t, page, "laptop-token")
}

// A change of credentials from a session that has not given its password
// lately is answered with the form that asks for it, saying what the user
// was doing; the password leads back to the page, and the change is made
// when it is sent again.
func TestAChangeOfCredentialsAsksForThePasswordFirstAndThenLeadsBackToItsPage(t *testing.T) {
	e := site(t)
	alice := e.login(t, "alice")
	res, page := e.form(t, alice, "/account/tokens", url.Values{"label": {"ci"}, "days": {"30"}})
	eq(t, "a token without sudo", res.StatusCode, http.StatusForbidden)
	contains(t, page, "Confirm your password", "make a token", `action="/sudo"`, `name="next" value="/account"`, `name="csrf"`, `autocomplete="current-password"`)
	selfContained(t, page)
	eq(t, "tokens made", len(e.tokens(t, "alice")), 1)

	res, page = e.form(t, alice, "/sudo", url.Values{"password": {"wrong"}, "next": {"/account"}, "action": {"POST /account/tokens"}})
	eq(t, "a wrong password", res.StatusCode, http.StatusUnauthorized)
	contains(t, page, "Wrong password.", "make a token", `name="next" value="/account"`)

	res = post(t, alice, e.srv.URL+"/sudo", url.Values{"csrf": {e.token(t, alice)}, "password": {"pw"}, "next": {"/account"}})
	eq(t, "the right password", res.StatusCode, http.StatusSeeOther)
	eq(t, "back to", res.Header.Get("Location"), "/account")

	res, page = e.form(t, alice, "/account/tokens", url.Values{"label": {"ci"}, "days": {"30"}})
	eq(t, "the token, sent again", res.StatusCode, http.StatusOK)
	eq(t, "tokens made", len(e.tokens(t, "alice")), 2)
	contains(t, page, "Your new token", `data-copy="once-value"`, "/static/copy.js?v=", "only time it is shown", "It expires on ")
	selfContained(t, page)

	// The token is real, and is never shown again.
	plain := onceValue(t, page)
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer "+plain)
	res, _ = do(t, e.anon(t), req)
	eq(t, "the new token on the API", res.StatusCode, http.StatusOK)
	_, page = get(t, alice, e.srv.URL+"/account")
	lacks(t, page, plain)
	contains(t, page, ">ci</td>")
}

func TestThePasswordLeadsOnlyToAPathOnThisSite(t *testing.T) {
	e := site(t)
	c := e.login(t, "alice")
	res := post(t, c, e.srv.URL+"/sudo", url.Values{"csrf": {e.token(t, c)}, "password": {"pw"}, "next": {"https://evil.example/account"}})
	eq(t, "sudo", res.StatusCode, http.StatusSeeOther)
	eq(t, "led to", res.Header.Get("Location"), "/")
}

func TestWrongPasswordsForSudoAreThrottledAsALoginIs(t *testing.T) {
	// A frozen clock: the wait the page names is that of the sixth try
	// made the instant after the fifth, however long the machine takes.
	frozen := time.Now()
	e := site(t, func(d *api.Deps) { d.Auth.SetThrottleClock(func() time.Time { return frozen }) })
	c := e.login(t, "alice")
	for range 5 {
		res, _ := e.form(t, c, "/sudo", url.Values{"password": {"wrong"}, "next": {"/account"}})
		eq(t, "a wrong password", res.StatusCode, http.StatusUnauthorized)
	}
	res, page := e.form(t, c, "/sudo", url.Values{"password": {"pw"}, "next": {"/account"}})
	eq(t, "throttled", res.StatusCode, http.StatusTooManyRequests)
	eq(t, "Retry-After", res.Header.Get("Retry-After"), "2")
	contains(t, page, "Too many wrong passwords for this name.", `action="/sudo"`)
}

func TestAPasswordChangeReplacesThisSessionAndEndsTheOthers(t *testing.T) {
	e := site(t)
	alice, elsewhere := e.login(t, "alice"), e.login(t, "alice")
	u, err := url.Parse(e.srv.URL)
	must(t, err)
	before := cookieNamed(t, alice, u, api.SessionCookie).Value

	res, _ := e.form(t, alice, "/account/password", url.Values{"current": {"pw"}, "new": {strongPassword}, "again": {strongPassword}})
	eq(t, "change", res.StatusCode, http.StatusSeeOther)
	eq(t, "led to", res.Header.Get("Location"), "/account?done=password")
	var set *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == api.SessionCookie {
			set = c
		}
	}
	if set == nil || set.Value == before || !set.Secure || !set.HttpOnly || cookieNamed(t, alice, u, api.SessionCookie).Value != set.Value {
		t.Fatalf("the session cookie after a change: %+v", set)
	}
	code, page := get(t, alice, e.srv.URL+"/account?done=password")
	eq(t, "this browser", code, http.StatusOK)
	contains(t, page, "Your password is changed.")
	code, _ = get(t, elsewhere, e.srv.URL+"/")
	eq(t, "another session", code, http.StatusSeeOther)
	res = post(t, e.anon(t), e.srv.URL+"/login", url.Values{"name": {"alice"}, "password": {strongPassword}})
	eq(t, "a login with the new password", res.StatusCode, http.StatusSeeOther)
}

func TestARefusedPasswordChangeSaysWhy(t *testing.T) {
	e := site(t)
	alice := e.login(t, "alice")
	for _, c := range []struct {
		current, next, again string
		code                 int
		says                 string
	}{
		{"wrong", strongPassword, strongPassword, http.StatusUnauthorized, "The current password is wrong."},
		{"pw", strongPassword, strongPassword + "x", http.StatusBadRequest, "the two new passwords differ"},
		{"pw", "short", "short", http.StatusBadRequest, "15"},
	} {
		res, page := e.form(t, alice, "/account/password", url.Values{"current": {c.current}, "new": {c.next}, "again": {c.again}})
		eq(t, c.says, res.StatusCode, c.code)
		contains(t, page, c.says, `action="/account/password"`, "laptop-token")
		lacks(t, page, `value="`+c.next)
	}
	res := post(t, e.anon(t), e.srv.URL+"/login", url.Values{"name": {"alice"}, "password": {"pw"}})
	eq(t, "the old password still", res.StatusCode, http.StatusSeeOther)
}

// Revoking is asked about on a page of its own, whose form sends it; the
// password, when it is asked for, leads back to that page.
func TestRevokingOnesOwnTokenIsConfirmedOnAPageOfItsOwn(t *testing.T) {
	e := site(t)
	alice := e.login(t, "alice")
	id := strconv.FormatInt(e.tokens(t, "alice")[0].ID, 10)
	path := "/account/tokens/" + id + "/revoke"
	code, page := get(t, alice, e.srv.URL+path)
	eq(t, "confirm", code, http.StatusOK)
	contains(t, page, `Revoke the token &#34;laptop-token&#34;?`, `method="post" action="`+path+`"`, `name="csrf"`, `href="/account">Cancel`)
	lacks(t, page, "confirm(")

	res, page := e.form(t, alice, path, nil)
	eq(t, "without sudo", res.StatusCode, http.StatusForbidden)
	contains(t, page, "revoke a token", `name="next" value="`+path+`"`)
	e.sudo(t, alice)
	res, _ = e.form(t, alice, path, nil)
	eq(t, "revoke", res.StatusCode, http.StatusSeeOther)
	eq(t, "led to", res.Header.Get("Location"), "/account?done=revoked")
	eq(t, "tokens left", len(e.tokens(t, "alice")), 0)

	// Another user's token is not one of yours, to see or to revoke.
	bob := e.login(t, "bob")
	_, hash, err := auth.NewToken()
	must(t, err)
	theirs, err := e.st.CreateToken(context.Background(), e.user(t, "admin").ID, "admins", hash)
	must(t, err)
	other := "/account/tokens/" + strconv.FormatInt(theirs.ID, 10) + "/revoke"
	code, _ = get(t, bob, e.srv.URL+other)
	eq(t, "another's token", code, http.StatusNotFound)
	e.sudo(t, bob)
	res, _ = e.form(t, bob, other, nil)
	eq(t, "another's token, revoked", res.StatusCode, http.StatusNotFound)
	eq(t, "the admin's tokens", len(e.tokens(t, "admin")), 1)
}

func TestEndingTheOtherSessionsKeepsThisOne(t *testing.T) {
	e := site(t)
	alice, elsewhere := e.login(t, "alice"), e.login(t, "alice")
	res, _ := e.form(t, alice, "/account/sessions/end", nil)
	eq(t, "end", res.StatusCode, http.StatusSeeOther)
	eq(t, "led to", res.Header.Get("Location"), "/account?done=sessions")
	code, page := get(t, alice, e.srv.URL+"/account?done=sessions")
	eq(t, "this one", code, http.StatusOK)
	contains(t, page, "Every other session of yours has ended.", "You are logged in here only.")
	code, _ = get(t, elsewhere, e.srv.URL+"/account")
	eq(t, "the other", code, http.StatusSeeOther)
}

// A notice is picked by a word in the address, never written there.
func TestANoticeIsOneTheSiteWroteAndNeverWhatTheAddressSays(t *testing.T) {
	e := site(t)
	_, page := get(t, e.login(t, "alice"), e.srv.URL+"/account?done=<b>owned</b>")
	lacks(t, page, "owned", `role="status">`)
}

func TestATokenIsNoSessionForTheAccountAndAdminPages(t *testing.T) {
	e := site(t)
	plain, hash, err := auth.NewToken()
	must(t, err)
	_, err = e.st.CreateToken(context.Background(), e.user(t, "admin").ID, "ci", hash)
	must(t, err)
	for _, c := range []struct{ method, path string }{
		{"GET", "/account"}, {"GET", "/admin/users"}, {"GET", "/admin/secrets"}, {"GET", "/admin/server"},
		{"POST", "/sudo"}, {"POST", "/account/tokens"}, {"POST", "/admin/users"}, {"POST", "/admin/secrets"},
	} {
		req, _ := http.NewRequest(c.method, e.srv.URL+c.path, strings.NewReader("password=pw&label=x&days=0&name=eve&value=12345678"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Authorization", "Bearer "+plain)
		res, _ := do(t, e.anon(t), req)
		eq(t, c.method+" "+c.path+" with a token", res.StatusCode, http.StatusSeeOther)
		if !strings.HasPrefix(res.Header.Get("Location"), "/login") {
			t.Fatalf("%s %s with a token led to %q", c.method, c.path, res.Header.Get("Location"))
		}
	}
	eq(t, "admin's tokens", len(e.tokens(t, "admin")), 1)
}

func TestEveryNewFormNeedsTheCSRFField(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	e.sudo(t, admin)
	id := strconv.FormatInt(e.tokens(t, "alice")[0].ID, 10)
	for _, path := range []string{
		"/sudo", "/account/password", "/account/tokens", "/account/tokens/" + id + "/revoke", "/account/sessions/end",
		"/admin/users", "/admin/users/bob/admin/on", "/admin/users/bob/admin/off", "/admin/users/bob/password",
		"/admin/users/bob/grants", "/admin/users/bob/grants/remove", "/admin/users/bob/tokens",
		"/admin/users/alice/tokens/" + id + "/revoke", "/admin/users/bob/delete",
		"/admin/secrets", "/admin/secrets/DEPLOY_KEY/delete", "/admin/server/reload", "/admin/server/quiet", "/admin/server/quiet/off", "/admin/server/quiet/rerun",
	} {
		res, page := postBody(t, admin, e.srv.URL+path, url.Values{"password": {"pw"}, "name": {"eve"}, "label": {"x"}, "days": {"0"}, "value": {"12345678"}})
		eq(t, path+" without the field", res.StatusCode, http.StatusForbidden)
		contains(t, page, "This form is out of date")
	}
	if _, err := e.st.UserByName(context.Background(), "bob"); err != nil {
		t.Fatal("bob was deleted without the field")
	}
}

// A password manager, and a screen reader, learn whose password a form
// asks for from a username field: the login form's own, kept after a
// refusal, and a hidden one with the user's name where the page knows it.
func TestEveryPasswordFormNamesItsUser(t *testing.T) {
	e := site(t)
	_, page := get(t, e.anon(t), e.srv.URL+"/login")
	contains(t, page, `name="name" value="" autocomplete="username"`, `autocomplete="current-password"`)
	_, page = postBody(t, e.anon(t), e.srv.URL+"/login", url.Values{"name": {"alice"}, "password": {"wrong"}})
	contains(t, page, `name="name" value="alice" autocomplete="username"`)

	alice := e.login(t, "alice")
	_, page = get(t, alice, e.srv.URL+"/account")
	contains(t, page, `name="name" value="alice" autocomplete="username" hidden`, `autocomplete="current-password"`, `autocomplete="new-password"`)
	_, page = e.form(t, alice, "/account/tokens", url.Values{"label": {"ci"}, "days": {"30"}})
	contains(t, page, `action="/sudo"`, `name="name" value="alice" autocomplete="username" hidden`, `autocomplete="current-password"`)
}
