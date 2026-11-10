package engine_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/ndthuan/kipp/internal/update"
	"github.com/ndthuan/kipp/internal/upgrade/backup"
	"github.com/ndthuan/kipp/internal/upgrade/engine"
	et "github.com/ndthuan/kipp/internal/upgrade/engine/enginetest"
	"github.com/ndthuan/kipp/internal/upgrade/state"
)

// dryRig is a server with a jobs directory and a release whose binary does
// what a dry run asks, writing what it was asked to log.
type dryRig struct {
	*rig
	jobs, log string
}

type dryBinary struct{ refuseJobs, failMigrate, breakDB bool }

func newDryRig(t *testing.T, b dryBinary, tweak func(*engine.Options)) *dryRig {
	t.Helper()
	jobs := t.TempDir()
	log := filepath.Join(t.TempDir(), "calls")
	rel := et.NewRelease(t, et.DryRunBinary(et.Newer, log, b.refuseJobs, b.failMigrate, b.breakDB))
	r := newRig(t, rel, func(o *engine.Options) {
		o.Jobs = jobs
		if tweak != nil {
			tweak(o)
		}
	})
	return &dryRig{rig: r, jobs: jobs, log: log}
}

// snapshot is every file under dir by its path, with its mode and the
// sha256 of what it holds.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	fsys := os.DirFS(dir)
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		val := info.Mode().String()
		if d.Type().IsRegular() {
			b, err := fs.ReadFile(fsys, path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(b)
			val += " " + hex.EncodeToString(sum[:])
		}
		out[path] = val
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// dryRun runs a dry run and holds that it left the data directory as it
// found it, made nothing quiet and restarted nothing.
func (r *dryRig) dryRun(t *testing.T, version string) engine.DryRun {
	t.Helper()
	before := snapshot(t, r.data)
	got, err := r.e.DryRunUpgrade(context.Background(), version)
	if err != nil {
		t.Fatal(err)
	}
	if after := snapshot(t, r.data); !maps.Equal(before, after) {
		t.Fatalf("the data directory changed:\nbefore %v\nafter  %v", before, after)
	}
	if len(r.f.QuietCalls()) != 0 || r.f.Switched.Load() != 0 {
		t.Fatal("something was made quiet or restarted")
	}
	if p, _ := state.ReadPending(r.data); p != nil {
		t.Fatal("a pending record")
	}
	return got
}

func results(d engine.DryRun) map[string]string {
	out := map[string]string{}
	for _, s := range d.Steps {
		out[s.Name] = s.Result
	}
	return out
}

func step(t *testing.T, d engine.DryRun, name string) engine.DryStep {
	t.Helper()
	for _, s := range d.Steps {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no step %s in %+v", name, d.Steps)
	return engine.DryStep{}
}

// wantSteps holds the result of every step, in the order they are made.
func wantSteps(t *testing.T, d engine.DryRun, want map[string]string) {
	t.Helper()
	var names []string
	for _, s := range d.Steps {
		names = append(names, s.Name)
	}
	order := []string{engine.CheckRelease, engine.CheckSpace, engine.CheckDownload, engine.CheckJobs, engine.CheckMigrate, engine.CheckBuilds}
	if !slices.Equal(names, order) {
		t.Fatalf("steps %v, want %v", names, order)
	}
	if got := results(d); !maps.Equal(got, want) {
		t.Fatalf("results %v, want %v\n%+v", got, want, d.Steps)
	}
	ok := true
	for _, r := range want {
		ok = ok && r == engine.Passed
	}
	if d.OK != ok {
		t.Fatalf("ok is %v with %v", d.OK, want)
	}
}

func allPassed() map[string]string {
	return map[string]string{engine.CheckRelease: engine.Passed, engine.CheckSpace: engine.Passed, engine.CheckDownload: engine.Passed,
		engine.CheckJobs: engine.Passed, engine.CheckMigrate: engine.Passed, engine.CheckBuilds: engine.Passed}
}

// allPassedBut is allPassed with the results of changed.
func allPassedBut(changed map[string]string) map[string]string {
	m := allPassed()
	maps.Copy(m, changed)
	return m
}

func TestADryRunPassesEveryStepAndHasTheNewBinaryCheckTheJobsAndMigrateACopy(t *testing.T) {
	r := newDryRig(t, dryBinary{}, nil)
	r.f.SetBusy(2)
	d := r.dryRun(t, "")
	wantSteps(t, d, allPassed())
	if d.Version != et.Newer || d.From != et.Running || d.Running != 2 {
		t.Fatalf("%+v", d)
	}
	if s := step(t, d, engine.CheckBuilds); !strings.Contains(s.Detail, "2 running") {
		t.Fatalf("%+v", s)
	}
	calls, err := fs.ReadFile(os.DirFS(filepath.Dir(r.log)), filepath.Base(r.log))
	if err != nil {
		t.Fatal(err)
	}
	text := string(calls)
	if !strings.Contains(text, "check "+r.jobs+"\n") {
		t.Fatalf("the jobs were not checked by the new binary:\n%s", text)
	}
	copyDir := filepath.Join(r.data, ".upgrade-dry-run-")
	if !strings.Contains(text, "migrate --data "+copyDir) || strings.Contains(text, "migrate --data "+r.data+"\n") {
		t.Fatalf("the migration did not run on a copy:\n%s", text)
	}
}

func TestADryRunOfTheVersionNamedNeedsItToBeTheLatest(t *testing.T) {
	r := newDryRig(t, dryBinary{}, nil)
	wantSteps(t, r.dryRun(t, et.Newer), allPassed())
	d := r.dryRun(t, "v1.1.0")
	wantSteps(t, d, allPassedBut(map[string]string{engine.CheckRelease: engine.Failed, engine.CheckDownload: engine.Skipped, engine.CheckJobs: engine.Skipped, engine.CheckMigrate: engine.Skipped}))
	if s := step(t, d, engine.CheckRelease); !strings.Contains(s.Detail, "latest release ("+et.Newer+")") {
		t.Fatalf("%+v", s)
	}
}

func TestADryRunOfADevelopmentBuildFailsItsReleaseStepAsTheUpgradeWould(t *testing.T) {
	r := newDryRig(t, dryBinary{}, func(o *engine.Options) { o.InstallType = func() string { return update.Dev } })
	d := r.dryRun(t, "")
	if s := step(t, d, engine.CheckRelease); s.Result != engine.Failed || !strings.Contains(s.Detail, "development build") {
		t.Fatalf("%+v", s)
	}
	if d.OK || r.rel.Hits.Load() > 1 {
		t.Fatalf("ok %v, %d requests", d.OK, r.rel.Hits.Load())
	}
}

func TestABadSignatureFailsTheDownloadStep(t *testing.T) {
	r := newDryRig(t, dryBinary{}, nil)
	_, other, _ := update.GenerateKey()
	r.rel.Files[update.SigFile] = other.Sign(r.rel.Files[update.SumsFile], "file:SHA256SUMS")
	d := r.dryRun(t, "")
	wantSteps(t, d, allPassedBut(map[string]string{engine.CheckDownload: engine.Failed, engine.CheckJobs: engine.Skipped, engine.CheckMigrate: engine.Skipped}))
	if s := step(t, d, engine.CheckDownload); !strings.Contains(s.Detail, "signature") {
		t.Fatalf("%+v", s)
	}
}

func TestAChecksumMismatchFailsTheDownloadStep(t *testing.T) {
	r := newDryRig(t, dryBinary{}, nil)
	name := update.ArchiveName(et.Newer, runtime.GOOS, runtime.GOARCH)
	r.rel.Files[name] = et.ArchiveOf(t, et.Newer, et.GoodBinary(et.Newer)+"# another\n")
	d := r.dryRun(t, "")
	wantSteps(t, d, allPassedBut(map[string]string{engine.CheckDownload: engine.Failed, engine.CheckJobs: engine.Skipped, engine.CheckMigrate: engine.Skipped}))
	if s := step(t, d, engine.CheckDownload); !strings.Contains(s.Detail, "checksum") {
		t.Fatalf("%+v", s)
	}
}

func TestTooLittleDiskFailsTheSpaceStepAndDownloadsNothing(t *testing.T) {
	r := newDryRig(t, dryBinary{}, func(o *engine.Options) { o.Free = func(string) (uint64, bool) { return 1 << 20, true } })
	d := r.dryRun(t, "")
	wantSteps(t, d, allPassedBut(map[string]string{engine.CheckSpace: engine.Failed, engine.CheckDownload: engine.Skipped, engine.CheckJobs: engine.Skipped, engine.CheckMigrate: engine.Skipped}))
	if s := step(t, d, engine.CheckSpace); !strings.Contains(s.Detail, "not enough free space") {
		t.Fatalf("%+v", s)
	}
	if hits := r.rel.Hits.Load(); hits != 1 {
		t.Fatalf("%d requests: only the lookup was to be made", hits)
	}
}

func TestRoomForTheUpgradeButNotForTheCopySaysSo(t *testing.T) {
	r := newDryRig(t, dryBinary{}, func(o *engine.Options) {
		o.Free = func(dir string) (uint64, bool) { return backup.Need(dir) + 128<<20 + 1, true }
	})
	d := r.dryRun(t, "")
	if s := step(t, d, engine.CheckSpace); s.Result != engine.Failed || !strings.Contains(s.Detail, "dry run's copy") {
		t.Fatalf("%+v", s)
	}
}

func TestAJobFileTheNewVersionRefusesFailsTheJobsStepWithWhatItSaid(t *testing.T) {
	r := newDryRig(t, dryBinary{refuseJobs: true}, nil)
	d := r.dryRun(t, "")
	wantSteps(t, d, allPassedBut(map[string]string{engine.CheckJobs: engine.Failed}))
	if s := step(t, d, engine.CheckJobs); !strings.Contains(s.Detail, "unknown field shell") {
		t.Fatalf("%+v", s)
	}
}

func TestAFailingMigrationFailsTheMigrateStepWithWhatItSaid(t *testing.T) {
	r := newDryRig(t, dryBinary{failMigrate: true}, nil)
	d := r.dryRun(t, "")
	wantSteps(t, d, allPassedBut(map[string]string{engine.CheckMigrate: engine.Failed}))
	if s := step(t, d, engine.CheckMigrate); !strings.Contains(s.Detail, "no such column") {
		t.Fatalf("%+v", s)
	}
}

func TestAMigratedCopyThatFailsItsIntegrityCheckFailsTheMigrateStep(t *testing.T) {
	r := newDryRig(t, dryBinary{breakDB: true}, nil)
	d := r.dryRun(t, "")
	wantSteps(t, d, allPassedBut(map[string]string{engine.CheckMigrate: engine.Failed}))
	if s := step(t, d, engine.CheckMigrate); !strings.Contains(s.Detail, "migrated copy") {
		t.Fatalf("%+v", s)
	}
}

func TestADryRunWhileAnotherRunsIsRefused(t *testing.T) {
	r := newDryRig(t, dryBinary{}, nil)
	block := make(chan struct{})
	entered := make(chan struct{})
	r.e = engine.New(engine.Options{Data: r.data, Jobs: r.jobs, Version: et.Running, Checker: r.rel.Checker(et.Running), Key: r.rel.Pub,
		InstallType: func() string { return update.Archive }, SetQuiet: r.f.SetQuiet, Switch: r.f.Switch,
		Free: func(string) (uint64, bool) { return 1 << 40, true },
		Counts: func(context.Context) (engine.Counts, error) {
			close(entered)
			<-block
			return engine.Counts{}, nil
		}})
	done := make(chan error, 1)
	go func() {
		_, err := r.e.DryRunUpgrade(context.Background(), "")
		done <- err
	}()
	<-entered
	_, err := r.e.DryRunUpgrade(context.Background(), "")
	close(block)
	if !errors.Is(err, engine.ErrConflict) {
		t.Fatalf("%v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWhatADryRunLeftWhenItsProcessEndedIsRemovedWhenTheServerStarts(t *testing.T) {
	r := newDryRig(t, dryBinary{}, nil)
	left := filepath.Join(r.data, ".upgrade-dry-run-123", "data")
	if err := os.MkdirAll(left, 0o700); err != nil {
		t.Fatal(err)
	}
	r.e.Resume()
	if _, err := os.Stat(filepath.Dir(left)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("it is still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.data, "db.sqlite")); err != nil {
		t.Fatal(err)
	}
}
