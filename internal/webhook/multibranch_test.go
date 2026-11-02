package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/apispec/apispectest"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/dispatch"
	"github.com/ndthuan/kipp/internal/index"
	"github.com/ndthuan/kipp/internal/runner"
	"github.com/ndthuan/kipp/internal/store"
)

// multibranchServer serves the hook of the multibranch job site, whose
// secret is topsecret, with an index the test can read.
func multibranchServer(t *testing.T) (*httptest.Server, *store.Store, *dispatch.Dispatcher) {
	t.Helper()
	dir := t.TempDir()
	jobs := filepath.Join(dir, "jobs")
	must(t, os.MkdirAll(jobs, 0o750))
	must(t, os.WriteFile(filepath.Join(jobs, "site.yml"), []byte("name: site\nmultibranch: {repo: acme/site}\n"), 0o600))
	snap, err := config.Load(jobs)
	must(t, err)
	st, err := store.Open(filepath.Join(dir, "db"))
	must(t, err)
	d := dispatch.New(st, runner.Local{}, dispatch.Options{DataDir: dir})
	ix := index.New(st, d.SetSnapshot)
	must(t, ix.SetBase(context.Background(), snap))
	secret := func(_ context.Context, name string) (string, error) {
		if name == "GITHUB_WEBHOOK_SITE" {
			return "topsecret", nil
		}
		return "", store.ErrNotFound
	}
	h := GitHub(d, Options{Secret: secret, Index: ix, Record: func(ctx context.Context, id, job string) error {
		return st.RecordDelivery(ctx, id, job, time.Now())
	}})
	mux := http.NewServeMux()
	mux.Handle("POST /api/v1/hooks/github/{job}", h)
	srv := httptest.NewServer(apispectest.Wrap(t, mux))
	t.Cleanup(func() { srv.Close(); _ = st.Close() })
	return srv, st, d
}

const sitePush = `{"ref":"refs/heads/main","after":"` + sha + `","repository":{"full_name":"acme/site","default_branch":"main"}}`

func TestAParentsWebhookIsVerifiedBeforeTheIndexSeesIt(t *testing.T) {
	srv, st, d := multibranchServer(t)
	url := srv.URL + "/api/v1/hooks/github/site"
	children := func() int {
		kids, err := st.Children(context.Background(), "")
		must(t, err)
		return len(kids)
	}
	for what, sig := range map[string]string{
		"no signature":           "",
		"a signature of another": sign("wrong", sitePush),
		"a malformed signature":  "sha256=zz",
	} {
		if code, _ := post(t, url, "push", sig, sitePush); code != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", what, code)
		}
	}
	if code, _ := post(t, srv.URL+"/api/v1/hooks/github/site.main", "push", sign("topsecret", sitePush), sitePush); code != http.StatusUnauthorized {
		t.Errorf("a child's name takes its parent's deliveries: %d", code)
	}
	if n := children(); n != 0 {
		t.Fatalf("the index saw a delivery nobody signed: %d children", n)
	}
	delivery := newDelivery()
	code, body, _ := send(t, url, "push", sign("topsecret", sitePush), delivery, sitePush)
	if code != http.StatusAccepted {
		t.Fatalf("%d %s", code, body)
	}
	var out struct {
		Builds []queued `json:"builds"`
	}
	must(t, json.Unmarshal([]byte(body), &out))
	if len(out.Builds) != 1 || out.Builds[0].Job != "site.main" || out.Builds[0].Number != 1 || children() != 1 {
		t.Fatalf("%s", body)
	}
	b, err := st.BuildByNumber(context.Background(), "site.main", 1)
	must(t, err)
	if b.Trigger != "webhook" || b.TriggerDetail != sha || b.Parent != "site" || b.TriggerBranch != "main" || b.TriggerDefaultBranch != "main" {
		t.Fatalf("%+v", b)
	}
	if d.Snapshot().Jobs["site.main"] == nil {
		t.Fatal("the child is not in the dispatcher's job set")
	}
	// The same delivery sent again is refused before the index sees it.
	if code, _, _ := send(t, url, "push", sign("topsecret", sitePush), delivery, sitePush); code != http.StatusConflict {
		t.Fatalf("a delivery sent again: %d", code)
	}
	if code, _ := post(t, url, "push", sign("topsecret", sitePush), sitePush); code != http.StatusConflict {
		t.Fatalf("the same body under another id: %d", code)
	}
	bad := `{"ref":"refs/heads/main","after":"abc","repository":{"full_name":"acme/site"}}`
	if code, _ := post(t, url, "push", sign("topsecret", bad), bad); code != http.StatusBadRequest {
		t.Fatalf("a push whose after is no commit: %d", code)
	}
	if code, body := post(t, url, "ping", sign("topsecret", `{"zen":"x"}`), `{"zen":"x"}`); code != http.StatusOK {
		t.Fatalf("a ping: %d %s", code, body)
	}
}

// sitePR is a pull_request delivery of acme/site: #12 from feature-x into
// main, from the repo itself unless head names another.
func sitePR(action, headRepo, extra string) string {
	repo := `null`
	if headRepo != "" {
		repo = `{"full_name":"` + headRepo + `"}`
	}
	return `{"action":"` + action + `","number":12,"pull_request":{"title":"Add login","html_url":"https://github.example/acme/site/pull/12","user":{"login":"ann"},"head":{"ref":"feature-x","sha":"` + sha +
		`","repo":` + repo + `},"base":{"ref":"main","sha":"` + sha + `"}}` + extra + `,"repository":{"full_name":"acme/site","default_branch":"main"}}`
}

func TestAPullRequestDeliveryBuildsItsChildAndAForksBuildsNothing(t *testing.T) {
	srv, st, d := multibranchServer(t)
	url := srv.URL + "/api/v1/hooks/github/site"
	body := sitePR("opened", "acme/site", "")
	code, got := post(t, url, "pull_request", sign("topsecret", body), body)
	if code != http.StatusAccepted {
		t.Fatalf("%d %s", code, got)
	}
	b, err := st.BuildByNumber(context.Background(), "site.pr-12", 1)
	must(t, err)
	if b.Trigger != "webhook" || b.TriggerDetail != sha || b.PR != 12 || b.BaseBranch != "main" || b.TriggerBranch != "feature-x" {
		t.Fatalf("%+v", b)
	}
	if job := d.Snapshot().Jobs["site.pr-12"]; job == nil || job.Description != "Add login" {
		t.Fatalf("%+v", job)
	}
	kids, err := st.Children(context.Background(), "site")
	must(t, err)
	if len(kids) != 1 || kids[0].Author != "ann" || kids[0].URL != "https://github.example/acme/site/pull/12" {
		t.Fatalf("the row keeps who opened the pull request and its page: %+v", kids)
	}
	edited := sitePR("edited", "acme/site", `,"changes":{"title":{"from":"x"}}`)
	if code, got := post(t, url, "pull_request", sign("topsecret", edited), edited); code != http.StatusOK {
		t.Fatalf("an edit of the title: %d %s", code, got)
	}
	moved := sitePR("edited", "acme/site", `,"changes":{"base":{"ref":{"from":"release"}}}`)
	if code, got := post(t, url, "pull_request", sign("topsecret", moved), moved); code != http.StatusAccepted {
		t.Fatalf("a new base: %d %s", code, got)
	}
	for _, from := range []string{"stranger/site", ""} {
		fork := sitePR("synchronize", from, `,"after":"`+from+`"`) // a field of its own, so the body is new
		code, got := post(t, url, "pull_request", sign("topsecret", fork), fork)
		if code != http.StatusOK || got != `{"ignored":"pull request #12 is from a fork: not built"}`+"\n" {
			t.Fatalf("a fork's pull request: %d %q", code, got)
		}
	}
	bad := `{"action":"opened","number":12,"pull_request":{"head":{"ref":"x","sha":"abc"},"base":{"ref":"main"}},"repository":{"full_name":"acme/site"}}`
	if code, _ := post(t, url, "pull_request", sign("topsecret", bad), bad); code != http.StatusBadRequest {
		t.Fatalf("a head that is no commit: %d", code)
	}
}
