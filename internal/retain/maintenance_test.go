package retain

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

func TestSweepCapsTheHistoryOfPerCaseMetricsAndNotOfJobMetrics(t *testing.T) {
	st, logs := openStore(t)
	ctx := context.Background()
	now := time.Now()
	seedEvery(t, st, logs, "a", CaseHistoryBuilds+4, now, time.Minute)
	builds, err := st.Builds(ctx, "a", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range builds {
		err := st.PutMetricHistory(ctx, b.ID, "site", "s", []store.Metric{
			{Name: config.CaseSeconds, Values: map[string]float64{"suite / one": 1}},
			{Name: "coverage", Values: map[string]float64{"total": 1}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	job := parse(t, "name: a\nstages:\n  s: {run: true}\n")
	snap := &config.Snapshot{Jobs: map[string]*config.Job{"a": job}, Names: []string{"a"}}
	if _, err := Sweep(ctx, st, snap, logs, now); err != nil {
		t.Fatal(err)
	}
	rows, err := st.JobRows(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatal(err, rows)
	}
	if want := int64(CaseHistoryBuilds + len(builds)); rows[0].Metrics != want {
		t.Fatalf("%d metric rows, want %d: %d case rows and every coverage row", rows[0].Metrics, want, CaseHistoryBuilds)
	}
}

// freedPages removes the builds of a job straight from the store, leaving
// free pages behind.
func freedPages(t *testing.T, st *store.Store, logs string) {
	t.Helper()
	ctx := context.Background()
	seedEvery(t, st, logs, "junk", 30, time.Now(), time.Minute)
	builds, _ := st.Builds(ctx, "junk", 100)
	for _, b := range builds {
		tests := make([]store.Test, 50)
		for i := range tests {
			tests[i] = store.Test{Stage: "s", Suite: "s", Name: strconv.Itoa(i), Status: "passed", Message: string(make([]byte, 300))}
		}
		if err := st.PutTests(ctx, b.ID, tests); err != nil {
			t.Fatal(err)
		}
	}
	for _, b := range builds {
		if err := st.DeleteBuild(ctx, b.ID); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOnceAutoVacuumIsIncrementalTheSweepGivesTheFreedPagesBack(t *testing.T) {
	st, logs := openStore(t)
	ctx := context.Background()
	if _, err := st.Compact(ctx); err != nil {
		t.Fatal(err)
	}
	freedPages(t, st, logs)
	if f, _ := st.DBFile(ctx); f.FreePages == 0 {
		t.Fatal("nothing freed to give back")
	}
	if _, err := Sweep(ctx, st, &config.Snapshot{Jobs: map[string]*config.Job{}}, logs, time.Now()); err != nil {
		t.Fatal(err)
	}
	if f, _ := st.DBFile(ctx); f.FreePages != 0 {
		t.Fatalf("%d free pages left after the sweep", f.FreePages)
	}
}

func TestRemoveTakesEveryEndedBuildOfAJobButTheKeptOnesAndEmptyFolders(t *testing.T) {
	st, logs := openStore(t)
	ctx := context.Background()
	seed(t, st, logs, "gone", 3, time.Now())
	seed(t, st, logs, "kept", 2, time.Now())
	kept, _ := st.BuildByNumber(ctx, "kept", 1)
	if err := st.KeepBuild(ctx, kept.ID, "admin", time.Now()); err != nil {
		t.Fatal(err)
	}
	n, err := Remove(ctx, st, "gone", logs)
	if err != nil || n != 3 {
		t.Fatal(err, n)
	}
	if _, err := os.Stat(filepath.Join(logs, "gone")); !os.IsNotExist(err) {
		t.Fatalf("the empty folder of the job is left: %v", err)
	}
	n, err = Remove(ctx, st, "kept", logs)
	if err != nil || n != 1 {
		t.Fatal(err, n)
	}
	if _, err := os.Stat(filepath.Join(logs, "kept", "1", "s.log")); err != nil {
		t.Fatalf("the kept build's log went: %v", err)
	}
	if _, err := Remove(ctx, st, "../x", logs); err == nil {
		t.Fatal("a name with a path in it was taken")
	}
}
