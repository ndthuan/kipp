package dispatch

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

type lockHolder struct {
	build int64
	stage string
}

type stageLockWait struct {
	locks []string
	lock  string
}

// StageLockWait is what a stage that waits for a lock waits for: the lock,
// and the build that holds it, nil in the moment it is free and not yet
// taken.
type StageLockWait struct {
	Lock    string
	Blocker *store.Build
}

// LockWaitLine is what a stage that waits for lock is said to wait for.
func LockWaitLine(lock string) string { return "waiting for lock " + lock }

func (d *Dispatcher) stageLocks(ctx context.Context, b *store.Build, e *buildEnv, s *config.Stage) (func(), store.StageStatus) {
	if len(s.Locks) == 0 {
		return func() {}, ""
	}
	said := ""
	for {
		d.mu.Lock()
		want := d.unheldOfLocked(b.ID, s.Locks)
		lock := d.heldOfLocked(want)
		if lock == "" {
			for _, l := range want {
				d.locks[l] = lockHolder{b.ID, s.Name}
			}
			d.endLockWaitLocked(b.ID, s.Name)
			d.mu.Unlock()
			d.changed()
			return func() { d.giveStageLocks(b.ID, s.Name, want) }, ""
		}
		d.lockWaitLocked(b.ID, s.Name, want, lock)
		if other := d.lockCycleLocked(b.ID); other != nil {
			d.endLockWaitLocked(b.ID, s.Name)
			d.mu.Unlock()
			d.changed()
			d.note(b, s.Name, e, fmt.Sprintf("kipp: lock %s is held by %s #%d, which waits for a lock this build holds: neither would ever end", lock, other.Job, other.Number))
			return nil, store.StageFailed
		}
		freed := d.lockFreed
		d.mu.Unlock()
		if lock != said {
			d.sayLockWait(ctx, b, e, s.Name, lock, said == "")
			said = lock
		}
		select {
		case <-freed:
		case <-ctx.Done():
			d.mu.Lock()
			d.endLockWaitLocked(b.ID, s.Name)
			d.mu.Unlock()
			d.changed()
			_, why := stopped(ctx)
			d.note(b, s.Name, e, why)
			return nil, store.StageCancelled
		}
	}
}

func (d *Dispatcher) sayLockWait(ctx context.Context, b *store.Build, e *buildEnv, stage, lock string, first bool) {
	if first {
		d.setStage(ctx, b, stage, store.StageRunning, nil)
	}
	d.note(b, stage, e, "kipp: "+LockWaitLine(lock))
	d.changed()
}

func (d *Dispatcher) unheldOfLocked(build int64, names []string) []string {
	var out []string
	for _, l := range names {
		if d.locks[l] != (lockHolder{build: build}) {
			out = append(out, l)
		}
	}
	return out
}

func (d *Dispatcher) heldOfLocked(names []string) string {
	for _, l := range names {
		if _, held := d.locks[l]; held {
			return l
		}
	}
	return ""
}

func (d *Dispatcher) lockWaitLocked(build int64, stage string, locks []string, lock string) {
	w := d.lockWaits[build]
	if w == nil {
		w = map[string]stageLockWait{}
		d.lockWaits[build] = w
	}
	if _, waits := w[stage]; !waits {
		for _, l := range locks {
			d.lockWanted[l]++
		}
	}
	w[stage] = stageLockWait{locks: locks, lock: lock}
}

func (d *Dispatcher) endLockWaitLocked(build int64, stage string) {
	w, waits := d.lockWaits[build][stage]
	if !waits {
		return
	}
	for _, l := range w.locks {
		if d.lockWanted[l]--; d.lockWanted[l] <= 0 {
			delete(d.lockWanted, l)
		}
	}
	delete(d.lockWaits[build], stage)
	if len(d.lockWaits[build]) == 0 {
		delete(d.lockWaits, build)
	}
}

func (d *Dispatcher) giveStageLocks(build int64, stage string, locks []string) {
	d.mu.Lock()
	for _, l := range locks {
		if d.locks[l] == (lockHolder{build, stage}) {
			delete(d.locks, l)
		}
	}
	d.lockFreedLocked()
	d.mu.Unlock()
	d.kick()
}

func (d *Dispatcher) lockFreedLocked() {
	close(d.lockFreed)
	d.lockFreed = make(chan struct{})
}

func (d *Dispatcher) lockCycleLocked(build int64) *store.Build {
	edges := map[int64][]int64{}
	for w, stages := range d.lockWaits {
		for _, sw := range stages {
			for _, l := range sw.locks {
				if h, held := d.locks[l]; held && h.stage == "" && h.build != w {
					edges[w] = append(edges[w], h.build)
				}
			}
		}
	}
	for _, next := range edges[build] {
		if r := d.running[next]; r != nil && reaches(edges, next, build) {
			return copyOf(r.build)
		}
	}
	return nil
}

func reaches(edges map[int64][]int64, from, to int64) bool {
	seen := map[int64]bool{}
	todo := []int64{from}
	for len(todo) > 0 {
		at := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		if at == to {
			return true
		}
		if !seen[at] {
			seen[at] = true
			todo = append(todo, edges[at]...)
		}
	}
	return false
}

func (d *Dispatcher) lockBlockerLocked(lock string) *store.Build {
	if _, held := d.locks[lock]; held {
		return d.holderLocked(lock)
	}
	for _, id := range slices.Sorted(maps.Keys(d.lockWaits)) {
		for _, w := range d.lockWaits[id] {
			if r := d.running[id]; r != nil && slices.Contains(w.locks, lock) {
				return copyOf(r.build)
			}
		}
	}
	return nil
}

func (d *Dispatcher) holderLocked(lock string) *store.Build {
	h, held := d.locks[lock]
	if !held {
		return nil
	}
	if r := d.running[h.build]; r != nil {
		return copyOf(r.build)
	}
	return nil
}

func (d *Dispatcher) lockWaitsOfLocked(build int64) map[string]StageLockWait {
	w := d.lockWaits[build]
	if len(w) == 0 {
		return nil
	}
	out := make(map[string]StageLockWait, len(w))
	for stage, sw := range w {
		out[stage] = StageLockWait{Lock: sw.lock, Blocker: d.holderLocked(sw.lock)}
	}
	return out
}

// LockWaits is what each stage of the build that waits for a lock waits
// for, nil when none does.
func (d *Dispatcher) LockWaits(build int64) map[string]StageLockWait {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lockWaitsOfLocked(build)
}
