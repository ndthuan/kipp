package cli

import (
	"context"
	"time"
)

// The server's clocks: the next time cron is looked at, the next time old
// builds are swept, and the next look at the free space of the data
// directory's disk. They are variables so that a test of serve waits for
// none.
var (
	cronAt  = nextMinute
	sweepAt = nextSweep
	floorAt = nextFloorLook
)

// floorEvery is how often the free space of the data directory's disk is
// looked at: a statfs, cheap, and often enough to clean before a build
// fills what is left.
const floorEvery = 10 * time.Minute

func nextFloorLook(now time.Time) time.Time { return now.Truncate(floorEvery).Add(floorEvery) }

// nextMinute is the next whole minute after now.
func nextMinute(now time.Time) time.Time {
	return now.Truncate(time.Minute).Add(time.Minute)
}

// nextSweep is the next half past three after now, in now's zone.
func nextSweep(now time.Time) time.Time {
	next := time.Date(now.Year(), now.Month(), now.Day(), 3, 30, 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

// every calls do at each time next names, and with that time, until ctx
// ends or wait says it has. now and wait are the clock: time.Now and sleep
// outside a test. A time is never named twice: when the clock reads
// earlier than the time just done, as it may when the timer runs ahead of
// the wall clock, the one after it is what next gives from there.
func every(ctx context.Context, now func() time.Time, wait func(ctx context.Context, d time.Duration) bool, next func(now time.Time) time.Time, do func(ctx context.Context, at time.Time)) {
	var last time.Time
	for {
		at := next(now())
		if !at.After(last) {
			at = next(last)
		}
		if !wait(ctx, at.Sub(now())) {
			return
		}
		do(ctx, at)
		last = at
	}
}

// sleep waits for d, and reports false if ctx ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
