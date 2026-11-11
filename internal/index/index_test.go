package index

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/retain"
	"github.com/ndthuan/kipp/internal/store"
)

var ctx = context.Background()

const (
	head  = "0123456789abcdef0123456789abcdef01234567"
	head2 = "89abcdef0123456789abcdef0123456789abcdef"
)

type fixture struct {
	t     *testing.T
	x     *Index
	st    *store.Store
	jobs  string
	mu    sync.Mutex
	snap  *config.Snapshot
	clock time.Time
}

func newFixture(t *testing.T, files map[string]string) *fixture {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "db"))
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	f := &fixture{t: t, st: st, jobs: filepath.Join(dir, "jobs"), clock: time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)}
	must(t, os.MkdirAll(f.jobs, 0o750))
	f.x = New(st, func(s *config.Snapshot) { f.mu.Lock(); f.snap = s; f.mu.Unlock() })
	f.x.now = func() time.Time { return f.clock }
	f.load(files)
	return f
}

// load writes the job files, removing the ones before, and hands them to
// the index as a reload would.
func (f *fixture) load(files map[string]string) {
	f.t.Helper()
	entries, err := os.ReadDir(f.jobs)
	must(f.t, err)
	for _, e := range entries {
		must(f.t, os.Remove(filepath.Join(f.jobs, e.Name())))
	}
	for name, body := range files {
		must(f.t, os.WriteFile(filepath.Join(f.jobs, name+".yml"), []byte(body), 0o600))
	}
	snap, err := config.Load(f.jobs)
	must(f.t, err)
	must(f.t, f.x.SetBase(ctx, snap))
}

func (f *fixture) jobSet() *config.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap
}

func (f *fixture) push(branch, after string) *TriggerPlan {
	f.t.Helper()
	plan, err := f.x.Push(ctx, "site", PushEvent{Repo: "acme/site", Ref: "refs/heads/" + branch, After: after, DefaultBranch: "main"})
	must(f.t, err)
	return plan
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

const site = "name: site\nmultibranch: {repo: acme/site}\n"

func TestPushToTheDefaultBranchBuildsItsChildAtTheHead(t *testing.T) {
	f := newFixture(t, map[string]string{"site": site})
	plan := f.push("main", head)
	if len(plan.Builds) != 1 {
		t.Fatalf("plan %+v", plan)
	}
	b := plan.Builds[0]
	if b.Job != "site.main" || b.Kind != "webhook" || b.Detail != head || b.Branch != "main" || b.DefaultBranch != "main" {
		t.Fatalf("build %+v", b)
	}
	job := f.jobSet().Jobs["site.main"]
	if job == nil || job.Parent != "site" || !job.OnDefaultBranch() || job.Disabled {
		t.Fatalf("the child is not in the job set as the default branch's: %+v", job)
	}
	if f.jobSet().DefaultChild("site") != job {
		t.Fatal("the parent does not find its default branch's child")
	}
	// The child outlives a restart: it is read back from the store.
	f.x = New(f.st, func(s *config.Snapshot) { f.mu.Lock(); f.snap = s; f.mu.Unlock() })
	f.load(map[string]string{"site": site})
	if j := f.jobSet().Jobs["site.main"]; j == nil || !j.OnDefaultBranch() {
		t.Fatalf("after a restart: %+v", j)
	}
	if again := f.push("main", head2); again.Builds[0].Job != "site.main" || again.Builds[0].Detail != head2 {
		t.Fatalf("a second push to the branch made another child: %+v", again)
	}
	if plan, _ := f.x.Push(ctx, "site", PushEvent{Repo: "acme/other", Ref: "refs/heads/main", After: head}); plan.Ignored == "" {
		t.Fatal("a push to another repo builds")
	}
	if plan, _ := f.x.Push(ctx, "site", PushEvent{Repo: "acme/site", Ref: "refs/notes/commits", After: head}); plan.Ignored != "refs/notes/commits is neither a branch nor a tag" {
		t.Fatalf("a note: %+v", plan)
	}
}

func TestPushToABranchWithNoPRBuildsNothingByDefault(t *testing.T) {
	f := newFixture(t, map[string]string{"site": site})
	plan := f.push("feature/login", head)
	if len(plan.Builds) != 0 || !strings.Contains(plan.Ignored, "branch feature/login is not built") {
		t.Fatalf("plan %+v", plan)
	}
	if kids := f.x.Children("site"); len(kids) != 0 {
		t.Fatalf("a branch nothing builds got a child: %+v", kids)
	}
}

func TestPushToAMatchingBranchBuildsItWhenBranchesSaysSo(t *testing.T) {
	f := newFixture(t, map[string]string{"site": "name: site\nmultibranch: {repo: acme/site, branches: [default, \"release/*\"]}\n"})
	plan := f.push("release/2.0", head)
	if len(plan.Builds) != 1 || !strings.HasPrefix(plan.Builds[0].Job, "site.release-2.0-") || plan.Builds[0].Branch != "release/2.0" {
		t.Fatalf("plan %+v", plan)
	}
	if plan := f.push("feature/x", head); len(plan.Builds) != 0 {
		t.Fatalf("a branch the patterns do not name built: %+v", plan)
	}
	every := newFixture(t, map[string]string{"site": "name: site\nmultibranch: {repo: acme/site, branches: [\"*\"]}\n"})
	if plan := every.push("PEN-3-x", head); len(plan.Builds) != 1 || plan.Builds[0].Job == "site.PEN-3-x" {
		t.Fatalf("[\"*\"] builds every branch, under a key that is a job name: %+v", plan)
	}
	job := every.jobSet().Jobs[every.push("PEN-3-x", head2).Builds[0].Job]
	if job == nil || job.OnDefaultBranch() || job.Origin.Branch != "PEN-3-x" {
		t.Fatalf("%+v", job)
	}
}

func TestDeletedBranchMakesItsChildGone(t *testing.T) {
	f := newFixture(t, map[string]string{"site": "name: site\nmultibranch: {repo: acme/site, branches: [\"*\"]}\n"})
	f.push("feature-x", head)
	plan, err := f.x.Push(ctx, "site", PushEvent{Repo: "acme/site", Ref: "refs/heads/feature-x", After: strings.Repeat("0", 40), Deleted: true, DefaultBranch: "main"})
	must(t, err)
	if len(plan.Builds) != 0 || plan.Ignored != "branch feature-x was deleted, and site.feature-x is gone" {
		t.Fatalf("plan %+v", plan)
	}
	kids := f.x.Children("site")
	if len(kids) != 1 || kids[0].State != config.ChildGone || kids[0].GoneAt == nil || !kids[0].GoneAt.Equal(f.clock) {
		t.Fatalf("%+v", kids)
	}
	if job := f.jobSet().Jobs["site.feature-x"]; job == nil || !job.Disabled {
		t.Fatalf("a gone child is listed and builds no more: %+v", job)
	}
	// Pushed again, it is the same child again.
	if plan := f.push("feature-x", head2); len(plan.Builds) != 1 || plan.Builds[0].Job != "site.feature-x" {
		t.Fatalf("%+v", plan)
	}
	if job := f.jobSet().Jobs["site.feature-x"]; job.Disabled {
		t.Fatal("a branch pushed again stays gone")
	}
}

func TestGoneChildLeavesTheSetAfterOrphansDaysAndKeepsItsBuilds(t *testing.T) {
	f := newFixture(t, map[string]string{"site": "name: site\nmultibranch: {repo: acme/site, branches: [\"*\"], orphans: {days: 2}}\n"})
	f.push("feature-x", head)
	f.push("feature-y", head)
	must(t, f.st.CreateBuild(ctx, &store.Build{Job: "site.feature-x", Parent: "site", Status: store.Success, Trigger: "webhook", QueuedAt: f.clock}))
	for _, b := range []string{"feature-x", "feature-y"} {
		_, err := f.x.Push(ctx, "site", PushEvent{Repo: "acme/site", Ref: "refs/heads/" + b, Deleted: true})
		must(t, err)
	}
	f.clock = f.clock.Add(47 * time.Hour)
	must(t, f.x.Sweep(ctx))
	if f.jobSet().Jobs["site.feature-x"] == nil {
		t.Fatal("a gone child left the set before orphans.days")
	}
	f.clock = f.clock.Add(2 * time.Hour)
	must(t, f.x.Sweep(ctx))
	set := f.jobSet()
	if set.Jobs["site.feature-x"] != nil || slices.Contains(set.Names, "site.feature-x") {
		t.Fatal("a gone child is still a job after orphans.days")
	}
	parents, err := f.st.Parents(ctx)
	must(t, err)
	if parents["site.feature-x"] != "site" || parents["site.feature-y"] != "" {
		t.Fatalf("the row of the child with a build went, or the one with none stayed: %v", parents)
	}
	// Its builds are a removed job's: kept for RemovedKeepDays.
	n, err := retain.Sweep(ctx, f.st, set, filepath.Join(t.TempDir(), "logs"), f.clock)
	must(t, err)
	if bs, _ := f.st.Builds(ctx, "site.feature-x", 5); n != 0 || len(bs) != 1 {
		t.Fatalf("the sweep removed %d builds, %d are left", n, len(bs))
	}
	if _, err := retain.Sweep(ctx, f.st, set, filepath.Join(t.TempDir(), "logs"), f.clock.AddDate(0, 0, config.RemovedKeepDays+1)); err != nil {
		t.Fatal(err)
	}
	must(t, f.x.Sweep(ctx))
	if parents, _ := f.st.Parents(ctx); len(parents) != 0 {
		t.Fatalf("the row outlived the child's last build: %v", parents)
	}
}

func TestAChildOfAParentWhoseFileIsRemovedIsNoJobAndKeepsItsRow(t *testing.T) {
	f := newFixture(t, map[string]string{"site": site})
	f.push("main", head)
	must(t, f.st.CreateBuild(ctx, &store.Build{Job: "site.main", Parent: "site", Status: store.Success, Trigger: "webhook"}))
	f.load(map[string]string{"other": "name: other\nstages:\n  s: {run: 'true'}\n"})
	must(t, f.x.Sweep(ctx))
	if f.jobSet().Jobs["site.main"] != nil {
		t.Fatal("a child of no parent is a job")
	}
	if p, err := f.st.ParentOf(ctx, "site.main"); err != nil || p != "site" {
		t.Fatalf("the row of a child with builds went with its parent's file: %q %v", p, err)
	}
	f.load(map[string]string{"site": site})
	if f.jobSet().Jobs["site.main"] == nil {
		t.Fatal("the parent came back without its child")
	}
}
