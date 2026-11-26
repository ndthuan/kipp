package retain

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

func liveSnapshot(names ...string) *config.Snapshot {
	snap := &config.Snapshot{Jobs: map[string]*config.Job{}}
	for _, name := range names {
		snap.Jobs[name] = &config.Job{Name: name, Keep: config.Keep{Builds: config.DefaultKeepBuilds}}
		snap.Names = append(snap.Names, name)
	}
	return snap
}

func TestAnEmptyJobSetRemovesNothingOfTheJobsNotInItAndTheSweepSaysSo(t *testing.T) {
	st, logs := openStore(t)
	ctx := context.Background()
	now := time.Now()
	seed(t, st, logs, "a", 3, now)
	seed(t, st, logs, "b", 3, now)
	n, err := Sweep(ctx, st, liveSnapshot(), logs, now.AddDate(0, 0, config.RemovedKeepDays+2))
	if !errors.Is(err, ErrJobSetShrank) || n != 0 {
		t.Fatalf("err %v, removed %d: want ErrJobSetShrank and nothing removed", err, n)
	}
	for _, job := range []string{"a", "b"} {
		if left, _ := st.Builds(ctx, job, 10); len(left) != 3 || !exists(filepath.Join(logs, job, "1")) {
			t.Fatalf("%s: %d builds left, want all 3 with their logs", job, len(left))
		}
	}
}

func TestAJobSetThatLostMoreJobsThanItHasRemovesNothingOfThem(t *testing.T) {
	st, logs := openStore(t)
	ctx := context.Background()
	now := time.Now()
	for _, job := range []string{"a", "b", "live"} {
		seed(t, st, logs, job, 2, now)
	}
	later := now.AddDate(0, 0, config.RemovedKeepDays+2)
	n, err := Sweep(ctx, st, liveSnapshot("live"), logs, later)
	if !errors.Is(err, ErrJobSetShrank) || n != 0 {
		t.Fatalf("err %v, removed %d: want ErrJobSetShrank and nothing removed", err, n)
	}
	n, err = Sweep(ctx, st, liveSnapshot("live", "other"), logs, later)
	if err != nil || n != 4 {
		t.Fatalf("err %v, removed %d: want the 4 builds of two removed jobs gone beside two jobs that have a file", err, n)
	}
}

func TestAShrunkenJobSetStillAppliesTheRulesOfTheJobsInIt(t *testing.T) {
	st, logs := openStore(t)
	ctx := context.Background()
	now := time.Now()
	for _, job := range []string{"a", "b", "live"} {
		seed(t, st, logs, job, 3, now)
	}
	snap := liveSnapshot("live")
	snap.Jobs["live"].Keep = config.Keep{Builds: 1}
	n, err := Sweep(ctx, st, snap, logs, now.AddDate(0, 0, config.RemovedKeepDays+2))
	if !errors.Is(err, ErrJobSetShrank) || n != 2 {
		t.Fatalf("err %v, removed %d: want ErrJobSetShrank and the 2 builds beyond the live job's keep gone", err, n)
	}
}

func activity(t *testing.T, st *store.Store, job string) int {
	t.Helper()
	lines, err := st.Activity(context.Background(), job, ActivityRows+10)
	if err != nil {
		t.Fatal(err)
	}
	return len(lines)
}

func TestSweepKeepsTheNewestActivityLinesOfAJobAndNoMore(t *testing.T) {
	st, logs := openStore(t)
	ctx := context.Background()
	now := time.Now()
	for range ActivityRows + 3 {
		if err := st.AddActivity(ctx, "live", "a line", now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Sweep(ctx, st, liveSnapshot("live"), logs, now); err != nil {
		t.Fatal(err)
	}
	if n := activity(t, st, "live"); n != ActivityRows {
		t.Fatalf("%d lines kept, want %d", n, ActivityRows)
	}
}

func TestARemovedJobsActivityGoesWithItsLastBuildAndNotBefore(t *testing.T) {
	st, logs := openStore(t)
	ctx := context.Background()
	now := time.Now()
	seed(t, st, logs, "gone", 2, now)
	for _, job := range []string{"gone", "live"} {
		if err := st.AddActivity(ctx, job, "a line", now); err != nil {
			t.Fatal(err)
		}
	}
	snap := liveSnapshot("live")
	if _, err := Sweep(ctx, st, snap, logs, now.AddDate(0, 0, config.RemovedKeepDays-2).Add(12*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if activity(t, st, "gone") != 1 {
		t.Fatal("a removed job that still has a build lost its activity")
	}
	if _, err := Sweep(ctx, st, snap, logs, now.AddDate(0, 0, config.RemovedKeepDays+2)); err != nil {
		t.Fatal(err)
	}
	if activity(t, st, "gone") != 0 {
		t.Fatal("the activity of a removed job outlived its last build")
	}
	if activity(t, st, "live") != 1 {
		t.Fatal("a job that has a file and no build lost its activity")
	}
}

func TestRemoveNowTakesTheJobsActivityUnlessABuildIsKept(t *testing.T) {
	st, logs := openStore(t)
	ctx := context.Background()
	now := time.Now()
	for _, job := range []string{"gone", "kept"} {
		seed(t, st, logs, job, 2, now)
		if err := st.AddActivity(ctx, job, "a line", now); err != nil {
			t.Fatal(err)
		}
	}
	keepBuild(t, st, "kept", 1, now)
	for _, job := range []string{"gone", "kept"} {
		if _, err := Remove(ctx, st, job, logs); err != nil {
			t.Fatal(err)
		}
	}
	if activity(t, st, "gone") != 0 || activity(t, st, "kept") != 1 {
		t.Fatalf("gone has %d lines, kept %d: want 0 and 1", activity(t, st, "gone"), activity(t, st, "kept"))
	}
}

func TestAnEmptyJobSetWithNothingToRemoveIsNotAnError(t *testing.T) {
	st, logs := openStore(t)
	ctx := context.Background()
	now := time.Now()
	seed(t, st, logs, "gone", 3, now)
	if n, err := Sweep(ctx, st, liveSnapshot(), logs, now.Add(time.Hour)); err != nil || n != 0 {
		t.Fatalf("err %v, removed %d", err, n)
	}
}

func TestAKeptBuildOfARemovedJobDoesNotCountAsAJobTheSetLost(t *testing.T) {
	st, logs := openStore(t)
	ctx := context.Background()
	now := time.Now()
	for _, job := range []string{"kept1", "kept2"} {
		seed(t, st, logs, job, 1, now)
		keepBuild(t, st, job, 1, now)
	}
	seed(t, st, logs, "gone", 2, now)
	n, err := Sweep(ctx, st, liveSnapshot("live"), logs, now.AddDate(0, 0, config.RemovedKeepDays+2))
	if err != nil || n != 2 {
		t.Fatalf("err %v, removed %d: want the 2 builds of the one removed job gone", err, n)
	}
}
