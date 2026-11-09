package dispatch

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/runnerhub"
	"github.com/ndthuan/kipp/internal/store"
)

// the stage prints each variable on a line of its own, between brackets, so
// that an empty one shows.
const printVars = "stages:\n  s:\n    run: |\n      set -u\n      for v in $VARS; do eval \"echo \\\"$v=[\\$$v]\\\"\"; done\n"

func varsJob(name, vars, extra string) string {
	return "name: " + name + "\nenv: {VARS: '" + vars + "'}\n" + extra + printVars
}

// seen reads the lines a stage printed as name=[value] into a map.
func seen(t *testing.T, log string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(log), "\n") {
		name, val, ok := strings.Cut(line, "=[")
		if !ok || !strings.HasSuffix(val, "]") {
			t.Fatalf("a line that is not name=[value]: %q in %q", line, log)
		}
		out[name] = strings.TrimSuffix(val, "]")
	}
	return out
}

func (h *harness) triggerAs(t *testing.T, req TriggerRequest) *store.Build {
	t.Helper()
	b, err := h.d.Trigger(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return h.wait(t, b.ID)
}

func TestACIBuildSaysSoAndNamesItselfAndItsPages(t *testing.T) {
	h := newHarness(t, map[string]string{"site-smoke": varsJob("site-smoke", "CI KIPP_BUILD_ID KIPP_BUILD_URL KIPP_JOB_URL", "")},
		func(o *Options) { o.PublicURL = "https://ci.example.com/" })
	h.triggerAs(t, TriggerRequest{Job: "site-smoke", Kind: "api", By: "ann"})
	b := h.triggerAs(t, TriggerRequest{Job: "site-smoke", Kind: "api", By: "ann"})
	got := seen(t, h.log(t, b, "s"))
	want := map[string]string{
		"CI": "true", "KIPP_BUILD_ID": strconv.FormatInt(b.ID, 10),
		"KIPP_BUILD_URL": "https://ci.example.com/jobs/site-smoke/2", "KIPP_JOB_URL": "https://ci.example.com/jobs/site-smoke",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s is %q, want %q", k, got[k], v)
		}
	}
}

func TestTheBuildAndJobURLsAreEmptyWithoutABaseURLEvenWhenStagesAreGivenKippURL(t *testing.T) {
	h := newHarness(t, map[string]string{"a": varsJob("a", "KIPP_URL KIPP_BUILD_URL KIPP_JOB_URL", "")},
		func(o *Options) { o.BaseURL = "http://127.0.0.1:8085" })
	b := h.triggerAs(t, TriggerRequest{Job: "a", Kind: "api", By: "ann"})
	got := seen(t, h.log(t, b, "s"))
	if got["KIPP_URL"] != "http://127.0.0.1:8085" || got["KIPP_BUILD_URL"] != "" || got["KIPP_JOB_URL"] != "" {
		t.Fatalf("%v", got)
	}
}

func TestStartedByIsThePersonForAnAPIOrWebBuildAndNobodyForTheRest(t *testing.T) {
	h := newHarness(t, map[string]string{"a": varsJob("a", "KIPP_STARTED_BY", "")})
	for kind, want := range map[string]string{"api": "ann", "web": "ann", "cron": "", "webhook": "", "after": "", "scan": ""} {
		b := h.triggerAs(t, TriggerRequest{Job: "a", Kind: kind, By: map[bool]string{true: "ann", false: "kipp"}[want != ""]})
		if got := seen(t, h.log(t, b, "s"))["KIPP_STARTED_BY"]; got != want {
			t.Errorf("a %s build: KIPP_STARTED_BY is %q, want %q", kind, got, want)
		}
	}
	again, err := h.d.Again(context.Background(), 1, "web", "bob")
	if err != nil {
		t.Fatal(err)
	}
	b := h.wait(t, again.ID)
	if got := seen(t, h.log(t, b, "s"))["KIPP_STARTED_BY"]; got != "bob" {
		t.Errorf("a build made again by bob: KIPP_STARTED_BY is %q", got)
	}
}

func TestCommitIsTheClonedCommitForASourceJobWhateverStartedItAndIsRecorded(t *testing.T) {
	file := "stages:\n  s: {run: 'echo \"[$KIPP_COMMIT]\"'}\n"
	h := newHarness(t, map[string]string{"src": "name: src\nsource: {repo: o/r, ref: main, path: ci.yml}\n"}, func(o *Options) {
		o.Clone = func(_ context.Context, dir, _, _ string, _ io.Writer) error {
			return os.WriteFile(filepath.Join(dir, "ci.yml"), []byte(file), 0o600)
		}
		o.Head = func(context.Context, string) (string, error) { return "c0ffee", nil }
	})
	for _, kind := range []string{"cron", "api", "web"} {
		b := h.triggerAs(t, TriggerRequest{Job: "src", Kind: kind, By: "x"})
		if got := h.log(t, b, "s"); b.Status != store.Success || got != "[c0ffee]\n" {
			t.Fatalf("%s: %v, log %q", kind, b.Status, got)
		}
		stored, err := h.st.Build(context.Background(), b.ID)
		if err != nil || stored.BuiltCommit != "c0ffee" {
			t.Fatalf("%s: stored built_commit %q, err %v", kind, stored.BuiltCommit, err)
		}
	}
}

func TestCommitIsEmptyForABuildThatClonedNothingAndNamedNoCommit(t *testing.T) {
	h := newHarness(t, map[string]string{"a": varsJob("a", "KIPP_COMMIT", "")})
	b := h.triggerAs(t, TriggerRequest{Job: "a", Kind: "cron", By: "kipp"})
	if got := seen(t, h.log(t, b, "s"))["KIPP_COMMIT"]; got != "" {
		t.Fatalf("KIPP_COMMIT is %q", got)
	}
}

func TestRepoIsTheWebhooksForEveryBuildOfThatJobAndEmptyWithoutOne(t *testing.T) {
	h := newHarness(t, map[string]string{
		"hooked": varsJob("hooked", "KIPP_REPO", "triggers: {webhook: {repo: acme/site, branches: [main]}}\n"),
		"plain":  varsJob("plain", "KIPP_REPO", ""),
	})
	for _, kind := range []string{"cron", "api", "webhook"} {
		b := h.triggerAs(t, TriggerRequest{Job: "hooked", Kind: kind, By: "x", Branch: "main"})
		if got := seen(t, h.log(t, b, "s"))["KIPP_REPO"]; got != "acme/site" {
			t.Errorf("a %s build of the hooked job: KIPP_REPO is %q", kind, got)
		}
	}
	b := h.triggerAs(t, TriggerRequest{Job: "plain", Kind: "api", By: "x"})
	if got := seen(t, h.log(t, b, "s"))["KIPP_REPO"]; got != "" {
		t.Errorf("KIPP_REPO is %q for a job with no webhook", got)
	}
}

func TestRefNamesTheBranchTheTagOrThePullRequestAndIsEmptyOtherwise(t *testing.T) {
	h := newHarness(t, map[string]string{"a": varsJob("a", "KIPP_REF KIPP_BRANCH KIPP_TAG", "")})
	for name, c := range map[string]struct {
		req  TriggerRequest
		want string
	}{
		"a pushed branch": {TriggerRequest{Job: "a", Kind: "webhook", Detail: "abc", Branch: "feature/x", By: "github"}, "refs/heads/feature/x"},
		"a pushed tag":    {TriggerRequest{Job: "a", Kind: "webhook", Detail: "abc", Tag: "v1.2.0", By: "github"}, "refs/tags/v1.2.0"},
		"cron":            {TriggerRequest{Job: "a", Kind: "cron", By: "kipp"}, ""},
		"api":             {TriggerRequest{Job: "a", Kind: "api", By: "ann"}, ""},
	} {
		b := h.triggerAs(t, c.req)
		if got := seen(t, h.log(t, b, "s"))["KIPP_REF"]; got != c.want {
			t.Errorf("%s: KIPP_REF is %q, want %q", name, got, c.want)
		}
	}
}

func TestPreviousCommitsAreTheLatestEarlierBuildsWithOneAndTheLatestSuccessful(t *testing.T) {
	// the stage fails when its commit ends in -bad
	h := newHarness(t, map[string]string{"a": "name: a\nenv: {VARS: 'KIPP_PREVIOUS_COMMIT KIPP_PREVIOUS_SUCCESSFUL_COMMIT'}\nstages:\n  s:\n    run: |\n      set -u\n      for v in $VARS; do eval \"echo \\\"$v=[\\$$v]\\\"\"; done\n      case \"$KIPP_COMMIT\" in *-bad) exit 1;; esac\n"})
	push := func(sha string) *store.Build {
		return h.triggerAs(t, TriggerRequest{Job: "a", Kind: "webhook", Detail: sha, Branch: "main", By: "github"})
	}
	first := seen(t, h.log(t, push("one"), "s"))
	if first["KIPP_PREVIOUS_COMMIT"] != "" || first["KIPP_PREVIOUS_SUCCESSFUL_COMMIT"] != "" {
		t.Fatalf("the first build has no previous: %v", first)
	}
	second := seen(t, h.log(t, push("two-bad"), "s"))
	if second["KIPP_PREVIOUS_COMMIT"] != "one" || second["KIPP_PREVIOUS_SUCCESSFUL_COMMIT"] != "one" {
		t.Fatalf("second: %v", second)
	}
	third := seen(t, h.log(t, push("three"), "s"))
	if third["KIPP_PREVIOUS_COMMIT"] != "two-bad" || third["KIPP_PREVIOUS_SUCCESSFUL_COMMIT"] != "one" {
		t.Fatalf("third: %v", third)
	}
	// a build no commit belongs to is passed over
	h.triggerAs(t, TriggerRequest{Job: "a", Kind: "api", By: "ann"})
	fifth := seen(t, h.log(t, push("five"), "s"))
	if fifth["KIPP_PREVIOUS_COMMIT"] != "three" || fifth["KIPP_PREVIOUS_SUCCESSFUL_COMMIT"] != "three" {
		t.Fatalf("fifth: %v", fifth)
	}
}

func TestAttemptCountsTheTriesOfAStageThatFailsOnce(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "failed-once")
	h := newHarness(t, map[string]string{"a": "name: a\nstages:\n  s:\n    retries: 1\n    run: |\n      echo \"attempt=$KIPP_ATTEMPT\"\n      if [ ! -e " + marker + " ]; then touch " + marker + "; exit 1; fi\n  t: {needs: [s], run: 'echo \"attempt=$KIPP_ATTEMPT\"'}\n"})
	b := h.triggerAs(t, TriggerRequest{Job: "a", Kind: "api", By: "ann"})
	if got := h.log(t, b, "s"); b.Status != store.Success || !strings.Contains(got, "attempt=1\n") || !strings.Contains(got, "kipp: try 2 of 2\nattempt=2\n") {
		t.Fatalf("%v: %q", b.Status, got)
	}
	if got := h.log(t, b, "t"); got != "attempt=1\n" {
		t.Fatalf("a stage that ran once: %q", got)
	}
}

func TestATmpDirIsEmptyOutsideTheWorkspaceAndGoneAfterTheStageAndTheHook(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "dirs")
	h := newHarness(t, map[string]string{"a": "name: a\nstages:\n  s:\n    run: |\n      echo \"$KIPP_TMP\" >> " + rec + "\n      test -d \"$KIPP_TMP\" && [ -z \"$(ls -A \"$KIPP_TMP\")\" ] && echo empty\n      touch \"$KIPP_TMP/leftover\"\n  u: {run: 'echo \"$KIPP_TMP\" >> " + rec + "; echo \"$KIPP_OS $KIPP_ARCH\"'}\non_success: {run: 'echo \"$KIPP_TMP\" >> " + rec + "; test -d \"$KIPP_TMP\"'}\n"})
	b := h.triggerAs(t, TriggerRequest{Job: "a", Kind: "api", By: "ann"})
	if got := h.log(t, b, "s"); b.Status != store.Success || got != "empty\n" {
		t.Fatalf("%v: %q", b.Status, got)
	}
	if got := strings.TrimSpace(h.log(t, b, "u")); got != runtime.GOOS+" "+runtime.GOARCH {
		t.Fatalf("KIPP_OS KIPP_ARCH: %q", got)
	}
	data, err := os.ReadFile(filepath.Clean(rec))
	if err != nil {
		t.Fatal(err)
	}
	dirs := strings.Fields(string(data))
	if len(dirs) != 3 || dirs[0] == dirs[1] || dirs[1] == dirs[2] {
		t.Fatalf("a directory of its own for each stage and the hook: %v", dirs)
	}
	for _, dir := range dirs {
		if strings.HasPrefix(dir, filepath.Join(h.dir, "workspace")) {
			t.Errorf("%s is inside a workspace", dir)
		}
	}
	assertNoTmpDirsLeft(t, h)
}

// assertNoTmpDirsLeft fails when a KIPP_TMP directory of the server's own
// outlived the build.
func assertNoTmpDirsLeft(t *testing.T, h *harness) {
	t.Helper()
	left, err := os.ReadDir(h.d.stageTmpDir())
	if err != nil || len(left) != 0 {
		t.Fatalf("directories are left after the build: %v (%v)", left, err)
	}
}

func TestATmpDirIsGoneWhenTheStageFails(t *testing.T) {
	h := newHarness(t, map[string]string{"a": "name: a\nstages:\n  s: {run: 'test -d \"$KIPP_TMP\"; exit 3'}\n"})
	b := h.triggerAs(t, TriggerRequest{Job: "a", Kind: "api", By: "ann"})
	if b.Status != store.Failed {
		t.Fatalf("%v", b.Status)
	}
	assertNoTmpDirsLeft(t, h)
}

func TestRecoverSweepsTheTmpDirsAStageLeftWhenTheServerDied(t *testing.T) {
	h := newHarness(t, map[string]string{"a": "name: a\nstages:\n  s: {run: echo hi}\n"})
	left := filepath.Join(h.d.stageTmpDir(), "t-left")
	if err := os.MkdirAll(left, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := h.d.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(left); !os.IsNotExist(err) {
		t.Fatalf("a leftover tmp dir survived Recover: %v", err)
	}
}

const childVarsStages = "stages:\n  s:\n    run: |\n      set -u\n      for v in KIPP_REPO KIPP_REF KIPP_COMMIT KIPP_BUILD_ID; do eval \"echo \\\"$v=[\\$$v]\\\"\"; done\n"

func TestAChildBuildNamesItsRepoItsRefAndItsCommit(t *testing.T) {
	r, _, head := prRepo(t, childVarsStages)
	h := newHarness(t, map[string]string{"site": parentFile("file://"+r.dir, "")})
	h.withChildren(t, config.ChildRecord{Key: "feature-x", Branch: "feature-x"}, prChild(config.KindPR, "pr-7"))
	branch := seen(t, h.log(t, h.hook(t, "site.feature-x", head), "s"))
	if branch["KIPP_REPO"] != "acme/site" || branch["KIPP_REF"] != "refs/heads/feature-x" || branch["KIPP_COMMIT"] != head {
		t.Fatalf("a branch child: %v", branch)
	}
	pr := seen(t, h.log(t, h.hook(t, "site.pr-7", head), "s"))
	if pr["KIPP_REPO"] != "acme/site" || pr["KIPP_REF"] != "refs/pull/7/head" || pr["KIPP_COMMIT"] != head {
		t.Fatalf("a pull request child: %v", pr)
	}
}

const prVarsStages = "stages:\n  s:\n    run: |\n      set -u\n      echo \"T=[$KIPP_PR_TITLE] U=[$KIPP_PR_URL] A=[$KIPP_PR_AUTHOR]\"\n"

func TestAPullRequestsBuildKnowsItsTitleItsPageAndItsAuthorAndABranchsAreEmpty(t *testing.T) {
	r, _, head := prRepo(t, prVarsStages)
	h := newHarness(t, map[string]string{"site": parentFile("file://"+r.dir, "")})
	h.withChildren(t, config.ChildRecord{Key: "feature-x", Branch: "feature-x"}, prChild(config.KindPR, "pr-7"))
	if got := h.log(t, h.hook(t, "site.pr-7", head), "s"); !strings.Contains(got, "T=[Add login] U=[https://github.example/acme/site/pull/7] A=[ann]") {
		t.Fatalf("a pull request child: %q", got)
	}
	if got := h.log(t, h.hook(t, "site.feature-x", head), "s"); !strings.Contains(got, "T=[] U=[] A=[]") {
		t.Fatalf("a branch child: %q", got)
	}
}

func TestAPullRequestsBuildOnAnRunnerKnowsItsTitleItsPageAndItsAuthor(t *testing.T) {
	ag := newRunners(t, runnerhub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
	ag.runBox(t.TempDir())
	r, _, head := prRepo(t, strings.Replace(prVarsStages, "  s:\n", "  s:\n    runs_on: linux\n", 1))
	h := newHarness(t, map[string]string{"site": parentFile("file://"+r.dir, "")}, ag.opts)
	h.withChildren(t, prChild(config.KindPR, "pr-7"))
	b := h.hook(t, "site.pr-7", head)
	if got := h.log(t, b, "s"); b.Status != store.Success || !strings.Contains(got, "T=[Add login] U=[https://github.example/acme/site/pull/7] A=[ann]") {
		t.Fatalf("%v: %q", b.Status, got)
	}
}

func TestAStageOnAnRunnerGetsTheSameVariablesAndTheRunnersOwnTmpOsAndArch(t *testing.T) {
	ag := newRunners(t, runnerhub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
	dir := t.TempDir()
	ag.runBox(dir)
	marker := filepath.Join(t.TempDir(), "failed-once")
	rec := filepath.Join(t.TempDir(), "dir")
	job := "name: a\nstages:\n  s:\n    runs_on: linux\n    retries: 1\n    run: |\n      set -u\n" +
		"      echo \"CI=[$CI] BUILD_ID=[$KIPP_BUILD_ID] URL=[$KIPP_BUILD_URL] JOB_URL=[$KIPP_JOB_URL] BY=[$KIPP_STARTED_BY] REF=[$KIPP_REF] PREV=[$KIPP_PREVIOUS_COMMIT] OK=[$KIPP_PREVIOUS_SUCCESSFUL_COMMIT] OS=[$KIPP_OS] ARCH=[$KIPP_ARCH] ATTEMPT=[$KIPP_ATTEMPT]\"\n" +
		"      echo \"$KIPP_TMP\" > " + rec + "; test -d \"$KIPP_TMP\" && [ -z \"$(ls -A \"$KIPP_TMP\")\" ] && echo tmp-empty; touch \"$KIPP_TMP/x\"\n" +
		"      if [ ! -e " + marker + " ]; then touch " + marker + "; exit 1; fi\n" +
		"  gone: {needs: [s], run: 'test ! -e \"$(cat " + rec + ")\" && echo gone'}\n"
	h := newHarness(t, map[string]string{"a": job}, ag.opts, func(o *Options) { o.PublicURL = "https://ci.example.com" })
	h.triggerAs(t, TriggerRequest{Job: "a", Kind: "webhook", Detail: "c1", Branch: "main", By: "github"})
	// the first build above failed once on the runner too, so the marker is there
	b := h.triggerAs(t, TriggerRequest{Job: "a", Kind: "webhook", Detail: "c2", Branch: "main", By: "github"})
	log := h.log(t, b, "s")
	want := "CI=[true] BUILD_ID=[" + strconv.FormatInt(b.ID, 10) + "] URL=[https://ci.example.com/jobs/a/2] JOB_URL=[https://ci.example.com/jobs/a] BY=[] REF=[refs/heads/main] PREV=[c1] OK=[c1] OS=[" + runtime.GOOS + "] ARCH=[" + runtime.GOARCH + "] ATTEMPT=[1]"
	if b.Status != store.Success || !strings.Contains(log, want) || !strings.Contains(log, "tmp-empty") {
		t.Fatalf("%v: log %q, want it to hold %q", b.Status, log, want)
	}
	if got := h.log(t, b, "gone"); got != "gone\n" {
		t.Fatalf("the stage's KIPP_TMP on the runner outlived it: %q", got)
	}
}

func TestAnRunnersStageIsTriedAgainAndKnowsWhichTryItIs(t *testing.T) {
	ag := newRunners(t, runnerhub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
	ag.runBox(t.TempDir())
	marker := filepath.Join(t.TempDir(), "failed-once")
	h := newHarness(t, map[string]string{"a": "name: a\nstages:\n  s:\n    runs_on: linux\n    retries: 1\n    run: |\n      echo \"attempt=$KIPP_ATTEMPT\"\n      if [ ! -e " + marker + " ]; then touch " + marker + "; exit 1; fi\n"}, ag.opts)
	b := h.triggerAs(t, TriggerRequest{Job: "a", Kind: "api", By: "ann"})
	if got := h.log(t, b, "s"); b.Status != store.Success || !strings.Contains(got, "attempt=1\n") || !strings.Contains(got, "attempt=2\n") {
		t.Fatalf("%v: %q", b.Status, got)
	}
}
