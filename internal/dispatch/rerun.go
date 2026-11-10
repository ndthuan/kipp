package dispatch

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ndthuan/kipp/internal/store"
)

// A build of a job with rerun_on_restart, or one an admin chose for this
// quiet, does not hold a safe restart back: the restart stops it as a cancel
// does, and the next start queues it again with its params and cause. A
// build of any other job is waited for, so that a deploy halfway is never
// killed by a guess.

var (
	// ErrNotQuiet is a build chosen to run again after a restart while
	// the server is not quiet: no restart is on its way.
	ErrNotQuiet = errors.New("kipp is not quiet, so no restart is waiting")
	// ErrNotRunning is a build chosen to run again after a restart that
	// does not run, or is already finishing.
	ErrNotRunning = errors.New("is not running")
)

// stoppedForRestart is why a build's context ended when a restart stopped
// it to run it again after the start.
type stoppedForRestart struct{}

func (stoppedForRestart) Error() string { return "stopped for a restart, runs again after it" }

// rerunsLocked reports whether a restart stops the running build and runs
// it again. One whose status is decided cannot be stopped any more, and is
// waited for.
func (d *Dispatcher) rerunsLocked(id int64, r *run) bool {
	return !r.settled && (r.job.RerunOnRestart || d.chosen[id])
}

// ChooseRerun has the restart that quiet waits for stop the running build
// id and run it again after the start, as rerun_on_restart does, for this
// quiet only: turning quiet off forgets it.
func (d *Dispatcher) ChooseRerun(id int64) (BuildRef, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.quiet.IsZero() {
		return BuildRef{}, ErrNotQuiet
	}
	r := d.running[id]
	if r == nil || r.settled {
		return BuildRef{}, ErrNotRunning
	}
	if d.chosen == nil {
		d.chosen = map[int64]bool{}
	}
	d.chosen[id] = true
	close(d.changes) // what was counted as running now counts to run again
	d.changes = make(chan struct{})
	return BuildRef{ID: id, Job: r.build.Job, Number: r.build.Number}, nil
}

// StopForRestart is called once the server is about to stop. While it is
// quiet, it stops each build a restart runs again, records it to be queued
// again at the next start (Rerun), and returns once they have ended or ctx
// has. Without quiet it does nothing: a restart nobody prepared loses what
// runs, as before.
func (d *Dispatcher) StopForRestart(ctx context.Context) []BuildRef {
	d.mu.Lock()
	if d.quiet.IsZero() {
		d.mu.Unlock()
		return nil
	}
	var runs []*run
	for id, r := range d.running {
		if d.rerunsLocked(id, r) {
			runs = append(runs, r)
		}
	}
	d.mu.Unlock()
	slices.SortFunc(runs, func(a, b *run) int { return cmp.Compare(a.build.ID, b.build.ID) })
	var stopped []BuildRef
	var ended []chan struct{}
	for _, r := range runs {
		if ch := d.stopToRunAgain(ctx, r); ch != nil {
			ended = append(ended, ch)
			stopped = append(stopped, BuildRef{ID: r.build.ID, Job: r.build.Job, Number: r.build.Number})
		}
	}
	for _, ch := range ended {
		select {
		case <-ch:
		case <-ctx.Done():
			return stopped
		}
	}
	return stopped
}

// stopToRunAgain records the build to be queued again at the next start
// and stops it. It returns what is closed once the build has ended, nil
// when it was not stopped: it could not be recorded (it then ends as lost,
// as before), or it had ended or was finishing already.
func (d *Dispatcher) stopToRunAgain(ctx context.Context, r *run) chan struct{} {
	b := r.build
	if err := d.st.KeepRerun(ctx, b.ID); err != nil {
		d.log.Error("a build to run again after the restart was not recorded; it ends as lost", "job", b.Job, "number", b.Number, "err", err)
		return nil
	}
	ch := d.subscribe(b.ID)
	if !d.stopForRestart(r) {
		d.unsubscribe(b.ID, ch)
		if err := d.st.DropRerun(ctx, b.ID); err != nil {
			d.log.Error("forgetting a build that ended before the restart stopped it", "job", b.Job, "number", b.Number, "err", err)
		}
		return nil
	}
	d.activity(ctx, b.Job, fmt.Sprintf("#%d stopped for a restart, runs again after it", b.Number))
	d.log.Info("stopped for a restart, runs again after it", "job", b.Job, "number", b.Number)
	return ch
}

// stopForRestart cancels the run, unless its status is decided already.
func (d *Dispatcher) stopForRestart(r *run) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r.settled || d.running[r.build.ID] != r {
		return false
	}
	r.cancel(stoppedForRestart{})
	return true
}

// Rerun queues again, at start and before anything else is queued, each
// build the last process stopped for its restart, in the order they were
// first queued, and links each to the build that runs it again. One that
// cannot be queued (its job is gone or disabled) is said on its job's
// activity and forgotten. It returns the builds it queued.
func (d *Dispatcher) Rerun(ctx context.Context) []*store.Build {
	pending, err := d.st.PendingReruns(ctx)
	if err != nil {
		d.log.Error("reading the builds to run again after the restart", "err", err)
		return nil
	}
	var out []*store.Build
	for _, o := range pending {
		req := d.againRequest(o, o.Trigger, o.TriggerDetail, o.StartedBy)
		req.UpstreamStatus, req.RerunOf = o.UpstreamStatus, o.Number
		b, err := d.Trigger(ctx, req)
		if err != nil {
			d.log.Warn("a build stopped for the restart was not run again", "job", o.Job, "number", o.Number, "err", err)
			d.activity(ctx, o.Job, fmt.Sprintf("#%d, stopped for a restart, was not run again: %v", o.Number, err))
			if err := d.st.DropRerun(ctx, o.ID); err != nil {
				d.log.Error("forgetting a build that was not run again", "job", o.Job, "number", o.Number, "err", err)
			}
			continue
		}
		if err := d.st.LinkRerun(ctx, o.ID, b.Number); err != nil {
			d.log.Error("linking a build to the one that runs it again", "job", o.Job, "number", o.Number, "err", err)
		}
		d.activity(ctx, o.Job, fmt.Sprintf("#%d runs #%d again, stopped for the restart", b.Number, o.Number))
		out = append(out, b)
	}
	return out
}

// activity says line on the job's activity, and logs a failure to.
func (d *Dispatcher) activity(ctx context.Context, job, line string) {
	if err := d.st.AddActivity(ctx, job, line, time.Now()); err != nil {
		d.log.Error("recording a job's activity", "job", job, "err", err)
	}
}
