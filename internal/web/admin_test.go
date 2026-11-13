package web

import (
	"bytes"
	"context"
	"maps"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

// serverInfo is what the test server says it runs with, but for when it
// started: 90 minutes before it is asked (serverInfoNow), so the uptime
// reads the same however long the tests have run.
var serverInfo = api.ServerInfo{
	Version: "t", GoVersion: "go-test", Schema: 7,
	Listen: "127.0.0.1:8085", BaseURL: "https://ci.example/", Executors: 3, Shell: "/bin/sh",
	SessionTTL: 720 * time.Hour, TimeZone: "UTC", DataDir: "/srv/kipp/data", JobsDir: "/srv/kipp/jobs",
	FreeDisk: 3 << 30, FreeDiskKnown: true, DBSize: 5 << 20, LastReloadAt: time.Now(),
}

func serverInfoNow() api.ServerInfo {
	info := serverInfo
	info.StartedAt = time.Now().Add(-90 * time.Minute)
	return info
}

// hook gives the loaded job name a webhook trigger and secrets, as a
// reload of its file with them would.
func (e *env) hook(name string, secrets ...string) {
	snap := e.d.Snapshot()
	jobs := maps.Clone(snap.Jobs)
	job := *jobs[name]
	job.Triggers.Webhook = &config.Webhook{Repo: "acme/site"}
	job.Secrets = secrets
	jobs[name] = &job
	e.d.SetSnapshot(&config.Snapshot{Jobs: jobs, Names: snap.Names})
}

func (e *env) perms(t *testing.T, name string) map[string]store.Perm {
	t.Helper()
	p, err := e.st.Permissions(context.Background(), e.user(t, name).ID)
	must(t, err)
	return p
}

func (e *env) audit(t *testing.T) string {
	t.Helper()
	rows, err := e.st.AuditBefore(context.Background(), 0, 100)
	must(t, err)
	var out []string
	for _, a := range rows {
		out = append(out, a.Actor+" "+a.Action+" "+a.Target+" "+a.Detail)
	}
	return strings.Join(out, "\n")
}

func TestAdminLeadsToTheUsers(t *testing.T) {
	e := site(t)
	res, _ := do(t, e.login(t, "admin"), mustGet(t, e.srv.URL+"/admin"))
	eq(t, "admin", res.StatusCode, http.StatusSeeOther)
	eq(t, "led to", res.Header.Get("Location"), "/admin/users")
}

// A user who is not an admin is refused every admin page and change, in
// sudo mode or not, and is never asked for the password first.
func TestTheAdminPagesAndChangesAreForAnAdminOnly(t *testing.T) {
	e := site(t)
	bob := e.login(t, "bob")
	id := strconv.FormatInt(e.tokens(t, "alice")[0].ID, 10)
	gets := []string{"/admin/users", "/admin/users/alice", "/admin/users/alice/admin/off", "/admin/users/alice/password",
		"/admin/users/alice/tokens/" + id + "/revoke", "/admin/users/alice/delete", "/admin/secrets", "/admin/secrets/DEPLOY_KEY/delete", "/admin/server"}
	posts := []string{"/admin/users", "/admin/users/alice/admin/on", "/admin/users/bob/admin/on", "/admin/users/alice/password",
		"/admin/users/alice/grants", "/admin/users/alice/tokens", "/admin/users/alice/tokens/" + id + "/revoke",
		"/admin/users/alice/delete", "/admin/secrets", "/admin/secrets/DEPLOY_KEY/delete", "/admin/server/reload", "/admin/server/quiet", "/admin/server/quiet/off", "/admin/server/quiet/rerun"}
	check := func(when string) {
		for _, path := range gets {
			code, page := get(t, bob, e.srv.URL+path)
			eq(t, when+" GET "+path, code, http.StatusForbidden)
			contains(t, page, "You are not allowed to do that.")
			lacks(t, page, "laptop-token", "Confirm your password")
		}
		for _, path := range posts {
			res, page := e.form(t, bob, path, url.Values{"name": {"eve"}, "admin": {"on"}, "label": {"x"}, "days": {"0"}, "value": {"12345678"}, "build": {"slow"}})
			eq(t, when+" POST "+path, res.StatusCode, http.StatusForbidden)
			lacks(t, page, "Confirm your password")
		}
	}
	check("before sudo")
	e.sudo(t, bob)
	check("in sudo")
	if e.user(t, "bob").Admin || len(e.tokens(t, "alice")) != 1 {
		t.Fatal("bob changed something")
	}
	if _, err := e.st.UserByName(context.Background(), "eve"); err == nil {
		t.Fatal("bob added a user")
	}
}

// The header's user menu is the way to the account and, for an admin, to
// the admin pages; it is a menu as the groups are, at the top right.
func TestTheUserMenuLeadsToTheAccountAndForAnAdminToTheAdminPages(t *testing.T) {
	e := site(t)
	_, page := get(t, e.login(t, "admin"), e.srv.URL+"/jobs/smoke")
	contains(t, page, `<details class="user drop">`+"\n"+`<summary aria-label="admin" title="admin"><svg class="icon icon-user"`,
		`<span class="who-name">admin</span></summary>`+"\n"+`<ul class="menu">`+"\n"+`<li class="menu-who">admin</li>`,
		`<a href="/account">Account</a>`, `<a href="/admin/users">Users</a>`, `<a href="/admin/secrets">Secrets</a>`, `<a href="/admin/server">Server</a>`,
		`<form method="post" action="/logout">`, `<button class="menu-button">Log out</button>`)
	lacks(t, page, `href="/admin">`)
	idx := func(s string) int { return strings.Index(page, s) }
	if queue, menu, theme := idx(`id="queue-badge"`), idx(`class="user drop"`), idx(`class="theme drop"`); queue > menu || menu > theme {
		t.Fatal("the user menu is not between the queue and the theme picker")
	}

	_, page = get(t, e.login(t, "bob"), e.srv.URL+"/admin/server")
	contains(t, page, `<span class="who-name">bob</span>`, `<a href="/account">Account</a>`, "Log out")
	lacks(t, page, `href="/admin/`)

	// The page the menu leads to is marked in it, and the menu with it.
	_, page = get(t, e.login(t, "admin"), e.srv.URL+"/admin/secrets")
	contains(t, page, `<summary aria-current="true" aria-label="admin"`, `<a href="/admin/secrets" aria-current="page">Secrets</a>`)
}

// Metrics is a page for everyone who may see a job, so it is in the bar
// beside Jobs; the menu holds the person's own page, and for an admin an
// "Admin" group of the server's, in this order, then Log out after a rule.
func TestTheUserMenuGroupsTheAdminPagesAndMetricsIsInTheBar(t *testing.T) {
	e := site(t)
	order := func(page string, subs ...string) {
		t.Helper()
		last := -1
		for _, s := range subs {
			i := strings.Index(page, s)
			if i < 0 || i <= last {
				t.Fatalf("%q is missing or out of order in the header", s)
			}
			last = i
		}
	}

	_, page := get(t, e.login(t, "bob"), e.srv.URL+"/jobs")
	order(page, `<a class="navlink" href="/jobs"`, `<a class="navlink" href="/metrics">Metrics</a>`, `<li class="menu-who">bob</li>`,
		`<a href="/account">Account</a>`, `<li class="menu-out"><form method="post" action="/logout">`)
	lacks(t, page, `>Admin<`, `menu-group`, `<li><a href="/metrics"`)

	_, page = get(t, e.login(t, "admin"), e.srv.URL+"/jobs")
	order(page, `<li class="menu-who">admin</li>`, `<a href="/account">Account</a>`,
		`<li class="menu-who menu-label" id="menu-admin">Admin</li>`, `<ul class="menu-group" aria-labelledby="menu-admin">`,
		`>Users</a>`, `>Secrets</a>`, `>GitHub Apps</a>`, `>Runners</a>`, `>Cache</a>`, `>Server</a>`, `<li class="menu-out">`)

	// aria-current is on the one page, and the user menu is current on its own pages only.
	admin := e.login(t, "admin")
	for _, path := range []string{"/account", "/admin/users", "/admin/secrets", "/admin/apps", "/admin/runners", "/admin/cache", "/admin/server"} {
		_, page = get(t, admin, e.srv.URL+path)
		contains(t, page, `<a href="`+path+`" aria-current="page">`, `<summary aria-current="true" aria-label="admin"`)
		lacks(t, page, `<a class="navlink" href="/metrics" aria-current`, `<a class="navlink" href="/jobs" aria-current`)
	}
	_, page = get(t, admin, e.srv.URL+"/metrics")
	contains(t, page, `<a class="navlink" href="/metrics" aria-current="page">Metrics</a>`)
	lacks(t, page, `<summary aria-current="true" aria-label="admin"`, `<a class="navlink" href="/jobs" aria-current`)
}

func TestTheUsersPageListsEveryUserAndAddsOneWithAPasswordShownOnce(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	code, page := get(t, admin, e.srv.URL+"/admin/users")
	eq(t, "users", code, http.StatusOK)
	contains(t, page, `href="/admin/users/bob">bob</a>`, `href="/admin/users/admin">admin</a> <span class="tag">admin</span>`,
		`action="/admin/users"`, `name="admin"`, "every job, as an admin")
	lacks(t, page, "hash-of-the-token", "argon2")
	selfContained(t, page)

	res, page := e.form(t, admin, "/admin/users", url.Values{"name": {"carol"}})
	eq(t, "add without sudo", res.StatusCode, http.StatusForbidden)
	contains(t, page, "add a user", `name="next" value="/admin/users"`)
	e.sudo(t, admin)
	res, page = e.form(t, admin, "/admin/users", url.Values{"name": {"carol"}, "admin": {"on"}})
	eq(t, "add", res.StatusCode, http.StatusOK)
	eq(t, "no redirect", res.Header.Get("Location"), "")
	contains(t, page, "Password for carol", `href="/admin/users/carol"`, "only time it is shown")
	pw := onceValue(t, page)
	if !e.user(t, "carol").Admin {
		t.Fatal("carol is not an admin")
	}
	res = post(t, e.anon(t), e.srv.URL+"/login", url.Values{"name": {"carol"}, "password": {pw}})
	eq(t, "carol logs in with it", res.StatusCode, http.StatusSeeOther)
	for _, path := range []string{"/admin/users", "/admin/users/carol", "/admin/server"} {
		_, page = get(t, admin, e.srv.URL+path)
		lacks(t, page, pw)
	}
	lacks(t, e.audit(t), pw)
	contains(t, e.audit(t), "admin user.add carol admin")

	res, page = e.form(t, admin, "/admin/users", url.Values{"name": {"Not A Name"}})
	eq(t, "a name outside the rule", res.StatusCode, http.StatusBadRequest)
	contains(t, page, "is not a name a user can have")
	lacks(t, page, "bad input")
	res, _ = e.form(t, admin, "/admin/users", url.Values{"name": {"bob"}})
	eq(t, "a name taken", res.StatusCode, http.StatusConflict)
}

func TestAUsersPageSetsTheirGrantsOnTheLoadedJobsAndListsTheOnesOnGoneJobsApart(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	must(t, e.st.Grant(context.Background(), e.user(t, "bob").ID, "gone-job", []string{"view", "build"}))
	code, page := get(t, admin, e.srv.URL+"/admin/users/bob")
	eq(t, "bob's page", code, http.StatusOK)
	selfContained(t, page)
	contains(t, page, `action="/admin/users/bob/grants"`, `scope="rowgroup">backend</th>`, `scope="rowgroup">No group</th>`,
		`name="build" value="smoke" aria-label="Build smoke" checked>`, `name="view" value="slow" aria-label="View slow" checked>`,
		`name="view" value="ask" aria-label="View ask">`, "On jobs no longer in the job files", "<code>gone-job</code>: view, build",
		`action="/admin/users/bob/grants/remove"`, `name="job" value="gone-job"`)
	if strings.Index(page, `value="smoke"`) > strings.Index(page, `value="ask"`) {
		t.Fatal("the groups are not in the job list's order")
	}

	e.sudo(t, admin)
	res, _ := e.form(t, admin, "/admin/users/bob/grants", url.Values{"view": {"ask", "smoke"}, "build": {"ask"}, "cancel": {"ask"}})
	eq(t, "save", res.StatusCode, http.StatusSeeOther)
	eq(t, "led to", res.Header.Get("Location"), "/admin/users/bob?done=grants")
	got := e.perms(t, "bob")
	want := map[string]store.Perm{"ask": {View: true, Build: true, Cancel: true}, "smoke": {View: true}, "gone-job": {View: true, Build: true}}
	if !maps.Equal(got, want) {
		t.Fatalf("grants: got %v, want %v", got, want)
	}
	_, page = get(t, admin, e.srv.URL+"/admin/users/bob?done=grants")
	contains(t, page, "The grants are saved.")

	res, _ = e.form(t, admin, "/admin/users/bob/grants/remove", url.Values{"job": {"gone-job"}})
	eq(t, "remove", res.StatusCode, http.StatusSeeOther)
	if _, ok := e.perms(t, "bob")["gone-job"]; ok {
		t.Fatal("the grant on the gone job is still there")
	}
	res, page = e.form(t, admin, "/admin/users/bob/grants", url.Values{"view": {"no-such-job"}})
	eq(t, "a job that is not loaded", res.StatusCode, http.StatusBadRequest)
	contains(t, page, `no job called &#34;no-such-job&#34; is loaded`)
}

func TestAUsersPageOrdersAdminTokensGrantsThenTheDangerousActions(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	order := func(page string) {
		t.Helper()
		last := -1
		for _, id := range []string{`id="admin-heading"`, `id="tokens-heading"`, `id="grants-heading"`, `id="danger-heading"`} {
			at := strings.Index(page, id)
			if at < 0 || at < last {
				t.Fatalf("%s is missing or out of order in:\n%s", id, page)
			}
			last = at
		}
	}
	_, page := get(t, admin, e.srv.URL+"/admin/users/bob")
	order(page)
	lacks(t, page, `<details class="fold">`)

	must(t, e.st.Grant(context.Background(), e.user(t, "bob").ID, "gone-job", []string{"view"}))
	e.sudo(t, admin)
	res, _ := e.form(t, admin, "/admin/users/bob/admin/on", nil)
	eq(t, "on", res.StatusCode, http.StatusSeeOther)
	_, page = get(t, admin, e.srv.URL+"/admin/users/bob")
	order(page)
	fold := strings.Index(page, `<details class="fold">`)
	form := strings.Index(page, `action="/admin/users/bob/grants"`)
	end := fold + strings.Index(page[fold:], "</details>")
	gone := strings.Index(page, "On jobs no longer in the job files")
	if fold < 0 || form < fold || end < form || gone < end {
		t.Fatalf("the grid is not alone in the fold (fold %d, form %d, end %d, gone %d)", fold, form, end, gone)
	}
	contains(t, page, "<summary>Show the grants</summary>", "these grants count only once admin is taken away")
	if strings.Index(page, "these grants count") > fold {
		t.Fatal("the sentence is not above the fold")
	}
	lacks(t, page, `<details class="fold" open`)
}

// A job the filter hides keeps its boxes, and the browser sends them with
// the rest of the form, so what is saved is the ticked state of every row.
func TestAGrantsFormWithRowsTheFilterHidesSavesExactlyWhatIsTicked(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	_, page := get(t, admin, e.srv.URL+"/admin/users/bob")
	contains(t, page, `<tr data-job="ask">`, `<tr data-job="smoke">`, `<tr data-job="slow">`, `<th colspan="4" data-group scope="rowgroup">`)
	e.sudo(t, admin)
	res, _ := e.form(t, admin, "/admin/users/bob/grants", url.Values{"view": {"ask", "smoke", "slow"}, "build": {"smoke", "slow"}})
	eq(t, "saved", res.StatusCode, http.StatusSeeOther)
	if got, want := e.perms(t, "bob"), map[string]store.Perm{"ask": {View: true}, "smoke": {View: true, Build: true}, "slow": {View: true, Build: true}}; !maps.Equal(got, want) {
		t.Fatalf("grants: got %v, want %v", got, want)
	}
	res, _ = e.form(t, admin, "/admin/users/bob/grants", url.Values{"view": {"ask"}})
	eq(t, "saved", res.StatusCode, http.StatusSeeOther)
	if got, want := e.perms(t, "bob"), map[string]store.Perm{"ask": {View: true}}; !maps.Equal(got, want) {
		t.Fatalf("grants: got %v, want %v", got, want)
	}
}

func TestAdminIsGivenAtOnceAndTakenAwayAfterAPageThatAsks(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	e.sudo(t, admin)
	_, page := get(t, admin, e.srv.URL+"/admin/users/bob")
	contains(t, page, `action="/admin/users/bob/admin/on"`)
	res, _ := e.form(t, admin, "/admin/users/bob/admin/on", nil)
	eq(t, "on", res.StatusCode, http.StatusSeeOther)
	if !e.user(t, "bob").Admin {
		t.Fatal("bob is not an admin")
	}
	_, page = get(t, admin, e.srv.URL+"/admin/users/bob")
	contains(t, page, `href="/admin/users/bob/admin/off"`)
	code, page := get(t, admin, e.srv.URL+"/admin/users/bob/admin/off")
	eq(t, "confirm", code, http.StatusOK)
	contains(t, page, "Take admin away from bob?", `action="/admin/users/bob/admin/off"`)
	res, _ = e.form(t, admin, "/admin/users/bob/admin/off", nil)
	eq(t, "off", res.StatusCode, http.StatusSeeOther)
	eq(t, "led to", res.Header.Get("Location"), "/admin/users/bob?done=admin-off")
	if e.user(t, "bob").Admin {
		t.Fatal("bob is still an admin")
	}

	// The page warns an admin about themselves, and the last admin stays one.
	_, page = get(t, admin, e.srv.URL+"/admin/users/admin/admin/off")
	contains(t, page, "This is you")
	res, page = e.form(t, admin, "/admin/users/admin/admin/off", nil)
	eq(t, "the last admin", res.StatusCode, http.StatusConflict)
	contains(t, page, "admin is the last admin")
}

func TestAResetPasswordIsShownOnceAndRevokesTheTokensOnlyWhenTicked(t *testing.T) {
	e := site(t)
	admin, alice := e.login(t, "admin"), e.login(t, "alice")
	code, page := get(t, admin, e.srv.URL+"/admin/users/alice/password")
	eq(t, "confirm", code, http.StatusOK)
	contains(t, page, "Reset alice&#39;s password?", `<input type="checkbox" name="revoke">`, `action="/admin/users/alice/password"`)

	res, page := e.form(t, admin, "/admin/users/alice/password", nil)
	eq(t, "without sudo", res.StatusCode, http.StatusForbidden)
	contains(t, page, "reset a user&#39;s password", "(<strong>alice</strong>)", `name="next" value="/admin/users/alice/password"`)
	e.sudo(t, admin)
	res, page = e.form(t, admin, "/admin/users/alice/password", nil)
	eq(t, "reset", res.StatusCode, http.StatusOK)
	pw := onceValue(t, page)
	eq(t, "alice's tokens kept", len(e.tokens(t, "alice")), 1)
	code, _ = get(t, alice, e.srv.URL+"/")
	eq(t, "alice's session", code, http.StatusSeeOther)
	res = post(t, e.anon(t), e.srv.URL+"/login", url.Values{"name": {"alice"}, "password": {pw}})
	eq(t, "the new password", res.StatusCode, http.StatusSeeOther)

	res, _ = e.form(t, admin, "/admin/users/alice/password", url.Values{"revoke": {"on"}})
	eq(t, "reset, revoking", res.StatusCode, http.StatusOK)
	eq(t, "alice's tokens", len(e.tokens(t, "alice")), 0)
	lacks(t, e.audit(t), pw)
}

func TestAUsersTokensAreMadeAndRevokedFromTheirPage(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	e.sudo(t, admin)
	res, page := e.form(t, admin, "/admin/users/bob/tokens", url.Values{"label": {"runner"}, "days": {"0"}})
	eq(t, "make", res.StatusCode, http.StatusOK)
	contains(t, page, "New token for bob", "It never expires.")
	plain := onceValue(t, page)
	_, page = get(t, admin, e.srv.URL+"/admin/users/bob")
	contains(t, page, ">runner</td>")
	lacks(t, page, plain)

	id := strconv.FormatInt(e.tokens(t, "bob")[0].ID, 10)
	code, page := get(t, admin, e.srv.URL+"/admin/users/bob/tokens/"+id+"/revoke")
	eq(t, "confirm", code, http.StatusOK)
	contains(t, page, "Revoke bob&#39;s token &#34;runner&#34;?")
	// A token is revoked under its own user's page only.
	code, _ = get(t, admin, e.srv.URL+"/admin/users/alice/tokens/"+id+"/revoke")
	eq(t, "under another user", code, http.StatusNotFound)
	res, _ = e.form(t, admin, "/admin/users/alice/tokens/"+id+"/revoke", nil)
	eq(t, "revoked under another user", res.StatusCode, http.StatusNotFound)
	res, _ = e.form(t, admin, "/admin/users/bob/tokens/"+id+"/revoke", nil)
	eq(t, "revoke", res.StatusCode, http.StatusSeeOther)
	eq(t, "bob's tokens", len(e.tokens(t, "bob")), 0)

	res, _ = e.form(t, admin, "/admin/users/bob/tokens", url.Values{"label": {"runner"}, "days": {"7"}})
	eq(t, "a lifetime not offered", res.StatusCode, http.StatusBadRequest)
}

func TestDeletingAUserIsConfirmedFirstAndNeverOneself(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	code, page := get(t, admin, e.srv.URL+"/admin/users/alice/delete")
	eq(t, "confirm", code, http.StatusOK)
	contains(t, page, "Delete alice?", "1 tokens", `class="action action-danger">Delete alice</button>`)
	e.sudo(t, admin)
	res, _ := e.form(t, admin, "/admin/users/alice/delete", nil)
	eq(t, "delete", res.StatusCode, http.StatusSeeOther)
	eq(t, "led to", res.Header.Get("Location"), "/admin/users?done=deleted")
	if _, err := e.st.UserByName(context.Background(), "alice"); err == nil {
		t.Fatal("alice is still there")
	}
	_, page = get(t, admin, e.srv.URL+"/admin/users/admin")
	lacks(t, page, `href="/admin/users/admin/delete"`)
	res, page = e.form(t, admin, "/admin/users/admin/delete", nil)
	eq(t, "oneself", res.StatusCode, http.StatusConflict)
	contains(t, page, "you cannot delete yourself")
	code, _ = get(t, admin, e.srv.URL+"/admin/users/nobody")
	eq(t, "nobody", code, http.StatusNotFound)
}

// A name made before the name rule may hold anything: it is escaped in
// every address and on every page, and its pages still work.
func TestAUserWithANameOutsideTheRuleStillHasPagesThatWork(t *testing.T) {
	e := site(t)
	odd := `O'Neil <b>x</b>/y?z`
	_, err := e.st.CreateUser(context.Background(), odd, hashOnce(), false)
	must(t, err)
	path := "/admin/users/" + url.PathEscape(odd)
	admin := e.login(t, "admin")
	_, page := get(t, admin, e.srv.URL+"/admin/users")
	contains(t, page, `href="`+path+`"`, "O&#39;Neil &lt;b&gt;x&lt;/b&gt;/y?z")
	lacks(t, page, "<b>x</b>")
	code, page := get(t, admin, e.srv.URL+path)
	eq(t, "the page", code, http.StatusOK)
	contains(t, page, `action="`+path+`/grants"`)
	lacks(t, page, "<b>x</b>")
	e.sudo(t, admin)
	res, _ := e.form(t, admin, path+"/grants", url.Values{"view": {"smoke"}})
	eq(t, "grants", res.StatusCode, http.StatusSeeOther)
	eq(t, "led to", res.Header.Get("Location"), path+"?done=grants")
	eq(t, "granted", e.perms(t, odd)["smoke"], store.Perm{View: true})
	res, page = e.form(t, admin, path+"/delete", nil)
	eq(t, "delete", res.StatusCode, http.StatusSeeOther)
	lacks(t, page, "<b>x</b>")

	// Asked for the password first, the way back is escaped as well.
	other := e.login(t, "admin")
	_, err = e.st.CreateUser(context.Background(), odd, hashOnce(), false)
	must(t, err)
	_, page = e.form(t, other, path+"/delete", nil)
	contains(t, page, `name="next" value="`+path+`/delete"`, "(<strong>O&#39;Neil &lt;b&gt;x&lt;/b&gt;/y?z</strong>)")
}

func TestTheSecretsPageSetsAndDeletesAndNeverShowsAValue(t *testing.T) {
	e := site(t)
	e.hook("smoke", "DEPLOY_KEY", "CHAT_TOKEN")
	admin := e.login(t, "admin")
	code, page := get(t, admin, e.srv.URL+"/admin/secrets")
	eq(t, "secrets", code, http.StatusOK)
	selfContained(t, page)
	contains(t, page, "<code>DEPLOY_KEY</code>", `href="/admin/secrets/DEPLOY_KEY/delete"`, "Missing",
		"<code>CHAT_TOKEN</code>", `href="/admin/secrets?name=CHAT_TOKEN#set"`,
		"<code>GITHUB_WEBHOOK_SMOKE</code> <span class=\"tag\">webhook</span>",
		`name="value" type="password" autocomplete="off"`)
	_, page = get(t, admin, e.srv.URL+"/admin/secrets?name=CHAT_TOKEN")
	contains(t, page, `name="name" value="CHAT_TOKEN"`)

	value := "an-obvious-test-secret"
	res, page := e.form(t, admin, "/admin/secrets", url.Values{"name": {"CHAT_TOKEN"}, "value": {value}})
	eq(t, "without sudo", res.StatusCode, http.StatusForbidden)
	contains(t, page, "set a secret", `name="next" value="/admin/secrets"`)
	lacks(t, page, value)
	e.sudo(t, admin)
	res, _ = e.form(t, admin, "/admin/secrets", url.Values{"name": {"CHAT_TOKEN"}, "value": {value}})
	eq(t, "set", res.StatusCode, http.StatusSeeOther)
	eq(t, "led to", res.Header.Get("Location"), "/admin/secrets?done=secret-set")
	if _, err := e.st.Secret(context.Background(), "CHAT_TOKEN"); err != nil {
		t.Fatal("CHAT_TOKEN is not set")
	}
	_, page = get(t, admin, e.srv.URL+"/admin/secrets?done=secret-set")
	contains(t, page, "The secret is set.", `href="/admin/secrets/CHAT_TOKEN/delete"`)
	lacks(t, page, value, "the-sealed-bytes", `href="/admin/secrets?name=CHAT_TOKEN#set"`)

	res, page = e.form(t, admin, "/admin/secrets", url.Values{"name": {"1BAD"}, "value": {value}})
	eq(t, "a bad name", res.StatusCode, http.StatusBadRequest)
	contains(t, page, `name="name" value="1BAD"`, "not a name a shell accepts", `role="alert"`)
	lacks(t, page, value)
	res, _ = e.form(t, admin, "/admin/secrets", url.Values{"name": {"SHORT"}, "value": {"x"}})
	eq(t, "a value too short", res.StatusCode, http.StatusBadRequest)
	lacks(t, e.audit(t), value)

	code, page = get(t, admin, e.srv.URL+"/admin/secrets/DEPLOY_KEY/delete")
	eq(t, "confirm", code, http.StatusOK)
	contains(t, page, "Delete the secret DEPLOY_KEY?", "Still named by smoke")
	res, _ = e.form(t, admin, "/admin/secrets/DEPLOY_KEY/delete", nil)
	eq(t, "delete", res.StatusCode, http.StatusSeeOther)
	if _, err := e.st.Secret(context.Background(), "DEPLOY_KEY"); err == nil {
		t.Fatal("DEPLOY_KEY is still there")
	}
	code, _ = get(t, admin, e.srv.URL+"/admin/secrets/NONE/delete")
	eq(t, "no such secret", code, http.StatusNotFound)
}

func TestTheServerPageSaysWhatTheServerRunsWithAndHasTheAudit(t *testing.T) {
	e := site(t)
	e.hook("smoke")
	ctx := context.Background()
	for i := range 101 {
		must(t, e.st.AppendAudit(ctx, store.Audit{At: time.Now(), Actor: "cli", Action: "token.create", Target: "row-" + strconv.Itoa(i)}))
	}
	admin := e.login(t, "admin")
	code, page := get(t, admin, e.srv.URL+"/admin/server")
	eq(t, "server", code, http.StatusOK)
	selfContained(t, page)
	contains(t, page, "go-test", "<dd>7</dd>", "up 1h 30m", "<code>127.0.0.1:8085</code>", "<code>https://ci.example/</code>",
		"<dd>3, the built-in runner's limit</dd>", "<code>/bin/sh</code>", "720h0m0s", "<code>/srv/kipp/data</code>", "<code>/srv/kipp/jobs</code>",
		"3.0 GiB", "5.0 MiB", "taken", `action="/admin/server/reload"`,
		"<code>https://ci.example/api/v1/hooks/github/smoke</code>", `<span class="status failed">missing</span>`, `href="/admin/secrets?name=GITHUB_WEBHOOK_SMOKE#set"`,
		"row-100", `href="/admin/server?before=`)
	lacks(t, page, "row-0<")

	_, older, _ := strings.Cut(page, `href="/admin/server?before=`)
	before, _, _ := strings.Cut(older, "#")
	code, page = get(t, admin, e.srv.URL+"/admin/server?before="+before)
	eq(t, "older", code, http.StatusOK)
	contains(t, page, "row-0<", `href="/admin/server#audit">Newest rows`)
	lacks(t, page, "row-100")
	code, _ = get(t, admin, e.srv.URL+"/admin/server?before=x")
	eq(t, "a bad before", code, http.StatusBadRequest)

	// Reload needs no password: it changes nothing the files do not say.
	res, _ := e.form(t, admin, "/admin/server/reload", nil)
	eq(t, "reload", res.StatusCode, http.StatusSeeOther)
	eq(t, "led to", res.Header.Get("Location"), "/admin/server?done=reloaded")
	contains(t, e.audit(t), "admin reload")
}

// A secret may be as long as config.MaxSecret, and a browser sends each of
// its bytes as three when it percent-encodes them, so the secrets form
// takes that much; every other form keeps the smaller limit.
func TestTheSecretsFormTakesTheLargestValueEvenPercentEncoded(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	e.sudo(t, admin)
	value := strings.Repeat("+", config.MaxSecret)
	res, _ := e.form(t, admin, "/admin/secrets", url.Values{"name": {"BIG_KEY"}, "value": {value}})
	eq(t, "a value of MaxSecret bytes", res.StatusCode, http.StatusSeeOther)
	if _, err := e.st.Secret(context.Background(), "BIG_KEY"); err != nil {
		t.Fatal("BIG_KEY is not set")
	}
	res, _ = e.form(t, admin, "/admin/secrets", url.Values{"name": {"BIG_KEY"}, "value": {value + "+"}})
	eq(t, "one byte more", res.StatusCode, http.StatusBadRequest)

	res, _ = e.form(t, admin, "/admin/users", url.Values{"name": {"carol"}, "pad": {strings.Repeat("+", maxForm)}})
	eq(t, "another form over its limit", res.StatusCode, http.StatusForbidden)
	if _, err := e.st.UserByName(context.Background(), "carol"); err == nil {
		t.Fatal("carol was added from a form over the limit")
	}
}

func TestQuietIsMadeFromTheServerPageAndEveryPageSaysSo(t *testing.T) {
	e := site(t)
	admin, bob := e.login(t, "admin"), e.login(t, "bob")
	_, page := get(t, admin, e.srv.URL+"/admin/server")
	contains(t, page, `action="/admin/server/quiet"`, "Make kipp quiet")
	lacks(t, page, "kipp is quiet")

	res, _ := e.form(t, admin, "/admin/server/quiet", nil)
	eq(t, "quiet on", res.StatusCode, http.StatusSeeOther)
	eq(t, "led to", res.Header.Get("Location"), "/admin/server?done=quiet-on")
	contains(t, e.audit(t), "admin quiet")
	_, page = get(t, admin, e.srv.URL+"/admin/server")
	contains(t, page, `action="/admin/server/quiet/off"`, "Take builds again", "nothing is left")
	for _, path := range []string{"/", "/jobs/smoke"} {
		_, page = get(t, bob, e.srv.URL+path)
		contains(t, page, "kipp is quiet")
	}
	_, page = get(t, e.anon(t), e.srv.URL+"/login")
	lacks(t, page, "kipp is quiet")

	// A trigger from a page is refused, and says when to try again.
	res, page = e.form(t, bob, "/jobs/smoke/build", nil)
	eq(t, "a build", res.StatusCode, http.StatusServiceUnavailable)
	contains(t, page, "kipp is quiet")
	eq(t, "Retry-After", res.Header.Get("Retry-After"), "60")

	res, _ = e.form(t, admin, "/admin/server/quiet/off", nil)
	eq(t, "quiet off", res.StatusCode, http.StatusSeeOther)
	_, page = get(t, bob, e.srv.URL+"/")
	lacks(t, page, "kipp is quiet")
}

// uploadSecret posts the secrets form with fields and a file as a browser's multipart form does.
func (e *env) uploadSecret(t *testing.T, c *http.Client, fields url.Values, file []byte) (*http.Response, string) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	must(t, mw.WriteField("csrf", e.token(t, c)))
	for k, vs := range fields {
		for _, v := range vs {
			must(t, mw.WriteField(k, v))
		}
	}
	part, err := mw.CreateFormFile("file", "id.pem")
	must(t, err)
	_, err = part.Write(file)
	must(t, err)
	must(t, mw.Close())
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+"/admin/secrets", &body)
	must(t, err)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return do(t, c, req)
}

func TestTheSecretsPageSetsAFileAndAUserAndPasswordUnderSudoAndShowsOnlyTheUser(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	key := []byte("\x00\xff binary bytes of a key\n")
	res, _ := e.uploadSecret(t, admin, url.Values{"name": {"KEYFILE"}, "kind": {"file"}}, key)
	eq(t, "a file without sudo", res.StatusCode, http.StatusForbidden)

	e.sudo(t, admin)
	res, _ = e.uploadSecret(t, admin, url.Values{"name": {"KEYFILE"}, "kind": {"file"}}, key)
	eq(t, "a file", res.StatusCode, http.StatusSeeOther)
	rec, err := e.st.SecretRecord(context.Background(), "KEYFILE")
	must(t, err)
	if rec.Kind != "file" || bytes.Contains(rec.Sealed, key) {
		t.Fatalf("%+v", rec)
	}
	const password = "a-password-long-enough"
	res, _ = e.uploadSecret(t, admin, url.Values{"name": {"LOGIN"}, "kind": {"userpass"}, "user": {"deployer"}, "password": {password}}, nil)
	eq(t, "a user and password", res.StatusCode, http.StatusSeeOther)
	_, page := get(t, admin, e.srv.URL+"/admin/secrets")
	contains(t, page, "<code>KEYFILE</code>", "<code>LOGIN</code>", "<td>file</td>", "user and password: <code>deployer</code>")
	lacks(t, page, password, "binary bytes")

	for what, c := range map[string]struct {
		fields url.Values
		file   []byte
		says   string
	}{
		"an empty file":         {url.Values{"name": {"F"}, "kind": {"file"}}, nil, "not empty"},
		"a file over a MiB":     {url.Values{"name": {"F"}, "kind": {"file"}}, bytes.Repeat([]byte("k"), config.MaxSecretFile+1), "at most 1048576 bytes"},
		"no kind of that name":  {url.Values{"name": {"F"}, "kind": {"cert"}}, nil, "not a kind of secret"},
		"a user with a newline": {url.Values{"name": {"U"}, "kind": {"userpass"}, "user": {"a\nb"}, "password": {password}}, nil, "one line"},
	} {
		res, page := e.uploadSecret(t, admin, c.fields, c.file)
		eq(t, what, res.StatusCode, http.StatusBadRequest)
		contains(t, page, c.says)
		lacks(t, page, password, "kkkkkkkk")
	}
	for _, name := range []string{"F", "U"} {
		if _, err := e.st.Secret(context.Background(), name); err == nil {
			t.Errorf("%s was stored", name)
		}
	}
}

func TestTheSecretsPageSetsShowsAndReplacesScopesUnderSudoAndHintsAtNarrowing(t *testing.T) {
	e := site(t)
	e.hook("smoke", "DEPLOY_KEY")
	admin := e.login(t, "admin")
	const value = "an-obvious-test-secret"
	e.sudo(t, admin)
	res, _ := e.form(t, admin, "/admin/secrets", url.Values{"name": {"DEPLOY_KEY"}, "value": {value}})
	eq(t, "set", res.StatusCode, http.StatusSeeOther)
	_, page := get(t, admin, e.srv.URL+"/admin/secrets")
	contains(t, page, "global", "only one job names it", `action="/admin/secrets/DEPLOY_KEY/scopes"`,
		`<select class="entry entry-scopes" id="scopes-DEPLOY_KEY" name="scopes" multiple data-tags>`,
		`<option value="global" selected>global</option>`, `<option value="job:smoke">job:smoke</option>`)
	lacks(t, page, "<textarea")

	res, _ = e.form(t, admin, "/admin/secrets/DEPLOY_KEY/scopes", url.Values{"scopes": {"group:site", "job:acme-*", "job:smoke", "job:smoke"}})
	eq(t, "rescoped", res.StatusCode, http.StatusSeeOther)
	eq(t, "led to", res.Header.Get("Location"), "/admin/secrets?done=scoped")
	rec, err := e.st.SecretRecord(context.Background(), "DEPLOY_KEY")
	must(t, err)
	eq(t, "scopes", strings.Join(rec.Scopes, "|"), "group:site|job:acme-*|job:smoke")
	contains(t, e.audit(t), "secret.scope", "group:site,job:acme-*,job:smoke")
	_, page = get(t, admin, e.srv.URL+"/admin/secrets")
	contains(t, page, "group:site, job:acme-*, job:smoke", `<optgroup label="Other">`,
		`<option value="job:acme-*" selected>job:acme-*</option>`, `<option value="job:smoke" selected>job:smoke</option>`)
	lacks(t, page, value, "only one job names it")

	res, _ = e.form(t, admin, "/admin/secrets/DEPLOY_KEY/scopes", url.Values{"scopes": {"job:["}})
	eq(t, "a bad pattern", res.StatusCode, http.StatusBadRequest)
	res, _ = e.form(t, admin, "/admin/secrets", url.Values{"name": {"CHAT_TOKEN"}, "value": {value}, "scopes": {"job:other"}})
	eq(t, "set with a scope", res.StatusCode, http.StatusSeeOther)
	rec, err = e.st.SecretRecord(context.Background(), "CHAT_TOKEN")
	must(t, err)
	eq(t, "scopes", strings.Join(rec.Scopes, "|"), "job:other")
	res, _ = e.form(t, admin, "/admin/secrets/NONE/scopes", url.Values{"scopes": {"global"}})
	eq(t, "no such secret", res.StatusCode, http.StatusNotFound)
}

func TestTheSetFormMarksEachFieldWithItsKindAndOffersScopesToPick(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	_, page := get(t, admin, e.srv.URL+"/admin/secrets")
	contains(t, page, `<div data-kind="text">`, `<div data-kind="file">`, `<div data-kind="userpass">`,
		`<select class="entry entry-scopes" id="secret-scopes" name="scopes" multiple data-tags aria-describedby="secret-scopes-help">`,
		`<option value="global">global</option>`, `scopes.js`)
}

func TestTheSecretsPageRefusesScopesWithoutSudoAndListsAnOutOfScopeSecretBesideTheMissingOnes(t *testing.T) {
	e := site(t)
	e.hook("slow", "DEPLOY_KEY")
	admin := e.login(t, "admin")
	e.sudo(t, admin)
	res, _ := e.form(t, admin, "/admin/secrets", url.Values{"name": {"DEPLOY_KEY"}, "value": {"an-obvious-test-secret"}, "scopes": {"job:other"}})
	eq(t, "set", res.StatusCode, http.StatusSeeOther)
	_, page := get(t, admin, e.srv.URL+"/admin/secrets")
	contains(t, page, "Out of scope", "<code>DEPLOY_KEY</code>", "job:other")
	other := e.login(t, "admin")
	res, page = e.form(t, other, "/admin/secrets/DEPLOY_KEY/scopes", url.Values{"scopes": {"global"}})
	eq(t, "without sudo", res.StatusCode, http.StatusForbidden)
	contains(t, page, "change a secret&#39;s scopes", `name="next" value="/admin/secrets"`)
}

func TestTheServerPageSaysWhereTheBinaryCameFromAndWhatTheUpdateCheckFound(t *testing.T) {
	checked := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, c := range []struct {
		name  string
		set   func(*api.ServerInfo)
		wants []string
		lacks []string
	}{
		{"a newer upgradable release", func(i *api.ServerInfo) {
			i.Latest, i.NotesURL, i.Upgradable, i.CheckedAt = "v9.9.9", "https://github.com/ndthuan/kipp/releases/tag/v9.9.9", true, checked
		}, []string{"Homebrew", "v9.9.9 is available", `href="https://github.com/ndthuan/kipp/releases/tag/v9.9.9"`}, []string{"cannot be installed"}},
		{"a newer release with no signature", func(i *api.ServerInfo) {
			i.Latest, i.NotesURL, i.CheckedAt = "v9.9.9", "https://github.com/ndthuan/kipp/releases/tag/v9.9.9", checked
		}, []string{"v9.9.9 is available", "cannot be installed from here"}, nil},
		{"nothing newer", func(i *api.ServerInfo) { i.CheckedAt = checked }, []string{"up to date, as of"}, []string{"is available"}},
		{"not asked yet", func(*api.ServerInfo) {}, []string{"not checked yet"}, nil},
		{"the check off", func(i *api.ServerInfo) { i.UpdateCheck = false }, []string{"the check is off (--no-update-check)"}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := site(t, func(d *api.Deps) {
				d.ServerInfo = func() api.ServerInfo {
					i := serverInfoNow()
					i.Origin, i.UpdateCheck = "Homebrew", true
					c.set(&i)
					return i
				}
			})
			code, page := get(t, e.login(t, "admin"), e.srv.URL+"/admin/server")
			eq(t, "server", code, http.StatusOK)
			contains(t, page, c.wants...)
			lacks(t, page, c.lacks...)
		})
	}
}

// The grants form held through the password step: the admin who is asked
// for the password again sends nothing twice, and the change is made once.
func TestGrantsSurviveThePasswordStepAndAreAppliedOnce(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	form := url.Values{"view": {"ask", "smoke"}, "build": {"ask"}, "cancel": {"ask"}}
	res, page := e.form(t, admin, "/admin/users/bob/grants", form)
	eq(t, "the password is asked", res.StatusCode, http.StatusForbidden)
	contains(t, page, "Confirm your password", `name="action" value="POST /admin/users/{name}/grants"`, `name="about" value="bob"`,
		`name="view" value="ask"`, `name="view" value="smoke"`, `name="build" value="ask"`, `name="cancel" value="ask"`, "the change is made as you ticked it")
	lacks(t, page, "to send the change again")
	if _, held := e.perms(t, "bob")["ask"]; held {
		t.Fatal("the change was made before the password")
	}

	// A wrong password changes nothing and still holds the form.
	sent := url.Values{"password": {"nope"}, "next": {"/admin/users/bob"}, "action": {"POST /admin/users/{name}/grants"}, "about": {"bob"},
		"view": {"ask", "smoke"}, "build": {"ask"}, "cancel": {"ask"}}
	res, page = e.form(t, admin, "/sudo", sent)
	eq(t, "a wrong password", res.StatusCode, http.StatusUnauthorized)
	contains(t, page, `name="build" value="ask"`)
	if _, held := e.perms(t, "bob")["ask"]; held {
		t.Fatal("a wrong password made the change")
	}

	sent.Set("password", "pw")
	res, _ = e.form(t, admin, "/sudo", sent)
	eq(t, "the password is right", res.StatusCode, http.StatusSeeOther)
	eq(t, "back on the user page, saved", res.Header.Get("Location"), "/admin/users/bob?done=grants")
	want := map[string]store.Perm{"ask": {View: true, Build: true, Cancel: true}, "smoke": {View: true}, "gone-job": {}}
	delete(want, "gone-job")
	if got := e.perms(t, "bob"); !maps.Equal(got, want) {
		t.Fatalf("grants: got %v, want %v", got, want)
	}
	_, page = get(t, admin, e.srv.URL+"/admin/users/bob?done=grants")
	contains(t, page, "The grants are saved.")
	eq(t, "audited once", strings.Count(e.audit(t), "grants.set bob"), 1)
}

func TestTheGrantsOfAnotherActionAreNotAppliedByThePasswordStep(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	res, _ := e.form(t, admin, "/sudo", url.Values{"password": {"pw"}, "next": {"/admin/users/bob"}, "action": {"POST /admin/users/{name}/tokens"}, "about": {"bob"},
		"view": {"ask"}})
	eq(t, "the password is right", res.StatusCode, http.StatusSeeOther)
	eq(t, "led back, nothing sent again", res.Header.Get("Location"), "/admin/users/bob")
	if _, held := e.perms(t, "bob")["ask"]; held {
		t.Fatal("a form of another change was applied as grants")
	}
}

func TestBuildOrCancelWithoutViewIsRefusedWithTheRowNamedAndTheTicksKept(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	e.sudo(t, admin)
	before := e.perms(t, "bob")
	res, page := e.form(t, admin, "/admin/users/bob/grants", url.Values{"view": {"slow"}, "build": {"ask"}, "cancel": {"smoke"}})
	eq(t, "refused", res.StatusCode, http.StatusBadRequest)
	contains(t, page, `class="notice notice-warn" role="alert">Not saved: build and cancel need view too. Tick View for ask,`,
		"Tick View for smoke,", `name="build" value="ask" aria-label="Build ask" checked>`, `name="view" value="slow" aria-label="View slow" checked>`)
	lacks(t, page, "Tick View for slow", "The grants are saved.")
	if got := e.perms(t, "bob"); !maps.Equal(got, before) {
		t.Fatalf("a refused form changed the grants: %v, was %v", got, before)
	}

	// The same set with view saves.
	res, _ = e.form(t, admin, "/admin/users/bob/grants", url.Values{"view": {"ask", "smoke", "slow"}, "build": {"ask"}, "cancel": {"smoke"}})
	eq(t, "saved", res.StatusCode, http.StatusSeeOther)
	eq(t, "bob", e.perms(t, "bob")["smoke"], store.Perm{View: true, Cancel: true})
}

func TestAGrantStoredWithoutViewShowsViewTickedAndSavingStoresIt(t *testing.T) {
	stored := store.Perm{Build: true}
	row := grantRowOf("smoke", stored, nil)
	eq(t, "view shown ticked", row.Perm, store.Perm{View: true, Build: true})
	eq(t, "no warning", row.Warn, "")
	eq(t, "a row with nothing stays so", grantRowOf("smoke", store.Perm{}, nil).Perm, store.Perm{})
}

func TestTheGrantsFormLoadsTheScriptThatTicksViewWithBuildAndCancel(t *testing.T) {
	e := site(t)
	_, page := get(t, e.login(t, "admin"), e.srv.URL+"/admin/users/bob")
	contains(t, page, `<script src="/static/grants.js`, `<table class="grants" data-needs-view>`, `<td class="tick"><label><input type="checkbox" name="view"`)
	lacks(t, page, "<script>")
}

// On a screen touched with a finger, what is tapped is 44px tall; the
// widths and heights the browser measures are checked by hand, the rules
// are held here.
func TestWhatIsTappedIsAtLeast44pxTallWhereThePointerIsCoarse(t *testing.T) {
	data, err := staticFS.ReadFile("static/kipp.css")
	must(t, err)
	css := string(data)
	_, coarse, ok := strings.Cut(css, "@media (pointer: coarse) {")
	if !ok {
		t.Fatal("no rules for a coarse pointer")
	}
	for _, sel := range []string{".action,", ".action-small,", ".seg a,", ".group > summary,", ".user > summary,", ".theme > summary,", "td .link,"} {
		if !strings.Contains(coarse, sel) {
			t.Errorf("%s is not made touch sized", sel)
		}
	}
	contains(t, coarse, "min-height: 44px;", ".dot::after { content: \"\"; position: absolute; inset: -17px",
		".grants .tick label { min-height: 44px; min-width: 44px; }")
	contains(t, css, ".grants .tick label { display: flex;", `.grants input[type="checkbox"] { width: calc(var(--space) * 6); height: calc(var(--space) * 6);`)
	lacks(t, coarse, "font-size")
}
