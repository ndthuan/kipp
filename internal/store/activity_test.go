package store

import (
	"fmt"
	"testing"
	"time"
)

func activityLines(t *testing.T, s *Store, job string) []string {
	t.Helper()
	got, err := s.Activity(ctx, job, 100)
	must(t, err)
	out := make([]string, 0, len(got))
	for _, a := range got {
		out = append(out, a.Line)
	}
	return out
}

func TestCapActivityKeepsTheNewestLinesOfEachJobAndNoOlder(t *testing.T) {
	s := open(t)
	for i := range 5 {
		must(t, s.AddActivity(ctx, "busy", fmt.Sprintf("line %d", i), cacheT0.Add(time.Duration(i)*time.Minute)))
		must(t, s.AddActivity(ctx, "other", fmt.Sprintf("line %d", i), cacheT0.Add(time.Duration(i)*time.Minute)))
	}
	must(t, s.AddActivity(ctx, "quiet", "its one line", cacheT0))
	must(t, s.CapActivity(ctx, 2))
	for _, job := range []string{"busy", "other"} {
		if got := activityLines(t, s, job); len(got) != 2 || got[0] != "line 4" || got[1] != "line 3" {
			t.Fatalf("%s: %v, want its two newest lines", job, got)
		}
	}
	if got := activityLines(t, s, "quiet"); len(got) != 1 {
		t.Fatalf("a job under the cap lost lines: %v", got)
	}
}

func TestForgetActivityRemovesTheLinesOfAJobWithNoBuildLeftAndOfNoOther(t *testing.T) {
	s := open(t)
	must(t, s.CreateBuild(ctx, &Build{Job: "built", Status: Success, Trigger: "cli", QueuedAt: cacheT0}))
	for _, job := range []string{"built", "empty", "other"} {
		must(t, s.AddActivity(ctx, job, "a line", cacheT0))
	}
	must(t, s.ForgetActivity(ctx, "built"))
	must(t, s.ForgetActivity(ctx, "empty"))
	if got := activityLines(t, s, "built"); len(got) != 1 {
		t.Fatalf("a job that still has a build lost its activity: %v", got)
	}
	if got := activityLines(t, s, "empty"); len(got) != 0 {
		t.Fatalf("a job with no build left keeps its activity: %v", got)
	}
	if got := activityLines(t, s, "other"); len(got) != 1 {
		t.Fatalf("another job lost its activity: %v", got)
	}
}
