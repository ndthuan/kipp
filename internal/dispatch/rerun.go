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

var (
	// ErrNotQuiet is a build chosen to run again after a restart while
	// the server is not quiet: no restart is on its way.
	ErrNotQuiet = errors.New("kipp is not quiet, so no restart is waiting")
	// ErrNotRunning is a build chosen to run again after a restart that
	// does not run, is being stopped (cancelled, superseded, its branch
	// gone), or is already finishing.
	ErrNotRunning = errors.New("is not running")
)

type stoppedForRestart struct{}

func (stoppedForRestart) Error() string { return "stopped for a restart, runs again after it" }

func (d *Dispatcher) rerunsLocked(id int64, r *run) bool {
	return !r.settled && !r.stopped && (r.job.RerunOnRestart || d.chosen[id])
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
	if r == nil || r.settled || r.stopped {
		return BuildRef{}, ErrNotRunning
	}
	if d.chosen == nil {
		d.chosen = map[int64]bool{}
	}
	d.chosen[id] = true
	close(d.changes)
	d.changes = make(chan struct{})
	return BuildRef{ID: id, Job: r.build.Job, Number: r.build.Number}, nil
}

// StopForRestart is called once the server is about to stop, and from then
// on no queued build starts: one that did would be lost when the server
// stops, and one left queued starts after the restart. While the server is
// quiet, it stops each build a restart runs again, records it to be queued
// again at the next start (Rerun), and returns once they have ended or ctx
// has. Without quiet it stops none: a restart nobody prepared loses what
// runs, as before.
func (d *Dispatcher) StopForRestart(ctx context.Context) []BuildRef {
	d.mu.Lock()
	d.stopping = true
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

func (d *Dispatcher) stopForRestart(r *run) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r.settled || r.stopped || d.running[r.build.ID] != r {
		return false
	}
	r.stop(stoppedForRestart{})
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
		req.UpstreamStatus, req.UpstreamCommit, req.UpstreamTag, req.RerunOf = o.UpstreamStatus, o.UpstreamCommit, o.UpstreamTag, o.Number
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

func (d *Dispatcher) activity(ctx context.Context, job, line string) {
	if err := d.st.AddActivity(ctx, job, line, time.Now()); err != nil {
		d.log.Error("recording a job's activity", "job", job, "err", err)
	}
}
