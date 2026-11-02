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

// ownerServer serves the hook of the owner job acme, whose secret is
// ownersecret, over a store that already holds its live repo acme/site.
func ownerServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	jobs := filepath.Join(dir, "jobs")
	must(t, os.MkdirAll(jobs, 0o750))
	must(t, os.WriteFile(filepath.Join(jobs, "acme.yml"), []byte("name: acme\nowner: {login: acme, token: GH_TOKEN}\n"), 0o600))
	snap, err := config.Load(jobs)
	must(t, err)
	st, err := store.Open(filepath.Join(dir, "db"))
	must(t, err)
	must(t, st.PutOwnerRepo(context.Background(), store.OwnerRepo{Owner: "acme", Key: "site", Repo: "acme/site", State: config.RepoLive, LastSeenAt: time.Now()}))
	d := dispatch.New(st, runner.Local{}, dispatch.Options{DataDir: dir})
	ix := index.New(st, d.SetSnapshot)
	must(t, ix.SetBase(context.Background(), snap))
	secret := func(_ context.Context, name string) (string, error) {
		if name == "GITHUB_WEBHOOK_ACME" {
			return "ownersecret", nil
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
	return srv, st
}

func TestAnOwnersWebhookIsRoutedByRepositoryToTheReposJob(t *testing.T) {
	srv, st := ownerServer(t)
	url := srv.URL + "/api/v1/hooks/github/acme"
	if code, _ := post(t, url, "push", sign("wrong", sitePush), sitePush); code != http.StatusUnauthorized {
		t.Fatalf("a wrong signature: %d", code)
	}
	if code, _ := post(t, url, "push", "", sitePush); code != http.StatusUnauthorized {
		t.Fatalf("no signature: %d", code)
	}
	code, body := post(t, url, "push", sign("ownersecret", sitePush), sitePush)
	if code != http.StatusAccepted {
		t.Fatalf("%d %s", code, body)
	}
	var out struct {
		Builds []queued `json:"builds"`
	}
	must(t, json.Unmarshal([]byte(body), &out))
	if len(out.Builds) != 1 || out.Builds[0].Job != "acme.site.main" || out.Builds[0].Number != 1 {
		t.Fatalf("%s", body)
	}
	b, err := st.BuildByNumber(context.Background(), "acme.site.main", 1)
	must(t, err)
	if b.Trigger != "webhook" || b.Parent != "acme.site" {
		t.Fatalf("%+v", b)
	}
}

func TestAnOwnersDeliveryFromAnUnknownRepoIsIgnoredNotRefused(t *testing.T) {
	srv, st := ownerServer(t)
	other := `{"ref":"refs/heads/main","after":"` + sha + `","repository":{"full_name":"acme/other","default_branch":"main"}}`
	code, body := post(t, srv.URL+"/api/v1/hooks/github/acme", "push", sign("ownersecret", other), other)
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	if kids, err := st.Children(context.Background(), ""); err != nil || len(kids) != 0 {
		t.Fatalf("children %v %v", kids, err)
	}
	none := `{"zen":"x"}`
	if code, _ := post(t, srv.URL+"/api/v1/hooks/github/acme", "push", sign("ownersecret", none), none); code != http.StatusBadRequest {
		t.Fatalf("a body that names no repository: %d", code)
	}
}

func TestARepoNodesNameTakesNoDeliveryOfItsOwn(t *testing.T) {
	srv, _ := ownerServer(t)
	if code, _ := post(t, srv.URL+"/api/v1/hooks/github/acme.site", "push", sign("ownersecret", sitePush), sitePush); code != http.StatusUnauthorized && code != http.StatusNotFound {
		t.Fatalf("a node takes the owner's deliveries: %d", code)
	}
}
