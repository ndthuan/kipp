package dispatch

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

// repo is a git repository in a temporary directory, for a clone to fetch
// from by its path.
type repo struct {
	t   *testing.T
	dir string
	has map[string]bool // the branches made
}

func newRepo(t *testing.T) *repo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on this machine")
	}
	r := &repo{t: t, dir: t.TempDir(), has: map[string]bool{"main": true}}
	r.git("init", "-q", "-b", "main")
	return r
}

func (r *repo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes the files on the branch, which it makes from what is
// checked out when it is new, and returns the commit.
func (r *repo) commit(branch string, files map[string]string) string {
	r.t.Helper()
	if !r.has[branch] {
		r.git("checkout", "-q", "-b", branch)
		r.has[branch] = true
	} else if r.git("symbolic-ref", "--short", "HEAD") != branch {
		r.git("checkout", "-q", branch)
	}
	for name, body := range files {
		p := filepath.Join(r.dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			r.t.Fatal(err)
		}
		r.git("add", name)
	}
	r.git("commit", "-q", "--allow-empty", "-m", "on "+branch)
	return r.git("rev-parse", "HEAD")
}

// parentFile is a multibranch job of the repo at dir.
func parentFile(dir, extra string) string {
	return "name: site\nmultibranch: {repo: acme/site, clone: '" + dir + "'}\nconcurrency: parallel\n" + extra
}

// withChildren gives the dispatcher the job set with these children of
// site added, the repo's default branch being main.
func (h *harness) withChildren(t *testing.T, recs ...config.ChildRecord) {
	t.Helper()
	snap := h.d.Snapshot()
	parent := snap.Jobs["site"]
	var kids []*config.Job
	for _, rec := range recs {
		rec.Parent = "site"
		if rec.State == "" {
			rec.State = config.ChildLive
		}
		if rec.Kind == "" {
			rec.Kind = config.KindBranch
		}
		c := parent.Child(rec)
		c.DefaultBranch = "main"
		kids = append(kids, c)
	}
	h.d.SetSnapshot(snap.WithJobs(kids...))
}

const echoStages = "stages:\n  s: {run: 'echo \"$KIPP_JOB $KIPP_TRIGGER $KIPP_BRANCH $KIPP_DEFAULT_BRANCH [$KIPP_COMMIT] [$KIPP_PR]\"'}\n"

func TestParentCronBuildsTheDefaultBranchChild(t *testing.T) {
	r := newRepo(t)
	head := r.commit("main", map[string]string{".kipp.yml": echoStages})
	h := newHarness(t, map[string]string{"site": parentFile(r.dir, "triggers: {cron: '*/5 * * * *'}\n")})
	h.withChildren(t, config.ChildRecord{Key: "main", Branch: "main"}, config.ChildRecord{Key: "feature-x", Branch: "feature-x"})
	h.d.Tick(context.Background(), time.Date(2026, 10, 29, 10, 5, 0, 0, time.UTC))
	var b *store.Build
	eventually(t, "the cron queued a build", func() bool {
		bs, err := h.st.Builds(context.Background(), "site.main", 1)
		if err == nil && len(bs) == 1 {
			b = bs[0]
		}
		return b != nil
	})
	b = h.wait(t, b.ID)
	if got := h.log(t, b, "s"); b.Status != store.Success || got != "site.main cron main main ["+head+"] []\n" {
		t.Fatalf("%s: %q", b.Status, got)
	}
	if b.Parent != "site" || b.BuiltCommit != head || b.TriggerBranch != "main" || b.TriggerDefaultBranch != "main" {
		t.Fatalf("the build records %+v", b)
	}
	for _, other := range []string{"site", "site.feature-x"} {
		if bs, _ := h.st.Builds(context.Background(), other, 1); len(bs) != 0 {
			t.Fatalf("the cron built %s", other)
		}
	}
	// With no child of its default branch yet, the parent builds nothing.
	h2 := newHarness(t, map[string]string{"site": parentFile(r.dir, "")})
	if _, err := h2.d.Trigger(context.Background(), TriggerRequest{Job: "site", Kind: "api", By: "test"}); !errors.Is(err, ErrNoDefaultChild) {
		t.Fatalf("a parent with no default branch's child built: %v", err)
	}
}

func TestAfterAParentFiresOnTheDefaultBranchChild(t *testing.T) {
	r := newRepo(t)
	r.commit("main", map[string]string{".kipp.yml": echoStages})
	r.commit("feature-x", nil)
	h := newHarness(t, map[string]string{
		"site":   parentFile(r.dir, ""),
		"deploy": "name: deploy\ntriggers: {after: [site]}\nstages:\n  s: {run: 'echo after $KIPP_UPSTREAM_BUILD'}\n",
	})
	h.withChildren(t, config.ChildRecord{Key: "main", Branch: "main"}, config.ChildRecord{Key: "feature-x", Branch: "feature-x"})
	h.wait(t, h.trigger(t, "site.feature-x").ID)
	b := h.wait(t, h.trigger(t, "site.main").ID)
	if b.Status != store.Success {
		t.Fatalf("%s", b.Status)
	}
	bs, err := h.st.Builds(context.Background(), "deploy", 5)
	if err != nil || len(bs) != 1 {
		t.Fatalf("deploy ran %d times after a child of site, want once, after the default branch's: %v", len(bs), err)
	}
	if got := h.log(t, h.wait(t, bs[0].ID), "s"); got != "after site.main#1\n" {
		t.Fatalf("%q", got)
	}
}

func TestAChildBuildWithoutTheFileFailsAndSaysWhich(t *testing.T) {
	r := newRepo(t)
	head := r.commit("main", map[string]string{"README": "no stages here"})
	h := newHarness(t, map[string]string{"site": parentFile(r.dir, "on_failure: {run: 'echo failed in $(pwd)'}\n")})
	h.withChildren(t, config.ChildRecord{Key: "main", Branch: "main"})
	b := h.wait(t, h.trigger(t, "site").ID)
	if b.Status != store.Failed || b.Job != "site.main" {
		t.Fatalf("%s %s", b.Job, b.Status)
	}
	if got, want := h.log(t, b, config.PrepareLog), "kipp: no .kipp.yml at "+head[:7]+"\n"; got != want {
		t.Fatalf("prepare log %q, want %q", got, want)
	}
	if b.BuiltCommit != head {
		t.Fatalf("a build that found no file records commit %q, want %q", b.BuiltCommit, head)
	}
}

func TestAChildBuildByHandBuildsTheTipAndRecordsTheCommit(t *testing.T) {
	r := newRepo(t)
	r.commit("main", map[string]string{".kipp.yml": echoStages})
	first := r.commit("feature/x", nil)
	h := newHarness(t, map[string]string{"site": parentFile(r.dir, "")})
	h.withChildren(t, config.ChildRecord{Key: "main", Branch: "main"}, config.ChildRecord{Key: "feature-x-abcdef", Branch: "feature/x", HeadSHA: first})
	tip := r.commit("feature/x", nil) // pushed after the child last saw the branch
	b := h.wait(t, h.trigger(t, "site.feature-x-abcdef").ID)
	if got := h.log(t, b, "s"); b.Status != store.Success || got != "site.feature-x-abcdef cli feature/x main ["+tip+"] []\n" {
		t.Fatalf("%s: %q", b.Status, got)
	}
	got, err := h.st.Build(context.Background(), b.ID)
	if err != nil || got.BuiltCommit != tip || got.Parent != "site" || got.TriggerBranch != "feature/x" {
		t.Fatalf("the build records %+v, %v", got, err)
	}
	// A push builds the commit it named, not the tip.
	hook, err := h.d.Trigger(context.Background(), TriggerRequest{Job: "site.feature-x-abcdef", Kind: "webhook", Detail: first, Branch: "feature/x", DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if b := h.wait(t, hook.ID); b.BuiltCommit != first || !strings.Contains(h.log(t, b, "s"), "["+first+"]") {
		t.Fatalf("the webhook build built %s: %q", b.BuiltCommit, h.log(t, b, "s"))
	}
}

func TestAChildOnABranchIsNotOnTheDefaultBranchWhateverStartedIt(t *testing.T) {
	r := newRepo(t)
	stages := "stages:\n  always: {run: 'true'}\n  release: {run: 'true', when: {default_branch: true}}\n  check: {run: 'true', when: {default_branch: false}}\n"
	r.commit("main", map[string]string{".kipp.yml": stages})
	r.commit("feature-x", nil)
	h := newHarness(t, map[string]string{"site": parentFile(r.dir, "")})
	h.withChildren(t, config.ChildRecord{Key: "main", Branch: "main"}, config.ChildRecord{Key: "feature-x", Branch: "feature-x"})
	for job, release := range map[string]store.StageStatus{"site.main": store.StageSuccess, "site.feature-x": store.StageSkipped} {
		for _, kind := range []string{"api", "web", "cron", "after"} {
			b, err := h.d.Trigger(context.Background(), TriggerRequest{Job: job, Kind: kind, By: "test"})
			if err != nil {
				t.Fatal(err)
			}
			b = h.wait(t, b.ID)
			st := h.stages(t, b.ID)
			check := store.StageSkipped
			if release == store.StageSkipped {
				check = store.StageSuccess
			}
			if st["release"].Status != release || st["check"].Status != check || st["always"].Status != store.StageSuccess {
				t.Errorf("%s by %s: release %s, check %s, want %s and %s", job, kind, st["release"].Status, st["check"].Status, release, check)
			}
		}
	}
}

func TestTheTokenIsNotInAStagesEnvironmentUnlessInSecrets(t *testing.T) {
	r := newRepo(t)
	r.commit("main", map[string]string{".kipp.yml": "stages:\n  s: {run: 'echo token=${GH_TOKEN-unset}'}\n"})
	for extra, want := range map[string]string{"": "token=unset\n", "secrets: [GH_TOKEN]\n": "token=***\n"} {
		file := "name: site\nmultibranch: {repo: acme/site, clone: '" + r.dir + "', token: GH_TOKEN}\n" + extra
		h := newHarness(t, map[string]string{"site": file})
		h.withChildren(t, config.ChildRecord{Key: "main", Branch: "main"})
		b := h.wait(t, h.trigger(t, "site.main").ID)
		if got := h.log(t, b, "s"); b.Status != store.Success || got != want || strings.Contains(got, "value-of-GH_TOKEN") {
			t.Fatalf("secrets %q: %s: %q, want %q", extra, b.Status, got, want)
		}
	}
}
