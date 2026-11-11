package dispatch

import (
	"context"
	"errors"
	"testing"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

func TestAnOwnerJobBuildsNothingItself(t *testing.T) {
	h := newHarness(t, map[string]string{"acme": "name: acme\nowner: {login: acme, token: GH_TOKEN}\n"})
	_, err := h.d.Trigger(context.Background(), TriggerRequest{Job: "acme", Kind: "manual"})
	if !errors.Is(err, ErrOwnerJob) {
		t.Fatalf("Trigger(owner job) = %v", err)
	}
}

func TestAStatusOfARepoOfAnOwnerJobIsSaidUnderTheOwnerJob(t *testing.T) {
	multi := &config.Multibranch{PullRequests: config.PRBoth}
	cases := map[string]*config.Job{
		"a parent's child":      {Parent: "site", Multibranch: multi, Origin: &config.ChildRecord{Kind: config.KindBranch}},
		"a repo of an owner":    {Parent: "acme.site", Folder: "acme", Multibranch: multi, Origin: &config.ChildRecord{Kind: config.KindBranch}},
		"its pull request head": {Parent: "acme.site", Folder: "acme", Multibranch: multi, Origin: &config.ChildRecord{Kind: config.KindPRHead}},
	}
	want := map[string]string{"a parent's child": "kipp/site", "a repo of an owner": "kipp/acme", "its pull request head": "kipp/acme/head"}
	for name, job := range cases {
		if got := statusContext(job); got != want[name] {
			t.Errorf("%s: %q, want %q", name, got, want[name])
		}
	}
}

// withRepo gives the dispatcher the node of acme's repo site, cloned from
// dir, and these children of it, the repo's default branch being main.
func (h *harness) withRepo(t *testing.T, dir string, recs ...config.ChildRecord) {
	t.Helper()
	snap := h.d.Snapshot()
	node := snap.Jobs["acme"].Node(config.RepoRecord{Owner: "acme", Key: "site", Repo: "acme/site", DefaultBranch: "main", State: config.RepoLive})
	node.Multibranch.Clone = dir
	jobs := []*config.Job{node}
	for _, rec := range recs {
		rec.Parent, rec.State = node.Name, config.ChildLive
		c := node.Child(rec)
		c.DefaultBranch = "main"
		jobs = append(jobs, c)
	}
	h.d.SetSnapshot(snap.WithJobs(jobs...))
}

func TestAfterAnOwnerJobsRepoNodeFiresOnItsDefaultBranchBuildOnly(t *testing.T) {
	r, _, head := prRepo(t, echoStages)
	main := r.git("rev-parse", "main")
	h := newHarness(t, map[string]string{
		"acme": "name: acme\nowner: {login: acme, token: GH_TOKEN}\n",
		"down": "name: down\ntriggers: {after: [acme.site]}\nstages:\n  s: {run: 'echo $KIPP_UPSTREAM_BUILD $KIPP_UPSTREAM_STATUS $KIPP_UPSTREAM_COMMIT'}\n",
	})
	h.withRepo(t, r.dir, config.ChildRecord{Key: "main", Kind: config.KindBranch, Branch: "main"}, prChild(config.KindPR, "pr-7"))
	if b := h.hook(t, "acme.site.pr-7", head); b.Status != store.Success {
		t.Fatalf("the pull request's build: %s", b.Status)
	}
	if bs := h.downs(t); len(bs) != 0 {
		t.Fatalf("a pull request's build started %d builds of down", len(bs))
	}
	b := h.wait(t, h.trigger(t, "acme.site.main").ID)
	if b.Status != store.Success || b.BuiltCommit != main {
		t.Fatalf("the default branch's build: %s at %q", b.Status, b.BuiltCommit)
	}
	bs := h.downs(t)
	if len(bs) != 1 {
		t.Fatalf("down ran %d times after acme.site, want once", len(bs))
	}
	down := h.wait(t, bs[0].ID)
	if down.UpstreamCommit != main {
		t.Fatalf("the downstream build records the upstream commit %q, want %q", down.UpstreamCommit, main)
	}
	if got, want := h.log(t, down, "s"), "acme.site.main#1 success "+main+"\n"; got != want {
		t.Fatalf("%q, want %q", got, want)
	}
}
