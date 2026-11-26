package jobstore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/config"
)

func policyScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.sh")
	must(t, os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700))
	return path
}

func refusedLines(t *testing.T, err error) []string {
	t.Helper()
	refused, ok := errors.AsType[*config.Errors](err)
	if !ok {
		t.Fatalf("want a *config.Errors, got %v", err)
	}
	return strings.Split(refused.Error(), "\n")
}

func eq[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
}

func (f *fixture) policy(p *Policy) { f.jobs.o.Policy = p }

func TestAPolicyThatExitsZeroLetsTheChangeThrough(t *testing.T) {
	f := newFixture(t, Store, map[string]string{"a.yml": jobText("a", "echo a")})
	f.policy(&Policy{Path: policyScript(t, "exit 0")})
	_, err := f.jobs.Create(ctx, "made", []byte(jobText("made", "echo made")), byAPI)
	must(t, err)
	f.serves("made", "echo made", FromStore)
}

func TestAPolicyThatRefusesStopsTheChangeBeforeAnythingIsKept(t *testing.T) {
	for _, mode := range []Mode{Store, Files} {
		f := newFixture(t, mode, map[string]string{"a.yml": jobText("a", "echo a")})
		f.policy(&Policy{Path: policyScript(t, "echo no build on this host; echo second line >&2; exit 1")})
		published := f.published
		_, err := f.jobs.Create(ctx, "made", []byte(jobText("made", "echo made")), byAPI)
		got := refusedLines(t, err)
		if len(got) != 2 || got[0] != "job policy: no build on this host" || got[1] != "job policy: second line" {
			t.Fatalf("%s: refusal is %q", mode, got)
		}
		f.absent("made")
		eq(t, string(mode)+" published", f.published, published)
		if _, err := os.Stat(filepath.Join(f.dir, "made.yml")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s: the file was written: %v", mode, err)
		}
		eq(t, string(mode)+" edits", len(f.edits()), 0)
		_, err = f.jobs.Replace(ctx, "a", []byte(jobText("a", "echo changed")), f.etag("a"), byAPI)
		refusedLines(t, err)
		f.serves("a", "echo a", FromFile)
		_, err = f.jobs.Delete(ctx, "a", f.etag("a"), byAPI)
		refusedLines(t, err)
		f.serves("a", "echo a", FromFile)
	}
}

func TestAPolicyThatRunsPastItsTimeoutRefusesAndSaysSo(t *testing.T) {
	f := newFixture(t, Store, nil)
	f.policy(&Policy{Path: policyScript(t, "exec sleep 30"), Timeout: 200 * time.Millisecond})
	_, err := f.jobs.Create(ctx, "made", []byte(jobText("made", "echo made")), byAPI)
	got := refusedLines(t, err)
	if !strings.Contains(strings.Join(got, "\n"), "job policy: timed out after 200ms") {
		t.Fatalf("refusal is %q", got)
	}
	f.absent("made")
}

func TestAPolicyThatIsNotThereRefuses(t *testing.T) {
	f := newFixture(t, Store, nil)
	f.policy(&Policy{Path: filepath.Join(t.TempDir(), "missing")})
	_, err := f.jobs.Create(ctx, "made", []byte(jobText("made", "echo made")), byAPI)
	got := refusedLines(t, err)
	if len(got) != 1 || !strings.HasPrefix(got[0], "job policy: ") {
		t.Fatalf("refusal is %q", got)
	}
	f.absent("made")
}

func TestAPolicyIsGivenTheCandidateSetAndWhatChangedAndNothingElseOfTheServer(t *testing.T) {
	t.Setenv("KIPP_POLICY_TEST_SECRET", "hidden")
	f := newFixture(t, Store, map[string]string{"a.yml": jobText("a", "echo a"), "_vars.yml": "global:\n  X: one\n"})
	seen := filepath.Join(t.TempDir(), "seen")
	script := `{ echo "job=$KIPP_POLICY_JOB change=$KIPP_POLICY_CHANGE author=$KIPP_POLICY_AUTHOR extra=$EXTRA secret=$KIPP_POLICY_TEST_SECRET"; ls "$1"; } > ` + seen
	f.policy(&Policy{Path: policyScript(t, script), Env: []string{"EXTRA=yes", "PATH=" + os.Getenv("PATH")}})
	read := func() string {
		data, err := os.ReadFile(seen)
		must(t, err)
		return string(data)
	}
	_, err := f.jobs.Create(ctx, "b", []byte(jobText("b", "echo b")), byAPI)
	must(t, err)
	eq(t, "on create", read(), "job=b change=create author=admin extra=yes secret=\n_vars.yml\na.yml\nb.yml\n")
	_, err = f.jobs.Delete(ctx, "a", f.etag("a"), byAPI)
	must(t, err)
	eq(t, "on delete", read(), "job=a change=delete author=admin extra=yes secret=\n_vars.yml\nb.yml\n")
}

func TestAPolicyOutputPastTheLimitIsCutAndSaid(t *testing.T) {
	f := newFixture(t, Store, nil)
	f.policy(&Policy{Path: policyScript(t, "head -c 20000 /dev/zero | tr '\\0' x; exit 1")})
	_, err := f.jobs.Create(ctx, "made", []byte(jobText("made", "echo made")), byAPI)
	got := refusedLines(t, err)
	if len(got) != 2 || len(got[0]) > PolicyOutputLimit+len("job policy: ") || got[1] != "job policy: (output cut at 8 KiB)" {
		t.Fatalf("refusal is %d lines, first %d bytes", len(got), len(got[0]))
	}
}

func TestALoadServesASetThePolicyWouldRefuse(t *testing.T) {
	f := newFixture(t, Store, map[string]string{"a.yml": jobText("a", "echo a")})
	f.policy(&Policy{Path: policyScript(t, "exit 1")})
	f.write("b.yml", jobText("b", "echo b"))
	f.load()
	f.serves("b", "echo b", FromFile)
}

func TestAnAbsentPolicyChangesNothing(t *testing.T) {
	var p *Policy
	must(t, p.Run(ctx, config.Files{}, PolicyChange{}))
}

func TestAPolicyRefusesAFileNameThatIsNotOneNameInItsDirectory(t *testing.T) {
	outside := t.TempDir()
	p := &Policy{Path: policyScript(t, "exit 0")}
	for _, name := range []string{"../escape.yml", filepath.Join(outside, "escape.yml"), "sub/a.yml", "..", ""} {
		err := p.Run(ctx, config.Files{name: []byte("x")}, PolicyChange{Kind: ChangeCheck})
		if err == nil {
			t.Fatalf("%q: the policy ran", name)
		}
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("a file was written outside the policy's directory: %v", entries)
	}
}
