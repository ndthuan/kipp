package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

// The states of a compact.
const (
	CompactIdle       = "idle"       // none has run since the server started
	CompactWaiting    = "waiting"    // quiet, waiting for the builds to end
	CompactCompacting = "compacting" // VACUUM runs
	CompactDone       = "done"
	CompactFailed     = "failed"
)

// ErrCompactBusy refuses a compact while one runs.
var ErrCompactBusy = errors.New("a compact of the database is already running")

// ErrStopped refuses a compact once the server is stopping, and ends one
// that was running then.
var ErrStopped = errors.New("the server stopped; the compact did not finish")

// RoomError refuses a compact the disk has no room for.
type RoomError struct {
	Dir        string
	Free, Need int64
	// Copies is how many copies of the database Need counts on that disk.
	Copies int
}

func (e *RoomError) Error() string {
	copies := "copies"
	if e.Copies == 1 {
		copies = "copy"
	}
	return fmt.Sprintf("the disk of %s has %s free and a compact needs %s there (%d %s of the database in use: SQLite's temporary copy and the write-ahead log); free %s first",
		e.Dir, config.Readable(e.Free), config.Readable(e.Need), e.Copies, copies, config.Readable(e.Need-e.Free))
}

// CompactState is the compact that runs or ran last.
type CompactState struct {
	State string
	// By is who started it.
	By                 string
	StartedAt, EndedAt time.Time
	// Running and Queued are the builds it waits for.
	Running, Queued int
	// Before and After are the bytes of the file and its log.
	Before, After int64
	// Incremental says it turned incremental auto_vacuum on.
	Incremental bool
	Error       string
	// HoldsQuiet says it keeps the server quiet now.
	HoldsQuiet bool
}

func (c CompactState) active() bool { return c.State == CompactWaiting || c.State == CompactCompacting }

// CompactState is the compact as it stands.
func (s *Service) CompactState() CompactState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.compact
}

func (s *Service) setCompact(f func(*CompactState)) {
	s.mu.Lock()
	f(&s.compact)
	s.changedLocked()
	s.mu.Unlock()
}

// room checks that the disks have room for what VACUUM writes: a copy of
// the pages in use in the temporary directory, and another through the
// write-ahead log beside the database. A disk whose free space cannot be
// read does not stop it.
func (s *Service) room(ctx context.Context) error {
	f, err := s.o.Store.DBFile(ctx)
	if err != nil {
		return err
	}
	used := f.UsedBytes()
	type disk struct {
		dir    string
		copies int
	}
	disks := []disk{{s.o.Data, 2}}
	if !s.o.SameDisk(s.o.Data, s.o.TempDir) {
		disks = []disk{{s.o.Data, 1}, {s.o.TempDir, 1}}
	}
	for _, d := range disks {
		if free, _, known := s.space(d.dir); known && free < used*int64(d.copies) {
			return &RoomError{Dir: d.dir, Free: free, Need: used * int64(d.copies), Copies: d.copies}
		}
	}
	return nil
}

// StartCompact begins a compact for actor, unless one runs or the disk has
// no room for it, and returns at once; Changes and CompactState follow it.
func (s *Service) StartCompact(ctx context.Context, actor string) (CompactState, error) {
	s.mu.Lock()
	if s.stopped {
		c := s.compact
		s.mu.Unlock()
		return c, ErrStopped
	}
	if s.compact.active() {
		c := s.compact
		s.mu.Unlock()
		return c, ErrCompactBusy
	}
	prev := s.compact
	s.compact = CompactState{State: CompactWaiting, By: actor, StartedAt: s.o.Now()}
	s.changedLocked()
	s.wg.Add(1)
	s.mu.Unlock()
	if err := s.room(ctx); err != nil {
		s.wg.Done()
		s.setCompact(func(c *CompactState) { *c = prev })
		s.audit(ctx, actor, "refused: "+err.Error())
		return s.CompactState(), err
	}
	s.apart(func(ctx context.Context) { s.runCompact(ctx, actor) })
	return s.CompactState(), nil
}

func (s *Service) audit(ctx context.Context, actor, detail string) {
	err := s.o.Store.AppendAudit(ctx, store.Audit{At: s.o.Now(), Actor: actor, Action: "db.compact", Target: "db.sqlite", Detail: detail})
	if err != nil {
		s.o.Log.Error("writing the audit of a compact", "err", err)
	}
}

// runCompact is the compact: quiet, the wait, the room again, VACUUM, then
// quiet off unless it was on before; a failure, or a Stop, ends the same
// way.
func (s *Service) runCompact(ctx context.Context, actor string) {
	before, err := s.o.Counts(ctx)
	wasQuiet := before.Quiet
	var res store.Compacted
	if err == nil {
		res, err = s.compactQuiet(ctx)
	}
	if err != nil && ctx.Err() != nil {
		err = ErrStopped
	}
	// What follows records the end, which a stop must not cut short.
	ctx = context.WithoutCancel(ctx)
	if !wasQuiet {
		if _, qerr := s.o.SetQuiet(ctx, false); qerr != nil {
			s.o.Log.Error("quiet could not be turned off after the compact", "err", qerr)
			err = errors.Join(err, qerr)
		}
	}
	end := s.o.Now()
	s.setCompact(func(c *CompactState) {
		c.EndedAt, c.HoldsQuiet, c.Running, c.Queued = end, false, 0, 0
		c.Before, c.After, c.Incremental = res.Before, res.After, res.Incremental
		c.State = CompactDone
		if err != nil {
			c.State, c.Error = CompactFailed, err.Error()
		}
	})
	took := end.Sub(s.CompactState().StartedAt).Round(time.Second)
	if err != nil {
		s.o.Log.Error("compact of the database failed; quiet is off", "err", err)
		s.audit(ctx, actor, "failed: "+err.Error())
		return
	}
	detail := fmt.Sprintf("%s to %s in %s", config.Readable(res.Before), config.Readable(res.After), took)
	if res.Incremental {
		detail += "; incremental auto_vacuum on"
	}
	s.o.Log.Info("database compacted", "before", res.Before, "after", res.After, "took", took)
	s.audit(ctx, actor, detail)
	_ = s.Measure(ctx)
}

// compactQuiet makes the server quiet, waits for its builds, checks the
// room again and compacts.
func (s *Service) compactQuiet(ctx context.Context) (store.Compacted, error) {
	c, err := s.o.SetQuiet(ctx, true)
	if err != nil {
		return store.Compacted{}, err
	}
	s.setCompact(func(st *CompactState) { st.HoldsQuiet = true })
	deadline := s.o.Now().Add(CompactWait)
	for c.Running+c.Queued > 0 {
		s.setCompact(func(st *CompactState) { st.Running, st.Queued = c.Running, c.Queued })
		if !s.o.Now().Before(deadline) {
			return store.Compacted{}, fmt.Errorf("still busy after %s (%d running, %d queued); nothing was compacted", CompactWait, c.Running, c.Queued)
		}
		select {
		case <-ctx.Done():
			return store.Compacted{}, ctx.Err()
		case <-time.After(s.o.Poll):
		}
		if c, err = s.o.Counts(ctx); err != nil {
			return store.Compacted{}, err
		}
	}
	if err := s.room(ctx); err != nil {
		return store.Compacted{}, err
	}
	s.setCompact(func(st *CompactState) { st.State, st.Running, st.Queued = CompactCompacting, 0, 0 })
	return s.o.Store.Compact(ctx)
}
