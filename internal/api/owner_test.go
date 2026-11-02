package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/dispatch"
	"github.com/ndthuan/kipp/internal/index"
	"github.com/ndthuan/kipp/internal/store"
)

// withOwner adds the owner job acme, whose repo acme/site is live and
// acme/old is gone, with a build of the branch main of site, and returns
// the token of a user granted view and build on the owner job alone.
func (e *env) withOwner(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	gh := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(gh.Close)
	owner, err := config.ParseJob("acme.yml", []byte("name: acme\ngroup: web\nowner: {login: acme, token: GH_TOKEN, scan: 0}\n"))
	must(t, err)
	must(t, e.st.PutOwnerRepo(ctx, store.OwnerRepo{Owner: "acme", Key: "site", Repo: "acme/site", State: config.RepoLive, LastSeenAt: now, Hook: store.HookOK}))
	must(t, e.st.PutOwnerRepo(ctx, store.OwnerRepo{Owner: "acme", Key: "old", Repo: "acme/old", State: config.RepoGone, GoneAt: &now, LastSeenAt: now, Hook: store.HookOff, HookNote: "no --base-url"}))
	must(t, e.st.PutChild(ctx, store.Child{Parent: "acme.site", Key: "main", Kind: "branch", Branch: "main", HeadSHA: "abc", State: "live", LastSeenAt: now}))
	ix := index.New(e.st, e.api.deps.Dispatch.SetSnapshot)
	must(t, ix.SetBase(ctx, e.api.deps.Dispatch.Snapshot().WithJobs(owner)))
	sc := index.NewScanner(ctx, ix, index.ScannerOptions{
		API:     gh.URL,
		Token:   func(context.Context, string) (string, error) { return "t", nil },
		Trigger: func(context.Context, dispatch.TriggerRequest) (*store.Build, error) { return &store.Build{}, nil },
	})
	t.Cleanup(sc.Wait)
	e.api.deps.Scanner = sc
	must(t, e.st.CreateBuild(ctx, &store.Build{Job: "acme.site.main", Parent: "acme.site", Status: store.Success, Trigger: "webhook", TriggerBranch: "main"}))
	return token(t, e.st, "acme-dev", false, map[string][]string{"acme": {"view", "build"}})
}

func TestAnOwnerJobListsOnceWithItsReposAndItsGrantReachesEveryRepoBuild(t *testing.T) {
	e := newEnv(t)
	tok := e.withOwner(t)
	list := e.list(t, tok, "/api/v1/jobs")
	if len(list) != 1 || list[0]["name"] != "acme" {
		t.Fatalf("the owner job is listed alone, not its repos' jobs: %v", list)
	}
	d := e.object(t, http.StatusOK, tok, "GET", "/api/v1/jobs/acme", "")
	owner, _ := d["owner"].(map[string]any)
	if owner == nil || owner["login"] != "acme" {
		t.Fatalf("%v", d)
	}
	repos, _ := owner["repos"].([]any)
	if len(repos) != 2 {
		t.Fatalf("repos = %v", owner)
	}
	first, _ := repos[0].(map[string]any)
	if first["repo"] != "acme/old" && first["repo"] != "acme/site" || first["state"] == "" {
		t.Fatalf("repo = %v", first)
	}
	// A repo's job is reached by the owner job's grant, and by no other.
	node := e.object(t, http.StatusOK, tok, "GET", "/api/v1/jobs/acme.site", "")
	if node["folder"] != "acme" {
		t.Fatalf("%v", node)
	}
	e.call(t, http.StatusOK, tok, "GET", "/api/v1/jobs/acme.site.main", "")
	e.call(t, http.StatusNotFound, e.viewer, "GET", "/api/v1/jobs/acme.site.main", "")
	e.call(t, http.StatusNotFound, e.viewer, "GET", "/api/v1/jobs/acme", "")
	// The owner job builds nothing itself.
	e.call(t, http.StatusConflict, tok, "POST", "/api/v1/jobs/acme/builds", "")
}
