// Package engine carries out an upgrade of a running server, or a return
// to the version before: preflight, download and check, wait for the
// builds to end, back up, and hand over to the launcher through a pending
// record. The server stops and starts again by itself (Options.Switch);
// what comes after is the launcher's (package launch).
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/ndthuan/kipp/internal/diskfree"
	"github.com/ndthuan/kipp/internal/update"
	"github.com/ndthuan/kipp/internal/upgrade/backup"
	"github.com/ndthuan/kipp/internal/upgrade/launch"
	"github.com/ndthuan/kipp/internal/upgrade/state"
)

// The two ways a request is refused. Both wrap the sentence a person reads.
var (
	// ErrConflict: the server is not in a state for it (409).
	ErrConflict = errors.New("conflict")
	// ErrBadRequest: the request itself is wrong.
	ErrBadRequest = errors.New("bad request")
)

// pinnedText is why a server that KIPP_USE_BUNDLED_BINARY=1 holds to its
// own executable cannot be switched: the launcher is what carries out the
// switch, and the variable skips it, so a run would restart the same
// version and leave the status at switching.
const pinnedText = "this server is pinned to its own executable (" + launch.EnvUseBundled + "=1)"

func pinned() bool { return os.Getenv(launch.EnvUseBundled) == "1" }

func conflict(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrConflict, fmt.Sprintf(format, a...))
}

func badRequest(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrBadRequest, fmt.Sprintf(format, a...))
}

const (
	// DefaultWait is how long the wait for the builds to end lasts.
	DefaultWait = 2 * time.Hour
	// unpackReserve is the room asked for beside the backup: the archive
	// and the binary unpacked from it, with margin.
	unpackReserve = 128 << 20
	cancelWait    = 10 * time.Second
)

// Counts is what the server says of its quiet mode and its builds.
type Counts struct {
	Quiet           bool
	Running, Queued int
}

// Options are what an Engine needs of the server around it.
type Options struct {
	// Data is the data directory, Version the running server's.
	Data, Version string
	// Checker knows the latest release and fetches its files; without
	// one there is nothing to upgrade to.
	Checker *update.Checker
	// Key is the release public key file; nil is update.ReleaseKey.
	Key []byte
	// InstallType is update.InstallType unless a test says otherwise.
	InstallType func() string
	// SetQuiet turns quiet mode on or off, Counts reads it. They are the
	// dispatcher's.
	SetQuiet func(ctx context.Context, on bool) (Counts, error)
	Counts   func(ctx context.Context) (Counts, error)
	// Switch is called once the pending record is written: the server is
	// to stop and start again, which is the launcher's cue.
	Switch func()
	// Free tells the free bytes of a directory's disk; nil is diskfree.Free.
	Free func(string) (uint64, bool)
	// OS and Arch name the archive to download; empty is this machine's.
	OS, Arch string
	// Poll is how often the wait looks at the builds.
	Poll time.Duration
	Now  func() time.Time
	Log  *slog.Logger
}

// Engine runs one upgrade at a time.
type Engine struct {
	o Options

	mu   sync.Mutex
	run  *active
	last string // why the last run ended in failure
}

// active is the run in progress.
type active struct {
	cancel context.CancelFunc
	done   chan struct{}
	// committed is set when the backup begins: from then on only a
	// rollback undoes it, so it can no longer be cancelled.
	committed bool
}

// New makes an Engine.
func New(o Options) *Engine {
	if o.InstallType == nil {
		o.InstallType = update.InstallType
	}
	if o.Free == nil {
		o.Free = diskfree.Free
	}
	if o.Poll <= 0 {
		o.Poll = 2 * time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Key == nil {
		o.Key = update.ReleaseKey()
	}
	return &Engine{o: o}
}

// Request starts an upgrade or a rollback.
type Request struct {
	// Version, when given, must be the latest release (an upgrade only).
	Version string
	// Safe waits for the builds to end first. Without it, ConfirmLost
	// says the builds that run will end as lost.
	Safe        bool
	ConfirmLost bool
	// WaitTimeout bounds the wait; zero is DefaultWait.
	WaitTimeout time.Duration
}

// Info is the upgrade as it stands.
type Info struct {
	// Status is the progress; its State is empty before any upgrade.
	Status state.Status
	// Error is why the last run of this process ended without switching.
	Error string
	// Failure is why the launcher went back to an earlier version.
	Failure *state.Failure
	// Latest is the newer release the update check knows, and Upgradable
	// whether it can be upgraded to.
	Latest     string
	Upgradable bool
	// Unavailable is why an upgrade cannot start now, empty when it can.
	Unavailable string
	// RollbackTo is the version a rollback returns to, empty when there is
	// none to return to; RollbackBackup is the backup it restores.
	RollbackTo, RollbackBackup string
}

// Info reads the progress, the last failure and what can be done now.
func (e *Engine) Info(ctx context.Context) (Info, error) {
	var in Info
	st, err := state.ReadStatus(e.o.Data)
	if err != nil {
		return in, err
	}
	if st != nil {
		in.Status = *st
	}
	if in.Failure, err = state.ReadFailure(e.o.Data); err != nil {
		return in, err
	}
	e.mu.Lock()
	in.Error = e.last
	busy := e.run != nil
	e.mu.Unlock()
	if in.Status.State == state.StateWaiting && e.o.Counts != nil {
		if c, err := e.o.Counts(ctx); err == nil {
			in.Status.Running, in.Status.Queued = c.Running, c.Queued
		}
	}
	if e.o.Checker != nil {
		s := e.o.Checker.Status()
		in.Latest, in.Upgradable = s.Latest, s.Upgradable
	}
	in.Unavailable = e.unavailable(busy, in.Latest, in.Upgradable)
	in.RollbackTo, in.RollbackBackup = e.rollbackTarget()
	return in, nil
}

func (e *Engine) unavailable(busy bool, latest string, upgradable bool) string {
	switch {
	case e.o.InstallType() == update.Dev:
		return "not available in a development build"
	case pinned():
		return pinnedText
	case e.o.Checker == nil:
		return "the update check is off, so there is no release to upgrade to"
	case busy:
		return "an upgrade is already running"
	case latest == "":
		return "this is the latest release"
	case !upgradable:
		return latest + " has no signature file, so kipp does not upgrade to it"
	}
	return ""
}

// rollbackTarget is the version and the backup a rollback would use, empty
// when it cannot be done: nothing before this version, or no backup of an
// upgrade that came up.
func (e *Engine) rollbackTarget() (to, bk string) {
	prev, err := state.ReadPrevious(e.o.Data)
	if err != nil || prev == "" || e.o.InstallType() == update.Dev || pinned() {
		return "", ""
	}
	name, ok := backup.LastSucceeded(e.o.Data)
	if !ok {
		return "", ""
	}
	return prev, name
}

// Resume is called when a server starts. An upgrade that was switching to
// this version has come up, which is "done"; one that was still at an
// earlier step when the process ended did not finish.
func (e *Engine) Resume() {
	st, err := state.ReadStatus(e.o.Data)
	if err != nil || st == nil {
		return
	}
	switch st.State {
	case state.StateSwitching:
		if st.Version != e.o.Version {
			return // the launcher's to settle: it writes failed on a rollback
		}
		st.State = state.StateDone
	case state.StatePreflight, state.StateDownloading, state.StateVerifying, state.StateWaiting, state.StateBackup:
		st.State = state.StateFailed
	default:
		return
	}
	e.writeStatus(*st)
}

func (e *Engine) writeStatus(st state.Status) {
	st.UpdatedAt = e.o.Now().UTC()
	if err := state.WriteStatus(e.o.Data, st); err != nil {
		e.o.Log.Warn("not writing the upgrade status", "err", err)
	}
}

// begin takes the one place for a run and writes its first status. It
// returns the run's context.
func (e *Engine) begin(st state.Status, steps func(context.Context, *active) error) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &active{cancel: cancel, done: make(chan struct{})}
	e.run, e.last = r, ""
	e.writeStatus(st)
	go func() {
		err := steps(ctx, r)
		e.finish(ctx, r, st, err)
	}()
}

// finish records how a run ended. A run that switched stays as it is: the
// process is about to end.
func (e *Engine) finish(ctx context.Context, r *active, st state.Status, err error) {
	defer close(r.done)
	switch {
	case err == nil:
		return
	case errors.Is(err, context.Canceled) && ctx.Err() != nil:
		st.State = state.StateCancelled
	default:
		st.State = state.StateFailed
		e.o.Log.Warn("the upgrade failed", "version", st.Version, "err", err)
	}
	// One critical section, so that a reader who sees the failed status
	// also sees why, and a new run cannot be overwritten by this one's end.
	e.mu.Lock()
	defer e.mu.Unlock()
	if st.State == state.StateFailed {
		e.last = err.Error()
	}
	e.writeStatus(st)
	e.run = nil
}

// Cancel ends the run in progress and leaves the server as it was. It is
// possible until the backup begins.
func (e *Engine) Cancel(ctx context.Context) (Info, error) {
	e.mu.Lock()
	r := e.run
	switch {
	case r == nil:
		e.mu.Unlock()
		return Info{}, conflict("no upgrade is running")
	case r.committed:
		e.mu.Unlock()
		return Info{}, conflict("the upgrade is past the point where it can be cancelled; only a rollback undoes it now")
	}
	r.cancel()
	e.mu.Unlock()
	select {
	case <-r.done:
	case <-time.After(cancelWait):
	case <-ctx.Done():
	}
	return e.Info(ctx)
}

// commit marks the run as past cancelling; false when it was cancelled.
func (e *Engine) commit(ctx context.Context, r *active) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ctx.Err() != nil {
		return false
	}
	r.committed = true
	return true
}

// Start begins an upgrade to the latest release and returns at once; its
// progress is Info.
func (e *Engine) Start(ctx context.Context, req Request) (Info, error) {
	e.mu.Lock()
	err := e.startLocked(ctx, req)
	e.mu.Unlock()
	if err != nil {
		return Info{}, err
	}
	return e.Info(ctx)
}

func (e *Engine) startLocked(ctx context.Context, req Request) error {
	if e.o.InstallType() == update.Dev {
		return conflict("not available in a development build")
	}
	if pinned() {
		return conflict("%s", pinnedText)
	}
	if e.run != nil {
		return conflict("an upgrade is already running")
	}
	if e.o.Checker == nil {
		return conflict("the update check is off, so there is no release to upgrade to")
	}
	latest := e.o.Checker.Status()
	switch {
	case req.Version != "" && req.Version != latest.Latest:
		return badRequest("version must be the latest release (%s), not %q", orNone(latest.Latest), req.Version)
	case latest.Latest == "":
		return conflict("this is the latest release")
	case !latest.Upgradable:
		return conflict("%s has no signature file, so kipp does not upgrade to it", latest.Latest)
	}
	if err := e.ready(ctx, req, latest.Latest); err != nil {
		return err
	}
	st := state.Status{State: state.StatePreflight, Version: latest.Latest, From: e.o.Version}
	e.begin(st, func(ctx context.Context, r *active) error { return e.upgrade(ctx, r, req, st) })
	return nil
}

// ready is what both kinds of request need: no record waiting, a place to
// work, and the confirmation of builds lost when nothing waits for them.
func (e *Engine) ready(ctx context.Context, req Request, latest string) error {
	if err := e.canWork(latest); err != nil {
		return err
	}
	if p, err := state.ReadPending(e.o.Data); err != nil {
		return err
	} else if p != nil {
		return conflict("an earlier switch is still pending; start the server again first")
	}
	if req.Safe || req.ConfirmLost {
		return nil
	}
	c, err := e.o.Counts(ctx)
	if err != nil {
		return err
	}
	return badRequest("without a safe restart %d running builds and %d queued ones end as lost; confirm with confirm_lost (--yes)", c.Running, c.Queued)
}

func orNone(v string) string {
	if v == "" {
		return "none"
	}
	return v
}

// Rollback returns to the version before, with the data as the last
// upgrade's backup had it: anything written since the upgrade is lost.
func (e *Engine) Rollback(ctx context.Context, req Request) (Info, error) {
	e.mu.Lock()
	err := e.rollbackLocked(ctx, req)
	e.mu.Unlock()
	if err != nil {
		return Info{}, err
	}
	return e.Info(ctx)
}

func (e *Engine) rollbackLocked(ctx context.Context, req Request) error {
	if e.o.InstallType() == update.Dev {
		return conflict("not available in a development build")
	}
	if pinned() {
		return conflict("%s", pinnedText)
	}
	if e.run != nil {
		return conflict("an upgrade is already running")
	}
	to, bk := e.rollbackTarget()
	if to == "" {
		return conflict("there is nothing to go back to: no earlier version, or no backup of the last upgrade")
	}
	if err := e.ready(ctx, req, to); err != nil {
		return err
	}
	st := state.Status{State: state.StatePreflight, Version: e.resolve(to), From: e.o.Version, Backup: bk}
	e.begin(st, func(ctx context.Context, r *active) error { return e.rollback(ctx, r, req, st, to) })
	return nil
}

// resolve is the version a server started from v says it is: bundled is
// the executable the launcher replaced.
func (e *Engine) resolve(v string) string {
	if v == state.Bundled {
		if b := os.Getenv(launch.EnvBundledVersion); update.Release(b) {
			return b
		}
	}
	return v
}
