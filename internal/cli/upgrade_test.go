package cli

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/update"
	"github.com/ndthuan/kipp/internal/upgrade/backup"
	et "github.com/ndthuan/kipp/internal/upgrade/engine/enginetest"
	"github.com/ndthuan/kipp/internal/upgrade/state"
	"github.com/ndthuan/kipp/internal/version"
)

const (
	busyJob  = "name: busy\nstages:\n  s: {run: 'sleep 1'}\n"
	slowJob4 = "name: slow\nstages:\n  s: {run: 'sleep 20'}\n"
)

// upgradeFixture points the servers of a test at a fake GitHub release, signed
// by a key of its own, and has a restart end the process the way an exec
// does: the test starts the next server itself.
type upgradeFixture struct {
	rel    *et.Release
	execed chan []string
}

func fakeUpgrade(t *testing.T) *upgradeFixture {
	t.Helper()
	fx := &upgradeFixture{rel: et.NewRelease(t, et.GoodBinary(et.Newer)), execed: make(chan []string, 4)}
	u, _ := url.Parse(fx.rel.Srv.URL)
	realOptions, realVersion, realKey, realExec := updateOptions, version.Version, releaseKey, execve
	realWait, realPoll, realAway := upgradeWaitPoll, upgradePoll, upgradeAway
	t.Cleanup(func() {
		updateOptions, version.Version, releaseKey, execve = realOptions, realVersion, realKey, realExec
		upgradeWaitPoll, upgradePoll, upgradeAway = realWait, realPoll, realAway
	})
	upgradeAway = 20 * time.Second
	version.Version = et.Running
	upgradeWaitPoll, upgradePoll = 20*time.Millisecond, 20*time.Millisecond
	updateOptions = func(current, dir string, log *slog.Logger) update.Options {
		return update.Options{Current: current, Dir: dir, Base: fx.rel.Srv.URL, DownloadBase: fx.rel.Srv.URL,
			Hosts: []string{u.Hostname()}, Insecure: true, Log: log}
	}
	releaseKey = func() []byte { return fx.rel.Pub }
	execve = func(_ string, argv, _ []string) error {
		fx.execed <- argv
		return errors.New("exec is stubbed in this test")
	}
	return fx
}

// freeAddr is an address a server can be started on again after it ends.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}

// serving starts a server on the fixture with the given jobs.
func (fx *upgradeFixture) serving(t *testing.T, jobs map[string]string) *served {
	t.Helper()
	s := &served{t: t, stderr: &syncBuffer{}, args: []string{"--listen", freeAddr(t)}}
	s.data, s.jobs, s.token = dataAndJobs(t, jobs)
	s.serve()
	eventually(t, "the server knows the release", func() bool { return serverJSON(t, s)["latest"] == et.Newer })
	return s
}

type cliResult struct {
	code     int
	out, err string
}

// inBackground runs a command against s and returns what it will print.
func (s *served) inBackground(args ...string) <-chan cliResult {
	done := make(chan cliResult, 1)
	ended := make(chan struct{})
	go func() {
		code, out, errOut := s.kipp(args...)
		done <- cliResult{code, out, errOut}
		close(ended)
	}()
	// The globals the fixture restores are not to change under a command.
	s.t.Cleanup(func() {
		select {
		case <-ended:
		case <-time.After(40 * time.Second):
		}
	})
	return done
}

func result(t *testing.T, c <-chan cliResult) cliResult {
	t.Helper()
	select {
	case r := <-c:
		return r
	case <-time.After(30 * time.Second):
		t.Fatal("the command did not end")
		return cliResult{}
	}
}

func (fx *upgradeFixture) restarted(t *testing.T) []string {
	t.Helper()
	select {
	case argv := <-fx.execed:
		return argv
	case <-time.After(30 * time.Second):
		t.Fatal("the server did not restart")
		return nil
	}
}

// launcherDone is what the launcher does between the two servers: it takes
// the record, and the next server is the version it names.
func launcherDone(t *testing.T, s *served, next string) *served {
	t.Helper()
	must(t, state.ClearPending(s.data))
	version.Version = next
	s2 := &served{t: t, stderr: &syncBuffer{}, args: s.args, data: s.data, jobs: s.jobs, token: s.token}
	s2.serve()
	return s2
}

func TestAnUpgradeFollowsEveryStepWaitsForTheBuildAndTheNewServerSaysItIsDone(t *testing.T) {
	fx := fakeUpgrade(t)
	s := fx.serving(t, map[string]string{"busy.yml": busyJob})
	ok(t, "", "run", "busy", "--url", s.url, "--token", s.token)
	cmd := s.inBackground("upgrade")

	argv := fx.restarted(t)
	if len(argv) < 2 || argv[1] != "serve" || !strings.Contains(strings.Join(argv, " "), "--data "+s.data) {
		t.Fatalf("the restart was %v", argv)
	}
	eq(t, "the stopped server's code", s.stop(), 1)
	contains(t, s.stderr.String(), "shutting down to start the other version")
	p, err := state.ReadPending(s.data)
	must(t, err)
	if p == nil || p.Kind != state.KindUpgrade || p.Version != et.Newer || p.From != et.Running || p.Backup == "" {
		t.Fatalf("pending %+v", p)
	}
	if fi, err := os.Stat(state.BinaryPath(s.data, et.Newer)); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("the binary: %v, %v", fi, err)
	}
	if _, err := os.Stat(filepath.Join(backup.Dir(s.data), p.Backup, backup.DBFile)); err != nil {
		t.Fatalf("the backup: %v", err)
	}

	s2 := launcherDone(t, s, et.Newer)
	r := result(t, cmd)
	eq(t, "exit", r.code, 0)
	contains(t, r.out, "upgrade: preflight", "upgrade: waiting", "1 running", "upgrade: backup", "upgrade: done", "kipp "+et.Newer+" is running")
	// Quiet ended with the process that held it.
	_, out, _ := s2.kipp("quiet", "status")
	contains(t, out, "quiet off")
	_, out, _ = s2.kipp("upgrade", "status")
	contains(t, out, "upgrade: done ("+et.Running+" to "+et.Newer+")")
}

func TestAServerUnderTheLauncherEndsWithTheCodeThatHasItReadTheRecords(t *testing.T) {
	fx := fakeUpgrade(t)
	t.Setenv("KIPP_SUPERVISED", "1")
	s := fx.serving(t, map[string]string{"busy.yml": busyJob})
	upgradeAway = 100 * time.Millisecond
	s.inBackground("upgrade")
	eventually(t, "the server stops", func() bool { return strings.Contains(s.stderr.String(), "shutting down to start the other version") })
	eq(t, "the exit code", s.stop(), 75)
	select {
	case argv := <-fx.execed:
		t.Fatalf("a supervised server became %v itself", argv)
	default:
	}
}

func TestUpgradeCheckAndStatusSayWhatIsKnownAndNothingIsDone(t *testing.T) {
	fx := fakeUpgrade(t)
	s := fx.serving(t, map[string]string{"busy.yml": busyJob})
	_, out, _ := s.kipp("upgrade", "--check")
	contains(t, out, et.Newer+" is available and can be upgraded to")
	_, out, _ = s.kipp("upgrade", "status")
	eq(t, "status", out, "upgrade: idle\n")
	code, _, errOut := s.kipp("upgrade", "--cancel")
	eq(t, "nothing to cancel", code, 1)
	contains(t, errOut, "no upgrade is running")
	eq(t, "downloads", fx.rel.Hits.Load() > 0, true)
	for _, bad := range [][]string{{"--yes"}, {"--check", "--cancel"}, {"--rollback", "--check"}, {"sideways"}, {"status", "status"}, {"--wait-timeout", "0s"}} {
		code, _, _ = s.kipp(append([]string{"upgrade"}, bad...)...)
		eq(t, strings.Join(bad, " "), code, 2)
	}
	code, _, errOut = kipp("", "upgrade", "--check", "--url", s.url, "--token", "kipp_nobody")
	eq(t, "a token nobody holds", code, 3)
	contains(t, errOut, "refused")
}

func TestAViewersTokenCannotUpgrade(t *testing.T) {
	fx := fakeUpgrade(t)
	s := fx.serving(t, map[string]string{"busy.yml": busyJob})
	ok(t, adminPassword+"\n", "user", "add", "bob", "--password-stdin", "--data", s.data)
	out := ok(t, "", "token", "create", "bob", "--label", "viewer", "--data", s.data)
	tok := strings.TrimSpace(out[strings.LastIndex(out, "kipp_"):])
	code, _, errOut := kipp("", "upgrade", "--url", s.url, "--token", tok)
	eq(t, "a viewer", code, 3)
	contains(t, errOut, "refused")
	if _, err := os.Stat(state.BinDir(s.data)); err == nil {
		des, _ := os.ReadDir(state.BinDir(s.data))
		eq(t, "files made", len(des), 0)
	}
}

func TestASecondUpgradeIsRefusedAndACancelLeavesTheServerAsItWasWithQuietOff(t *testing.T) {
	fx := fakeUpgrade(t)
	s := fx.serving(t, map[string]string{"slow.yml": slowJob4})
	ok(t, "", "run", "slow", "--url", s.url, "--token", s.token)
	first := s.inBackground("upgrade")
	eventually(t, "the upgrade waits", func() bool { _, out, _ := s.kipp("upgrade", "status"); return strings.Contains(out, "waiting") })
	_, out, _ := s.kipp("quiet", "status")
	contains(t, out, "quiet on: 1 running")

	code, _, errOut := s.kipp("upgrade")
	eq(t, "a second upgrade", code, 1)
	contains(t, errOut, "already running")
	code, _, errOut = s.kipp("upgrade", "--no-safe", "--yes")
	eq(t, "a second upgrade, unsafe", code, 1)
	contains(t, errOut, "already running")

	code, out, _ = s.kipp("upgrade", "--cancel")
	eq(t, "cancel", code, 0)
	contains(t, out, "upgrade: cancelled")
	r := result(t, first)
	eq(t, "the cancelled command", r.code, 1)
	contains(t, r.out, "upgrade: cancelled")
	_, out, _ = s.kipp("quiet", "status")
	contains(t, out, "quiet off")
	if _, err := os.Stat(state.BinaryPath(s.data, et.Newer)); err == nil {
		t.Fatal("the download was left")
	}
	select {
	case argv := <-fx.execed:
		t.Fatalf("restarted as %v", argv)
	default:
	}
}

func TestTheWaitTimingOutLeavesTheServerRunningWithQuietOff(t *testing.T) {
	fx := fakeUpgrade(t)
	s := fx.serving(t, map[string]string{"slow.yml": slowJob4})
	ok(t, "", "run", "slow", "--url", s.url, "--token", s.token)
	code, out, errOut := s.kipp("upgrade", "--wait-timeout", "300ms")
	eq(t, "a build outlasts the wait", code, 1)
	contains(t, out, "upgrade: waiting")
	contains(t, errOut, "still busy after 300ms", "quiet is off")
	_, out, _ = s.kipp("quiet", "status")
	contains(t, out, "quiet off")
	select {
	case argv := <-fx.execed:
		t.Fatalf("restarted as %v", argv)
	default:
	}
}

func TestWithoutASafeRestartTheBuildsLostMustBeConfirmedAndThenItRestartsAtOnce(t *testing.T) {
	fx := fakeUpgrade(t)
	s := fx.serving(t, map[string]string{"slow.yml": slowJob4})
	ok(t, "", "run", "slow", "--url", s.url, "--token", s.token)
	code, _, errOut := s.kipp("upgrade", "--no-safe")
	eq(t, "unconfirmed", code, 1)
	contains(t, errOut, "1 running builds", "lost", "--yes")
	cmd := s.inBackground("upgrade", "--no-safe", "--yes")
	fx.restarted(t)
	s.stop()
	s2 := launcherDone(t, s, et.Newer)
	r := result(t, cmd)
	eq(t, "exit", r.code, 0)
	lacks(t, r.out, "upgrade: waiting")
	_, out, _ := s2.kipp("quiet", "status")
	contains(t, out, "quiet off")
}

func TestABadSignatureStopsTheUpgradeWithNothingDownloadedKeptAndNothingRestarted(t *testing.T) {
	fx := fakeUpgrade(t)
	_, other, _ := update.GenerateKey()
	fx.rel.Files[update.SigFile] = other.Sign(fx.rel.Files[update.SumsFile], "file:SHA256SUMS")
	s := fx.serving(t, map[string]string{"busy.yml": busyJob})
	code, out, errOut := s.kipp("upgrade")
	eq(t, "exit", code, 1)
	// The signature is refused so soon that the answer to the request may
	// already say failed, and the first status printed is the one it carries.
	if !strings.Contains(out, "upgrade: preflight") {
		contains(t, out, "upgrade: failed")
	}
	contains(t, errOut, "signature")
	des, _ := os.ReadDir(state.BinDir(s.data))
	eq(t, "files in bin/", len(des), 0)
	_, out, _ = s.kipp("quiet", "status")
	contains(t, out, "quiet off")
	select {
	case argv := <-fx.execed:
		t.Fatalf("restarted as %v", argv)
	default:
	}
}

func TestARollbackRestoresTheBackupOfTheLastUpgradeThroughTheSameRestart(t *testing.T) {
	fx := fakeUpgrade(t)
	version.Version = "v1.1.0"
	s := fx.serving(t, map[string]string{"busy.yml": busyJob})
	_, _, errOut := s.kipp("upgrade", "--rollback")
	contains(t, errOut, "nothing to go back to")
	name, err := backup.Take(context.Background(), s.data, et.Running, time.Now())
	must(t, err)
	must(t, backup.MarkSucceeded(s.data, name, time.Now()))
	must(t, state.WritePrevious(s.data, et.Running))

	cmd := s.inBackground("upgrade", "--rollback")
	fx.restarted(t)
	s.stop()
	p, err := state.ReadPending(s.data)
	must(t, err)
	if p == nil || *p != (state.Pending{Kind: state.KindRollback, Version: et.Running, Backup: name, From: "v1.1.0"}) {
		t.Fatalf("pending %+v", p)
	}
	launcherDone(t, s, et.Running)
	r := result(t, cmd)
	eq(t, "exit", r.code, 0)
	contains(t, r.out, "kipp "+et.Running+" is running")
}

func TestAnUpgradeIsNotAvailableToAServerThatDoesNotCheckForReleases(t *testing.T) {
	fx := fakeUpgrade(t)
	s := &served{t: t, stderr: &syncBuffer{}, args: []string{"--no-update-check"}}
	s.data, s.jobs, s.token = dataAndJobs(t, map[string]string{"busy.yml": busyJob})
	s.serve()
	_, out, _ := s.kipp("upgrade", "--check")
	contains(t, out, "no upgrade: the update check is off")
	code, _, errOut := s.kipp("upgrade")
	eq(t, "exit", code, 1)
	contains(t, errOut, "update check is off")
	eq(t, "lookups", fx.rel.Hits.Load(), int32(0))
}

func TestACommandThatLosesTheServerForGoodGivesUpWithAMessage(t *testing.T) {
	fx := fakeUpgrade(t)
	upgradeAway = 100 * time.Millisecond
	s := fx.serving(t, map[string]string{"busy.yml": busyJob})
	cmd := s.inBackground("upgrade")
	fx.restarted(t)
	s.stop()
	r := result(t, cmd)
	eq(t, "exit", r.code, 1)
	contains(t, r.err, "was not done", "after it began to switch")
}

func TestTheCommandSaysInWordsWhatTheServerKnows(t *testing.T) {
	tests := []struct {
		st   upgradeState
		text func(upgradeState) string
		want string
	}{
		{upgradeState{Latest: "v2.0.0"}, upgradeState.checkText, "v2.0.0 is available and can be upgraded to: kipp upgrade"},
		{upgradeState{Latest: "v2.0.0", Unavailable: "it has no signature file"}, upgradeState.checkText, "v2.0.0 is available, but no upgrade: it has no signature file"},
		{upgradeState{Unavailable: "this is the latest release"}, upgradeState.checkText, "no upgrade: this is the latest release"},
		{upgradeState{State: "idle"}, upgradeState.statusText, "upgrade: idle"},
		{upgradeState{State: "waiting", Version: "v2.0.0", Running: 2, Queued: 1}, upgradeState.statusText, "upgrade: waiting (? to v2.0.0): 2 running, 1 queued"},
		{upgradeState{State: "failed", Version: "v2.0.0", From: "v1.0.0", Error: "no room"}, upgradeState.statusText, "upgrade: failed (v1.0.0 to v2.0.0): no room"},
	}
	for _, tc := range tests {
		eq(t, tc.want, tc.text(tc.st), tc.want)
	}
	st := upgradeState{State: "done", LastFailure: &struct {
		Version string `json:"version"`
		Reason  string `json:"reason"`
	}{"v1.9.0", "it did not answer"}}
	eq(t, "a failure the launcher recorded", st.failure(), "v1.9.0 went back to the version before: it did not answer")
	eq(t, "a done upgrade does not repeat it", st.statusText(), "upgrade: done")
	eq(t, "a default", orDefault("", "x"), "x")
	eq(t, "a default kept", orDefault("y", "x"), "y")
}
