package store

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// seedJob gives job n ended builds, each with tests test rows and metrics
// rows of kipp.case_seconds-like history on items "c0".."c<metrics-1>".
func seedJob(t *testing.T, s *Store, job string, n, tests, metrics int) []int64 {
	t.Helper()
	var ids []int64
	for range n {
		b := &Build{Job: job, Status: Success, Trigger: "cli"}
		must(t, s.CreateBuild(ctx, b))
		ts := make([]Test, tests)
		for i := range ts {
			ts[i] = Test{Stage: "s", Suite: "suite", Name: fmt.Sprintf("case %d", i), Status: "passed", Message: strings.Repeat("x", 200)}
		}
		must(t, s.PutTests(ctx, b.ID, ts))
		vals := map[string]float64{}
		for i := range metrics {
			vals[fmt.Sprintf("c%d", i)] = float64(i)
		}
		must(t, s.PutMetricHistory(ctx, b.ID, "site", "s", []Metric{{Name: "kipp.case_seconds", Unit: "s", Values: vals}, {Name: "coverage", Values: map[string]float64{"total": 1}}}))
		ids = append(ids, b.ID)
	}
	return ids
}

func TestJobRowsCountBuildsTestsAndMetricRowsPerJob(t *testing.T) {
	s := open(t)
	seedJob(t, s, "big", 3, 5, 2)
	seedJob(t, s, "small", 1, 1, 0)
	ids := seedJob(t, s, "kept", 2, 0, 0)
	must(t, s.KeepBuild(ctx, ids[0], "admin", time.Now()))
	rows, err := s.JobRows(ctx)
	must(t, err)
	got := map[string]JobRows{}
	for _, r := range rows {
		got[r.Job] = r
	}
	if b := got["big"]; b.Builds != 3 || b.Tests != 15 || b.Metrics != 9 || b.Newest.IsZero() {
		t.Fatalf("big %+v", b)
	}
	if b := got["small"]; b.Builds != 1 || b.Tests != 1 || b.Metrics != 1 {
		t.Fatalf("small %+v", b)
	}
	if k := got["kept"]; k.Builds != 2 || k.Kept != 1 {
		t.Fatalf("kept %+v", k)
	}
}

func TestTheTablesAndIndexesAreMeasuredPageByPageLargestFirst(t *testing.T) {
	s := open(t)
	seedJob(t, s, "big", 20, 50, 0)
	objs, err := s.MeasureObjects(ctx)
	must(t, err)
	if !objs.Exact {
		t.Fatal("the test SQLite has dbstat, and it was not used")
	}
	if objs.Objects[0].Name != "tests" || objs.Objects[0].Index || objs.Objects[0].Bytes <= 0 {
		t.Fatalf("largest %+v", objs.Objects[0])
	}
	found := false
	for i, o := range objs.Objects {
		if i > 0 && o.Bytes > objs.Objects[i-1].Bytes {
			t.Fatalf("not largest first at %d: %+v", i, objs.Objects)
		}
		if o.Name == "tests_build" {
			found = o.Index && o.Table == "tests"
		}
	}
	if !found {
		t.Fatalf("the index tests_build is not an index of tests: %+v", objs.Objects)
	}
}

func TestWithoutDbstatTheTablesAreEstimatedByTheirRows(t *testing.T) {
	s := open(t)
	// More test rows than schema rows, one per migration, so tests is the
	// largest table however many migrations come.
	seedJob(t, s, "a", 4, len(migrations), 0)
	ro, err := s.reader()
	must(t, err)
	objs, err := estimateObjects(ctx, ro, 1000)
	must(t, err)
	if objs.Exact {
		t.Fatal("an estimate is not exact")
	}
	byName := map[string]int64{}
	var sum int64
	for _, o := range objs.Objects {
		if o.Index {
			t.Fatalf("an estimate names an index: %+v", o)
		}
		byName[o.Name] = o.Bytes
		sum += o.Bytes
	}
	if objs.Objects[0].Name != "tests" || byName["tests"] <= byName["builds"] || sum > 1000 || sum < 990 {
		t.Fatalf("%+v", objs.Objects)
	}
}

func TestTheCaseMetricCapKeepsTheNewestBuildsOfEachItemPerJob(t *testing.T) {
	s := open(t)
	a := seedJob(t, s, "a", 35, 0, 2)
	seedJob(t, s, "b", 3, 0, 2)
	n, err := s.CapItemHistory(ctx, "kipp.case_seconds", 30)
	must(t, err)
	if n != 5*2 {
		t.Fatalf("removed %d, want 10", n)
	}
	var left, oldest int64
	must(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*), MIN(h.build_id) FROM metric_history h JOIN builds b ON b.id = h.build_id WHERE b.job = 'a' AND h.metric = 'kipp.case_seconds'`).Scan(&left, &oldest))
	if left != 60 || oldest != a[5] {
		t.Fatalf("a keeps %d rows from build %d, want 60 from %d", left, oldest, a[5])
	}
	var others int64
	must(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM metric_history WHERE metric = 'coverage'`).Scan(&others))
	if others != 38 {
		t.Fatalf("a job-level metric lost rows: %d", others)
	}
	rows, err := s.JobRows(ctx)
	must(t, err)
	for _, r := range rows {
		if r.Job == "b" && r.Metrics != 9 {
			t.Fatalf("b under the cap lost rows: %+v", r)
		}
	}
}

func TestTwoStagesReportingOneItemInABuildCountAsOneBuildOfTheCap(t *testing.T) {
	s := open(t)
	for range 3 {
		b := &Build{Job: "a", Status: Success, Trigger: "cli"}
		must(t, s.CreateBuild(ctx, b))
		for _, stage := range []string{"x", "y"} {
			must(t, s.PutMetricHistory(ctx, b.ID, "site", stage, []Metric{{Name: "kipp.case_seconds", Values: map[string]float64{"c": 1}}}))
		}
	}
	n, err := s.CapItemHistory(ctx, "kipp.case_seconds", 2)
	must(t, err)
	if n != 2 {
		t.Fatalf("removed %d rows, want the 2 of the oldest build", n)
	}
}

func TestCompactTurnsOnIncrementalAutoVacuumOnceAndShrinksAFileWithFreePages(t *testing.T) {
	s := open(t)
	ids := seedJob(t, s, "a", 40, 100, 0)
	for _, id := range ids {
		must(t, s.DeleteBuild(ctx, id))
	}
	f, err := s.DBFile(ctx)
	must(t, err)
	if f.FreePages == 0 || f.AutoVacuum != AutoVacuumNone {
		t.Fatalf("before %+v", f)
	}
	first, err := s.Compact(ctx)
	must(t, err)
	if !first.Incremental || first.After >= first.Before || first.Before == 0 {
		t.Fatalf("first compact %+v", first)
	}
	f, err = s.DBFile(ctx)
	must(t, err)
	if f.FreePages != 0 || f.AutoVacuum != AutoVacuumIncremental {
		t.Fatalf("after %+v", f)
	}
	again, err := s.Compact(ctx)
	must(t, err)
	if again.Incremental {
		t.Fatal("the second compact says it turned incremental auto_vacuum on again")
	}
	// The store still works on its own connection.
	seedJob(t, s, "b", 1, 1, 1)
}

func TestIncrementalVacuumGivesFreePagesBackOnlyOnceAutoVacuumIsIncremental(t *testing.T) {
	s := open(t)
	ids := seedJob(t, s, "a", 20, 100, 0)
	for _, id := range ids[:10] {
		must(t, s.DeleteBuild(ctx, id))
	}
	ran, err := s.IncrementalVacuum(ctx)
	must(t, err)
	f, _ := s.DBFile(ctx)
	if ran || f.FreePages == 0 {
		t.Fatalf("ran %v on a file with no auto_vacuum: %+v", ran, f)
	}
	_, err = s.Compact(ctx)
	must(t, err)
	for _, id := range ids[10:] {
		must(t, s.DeleteBuild(ctx, id))
	}
	f, _ = s.DBFile(ctx)
	if f.FreePages == 0 {
		t.Fatal("no free pages to give back")
	}
	ran, err = s.IncrementalVacuum(ctx)
	must(t, err)
	f, _ = s.DBFile(ctx)
	if !ran || f.FreePages != 0 {
		t.Fatalf("ran %v, %+v", ran, f)
	}
}

func TestQuickCheckOfASoundFileFindsNothing(t *testing.T) {
	s := open(t)
	seedJob(t, s, "a", 2, 2, 2)
	problems, err := s.QuickCheck(ctx)
	must(t, err)
	if problems != nil {
		t.Fatalf("%v", problems)
	}
}

func TestEndedBuildsLeaveOutRunningAndKeptOnes(t *testing.T) {
	s := open(t)
	ids := seedJob(t, s, "a", 3, 0, 0)
	must(t, s.KeepBuild(ctx, ids[1], "admin", time.Now()))
	running := &Build{Job: "a", Status: Running, Trigger: "cli"}
	must(t, s.CreateBuild(ctx, running))
	got, err := s.EndedBuilds(ctx, "a")
	must(t, err)
	if len(got) != 2 || got[0] != ids[0] || got[1] != ids[2] {
		t.Fatalf("%v", got)
	}
}
