package dispatch

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

func (h *harness) again(t *testing.T, id int64, kind string) *store.Build {
	t.Helper()
	b, err := h.d.Again(context.Background(), id, kind, "ada")
	if err != nil {
		t.Fatal(err)
	}
	return h.wait(t, b.ID)
}

func TestBuildAgainCopiesTheParamsAndNamesTheOriginal(t *testing.T) {
	h := newHarness(t, map[string]string{"j": "name: j\nparams:\n  flavor: {type: string, default: plain}\nstages:\n  s: {run: 'echo \"$flavor $KIPP_TRIGGER\"'}\n"})
	first, err := h.d.Trigger(context.Background(), TriggerRequest{Job: "j", Kind: "api", By: "bob", Params: map[string]string{"flavor": "spicy"}})
	if err != nil {
		t.Fatal(err)
	}
	h.wait(t, first.ID)
	b := h.again(t, first.ID, "web")
	if b.Number != 2 || b.Trigger != "web" || b.TriggerDetail != "again #1" || b.StartedBy != "ada" || b.Params["flavor"] != "spicy" {
		t.Fatalf("the new build is %+v", b)
	}
	if got := h.log(t, b, "s"); got != "spicy web\n" {
		t.Fatalf("the stage saw %q", got)
	}
	act, err := h.st.Activity(context.Background(), "j", 5)
	if err != nil || len(act) == 0 || act[0].Line != "#2 built again from #1 by ada" {
		t.Fatalf("activity %+v, %v", act, err)
	}
}

func TestBuildAgainDropsAParamTheJobLostAndDefaultsOneItGained(t *testing.T) {
	h := newHarness(t, map[string]string{"j": "name: j\nparams:\n  old: {type: string, default: o}\nstages:\n  s: {run: 'true'}\n"})
	first, err := h.d.Trigger(context.Background(), TriggerRequest{Job: "j", Kind: "api", Params: map[string]string{"old": "kept?"}})
	if err != nil {
		t.Fatal(err)
	}
	h.wait(t, first.ID)
	snap, err := config.Load(writeJobs(t, t.TempDir(), map[string]string{"j": "name: j\nparams:\n  fresh: {type: string, default: f}\nstages:\n  s: {run: 'true'}\n"}))
	if err != nil {
		t.Fatal(err)
	}
	h.d.SetSnapshot(snap)
	b := h.again(t, first.ID, "api")
	if len(b.Params) != 1 || b.Params["fresh"] != "f" {
		t.Fatalf("params %v, want only fresh=f", b.Params)
	}
}

func TestBuildAgainOfAWebhookBuildChecksOutTheSameCommitAndBranch(t *testing.T) {
	var refs []string
	h := newHarness(t, map[string]string{"src": "name: src\nsource: {repo: o/r, ref: main, path: kipp.yml}\nconcurrency: parallel\n"}, func(o *Options) {
		o.Clone = func(_ context.Context, dir, _, ref string, _ io.Writer) error {
			refs = append(refs, ref)
			return os.WriteFile(filepath.Join(dir, "kipp.yml"), []byte("stages:\n"+
				"  s: {run: 'set -u; echo \"$KIPP_TRIGGER [$KIPP_COMMIT] $KIPP_BRANCH $KIPP_DEFAULT_BRANCH\"'}\n"+
				"  feature: {needs: [s], when: {branches: [feat*], default_branch: false}, run: echo feature}\n"+
				"  default: {needs: [s], when: {default_branch: true}, run: echo default}\n"), 0o600)
		}
	})
	pushed, err := h.d.Trigger(context.Background(), TriggerRequest{Job: "src", Kind: "webhook", Detail: "abc123", Branch: "feat-x", DefaultBranch: "main", By: "github"})
	if err != nil {
		t.Fatal(err)
	}
	h.wait(t, pushed.ID)
	b := h.again(t, pushed.ID, "web")
	if b.Status != store.Success || len(refs) != 2 || refs[1] != "abc123" {
		t.Fatalf("%s: cloned %v, want the pushed commit twice", b.Status, refs)
	}
	if got, want := h.log(t, b, "s"), "web [abc123] feat-x main\n"; got != want {
		t.Fatalf("the stage saw %q, want %q", got, want)
	}
	st := h.stages(t, b.ID)
	if st["feature"].Status != store.StageSuccess || st["default"].Status != store.StageSkipped {
		t.Fatalf("branch conditions read %s and %s, want them as in the original", st["feature"].Status, st["default"].Status)
	}
	again := h.again(t, b.ID, "api")
	if again.TriggerDetail != "again #2" || len(refs) != 3 || refs[2] != "abc123" {
		t.Fatalf("a build of a build again: %q, cloned %v", again.TriggerDetail, refs)
	}
}

func TestBuildAgainOfAPullRequestRedoesTheMergeAgainstTheRecordedBase(t *testing.T) {
	r, base, head := prRepo(t, treeStages)
	h := newHarness(t, map[string]string{"site": parentFile("file://"+r.dir, "")})
	h.withChildren(t, prChild(config.KindPR, "pr-7"))
	first := h.hook(t, "site.pr-7", head)
	r.commit("main", map[string]string{"later.txt": "pushed after the first build\n"})
	b := h.again(t, first.ID, "web")
	got := h.log(t, b, "s")
	if b.Status != store.Success || strings.Contains(got, "later.txt") {
		t.Fatalf("%s: the merge saw %q, want the base as it was", b.Status, got)
	}
	if b.BuiltCommit != head || b.BaseCommit != base || b.PR != 7 || b.BaseBranch != "main" {
		t.Fatalf("the new build records %+v, want head %s into base %s", b, head, base)
	}
}

func TestBuildAgainOfABranchChildBuildsTheSameCommit(t *testing.T) {
	r := newRepo(t)
	first := r.commit("main", map[string]string{".kipp.yml": echoStages})
	h := newHarness(t, map[string]string{"site": parentFile(r.dir, "")})
	h.withChildren(t, config.ChildRecord{Key: "main", Branch: "main"})
	b := h.wait(t, h.trigger(t, "site.main").ID)
	r.commit("main", map[string]string{"later.txt": "x"})
	again := h.again(t, b.ID, "web")
	if again.BuiltCommit != first || b.BuiltCommit != first {
		t.Fatalf("built %s then %s, want %s both times", b.BuiltCommit, again.BuiltCommit, first)
	}
}

func TestBuildAgainIsRefusedWhileTheBuildHasNotEnded(t *testing.T) {
	block, open := gated(t)
	h := newHarness(t, map[string]string{"j": "name: j\nconcurrency: parallel\nstages:\n  s: {run: '" + block + "'}\n"})
	b := h.trigger(t, "j")
	h.untilRunning(t, b.ID)
	if _, err := h.d.Again(context.Background(), b.ID, "web", "ada"); !errors.Is(err, ErrNotEnded) {
		t.Fatalf("a running build built again: %v", err)
	}
	open()
	h.wait(t, b.ID)
}

func TestBuildAgainIsRefusedAsABuildIsWhenKippIsQuietOrTheJobDisabled(t *testing.T) {
	h := newHarness(t, map[string]string{"j": "name: j\nstages:\n  s: {run: 'true'}\n"})
	b := h.wait(t, h.trigger(t, "j").ID)
	if _, err := h.d.SetQuiet(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.Again(context.Background(), b.ID, "web", "ada"); !errors.Is(err, ErrQuiet) {
		t.Fatalf("quiet: %v", err)
	}
	if _, err := h.d.SetQuiet(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	snap, err := config.Load(writeJobs(t, t.TempDir(), map[string]string{"j": "name: j\ndisabled: true\nstages:\n  s: {run: 'true'}\n"}))
	if err != nil {
		t.Fatal(err)
	}
	h.d.SetSnapshot(snap)
	if _, err := h.d.Again(context.Background(), b.ID, "web", "ada"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled: %v", err)
	}
	if _, err := h.d.Again(context.Background(), 999, "web", "ada"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown build: %v", err)
	}
}

func TestBuildAgainOfAnAfterBuildHasNoUpstream(t *testing.T) {
	h := newHarness(t, map[string]string{"j": "name: j\nstages:\n  s: {run: 'set -u; echo \"[${KIPP_UPSTREAM_BUILD-}]\"'}\n"})
	first, err := h.d.Trigger(context.Background(), TriggerRequest{Job: "j", Kind: "after", Detail: "up#4", UpstreamStatus: store.Success})
	if err != nil {
		t.Fatal(err)
	}
	h.wait(t, first.ID)
	b := h.again(t, first.ID, "web")
	if got := h.log(t, b, "s"); got != "[]\n" || b.UpstreamStatus != "" {
		t.Fatalf("upstream %q in %q", b.UpstreamStatus, got)
	}
}

func TestATagBuildHasItsTagAndCommitAndNoBranchAndBuildAgainKeepsThem(t *testing.T) {
	h := newHarness(t, map[string]string{"rel": "name: rel\nconcurrency: parallel\nstages:\n" +
		"  s: {run: 'set -u; echo \"$KIPP_TRIGGER [$KIPP_TAG] [$KIPP_BRANCH] [$KIPP_COMMIT] $KIPP_DEFAULT_BRANCH\"'}\n" +
		"  prod: {needs: [s], when: {tags: [\"v*\"]}, run: echo prod}\n" +
		"  staging: {needs: [s], when: {branches: [main]}, run: echo staging}\n"})
	tagged, err := h.d.Trigger(context.Background(), TriggerRequest{Job: "rel", Kind: "webhook", Detail: "abc123", Tag: "v1.2.0", DefaultBranch: "main", By: "github"})
	if err != nil {
		t.Fatal(err)
	}
	h.wait(t, tagged.ID)
	if got, want := h.log(t, tagged, "s"), "webhook [v1.2.0] [] [abc123] main\n"; got != want {
		t.Fatalf("the stage saw %q, want %q", got, want)
	}
	b := h.again(t, tagged.ID, "web")
	if got, want := h.log(t, b, "s"), "web [v1.2.0] [] [abc123] main\n"; got != want {
		t.Fatalf("built again, the stage saw %q, want %q", got, want)
	}
	for _, id := range []int64{tagged.ID, b.ID} {
		st := h.stages(t, id)
		if st["prod"].Status != store.StageSuccess || st["staging"].Status != store.StageSkipped {
			t.Fatalf("build %d: prod %s, staging %s, want the tag's stage alone", id, st["prod"].Status, st["staging"].Status)
		}
	}
	pushed, err := h.d.Trigger(context.Background(), TriggerRequest{Job: "rel", Kind: "webhook", Detail: "def456", Branch: "main", DefaultBranch: "main", By: "github"})
	if err != nil {
		t.Fatal(err)
	}
	h.wait(t, pushed.ID)
	if got, want := h.log(t, pushed, "s"), "webhook [] [main] [def456] main\n"; got != want {
		t.Fatalf("a branch build saw %q, want %q", got, want)
	}
}
