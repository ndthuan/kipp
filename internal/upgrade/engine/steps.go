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

func (e *Engine) upgrade(ctx context.Context, r *active, req Request, st state.Status) (err error) {
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
