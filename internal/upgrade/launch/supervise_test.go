package launch

import (
	"bytes"
	"errors"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/upgrade/backup"
	"github.com/ndthuan/kipp/internal/upgrade/state"
	"github.com/ndthuan/kipp/internal/waitfor"
)

const backupName = "backup-v1.0.0-20261103T101500Z"

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// withBackup leaves the data directory as an upgrade does: the live
// database already changed by the new version, the backup holding the old.
func withBackup(t *testing.T, data string) {
	t.Helper()
	dir := filepath.Join(backup.Dir(data), backupName)
	write(t, filepath.Join(dir, backup.DBFile), "old database")
	write(t, filepath.Join(dir, backup.KeyFile), "old key")
	write(t, filepath.Join(data, backup.DBFile), "new database")
	write(t, filepath.Join(data, backup.DBFile+"-wal"), "log of the new one")
	write(t, filepath.Join(data, backup.KeyFile), "old key")
}

func pend(t *testing.T, data string, p state.Pending) {
	t.Helper()
	if err := state.WritePending(data, p); err != nil {
		t.Fatal(err)
	}
}

type ended struct {
	stdout, stderr string
	code           int
}

// runServe runs "exe serve" to its end.
func runServe(t *testing.T, exe, data string, env []string, args ...string) ended {
	t.Helper()
	cmd := exec.Command(exe, append([]string{"serve", "--data", data}, args...)...)
	cmd.Env = append(os.Environ(), append([]string{"FAKE_DATA=" + data}, env...)...)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	code := 0
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return ended{o.String(), e.String(), code}
}

// The window is long enough for a child to start on a loaded machine; only
// the child that never answers uses all of it.
const fast = EnvTiming + "=window=3s,cap=20s,grace=300ms,poll=10ms,settle=300ms"

// upgradeSetup is a first upgrade: the executable v1.0.0, no bin/ state,
// and a pending record for v1.0.1.
func upgradeSetup(t *testing.T) (exe, data string) {
	t.Helper()
	exe, data = setup(t, "v1.0.0", "")
	install(t, data, "v1.0.1", "v1.0.1")
	withBackup(t, data)
	pend(t, data, state.Pending{Kind: state.KindUpgrade, Version: "v1.0.1", From: "v1.0.0", Backup: backupName})
	return exe, data
}

func TestAChildThatExitsAtOnceIsRolledBackToTheBundledExecutable(t *testing.T) {
	exe, data := upgradeSetup(t)
	e := runServe(t, exe, data, []string{"FAKE_MODE=exit", fast}, "--listen", freeAddr(t))
	if !strings.Contains(e.stdout, "RAN v1.0.0") || strings.Contains(e.stdout, "launched=1") {
		t.Fatalf("the original executable did not run:\n%s%s", e.stdout, e.stderr)
	}
	checkRolledBack(t, data)
	f, _ := state.ReadFailure(data)
	if !strings.Contains(f.Reason, "exited") {
		t.Fatalf("reason %q", f.Reason)
	}
	// Run again: the version that failed is not tried by itself.
	again := runServe(t, exe, data, []string{"FAKE_MODE=exit"})
	if !strings.Contains(again.stdout, "RAN v1.0.0") || strings.Contains(again.stdout, "CHILD") {
		t.Fatalf("the rejected version ran again:\n%s", again.stdout)
	}
}

func checkRolledBack(t *testing.T, data string) {
	t.Helper()
	if got := read(t, filepath.Join(data, backup.DBFile)); got != "old database" {
		t.Fatalf("database %q", got)
	}
	if _, err := os.Stat(filepath.Join(data, backup.DBFile+"-wal")); !os.IsNotExist(err) {
		t.Fatal("the new database's log is still there")
	}
	if p, _ := state.ReadPending(data); p != nil {
		t.Fatalf("pending is still there: %+v", p)
	}
	if cur, _ := state.ReadCurrent(data); cur != "" {
		t.Fatalf("current is %s", cur)
	}
	if rej, _ := state.ReadRejected(data); len(rej) != 1 || rej[0] != "v1.0.1" {
		t.Fatalf("rejected %v", rej)
	}
	f, _ := state.ReadFailure(data)
	if f == nil || f.Version != "v1.0.1" || f.From != "v1.0.0" || f.Reason == "" || f.At.IsZero() {
		t.Fatalf("failure %+v", f)
	}
	if !strings.Contains(f.LogTail, "CHILD v1.0.1") {
		t.Fatalf("log tail %q", f.LogTail)
	}
	if s, _ := state.ReadStatus(data); s == nil || s.State != state.StateFailed || s.Version != "v1.0.1" {
		t.Fatalf("status %+v", s)
	}
}

func TestAChildThatNeverAnswersIsKilledAtTheDeadlineAndRolledBack(t *testing.T) {
	exe, data := upgradeSetup(t)
	start := time.Now()
	e := runServe(t, exe, data, []string{"FAKE_MODE=stubborn", fast}, "--listen", freeAddr(t))
	if !strings.Contains(e.stdout, "RAN v1.0.0") {
		t.Fatalf("not rolled back:\n%s%s", e.stdout, e.stderr)
	}
	if time.Since(start) < 3*time.Second {
		t.Fatal("it did not wait for the deadline")
	}
	checkRolledBack(t, data)
	if f, _ := state.ReadFailure(data); !strings.Contains(f.Reason, "health check") {
		t.Fatalf("reason %q", f.Reason)
	}
}

func TestALauncherStartedFromTheDataDirectoryStillProcessesPending(t *testing.T) {
	exe, data := upgradeSetup(t)
	e := runServe(t, exe, data, []string{"FAKE_MODE=exit", fast, EnvLaunched + "=1",
		EnvBundledExe + "=" + exe, EnvBundledVersion + "=v1.0.0"}, "--listen", freeAddr(t))
	if !strings.Contains(e.stdout, "RAN v1.0.0") {
		t.Fatalf("not rolled back:\n%s%s", e.stdout, e.stderr)
	}
	checkRolledBack(t, data)
}

func TestASupervisedServerDoesNotProcessPending(t *testing.T) {
	exe, data := upgradeSetup(t)
	e := runServe(t, exe, data, []string{"FAKE_MODE=exit", EnvSupervised + "=1"})
	if e.code != 3 || !strings.Contains(e.stdout, "CHILD v1.0.0") {
		t.Fatalf("%+v", e)
	}
	if p, _ := state.ReadPending(data); p == nil {
		t.Fatal("a supervised server took pending")
	}
}

// start runs "exe serve" in the background.
func start(t *testing.T, exe, data string, env []string, args ...string) (*exec.Cmd, *syncBuf) {
	t.Helper()
	cmd := exec.Command(exe, append([]string{"serve", "--data", data}, args...)...)
	cmd.Env = append(os.Environ(), append([]string{"FAKE_DATA=" + data}, env...)...)
	out := &syncBuf{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd, out
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	waitfor.Until(t, what, ok)
}

// watch is a log that closes seen once want has been written to it.
type watch struct {
	mu   sync.Mutex
	b    bytes.Buffer
	want string
	seen chan struct{}
	once sync.Once
}

func newWatch(want string) *watch { return &watch{want: want, seen: make(chan struct{})} }

func (w *watch) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.b.Write(p)
	if strings.Contains(w.b.String(), w.want) {
		w.once.Do(func() { close(w.seen) })
	}
	return n, err
}

func TestAHealthyPendingVersionBecomesCurrentAndSIGTERMReachesIt(t *testing.T) {
	exe, data := upgradeSetup(t)
	cmd, out := start(t, exe, data, []string{"FAKE_MODE=ok", "FAKE_EXIT=7", fast}, "--listen", freeAddr(t))
	waitFor(t, "pending to go", func() bool { p, _ := state.ReadPending(data); return p == nil })
	if cur, _ := state.ReadCurrent(data); cur != "v1.0.1" {
		t.Fatalf("current %q", cur)
	}
	if prev, _ := state.ReadPrevious(data); prev != state.Bundled {
		t.Fatalf("previous %q", prev)
	}
	if _, err := os.Stat(filepath.Join(backup.Dir(data), backupName, "succeeded")); err != nil {
		t.Fatal("the backup is not marked as succeeded:", err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 7 {
		t.Fatalf("the launcher ended with %v, not the child's 7\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "CHILD v1.0.1 supervised") {
		t.Fatalf("the child's output was not passed through:\n%s", out.String())
	}
}

func TestALauncherKilledAfterCurrentWasWrittenButBeforePendingWentRecovers(t *testing.T) {
	exe, data := upgradeSetup(t)
	// The state a launcher left when it died between those two steps.
	if err := state.WriteCurrent(data, "v1.0.1"); err != nil {
		t.Fatal(err)
	}
	if err := state.WritePrevious(data, state.Bundled); err != nil {
		t.Fatal(err)
	}
	cmd, out := start(t, exe, data, []string{"FAKE_MODE=ok", fast}, "--listen", freeAddr(t))
	waitFor(t, "pending to go", func() bool { p, _ := state.ReadPending(data); return p == nil })
	_ = cmd.Process.Signal(syscall.SIGTERM)
	_ = cmd.Wait()
	if cur, _ := state.ReadCurrent(data); cur != "v1.0.1" || !strings.Contains(out.String(), "CHILD v1.0.1") {
		t.Fatalf("current %q\n%s", cur, out.String())
	}
}

func TestAStopWhileWaitingLeavesPendingForTheNextStart(t *testing.T) {
	exe, data := upgradeSetup(t)
	cmd, out := start(t, exe, data, []string{"FAKE_MODE=hang", EnvTiming + "=window=1h,cap=2h,poll=10ms,grace=300ms"},
		"--listen", freeAddr(t))
	waitFor(t, "the child to start", func() bool { return strings.Contains(out.String(), "CHILD v1.0.1") })
	_ = cmd.Process.Signal(syscall.SIGTERM)
	_ = cmd.Wait()
	if p, _ := state.ReadPending(data); p == nil {
		t.Fatal("pending went although the version was never judged")
	}
	if rej, _ := state.ReadRejected(data); len(rej) != 0 {
		t.Fatalf("rejected %v", rej)
	}
	if got := read(t, filepath.Join(data, backup.DBFile)); got != "new database" {
		t.Fatalf("the backup was restored: %q", got)
	}
}

func TestARollbackRecordToBundledRestoresTheBackupAndRemovesCurrent(t *testing.T) {
	exe, data := setup(t, "v1.0.0", "v1.0.1")
	withBackup(t, data)
	pend(t, data, state.Pending{Kind: state.KindRollback, Version: state.Bundled, From: "v1.0.1", Backup: backupName})
	e := runServe(t, exe, data, nil)
	if !strings.Contains(e.stdout, "RAN v1.0.0") || strings.Contains(e.stdout, "launched=1") {
		t.Fatalf("%s%s", e.stdout, e.stderr)
	}
	if got := read(t, filepath.Join(data, backup.DBFile)); got != "old database" {
		t.Fatalf("database %q", got)
	}
	if cur, _ := state.ReadCurrent(data); cur != "" {
		t.Fatalf("current %q", cur)
	}
	if rej, _ := state.ReadRejected(data); len(rej) != 1 || rej[0] != "v1.0.1" {
		t.Fatalf("rejected %v", rej)
	}
	if p, _ := state.ReadPending(data); p != nil {
		t.Fatal("pending is still there")
	}
}

func TestARollbackRecordToAnEarlierVersionRestoresAndStartsIt(t *testing.T) {
	exe, data := setup(t, "v1.0.2", "")
	install(t, data, "v1.0.1", "v1.0.1")
	withBackup(t, data)
	pend(t, data, state.Pending{Kind: state.KindRollback, Version: "v1.0.1", From: "v1.0.2", Backup: backupName})
	e := runServe(t, exe, data, nil)
	if !strings.Contains(e.stdout, "RAN v1.0.1") || !strings.Contains(e.stdout, "launched=1") {
		t.Fatalf("%s%s", e.stdout, e.stderr)
	}
	if got := read(t, filepath.Join(data, backup.DBFile)); got != "old database" {
		t.Fatalf("database %q", got)
	}
	if cur, _ := state.ReadCurrent(data); cur != "v1.0.1" {
		t.Fatalf("current %q", cur)
	}
	if rej, _ := state.ReadRejected(data); len(rej) != 1 || rej[0] != "v1.0.2" {
		t.Fatalf("rejected %v", rej)
	}
}

func TestARollbackRecordWhoseBackupIsMissingIsSetAsideAndTheHigherVersionRuns(t *testing.T) {
	exe, data := setup(t, "v1.0.0", "v1.0.1")
	write(t, filepath.Join(data, backup.DBFile), "live database")
	pend(t, data, state.Pending{Kind: state.KindRollback, Version: state.Bundled, From: "v1.0.1", Backup: backupName})
	e := runServe(t, exe, data, nil)
	if e.code != 0 || !strings.Contains(e.stdout, "RAN v1.0.1") {
		t.Fatalf("%+v", e)
	}
	if !strings.Contains(e.stderr, "refusing the rollback record") {
		t.Fatalf("no log line:\n%s", e.stderr)
	}
	if p, _ := state.ReadPending(data); p != nil {
		t.Fatal("pending is still there")
	}
	if _, err := os.Stat(filepath.Join(state.BinDir(data), "pending.refused")); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(data, backup.DBFile)); got != "live database" {
		t.Fatalf("the database was touched: %q", got)
	}
}

func TestEveryStartCleansTheBackupsThatExpired(t *testing.T) {
	exe, data := setup(t, "v1.0.0", "")
	old := filepath.Join(backup.Dir(data), "backup-v0.9.0-20250101T000000Z")
	write(t, filepath.Join(old, backup.DBFile), "x")
	write(t, filepath.Join(old, "succeeded"), "2025-01-02T00:00:00Z\n")
	runServe(t, exe, data, nil)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("the expired backup is still there:", err)
	}
}

// clock is a fake time: a pause moves it, and a hook sees every step. Before
// each step the pause waits for the real child to have done what its mode
// makes it do, so that the fake time never runs ahead of it.
type clock struct {
	mu   sync.Mutex
	t    time.Time
	step time.Duration
	hook func(elapsed time.Duration)
	t0   time.Time
	// settled waits for the child; nil for a child that does nothing.
	settled func(c *child)
}

func newClock(step time.Duration, hook func(time.Duration)) *clock {
	t0 := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	return &clock{t: t0, t0: t0, step: step, hook: hook}
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func (c *clock) pause(ch *child, _ time.Duration) {
	if c.settled != nil {
		c.settled(ch)
	}
	c.mu.Lock()
	c.t = c.t.Add(c.step)
	el := c.t.Sub(c.t0)
	c.mu.Unlock()
	if c.hook != nil {
		c.hook(el)
	}
}

// inPackage is a launcher on a fake clock that runs the fake child of
// version v1.0.1 for real, on mode.
func inPackage(t *testing.T, mode string, clk *clock) (*launcher, *state.Pending, string) {
	t.Helper()
	_, data := setup(t, "v1.0.0", "")
	install(t, data, "v1.0.1", "v1.0.1")
	withBackup(t, data)
	p := &state.Pending{Kind: state.KindUpgrade, Version: "v1.0.1", From: "v1.0.0", Backup: backupName}
	pend(t, data, *p)
	t.Setenv("FAKE_DATA", data)
	t.Setenv("FAKE_MODE", mode)
	l := newLauncher(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), data, "v1.0.0",
		[]string{"--data", data, "--listen", freeAddr(t)})
	l.t = timing{window: 90 * time.Second, cap: 30 * time.Minute, grace: 300 * time.Millisecond,
		poll: time.Second, settle: 10 * time.Second}
	l.stdout, l.stderr = &bytes.Buffer{}, &bytes.Buffer{}
	l.now, l.pause = clk.now, clk.pause
	// What each mode of the fake child comes to, for the pause to wait for.
	switch mode {
	case "exit":
		clk.settled = func(c *child) { waitDone(t, c) }
	case "ok", "migrate":
		listening := newWatch("LISTENING")
		l.stdout = listening
		clk.settled = func(c *child) {
			// A migrating child has nothing to wait for until it is let go.
			if mode == "migrate" && !fileExists(filepath.Join(data, "fake-proceed")) {
				return
			}
			select {
			case <-listening.seen:
			case <-c.done:
			case <-time.After(waitfor.Safety):
				t.Error("the child never listened")
			}
		}
	}
	return l, p, data
}

func waitDone(t *testing.T, c *child) {
	t.Helper()
	select {
	case <-c.done:
	case <-time.After(waitfor.Safety):
		t.Error("the child never ended")
	}
}

func waitMigrating(t *testing.T, data string) {
	t.Helper()
	waitFor(t, "the migration to start", func() bool { return fileExists(state.MigratingPath(data)) })
}

func TestAMigrationLongerThanTheWindowThatFinishesIsNotRolledBack(t *testing.T) {
	var data string
	clk := newClock(10*time.Second, func(el time.Duration) {
		if el == 10*time.Second {
			waitMigrating(t, data)
		}
		if el >= 200*time.Second { // past 90 seconds of migrating
			write(t, filepath.Join(data, "fake-proceed"), "")
		}
	})
	l, p, d := inPackage(t, "migrate", clk)
	data = d
	done := make(chan result, 1)
	go func() { done <- l.upgrade(p) }()
	waitFor(t, "current to be written", func() bool { c, _ := state.ReadCurrent(data); return c == "v1.0.1" })
	if got := read(t, filepath.Join(data, backup.DBFile)); got != "new database" {
		t.Fatalf("restored: %q", got)
	}
	write(t, filepath.Join(data, "fake-restart"), "")
	res := <-done
	if !res.restart {
		t.Fatalf("%+v", res)
	}
}

func TestTheCapOf30MinutesRollsBackAMigrationThatNeverEnds(t *testing.T) {
	var data string
	clk := newClock(30*time.Second, func(el time.Duration) {
		if el == 30*time.Second {
			waitMigrating(t, data)
		}
	})
	l, p, d := inPackage(t, "migrate", clk)
	data = d
	res := l.upgrade(p)
	if res.target != state.Bundled || res.ended {
		t.Fatalf("%+v", res)
	}
	f, _ := state.ReadFailure(data)
	if f == nil || !strings.Contains(f.Reason, "30m0s") {
		t.Fatalf("failure %+v", f)
	}
	if got := read(t, filepath.Join(data, backup.DBFile)); got != "old database" {
		t.Fatalf("database %q", got)
	}
	if clk.now().Sub(clk.t0) < 30*time.Minute {
		t.Fatal("rolled back before the cap")
	}
}

func TestWithoutAMigrationTheWindowIsNinetySecondsFromTheStart(t *testing.T) {
	clk := newClock(10*time.Second, nil)
	l, p, data := inPackage(t, "hang", clk)
	res := l.upgrade(p)
	if res.target != state.Bundled {
		t.Fatalf("%+v", res)
	}
	if el := clk.now().Sub(clk.t0); el < 90*time.Second || el > 110*time.Second {
		t.Fatalf("rolled back after %s", el)
	}
	if f, _ := state.ReadFailure(data); f == nil || !strings.Contains(f.Reason, "1m30s") {
		t.Fatalf("failure %+v", f)
	}
}

func TestAPendingVersionWhoseBinaryIsMissingIsRolledBackWithoutAChild(t *testing.T) {
	l, p, data := inPackage(t, "ok", newClock(time.Second, nil))
	if err := os.Remove(state.BinaryPath(data, "v1.0.1")); err != nil {
		t.Fatal(err)
	}
	res := l.upgrade(p)
	if res.target != state.Bundled {
		t.Fatalf("%+v", res)
	}
	if got := read(t, filepath.Join(data, backup.DBFile)); got != "old database" {
		t.Fatalf("database %q", got)
	}
}

func TestAnUpgradeFromADataDirectoryBinaryReturnsToThatBinary(t *testing.T) {
	l, p, data := inPackage(t, "exit", newClock(time.Second, nil))
	p.From = "v1.0.0"
	t.Setenv(EnvLaunched, "1")
	t.Setenv(EnvBundledVersion, "v0.9.0")
	t.Setenv(EnvBundledExe, "/nonexistent/kipp")
	res := l.upgrade(p)
	if res.target != "v1.0.0" {
		t.Fatalf("%+v", res)
	}
	if cur, _ := state.ReadCurrent(data); cur != "v1.0.0" {
		t.Fatalf("current %q", cur)
	}
}

func TestHealthAddrReadsListenFromTheArguments(t *testing.T) {
	for _, c := range []struct {
		args []string
		addr string
		ok   bool
	}{
		{nil, "127.0.0.1:8085", true},
		{[]string{"--listen", ":9000"}, "127.0.0.1:9000", true},
		{[]string{"-listen=0.0.0.0:9001"}, "127.0.0.1:9001", true},
		{[]string{"--listen=[::]:9002"}, "[::1]:9002", true},
		{[]string{"--listen", "10.1.2.3:9003"}, "10.1.2.3:9003", true},
		{[]string{"--listen", "127.0.0.1:0"}, "", false},
		{[]string{"--data", "--listen"}, "127.0.0.1:8085", true},
		{[]string{"--", "--listen=:1"}, "127.0.0.1:8085", true},
	} {
		if addr, ok := healthAddr(c.args); addr != c.addr || ok != c.ok {
			t.Errorf("%v: %q %v", c.args, addr, ok)
		}
	}
}

func TestTimingComesFromTheEnvironmentAndIgnoresJunk(t *testing.T) {
	t.Setenv(EnvTiming, "window=5s,cap=1m,bogus=1s,poll=x,grace=-1s")
	got := timingFromEnv(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	want := defaultTiming()
	want.window, want.cap = 5*time.Second, time.Minute
	if got != want {
		t.Fatalf("%+v", got)
	}
}

func TestAnAddressThatIsNotKnownIsJudgedByTheProcessStayingUp(t *testing.T) {
	clk := newClock(time.Second, nil)
	l, p, data := inPackage(t, "ok", clk)
	l.args = []string{"--data", data, "--listen", "127.0.0.1:0"}
	done := make(chan result, 1)
	go func() { done <- l.upgrade(p) }()
	waitFor(t, "current", func() bool { c, _ := state.ReadCurrent(data); return c == "v1.0.1" })
	write(t, filepath.Join(data, "fake-restart"), "")
	if res := <-done; !res.restart {
		t.Fatalf("%+v", res)
	}
}

type execCall struct {
	path string
	argv []string
	env  []string
}

// recorded is a launcher whose exit and exec are noted, not done.
func recorded(l *launcher) (calls *[]execCall, exits *[]int) {
	calls, exits = &[]execCall{}, &[]int{}
	l.exec = func(path string, argv, env []string) error {
		*calls = append(*calls, execCall{path, argv, env})
		return nil
	}
	l.exit = func(c int) { *exits = append(*exits, c) }
	return calls, exits
}

func hasEnv(env []string, kv string) bool { return slices.Contains(env, kv) }

func TestAnUnhealthyPendingVersionHandsOverToTheBundledExecutable(t *testing.T) {
	l, _, data := inPackage(t, "exit", newClock(time.Second, nil))
	calls, exits := recorded(l)
	l.processPending()
	if len(*exits) != 0 || len(*calls) != 1 {
		t.Fatalf("exits %v calls %v", *exits, *calls)
	}
	exe, _ := os.Executable()
	c := (*calls)[0]
	if c.path != exe || len(c.argv) < 2 || c.argv[1] != "serve" || hasEnv(c.env, EnvLaunched+"=1") {
		t.Fatalf("%+v", c)
	}
	if got := read(t, filepath.Join(data, backup.DBFile)); got != "old database" {
		t.Fatalf("database %q", got)
	}
}

func TestARolledBackDataDirectoryVersionIsStartedMarkedAsLaunched(t *testing.T) {
	l, _, data := inPackage(t, "exit", newClock(time.Second, nil))
	install(t, data, "v1.0.0", "v1.0.0")
	pend(t, data, state.Pending{Kind: state.KindUpgrade, Version: "v1.0.1", From: "v1.0.0", Backup: backupName})
	t.Setenv(EnvLaunched, "1")
	t.Setenv(EnvBundledVersion, "v0.9.0")
	t.Setenv(EnvBundledExe, "/opt/kipp")
	calls, _ := recorded(l)
	l.processPending()
	c := (*calls)[0]
	if c.path != state.BinaryPath(data, "v1.0.0") || !hasEnv(c.env, EnvLaunched+"=1") ||
		!hasEnv(c.env, EnvBundledExe+"=/opt/kipp") || !hasEnv(c.env, EnvBundledVersion+"=v0.9.0") {
		t.Fatalf("%+v", c)
	}
}

func TestAHealthyChildEndsTheLauncherWithTheChildsCode(t *testing.T) {
	l, _, data := inPackage(t, "ok", newClock(time.Second, nil))
	t.Setenv("FAKE_EXIT", "9")
	_, exits := recorded(l)
	done := make(chan struct{})
	go func() { l.processPending(); close(done) }()
	waitFor(t, "current", func() bool { c, _ := state.ReadCurrent(data); return c == "v1.0.1" })
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	<-done
	if len(*exits) != 1 || (*exits)[0] != 9 {
		t.Fatalf("exits %v", *exits)
	}
}

func TestARestartCodeMakesTheLauncherReadTheNextRecord(t *testing.T) {
	l, _, data := inPackage(t, "ok", newClock(time.Second, nil))
	calls, exits := recorded(l)
	done := make(chan struct{})
	go func() { l.processPending(); close(done) }()
	// Removing pending is becomeCurrent's last step; a record written before
	// it would be removed with it.
	waitFor(t, "pending to be settled", func() bool { p, err := state.ReadPending(data); return err == nil && p == nil })
	// The server writes a rollback record and exits with 75.
	pend(t, data, state.Pending{Kind: state.KindRollback, Version: state.Bundled, From: "v1.0.1", Backup: backupName})
	write(t, filepath.Join(data, "fake-restart"), "")
	<-done
	if len(*exits) != 0 || len(*calls) != 1 {
		t.Fatalf("exits %v calls %v", *exits, *calls)
	}
	if cur, _ := state.ReadCurrent(data); cur != "" {
		t.Fatalf("current %q after a rollback to bundled", cur)
	}
}

func TestARollbackRecordWithoutABackupNameIsRefused(t *testing.T) {
	l, _, data := inPackage(t, "ok", newClock(time.Second, nil))
	pend(t, data, state.Pending{Kind: state.KindRollback, Version: state.Bundled, From: "v1.0.1"})
	calls, _ := recorded(l)
	l.processPending()
	if len(*calls) != 0 || fileExists(filepath.Join(state.BinDir(data), "pending")) {
		t.Fatalf("calls %v", *calls)
	}
	if !fileExists(filepath.Join(state.BinDir(data), "pending.refused")) {
		t.Fatal("not set aside")
	}
}

func TestARollbackToAnEarlierVersionExecsItAndForgetsItsRejection(t *testing.T) {
	l, _, data := inPackage(t, "ok", newClock(time.Second, nil))
	if err := state.Reject(data, "v1.0.1"); err != nil {
		t.Fatal(err)
	}
	pend(t, data, state.Pending{Kind: state.KindRollback, Version: "v1.0.1", From: "v1.0.2", Backup: backupName})
	calls, _ := recorded(l)
	l.processPending()
	if len(*calls) != 1 || (*calls)[0].path != state.BinaryPath(data, "v1.0.1") {
		t.Fatalf("%v", *calls)
	}
	if rej, _ := state.ReadRejected(data); len(rej) != 1 || rej[0] != "v1.0.2" {
		t.Fatalf("rejected %v", rej)
	}
}

func TestAnExecThatFailsLeavesTheLauncherToCarryOn(t *testing.T) {
	l, _, _ := inPackage(t, "exit", newClock(time.Second, nil))
	l.exec = func(string, []string, []string) error { return errors.New("no such file") }
	l.exit = func(int) { t.Fatal("exited") }
	l.processPending()
}

func TestExitCodeIsTheChildsOr128PlusTheSignal(t *testing.T) {
	if exitCode(nil) != 0 || exitCode(errors.New("x")) != 1 {
		t.Fatal("plain codes")
	}
	err := exec.Command("sh", "-c", "exit 4").Run()
	if exitCode(err) != 4 {
		t.Fatal(exitCode(err))
	}
	cmd := exec.Command("sleep", "30")
	_ = cmd.Start()
	_ = cmd.Process.Kill()
	err = cmd.Wait()
	if exitCode(err) != 128+int(syscall.SIGKILL) || !strings.Contains(describeExit(err), "signal 9") {
		t.Fatal(exitCode(err), describeExit(err))
	}
}
