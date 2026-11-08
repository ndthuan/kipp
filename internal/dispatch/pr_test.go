package dispatch

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

// prRepo is a repo whose main has the stages and whose feature-x, the
// head of pull request #7, adds a file of its own.
func prRepo(t *testing.T, stages string) (r *repo, base, head string) {
	t.Helper()
	r = newRepo(t)
	r.git("config", "uploadpack.allowFilter", "true")
	r.commit("main", map[string]string{".kipp.yml": stages, "shared.txt": "one\n"})
	head = r.commit("feature-x", map[string]string{"head.txt": "from the head\n"})
	r.git("checkout", "-q", "main")
	base = r.commit("main", map[string]string{"base.txt": "from the base\n"})
	return r, base, head
}

// prChild is pull request #7 of site, of kind, into main.
func prChild(kind, key string) config.ChildRecord {
	return config.ChildRecord{Key: key, Kind: kind, Branch: "feature-x", PR: 7, Title: "Add login", Author: "ann", URL: "https://github.example/acme/site/pull/7", BaseBranch: "main"}
}

// hook is a build of job a pull request event queued at head.
func (h *harness) hook(t *testing.T, job, head string) *store.Build {
	t.Helper()
	b, err := h.d.Trigger(context.Background(), TriggerRequest{Job: job, Kind: "webhook", Detail: head, Branch: "feature-x", DefaultBranch: "main", By: "github"})
	if err != nil {
		t.Fatal(err)
	}
	return h.wait(t, b.ID)
}

const treeStages = "stages:\n  s: {run: 'ls *.txt | tr \"\\n\" \" \"; git rev-list --parents -n 1 HEAD'}\n" +
	"  who: {run: 'git log -1 --format=\"%an <%ae>\"'}\n"

func TestOpenedPRBuildsItsMergeIntoTheBaseTip(t *testing.T) {
	r, base, head := prRepo(t, treeStages)
	h := newHarness(t, map[string]string{"site": parentFile("file://"+r.dir, "")})
	h.withChildren(t, prChild(config.KindPR, "pr-7"))
	b := h.hook(t, "site.pr-7", head)
	got := h.log(t, b, "s")
	if b.Status != store.Success || !strings.HasPrefix(got, "base.txt head.txt shared.txt ") {
		t.Fatalf("%s: the stage saw %q, want the base's and the head's files", b.Status, got)
	}
	parents := strings.Fields(got)[3:]
	if len(parents) != 3 || parents[1] != base || parents[2] != head {
		t.Fatalf("the build ran on %v, want a merge of %s into %s", parents, head, base)
	}
	if b.BuiltCommit != head || b.BaseCommit != base || b.PR != 7 || b.BaseBranch != "main" || b.TriggerBranch != "feature-x" {
		t.Fatalf("the build records %+v", b)
	}
	if who := h.log(t, b, "who"); who != "kipp <kipp@localhost>\n" {
		t.Fatalf("the merge was made by %q", who)
	}
}

func TestHeadModeBuildsTheHeadAlone(t *testing.T) {
	r, _, head := prRepo(t, treeStages)
	h := newHarness(t, map[string]string{"site": parentFile("file://"+r.dir, "")})
	h.withChildren(t, prChild(config.KindPRHead, "pr-7-head"))
	b := h.hook(t, "site.pr-7-head", head)
	got := h.log(t, b, "s")
	if b.Status != store.Success || got != "head.txt shared.txt "+head+"\n" {
		t.Fatalf("%s: %q, want the head's tree and commit alone", b.Status, got)
	}
	if b.BuiltCommit != head || b.BaseCommit != "" {
		t.Fatalf("a head build records base %q", b.BaseCommit)
	}
}

func TestMergeUsesTheBaseTipFetchedAtBuildTimeNotTheEventsBaseSha(t *testing.T) {
	r, base, head := prRepo(t, treeStages)
	later := r.commit("main", map[string]string{"later.txt": "pushed after the event\n"})
	h := newHarness(t, map[string]string{"site": parentFile("file://"+r.dir, "")})
	h.withChildren(t, prChild(config.KindPR, "pr-7"))
	b := h.hook(t, "site.pr-7", head)
	if b.BaseCommit != later || b.BaseCommit == base || !strings.Contains(h.log(t, b, "s"), "later.txt") {
		t.Fatalf("merged into %s, want the tip %s: %q", b.BaseCommit, later, h.log(t, b, "s"))
	}
}

func TestAConflictFailsTheBuildBeforeAnyStage(t *testing.T) {
	r, _, _ := prRepo(t, treeStages)
	r.git("checkout", "-q", "feature-x")
	head := r.commit("feature-x", map[string]string{"shared.txt": "the head's\n"})
	base := r.commit("main", map[string]string{"shared.txt": "the base's\n"})
	h := newHarness(t, map[string]string{"site": parentFile("file://"+r.dir, "")})
	h.withChildren(t, prChild(config.KindPR, "pr-7"))
	b := h.hook(t, "site.pr-7", head)
	if b.Status != store.Failed {
		t.Fatalf("%s", b.Status)
	}
	if got, want := h.log(t, b, config.PrepareLog), "kipp: feature-x does not merge cleanly into main@"+base[:7]+"\n"; got != want {
		t.Fatalf("prepare log %q, want %q", got, want)
	}
	if s := h.stages(t, b.ID)["s"]; s != nil && (s.Status == store.StageSuccess || s.Status == store.StageFailed) {
		t.Fatalf("a stage ran: %+v", s)
	}
	if b.BuiltCommit != head || b.BaseCommit != base {
		t.Fatalf("a conflict records %s into %s", b.BuiltCommit, b.BaseCommit)
	}
}

func TestPRBuildEnvNamesPRBaseAndBaseCommit(t *testing.T) {
	stages := "stages:\n  s: {run: 'set -u; echo \"$KIPP_TRIGGER $KIPP_BRANCH $KIPP_DEFAULT_BRANCH [$KIPP_COMMIT] [$KIPP_PR] [$KIPP_BASE_BRANCH] [$KIPP_BASE_COMMIT]\"'}\n"
	r, base, head := prRepo(t, stages)
	h := newHarness(t, map[string]string{"site": parentFile("file://"+r.dir, "")})
	h.withChildren(t, prChild(config.KindPR, "pr-7"), config.ChildRecord{Key: "main", Branch: "main"})
	b := h.hook(t, "site.pr-7", head)
	if got, want := h.log(t, b, "s"), "webhook feature-x main ["+head+"] [7] [main] ["+base+"]\n"; got != want {
		t.Fatalf("%s: %q, want %q", b.Status, got, want)
	}
	b = h.wait(t, h.trigger(t, "site.main").ID)
	if got, want := h.log(t, b, "s"), "cli main main ["+base+"] [] [] []\n"; got != want {
		t.Fatalf("a branch build: %q, want %q", got, want)
	}
}

func TestMergeRefusesAnArgumentThatIsAnOption(t *testing.T) {
	for _, args := range [][3]string{{"-u", "main", "x"}, {"/r", "--upload-pack=x", "x"}, {"/r", "main", "-x"}} {
		if _, _, err := gitMerge(context.Background(), t.TempDir(), args[0], args[1], "", args[2], io.Discard); err == nil || !strings.Contains(err.Error(), "dash") {
			t.Fatalf("%v: %v", args, err)
		}
	}
}

// kipp writes one commit itself, the merge of a pull request's head into its
// base: its subject fits a one-line log however long the branch's name, and
// it names kipp, not whoever runs the server, as its author and committer.
func TestMergeCommitKippWritesHasAShortSubjectAndKippAsItsAuthor(t *testing.T) {
	r := newRepo(t)
	r.git("config", "uploadpack.allowFilter", "true")
	r.commit("main", map[string]string{"a": "1"})
	r.commit("a-very-long-branch-name-"+strings.Repeat("x", 120), map[string]string{"b": "2"})
	r.commit("main", map[string]string{"c": "3"})
	dir := t.TempDir()
	if _, _, err := gitMerge(context.Background(), dir, "file://"+r.dir, "main", "", "a-very-long-branch-name-"+strings.Repeat("x", 120), io.Discard); err != nil {
		t.Fatal(err)
	}
	log := func(format string) string {
		out, err := exec.Command("git", "-C", dir, "log", "-1", "--format="+format).Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimRight(string(out), "\n")
	}
	if subject := log("%s"); len(subject) == 0 || len(subject) > 80 {
		t.Fatalf("the merge's subject is %d characters: %q", len(subject), subject)
	}
	for _, who := range []string{"a", "c"} {
		if got := log("%" + who + "n <%" + who + "e>"); got != "kipp <kipp@localhost>" {
			t.Fatalf("the merge's %s is %q", who, got)
		}
	}
}

// gitDyingOnItsFirstMerge puts a git ahead of the real one on PATH that
// answers its first merge with said and exit 128, and counts the merges it
// is asked for in the file it returns.
func gitDyingOnItsFirstMerge(t *testing.T, said string) (merges string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git on this machine")
	}
	bin := t.TempDir()
	merges = filepath.Join(bin, "merges")
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do if [ \"$a\" = merge ]; then\n" +
		"  echo m >> " + merges + "\n" +
		"  if [ \"$(wc -l < " + merges + ")\" -eq 1 ]; then echo '" + said + "' >&2; exit 128; fi\n" +
		"fi; done\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil { //nolint:gosec // a script for the test to run
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return merges
}

func mergesAsked(file string) int {
	data, _ := os.ReadFile(file) //nolint:gosec // the test's own file
	return strings.Count(string(data), "m")
}

func conflictingPR(t *testing.T) (r *repo, head string) {
	t.Helper()
	r, _, _ = prRepo(t, treeStages)
	r.git("checkout", "-q", "feature-x")
	head = r.commit("feature-x", map[string]string{"shared.txt": "the head's\n"})
	r.commit("main", map[string]string{"shared.txt": "the base's\n"})
	return r, head
}

// Git has been seen to die, in a blob-less clone, asking the remote for the
// conflict it had just written: a merge that died so is made once more.
func TestMergeTriesAgainWhenGitDiesFetchingAnObjectItMade(t *testing.T) {
	r, head := conflictingPR(t)
	merges := gitDyingOnItsFirstMerge(t, "fatal: could not fetch abc from promisor remote")
	var out strings.Builder
	_, _, err := gitMerge(context.Background(), t.TempDir(), "file://"+r.dir, "main", "", head, &out)
	if !errors.Is(err, ErrNoCleanMerge) {
		t.Fatalf("%v, want the conflict the second merge found: %s", err, out.String())
	}
	if n := mergesAsked(merges); n != 2 || !strings.Contains(out.String(), "merging again") {
		t.Fatalf("%d merges, log %q", n, out.String())
	}
}

func TestMergeDoesNotTryAgainForAnyOtherFailure(t *testing.T) {
	r, head := conflictingPR(t)
	merges := gitDyingOnItsFirstMerge(t, "fatal: something else")
	_, _, err := gitMerge(context.Background(), t.TempDir(), "file://"+r.dir, "main", "", head, io.Discard)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 128 {
		t.Fatalf("%v, want git's own exit 128", err)
	}
	if n := mergesAsked(merges); n != 1 {
		t.Fatalf("%d merges, want one", n)
	}
}
