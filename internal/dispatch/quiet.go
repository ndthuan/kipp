package dispatch

import (
	"context"
	"time"

	"github.com/ndthuan/kipp/internal/store"
)

// Quiet is how the server is made ready for a restart, as Jenkins does it:
// no new build starts, the ones running end by themselves, and a trigger
// that comes meanwhile is refused with ErrQuiet. A build that was queued
// before the server went quiet is accepted work and still runs. A cron fire
// that was refused is kept by job and runs once when quiet ends; the other
// triggers are their callers' to repeat (a scan finds the commit it missed);
// a webhook is the exception, since its sender never repeats it: the webhook
// package keeps one that arrives meanwhile and runs it when quiet ends (see
// OnQuietEnd).

// QuietState is what the server says of its quiet.
type QuietState struct {
	On    bool
	Since time.Time // when quiet began, zero when it is off
	// Running is the builds that have started and not yet let go of what
	// they hold, and Queued the ones accepted and not started: while both
	// are 0 the server is idle and may be restarted.
	Running, Queued int
}

// Quiet reports whether the server is quiet.
func (d *Dispatcher) Quiet() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.quiet.IsZero()
}

// refusedQuiet is true while the server is quiet, and notes the job of a
// cron fire it refuses.
func (d *Dispatcher) refusedQuiet(req TriggerRequest) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.quiet.IsZero() {
		return false
	}
	if req.Kind == "cron" {
		if d.skipped == nil {
			d.skipped = map[string]bool{}
		}
		d.skipped[req.Job] = true
	}
	return true
}

// OnQuietEnd sets what is called, once and after the cron fires are queued,
// each time quiet is turned off.
func (d *Dispatcher) OnQuietEnd(f func()) {
	d.mu.Lock()
	d.quietEnded = f
	d.mu.Unlock()
}

// SetQuiet turns quiet on or off, and reports it as it is then. Turning it
// off triggers once each job whose cron fired while it was on, and wakes
// the loop. Turning on what is on, or off what is off, changes nothing.
func (d *Dispatcher) SetQuiet(ctx context.Context, on bool) (QuietState, error) {
	d.mu.Lock()
	was := !d.quiet.IsZero()
	var again []string
	var ended func()
	switch {
	case on && !was:
		d.quiet = time.Now()
		d.log.Info("quiet: no build starts until quiet is turned off or the server restarts")
	case !on && was:
		d.quiet = time.Time{}
		for name := range d.skipped {
			again = append(again, name)
		}
		d.skipped = nil
		ended = d.quietEnded
		d.log.Info("quiet ends", "cron fires to run", len(again))
	}
	d.mu.Unlock()
	d.changed()
	for _, name := range again {
		if _, err := d.Trigger(ctx, TriggerRequest{Job: name, Kind: "cron", By: "kipp"}); err != nil {
			d.log.Info("a cron fire kept through quiet was not queued", "job", name, "err", err)
		}
	}
	if ended != nil {
		ended()
	}
	return d.QuietState(ctx)
}

// QuietState is the quiet as it is now, with what still runs or waits.
func (d *Dispatcher) QuietState(ctx context.Context) (QuietState, error) {
	d.mu.Lock()
	st := QuietState{On: !d.quiet.IsZero(), Since: d.quiet, Running: len(d.running)}
	d.mu.Unlock()
	queued, err := d.st.BuildsWithStatus(ctx, store.Queued)
	st.Queued = len(queued)
	return st, err
}
