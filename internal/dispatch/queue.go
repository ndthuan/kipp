package dispatch

import (
	"context"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

// WaitReason is why a queued build has not started.
type WaitReason string

// The reasons, in the order the scheduler applies its rules.
const (
	// WaitGone: its job is no longer in the job files, or is disabled; the
	// next pass over the queue fails it.
	WaitGone WaitReason = "gone"
	// WaitJob: a build of the same job is ahead of it in the queue, or
	// runs, and the job does not run its builds in parallel.
	WaitJob WaitReason = "job"
	// WaitLock: another build holds one of the job's locks.
	WaitLock WaitReason = "lock"
	// WaitExecutor: every slot is taken, or a started stage that waits for
	// one is served first.
	WaitExecutor WaitReason = "executor"
	// WaitNothing: nothing holds it back; the next pass starts it.
	WaitNothing WaitReason = "nothing"
)

// Queue is what runs and what waits, at one moment.
type Queue struct {
	// Running builds, oldest first.
	Running []RunningBuild
	// Queued builds in the order the scheduler takes them, oldest first.
	Queued []QueuedBuild
	// Executors is the limit, and Busy how many of them are in use.
	Executors, Busy int
}

// RunningBuild is a build that runs, with the stages it runs now, and
// what each of them that waits to be placed on a runner or an engine
// waits for.
type RunningBuild struct {
	Build   *store.Build
	Stages  []string
	Waiting map[string]string
}

// QueuedBuild is a build that waits, and why. Blocker is the build it waits
// for, under WaitJob and WaitLock; Lock is the lock, under WaitLock.
type QueuedBuild struct {
	Build   *store.Build
	Waiting WaitReason
	Lock    string
	Blocker *store.Build
}

// Changes is a channel closed at the next change to what runs or waits: a
// build queued, started, ended or cancelled, a stage started or ended, or
// a new job set. Ask again for the change after that.
func (d *Dispatcher) Changes() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.changes
}

// changed announces a change. mu must not be held.
func (d *Dispatcher) changed() {
	d.mu.Lock()
	close(d.changes)
	d.changes = make(chan struct{})
	d.mu.Unlock()
}

// Queue reads what runs and what waits. Why a build waits is read from
// the dispatcher's own state at the moment of asking, by the rules the
// scheduler applies, so it can be a step behind a change being made.
func (d *Dispatcher) Queue(ctx context.Context) (*Queue, error) {
	builds, err := d.st.BuildsWithStatus(ctx, store.Queued, store.Running)
	if err != nil {
		return nil, err
	}
	q := &Queue{Executors: cap(d.slots)}
	var queued []*store.Build
	for _, b := range builds {
		if b.Status == store.Queued {
			queued = append(queued, b)
			continue
		}
		stages, err := d.st.Stages(ctx, b.ID)
		if err != nil {
			return nil, err
		}
		r := RunningBuild{Build: b, Stages: []string{}}
		for _, s := range stages {
			if s.Status == store.StageRunning {
				r.Stages = append(r.Stages, s.Name)
			}
		}
		q.Running = append(q.Running, r)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := range q.Running {
		q.Running[i].Waiting = d.waitingOfLocked(q.Running[i].Build.ID)
	}
	q.Busy = len(d.slots)
	ahead := map[string]*store.Build{} // the first queued build of each job
	for _, b := range queued {
		q.Queued = append(q.Queued, d.whyWaitingLocked(b, ahead))
		if ahead[b.Job] == nil {
			ahead[b.Job] = b
		}
	}
	return q, nil
}

// whyWaitingLocked applies mayStartLocked's rules and the executor limit
// to b, and says which one holds it back. ahead is the first build of each
// job queued before b.
func (d *Dispatcher) whyWaitingLocked(b *store.Build, ahead map[string]*store.Build) QueuedBuild {
	w := QueuedBuild{Build: b, Waiting: WaitNothing}
	job := d.snap.Jobs[b.Job]
	if job == nil || job.Disabled {
		w.Waiting = WaitGone
		return w
	}
	// One of the job's builds ahead in the queue starts before b does,
	// whatever runs now, so that is the build b waits for.
	if job.Concurrency != config.Parallel {
		if a := ahead[b.Job]; a != nil {
			w.Waiting, w.Blocker = WaitJob, a
			return w
		}
		if r := d.runningOfLocked(b.Job); r != nil {
			w.Waiting, w.Blocker = WaitJob, copyOf(r)
			return w
		}
	}
	for _, l := range job.Locks {
		if id, held := d.locks[l]; held {
			w.Waiting, w.Lock = WaitLock, l
			if r := d.running[id]; r != nil {
				w.Blocker = copyOf(r.build)
			}
			return w
		}
	}
	if len(d.slots) == cap(d.slots) || d.slotWaiters > 0 {
		w.Waiting = WaitExecutor
	}
	return w
}

// runningOfLocked is the oldest build of the job the dispatcher holds, or nil.
func (d *Dispatcher) runningOfLocked(job string) *store.Build {
	var oldest *store.Build
	for _, r := range d.running {
		if r.build.Job == job && (oldest == nil || r.build.ID < oldest.ID) {
			oldest = r.build
		}
	}
	return oldest
}

// copyOf is a copy of a build the dispatcher holds, for a caller to read
// without its lock.
func copyOf(b *store.Build) *store.Build {
	c := *b
	return &c
}
