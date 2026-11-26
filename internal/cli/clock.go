package cli

import (
	"context"
	"time"
)

var (
	cronAt  = nextMinute
	sweepAt = nextSweep
	floorAt = nextFloorLook
)

const floorEvery = 10 * time.Minute

func nextFloorLook(now time.Time) time.Time { return now.Truncate(floorEvery).Add(floorEvery) }

func nextMinute(now time.Time) time.Time {
	return now.Truncate(time.Minute).Add(time.Minute)
}

func nextSweep(now time.Time) time.Time {
	next := time.Date(now.Year(), now.Month(), now.Day(), 3, 30, 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

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
