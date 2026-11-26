package store

import (
	"slices"
	"testing"
)

func TestAKeptCronFireWaitsUntilDroppedOncePerJob(t *testing.T) {
	s := open(t)
	must(t, s.KeepCron(ctx, "b"))
	must(t, s.KeepCron(ctx, "a"))
	must(t, s.KeepCron(ctx, "b"))
	got, err := s.PendingCrons(ctx)
	must(t, err)
	if !slices.Equal(got, []string{"b", "a"}) {
		t.Fatalf("pending crons %v, want b then a", got)
	}
	must(t, s.DropCron(ctx, "b"))
	got, err = s.PendingCrons(ctx)
	must(t, err)
	if !slices.Equal(got, []string{"a"}) {
		t.Fatalf("pending crons %v, want a alone", got)
	}
}
