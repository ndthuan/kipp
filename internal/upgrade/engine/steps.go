package engine

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/ndthuan/kipp/internal/update"
	"github.com/ndthuan/kipp/internal/upgrade/backup"
	"github.com/ndthuan/kipp/internal/upgrade/launch"
	"github.com/ndthuan/kipp/internal/upgrade/state"
)

// upgrade is the steps of an upgrade: it returns nil once the switch is
// asked for, and the error of the step that stopped it.
func (e *Engine) upgrade(ctx context.Context, r *active, req Request, st state.Status) (err error) {
	// What a run that stops leaves must be what it found.
	var binary string
	var quiet *quietGuard
	defer func() {
		if err == nil {
			return
		}
		if quiet != nil {
			quiet.release()
		}
		if binary != "" {
			_ = os.Remove(binary)
		}
	}()
	if err = backup.Preflight(e.o.Data, unpackReserve, e.o.Free); err != nil {
		return err
	}
	if binary, err = e.download(ctx, st); err != nil {
		return err
	}
	if req.Safe {
		if quiet, err = e.wait(ctx, req, st); err != nil {
			return err
		}
	}
	return e.hand(ctx, r, st, state.Pending{Kind: state.KindUpgrade, Version: st.Version, From: e.o.Version})
}

// rollback waits for the builds to end, when asked to, and writes the
// record that has the launcher put the backup back.
func (e *Engine) rollback(ctx context.Context, r *active, req Request, st state.Status, to string) (err error) {
	var quiet *quietGuard
	defer func() {
		if err != nil && quiet != nil {
			quiet.release()
		}
	}()
	if req.Safe {
		if quiet, err = e.wait(ctx, req, st); err != nil {
			return err
		}
	}
	return e.hand(ctx, r, st, state.Pending{Kind: state.KindRollback, Version: to, Backup: st.Backup, From: e.o.Version})
}

// hand is the last step: from here only a rollback undoes the upgrade. An
// upgrade takes its backup now, while nothing writes, and both write the
// record and ask the server to stop and start again.
func (e *Engine) hand(ctx context.Context, r *active, st state.Status, p state.Pending) error {
	if !e.commit(ctx, r) {
		return context.Canceled
	}
	if p.Kind == state.KindUpgrade {
		st.State = state.StateBackup
		e.writeStatus(st)
		name, err := backup.Take(context.WithoutCancel(ctx), e.o.Data, e.o.Version, e.o.Now())
		if err != nil {
			return fmt.Errorf("the backup failed, so nothing was switched: %w", err)
		}
		p.Backup, st.Backup = name, name
	}
	if err := state.WritePending(e.o.Data, p); err != nil {
		return fmt.Errorf("writing the pending record: %w", err)
	}
	st.State = state.StateSwitching
	e.writeStatus(st)
	e.o.Log.Info("switching", "kind", p.Kind, "version", p.Version, "from", e.o.Version, "backup", p.Backup)
	e.o.Switch()
	return nil
}

// download fetches, checks and unpacks the release, and proves the binary
// runs. The binary is put in bin/ only when all of that has passed.
func (e *Engine) download(ctx context.Context, st state.Status) (string, error) {
	st.State = state.StateDownloading
	e.writeStatus(st)
	bin, err := e.fetch(ctx, st.Version, func() {
		st.State = state.StateVerifying
		e.writeStatus(st)
	})
	if err != nil {
		return "", err
	}
	return e.install(bin, st.Version)
}

// fetch downloads the files of release ver, checks the signature and the
// checksum, and returns the binary unpacked from the archive. verifying is
// called once the files are in, before they are checked.
func (e *Engine) fetch(ctx context.Context, ver string, verifying func()) ([]byte, error) {
	goos, goarch := cmpOr(e.o.OS, runtime.GOOS), cmpOr(e.o.Arch, runtime.GOARCH)
	name := update.ArchiveName(ver, goos, goarch)
	get := func(file string, max int64) ([]byte, error) { return e.o.Checker.Fetch(ctx, ver, file, max) }
	sums, err := get(update.SumsFile, update.MaxSmall)
	if err != nil {
		return nil, err
	}
	sig, err := get(update.SigFile, update.MaxSmall)
	if err != nil {
		return nil, err
	}
	archive, err := get(name, update.MaxArchive)
	if err != nil {
		return nil, err
	}
	verifying()
	if err := update.Verify(e.o.Key, archive, sums, sig, name); err != nil {
		return nil, err
	}
	return update.ExtractBinary(archive, update.ArchiveDir(ver, goos, goarch), update.MaxArchive)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// install writes the binary beside its place, runs it once, and renames it
// there. A binary that does not run is not left behind.
func (e *Engine) install(bin []byte, ver string) (string, error) {
	tmp, err := writeExecutable(state.BinDir(e.o.Data), ".kipp-"+ver+".*", bin)
	if err == nil {
		err = probe(tmp, ver)
	}
	dst := state.BinaryPath(e.o.Data, ver)
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		if tmp != "" {
			_ = os.Remove(tmp)
		}
		return "", fmt.Errorf("the downloaded kipp %s does not run here: %w", ver, err)
	}
	return dst, nil
}

// probe runs the binary's version once. A file just written can be
// reported busy by Linux while another process still holds a copy of the
// descriptor from before it was closed; that passes in a moment.
func probe(path, ver string) error {
	var err error
	for range 10 {
		if err = launch.Usable(path, ver); err == nil || !strings.Contains(err.Error(), "text file busy") {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return err
}

// quietGuard turns quiet mode off again, unless it was on before the run.
type quietGuard struct {
	e   *Engine
	was bool
}

func (q *quietGuard) release() {
	if q.was {
		return
	}
	if _, err := q.e.o.SetQuiet(context.Background(), false); err != nil {
		q.e.o.Log.Error("quiet mode could not be turned off again", "err", err)
	}
}

// wait makes the server quiet and waits until no build runs or waits. It
// returns the guard that undoes the quiet; on an error quiet is already off.
func (e *Engine) wait(ctx context.Context, req Request, st state.Status) (*quietGuard, error) {
	before, err := e.o.Counts(ctx)
	if err != nil {
		return nil, err
	}
	g := &quietGuard{e: e, was: before.Quiet}
	st.State = state.StateWaiting
	e.writeStatus(st)
	c, err := e.o.SetQuiet(ctx, true)
	if err != nil {
		g.release()
		return nil, err
	}
	timeout := req.WaitTimeout
	if timeout <= 0 {
		timeout = DefaultWait
	}
	deadline := e.o.Now().Add(timeout)
	for c.Running+c.Queued > 0 {
		if st.Running != c.Running || st.Queued != c.Queued || st.Rerun != c.Rerun {
			st.Running, st.Queued, st.Rerun = c.Running, c.Queued, c.Rerun
			e.writeStatus(st)
		}
		if !e.o.Now().Before(deadline) {
			g.release()
			return nil, fmt.Errorf("still busy after %s (%d running, %d queued): quiet is off again and nothing was restarted", timeout, c.Running, c.Queued)
		}
		select {
		case <-ctx.Done():
			g.release()
			return nil, ctx.Err()
		case <-time.After(e.o.Poll):
		}
		if c, err = e.o.Counts(ctx); err != nil {
			g.release()
			return nil, err
		}
	}
	return g, nil
}

// canWork refuses, before anything is done, a server that cannot keep the
// files of an upgrade, and says how to upgrade by hand instead.
func (e *Engine) canWork(target string) error {
	if err := e.writable(); err != nil {
		return &CannotError{Err: err, Manual: e.manual(target)}
	}
	return nil
}

func (e *Engine) writable() error {
	if fi, err := os.Stat(e.o.Data); err != nil || !fi.IsDir() {
		return fmt.Errorf("the data directory %s is not there", e.o.Data)
	}
	for _, dir := range []string{state.BinDir(e.o.Data), state.UpgradeDir(e.o.Data)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		f, err := os.CreateTemp(dir, ".probe-*")
		if err != nil {
			return err
		}
		_ = f.Close()
		_ = os.Remove(f.Name())
	}
	return nil
}

// CannotError says this server cannot upgrade itself and what to do by hand.
type CannotError struct {
	Err    error
	Manual string
}

func (c *CannotError) Error() string {
	return fmt.Sprintf("%s: this server cannot upgrade itself (%v). Upgrade by hand instead:\n%s", ErrConflict, c.Err, c.Manual)
}

// Is makes it a conflict, which is what the API answers it with.
func (c *CannotError) Is(target error) bool { return target == ErrConflict }

// manual is the lines that upgrade this kind of install by hand.
func (e *Engine) manual(target string) string {
	switch e.o.InstallType() {
	case update.Homebrew:
		return "  brew upgrade kipp\n  brew services restart kipp"
	case update.Docker:
		return "  docker pull ghcr.io/ndthuan/kipp:" + strings.TrimPrefix(target, "v") + "\n" +
			"  docker rm -f kipp\n" +
			"  docker run -d --name kipp -p 8085:8085 -v kipp-data:/var/lib/kipp -v ./jobs:/etc/kipp ghcr.io/ndthuan/kipp:" + strings.TrimPrefix(target, "v") + "\n" +
			"  (with your own volumes and flags, as docs/install.md has them)"
	}
	return "  download " + update.ArchiveName(target, cmpOr(e.o.OS, runtime.GOOS), cmpOr(e.o.Arch, runtime.GOARCH)) +
		" from https://github.com/ndthuan/kipp/releases, put its kipp where the old one is, and restart the server"
}
