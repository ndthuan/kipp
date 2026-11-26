package dispatch

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ndthuan/kipp/internal/store"
)

// QuietState is what the server says of its quiet.
type QuietState struct {
	On              bool
	Since           time.Time
	Running, Queued int
	Rerun           int
	Holding         []BuildRef
}

// BuildRef names a build.
type BuildRef struct {
	ID     int64
	Job    string
	Number int
}

// HoldingAfter is how long quiet waits before it names the builds that may
// be holding it (QuietState.Holding).
const HoldingAfter = 10 * time.Minute

// Quiet reports whether the server is quiet.
func (d *Dispatcher) Quiet() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.quiet.IsZero()
}

func (d *Dispatcher) refusedQuiet() bool {
	return d.Quiet()
}

func (d *Dispatcher) keepCron(ctx context.Context, job string) {
	if err := d.st.KeepCron(ctx, job); err != nil {
		d.log.Error("a cron fire refused while quiet was not kept; it is lost", "job", job, "err", err)
		return
	}
	if !d.Quiet() {
		d.TriggerKeptCrons(ctx)
	}
}

// TriggerKeptCrons triggers once each job whose cron fired while the server
// was quiet, and forgets each once triggered or refused for good. The server
// calls it when quiet ends and at start, after TriggerKeptAfters. It stops
// at the first one the server is quiet again for, and leaves it and the
// ones after it kept.
func (d *Dispatcher) TriggerKeptCrons(ctx context.Context) {
	d.keptCronsMu.Lock()
	defer d.keptCronsMu.Unlock()
	jobs, err := d.st.PendingCrons(ctx)
	if err != nil {
		d.log.Error("reading the cron fires kept while quiet", "err", err)
		return
	}
	for _, name := range jobs {
		_, err := d.trigger(ctx, TriggerRequest{Job: name, Kind: "cron", By: "kipp"})
		for errors.Is(err, ErrQuiet) {
			if d.Quiet() {
				d.log.Info("quiet again, kept cron fires left for when it ends")
				return
			}
			_, err = d.trigger(ctx, TriggerRequest{Job: name, Kind: "cron", By: "kipp"})
		}
		if err != nil {
			d.log.Info("a cron fire kept through quiet was not queued", "job", name, "err", err)
		}
		if err := d.st.DropCron(ctx, name); err != nil {
			d.log.Error("forgetting a kept cron fire", "job", name, "err", err)
		}
	}
}

// OnQuietEnd sets what is called, once and after the cron fires and the
// kept after triggers are queued, each time quiet is turned off.
func (d *Dispatcher) OnQuietEnd(f func()) {
	d.mu.Lock()
	d.quietEnded = f
	d.mu.Unlock()
}

// SetQuiet turns quiet on or off, and reports it as it is then. Turning it
// off triggers once each job whose cron fired while it was on, then the
// kept after triggers, and wakes the loop. Turning on what is on, or off
// what is off, changes nothing.
func (d *Dispatcher) SetQuiet(ctx context.Context, on bool) (QuietState, error) {
	d.mu.Lock()
	was := !d.quiet.IsZero()
	var ended func()
	switch {
	case on && !was:
		d.quiet = time.Now()
		d.log.Info("quiet: no build starts until quiet is turned off or the server restarts")
	case !on && was:
		d.quiet = time.Time{}
		d.chosen = nil
		ended = d.quietEnded
		d.log.Info("quiet ends")
	}
	d.mu.Unlock()
	d.changed()
	if !on && was {
		d.TriggerKeptCrons(ctx)
		d.TriggerKeptAfters(ctx)
	}
	if ended != nil {
		ended()
	}
	return d.QuietState(ctx)
}

func (d *Dispatcher) keepAfter(ctx context.Context, up *store.Build, name string) bool {
	if err := d.st.KeepAfter(ctx, up.ID, name); err != nil {
		d.log.Error("an after trigger refused while quiet was not kept; it is lost", "job", name, "after", up.Job, "number", up.Number, "err", err)
		return false
	}
	d.log.Info("an after trigger kept while quiet", "job", name, "after", up.Job, "number", up.Number)
	d.activity(ctx, name, fmt.Sprintf("kept the after trigger of %s #%d while quiet; it runs when quiet ends", up.Job, up.Number))
	return true
}

// TriggerKeptAfters triggers each after trigger kept while the server was
// quiet, in the order they were kept, and forgets each once triggered or
// refused for good. The server calls it when quiet ends and at start, after
// Rerun. It stops at the first one the server is quiet again for, and
// leaves it and the ones after it kept.
func (d *Dispatcher) TriggerKeptAfters(ctx context.Context) {
	d.keptAftersMu.Lock()
	defer d.keptAftersMu.Unlock()
	pending, err := d.st.PendingAfters(ctx)
	if err != nil {
		d.log.Error("reading the after triggers kept while quiet", "err", err)
		return
	}
	for _, p := range pending {
		_, err := d.Trigger(ctx, afterRequest(p.Upstream, p.Job, p.Upstream.Status))
		if errors.Is(err, ErrQuiet) {
			d.log.Info("quiet again, kept after triggers left for when it ends")
			return
		}
		if err != nil {
			d.log.Warn("a kept after trigger was not queued", "job", p.Job, "after", p.Upstream.Job, "number", p.Upstream.Number, "err", err)
		} else {
			d.log.Info("a kept after trigger queued", "job", p.Job, "after", p.Upstream.Job, "number", p.Upstream.Number)
		}
		if err := d.st.DropAfter(ctx, p.Upstream.ID, p.Job); err != nil {
			d.log.Error("forgetting a kept after trigger", "job", p.Job, "after", p.Upstream.Job, "number", p.Upstream.Number, "err", err)
		}
	}
}

// QuietState is the quiet as it is now, with what still runs or waits.
func (d *Dispatcher) QuietState(ctx context.Context) (QuietState, error) {
	d.mu.Lock()
	st := QuietState{On: !d.quiet.IsZero(), Since: d.quiet}
	var left []BuildRef
	startedSince := false
	for id, r := range d.running {
		if st.On && d.rerunsLocked(id, r) {
			st.Rerun++
			continue
		}
		st.Running++
		left = append(left, BuildRef{ID: id, Job: r.build.Job, Number: r.build.Number})
		if r.build.StartedAt == nil || r.build.StartedAt.After(st.Since) {
			startedSince = true
		}
	}
	now := d.now()
	d.mu.Unlock()
	queued, err := d.st.BuildsWithStatus(ctx, store.Queued)
	st.Queued = len(queued)
	if st.On && st.Queued == 0 && !startedSince && now.Sub(st.Since) >= HoldingAfter {
		slices.SortFunc(left, func(a, b BuildRef) int { return cmp.Compare(a.ID, b.ID) })
		st.Holding = left
	}
	return st, err
}

// Counts says what quiet still waits for: "N running, M queued", and how
// many will run again after the restart when there are any.
func (q QuietState) Counts() string {
	s := fmt.Sprintf("%d running, %d queued", q.Running, q.Queued)
	if q.Rerun > 0 {
		s += fmt.Sprintf(", %d will run again after the restart", q.Rerun)
	}
	return s
}

// Hint names the builds that may be holding quiet back (Holding), empty
// when there are none.
func (q QuietState) Hint() string {
	switch len(q.Holding) {
	case 0:
		return ""
	case 1:
		return "Only " + q.HoldingNames() + " still runs; it may be waiting on something quiet holds back"
	}
	return "Only " + q.HoldingNames() + " still run; they may be waiting on something quiet holds back"
}

// HoldingNames is the builds of Holding as "a #1, b #2 and c #3".
func (q QuietState) HoldingNames() string {
	names := make([]string, len(q.Holding))
	for i, b := range q.Holding {
		names[i] = fmt.Sprintf("%s #%d", b.Job, b.Number)
	}
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}
