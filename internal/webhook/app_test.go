package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/apispec/apispectest"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/dispatch"
	"github.com/ndthuan/kipp/internal/index"
	"github.com/ndthuan/kipp/internal/runner"
	"github.com/ndthuan/kipp/internal/store"
)

const appSecret = "the-app-webhook-secret"

// appHooks is a server with the App ci, which the owner jobs acme and
// other sign in as; acme also has the secret its old token-made hooks were
// signed with.
type appHooks struct {
	srv     *httptest.Server
	st      *store.Store
	d       *dispatch.Dispatcher
	mu      sync.Mutex
	rescans []string
	rescan  func(string) error
}

func newAppHooks(t *testing.T, withRescan bool) *appHooks {
	t.Helper()
	dir := t.TempDir()
	jobs := filepath.Join(dir, "jobs")
	must(t, os.MkdirAll(jobs, 0o750))
	app := "app: {id: 1, key_file: /keys/ci.pem}"
	must(t, os.WriteFile(filepath.Join(jobs, "acme.yml"), []byte("name: acme\nowner: {login: acme, "+app+"}\n"), 0o600))
	must(t, os.WriteFile(filepath.Join(jobs, "other.yml"), []byte("name: other\nowner: {login: Other, "+app+"}\n"), 0o600))
	snap, err := config.Load(jobs)
	must(t, err)
	st, err := store.Open(filepath.Join(dir, "db"))
	must(t, err)
	ctx := context.Background()
	must(t, st.PutOwnerRepo(ctx, store.OwnerRepo{Owner: "acme", Key: "site", Repo: "acme/site", State: config.RepoLive, LastSeenAt: time.Now()}))
	must(t, st.PutOwnerRepo(ctx, store.OwnerRepo{Owner: "other", Key: "site", Repo: "Other/site", State: config.RepoLive, LastSeenAt: time.Now()}))
	d := dispatch.New(st, runner.Local{}, dispatch.Options{DataDir: dir})
	ix := index.New(st, d.SetSnapshot)
	must(t, ix.SetBase(ctx, snap))
	secret := func(_ context.Context, name string) (string, error) {
		switch name {
		case "GITHUB_APP_WEBHOOK_CI":
			return appSecret, nil
		case "GITHUB_WEBHOOK_ACME":
			return "ownersecret", nil
		}
		return "", store.ErrNotFound
	}
	f := &appHooks{st: st}
	o := Options{Secret: secret, Index: ix, Record: func(ctx context.Context, id, job string) error {
		return st.RecordDelivery(ctx, id, job, time.Now())
	}}
	if withRescan {
		o.Rescan = func(job string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.rescans = append(f.rescans, job)
			if f.rescan != nil {
				return f.rescan(job)
			}
			return nil
		}
	}
	h := New(d, keeping(o, st))
	f.d = d
	mux := http.NewServeMux()
	mux.Handle("POST /api/v1/hooks/github/{job}", h.Job())
	mux.Handle("POST /api/v1/hooks/github-app/{app}", h.App())
	f.srv = httptest.NewServer(apispectest.Wrap(t, mux))
	t.Cleanup(func() { f.srv.Close(); _ = st.Close() })
	return f
}

func (f *appHooks) app(t *testing.T, name, event, secret, body string) (int, string) {
	t.Helper()
	return post(t, f.srv.URL+"/api/v1/hooks/github-app/"+name, event, sign(secret, body), body)
}

func (f *appHooks) builds(t *testing.T, job string) int {
	t.Helper()
	all, err := f.st.BuildsWithStatus(context.Background(), store.Queued, store.Running)
	must(t, err)
	n := 0
	for _, b := range all {
		if b.Job == job {
			n++
		}
	}
	return n
}

func pushOf(repo, login string) string {
	return `{"ref":"refs/heads/main","after":"` + sha + `","repository":{"full_name":"` + repo + `","default_branch":"main","owner":{"login":"` + login + `"}}}`
}

func TestAnAppDeliveryIsVerifiedWithTheAppsSecretAndRoutedByTheRepositorysOwner(t *testing.T) {
	f := newAppHooks(t, true)
	push := pushOf("acme/site", "acme")
	for name, tc := range map[string]struct{ app, secret string }{
		"wrong secret":           {"ci", "nope"},
		"the owner job's secret": {"ci", "ownersecret"},
		"an App without secret":  {"nobody", appSecret},
	} {
		if code, _ := f.app(t, tc.app, "push", tc.secret, push); code != http.StatusUnauthorized {
			t.Errorf("%s: %d", name, code)
		}
	}
	if code, _ := post(t, f.srv.URL+"/api/v1/hooks/github-app/ci", "push", "", push); code != http.StatusUnauthorized {
		t.Errorf("no signature: %d", code)
	}
	code, body := f.app(t, "ci", "push", appSecret, push)
	var out struct {
		Builds []queued `json:"builds"`
	}
	must(t, json.Unmarshal([]byte(body), &out))
	if code != http.StatusAccepted || len(out.Builds) != 1 || out.Builds[0].Job != "acme.site.main" {
		t.Fatalf("%d %s", code, body)
	}
	if f.builds(t, "acme.site.main") != 1 || f.builds(t, "other.site.main") != 0 {
		t.Fatal("the push built what it was not for")
	}
	// Another account under the same App goes to its own job; the login is read without regard to case.
	if code, body := f.app(t, "ci", "push", appSecret, pushOf("Other/site", "other")); code != http.StatusAccepted {
		t.Fatalf("%d %s", code, body)
	}
	if f.builds(t, "other.site.main") != 1 {
		t.Fatal("the other account's job was not built")
	}
}

func TestAnAppDeliveryNoJobIsForIsAcknowledgedAndSaysWhy(t *testing.T) {
	f := newAppHooks(t, true)
	for name, body := range map[string]string{
		"no owner job":     pushOf("stranger/site", "stranger"),
		"no repo of it":    pushOf("acme/unknown", "acme"),
		"not a push event": `{"zen":"x","repository":{"full_name":"acme/site","owner":{"login":"acme"}}}`,
	} {
		event := "push"
		if name == "not a push event" {
			event = "star"
		}
		code, out := f.app(t, "ci", event, appSecret, body)
		if code != http.StatusOK || !json.Valid([]byte(out)) || !containsKey(out, "ignored") {
			t.Errorf("%s: %d %s", name, code, out)
		}
	}
	if code, out := f.app(t, "ci", "ping", appSecret, `{"zen":"Keep it logically awesome."}`); code != http.StatusOK || !containsKey(out, "ignored") {
		t.Errorf("ping: %d %s", code, out)
	}
	if code, _ := f.app(t, "ci", "push", appSecret, `{"ref":"x"}`); code != http.StatusBadRequest {
		t.Errorf("a push that names no owner: %d", code)
	}
	if f.builds(t, "acme.site.main") != 0 {
		t.Fatal("something was built")
	}
}

func containsKey(body, key string) bool {
	var m map[string]any
	return json.Unmarshal([]byte(body), &m) == nil && m[key] != nil
}

func TestAnAppDeliveryReplayedIsRefused(t *testing.T) {
	f := newAppHooks(t, true)
	push := pushOf("acme/site", "acme")
	if code, _, _ := send(t, f.srv.URL+"/api/v1/hooks/github-app/ci", "push", sign(appSecret, push), "same-id", push); code != http.StatusAccepted {
		t.Fatalf("first: %d", code)
	}
	if code, _, _ := send(t, f.srv.URL+"/api/v1/hooks/github-app/ci", "push", sign(appSecret, push), "same-id", push); code != http.StatusConflict {
		t.Fatalf("replayed: %d", code)
	}
	if code, _, _ := send(t, f.srv.URL+"/api/v1/hooks/github-app/ci", "push", sign(appSecret, push), "", push); code != http.StatusBadRequest {
		t.Fatalf("no delivery id: %d", code)
	}
}

func TestAPushDeliveredByTheOldHookAndTheAppBuildsOnce(t *testing.T) {
	f := newAppHooks(t, true)
	push := pushOf("acme/site", "acme")
	if code, body := post(t, f.srv.URL+"/api/v1/hooks/github/acme", "push", sign("ownersecret", push), push); code != http.StatusAccepted {
		t.Fatalf("old hook: %d %s", code, body)
	}
	code, body := f.app(t, "ci", "push", appSecret, push)
	if code != http.StatusOK || !containsKey(body, "ignored") {
		t.Fatalf("the App's copy of the push: %d %s", code, body)
	}
	if n := f.builds(t, "acme.site.main"); n != 1 {
		t.Fatalf("%d builds of one commit", n)
	}
	// A later commit of the branch is built, though the first is still queued.
	later := strings.Replace(push, sha, strings.Repeat("b", 40), 1)
	if code, _ := f.app(t, "ci", "push", appSecret, later); code != http.StatusAccepted {
		t.Fatalf("a later commit: %d", code)
	}
	if n := f.builds(t, "acme.site.main"); n != 2 {
		t.Fatalf("%d builds of two commits", n)
	}
}

func TestTwoRoutesDeliveringOneCommitAtOnceBuildItOnce(t *testing.T) {
	f := newAppHooks(t, true)
	push := pushOf("acme/site", "acme")
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Go(func() {
			if i%2 == 0 {
				post(t, f.srv.URL+"/api/v1/hooks/github/acme", "push", sign("ownersecret", push), push)
			} else {
				f.app(t, "ci", "push", appSecret, push)
			}
		})
	}
	wg.Wait()
	if n := f.builds(t, "acme.site.main"); n != 1 {
		t.Fatalf("%d builds of one commit", n)
	}
}

func TestAnInstallationEventScansTheAccountsOwnerJob(t *testing.T) {
	f := newAppHooks(t, true)
	for _, event := range []string{"installation", "installation_repositories"} {
		body := `{"action":"` + event + `","installation":{"account":{"login":"ACME"}}}`
		code, out := f.app(t, "ci", event, appSecret, body)
		if code != http.StatusAccepted || !containsKey(out, "scan") {
			t.Fatalf("%s: %d %s", event, code, out)
		}
	}
	if !slices.Equal(f.rescans, []string{"acme", "acme"}) {
		t.Fatalf("rescans %v", f.rescans)
	}
	code, out := f.app(t, "ci", "installation", appSecret, `{"installation":{"account":{"login":"stranger"}}}`)
	if code != http.StatusOK || !containsKey(out, "ignored") || len(f.rescans) != 2 {
		t.Fatalf("an account no job is for: %d %s %v", code, out, f.rescans)
	}
	f.rescan = func(string) error { return errors.New("boom") }
	if code, _ := f.app(t, "ci", "installation", appSecret, `{"installation":{"account":{"login":"acme"}}}`); code != http.StatusInternalServerError {
		t.Fatalf("a rescan that could not be asked for: %d", code)
	}
}

func TestAnInstallationEventWithoutAScannerIsAcknowledged(t *testing.T) {
	f := newAppHooks(t, false)
	code, out := f.app(t, "ci", "installation", appSecret, `{"installation":{"account":{"login":"acme"}}}`)
	if code != http.StatusOK || !containsKey(out, "ignored") {
		t.Fatalf("%d %s", code, out)
	}
}

func TestAppSecretNameIsDerivedFromTheAppName(t *testing.T) {
	for in, want := range map[string]string{"octocat": "GITHUB_APP_WEBHOOK_OCTOCAT", "acme-ci": "GITHUB_APP_WEBHOOK_ACME_CI"} {
		if got := AppSecretName(in); got != want {
			t.Errorf("AppSecretName(%q) = %q, want %q", in, got, want)
		}
	}
}
