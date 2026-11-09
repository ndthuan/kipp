package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/runnerhub"
	"github.com/ndthuan/kipp/internal/store"
)

func withSecrets(all map[string]Secret) func(*Options) {
	return func(o *Options) {
		o.Secrets = func(_ context.Context, names []string) (map[string]Secret, error) {
			out := map[string]Secret{}
			for _, n := range names {
				sec := all[n]
				if sec.Scopes == nil {
					sec.Scopes = []string{config.ScopeGlobal}
				}
				out[n] = sec
			}
			return out, nil
		}
	}
}

// binary holds bytes that are not UTF-8 and a NUL.
var binary = []byte{0x00, 0xff, 0xfe, 'k', 'e', 'y', 0x80, 0x01}

const fileJob = `
name: f
secrets: [KEYFILE]
stages:
  s:
    run: 'echo path=$KEYFILE; ls -l "$KEYFILE" | cut -c1-10; od -An -tx1 "$KEYFILE" | tr -s " \\n" " "; echo; case "$KEYFILE" in "$KIPP_WORKSPACE"/*) echo inside;; *) echo outside;; esac; %s'
`

func pathOf(t *testing.T, log string) string {
	t.Helper()
	line, _, _ := strings.Cut(log, "\n")
	path, ok := strings.CutPrefix(line, "path=")
	if !ok {
		t.Fatalf("no path in %q", log)
	}
	return path
}

func TestAFileSecretIsAPathToAPrivateFileOfTheExactBytesOutsideTheWorkspace(t *testing.T) {
	h := newHarness(t, map[string]string{"f": strings.Replace(fileJob, "%s", "true", 1)},
		withSecrets(map[string]Secret{"KEYFILE": {Kind: config.SecretFile, Data: binary}}))
	b := h.wait(t, h.trigger(t, "f").ID)
	log := h.log(t, b, "s")
	if b.Status != store.Success {
		t.Fatalf("%v: %s", b.Status, log)
	}
	for _, want := range []string{"-rw-------", " 00 ff fe 6b 65 79 80 01", "outside"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q: %s", want, log)
		}
	}
	if _, err := os.Stat(pathOf(t, log)); !os.IsNotExist(err) {
		t.Fatalf("the file is still there after success: %v", err)
	}
}

func TestAFileSecretIsGoneAfterAFailedStage(t *testing.T) {
	h := newHarness(t, map[string]string{"f": strings.Replace(fileJob, "%s", "exit 3", 1)},
		withSecrets(map[string]Secret{"KEYFILE": {Kind: config.SecretFile, Data: binary}}))
	b := h.wait(t, h.trigger(t, "f").ID)
	if b.Status != store.Failed {
		t.Fatalf("%v", b.Status)
	}
	if _, err := os.Stat(pathOf(t, h.log(t, b, "s"))); !os.IsNotExist(err) {
		t.Fatalf("the file is still there after a failure: %v", err)
	}
}

func TestAFileSecretIsGoneAfterATimeoutAndAfterACancel(t *testing.T) {
	for what, job := range map[string]string{
		"a timeout": strings.Replace(fileJob, "name: f\n", "name: f\ntimeout: 1s\n", 1),
		"a cancel":  fileJob,
	} {
		h := newHarness(t, map[string]string{"f": strings.Replace(job, "%s", "sleep 60", 1)},
			withSecrets(map[string]Secret{"KEYFILE": {Kind: config.SecretFile, Data: binary}}))
		b := h.trigger(t, "f")
		if what == "a cancel" {
			h.untilRunning(t, b.ID)
			eventually(t, "the log has its path", func() bool {
				data, _ := os.ReadFile(h.d.LogPath(b.Job, b.Number, "s"))
				return strings.Contains(string(data), "outside")
			})
			if err := h.d.Cancel(context.Background(), b.ID, "ada"); err != nil {
				t.Fatal(err)
			}
		}
		done := h.wait(t, b.ID)
		if _, err := os.Stat(pathOf(t, h.log(t, done, "s"))); !os.IsNotExist(err) {
			t.Fatalf("%s: the file is still there: %v", what, err)
		}
	}
}

func TestAServerStartedAfterACrashRemovesTheFilesAStageLeft(t *testing.T) {
	h := newHarness(t, map[string]string{"f": strings.Replace(fileJob, "%s", "true", 1)})
	left := filepath.Join(h.d.secretFilesDir(), "s-dead", "KEYFILE")
	if err := os.MkdirAll(filepath.Dir(left), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(left, binary, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.d.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(left); !os.IsNotExist(err) {
		t.Fatalf("the file a dead stage left is still there: %v", err)
	}
}

func TestAUserAndPasswordSecretGivesThreeVariablesAndMasksThePasswordNotTheUser(t *testing.T) {
	h := newHarness(t, map[string]string{"u": `
name: u
secrets: [LOGIN]
stages:
  s: {run: 'echo "all=$LOGIN"; echo "user=$LOGIN_USR"; echo "pass=$LOGIN_PSW"'}
`}, withSecrets(map[string]Secret{"LOGIN": {Kind: config.SecretUserPass, User: "deployer", Value: "pa55-word-long"}}))
	b := h.wait(t, h.trigger(t, "u").ID)
	if got, want := h.log(t, b, "s"), "all=***\nuser=deployer\npass=***\n"; got != want {
		t.Fatalf("log %q, want %q", got, want)
	}
}

func TestAUTF8FileIsMaskedLineByLineAndANonUTF8FileIsNot(t *testing.T) {
	pem := []byte("-----BEGIN KEY-----\nabcdefghijklmnop\nshort\n  indented-line-here  \n-----END KEY-----\n")
	h := newHarness(t, map[string]string{"f": strings.Replace(fileJob, "%s", `cat "$KEYFILE"`, 1)},
		withSecrets(map[string]Secret{"KEYFILE": {Kind: config.SecretFile, Data: pem}}))
	b := h.wait(t, h.trigger(t, "f").ID)
	log := h.log(t, b, "s")
	for _, leaked := range []string{"BEGIN KEY", "abcdefghijklmnop", "indented-line-here", "END KEY"} {
		if strings.Contains(log, leaked) {
			t.Errorf("%q leaked: %s", leaked, log)
		}
	}
	if !strings.Contains(log, "\nshort\n") {
		t.Errorf("a line under 8 characters is not masked: %s", log)
	}

	h = newHarness(t, map[string]string{"f": strings.Replace(fileJob, "%s", `cat "$KEYFILE"`, 1)},
		withSecrets(map[string]Secret{"KEYFILE": {Kind: config.SecretFile, Data: []byte("\xff\xfe not utf8 line here\n")}}))
	b = h.wait(t, h.trigger(t, "f").ID)
	if b.Status != store.Success || !strings.Contains(h.log(t, b, "s"), "not utf8 line here") {
		t.Fatalf("%v: %s", b.Status, h.log(t, b, "s"))
	}
}

func TestAUserAndPasswordSecretRefusesTheNamesItGivesWhenAnEnvOrAParamHasThem(t *testing.T) {
	secrets := withSecrets(map[string]Secret{"LOGIN": {Kind: config.SecretUserPass, User: "deployer", Value: "pa55-word-long"}})
	for what, extra := range map[string]string{
		"an env key":    "env: {LOGIN_USR: other}\n",
		"another param": "params:\n  LOGIN_PSW: {type: string}\n",
	} {
		h := newHarness(t, map[string]string{"u": "name: u\nsecrets: [LOGIN]\n" + extra + "stages:\n  s: {run: 'true'}\n"}, secrets)
		b := h.wait(t, h.trigger(t, "u").ID)
		if got := h.log(t, b, "prepare"); b.Status != store.Failed || !strings.Contains(got, "LOGIN") {
			t.Errorf("%s: %v %q", what, b.Status, got)
		}
	}
}

func TestAFileSecretOnAnRunnerIsWrittenByTheRunnerOutsideItsWorkspaceAndRemoved(t *testing.T) {
	ag := newRunners(t, runnerhub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
	dir := t.TempDir()
	ag.runBox(dir)
	job := strings.Replace(fileJob, "stages:\n  s:\n    run:", "stages:\n  s:\n    runs_on: linux\n    run:", 1)
	h := newHarness(t, map[string]string{"f": strings.Replace(job, "%s", "true", 1)}, ag.opts,
		withSecrets(map[string]Secret{"KEYFILE": {Kind: config.SecretFile, Data: binary}}))
	b := h.wait(t, h.trigger(t, "f").ID)
	log := h.log(t, b, "s")
	if b.Status != store.Success {
		t.Fatalf("%v: %s", b.Status, log)
	}
	for _, want := range []string{"-rw-------", " 00 ff fe 6b 65 79 80 01", "outside"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q: %s", want, log)
		}
	}
	path := pathOf(t, log)
	if strings.HasPrefix(path, dir) {
		t.Errorf("the file %s is inside the runner's workdir %s", path, dir)
	}
	eventually(t, "the runner removed the file", func() bool {
		_, err := os.Stat(path)
		return os.IsNotExist(err)
	})
}

func TestABuildWhoseJobIsOutsideASecretsScopesFailsInPrepareNamingBothAndNoValue(t *testing.T) {
	const value = "a-secret-value-nobody-sees"
	h := newHarness(t, map[string]string{
		"in-group": "name: in-group\ngroup: site\nsecrets: [TOKEN]\nstages:\n  s: {run: 'echo got=$TOKEN'}\n",
		"by-name":  "name: by-name\nsecrets: [TOKEN]\nstages:\n  s: {run: 'echo got=$TOKEN'}\n",
		"outside":  "name: outside\ngroup: billing\nsecrets: [TOKEN]\nstages:\n  s: {run: echo never}\non_failure: {run: 'echo hook=$KIPP_STATUS'}\n",
	}, withSecrets(map[string]Secret{"TOKEN": {Kind: config.SecretText, Value: value, Scopes: []string{"group:site", "job:by-*"}}}))
	for _, name := range []string{"in-group", "by-name"} {
		if b := h.wait(t, h.trigger(t, name).ID); b.Status != store.Success || h.log(t, b, "s") != "got=***\n" {
			t.Fatalf("%s: %v %q", name, b.Status, h.log(t, b, "s"))
		}
	}
	b := h.wait(t, h.trigger(t, "outside").ID)
	if b.Status != store.Failed {
		t.Fatalf("%v", b.Status)
	}
	if got, want := h.log(t, b, "prepare"), "kipp: secret TOKEN is not in scope for job outside\n"; got != want {
		t.Fatalf("prepare log %q, want %q", got, want)
	}
	if got := h.log(t, b, "hooks"); got != "hook=failed\n" {
		t.Fatalf("hooks %q", got)
	}
	if strings.Contains(h.log(t, b, "prepare"), value) {
		t.Fatal("the prepare log holds the value")
	}
}
