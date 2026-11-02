package web

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ndthuan/kipp/internal/config"
)

const (
	appPEM    = "-----BEGIN RSA PRIVATE KEY-----\nTHE-KEY-BODY-NOBODY-MAY-SEE\n-----END RSA PRIVATE KEY-----\n"
	appHookSc = "the-webhook-secret-from-github"
)

// githubApps is a GitHub that converts one manifest code.
func githubApps(t *testing.T, status int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/app-manifests/CODE/conversions" || status != http.StatusCreated {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 77, "slug": "ci", "html_url": "https://github.com/apps/ci",
			"pem": appPEM, "webhook_secret": appHookSc, "client_secret": "the-client-secret"})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func (e *env) appsOn(t *testing.T, gh *httptest.Server, base string) string {
	t.Helper()
	dir := t.TempDir()
	WithApps(AppFlow{DataDir: dir, BaseURL: base, API: gh.URL, WebBase: "https://github.com"})(e.site)
	return dir
}

// start sends the Apps page's form and returns the state the form to GitHub carries.
func (e *env) startApp(t *testing.T, c *http.Client, name, org string) (*http.Response, string, string) {
	t.Helper()
	res, page := e.form(t, c, "/admin/apps", url.Values{"name": {name}, "org": {org}})
	_, rest, _ := strings.Cut(page, `action="https://`)
	action, _, _ := strings.Cut(rest, `"`)
	_, state, _ := strings.Cut(action, "state=")
	return res, page, state
}

func TestAnAdminMakesAnAppAndNeitherItsKeyNorItsSecretIsShown(t *testing.T) {
	gh, calls := githubApps(t, http.StatusCreated)
	e := site(t)
	dir := e.appsOn(t, gh, "https://kipp.example/")
	admin := e.login(t, "admin")

	res, page, _ := e.startApp(t, admin, "ci", "")
	eq(t, "start without sudo", res.StatusCode, http.StatusForbidden)
	contains(t, page, "make a GitHub App")
	e.sudo(t, admin)

	res, page, state := e.startApp(t, admin, "ci", "")
	eq(t, "start", res.StatusCode, http.StatusOK)
	contains(t, page, `action="https://github.com/settings/apps/new?state=`, `name="manifest"`, "&#34;url&#34;:&#34;https://kipp.example&#34;")
	contains(t, page, "https://kipp.example/api/v1/hooks/github-app/ci", "https://kipp.example/admin/apps/created", "&#34;statuses&#34;:&#34;write&#34;")
	csp := res.Header.Get("Content-Security-Policy")
	contains(t, csp, "form-action 'self' https://github.com;")
	if state == "" {
		t.Fatal("no state in the form to GitHub")
	}

	res, _ = getRes(t, admin, e.srv.URL+"/admin/apps/created?code=CODE&state="+state)
	eq(t, "created", res.StatusCode, http.StatusSeeOther)
	eq(t, "back to the Apps page", res.Header.Get("Location"), "/admin/apps?done=app-created")

	key := filepath.Join(dir, "apps", "ci.pem")
	got, err := os.ReadFile(key)
	must(t, err)
	eq(t, "the key", string(got), appPEM)
	info, err := os.Stat(key)
	must(t, err)
	eq(t, "key mode", info.Mode().Perm(), os.FileMode(0o600))
	info, err = os.Stat(filepath.Join(dir, "apps"))
	must(t, err)
	eq(t, "directory mode", info.Mode().Perm(), os.FileMode(0o700))
	if _, err := e.st.Secret(t.Context(), "GITHUB_APP_WEBHOOK_CI"); err != nil {
		t.Fatalf("the webhook secret is not stored: %v", err)
	}

	code, page := get(t, admin, e.srv.URL+"/admin/apps?done=app-created")
	eq(t, "apps page", code, http.StatusOK)
	contains(t, page, "The App is made", "app: {id: 77, key_file: "+key+"}", "/api/v1/hooks/github-app/ci", "GITHUB_APP_WEBHOOK_CI",
		"https://github.com/apps/ci/installations/new")
	lacks(t, page, "THE-KEY-BODY", appHookSc, "the-client-secret", "SUPERSECRET")
	for _, f := range []string{"ci.json"} {
		raw, err := os.ReadFile(filepath.Join(dir, "apps", f))
		must(t, err)
		lacks(t, string(raw), "THE-KEY-BODY", appHookSc, "the-client-secret")
	}

	// The same return again is not a second App.
	res, page = getRes(t, admin, e.srv.URL+"/admin/apps/created?code=CODE&state="+state)
	eq(t, "replay", res.StatusCode, http.StatusBadRequest)
	eq(t, "GitHub was asked once", calls.Load(), int32(1))
	lacks(t, page, "THE-KEY-BODY")
}

func TestAnOrganizationsAppIsMadeAtItsOwnAddress(t *testing.T) {
	gh, _ := githubApps(t, http.StatusCreated)
	e := site(t)
	e.appsOn(t, gh, "https://kipp.example")
	admin := e.login(t, "admin")
	e.sudo(t, admin)
	res, page, _ := e.startApp(t, admin, "ci", "acme-inc")
	eq(t, "start", res.StatusCode, http.StatusOK)
	contains(t, page, `action="https://github.com/organizations/acme-inc/settings/apps/new?state=`)
}

func TestTheAppsPageIsTheAdminsAndNeedsABaseURLAndAFreeName(t *testing.T) {
	gh, _ := githubApps(t, http.StatusCreated)
	e := site(t)
	dir := e.appsOn(t, gh, "")
	bob := e.login(t, "bob")
	code, _ := get(t, bob, e.srv.URL+"/admin/apps")
	eq(t, "bob's page", code, http.StatusForbidden)
	res, _ := e.form(t, bob, "/admin/apps", url.Values{"name": {"ci"}})
	eq(t, "bob's start", res.StatusCode, http.StatusForbidden)
	res, _ = getRes(t, bob, e.srv.URL+"/admin/apps/created?code=CODE&state=x")
	eq(t, "bob's return", res.StatusCode, http.StatusForbidden)

	admin := e.login(t, "admin")
	e.sudo(t, admin)
	res, page, _ := e.startApp(t, admin, "ci", "")
	eq(t, "no base url", res.StatusCode, http.StatusBadRequest)
	contains(t, page, "--base-url")

	e.site.apps.BaseURL = "https://kipp.example"
	for what, f := range map[string]url.Values{
		"underscore": {"name": {"my_app"}}, "slash": {"name": {"../x"}}, "empty": {"name": {""}},
		"org": {"name": {"ci"}, "org": {"a/b"}},
	} {
		res, page, _ := e.startApp(t, admin, f.Get("name"), f.Get("org"))
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d %s", what, res.StatusCode, page)
		}
	}
	must(t, os.MkdirAll(filepath.Join(dir, "apps"), 0o700))
	must(t, os.WriteFile(filepath.Join(dir, "apps", "ci.pem"), []byte("kept"), 0o600))
	res, page, _ = e.startApp(t, admin, "ci", "")
	eq(t, "name taken", res.StatusCode, http.StatusBadRequest)
	contains(t, page, "already")
}

func TestAStateIsTheUsersAndSurvivesASudoThatRanOut(t *testing.T) {
	gh, calls := githubApps(t, http.StatusCreated)
	e := site(t)
	dir := e.appsOn(t, gh, "https://kipp.example")
	_, err := e.st.CreateUser(t.Context(), "admin2", hashOnce(), true)
	must(t, err)

	admin := e.login(t, "admin")
	e.sudo(t, admin)
	_, _, state := e.startApp(t, admin, "ci", "")

	other := e.login(t, "admin2")
	e.sudo(t, other)
	res, _ := getRes(t, other, e.srv.URL+"/admin/apps/created?code=CODE&state="+state)
	eq(t, "someone else's state", res.StatusCode, http.StatusBadRequest)

	// Back from GitHub in a session whose sudo has ended: asked for the password, nothing spent.
	later := e.login(t, "admin")
	back := "/admin/apps/created?code=CODE&state=" + state
	res, page := getRes(t, later, e.srv.URL+back)
	eq(t, "no sudo", res.StatusCode, http.StatusForbidden)
	contains(t, page, "finish making a GitHub App", `name="next" value="`+strings.ReplaceAll(back, "&", "&amp;")+`"`)
	eq(t, "GitHub not asked", calls.Load(), int32(0))
	e.sudo(t, later)
	res, _ = getRes(t, later, e.srv.URL+back)
	eq(t, "after sudo", res.StatusCode, http.StatusSeeOther)
	if _, err := os.Stat(filepath.Join(dir, "apps", "ci.pem")); err != nil {
		t.Fatal(err)
	}
}

func TestAnAppGitHubWillNotHandOverLeavesNothingBehind(t *testing.T) {
	gh, _ := githubApps(t, http.StatusNotFound)
	e := site(t)
	dir := e.appsOn(t, gh, "https://kipp.example")
	admin := e.login(t, "admin")
	e.sudo(t, admin)
	_, _, state := e.startApp(t, admin, "ci", "")
	res, page := getRes(t, admin, e.srv.URL+"/admin/apps/created?code=CODE&state="+state)
	eq(t, "refused", res.StatusCode, http.StatusBadRequest)
	contains(t, page, "Start again")
	if _, err := os.Stat(filepath.Join(dir, "apps")); err == nil {
		t.Fatal("a directory was made for nothing")
	}
	if _, err := e.st.Secret(t.Context(), "GITHUB_APP_WEBHOOK_CI"); err == nil {
		t.Fatal("a secret was stored for nothing")
	}
}

func getRes(t *testing.T, c *http.Client, u string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, u, nil)
	must(t, err)
	return do(t, c, req)
}

// ownerJobs adds owner jobs that sign in as the Apps given, name to app
// line, as a reload of their files would.
func (e *env) ownerJobs(t *testing.T, apps map[string]string) {
	t.Helper()
	snap := e.d.Snapshot()
	jobs, names := maps.Clone(snap.Jobs), slices.Clone(snap.Names)
	for name, app := range apps {
		j, err := config.ParseJob(name+".yml", []byte("name: "+name+"\nowner: {login: "+name+", app: "+app+"}\n"))
		if err != nil {
			t.Fatal(err)
		}
		jobs[name] = j
		names = append(names, name)
	}
	slices.Sort(names)
	e.d.SetSnapshot(&config.Snapshot{Jobs: jobs, Names: names})
}

func writeAppRecord(t *testing.T, dir, name string, id int64) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"name": name, "id": id, "slug": name, "html_url": "https://github.com/apps/" + name})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "apps"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "apps", name+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTheAppsPageListsEveryAppTheJobsUseAndWhereEachWasMade(t *testing.T) {
	gh, _ := githubApps(t, http.StatusCreated)
	e := site(t)
	dir := e.appsOn(t, gh, "https://kipp.example/")
	// ci was made here, as 77, and its key is not one an App can sign with;
	// spare was made here and no job uses it; hand was made at GitHub.
	writeAppRecord(t, dir, "ci", 77)
	writeAppRecord(t, dir, "spare", 79)
	if err := os.WriteFile(filepath.Join(dir, "apps", "ci.pem"), []byte(appPEM), 0o600); err != nil {
		t.Fatal(err)
	}
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	hand := filepath.Join(t.TempDir(), "hand.pem")
	if err := os.WriteFile(hand, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), 0o600); err != nil {
		t.Fatal(err)
	}
	e.ownerJobs(t, map[string]string{
		"acme": "{id: 78, key_file: " + filepath.Join(dir, "apps", "ci.pem") + "}",
		"beta": "{id: 90, key_file: " + hand + "}",
	})
	must(t, e.st.PutSecret(context.Background(), "GITHUB_APP_WEBHOOK_CI", []byte("sealed-test-bytes")))

	_, page := get(t, e.login(t, "admin"), e.srv.URL+"/admin/apps")
	lacks(t, page, "No App is made here", "THE-KEY-BODY-NOBODY-MAY-SEE")
	ci, hd, sp := row(t, page, "ci"), row(t, page, "hand"), row(t, page, "spare")
	contains(t, ci, "made here", "<a href=\"/jobs/acme\">acme</a>", "the job files give it the ID 78, and GitHub gave the App made here 77", "no private key in PEM")
	lacks(t, ci, "GITHUB_APP_WEBHOOK_CI is not set")
	contains(t, hd, "made at GitHub", "<a href=\"/jobs/beta\">beta</a>", "90", "GITHUB_APP_WEBHOOK_HAND is not set")
	lacks(t, hd, "Install it on an account", "key file")
	contains(t, sp, "made here", "no owner job signs in as it", "Install it on an account")
}

// row is the table row of the App called name.
func row(t *testing.T, page, name string) string {
	t.Helper()
	_, rest, ok := strings.Cut(page, "<tr id=\"app-"+name+"\">")
	if !ok {
		t.Fatalf("no row for %s in\n%s", name, page)
	}
	r, _, _ := strings.Cut(rest, "</tr>")
	return r
}
