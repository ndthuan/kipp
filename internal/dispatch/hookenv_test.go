package dispatch

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

func repoFile(file string) func(*Options) {
	return func(o *Options) {
		o.Clone = func(_ context.Context, dir, _, _ string, _ io.Writer) error {
			return os.WriteFile(filepath.Join(dir, "ci.yml"), []byte(file), 0o600)
		}
		o.Head = func(context.Context, string) (string, error) { return "abc", nil }
	}
}

const srcJob = "name: src\nsource: {repo: o/r, ref: main, path: ci.yml}\n"

func TestARepoFilesEnvSetsThePathOfItsStagesAndNeverTheHooks(t *testing.T) {
	h := newHarness(t, map[string]string{"src": srcJob + "on_success: {run: 'echo \"hook [$PATH]\"'}\n"},
		repoFile("env: {PATH: '.:/usr/bin:/bin'}\nstages:\n  s: {run: 'echo \"stage [$PATH]\"'}\n"))
	b := h.wait(t, h.trigger(t, "src").ID)
	if got := h.log(t, b, "s"); b.Status != store.Success || got != "stage [.:/usr/bin:/bin]\n" {
		t.Fatalf("%s: the stage's log is %q, and a stage keeps the PATH its file gives it", b.Status, got)
	}
	if got := h.log(t, b, config.HooksLog); got != "hook ["+os.Getenv("PATH")+"]\n" {
		t.Fatalf("the hook's log is %q, want the server's PATH", got)
	}
}

func TestARepoFilesEnvGivesTheHookANewNameAndNeverChangesOneOfTheJob(t *testing.T) {
	h := newHarness(t, map[string]string{"src": srcJob + "env: {OURS: job}\non_success: {run: 'echo \"hook [$OURS] [$THEIRS]\"'}\n"},
		repoFile("env: {OURS: repo, THEIRS: repo}\nstages:\n  s: {run: 'echo \"stage [$OURS] [$THEIRS]\"'}\n"))
	b := h.wait(t, h.trigger(t, "src").ID)
	if got := h.log(t, b, "s"); got != "stage [repo] [repo]\n" {
		t.Fatalf("%s: the stage's log is %q", b.Status, got)
	}
	if got := h.log(t, b, config.HooksLog); got != "hook [job] [repo]\n" {
		t.Fatalf("the hook's log is %q", got)
	}
}

func TestAStagesOutputGivesTheHookANewNameAndNeverChangesOneOfTheJob(t *testing.T) {
	h, b := runEnvJob(t, envJob(`
  a: {run: 'echo "OURS=stage" >> "$KIPP_ENV"; echo "NEW=stage" >> "$KIPP_ENV"'}
  b: {needs: [a], run: 'echo "b [$OURS] [$NEW]"'}
`, "env: {OURS: job}", "on_success: {run: 'echo \"hook [$OURS] [$NEW]\"'}"))
	if got := h.log(t, b, "b"); got != "b [stage] [stage]\n" {
		t.Fatalf("%s: the log of b is %q", b.Status, got)
	}
	if got := h.log(t, b, config.HooksLog); got != "hook [job] [stage]\n" {
		t.Fatalf("the hook's log is %q", got)
	}
}

func TestAHookIsNotGivenANameThatSteersItsShellOrItsProgramsByARepoOrAStage(t *testing.T) {
	hook := `echo "hook [${BASH_ENV-unset}] [${LD_PRELOAD-unset}] [${GIT_DIR-unset}] [${https_proxy-unset}] [$FINE]"`
	h := newHarness(t, map[string]string{"src": srcJob + "on_success:\n  run: |\n    " + hook + "\n"},
		repoFile("env: {BASH_ENV: ./rc, GIT_DIR: /elsewhere}\nstages:\n  s: {run: 'echo \"LD_PRELOAD=./lib.so\" >> \"$KIPP_ENV\"; echo \"https_proxy=http://192.0.2.1\" >> \"$KIPP_ENV\"; echo \"FINE=yes\" >> \"$KIPP_ENV\"'}\n"),
		func(o *Options) { o.PassEnv = []string{"PATH"} })
	b := h.wait(t, h.trigger(t, "src").ID)
	if got := h.log(t, b, config.HooksLog); b.Status != store.Success || got != "hook [unset] [unset] [unset] [unset] [yes]\n" {
		t.Fatalf("%s: the hook's log is %q", b.Status, got)
	}
}

func TestTheNamesAHookTakesOnlyFromTheServer(t *testing.T) {
	for _, name := range []string{"PATH", "HOME", "ENV", "BASH_ENV", "IFS", "SHELL", "CDPATH", "TMPDIR", "LD_PRELOAD", "LD_LIBRARY_PATH", "DYLD_INSERT_LIBRARIES",
		"GIT_SSH_COMMAND", "GIT_DIR", "HTTPS_PROXY", "http_proxy", "NO_PROXY", "SSL_CERT_FILE", "CURL_HOME", "PYTHONPATH", "NODE_OPTIONS", "PERL5LIB", "RUBYOPT", "XDG_CONFIG_HOME", "SSH_AUTH_SOCK"} {
		if !steersHook(name) {
			t.Errorf("%s may be given to a hook by a repo", name)
		}
	}
	for _, name := range []string{"TAG", "VERSION", "PATHS", "DEPLOY_HOST", "LDFLAGS", "GITHUB_SHA", "NOTES"} {
		if steersHook(name) {
			t.Errorf("%s is refused", name)
		}
	}
}
