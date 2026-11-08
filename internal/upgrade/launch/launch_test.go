package launch

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/upgrade/state"
)

var (
	buildMu sync.Mutex
	built   = map[string]string{}
	buildIn string
)

// fake builds the stand-in kipp stamped with ver, once per version.
func fake(t *testing.T, ver string) string {
	t.Helper()
	buildMu.Lock()
	defer buildMu.Unlock()
	if p, ok := built[ver]; ok {
		return p
	}
	if buildIn == "" {
		dir, err := os.MkdirTemp("", "kipp-fake-")
		if err != nil {
			t.Fatal(err)
		}
		buildIn = dir
	}
	out := filepath.Join(buildIn, "kipp-"+ver)
	cmd := exec.Command("go", "build", "-ldflags", "-X main.ver="+ver, "-o", out, "./testdata/fakekipp")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the fake kipp %s: %v\n%s", ver, err, b)
	}
	built[ver] = out
	return out
}

func TestMain(m *testing.M) {
	code := m.Run()
	if buildIn != "" {
		_ = os.RemoveAll(buildIn)
	}
	os.Exit(code)
}

func copyTo(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o755); err != nil {
		t.Fatal(err)
	}
}

// install puts a fake stamped ver in <data>/bin/ as the binary of the
// version named by as (as ver unless a test needs the two to differ).
func install(t *testing.T, data, ver, as string) {
	t.Helper()
	copyTo(t, fake(t, ver), state.BinaryPath(data, as))
}

// outcome is what a run of serve reported.
type outcome struct {
	ran, exe, launched, bundledExe, bundledVersion string
	pid                                            int
	stderr                                         string
}

// serve starts the executable exe as "serve a b" on data, with env added,
// and reads which stand-in ended up serving.
func serve(t *testing.T, exe, data string, env ...string) outcome {
	t.Helper()
	cmd := exec.Command(exe, "serve", "--data", data)
	cmd.Env = append(os.Environ(), "FAKE_DATA="+data)
	cmd.Env = append(cmd.Env, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("%v\n%s%s", err, stdout.String(), stderr.String())
	}
	line := strings.TrimSpace(stdout.String())
	f := strings.Fields(line)
	if len(f) < 7 || f[0] != "RAN" {
		t.Fatalf("unexpected output %q", line)
	}
	val := func(prefix string) string {
		for _, x := range f {
			if v, ok := strings.CutPrefix(x, prefix); ok {
				return v
			}
		}
		return ""
	}
	var gotPID int
	_, _ = fmt.Sscanf(val("pid="), "%d", &gotPID)
	if gotPID != pid {
		t.Fatalf("the process ID changed from %d to %d", pid, gotPID)
	}
	return outcome{
		ran: f[1], exe: val("exe="), launched: val("launched="), bundledExe: val("bundled_exe="),
		bundledVersion: val("bundled_version="), pid: gotPID, stderr: stderr.String(),
	}
}

func realPath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func setup(t *testing.T, own, bin string) (exe, data string) {
	t.Helper()
	data = t.TempDir()
	exe = filepath.Join(t.TempDir(), "kipp")
	copyTo(t, fake(t, own), exe)
	if bin != "" {
		install(t, data, bin, bin)
		if err := state.WriteCurrent(data, bin); err != nil {
			t.Fatal(err)
		}
	}
	return exe, data
}

func TestTheHigherDataDirectoryBinaryRunsInPlaceOfTheExecutable(t *testing.T) {
	exe, data := setup(t, "v1.0.0", "v1.0.1")
	o := serve(t, exe, data)
	if o.ran != "v1.0.1" {
		t.Fatalf("ran %s", o.ran)
	}
	if realPath(t, o.exe) != realPath(t, state.BinaryPath(data, "v1.0.1")) {
		t.Fatalf("exe %s", o.exe)
	}
	if o.launched != "1" || o.bundledVersion != "v1.0.0" {
		t.Fatalf("%+v", o)
	}
	if realPath(t, o.bundledExe) != realPath(t, exe) {
		t.Fatalf("bundled exe %s, started as %s", o.bundledExe, exe)
	}
}

func TestTheHigherExecutableRunsWhenTheDataDirectoryHoldsAnOlderBinary(t *testing.T) {
	exe, data := setup(t, "v1.0.2", "v1.0.1")
	o := serve(t, exe, data)
	if o.ran != "v1.0.2" || o.launched != "" || o.bundledExe != "" {
		t.Fatalf("%+v", o)
	}
}

func TestEqualVersionsDoNotSwitch(t *testing.T) {
	exe, data := setup(t, "v1.0.1", "v1.0.1")
	o := serve(t, exe, data)
	if o.ran != "v1.0.1" || o.launched != "" {
		t.Fatalf("%+v", o)
	}
}

func TestVersionsCompareAsNumbersNotAsText(t *testing.T) {
	exe, data := setup(t, "v1.9.0", "v1.10.0")
	if o := serve(t, exe, data); o.ran != "v1.10.0" {
		t.Fatalf("ran %s", o.ran)
	}
}

func TestADevBuildIgnoresTheDataDirectoryAndCleansNothingInIt(t *testing.T) {
	exe, data := setup(t, "dev", "v9.0.0")
	install(t, data, "v1.0.0", "v0.0.1")
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(state.BinaryPath(data, "v0.0.1"), old, old); err != nil {
		t.Fatal(err)
	}
	o := serve(t, exe, data)
	if o.ran != "dev" || o.launched != "" {
		t.Fatalf("%+v", o)
	}
	if _, err := os.Stat(state.BinaryPath(data, "v0.0.1")); err != nil {
		t.Fatal("a dev build cleaned bin/:", err)
	}
}

func TestAPreReleaseExecutableIsNotARelease(t *testing.T) {
	exe, data := setup(t, "v1.0.0-rc1", "v1.0.1")
	if o := serve(t, exe, data); o.ran != "v1.0.0-rc1" {
		t.Fatalf("ran %s", o.ran)
	}
}

func TestABinaryOfAnotherThanAReleaseVersionIsIgnored(t *testing.T) {
	exe, data := setup(t, "v1.0.0", "")
	install(t, data, "v1.0.5-rc1", "v1.0.5-rc1")
	for _, name := range []string{"v1.0.5-rc1", "dev"} {
		if err := os.WriteFile(filepath.Join(state.BinDir(data), "current"), []byte(name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if o := serve(t, exe, data); o.ran != "v1.0.0" {
			t.Fatalf("current %s: ran %s", name, o.ran)
		}
	}
}

func TestABinaryThatSaysAnotherVersionThanItsNameFallsBackWithOneLogLine(t *testing.T) {
	exe, data := setup(t, "v1.0.0", "")
	install(t, data, "v1.0.1", "v1.0.2")
	if err := state.WriteCurrent(data, "v1.0.2"); err != nil {
		t.Fatal(err)
	}
	o := serve(t, exe, data)
	if o.ran != "v1.0.0" || o.launched != "" {
		t.Fatalf("%+v", o)
	}
	if n := strings.Count(strings.TrimSpace(o.stderr), "\n") + 1; n != 1 || !strings.Contains(o.stderr, "v1.0.2") {
		t.Fatalf("log lines: %q", o.stderr)
	}
}

func TestABrokenBinaryFallsBackWithOneLogLine(t *testing.T) {
	cases := map[string]func(t *testing.T, path string){
		"missing":        func(t *testing.T, path string) {},
		"not executable": func(t *testing.T, path string) { writeFile(t, path, "#!/bin/sh\necho kipp v1.0.1\n", 0o600) },
		"version fails":  func(t *testing.T, path string) { writeFile(t, path, "#!/bin/sh\nexit 3\n", 0o755) },
		"not a program":  func(t *testing.T, path string) { writeFile(t, path, "garbage", 0o755) },
		"a directory":    func(t *testing.T, path string) { _ = os.MkdirAll(path, 0o755) },
	}
	for name, make := range cases {
		t.Run(name, func(t *testing.T) {
			exe, data := setup(t, "v1.0.0", "")
			if err := state.WriteCurrent(data, "v1.0.1"); err != nil {
				t.Fatal(err)
			}
			make(t, state.BinaryPath(data, "v1.0.1"))
			o := serve(t, exe, data)
			if o.ran != "v1.0.0" || o.launched != "" {
				t.Fatalf("%+v", o)
			}
			if lines := strings.Split(strings.TrimSpace(o.stderr), "\n"); len(lines) != 1 || lines[0] == "" {
				t.Fatalf("log lines: %q", o.stderr)
			}
		})
	}
}

func writeFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestARejectedVersionIsNeverPicked(t *testing.T) {
	exe, data := setup(t, "v1.0.0", "v1.0.1")
	if err := state.Reject(data, "v1.0.1"); err != nil {
		t.Fatal(err)
	}
	if o := serve(t, exe, data); o.ran != "v1.0.0" || o.launched != "" {
		t.Fatalf("%+v", o)
	}
}

func TestUseBundledBinaryForcesTheExecutablesOwn(t *testing.T) {
	exe, data := setup(t, "v1.0.0", "v1.0.1")
	if o := serve(t, exe, data, EnvUseBundled+"=1"); o.ran != "v1.0.0" || o.launched != "" {
		t.Fatalf("%+v", o)
	}
	if o := serve(t, exe, data, EnvUseBundled+"=0"); o.ran != "v1.0.1" {
		t.Fatalf("=0 is not forcing: %+v", o)
	}
}

func TestAProcessStartedFromTheDataDirectoryDoesNotLookAgain(t *testing.T) {
	// The data directory's binary is v1.0.1 and names v1.0.9 as current:
	// started from the data directory it runs as it is.
	_, data := setup(t, "v1.0.0", "")
	install(t, data, "v1.0.1", "v1.0.1")
	install(t, data, "v1.0.9", "v1.0.9")
	if err := state.WriteCurrent(data, "v1.0.9"); err != nil {
		t.Fatal(err)
	}
	o := serve(t, state.BinaryPath(data, "v1.0.1"), data, EnvLaunched+"=1", EnvBundledVersion+"=v1.0.0")
	if o.ran != "v1.0.1" {
		t.Fatalf("%+v", o)
	}
}

func TestTheVariablesAnOuterProcessSetAreReplacedNotRepeated(t *testing.T) {
	exe, data := setup(t, "v1.0.0", "v1.0.1")
	o := serve(t, exe, data, EnvBundledVersion+"=v0.0.1", EnvBundledExe+"=/elsewhere")
	if o.bundledVersion != "v1.0.0" || o.bundledExe == "/elsewhere" {
		t.Fatalf("%+v", o)
	}
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestCleanupRemovesOnlyOldBinariesOfLowerVersionsAndNeverPrevious(t *testing.T) {
	data := t.TempDir()
	now := time.Now()
	old := now.Add(-8 * 24 * time.Hour)
	recent := now.Add(-6 * 24 * time.Hour)
	files := map[string]time.Time{
		"kipp-v1.0.0": old,    // lower and old: removed
		"kipp-v1.0.1": old,    // previous: kept
		"kipp-v1.0.2": recent, // lower but recent: kept
		"kipp-v1.1.0": old,    // the running one: kept
		"kipp-v2.0.0": old,    // higher: kept
		"kipp-v0.9.0": old,    // lower and old: removed
		"kipp-v0.8.0": old,    // a rollback's target in pending: kept
		"notes.txt":   old,    // not a binary: kept
		"kipp-dev":    old,    // not a release: kept
	}
	for name, mt := range files {
		p := filepath.Join(state.BinDir(data), name)
		writeFile(t, p, "x", 0o755)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	if err := state.WritePrevious(data, "v1.0.1"); err != nil {
		t.Fatal(err)
	}
	if err := state.WritePending(data, state.Pending{Kind: state.KindRollback, Version: "v0.8.0"}); err != nil {
		t.Fatal(err)
	}
	cleanup(discard(), data, "v1.1.0", now)
	for name := range files {
		_, err := os.Stat(filepath.Join(state.BinDir(data), name))
		gone := name == "kipp-v1.0.0" || name == "kipp-v0.9.0"
		if gone != (err != nil) {
			t.Errorf("%s: removed=%v, want %v", name, err != nil, gone)
		}
	}
	for _, f := range []string{"previous", "pending"} {
		if _, err := os.Stat(filepath.Join(state.BinDir(data), f)); err != nil {
			t.Errorf("%s was touched: %v", f, err)
		}
	}
}

func TestCleanupKeepsAnOldBinaryWhenPreviousIsBundled(t *testing.T) {
	data := t.TempDir()
	now := time.Now()
	p := state.BinaryPath(data, "v1.0.0")
	writeFile(t, p, "x", 0o755)
	old := now.Add(-30 * 24 * time.Hour)
	_ = os.Chtimes(p, old, old)
	if err := state.WritePrevious(data, state.Bundled); err != nil {
		t.Fatal(err)
	}
	cleanup(discard(), data, "v1.1.0", now)
	if _, err := os.Stat(p); err == nil {
		t.Fatal("an old binary that nothing names stays")
	}
}

func TestCleanupRunsAtStartInTheProcessThatStays(t *testing.T) {
	exe, data := setup(t, "v1.0.5", "v1.0.1")
	old := time.Now().Add(-9 * 24 * time.Hour)
	if err := os.Chtimes(state.BinaryPath(data, "v1.0.1"), old, old); err != nil {
		t.Fatal(err)
	}
	if o := serve(t, exe, data); o.ran != "v1.0.5" {
		t.Fatalf("%+v", o)
	}
	if _, err := os.Stat(state.BinaryPath(data, "v1.0.1")); err == nil {
		t.Fatal("the old lower binary stays")
	}
}

// These run the launcher in the test's own process, in the cases where it
// must not exec, so that nothing is replaced.
func TestSwitchToReturnsWithoutReplacingTheProcessWhenItShouldNot(t *testing.T) {
	data := t.TempDir()
	var log bytes.Buffer
	l := slog.New(slog.NewTextHandler(&log, nil))
	switchTo(l, data, "v1.0.0", nil)
	if log.Len() != 0 {
		t.Fatalf("no current is not worth a line: %s", log.String())
	}
	if err := state.WriteCurrent(data, "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	switchTo(l, data, "v1.0.0", nil)
	if err := state.WriteCurrent(data, "v2.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := state.Reject(data, "v2.0.0"); err != nil {
		t.Fatal(err)
	}
	switchTo(l, data, "v1.0.0", nil)
	if log.Len() != 0 {
		t.Fatalf("equal and rejected are silent: %s", log.String())
	}
	if err := os.Remove(filepath.Join(state.BinDir(data), "rejected")); err != nil {
		t.Fatal(err)
	}
	switchTo(l, data, "v1.0.0", nil)
	if strings.Count(log.String(), "\n") != 1 {
		t.Fatalf("a missing binary is one line: %q", log.String())
	}
}

func TestRunIgnoresADevBuildAndTheForcedOwnBinary(t *testing.T) {
	data := t.TempDir()
	if err := state.WriteCurrent(data, "v9.0.0"); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	l := slog.New(slog.NewTextHandler(&log, nil))
	Run(l, data, "dev", nil)
	t.Setenv(EnvUseBundled, "1")
	Run(l, data, "v1.0.0", nil)
	t.Setenv(EnvUseBundled, "")
	t.Setenv(EnvLaunched, "1")
	Run(l, data, "v1.0.0", nil)
	if log.Len() != 0 {
		t.Fatal(log.String())
	}
}
