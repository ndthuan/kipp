package dispatch

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

// lockHolder is who holds a lock: a build from its start to the end of its
// hooks, when stage is empty, or one stage of a build while it runs. Job
// locks and stage locks are one set of names, and a name is held once.
type lockHolder struct {
	build int64
	stage string
}

// stageLockWait is a stage that waits to take its locks: all of them, and
// the one held now.
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

// stageLocks takes every lock the stage names, all at once, before it is
// given an executor or a place: while one is held it waits, holding none of
// them and nothing else. A lock its build holds as a job lock it has
// already. The func gives them back. A status is how the stage ends
// without running: cancelled when the build is stopped while it waits,
// failed when the wait would never end.
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

// sayLockWait records that the stage waits for lock: the first time it
// waits it reads running, as a stage that waits to be placed does, and
// every change of the lock is a line of its log.
func (d *Dispatcher) sayLockWait(ctx context.Context, b *store.Build, e *buildEnv, stage, lock string, first bool) {
	if first {
		d.setStage(ctx, b, stage, store.StageRunning, nil)
	}
	d.note(b, stage, e, "kipp: "+LockWaitLine(lock))
	d.changed()
}

// unheldOfLocked is the locks of names the build does not hold as job
// locks. mu is held.
func (d *Dispatcher) unheldOfLocked(build int64, names []string) []string {
	var out []string
	for _, l := range names {
		if d.locks[l] != (lockHolder{build: build}) {
			out = append(out, l)
		}
	}
	return out
}

// heldOfLocked is the first of names somebody holds, or "". mu is held.
func (d *Dispatcher) heldOfLocked(names []string) string {
	for _, l := range names {
		if _, held := d.locks[l]; held {
			return l
		}
	}
	return ""
}

// lockWaitLocked records that the stage waits for lock, and counts each of
// its locks as wanted, which keeps a queued build from taking it first.
// mu is held.
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

// endLockWaitLocked clears what lockWaitLocked recorded. mu is held.
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

// giveStageLocks gives back the locks a stage took, and wakes what waits
// for them: the stages first, then the loop.
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

// lockFreedLocked wakes every stage that waits for a lock. mu is held.
func (d *Dispatcher) lockFreedLocked() {
	close(d.lockFreed)
	d.lockFreed = make(chan struct{})
}

// lockCycleLocked is a build that holds, as a job lock, a lock a stage of
// build waits for, and that waits itself, through more such holds, for a
// lock build holds as a job lock: neither would ever end. A stage lock
// makes no such wait, since the stage that holds it runs to its end. nil
// is none. mu is held.
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

// reaches reports whether edges lead from one build to another.
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

// lockBlockerLocked is the build that keeps a queued build from taking
// lock: the one that holds it, or else the oldest build with a stage that
// waits for it; nil when neither is running. mu is held.
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

// holderLocked is a copy of the running build that holds lock, nil when
// none does. mu is held.
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

// lockWaitsOfLocked is what each stage of the build that waits for a lock
// waits for, nil when none does. mu is held.
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
