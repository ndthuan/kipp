package dispatch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

// gateStages is a repo's .kipp.yml whose run stage is the gate a deploy
// may reuse.
const gateStages = "stages:\n  run: {run: 'echo gated'}\n"

// deployJob is a plain job that checks main of the repo at dir out into
// src, gates it with reuse and ships; extra goes at the top of the file
// and after runs at the end of the checkout.
func deployJob(dir, extra, after string) string {
	return "name: deploy\nparams:\n  FULL_GATE: {type: bool}\n" + extra +
		"stages:\n" +
		"  checkout: {run: 'rm -rf src && git clone -q file://" + dir + " src && git -C src checkout -q main" + after + "'}\n" +
		"  gate:\n    needs: [checkout]\n    run: 'echo ran the gate'\n" +
		"    reuse: {from: [site], stage: run, tree: $KIPP_WORKSPACE/src, force: FULL_GATE}\n" +
		"  ship: {needs: [gate], run: 'echo shipped'}\n"
}

const tools = "fingerprint: ['echo tools-1']\n"

// squash makes a commit on main with the merge of feature-x into it, as a
// squash merge does: another commit than kipp's merge, the same tree.
func squash(r *repo) string {
	r.git("checkout", "-q", "main")
	r.git("merge", "-q", "--squash", "feature-x")
	r.git("commit", "-q", "-m", "squash")
	return r.git("rev-parse", "HEAD")
}

func (h *harness) treeOf(t *testing.T, id int64) (tree, fingerprintSum string) {
	t.Helper()
	tree, sum, err := h.st.BuildTree(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return tree, sum
}

func (h *harness) withParams(t *testing.T, job string, params map[string]string) *store.Build {
	t.Helper()
	b, err := h.d.Trigger(context.Background(), TriggerRequest{Job: job, Kind: "cli", By: "test", Params: params})
	if err != nil {
		t.Fatal(err)
	}
	return h.wait(t, b.ID)
}

// greenPR is a dispatcher with site, whose pull request #7 has built green,
// and the jobs given; it returns the repo and the pull request's tree.
func greenPR(t *testing.T, siteExtra string, jobs func(dir string) map[string]string) (*harness, *repo, string) {
	t.Helper()
	r, _, head := prRepo(t, gateStages)
	files := jobs(r.dir)
	files["site"] = parentFile("file://"+r.dir, siteExtra)
	h := newHarness(t, files)
	h.withChildren(t, prChild(config.KindPR, "pr-7"))
	pr := h.hook(t, "site.pr-7", head)
	if pr.Status != store.Success {
		t.Fatalf("the pull request's build %s", pr.Status)
	}
	tree, _ := h.treeOf(t, pr.ID)
	return h, r, tree
}

func TestADeployReusesTheGreenGateOfThePullRequestWhoseSquashItChecksOut(t *testing.T) {
	// The commands differ and print the same: only the output counts.
	h, r, prTree := greenPR(t, tools, func(dir string) map[string]string {
		return map[string]string{"deploy": deployJob(dir, "fingerprint: ['printf \"tools-1\\n\"']\n", "")}
	})
	sq := squash(r)
	if r.git("rev-parse", sq+"^{tree}") != prTree {
		t.Fatalf("the squash %s is not of the pull request's tree %s", sq, prTree)
	}
	d := h.wait(t, h.trigger(t, "deploy").ID)
	want := fmt.Sprintf("gate skipped: tree %s green in site.pr-7 #1\n", prTree[:12])
	if got := h.log(t, d, "gate"); d.Status != store.Success || got != want {
		t.Fatalf("%s: the gate's log %q, want %q", d.Status, got, want)
	}
	st := h.stages(t, d.ID)
	g := st["gate"]
	if g.Status != store.StageSuccess || g.ExitCode != nil || g.Reused == nil || *g.Reused != (store.StageRef{Job: "site.pr-7", Number: 1, Stage: "run"}) {
		t.Fatalf("the gate reads %+v", g)
	}
	if st["ship"].Status != store.StageSuccess || h.log(t, d, "ship") != "shipped\n" {
		t.Fatalf("the stage that needs the reused gate: %s", st["ship"].Status)
	}
}

func TestTheTreeOfEveryCheckoutKippMakesIsRecorded(t *testing.T) {
	r, _, head := prRepo(t, gateStages)
	r.commit("ci", map[string]string{"ci.yml": gateStages})
	r.git("checkout", "-q", "main")
	h := newHarness(t, map[string]string{
		"site":  parentFile("file://"+r.dir, ""),
		"build": "name: build\nsource: {repo: 'file://" + r.dir + "', ref: ci, path: ci.yml}\n",
	})
	h.withChildren(t, config.ChildRecord{Key: "main", Branch: "main"}, prChild(config.KindPR, "pr-7"))
	branch := h.wait(t, h.trigger(t, "site.main").ID)
	pr := h.hook(t, "site.pr-7", head)
	src := h.wait(t, h.trigger(t, "build").ID)
	merged := r.git("merge-tree", "--write-tree", "main", "feature-x")
	for name, c := range map[string]struct {
		b    *store.Build
		want string
	}{
		"a branch":                {branch, r.git("rev-parse", "main^{tree}")},
		"a pull request's merge":  {pr, merged},
		"the ref of a source job": {src, r.git("rev-parse", "ci^{tree}")},
	} {
		if tree, _ := h.treeOf(t, c.b.ID); c.b.Status != store.Success || tree != c.want {
			t.Errorf("%s: %s, tree %q, want %q", name, c.b.Status, tree, c.want)
		}
	}
}

func TestTheFingerprintIsTheOutputOfItsCommandsInTheBuildsEnvironment(t *testing.T) {
	h := newHarness(t, map[string]string{"a": "name: a\nenv: {TOOL: tool-2}\nsecrets: [KEY]\n" +
		"fingerprint: ['echo tool-1', 'echo $TOOL; echo noise >&2', 'echo $KEY']\nstages:\n  s: {run: 'true'}\n"})
	b := h.wait(t, h.trigger(t, "a").ID)
	sum := sha256.Sum256([]byte("tool-1\ntool-2\n***"))
	if _, got := h.treeOf(t, b.ID); b.Status != store.Success || got != hex.EncodeToString(sum[:]) {
		t.Fatalf("%s: fingerprint sum %q", b.Status, got)
	}
}

func TestAFingerprintCommandThatFailsFailsThePrepare(t *testing.T) {
	h := newHarness(t, map[string]string{"a": "name: a\nfingerprint: ['echo ok', 'exit 3']\nstages:\n  s: {run: 'echo ran'}\n"})
	b := h.wait(t, h.trigger(t, "a").ID)
	if got := h.log(t, b, config.PrepareLog); b.Status != store.Failed || got != "fingerprint \"exit 3\": exit status 3\n" {
		t.Fatalf("%s: prepare log %q", b.Status, got)
	}
	if st := h.stages(t, b.ID); len(st) != 0 {
		t.Fatalf("stages ran: %+v", st)
	}
	if _, sum := h.treeOf(t, b.ID); sum != "" {
		t.Fatalf("a fingerprint that failed was recorded: %q", sum)
	}
}

func TestAWorkTreeWithChangesOrNoRepoRunsTheStage(t *testing.T) {
	h, _, _ := greenPR(t, tools, func(dir string) map[string]string {
		return map[string]string{
			"deploy": deployJob(dir, tools, " && echo x > src/stray"),
			"bare":   strings.Replace(deployJob(dir, tools, " && rm -rf src/.git"), "name: deploy", "name: bare", 1),
		}
	})
	for job, why := range map[string]string{"deploy": "the work tree at %s has changes", "bare": "no git work tree at %s"} {
		b := h.wait(t, h.trigger(t, job).ID)
		src := filepath.Join(h.dir, "workspace", job, "1", "src")
		want := "gate: " + fmt.Sprintf(why, src) + "; running it\nran the gate\n"
		if got := h.log(t, b, "gate"); b.Status != store.Success || got != want {
			t.Errorf("%s: %s, %q, want %q", job, b.Status, got, want)
		}
		if g := h.stages(t, b.ID)["gate"]; g.Reused != nil || g.ExitCode == nil {
			t.Errorf("%s: the gate reads %+v", job, g)
		}
	}
}

func TestForceRunsTheStageInFull(t *testing.T) {
	h, r, _ := greenPR(t, tools, func(dir string) map[string]string {
		return map[string]string{"deploy": deployJob(dir, tools, "")}
	})
	squash(r)
	b := h.withParams(t, "deploy", map[string]string{"FULL_GATE": "true"})
	if got := h.log(t, b, "gate"); b.Status != store.Success || got != "gate: full run forced by FULL_GATE\nran the gate\n" {
		t.Fatalf("%s: %q", b.Status, got)
	}
}

func TestAFullRunThatEndsSuccessIsMatchedByTheNextBuildOfTheJob(t *testing.T) {
	h, r, prTree := greenPR(t, tools, func(dir string) map[string]string {
		return map[string]string{"deploy": deployJob(dir, "fingerprint: ['echo tools-2']\n", "")}
	})
	squash(r)
	first := h.wait(t, h.trigger(t, "deploy").ID)
	if got, want := h.log(t, first, "gate"), "gate: no green run of tree "+prTree[:12]+"; running it\nran the gate\n"; got != want {
		t.Fatalf("another toolset: %q, want %q", got, want)
	}
	second := h.wait(t, h.trigger(t, "deploy").ID)
	if got, want := h.log(t, second, "gate"), "gate skipped: tree "+prTree[:12]+" green in deploy #1\n"; second.Status != store.Success || got != want {
		t.Fatalf("%s: %q, want %q", second.Status, got, want)
	}
}

func TestAJobWithoutAFingerprintNeverMatchesNorIsMatched(t *testing.T) {
	h, r, prTree := greenPR(t, "", func(dir string) map[string]string {
		return map[string]string{
			"deploy": deployJob(dir, tools, ""),
			"bare":   strings.Replace(deployJob(dir, "", ""), "name: deploy", "name: bare", 1),
		}
	})
	squash(r)
	b := h.wait(t, h.trigger(t, "deploy").ID)
	if got, want := h.log(t, b, "gate"), "gate: no green run of tree "+prTree[:12]+"; running it\nran the gate\n"; got != want {
		t.Fatalf("a pull request built with no fingerprint matched: %q", got)
	}
	for range 2 {
		b = h.wait(t, h.trigger(t, "bare").ID)
		if got := h.log(t, b, "gate"); got != "gate: the job takes no fingerprint, so no green run can match; running it\nran the gate\n" {
			t.Fatalf("a job with no fingerprint: %q", got)
		}
	}
}

func TestAStageOfARepoWithoutATreeGatesTheCheckoutKippMade(t *testing.T) {
	r, _, head := prRepo(t, "stages:\n  run: {run: 'echo gated', reuse: {from: [site], stage: run}}\n")
	h := newHarness(t, map[string]string{"site": parentFile("file://"+r.dir, tools)})
	h.withChildren(t, prChild(config.KindPR, "pr-7"))
	pr := h.hook(t, "site.pr-7", head)
	tree, _ := h.treeOf(t, pr.ID)
	if got := h.log(t, pr, "run"); pr.Status != store.Success || got != "run: no green run of tree "+tree[:12]+"; running it\ngated\n" {
		t.Fatalf("%s: %q", pr.Status, got)
	}
	squash(r)
	h.withChildren(t, prChild(config.KindPR, "pr-7"), config.ChildRecord{Key: "main", Branch: "main"})
	b := h.wait(t, h.trigger(t, "site.main").ID)
	if got := h.log(t, b, "run"); b.Status != store.Success || got != "run skipped: tree "+tree[:12]+" green in site.pr-7 #1\n" {
		t.Fatalf("%s: %q", b.Status, got)
	}
}
