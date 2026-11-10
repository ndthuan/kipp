package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

// withSite adds the multibranch job site to the job set, with the
// children the rows name; site's children are granted to the token
// returned, through a grant on site alone. A build of a child clones a
// path that is not there, and so never reaches a network.
func (e *env) withSite(t *testing.T, rows ...store.Child) string {
	t.Helper()
	ctx := context.Background()
	parent, err := config.ParseJob("site.yml", []byte("name: site\ngroup: web\nmultibranch: {repo: acme/site, clone: /nonexistent/site.git}\n"))
	must(t, err)
	snap := e.api.deps.Dispatch.Snapshot()
	jobs := []*config.Job{parent}
	for _, c := range rows {
		must(t, e.st.PutChild(ctx, c))
		if !c.FromFork {
			child := parent.Child(config.ChildRecord{Parent: c.Parent, Key: c.Key, Kind: c.Kind, Branch: c.Branch, PR: c.PR, State: c.State})
			child.DefaultBranch = "main"
			jobs = append(jobs, child)
		}
	}
	e.api.deps.Dispatch.SetSnapshot(snap.WithJobs(jobs...))
	return token(t, e.st, "site-dev", false, map[string][]string{"site": {"view"}})
}

func TestAGoneChildsBuildsAreReadableWithTheParentsGrantAfterTheParentFileIsRemoved(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	now := time.Now()
	tok := e.withSite(t, store.Child{Parent: "site", Key: "main", Kind: "branch", Branch: "main", State: "gone", GoneAt: &now, LastSeenAt: now})
	b := &store.Build{Job: "site.main", Parent: "site", Status: store.Success, Trigger: "webhook", TriggerBranch: "main", BuiltCommit: "abc123"}
	must(t, e.st.CreateBuild(ctx, b))
	// The parent's file goes, and its child with it.
	before := e.api.deps.Dispatch.Snapshot()
	e.api.deps.Dispatch.SetSnapshot(&config.Snapshot{Jobs: map[string]*config.Job{"smoke": before.Jobs["smoke"]}, Names: []string{"smoke"}})
	got := e.object(t, http.StatusOK, tok, "GET", "/api/v1/builds/"+strconv.FormatInt(b.ID, 10), "")
	if got["job"] != "site.main" || got["parent"] != "site" || got["built_commit"] != "abc123" || got["branch"] != "main" {
		t.Fatalf("%v", got)
	}
	detail := e.object(t, http.StatusOK, tok, "GET", "/api/v1/jobs/site.main", "")
	if detail["removed"] != true || detail["parent"] != "site" {
		t.Fatalf("%v", detail)
	}
	list := e.list(t, tok, "/api/v1/jobs")
	if len(list) != 1 || list[0]["name"] != "site.main" || list[0]["removed"] != true {
		t.Fatalf("a child whose parent is gone is listed as a removed job: %v", list)
	}
	// Somebody with no grant on the parent sees nothing of it.
	e.call(t, http.StatusNotFound, e.viewer, "GET", "/api/v1/builds/"+strconv.FormatInt(b.ID, 10), "")
	e.call(t, http.StatusNotFound, e.viewer, "GET", "/api/v1/jobs/site.main", "")
}

func TestAParentListsItsChildrenAndTheJobsListShowsItOnce(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	now := time.Now()
	tok := e.withSite(t,
		store.Child{Parent: "site", Key: "main", Kind: "branch", Branch: "main", HeadSHA: "abc", State: "live", LastSeenAt: now},
		store.Child{Parent: "site", Key: "pr-12", Kind: "pr", Branch: "feature/login", PR: 12, Title: "Log in", Author: "ann", URL: "https://github.example/acme/site/pull/12", BaseBranch: "main", State: "live", LastSeenAt: now},
		store.Child{Parent: "site", Key: "pr-3", Kind: "pr", Branch: "old", PR: 3, State: "gone", GoneAt: &now, LastSeenAt: now},
		store.Child{Parent: "site", Key: "pr-9", Kind: "pr", Branch: "patch", PR: 9, Title: "From afar", FromFork: true, State: "live", LastSeenAt: now},
	)
	must(t, e.st.CreateBuild(ctx, &store.Build{Job: "site.main", Parent: "site", Status: store.Success, Trigger: "webhook"}))
	must(t, e.st.CreateBuild(ctx, &store.Build{Job: "site.pr-12", Parent: "site", Status: store.Failed, Trigger: "webhook", PR: 12}))
	list := e.list(t, tok, "/api/v1/jobs")
	if len(list) != 1 || list[0]["name"] != "site" || list[0]["children_count"] != float64(2) || list[0]["worst"] != "failed" {
		t.Fatalf("%v", list)
	}
	kids := e.list(t, tok, "/api/v1/jobs/site/children")
	text := fmt.Sprint(kids)
	order := []string{}
	for _, k := range kids {
		order = append(order, k["key"].(string)+":"+k["state"].(string))
	}
	if len(kids) != 4 || order[0] != "main:live" || order[1] != "pr-12:live" || order[2] != "pr-3:gone" || order[3] != "pr-9:live" ||
		kids[3]["from_fork"] != true || kids[3]["job"] != "" || kids[1]["title"] != "Log in" || kids[1]["last"].(map[string]any)["status"] != "failed" {
		t.Fatalf("%s", text)
	}
	if kids[1]["author"] != "ann" || kids[1]["url"] != "https://github.example/acme/site/pull/12" || kids[0]["author"] != nil || kids[0]["url"] != nil {
		t.Fatalf("a pull request lists who opened it and its page, a branch neither: %s", text)
	}
	e.call(t, http.StatusNotFound, tok, "GET", "/api/v1/jobs/smoke/children", "")
	e.call(t, http.StatusNotFound, e.viewer, "GET", "/api/v1/jobs/site/children", "")
	// Building the parent builds its default branch's child.
	b := e.object(t, http.StatusCreated, e.admin, "POST", "/api/v1/jobs/site/builds", "")
	if b["job"] != "site.main" {
		t.Fatalf("%v", b)
	}
}

func TestASourceJobsBuildShowsTheCommitItBuiltAndNothingElseOfAChild(t *testing.T) {
	e := newEnv(t)
	b := &store.Build{Job: "smoke", Status: store.Success, Trigger: "cron", BuiltCommit: "abc123"}
	must(t, e.st.CreateBuild(context.Background(), b))
	id := strconv.FormatInt(b.ID, 10)
	got := e.object(t, http.StatusOK, e.admin, "GET", "/api/v1/builds/"+id, "")
	if got["built_commit"] != "abc123" {
		t.Fatalf("built_commit: %v", got)
	}
	for _, k := range []string{"parent", "branch", "pr", "base_branch", "base_commit"} {
		if got[k] != nil {
			t.Errorf("%s is %v, want null for a build that is not a child's", k, got[k])
		}
	}
}

func TestAJobIsRunningWhenItOrAChildHasABuildRunningOrQueued(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	now := time.Now()
	e.withSite(t, store.Child{Parent: "site", Key: "main", Kind: "branch", Branch: "main", State: "live", LastSeenAt: now})
	must(t, e.st.CreateBuild(ctx, &store.Build{Job: "site.main", Parent: "site", Status: store.Failed, Trigger: "webhook"}))
	must(t, e.st.CreateBuild(ctx, &store.Build{Job: "smoke", Status: store.Queued, Trigger: "cli"}))
	must(t, e.st.CreateBuild(ctx, &store.Build{Job: "quiet", Status: store.Success, Trigger: "cli"}))
	running := func() map[string]bool {
		jobs, err := e.api.Service().Jobs(ctx, e.user(t, "admin"))
		must(t, err)
		out := map[string]bool{}
		for _, j := range jobs {
			out[j.Name] = j.Running
		}
		return out
	}
	if got := running(); !got["smoke"] || got["site"] || got["quiet"] {
		t.Fatalf("a queued build counts, a parent whose child is red does not: %v", got)
	}
	must(t, e.st.CreateBuild(ctx, &store.Build{Job: "site.main", Parent: "site", Status: store.Running, Trigger: "webhook"}))
	if got := running(); !got["site"] {
		t.Fatalf("a parent runs when a child does: %v", got)
	}
}

func TestAJobSummaryStatusIsTheOneItsRowShows(t *testing.T) {
	failed := &store.Build{Status: store.Failed}
	for name, tc := range map[string]struct {
		j    JobSummary
		want store.Status
	}{
		"never built":      {JobSummary{}, ""},
		"its last build":   {JobSummary{Last: failed}, store.Failed},
		"a parent's worst": {JobSummary{Children: 2, Worst: store.Timeout}, store.Timeout},
		"own build first":  {JobSummary{Children: 2, Worst: store.Timeout, Last: &store.Build{Status: store.Success}}, store.Success},
	} {
		if got := tc.j.Status(); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}
