package dispatch

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

// tokens gives the dispatcher the build tokens a server makes, and the
// service that resolves them as the API does.
func tokens(t *testing.T) (func(*Options), *auth.Service) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "auth.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	bt := auth.NewBuildTokens()
	svc := auth.New(st, time.Hour, []byte("device-key"))
	svc.UseBuildTokens(bt)
	return func(o *Options) { o.BuildTokens = bt }, svc
}

// holding is a stage's command that writes its KIPP_TOKEN to a file the
// test reads, and waits for the test to let it go.
func holding(dir string) string {
	return "printf %s \"$KIPP_TOKEN\" > '" + dir + "/token'; until [ -f '" + dir + "/go' ]; do sleep 0.05; done"
}

// liveToken is the token of build id once its stage has written it, and
// release the way to let the stage end.
func liveToken(t *testing.T, dir string) (token string, release func()) {
	t.Helper()
	eventually(t, "the stage wrote its token", func() bool {
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Error(err)
			return true
		}
		defer func() { _ = root.Close() }()
		data, err := root.ReadFile("token")
		token = string(data)
		return err == nil && token != ""
	})
	return token, func() {
		if err := os.WriteFile(filepath.Join(dir, "go"), nil, 0o600); err != nil {
			t.Error(err)
		}
	}
}

func can(t *testing.T, svc *auth.Service, token, job, action string) bool {
	t.Helper()
	u, err := svc.UserFromToken(context.Background(), token)
	if err != nil {
		t.Fatalf("the token is refused: %v", err)
	}
	ok, err := svc.Can(context.Background(), u, job, action)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestAStageIsGivenATokenThatViewsItsOwnJobAndNothingElse(t *testing.T) {
	opt, svc := tokens(t)
	dir := t.TempDir()
	h := newHarness(t, map[string]string{
		"a": "name: a\n" + stage(holding(dir)),
		"b": "name: b\nstages:\n  s: {run: 'true'}\n",
	}, opt)
	b := h.trigger(t, "a")
	token, release := liveToken(t, dir)
	if !strings.HasPrefix(token, auth.BuildTokenPrefix) {
		t.Fatalf("the token is %q", token)
	}
	for _, c := range []struct {
		job, action string
		want        bool
	}{
		{"a", "view", true}, {"a", "build", false}, {"a", "cancel", false},
		{"b", "view", false}, {"b", "build", false}, {"b", "cancel", false},
	} {
		if got := can(t, svc, token, c.job, c.action); got != c.want {
			t.Errorf("%s on %s: %v, want %v", c.action, c.job, got, c.want)
		}
	}
	release()
	h.wait(t, b.ID)
}

func TestPermissionsOnAServerJobGrantExactlyWhatTheyListAliasesIncluded(t *testing.T) {
	opt, svc := tokens(t)
	dir := t.TempDir()
	h := newHarness(t, map[string]string{
		"a": "name: a\npermissions:\n  self: [view, cancel]\n  b: [view, build]\n  c: read\n  d: write\n" + stage(holding(dir)),
		"b": "name: b\nstages:\n  s: {run: 'true'}\n",
		"c": "name: c\nstages:\n  s: {run: 'true'}\n",
		"d": "name: d\nstages:\n  s: {run: 'true'}\n",
	}, opt)
	b := h.trigger(t, "a")
	token, release := liveToken(t, dir)
	for _, c := range []struct {
		job, action string
		want        bool
	}{
		{"a", "view", true}, {"a", "cancel", true}, {"a", "build", false},
		{"b", "view", true}, {"b", "build", true}, {"b", "cancel", false},
		{"c", "view", true}, {"c", "build", false},
		{"d", "view", true}, {"d", "build", true}, {"d", "cancel", true},
	} {
		if got := can(t, svc, token, c.job, c.action); got != c.want {
			t.Errorf("%s on %s: %v, want %v", c.action, c.job, got, c.want)
		}
	}
	release()
	h.wait(t, b.ID)
}

func TestPermissionsNoneGivesNoKippToken(t *testing.T) {
	opt, _ := tokens(t)
	h := newHarness(t, map[string]string{
		"a": "name: a\npermissions: none\nstages:\n  s: {run: 'echo \"[${KIPP_TOKEN-unset}]\"'}\n",
	}, opt)
	b := h.wait(t, h.trigger(t, "a").ID)
	if got := h.log(t, b, "s"); got != "[unset]\n" {
		t.Fatalf("the stage was given %q", got)
	}
}

func TestTheBuildsTokenWinsOverAKippTokenASecretGives(t *testing.T) {
	opt, _ := tokens(t)
	h := newHarness(t, map[string]string{
		"a": "name: a\nsecrets: [KIPP_TOKEN]\nstages:\n  s: {run: 'echo \"${KIPP_TOKEN%%_*}\"'}\n",
	}, opt)
	b := h.wait(t, h.trigger(t, "a").ID)
	if got := h.log(t, b, "s"); got != "kippb\n" {
		t.Fatalf("the stage was given %q, not the build's token", got)
	}
}

func TestTheTokenIsMaskedInTheBuildLog(t *testing.T) {
	opt, _ := tokens(t)
	h := newHarness(t, map[string]string{
		"a": "name: a\nstages:\n  s: {run: 'echo \"token=$KIPP_TOKEN\"'}\non_success: {run: 'echo \"hook=$KIPP_TOKEN\"'}\n",
	}, opt)
	b := h.wait(t, h.trigger(t, "a").ID)
	for stage, want := range map[string]string{"s": "token=***\n", "hooks": "hook=***\n"} {
		if got := h.log(t, b, stage); got != want {
			t.Errorf("%s log %q, want %q", stage, got, want)
		}
	}
}

func TestTheTokenIsRefusedOnceTheBuildEndsHoweverItEnds(t *testing.T) {
	for how, want := range map[string]store.Status{"success": store.Success, "failure": store.Failed, "cancelled": store.Cancelled} {
		t.Run(how, func(t *testing.T) {
			opt, svc := tokens(t)
			dir := t.TempDir()
			run := holding(dir)
			if how == "failure" {
				run += "; exit 1"
			}
			h := newHarness(t, map[string]string{"a": "name: a\n" + stage(run)}, opt)
			b := h.trigger(t, "a")
			token, release := liveToken(t, dir)
			if !can(t, svc, token, "a", "view") {
				t.Fatal("the token works while the build runs")
			}
			h.end(t, b.ID, how == "cancelled", release)
			if end := h.wait(t, b.ID); end.Status != want {
				t.Fatalf("the build ended %s", end.Status)
			}
			eventually(t, "the token is revoked", func() bool {
				_, err := svc.UserFromToken(context.Background(), token)
				return errors.Is(err, store.ErrNotFound)
			})
		})
	}
}

// end ends a build: cancelled by a user, or by letting its stage go.
func (h *harness) end(t *testing.T, id int64, cancel bool, release func()) {
	t.Helper()
	if !cancel {
		release()
		return
	}
	if err := h.d.Cancel(context.Background(), id, "test"); err != nil {
		t.Fatal(err)
	}
}

func TestEveryBuildHasItsOwnToken(t *testing.T) {
	opt, svc := tokens(t)
	dir1, dir2 := t.TempDir(), t.TempDir()
	h := newHarness(t, map[string]string{
		"a": "name: a\nconcurrency: parallel\nparams: {WHERE: {}}\nstages:\n  s: {run: 'printf %s \"$KIPP_TOKEN\" > \"$WHERE/token\"; until [ -f \"$WHERE/go\" ]; do sleep 0.05; done'}\n",
	}, opt)
	var ids []int64
	for _, dir := range []string{dir1, dir2} {
		b, err := h.d.Trigger(context.Background(), TriggerRequest{Job: "a", Kind: "cli", By: "t", Params: map[string]string{"WHERE": dir}})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, b.ID)
	}
	one, release1 := liveToken(t, dir1)
	two, release2 := liveToken(t, dir2)
	if one == two {
		t.Fatal("two builds shared a token")
	}
	release1()
	h.wait(t, ids[0])
	eventually(t, "the first token is revoked", func() bool {
		_, err := svc.UserFromToken(context.Background(), one)
		return errors.Is(err, store.ErrNotFound)
	})
	if !can(t, svc, two, "a", "view") {
		t.Fatal("ending a build ended another's token")
	}
	release2()
	h.wait(t, ids[1])
}

// stagesFile is a repo's stages file asking for permissions.
func stagesFile(permissions, run string) string { return permissions + stage(run) }

// stage is a stages: key with one stage s that runs run.
func stage(run string) string { return "stages:\n  s:\n    run: |\n      " + run + "\n" }

func withRepoFile(content string) func(*Options) {
	return func(o *Options) {
		o.Clone = func(_ context.Context, dir, _, _ string, _ io.Writer) error {
			return os.WriteFile(filepath.Join(dir, "kipp.yml"), []byte(content), 0o600)
		}
	}
}

func TestASourceJobsFileWithinItsCeilingGetsWhatItAsksFor(t *testing.T) {
	opt, svc := tokens(t)
	dir := t.TempDir()
	h := newHarness(t, map[string]string{
		"src":   "name: src\nmax_permissions:\n  other: [view, build]\nsource: {repo: o/r, ref: main, path: kipp.yml}\n",
		"other": "name: other\nstages:\n  s: {run: 'true'}\n",
	}, opt, withRepoFile(stagesFile("permissions:\n  other: build\n", holding(dir))))
	b := h.trigger(t, "src")
	token, release := liveToken(t, dir)
	if !can(t, svc, token, "other", "build") || can(t, svc, token, "other", "cancel") || !can(t, svc, token, "src", "view") {
		t.Fatal("the token holds what the file asked for within the ceiling")
	}
	release()
	h.wait(t, b.ID)
}

func TestASourceJobsFileOverItsCeilingFailsBeforeItsFirstStage(t *testing.T) {
	opt, _ := tokens(t)
	h := newHarness(t, map[string]string{
		"src":   "name: src\nmax_permissions:\n  other: view\nsource: {repo: o/r, ref: main, path: kipp.yml}\n",
		"other": "name: other\nstages:\n  s: {run: 'true'}\n",
	}, opt, withRepoFile(stagesFile("permissions:\n  other: write\n", "echo ran")))
	b := h.wait(t, h.trigger(t, "src").ID)
	prepare := h.log(t, b, "prepare")
	if b.Status != store.Failed || !strings.Contains(prepare, "other: build, cancel") || !strings.Contains(prepare, "job src would have to allow") {
		t.Fatalf("%s: %q", b.Status, prepare)
	}
	if _, err := os.Stat(h.d.LogPath("src", b.Number, "s")); err == nil {
		t.Fatal("a stage ran")
	}
}

func TestASourceJobWithNoCeilingAllowsOnlyTheDefault(t *testing.T) {
	opt, _ := tokens(t)
	h := newHarness(t, map[string]string{
		"src": "name: src\nsource: {repo: o/r, ref: main, path: kipp.yml}\n",
	}, opt, withRepoFile(stagesFile("permissions:\n  self: cancel\n", "echo ran")))
	b := h.wait(t, h.trigger(t, "src").ID)
	if b.Status != store.Failed || !strings.Contains(h.log(t, b, "prepare"), "src: cancel") {
		t.Fatalf("%s: %q", b.Status, h.log(t, b, "prepare"))
	}
}

func TestAStagesFileThatSaysNoneTakesTheTokenAway(t *testing.T) {
	opt, _ := tokens(t)
	h := newHarness(t, map[string]string{
		"src": "name: src\nsource: {repo: o/r, ref: main, path: kipp.yml}\n",
	}, opt, withRepoFile(stagesFile("permissions: none\n", "echo \"[${KIPP_TOKEN-unset}]\"")))
	b := h.wait(t, h.trigger(t, "src").ID)
	if got := h.log(t, b, "s"); b.Status != store.Success || got != "[unset]\n" {
		t.Fatalf("%s: %q", b.Status, got)
	}
}

func TestAStagesFileThatAsksToBuildItsOwnJobFailsTheBuild(t *testing.T) {
	opt, _ := tokens(t)
	h := newHarness(t, map[string]string{
		"src":   "name: src\nmax_permissions:\n  other: view\nsource: {repo: o/r, ref: main, path: kipp.yml}\n",
		"other": "name: other\nstages:\n  s: {run: 'true'}\n",
	}, opt, withRepoFile(stagesFile("permissions:\n  self: build\n", "echo ran")))
	b := h.wait(t, h.trigger(t, "src").ID)
	if b.Status != store.Failed || !strings.Contains(h.log(t, b, "prepare"), "its own job") {
		t.Fatalf("%s: %q", b.Status, h.log(t, b, "prepare"))
	}
}

func childHarness(t *testing.T, repoFile, parentExtra string) (*harness, *auth.Service, string) {
	t.Helper()
	opt, svc := tokens(t)
	dir := t.TempDir()
	r := newRepo(t)
	r.commit("main", map[string]string{".kipp.yml": stagesFile(repoFile, holding(dir))})
	h := newHarness(t, map[string]string{
		"site":  parentFile(r.dir, parentExtra),
		"other": "name: other\nstages:\n  s: {run: 'true'}\n",
	}, opt)
	h.withChildren(t, config.ChildRecord{Key: "main", Branch: "main"})
	return h, svc, dir
}

func TestAChildWithinItsParentsCeilingGetsWhatItsFileAsksForAndSelfIsTheChild(t *testing.T) {
	h, svc, dir := childHarness(t, "permissions:\n  other: [view, build]\n  self: cancel\n",
		"permissions: {other: write}\nmax_permissions:\n  other: [view, build]\n  self: cancel\n")
	b := h.trigger(t, "site.main")
	token, release := liveToken(t, dir)
	for _, c := range []struct {
		job, action string
		want        bool
	}{
		{"site.main", "view", true}, {"site.main", "cancel", true}, {"site.main", "build", false},
		{"other", "build", true}, {"other", "cancel", false},
		// The parent's own permissions are not its children's.
		{"site", "view", false},
	} {
		if got := can(t, svc, token, c.job, c.action); got != c.want {
			t.Errorf("%s on %s: %v, want %v", c.action, c.job, got, c.want)
		}
	}
	release()
	h.wait(t, b.ID)
}

func TestAChildOverItsParentsCeilingFailsBeforeItsFirstStageNamingTheParent(t *testing.T) {
	h, _, _ := childHarness(t, "permissions:\n  other: write\n", "max_permissions:\n  other: view\n")
	b := h.wait(t, h.trigger(t, "site.main").ID)
	prepare := h.log(t, b, "prepare")
	for _, want := range []string{"other: build, cancel", "job site would have to allow", ".kipp.yml"} {
		if b.Status != store.Failed || !strings.Contains(prepare, want) {
			t.Fatalf("%s: %q lacks %q", b.Status, prepare, want)
		}
	}
	if _, err := os.Stat(h.d.LogPath("site.main", b.Number, "s")); err == nil {
		t.Fatal("a stage ran")
	}
}

func TestAChildWithNoCeilingAllowsOnlyTheDefault(t *testing.T) {
	h, _, _ := childHarness(t, "permissions:\n  other: read\n", "")
	b := h.wait(t, h.trigger(t, "site.main").ID)
	if b.Status != store.Failed || !strings.Contains(h.log(t, b, "prepare"), "other: view") {
		t.Fatalf("%s: %q", b.Status, h.log(t, b, "prepare"))
	}
}

func TestAChildThatAsksForNothingHoldsTheDefaultWhateverItsParentsPermissionsSay(t *testing.T) {
	h, svc, dir := childHarness(t, "", "permissions: {other: write}\n")
	b := h.trigger(t, "site.main")
	token, release := liveToken(t, dir)
	if can(t, svc, token, "other", "view") || !can(t, svc, token, "site.main", "view") {
		t.Fatal("a child holds view on itself alone")
	}
	release()
	h.wait(t, b.ID)
}

func TestTheHookIsGivenTheTokenAndItWorksUntilTheHookHasRun(t *testing.T) {
	opt, svc := tokens(t)
	dir := t.TempDir()
	h := newHarness(t, map[string]string{
		"a": "name: a\nstages:\n  s: {run: 'true'}\non_success:\n  run: |\n    printf %s \"$KIPP_TOKEN\" > '" + dir + "/token'\n",
	}, opt)
	b := h.wait(t, h.trigger(t, "a").ID)
	token, _ := liveToken(t, dir)
	if b.Status != store.Success || !strings.HasPrefix(token, auth.BuildTokenPrefix) {
		t.Fatalf("%s: the hook was given %q", b.Status, token)
	}
	eventually(t, "the token is revoked after the hook", func() bool {
		_, err := svc.UserFromToken(context.Background(), token)
		return errors.Is(err, store.ErrNotFound)
	})
}
