package store

import (
	"strings"
	"testing"
	"time"
)

// ended makes a build of job that ended at the offset from base with st.
func ended(t *testing.T, s *Store, job string, st Status, base time.Time, off time.Duration) {
	t.Helper()
	b := &Build{Job: job, Status: Queued, Trigger: "cli", QueuedAt: base.Add(off - time.Minute)}
	must(t, s.CreateBuild(ctx, b))
	must(t, s.SetBuildStatus(ctx, b.ID, Running, base.Add(off-30*time.Second)))
	if st.Terminal() {
		must(t, s.SetBuildStatus(ctx, b.ID, st, base.Add(off)))
	}
}

func TestFinishedSinceHoldsTheBuildsThatEndedFromThenAndTheOnesNotOver(t *testing.T) {
	s := open(t)
	base := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	ended(t, s, "a", Success, base, -3*time.Hour)
	ended(t, s, "a", Failed, base, -time.Hour)
	ended(t, s, "b", Cancelled, base, -time.Hour)
	ended(t, s, "b", Running, base, -time.Minute)
	must(t, s.CreateBuild(ctx, &Build{Job: "b", Status: Queued, Trigger: "cli", QueuedAt: base}))
	got, err := s.FinishedSince(ctx, base.Add(-2*time.Hour))
	must(t, err)
	var seen []string
	for _, b := range got {
		seen = append(seen, b.Job+"#"+string(b.Status))
	}
	if len(seen) != 4 || strings.Contains(strings.Join(seen, " "), "success") {
		t.Fatalf("the success of three hours ago is out and the rest in: %v", seen)
	}
}

func TestFinishedSinceReadsTheIndexOnWhenBuildsEndedAndDoesNotScanTheTable(t *testing.T) {
	s := open(t)
	rows, err := s.db.QueryContext(ctx, `EXPLAIN QUERY PLAN SELECT `+buildCols+` FROM builds WHERE finished_at >= ? OR finished_at IS NULL`, ts(time.Now()))
	must(t, err)
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		must(t, rows.Scan(&id, &parent, &unused, &detail))
		plan = append(plan, detail)
	}
	must(t, rows.Err())
	if !strings.Contains(strings.Join(plan, "\n"), "builds_finished_at") || strings.Contains(strings.Join(plan, "\n"), "SCAN builds") {
		t.Fatalf("the plan scans the table:\n%s", strings.Join(plan, "\n"))
	}
}

func TestTalliesBetweenCountsGreenAndRedPerJobInAHalfOpenRange(t *testing.T) {
	s := open(t)
	base := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		job string
		st  Status
		off time.Duration
	}{
		{"a", Success, -5 * time.Hour}, {"a", Unstable, -4 * time.Hour}, {"a", Failed, -3 * time.Hour}, {"a", Timeout, -3 * time.Hour},
		{"a", Lost, -2 * time.Hour}, {"a", Cancelled, -2 * time.Hour},
		{"a", Success, -time.Hour},      // the end of the range is not in it
		{"a", Success, -10 * time.Hour}, // before the range
		{"b", Cancelled, -3 * time.Hour},
	} {
		ended(t, s, c.job, c.st, base, c.off)
	}
	got, err := s.TalliesBetween(ctx, base.Add(-5*time.Hour), base.Add(-time.Hour))
	must(t, err)
	if len(got) != 1 || got["a"] != (Tally{Green: 2, Red: 3}) {
		t.Fatalf("%+v: a job of cancelled builds alone has no entry", got)
	}
	if got["a"].Total() != 5 || got["a"].Percent() != 40 || (Tally{}).Percent() != 0 {
		t.Fatalf("%d %d", got["a"].Total(), got["a"].Percent())
	}
}
