package dispatch

import (
	"context"
	"errors"
	"time"

	"github.com/ndthuan/kipp/internal/sched"
)

// Tick fires every job whose cron, read in the job's zone, matches the
// minute of now, but for the disabled ones. Whoever runs the dispatcher
// calls it once for each minute.
func (d *Dispatcher) Tick(ctx context.Context, now time.Time) {
	snap := d.Snapshot()
	minute := now.Truncate(time.Minute)
	for _, name := range snap.Names {
		job := snap.Jobs[name]
		if job.Triggers.Cron == "" || job.Disabled {
			continue
		}
		s, err := sched.Parse(job.Triggers.Cron, job.Name)
		if err != nil {
			continue // Load refused it already; defensive
		}
		if !s.Next(minute.Add(-time.Minute).In(d.Zone(job))).Equal(minute) {
			continue
		}
		_, err = d.Trigger(ctx, TriggerRequest{Job: name, Kind: "cron", By: "kipp"})
		switch {
		case errors.Is(err, ErrDropped):
			d.log.Info("cron trigger not queued", "job", name, "err", err)
		case err != nil:
			d.log.Warn("cron trigger failed", "job", name, "err", err)
		}
	}
}
