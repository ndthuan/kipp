package store

import (
	"errors"
	"testing"
	"time"
)

func TestABuildThatEndedNeverChangesItsStatus(t *testing.T) {
	s := open(t)
	t1 := time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC)
	for _, end := range []Status{Success, Failed, Unstable, Cancelled, Timeout, Lost} {
		for _, next := range []Status{Queued, Running, Success, Failed, Cancelled} {
			if next == end {
				continue
			}
			b := &Build{Job: "a", Status: Queued, Trigger: "cli"}
			must(t, s.CreateBuild(ctx, b))
			must(t, s.SetBuildStatus(ctx, b.ID, Running, t1))
			must(t, s.SetBuildStatus(ctx, b.ID, end, t1.Add(time.Minute)))
			if err := s.SetBuildStatus(ctx, b.ID, next, t1.Add(time.Hour)); !errors.Is(err, ErrBuildEnded) {
				t.Fatalf("%s to %s: %v, want ErrBuildEnded", end, next, err)
			}
			got, err := s.Build(ctx, b.ID)
			must(t, err)
			if got.Status != end || got.FinishedAt == nil || !got.FinishedAt.Equal(t1.Add(time.Minute)) || got.StartedAt == nil || !got.StartedAt.Equal(t1) {
				t.Fatalf("%s to %s: the row reads %s, started %v, finished %v", end, next, got.Status, got.StartedAt, got.FinishedAt)
			}
		}
	}
}

func TestWritingTheStatusABuildEndedWithAgainIsNoErrorAndKeepsItsEnd(t *testing.T) {
	s := open(t)
	t1 := time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC)
	b := &Build{Job: "a", Status: Queued, Trigger: "cli"}
	must(t, s.CreateBuild(ctx, b))
	must(t, s.SetBuildStatus(ctx, b.ID, Failed, t1))
	must(t, s.SetBuildStatus(ctx, b.ID, Failed, t1.Add(time.Hour)))
	got, err := s.Build(ctx, b.ID)
	must(t, err)
	if got.Status != Failed || got.FinishedAt == nil || !got.FinishedAt.Equal(t1) {
		t.Fatalf("%s, finished %v: want the first end kept", got.Status, got.FinishedAt)
	}
}
