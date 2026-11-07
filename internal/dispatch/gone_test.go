package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

// gone is rec as the index has it once its pull request closed or its
// branch went.
func gone(rec config.ChildRecord) config.ChildRecord {
	rec.State, rec.GoneAt = config.ChildGone, time.Now()
	return rec
}

// activityOf is the job's activity lines, newest first.
func (h *harness) activityOf(t *testing.T, job string) []string {
	t.Helper()
	acts, err := h.st.Activity(context.Background(), job, 20)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, a := range acts {
		out = append(out, a.Line)
	}
	return out
}

func TestARunningBuildOfAPullRequestThatClosesIsCancelledSayingWhy(t *testing.T) {
	r, _, head := prRepo(t, oneStage("sleep 30"))
	h := newHarness(t, map[string]string{"site": parentFile("file://"+r.dir, "")})
	h.withChildren(t, prChild(config.KindPR, "pr-7"))
	b, err := h.d.Trigger(context.Background(), TriggerRequest{Job: "site.pr-7", Kind: "webhook", Detail: head, Branch: "feature-x", DefaultBranch: "main", By: "github"})
	if err != nil {
		t.Fatal(err)
	}
	h.untilStage(t, b.ID, "s", store.StageRunning)
	h.withChildren(t, gone(prChild(config.KindPR, "pr-7")))
	b = h.wait(t, b.ID)
	if st := h.stages(t, b.ID); b.Status != store.Cancelled || st["s"].Status != store.StageCancelled {
		t.Fatalf("%s, its stage %+v", b.Status, st["s"])
	}
	h.assertLogEnds(t, b, "kipp: cancelled: pull request #7 is closed\n")
	if acts := h.activityOf(t, "site.pr-7"); !slices.Contains(acts, "#1 cancelled: pull request #7 is closed") {
		t.Fatalf("the activity says %q", acts)
	}
}

func TestARunningBuildOfABranchThatGoesIsCancelledSayingWhy(t *testing.T) {
	r := newRepo(t)
	sha := r.commit("feature-x", map[string]string{".kipp.yml": oneStage("sleep 30")})
	h := newHarness(t, map[string]string{"site": parentFile("file://"+r.dir, "")})
	rec := config.ChildRecord{Key: "feature-x", Branch: "feature-x"}
	h.withChildren(t, rec)
	b, err := h.d.Trigger(context.Background(), TriggerRequest{Job: "site.feature-x", Kind: "webhook", Detail: sha, Branch: "feature-x", DefaultBranch: "main", By: "github"})
	if err != nil {
		t.Fatal(err)
	}
	h.untilStage(t, b.ID, "s", store.StageRunning)
	h.withChildren(t, gone(rec))
	b = h.wait(t, b.ID)
	if st := h.stages(t, b.ID); b.Status != store.Cancelled || st["s"].Status != store.StageCancelled {
		t.Fatalf("%s, its stage %+v", b.Status, st["s"])
	}
	h.assertLogEnds(t, b, "kipp: cancelled: branch feature-x is gone\n")
}

func TestAQueuedBuildOfAGoneChildIsCancelledNotFailed(t *testing.T) {
	block, open := gated(t)
	r, _, head := prRepo(t, oneStage(block))
	h := newHarness(t, map[string]string{"site": "name: site\nmultibranch: {repo: acme/site, clone: 'file://" + r.dir + "'}\nconcurrency: queue\n"})
	h.withChildren(t, prChild(config.KindPR, "pr-7"))
	first, err := h.d.Trigger(context.Background(), TriggerRequest{Job: "site.pr-7", Kind: "webhook", Detail: head, Branch: "feature-x", DefaultBranch: "main", By: "github"})
	if err != nil {
		t.Fatal(err)
	}
	h.untilStage(t, first.ID, "s", store.StageRunning)
	second, err := h.d.Trigger(context.Background(), TriggerRequest{Job: "site.pr-7", Kind: "api", By: "ada"})
	if err != nil {
		t.Fatal(err)
	}
	h.withChildren(t, gone(prChild(config.KindPR, "pr-7")))
	open()
	if b := h.wait(t, second.ID); b.Status != store.Cancelled || b.StartedAt != nil {
		t.Fatalf("the queued build ended %s, started %v", b.Status, b.StartedAt)
	}
	if b := h.wait(t, first.ID); b.Status != store.Cancelled {
		t.Fatalf("the running build ended %s", b.Status)
	}
	if acts := h.activityOf(t, "site.pr-7"); !slices.Contains(acts, "#2 cancelled before it started: pull request #7 is closed") {
		t.Fatalf("the activity says %q", acts)
	}
}

func TestABuildOfALiveChildIsNotCancelledWhenAnotherChildGoes(t *testing.T) {
	r, _, head := prRepo(t, oneStage("true"))
	h := newHarness(t, map[string]string{"site": parentFile("file://"+r.dir, "")})
	other := config.ChildRecord{Key: "pr-8", Kind: config.KindPR, Branch: "other", PR: 8, BaseBranch: "main"}
	h.withChildren(t, prChild(config.KindPR, "pr-7"), other)
	h.withChildren(t, prChild(config.KindPR, "pr-7"), gone(other))
	if b := h.hook(t, "site.pr-7", head); b.Status != store.Success {
		t.Fatalf("the live child's build ended %s", b.Status)
	}
}

// A pull request merged the moment its build went green moves its base,
// and a rebuild on the new base can start before GitHub says it closed: the
// rebuild it cancels says nothing of the code, so the head keeps the
// verdict of the build before it.
func TestACancelledBuildOfAGoneChildGivesTheCommitBackTheVerdictBeforeIt(t *testing.T) {
	again := filepath.Join(t.TempDir(), "again")
	r, _, head := prRepo(t, oneStage("if [ -f "+again+" ]; then sleep 30; fi"))
	p := &poster{}
	h := statusHarness(t, p, tokenParent("file://"+r.dir, ""))
	h.withChildren(t, prChild(config.KindPR, "pr-7"))
	if b := h.hook(t, "site.pr-7", head); b.Status != store.Success {
		t.Fatalf("the first build ended %s", b.Status)
	}
	if err := os.WriteFile(again, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := h.d.Trigger(context.Background(), TriggerRequest{Job: "site.pr-7", Kind: "webhook", Detail: head, Branch: "feature-x", DefaultBranch: "main", By: "github"})
	if err != nil {
		t.Fatal(err)
	}
	h.untilStage(t, b.ID, "s", store.StageRunning)
	h.withChildren(t, gone(prChild(config.KindPR, "pr-7")))
	if b = h.wait(t, b.ID); b.Status != store.Cancelled {
		t.Fatalf("the rebuild ended %s", b.Status)
	}
	got := p.all()
	if states := p.states(); !slices.Equal(states, []string{"pending", "success", "pending", "success"}) {
		t.Fatalf("posted %v", states)
	}
	last := got[len(got)-1]
	if last.SHA != head || last.Description != "site.pr-7 #1 success" || last.TargetURL != "https://ci.example/jobs/site.pr-7/1" {
		t.Fatalf("the last status is %+v, want build #1's again", last)
	}
}

func TestACancelledBuildOfAGoneChildWithNoVerdictBeforeItPostsError(t *testing.T) {
	r, _, head := prRepo(t, oneStage("sleep 30"))
	p := &poster{}
	h := statusHarness(t, p, tokenParent("file://"+r.dir, ""))
	h.withChildren(t, prChild(config.KindPR, "pr-7"))
	b, err := h.d.Trigger(context.Background(), TriggerRequest{Job: "site.pr-7", Kind: "webhook", Detail: head, Branch: "feature-x", DefaultBranch: "main", By: "github"})
	if err != nil {
		t.Fatal(err)
	}
	h.untilStage(t, b.ID, "s", store.StageRunning)
	h.withChildren(t, gone(prChild(config.KindPR, "pr-7")))
	if b = h.wait(t, b.ID); b.Status != store.Cancelled {
		t.Fatalf("the build ended %s", b.Status)
	}
	if states := p.states(); !slices.Equal(states, []string{"pending", "error"}) {
		t.Fatalf("posted %v", states)
	}
	if last := p.all()[1]; !strings.HasSuffix(last.Description, "#1 cancelled") {
		t.Fatalf("the description is %q", last.Description)
	}
}

// ended is a build of site.pr-7 on sha that ended in status, having
// started its stage or not. It is made running, never queued, so that the
// dispatcher does not take it and build it for real first.
func (h *harness) ended(t *testing.T, sha string, status store.Status, stage bool) *store.Build {
	t.Helper()
	ctx := context.Background()
	b := &store.Build{Job: "site.pr-7", Status: store.Running, Trigger: "api", BuiltCommit: sha}
	if err := h.st.CreateBuild(ctx, b); err != nil {
		t.Fatal(err)
	}
	if stage {
		if err := h.st.PutStages(ctx, b.ID, []string{"s"}, nil); err != nil {
			t.Fatal(err)
		}
		if err := h.st.SetStage(ctx, b.ID, "s", store.StageRunning, nil, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.st.SetBuildStatus(ctx, b.ID, status, time.Now()); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTheVerdictBeforeIsTheStateThatBuildToldGitHub(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	for _, c := range []struct {
		name   string
		status store.Status
		stage  bool
		want   string
	}{
		{"a stage failed", store.Failed, true, "failure"},
		{"it failed before a stage", store.Failed, false, "error"},
		{"it timed out", store.Timeout, true, "failure"},
		{"it was unstable", store.Unstable, true, "success"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, map[string]string{"site": parentFile("file:///nowhere", "")})
			h.withChildren(t, prChild(config.KindPR, "pr-7"))
			old := h.ended(t, sha, c.status, c.stage)
			h.ended(t, sha, store.Cancelled, true) // said nothing, and is passed over
			now := h.ended(t, sha, store.Cancelled, true)
			got, state := h.d.verdictBefore(context.Background(), now)
			if got == nil || got.ID != old.ID || state != c.want {
				t.Fatalf("the verdict before is %+v, %q; want #%d, %q", got, state, old.Number, c.want)
			}
		})
	}
}
