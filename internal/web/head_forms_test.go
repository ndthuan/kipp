package web

import (
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
)

// popOf returns the details that a heading's button opens: its opening tag
// and everything up to its closing tag, found by the button's text.
func popOf(t *testing.T, page, button string) (open bool, body string) {
	t.Helper()
	at := strings.Index(page, `">`+button+`</summary>`)
	if at < 0 {
		t.Fatalf("no button %q in:\n%s", button, page)
	}
	from := strings.LastIndex(page[:at], `<details class="build drop"`)
	if from < 0 {
		t.Fatalf("button %q is in no details.build.drop in:\n%s", button, page)
	}
	end := strings.Index(page[at:], "</details>")
	tag, _, _ := strings.Cut(page[from:], ">")
	return strings.HasSuffix(tag, " open"), page[from : at+end]
}

// wantPop holds that a form sits behind a button in the heading's row, the
// button first in its row where main is true, and that it is open or closed.
func wantPop(t *testing.T, page, button string, main, open bool) string {
	t.Helper()
	got, body := popOf(t, page, button)
	eq(t, button+" open", got, open)
	class := "action"
	if main {
		class = "action action-main"
	}
	contains(t, body, `<summary class="`+class+`">`+button+`</summary>`, `<form class="panel stack pop"`)
	head, _, _ := strings.Cut(page, body)
	if !strings.Contains(head[strings.LastIndex(head, `<div class="head">`):], `<div class="head-actions">`) {
		t.Fatalf("%q is not in the heading's row:\n%s", button, page)
	}
	return body
}

func TestThePopsAreAnchoredToTheHeadingsRow(t *testing.T) {
	css, err := os.ReadFile("static/kipp.css")
	must(t, err)
	contains(t, string(css), ".head { position: relative;", ".head .drop { position: static; }")
}

func TestUsersAddAUserFromAButtonInTheHeadingsRow(t *testing.T) {
	e := site(t)
	_, page := get(t, e.login(t, "admin"), e.srv.URL+"/admin/users")
	body := wantPop(t, page, "Add a user", true, false)
	contains(t, body, `action="/admin/users"`, "<h2>Add a user</h2>")
	eq(t, "one form", strings.Count(page, `action="/admin/users"`), 1)
}

func TestSecretsSetASecretFromAButtonThatOpensWhenPrefilledOrRefused(t *testing.T) {
	e := site(t)
	e.hook("smoke", "CHAT_TOKEN")
	admin := e.login(t, "admin")
	e.sudo(t, admin)
	_, page := get(t, admin, e.srv.URL+"/admin/secrets")
	wantPop(t, page, "Set a secret", true, false)
	_, page = get(t, admin, e.srv.URL+"/admin/secrets?name=CHAT_TOKEN")
	body := wantPop(t, page, "Set a secret", true, true)
	contains(t, body, `id="set"`, `name="name" value="CHAT_TOKEN"`)

	res, page := e.form(t, admin, "/admin/secrets", url.Values{"name": {"CHAT_TOKEN"}, "value": {"x"}})
	eq(t, "refused", res.StatusCode, http.StatusBadRequest)
	body = wantPop(t, page, "Set a secret", true, true)
	contains(t, body, `role="alert"`, `name="name" value="CHAT_TOKEN"`)
}

func TestAgentsOpenOnlyTheFormThatWasRefusedWithItsErrorInside(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	e.sudo(t, admin)
	_, page := get(t, admin, e.srv.URL+"/admin/agents")
	wantPop(t, page, "Add an engine", true, false)
	wantPop(t, page, "Add a standing agent", false, false)
	lacks(t, page, `role="alert"`)

	res, page := e.form(t, admin, "/admin/agents/engines", url.Values{"name": {"tcp"}, "url": {"tcp://host:2376"}, "labels": {"linux"}, "max": {"1"}})
	eq(t, "engine refused", res.StatusCode, http.StatusBadRequest)
	body := wantPop(t, page, "Add an engine", true, true)
	contains(t, body, `role="alert"`, "a tcp engine needs")
	body = wantPop(t, page, "Add a standing agent", false, false)
	lacks(t, body, `role="alert"`)

	res, page = e.form(t, admin, "/admin/agents/agents", url.Values{"name": {"box"}, "labels": {"linux"}, "executors": {"no"}})
	eq(t, "agent refused", res.StatusCode, http.StatusBadRequest)
	body = wantPop(t, page, "Add a standing agent", false, true)
	contains(t, body, `role="alert"`, `value="box"`)
	body = wantPop(t, page, "Add an engine", true, false)
	lacks(t, body, `role="alert"`)
	aboveHeading, _, _ := strings.Cut(page, `<div class="head">`)
	lacks(t, aboveHeading, `role="alert"`)
}

func TestAppsMakeAnAppFromAButtonThatOpensWhenRefused(t *testing.T) {
	gh, _ := githubApps(t, http.StatusCreated)
	e := site(t)
	e.appsOn(t, gh, "https://kipp.example")
	admin := e.login(t, "admin")
	e.sudo(t, admin)
	_, page := get(t, admin, e.srv.URL+"/admin/apps")
	wantPop(t, page, "Make an App", true, false)

	res, page, _ := e.startApp(t, admin, "my_app", "")
	eq(t, "refused", res.StatusCode, http.StatusBadRequest)
	body := wantPop(t, page, "Make an App", true, true)
	contains(t, body, `role="alert"`, `value="my_app"`)
}

func TestTheAccountPageOpensItsFormsFromTwoButtonsAndTheRefusedOneOpen(t *testing.T) {
	e := site(t)
	alice := e.login(t, "alice")
	_, page := get(t, alice, e.srv.URL+"/account")
	wantPop(t, page, "Change the password", true, false)
	body := wantPop(t, page, "Make a token", false, false)
	contains(t, body, `action="/account/tokens"`, "<h3>Make a token</h3>")

	res, page := e.form(t, alice, "/account/password", url.Values{"current": {"wrong"}, "new": {strongPassword}, "again": {strongPassword}})
	eq(t, "refused", res.StatusCode, http.StatusUnauthorized)
	body = wantPop(t, page, "Change the password", true, true)
	contains(t, body, "The current password is wrong.")
	wantPop(t, page, "Make a token", false, false)
}

func TestAUsersPageMakesATokenFromAButtonAndKeepsItsOneButtonForms(t *testing.T) {
	e := site(t)
	e.login(t, "alice")
	_, page := get(t, e.login(t, "admin"), e.srv.URL+"/admin/users/alice")
	body := wantPop(t, page, "Make a token", true, false)
	contains(t, body, `action="/admin/users/alice/tokens"`)
	contains(t, page, `action="/admin/users/alice/admin/on"`, `action="/admin/users/alice/grants"`)
	eq(t, "one token form", strings.Count(page, "<h3>Make a token</h3>"), 1)
}
