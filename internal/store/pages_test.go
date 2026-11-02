package store

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

func TestActivityBeforeALineIsTheOlderOnesNewestFirst(t *testing.T) {
	s := open(t)
	for i := range 5 {
		must(t, s.AddActivity(ctx, "a", fmt.Sprintf("line %d", i), cacheT0.Add(time.Duration(i)*time.Minute)))
	}
	must(t, s.AddActivity(ctx, "b", "another job's", cacheT0))
	lines := func(before int64, limit int) []string {
		t.Helper()
		got, err := s.ActivityBefore(ctx, "a", before, limit)
		must(t, err)
		var out []string
		for _, a := range got {
			out = append(out, a.Line)
		}
		return out
	}
	first, err := s.Activity(ctx, "a", 2)
	must(t, err)
	if first[0].ID == 0 || first[0].ID <= first[1].ID {
		t.Fatalf("lines carry their ids, newest first: %+v", first)
	}
	if got := lines(first[1].ID, 2); !slices.Equal(got, []string{"line 2", "line 1"}) {
		t.Fatalf("the page before the second line: %v", got)
	}
	if got := lines(0, 9); len(got) != 5 {
		t.Fatalf("a before of 0 starts at the newest: %v", got)
	}
}

func TestTestsAfterACaseAreTheLaterOnesInTheOrderStored(t *testing.T) {
	s := open(t)
	b := &Build{Job: "a", Status: Success, Trigger: "cli"}
	must(t, s.CreateBuild(ctx, b))
	var cases []Test
	for i := range 5 {
		cases = append(cases, Test{Stage: "s", Suite: "x", Name: fmt.Sprintf("c%d", i), Status: "passed"})
	}
	must(t, s.PutTests(ctx, b.ID, cases))
	first, err := s.TestsAfter(ctx, b.ID, 0, 2)
	must(t, err)
	if len(first) != 2 || first[0].Name != "c0" || first[0].ID == 0 {
		t.Fatalf("the first page: %+v", first)
	}
	next, err := s.TestsAfter(ctx, b.ID, first[1].ID, 9)
	must(t, err)
	if len(next) != 3 || next[0].Name != "c2" {
		t.Fatalf("the page after c1: %+v", next)
	}
	all, err := s.Tests(ctx, b.ID)
	must(t, err)
	if len(all) != 5 {
		t.Fatalf("Tests is every case: %d", len(all))
	}
}

func TestMetricHistoryPagesDoNotCutABuildThatReportedTwiceInTwo(t *testing.T) {
	s := open(t)
	var ids []int64
	for range 3 {
		b := &Build{Job: "a", Status: Success, Trigger: "cli"}
		must(t, s.CreateBuild(ctx, b))
		ids = append(ids, b.ID)
		for _, stage := range []string{"one", "two"} {
			must(t, s.PutMetricHistory(ctx, b.ID, "site", stage, []Metric{{Name: "m", Unit: "s", Values: map[string]float64{"i": float64(b.ID)}}}))
		}
	}
	page := func(build, seq int64, limit int) []HistoryRow {
		t.Helper()
		got, err := s.MetricHistory(ctx, "site", "m", "i", build, seq, limit)
		must(t, err)
		return got
	}
	var seen []string
	rows := page(0, 0, 2)
	for len(rows) > 0 {
		for _, r := range rows {
			seen = append(seen, fmt.Sprintf("%d/%s", r.BuildID, r.Stage))
		}
		last := rows[len(rows)-1]
		rows = page(last.BuildID, last.Seq, 2)
	}
	var want []string
	for _, id := range slices.Backward(ids) {
		want = append(want, fmt.Sprintf("%d/two", id), fmt.Sprintf("%d/one", id))
	}
	if !slices.Equal(seen, want) {
		t.Fatalf("pages of two walk the rows %v, want %v", seen, want)
	}
}

func TestCacheEntriesAfterAnEntryAreTheLaterOnesInTheOrderTheyAreListed(t *testing.T) {
	s := open(t)
	a := putEntry(t, s, "a", cacheT0)
	b := putEntry(t, s, "b", cacheT0)
	c := putEntry(t, s, "c", cacheT0.Add(time.Hour))
	must(t, s.TouchCacheEntry(ctx, a.ID, cacheT0.Add(2*time.Hour))) // a is now the most recently restored
	keys := func(es []CacheEntry) []string {
		var out []string
		for _, e := range es {
			out = append(out, e.Key)
		}
		return out
	}
	first, err := s.CacheEntriesAfter(ctx, time.Time{}, 0, 2)
	must(t, err)
	if !slices.Equal(keys(first), []string{"b", "c"}) {
		t.Fatalf("the first page: %v", keys(first))
	}
	last := first[1]
	next, err := s.CacheEntriesAfter(ctx, last.OrderedAt(), last.ID, 2)
	must(t, err)
	if !slices.Equal(keys(next), []string{"a"}) {
		t.Fatalf("the page after c: %v", keys(next))
	}
	if first[0].ID != b.ID || first[1].ID != c.ID {
		t.Fatal("the ids are the entries'")
	}
	// An entry at the very instant of the cursor is after it only by its id.
	tie, err := s.CacheEntriesAfter(ctx, cacheT0, a.ID, 9)
	must(t, err)
	if !slices.Equal(keys(tie), []string{"b", "c", "a"}) {
		t.Fatalf("after a at the first instant: %v", keys(tie))
	}
}
