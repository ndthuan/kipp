package engine_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/update"
	"github.com/ndthuan/kipp/internal/upgrade/backup"
	"github.com/ndthuan/kipp/internal/upgrade/engine"
	et "github.com/ndthuan/kipp/internal/upgrade/engine/enginetest"
	"github.com/ndthuan/kipp/internal/upgrade/launch"
	"github.com/ndthuan/kipp/internal/upgrade/state"
)

type rig struct {
	e    *engine.Engine
	f    *et.Server
	data string
	rel  *et.Release
}

func newRig(t *testing.T, rel *et.Release, tweak func(*engine.Options)) *rig {
	t.Helper()
	f := et.NewServer()
	e, data := et.Engine(t, rel, f, tweak)
	return &rig{e: e, f: f, data: data, rel: rel}
}

// settle waits for the run to end one way or the other and returns the
// info then.
func (r *rig) settle(t *testing.T) engine.Info {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		in, err := r.e.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		switch in.Status.State {
		case state.StateSwitching, state.StateFailed, state.StateCancelled, state.StateDone:
			return in
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the upgrade did not end")
	return engine.Info{}
}

func (r *rig) binEntries(t *testing.T) []string {
	t.Helper()
	des, _ := os.ReadDir(state.BinDir(r.data))
	var out []string
	for _, de := range des {
		out = append(out, de.Name())
	}
	return out
}

func start(t *testing.T, r *rig, req engine.Request) engine.Info {
	t.Helper()
	in, err := r.e.Start(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

// untilWaiting waits until the upgrade waits for n builds.
func untilWaiting(t *testing.T, r *rig, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		in, _ := r.e.Info(context.Background())
		if in.Status.State == state.StateWaiting && in.Status.Running == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("never waiting for %d builds: %+v", n, in.Status)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestASafeUpgradeGoesThroughTheStepsWithQuietOnWhileItWaitsAndHandsOver(t *testing.T) {
	r := newRig(t, et.NewRelease(t, et.GoodBinary(et.Newer)), nil)
	r.f.SetBusy(2)
	in := start(t, r, engine.Request{Safe: true})
	if in.Status.State != state.StatePreflight && in.Status.State != state.StateDownloading {
		t.Fatalf("the first status is %+v", in.Status)
	}
	// It waits, quiet, while two builds run, and says how many.
	untilWaiting(t, r, 2)
	if got := r.f.QuietCalls(); len(got) != 1 || !got[0] {
		t.Fatalf("quiet was not turned on for the wait: %v", got)
	}
	if got := r.f.Switched.Load(); got != 0 {
		t.Fatal("switched while builds ran")
	}
	r.f.SetBusy(0)
	in = r.settle(t)
	if in.Status.State != state.StateSwitching || in.Status.Version != et.Newer || in.Status.From != et.Running || in.Status.Backup == "" {
		t.Fatalf("%+v", in.Status)
	}
	checkHandedOver(t, r, in)
	if _, err := r.e.Start(context.Background(), engine.Request{Safe: true}); !errors.Is(err, engine.ErrConflict) {
		t.Fatalf("a second upgrade: %v", err)
	}
	// The new server, started by the launcher, says it is done.
	engine.New(engine.Options{Data: r.data, Version: et.Newer}).Resume()
	if st, _ := state.ReadStatus(r.data); st.State != state.StateDone || st.Backup == "" {
		t.Fatalf("after the start: %+v", st)
	}
}

// checkHandedOver holds what a finished upgrade leaves for the launcher.
func checkHandedOver(t *testing.T, r *rig, in engine.Info) {
	t.Helper()
	p, err := state.ReadPending(r.data)
	if err != nil || p == nil || *p != (state.Pending{Kind: state.KindUpgrade, Version: et.Newer, Backup: in.Status.Backup, From: et.Running}) {
		t.Fatalf("pending %+v, %v", p, err)
	}
	fi, err := os.Stat(state.BinaryPath(r.data, et.Newer))
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("binary %v, %v", fi, err)
	}
	if got := r.binEntries(t); len(got) != 2 { // the binary and the pending record
		t.Fatalf("bin/ holds %v", got)
	}
	if _, err := os.Stat(filepath.Join(backup.Dir(r.data), in.Status.Backup, backup.DBFile)); err != nil {
		t.Fatalf("the backup: %v", err)
	}
	if got := r.f.QuietCalls(); len(got) != 1 || !got[0] {
		t.Fatalf("quiet calls %v: on once, and left on for the restart to end", got)
	}
}

func TestABadSignatureStopsAtTheDownloadWithNothingInBinAndNothingRestarted(t *testing.T) {
	rel := et.NewRelease(t, et.GoodBinary(et.Newer))
	_, other, _ := update.GenerateKey()
	rel.Files[update.SigFile] = other.Sign(rel.Files[update.SumsFile], "file:SHA256SUMS")
	r := newRig(t, rel, nil)
	start(t, r, engine.Request{Safe: true})
	in := r.settle(t)
	if in.Status.State != state.StateFailed || !strings.Contains(in.Error, "signature") {
		t.Fatalf("%+v %q", in.Status, in.Error)
	}
	if got := r.binEntries(t); len(got) != 0 {
		t.Fatalf("bin/ holds %v", got)
	}
	if len(r.f.QuietCalls()) != 0 || r.f.Switched.Load() != 0 {
		t.Fatal("something was restarted or made quiet")
	}
	if p, _ := state.ReadPending(r.data); p != nil {
		t.Fatal("a pending record")
	}
}

func TestABinaryThatDoesNotRunIsNotLeftInBin(t *testing.T) {
	r := newRig(t, et.NewRelease(t, "#!/bin/sh\nexit 3\n"), nil)
	start(t, r, engine.Request{Safe: true})
	in := r.settle(t)
	if in.Status.State != state.StateFailed || !strings.Contains(in.Error, "does not run") {
		t.Fatalf("%+v %q", in.Status, in.Error)
	}
	if got := r.binEntries(t); len(got) != 0 {
		t.Fatalf("bin/ holds %v", got)
	}
}

func TestCancellingTheWaitTurnsQuietOffRemovesTheDownloadAndRestartsNothing(t *testing.T) {
	r := newRig(t, et.NewRelease(t, et.GoodBinary(et.Newer)), nil)
	r.f.SetBusy(1)
	start(t, r, engine.Request{Safe: true})
	for {
		in, _ := r.e.Info(context.Background())
		if in.Status.State == state.StateWaiting {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	in, err := r.e.Cancel(context.Background())
	if err != nil || in.Status.State != state.StateCancelled {
		t.Fatalf("%+v, %v", in.Status, err)
	}
	if got := r.f.QuietCalls(); len(got) != 2 || !got[0] || got[1] {
		t.Fatalf("quiet calls %v", got)
	}
	if got := r.binEntries(t); len(got) != 0 || r.f.Switched.Load() != 0 {
		t.Fatalf("bin/ holds %v", got)
	}
	if _, err := r.e.Cancel(context.Background()); !errors.Is(err, engine.ErrConflict) {
		t.Fatalf("cancelling nothing: %v", err)
	}
	// And the server can be upgraded again.
	r.f.SetBusy(0)
	start(t, r, engine.Request{Safe: true})
	if in := r.settle(t); in.Status.State != state.StateSwitching {
		t.Fatalf("%+v", in.Status)
	}
}

func TestACancelAfterTheBackupBeganIsRefused(t *testing.T) {
	r := newRig(t, et.NewRelease(t, et.GoodBinary(et.Newer)), nil)
	start(t, r, engine.Request{Safe: true})
	r.settle(t)
	if _, err := r.e.Cancel(context.Background()); !errors.Is(err, engine.ErrConflict) || !strings.Contains(err.Error(), "rollback") {
		t.Fatalf("%v", err)
	}
}

func TestTheWaitTimingOutTurnsQuietOffAndRestartsNothing(t *testing.T) {
	r := newRig(t, et.NewRelease(t, et.GoodBinary(et.Newer)), nil)
	r.f.SetBusy(1)
	start(t, r, engine.Request{Safe: true, WaitTimeout: 60 * time.Millisecond})
	in := r.settle(t)
	if in.Status.State != state.StateFailed || !strings.Contains(in.Error, "still busy") || !strings.Contains(in.Error, "quiet is off") {
		t.Fatalf("%+v %q", in.Status, in.Error)
	}
	if got := r.f.QuietCalls(); len(got) != 2 || !got[0] || got[1] {
		t.Fatalf("quiet calls %v", got)
	}
	if r.f.Switched.Load() != 0 || len(r.binEntries(t)) != 0 {
		t.Fatal("something was switched or left")
	}
}

func TestQuietThatWasOnBeforeStaysOnWhenTheUpgradeIsCancelled(t *testing.T) {
	r := newRig(t, et.NewRelease(t, et.GoodBinary(et.Newer)), nil)
	r.f.SetQuietOn(true)
	r.f.SetBusy(1)
	start(t, r, engine.Request{Safe: true})
	for {
		in, _ := r.e.Info(context.Background())
		if in.Status.State == state.StateWaiting {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := r.e.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := r.f.QuietCalls(); len(got) != 1 {
		t.Fatalf("quiet calls %v: an operator's quiet is theirs to end", got)
	}
}

func TestWithoutASafeRestartTheBuildsLostAreConfirmedFirstAndQuietIsNotUsed(t *testing.T) {
	r := newRig(t, et.NewRelease(t, et.GoodBinary(et.Newer)), nil)
	r.f.SetBusy(3)
	_, err := r.e.Start(context.Background(), engine.Request{Safe: false})
	if !errors.Is(err, engine.ErrBadRequest) || !strings.Contains(err.Error(), "3 running builds") || !strings.Contains(err.Error(), "lost") {
		t.Fatalf("%v", err)
	}
	if r.rel.Hits.Load() != 1 { // the lookup of the fixture alone
		t.Fatalf("%d requests: a refused call downloaded something", r.rel.Hits.Load())
	}
	start(t, r, engine.Request{Safe: false, ConfirmLost: true})
	in := r.settle(t)
	if in.Status.State != state.StateSwitching || len(r.f.QuietCalls()) != 0 {
		t.Fatalf("%+v, quiet calls %v", in.Status, r.f.QuietCalls())
	}
}

func TestARequestForAnotherVersionThanTheLatestIsRefused(t *testing.T) {
	r := newRig(t, et.NewRelease(t, et.GoodBinary(et.Newer)), nil)
	_, err := r.e.Start(context.Background(), engine.Request{Safe: true, Version: "v9.9.9"})
	if !errors.Is(err, engine.ErrBadRequest) || !strings.Contains(err.Error(), et.Newer) {
		t.Fatalf("%v", err)
	}
	start(t, r, engine.Request{Safe: true, Version: et.Newer})
	r.settle(t)
}

func TestADevelopmentBuildAnUpToDateServerAndNoCheckerAreRefused(t *testing.T) {
	rel := et.NewRelease(t, et.GoodBinary(et.Newer))
	r := newRig(t, rel, func(o *engine.Options) { o.InstallType = func() string { return update.Dev } })
	_, err := r.e.Start(context.Background(), engine.Request{Safe: true})
	if !errors.Is(err, engine.ErrConflict) || !strings.Contains(err.Error(), "not available in a development build") {
		t.Fatalf("%v", err)
	}
	in, _ := r.e.Info(context.Background())
	if in.Unavailable != "not available in a development build" {
		t.Fatalf("%q", in.Unavailable)
	}
	r = newRig(t, rel, func(o *engine.Options) { o.Checker = nil })
	if _, err := r.e.Start(context.Background(), engine.Request{Safe: true}); !errors.Is(err, engine.ErrConflict) {
		t.Fatalf("%v", err)
	}
	r = newRig(t, rel, func(o *engine.Options) { o.Version = et.Newer; o.Checker = rel.Checker(et.Newer) })
	if _, err := r.e.Start(context.Background(), engine.Request{Safe: true}); !errors.Is(err, engine.ErrConflict) || !strings.Contains(err.Error(), "latest") {
		t.Fatalf("%v", err)
	}
}

func TestAServerThatCannotKeepTheFilesPrintsTheManualLinesOfItsInstall(t *testing.T) {
	for kind, want := range map[string][]string{
		update.Homebrew: {"brew upgrade kipp", "brew services restart kipp"},
		update.Docker:   {"docker pull ghcr.io/ndthuan/kipp:1.2.0", "docker run", "-v kipp-data:/var/lib/kipp"},
		update.Archive:  {update.ArchiveName(et.Newer, runtime.GOOS, runtime.GOARCH), "releases"},
	} {
		r := newRig(t, et.NewRelease(t, et.GoodBinary(et.Newer)), func(o *engine.Options) { o.InstallType = func() string { return kind } })
		if err := os.WriteFile(state.BinDir(r.data), []byte("a file where the directory goes"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := r.e.Start(context.Background(), engine.Request{Safe: true})
		var cannot *engine.CannotError
		if !errors.As(err, &cannot) || !errors.Is(err, engine.ErrConflict) {
			t.Fatalf("%s: %v", kind, err)
		}
		for _, w := range want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: %q is not in %q", kind, w, err)
			}
		}
		if r.rel.Hits.Load() != 1 {
			t.Fatalf("%s: something was downloaded", kind)
		}
	}
}

func TestAMissingDataDirectoryIsRefusedToo(t *testing.T) {
	r := newRig(t, et.NewRelease(t, et.GoodBinary(et.Newer)), nil)
	if err := os.RemoveAll(r.data); err != nil {
		t.Fatal(err)
	}
	_, err := r.e.Start(context.Background(), engine.Request{Safe: true})
	if !errors.Is(err, engine.ErrConflict) || !strings.Contains(err.Error(), "not there") {
		t.Fatalf("%v", err)
	}
}

func TestNotEnoughFreeSpaceFailsThePreflightWithTheSizesAndDownloadsNothing(t *testing.T) {
	r := newRig(t, et.NewRelease(t, et.GoodBinary(et.Newer)), func(o *engine.Options) {
		o.Free = func(string) (uint64, bool) { return 1 << 20, true }
	})
	start(t, r, engine.Request{Safe: true})
	in := r.settle(t)
	if in.Status.State != state.StateFailed || !strings.Contains(in.Error, "not enough free space") {
		t.Fatalf("%+v %q", in.Status, in.Error)
	}
	if r.rel.Hits.Load() != 1 {
		t.Fatal("something was downloaded")
	}
}

func TestAnUpgradeStartedWhileARecordIsPendingIsRefused(t *testing.T) {
	r := newRig(t, et.NewRelease(t, et.GoodBinary(et.Newer)), nil)
	if err := state.WritePending(r.data, state.Pending{Kind: state.KindUpgrade, Version: "v1.1.0", From: et.Running}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.e.Start(context.Background(), engine.Request{Safe: true}); !errors.Is(err, engine.ErrConflict) || !strings.Contains(err.Error(), "pending") {
		t.Fatalf("%v", err)
	}
}

func TestResumeSettlesWhatAnEndedProcessLeft(t *testing.T) {
	data := t.TempDir()
	e := engine.New(engine.Options{Data: data, Version: et.Newer})
	for _, c := range []struct{ in, version, want string }{
		{state.StateSwitching, et.Newer, state.StateDone},
		{state.StateSwitching, "v1.3.0", state.StateSwitching},
		{state.StateDownloading, "v1.3.0", state.StateFailed},
		{state.StateWaiting, "v1.3.0", state.StateFailed},
		{state.StateFailed, et.Newer, state.StateFailed},
		{state.StateCancelled, et.Newer, state.StateCancelled},
	} {
		if err := state.WriteStatus(data, state.Status{State: c.in, Version: c.version, From: et.Running}); err != nil {
			t.Fatal(err)
		}
		e.Resume()
		if st, _ := state.ReadStatus(data); st.State != c.want {
			t.Errorf("%s toward %s: %s, want %s", c.in, c.version, st.State, c.want)
		}
	}
	// No file at all is nothing to settle.
	e = engine.New(engine.Options{Data: t.TempDir(), Version: et.Newer})
	e.Resume()
}

// afterAnUpgrade makes a data directory the way a finished upgrade left it.
func (r *rig) afterAnUpgrade(t *testing.T, prev string) string {
	t.Helper()
	name, err := backup.Take(context.Background(), r.data, et.Running, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(backup.MarkSucceeded(r.data, name, time.Now()), state.WritePrevious(r.data, prev)); err != nil {
		t.Fatal(err)
	}
	return name
}

func TestARollbackWritesTheRecordThatRestoresTheLastBackupAfterQuietIsReached(t *testing.T) {
	r := newRig(t, et.NewRelease(t, et.GoodBinary(et.Newer)), func(o *engine.Options) { o.Version = "v1.1.0" })
	name := r.afterAnUpgrade(t, et.Running)
	in, _ := r.e.Info(context.Background())
	if in.RollbackTo != et.Running || in.RollbackBackup != name {
		t.Fatalf("%+v", in)
	}
	r.f.SetBusy(1)
	if _, err := r.e.Rollback(context.Background(), engine.Request{Safe: true}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if r.f.Switched.Load() != 0 {
		t.Fatal("rolled back while a build ran")
	}
	r.f.SetBusy(0)
	in = r.settle(t)
	p, _ := state.ReadPending(r.data)
	if p == nil || *p != (state.Pending{Kind: state.KindRollback, Version: et.Running, Backup: name, From: "v1.1.0"}) {
		t.Fatalf("%+v", p)
	}
	if in.Status.Version != et.Running || in.Status.From != "v1.1.0" {
		t.Fatalf("%+v", in.Status)
	}
}

func TestARollbackToTheBundledExecutableNamesItsVersionInTheStatus(t *testing.T) {
	t.Setenv("KIPP_BUNDLED_VERSION", "v0.9.0")
	r := newRig(t, et.NewRelease(t, et.GoodBinary(et.Newer)), nil)
	r.afterAnUpgrade(t, state.Bundled)
	if _, err := r.e.Rollback(context.Background(), engine.Request{Safe: true}); err != nil {
		t.Fatal(err)
	}
	in := r.settle(t)
	p, _ := state.ReadPending(r.data)
	if p.Version != state.Bundled || in.Status.Version != "v0.9.0" {
		t.Fatalf("%+v %+v", p, in.Status)
	}
}

func TestARollbackNeedsAnEarlierVersionABackupAndAConfirmationWithoutAWait(t *testing.T) {
	r := newRig(t, et.NewRelease(t, et.GoodBinary(et.Newer)), nil)
	if _, err := r.e.Rollback(context.Background(), engine.Request{Safe: true}); !errors.Is(err, engine.ErrConflict) {
		t.Fatalf("with nothing to go back to: %v", err)
	}
	if err := state.WritePrevious(r.data, et.Running); err != nil {
		t.Fatal(err)
	}
	if _, err := r.e.Rollback(context.Background(), engine.Request{Safe: true}); !errors.Is(err, engine.ErrConflict) {
		t.Fatalf("with no backup of a good upgrade: %v", err)
	}
	r.afterAnUpgrade(t, et.Running)
	r.f.SetBusy(2)
	if _, err := r.e.Rollback(context.Background(), engine.Request{}); !errors.Is(err, engine.ErrBadRequest) || !strings.Contains(err.Error(), "2 running") {
		t.Fatalf("unconfirmed: %v", err)
	}
	if _, err := r.e.Rollback(context.Background(), engine.Request{ConfirmLost: true}); err != nil {
		t.Fatal(err)
	}
	if in := r.settle(t); in.Status.State != state.StateSwitching || len(r.f.QuietCalls()) != 0 {
		t.Fatalf("%+v", in.Status)
	}
}

func TestADevelopmentBuildHasNoRollback(t *testing.T) {
	r := newRig(t, et.NewRelease(t, et.GoodBinary(et.Newer)), func(o *engine.Options) { o.InstallType = func() string { return update.Dev } })
	r.afterAnUpgrade(t, et.Running)
	if _, err := r.e.Rollback(context.Background(), engine.Request{Safe: true}); !errors.Is(err, engine.ErrConflict) {
		t.Fatalf("%v", err)
	}
}

// TestTheArchiveOfScriptsDistRunsAfterTheEngineUnpacksIt downloads a real
// release archive, built by scripts/dist.sh for this machine, through the
// engine: signed with a key of the test's, unpacked, run once. It needs no
// signing or quarantine step if the binary runs. KIPP_TEST_DIST names the
// directory dist.sh wrote and KIPP_TEST_DIST_VERSION the version it was given.
func TestTheArchiveOfScriptsDistRunsAfterTheEngineUnpacksIt(t *testing.T) {
	dir, ver := os.Getenv("KIPP_TEST_DIST"), os.Getenv("KIPP_TEST_DIST_VERSION")
	if dir == "" || ver != et.Newer {
		t.Skip("KIPP_TEST_DIST and KIPP_TEST_DIST_VERSION=" + et.Newer + " name a dist/ made by scripts/dist.sh")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	archive, err := root.ReadFile(update.ArchiveName(et.Newer, runtime.GOOS, runtime.GOARCH))
	if err != nil {
		t.Fatal(err)
	}
	rel := et.NewRelease(t, "")
	rel.SetArchive(archive)
	r := newRig(t, rel, nil)
	start(t, r, engine.Request{Safe: true})
	in := r.settle(t)
	if in.Status.State != state.StateSwitching {
		t.Fatalf("%+v %q", in.Status, in.Error)
	}
	t.Logf("%s/%s: the unpacked binary ran and said it is kipp %s", runtime.GOOS, runtime.GOARCH, et.Newer)
}

func TestWhatKeepsAnUpgradeFromStartingIsSaidInInfo(t *testing.T) {
	rel := et.NewRelease(t, et.GoodBinary(et.Newer))
	r := newRig(t, rel, func(o *engine.Options) { o.Checker = nil })
	in, err := r.e.Info(context.Background())
	if err != nil || !strings.Contains(in.Unavailable, "update check is off") || in.Status.State != "" {
		t.Fatalf("%+v, %v", in, err)
	}
	// A release that carries no signature file is not one to upgrade to.
	rel.Srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"` + et.Newer + `","html_url":"x","assets":[]}`))
	})
	r = newRig(t, rel, nil)
	in, _ = r.e.Info(context.Background())
	if !strings.Contains(in.Unavailable, "no signature file") {
		t.Fatalf("%q", in.Unavailable)
	}
	if _, err := r.e.Start(context.Background(), engine.Request{Safe: true}); !errors.Is(err, engine.ErrConflict) || !strings.Contains(err.Error(), "no signature file") {
		t.Fatalf("%v", err)
	}
	// And a running upgrade says that it is running.
	rel = et.NewRelease(t, et.GoodBinary(et.Newer))
	r = newRig(t, rel, nil)
	r.f.SetBusy(1)
	start(t, r, engine.Request{Safe: true})
	in, _ = r.e.Info(context.Background())
	if in.Unavailable != "an upgrade is already running" {
		t.Fatalf("%q", in.Unavailable)
	}
	if _, err := r.e.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestARollbackThatIsCancelledWhileItWaitsLeavesQuietOffAndWritesNothing(t *testing.T) {
	r := newRig(t, et.NewRelease(t, et.GoodBinary(et.Newer)), nil)
	r.afterAnUpgrade(t, et.Running)
	r.f.SetBusy(1)
	if _, err := r.e.Rollback(context.Background(), engine.Request{Safe: true}); err != nil {
		t.Fatal(err)
	}
	untilWaiting(t, r, 1)
	in, err := r.e.Cancel(context.Background())
	if err != nil || in.Status.State != state.StateCancelled {
		t.Fatalf("%+v, %v", in.Status, err)
	}
	if p, _ := state.ReadPending(r.data); p != nil || r.f.Switched.Load() != 0 {
		t.Fatal("a rollback was written")
	}
	if got := r.f.QuietCalls(); len(got) != 2 || got[1] {
		t.Fatalf("quiet calls %v", got)
	}
}

func TestAServerPinnedToItsOwnExecutableRefusesAnUpgradeBeforeDownloading(t *testing.T) {
	rel := et.NewRelease(t, et.GoodBinary(et.Newer))
	r := newRig(t, rel, nil)
	t.Setenv(launch.EnvUseBundled, "1")
	_, err := r.e.Start(context.Background(), engine.Request{Safe: true})
	if !errors.Is(err, engine.ErrConflict) || !strings.Contains(err.Error(), "pinned to its own executable") {
		t.Fatalf("%v", err)
	}
	in, _ := r.e.Info(context.Background())
	if !strings.Contains(in.Unavailable, "KIPP_USE_BUNDLED_BINARY=1") || in.Status.State != "" {
		t.Fatalf("%q %+v", in.Unavailable, in.Status)
	}
	if got := r.binEntries(t); len(got) != 0 {
		t.Fatalf("bin/ holds %v", got)
	}
	if p, _ := state.ReadPending(r.data); p != nil {
		t.Fatalf("pending %+v", p)
	}
}

func TestAServerPinnedToItsOwnExecutableRefusesARollbackAndOffersNone(t *testing.T) {
	r := newRig(t, et.NewRelease(t, et.GoodBinary(et.Newer)), nil)
	r.afterAnUpgrade(t, et.Running)
	if in, _ := r.e.Info(context.Background()); in.RollbackTo == "" {
		t.Fatal("no rollback to refuse")
	}
	t.Setenv(launch.EnvUseBundled, "1")
	_, err := r.e.Rollback(context.Background(), engine.Request{Safe: true})
	if !errors.Is(err, engine.ErrConflict) || !strings.Contains(err.Error(), "pinned to its own executable") {
		t.Fatalf("%v", err)
	}
	if in, _ := r.e.Info(context.Background()); in.RollbackTo != "" || in.RollbackBackup != "" {
		t.Fatalf("%+v", in)
	}
}
