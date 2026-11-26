package dispatch

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

const sideBuildsRule = "params:\n  EXTRA: {type: choice, options: ['off', 'on']}\nside_builds: {params: {EXTRA: ['on']}}\n"

func (h *harness) sideBuild(t *testing.T, job string) *store.Build {
	t.Helper()
	b, err := h.d.Trigger(context.Background(), TriggerRequest{Job: job, Kind: "api", By: "ada", Params: map[string]string{"EXTRA": "on"}})
	if err != nil {
		t.Fatal(err)
	}
	return h.wait(t, b.ID)
}

func TestASideBuildOfTheDefaultBranchPostsNoStatusAndStartsNoAfterJob(t *testing.T) {
	r := newRepo(t)
	sha := r.commit("main", map[string]string{".kipp.yml": oneStage("true")})
	p := &poster{}
	h := newHarness(t, map[string]string{"site": tokenParent("file://"+r.dir, "") + sideBuildsRule, "down": downAfter("site")}, func(o *Options) {
		o.Status = p.post
		o.PublicURL = "https://ci.example"
	})
	h.withChildren(t, config.ChildRecord{Key: "main", Branch: "main"})
	if b := h.sideBuild(t, "site.main"); b.Status != store.Success || b.BuiltCommit != sha {
		t.Fatalf("the side build ended %s at %q", b.Status, b.BuiltCommit)
	}
	if got := p.all(); len(got) != 0 {
		t.Fatalf("a side build posted %+v", got)
	}
	if bs := h.downs(t); len(bs) != 0 {
		t.Fatalf("a side build started %d builds of down", len(bs))
	}
	if b := h.wait(t, h.pushed(t, sha).ID); b.Status != store.Success {
		t.Fatalf("the push's build ended %s", b.Status)
	}
	if got := p.states(); !slices.Equal(got, []string{"pending", "success"}) || p.all()[1].Context != "kipp/site" {
		t.Fatalf("the push's build posted %+v, want pending and success under kipp/site", p.all())
	}
	if bs := h.downs(t); len(bs) != 1 {
		t.Fatalf("the push's build started %d builds of down, want one", len(bs))
	}
}

func TestASideBuildOfAnOwnerJobsRepoPostsNoStatusAndStartsNoAfterJob(t *testing.T) {
	r := newRepo(t)
	sha := r.commit("main", map[string]string{".kipp.yml": oneStage("true")})
	p := &poster{}
	h := newHarness(t, map[string]string{"acme": "name: acme\nowner: {login: acme, token: GH_TOKEN}\n" + sideBuildsRule, "down": downAfter("acme.site")}, func(o *Options) {
		o.Status = p.post
	})
	h.withRepo(t, "file://"+r.dir, config.ChildRecord{Key: "main", Kind: config.KindBranch, Branch: "main"})
	if b := h.sideBuild(t, "acme.site.main"); b.Status != store.Success {
		t.Fatalf("the side build ended %s", b.Status)
	}
	if got := p.all(); len(got) != 0 {
		t.Fatalf("a side build posted %+v", got)
	}
	if bs := h.downs(t); len(bs) != 0 {
		t.Fatalf("a side build started %d builds of down", len(bs))
	}
	b, err := h.d.Trigger(context.Background(), TriggerRequest{Job: "acme.site.main", Kind: "webhook", Detail: sha, Branch: "main", DefaultBranch: "main", By: "github"})
	if err != nil {
		t.Fatal(err)
	}
	if b = h.wait(t, b.ID); b.Status != store.Success {
		t.Fatalf("the push's build ended %s", b.Status)
	}
	if got := p.all(); len(got) != 2 || got[1].State != "success" || got[1].Context != "kipp/acme" {
		t.Fatalf("the push's build posted %+v, want pending and success under kipp/acme", got)
	}
	if bs := h.downs(t); len(bs) != 1 {
		t.Fatalf("the push's build started %d builds of down, want one", len(bs))
	}
}

// A side build said nothing to GitHub, so a commit whose last build was
// cancelled never gets its state back as the verdict before.
func TestTheVerdictBeforeIsNeverASideBuilds(t *testing.T) {
	const sha = "89abcdef0123456789abcdef0123456789abcdef"
	h := newHarness(t, map[string]string{"site": parentFile("file:///nowhere", sideBuildsRule)})
	h.withChildren(t, prChild(config.KindPR, "pr-7"))
	ctx := context.Background()
	old := h.ended(t, sha, store.Success, true)
	side := &store.Build{Job: "site.pr-7", Status: store.Running, Trigger: "api", BuiltCommit: sha, Params: map[string]string{"EXTRA": "on"}}
	if err := h.st.CreateBuild(ctx, side); err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetBuildStatus(ctx, side.ID, store.Failed, time.Now()); err != nil {
		t.Fatal(err)
	}
	now := h.ended(t, sha, store.Cancelled, true)
	got, state := h.d.verdictBefore(ctx, now, h.d.Snapshot().Jobs["site.pr-7"])
	if got == nil || got.ID != old.ID || state != "success" {
		t.Fatalf("the verdict before is %+v, %q; want #%d, success", got, state, old.Number)
	}
}

func downAfter(up string) string {
	return "name: down\ntriggers: {after: [" + up + "]}\nstages:\n  s: {run: 'true'}\n"
}
