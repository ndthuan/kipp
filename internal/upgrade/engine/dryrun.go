package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ndthuan/kipp/internal/upgrade/backup"
)

// The checks of a dry run, in the order it makes them.
const (
	CheckRelease  = "release"
	CheckSpace    = "space"
	CheckDownload = "download"
	CheckJobs     = "jobs"
	CheckMigrate  = "migrate"
	CheckBuilds   = "builds"
)

// How a check of a dry run ended. A check is skipped when one it needs
// failed.
const (
	Passed  = "passed"
	Failed  = "failed"
	Skipped = "skipped"
)

// The longest the new binary may take: a job check reads files, a
// migration may rewrite a large database.
var (
	jobsTimeout    = 2 * time.Minute
	migrateTimeout = 30 * time.Minute
)

// dryRunPrefix names the temporary directory of a dry run in the data
// directory.
const dryRunPrefix = ".upgrade-dry-run-"

// clearDryRuns removes the temporary directories of dry runs a process
// ended in the middle of; it is called before any dry run can start.
func (e *Engine) clearDryRuns() {
	left, _ := filepath.Glob(filepath.Join(e.o.Data, dryRunPrefix+"*"))
	for _, dir := range left {
		if err := os.RemoveAll(dir); err != nil {
			e.o.Log.Warn("not removing what a dry run left", "dir", dir, "err", err)
		}
	}
}

// outputLines is how much of what the new binary printed a failed check
// keeps.
const outputLines = 20

// DryStep is one check of a dry run and how it ended.
type DryStep struct {
	Name, Result, Detail string
}

// DryRun is what an upgrade would meet, found without changing the server.
type DryRun struct {
	// Version is the release checked, empty when none was found; From is
	// the running server's.
	Version, From string
	// OK: every check passed.
	OK    bool
	Steps []DryStep
	// Running and Queued are the builds a safe restart would wait for.
	Running, Queued int
}

// dryRun is one dry run's progress.
type dryRun struct {
	r DryRun
}

func (d *dryRun) add(name, result, format string, a ...any) {
	d.r.Steps = append(d.r.Steps, DryStep{Name: name, Result: result, Detail: fmt.Sprintf(format, a...)})
}

func (d *dryRun) fail(name string, err error) { d.add(name, Failed, "%v", err) }

func (d *dryRun) skip(name, why string) { d.add(name, Skipped, "%s", why) }

// DryRunUpgrade makes every check of an upgrade to version (the latest
// release when empty) but the restart: it finds the release, checks the
// room, downloads and verifies it, has the new binary check the jobs
// directory and migrate a copy of the database, and counts the builds a
// safe restart would wait for. Nothing it does changes the server: no
// quiet, no backup, nothing in bin/, no pending record; its files are in a
// temporary directory of the data directory, removed before it returns.
func (e *Engine) DryRunUpgrade(ctx context.Context, version string) (DryRun, error) {
	if !e.dryMu.TryLock() {
		return DryRun{}, conflict("a dry run is already running")
	}
	defer e.dryMu.Unlock()
	d := &dryRun{r: DryRun{From: e.o.Version}}
	target, relErr := e.dryRelease(version)
	if relErr != nil {
		d.fail(CheckRelease, relErr)
	} else {
		d.r.Version = target
		d.add(CheckRelease, Passed, "%s is the latest release and kipp can upgrade to it", target)
	}
	spaceOK := e.drySpace(d)
	switch {
	case relErr != nil:
		d.skipNew("there is no release to check")
	case !spaceOK:
		d.skipNew("there is not the room to check it")
	default:
		e.dryNew(ctx, d, target)
	}
	e.dryBuilds(ctx, d)
	d.r.OK = true
	for _, s := range d.r.Steps {
		d.r.OK = d.r.OK && s.Result == Passed
	}
	e.o.Log.Info("upgrade dry run", "version", target, "ok", d.r.OK)
	return d.r, nil
}

// skipNew marks the checks that need the new binary as skipped.
func (d *dryRun) skipNew(why string) {
	for _, name := range []string{CheckDownload, CheckJobs, CheckMigrate} {
		d.skip(name, why)
	}
}

func (d *dryRun) failDownload(err error) {
	d.fail(CheckDownload, err)
	for _, name := range []string{CheckJobs, CheckMigrate} {
		d.skip(name, "nothing was downloaded")
	}
}

// dryRelease is the release an upgrade would go to, or why there is none:
// the same refusals as Start's.
func (e *Engine) dryRelease(version string) (string, error) {
	var latest string
	var upgradable bool
	if e.o.Checker != nil {
		s := e.o.Checker.Status()
		latest, upgradable = s.Latest, s.Upgradable
	}
	if why := e.unavailable(false, latest, upgradable); why != "" {
		return "", errors.New(why)
	}
	if version != "" && version != latest {
		return "", fmt.Errorf("the version must be the latest release (%s), not %q", latest, version)
	}
	return latest, nil
}

// drySpace checks the room the upgrade asks for, and then the room for this
// run's own copy of the database beside it.
func (e *Engine) drySpace(d *dryRun) bool {
	if err := backup.Preflight(e.o.Data, unpackReserve, e.o.Free); err != nil {
		d.fail(CheckSpace, err)
		return false
	}
	need := unpackReserve + backup.Need(e.o.Data)
	if err := backup.Preflight(e.o.Data, need, e.o.Free); err != nil {
		d.fail(CheckSpace, fmt.Errorf("the upgrade has room, but not this dry run's copy of the database too: %w", err))
		return false
	}
	have, known := e.o.Free(e.o.Data)
	if !known {
		d.add(CheckSpace, Passed, "the free space of %s cannot be told, so an upgrade goes on without knowing it", e.o.Data)
		return true
	}
	d.add(CheckSpace, Passed, "%.1f MiB free in %s, %.1f MiB needed", float64(have)/(1<<20), e.o.Data, float64(need)/(1<<20))
	return true
}

// dryNew downloads the release into a temporary directory and has its
// binary check the jobs and migrate a copy of the database there.
func (e *Engine) dryNew(ctx context.Context, d *dryRun, ver string) {
	tmp, err := os.MkdirTemp(e.o.Data, dryRunPrefix)
	if err != nil {
		d.failDownload(err)
		return
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	bin, err := e.dryDownload(ctx, tmp, ver)
	if err != nil {
		d.failDownload(err)
		return
	}
	d.add(CheckDownload, Passed, "SHA256SUMS is signed by the release key, the archive matches it, and the binary says it is kipp %s", ver)
	e.dryJobs(ctx, d, bin)
	e.dryMigrate(ctx, d, bin, tmp)
}

func (e *Engine) dryDownload(ctx context.Context, dir, ver string) (string, error) {
	bin, err := e.fetch(ctx, ver, func() {})
	if err != nil {
		return "", err
	}
	path, err := writeExecutable(dir, "kipp-*", bin)
	if err == nil {
		err = probe(path, ver)
	}
	if err != nil {
		return "", fmt.Errorf("the downloaded kipp %s does not run here: %w", ver, err)
	}
	return path, nil
}

func (e *Engine) dryJobs(ctx context.Context, d *dryRun, bin string) {
	if e.o.Jobs == "" {
		d.skip(CheckJobs, "this server did not say where its jobs directory is")
		return
	}
	out, err := runNew(ctx, jobsTimeout, bin, "check", e.o.Jobs)
	if err != nil {
		d.fail(CheckJobs, fmt.Errorf("the new version refuses the job files in %s: %w", e.o.Jobs, err))
		return
	}
	d.add(CheckJobs, Passed, "the new version reads the job files in %s: %s", e.o.Jobs, out)
}

func (e *Engine) dryMigrate(ctx context.Context, d *dryRun, bin, tmp string) {
	copyDir := filepath.Join(tmp, "data")
	err := os.Mkdir(copyDir, 0o700)
	if err == nil {
		err = backup.CopyDatabase(ctx, e.o.Data, copyDir)
	}
	if err != nil {
		d.fail(CheckMigrate, fmt.Errorf("copying the database: %w", err))
		return
	}
	out, err := runNew(ctx, migrateTimeout, bin, "migrate", "--data", copyDir)
	if err != nil {
		d.fail(CheckMigrate, fmt.Errorf("the new version could not migrate a copy of the database: %w", err))
		return
	}
	if err := backup.CheckDatabase(ctx, filepath.Join(copyDir, backup.DBFile)); err != nil {
		d.fail(CheckMigrate, fmt.Errorf("the migrated copy: %w", err))
		return
	}
	d.add(CheckMigrate, Passed, "a copy of the database migrated and passes its integrity check: %s", out)
}

func (e *Engine) dryBuilds(ctx context.Context, d *dryRun) {
	if e.o.Counts == nil {
		d.skip(CheckBuilds, "this server does not say what runs")
		return
	}
	c, err := e.o.Counts(ctx)
	if err != nil {
		d.fail(CheckBuilds, err)
		return
	}
	d.r.Running, d.r.Queued = c.Running, c.Queued
	if c.Running+c.Queued == 0 {
		d.add(CheckBuilds, Passed, "no build runs or waits, so a safe restart would not wait")
		return
	}
	d.add(CheckBuilds, Passed, "%d running and %d queued: a safe restart waits for them in quiet mode first", c.Running, c.Queued)
}

// runNew runs the downloaded binary with args and returns the end of what it
// printed; a binary that fails or takes longer than timeout is an error
// carrying that end.
func runNew(ctx context.Context, timeout time.Duration, bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	text := lastLines(strings.TrimSpace(string(out)), outputLines)
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return text, fmt.Errorf("it did not finish within %s", timeout)
	case err != nil && text != "":
		return text, fmt.Errorf("%w: %s", err, text)
	}
	return text, err
}

func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// writeExecutable writes bin to a new file of dir, for its owner to run.
func writeExecutable(dir, pattern string, bin []byte) (string, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	_, err = f.Write(bin)
	if err == nil {
		err = f.Chmod(0o755)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}
