package dispatch

import (
	"context"
	"errors"
	"testing"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

var tagsChild = config.ChildRecord{Key: config.TagsKey, Kind: config.KindTag}

const tagStages = "stages:\n" +
	"  s: {run: 'echo \"$KIPP_JOB [$KIPP_BRANCH] $KIPP_TAG $KIPP_DEFAULT_BRANCH $KIPP_COMMIT $KIPP_REF\"'}\n" +
	"  release: {run: 'true', when: {tags: [\"v*\"]}}\n" +
	"  main_only: {run: 'true', when: {default_branch: true}}\n"

// tagged is a build of the tags' child that the push of tag at commit queued.
func (h *harness) tagged(t *testing.T, tag, commit string) *store.Build {
	t.Helper()
	b, err := h.d.Trigger(context.Background(), TriggerRequest{Job: "site.tags", Kind: "webhook", Detail: commit, Tag: tag, DefaultBranch: "main", By: "github"})
	if err != nil {
		t.Fatal(err)
	}
	return h.wait(t, b.ID)
}

func TestATagBuildBuildsTheTaggedCommitWithKippTagAndNoBranch(t *testing.T) {
	r := newRepo(t)
	tagged := r.commit("main", map[string]string{".kipp.yml": tagStages})
	r.git("tag", "-a", "v1.2.0", "-m", "release")
	r.commit("main", nil) // the branch moved on since
	h := newHarness(t, map[string]string{"site": parentFile(r.dir, "")})
	h.withChildren(t, config.ChildRecord{Key: "main", Branch: "main"}, tagsChild)
	b := h.tagged(t, "v1.2.0", tagged)
	if got, want := h.log(t, b, "s"), "site.tags [] v1.2.0 main "+tagged+" refs/tags/v1.2.0\n"; b.Status != store.Success || got != want {
		t.Fatalf("%s: %q, want %q", b.Status, got, want)
	}
	st := h.stages(t, b.ID)
	if st["release"].Status != store.StageSuccess || st["main_only"].Status != store.StageSkipped {
		t.Fatalf("when: release %s, main_only %s", st["release"].Status, st["main_only"].Status)
	}
	if b.BuiltCommit != tagged || b.TriggerTag != "v1.2.0" || b.TriggerBranch != "" || b.Parent != "site" {
		t.Fatalf("the build records %+v", b)
	}
}

func TestATagsChildBuildsNothingByHandAndBuildAgainRebuildsTheSameTag(t *testing.T) {
	r := newRepo(t)
	first := r.commit("main", map[string]string{".kipp.yml": tagStages})
	r.git("tag", "v1", first)
	h := newHarness(t, map[string]string{"site": parentFile(r.dir, "")})
	h.withChildren(t, tagsChild)
	if _, err := h.d.Trigger(context.Background(), TriggerRequest{Job: "site.tags", Kind: "api", By: "ann"}); !errors.Is(err, ErrNoTag) {
		t.Fatalf("a tags' child built by hand: %v", err)
	}
	o := h.tagged(t, "v1", first)
	r.commit("main", nil)
	again, err := h.d.Again(context.Background(), o.ID, "web", "ann")
	if err != nil {
		t.Fatal(err)
	}
	b := h.wait(t, again.ID)
	if b.Status != store.Success || b.BuiltCommit != first || b.TriggerTag != "v1" {
		t.Fatalf("built again: %s at %q, tag %q", b.Status, b.BuiltCommit, b.TriggerTag)
	}
}

func TestAGreenTagBuildIsKeptAndARedOneIsNot(t *testing.T) {
	r := newRepo(t)
	green := r.commit("main", map[string]string{".kipp.yml": oneStage("true")})
	red := r.commit("main", map[string]string{".kipp.yml": oneStage("false")})
	h := newHarness(t, map[string]string{"site": parentFile(r.dir, "")})
	h.withChildren(t, tagsChild)
	ok := h.tagged(t, "v1", green)
	bad := h.tagged(t, "v2", red)
	for b, kept := range map[*store.Build]bool{ok: true, bad: false} {
		got, err := h.st.Build(context.Background(), b.ID)
		if err != nil {
			t.Fatal(err)
		}
		if (got.KeptAt != nil) != kept || kept && got.KeptBy != "kipp" {
			t.Errorf("tag %s (%s): kept by %q at %v, want kept %v", got.TriggerTag, got.Status, got.KeptBy, got.KeptAt, kept)
		}
	}
}

func TestAfterAParentFiresOnAGreenTagBuildWithTheUpstreamTag(t *testing.T) {
	r := newRepo(t)
	green := r.commit("main", map[string]string{".kipp.yml": oneStage("true")})
	red := r.commit("main", map[string]string{".kipp.yml": oneStage("false")})
	h := newHarness(t, map[string]string{
		"site": parentFile(r.dir, ""),
		"down": "name: down\ntriggers: {after: [site]}\nstages:\n  s: {run: 'echo $KIPP_UPSTREAM_BUILD $KIPP_UPSTREAM_STATUS $KIPP_UPSTREAM_COMMIT [$KIPP_UPSTREAM_TAG]'}\n",
	})
	h.withChildren(t, config.ChildRecord{Key: "main", Branch: "main"}, tagsChild)
	if b := h.tagged(t, "v2", red); b.Status != store.Failed {
		t.Fatalf("the red tag build: %s", b.Status)
	}
	if bs := h.downs(t); len(bs) != 0 {
		t.Fatalf("a red tag build started %d builds of down", len(bs))
	}
	h.tagged(t, "v1", green)
	bs := h.downs(t)
	if len(bs) != 1 {
		t.Fatalf("down ran %d times after a green tag build, want once", len(bs))
	}
	down := h.wait(t, bs[0].ID)
	if got, want := h.log(t, down, "s"), "site.tags#2 success "+green+" [v1]\n"; down.UpstreamTag != "v1" || got != want {
		t.Fatalf("upstream tag %q, log %q, want %q", down.UpstreamTag, got, want)
	}
}

func TestADefaultBranchUpstreamLeavesTheUpstreamTagEmpty(t *testing.T) {
	r := newRepo(t)
	r.commit("main", map[string]string{".kipp.yml": oneStage("true")})
	h := newHarness(t, map[string]string{
		"site": parentFile(r.dir, ""),
		"down": "name: down\ntriggers: {after: [site]}\nstages:\n  s: {run: 'echo [$KIPP_UPSTREAM_TAG]'}\n",
	})
	h.withChildren(t, config.ChildRecord{Key: "main", Branch: "main"})
	h.wait(t, h.trigger(t, "site.main").ID)
	bs := h.downs(t)
	if len(bs) != 1 {
		t.Fatalf("down ran %d times", len(bs))
	}
	if got := h.log(t, h.wait(t, bs[0].ID), "s"); got != "[]\n" {
		t.Fatalf("%q", got)
	}
}

func TestATagBuildPostsItsStatusUnderItsOwnContext(t *testing.T) {
	r := newRepo(t)
	sha := r.commit("main", map[string]string{".kipp.yml": oneStage("true")})
	p := &poster{}
	h := statusHarness(t, p, tokenParent(r.dir, ""))
	h.withChildren(t, config.ChildRecord{Key: "main", Branch: "main"}, tagsChild)
	h.tagged(t, "v1", sha)
	h.wait(t, h.pushed(t, sha).ID)
	contexts := map[string]string{}
	for _, s := range p.all() {
		contexts[s.Job] = s.Context
	}
	if contexts["site.tags"] != "kipp/site/tag" || contexts["site.main"] != "kipp/site" {
		t.Fatalf("contexts %v", contexts)
	}
}
