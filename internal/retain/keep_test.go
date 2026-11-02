package retain

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

// keepBuild marks the job's build with that number as kept.
func keepBuild(t *testing.T, st *store.Store, job string, number int, now time.Time) *store.Build {
	t.Helper()
	b, err := st.BuildByNumber(context.Background(), job, number)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.KeepBuild(context.Background(), b.ID, "ada", now); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAKeptBuildSurvivesTheCountAndTheDaysAndIsNotCountedToTheLimit(t *testing.T) {
	st, logs := openStore(t)
	ctx := context.Background()
	now := time.Now()
	seed(t, st, logs, "a", 6, now) // #1 is six days old, #6 a day
	keepBuild(t, st, "a", 1, now)
	keepBuild(t, st, "a", 6, now)
	job := parse(t, "name: a\nkeep: {builds: 2}\nstages:\n  s: {run: true}\n")
	if err := Job(ctx, st, job, logs, now); err != nil {
		t.Fatal(err)
	}
	left, _ := st.Builds(ctx, "a", 10)
	var numbers []int
	for _, b := range left {
		numbers = append(numbers, b.Number)
	}
	// The kept #1 and #6, and the newest two of the others.
	if want := []int{6, 5, 4, 1}; len(numbers) != len(want) || numbers[0] != 6 || numbers[1] != 5 || numbers[2] != 4 || numbers[3] != 1 {
		t.Fatalf("left %v, want %v", numbers, want)
	}
	if !exists(filepath.Join(logs, "a", "1")) || exists(filepath.Join(logs, "a", "2")) {
		t.Fatal("the kept build lost its logs, or an unkept one kept them")
	}
	byDays := parse(t, "name: a\nkeep: {days: 3}\nstages:\n  s: {run: true}\n")
	if err := Job(ctx, st, byDays, logs, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BuildByNumber(ctx, "a", 1); err != nil {
		t.Fatalf("a kept build older than the days limit was removed: %v", err)
	}
}

func TestStopKeepingPutsABuildBackUnderTheRule(t *testing.T) {
	st, logs := openStore(t)
	ctx := context.Background()
	now := time.Now()
	seed(t, st, logs, "a", 3, now)
	b := keepBuild(t, st, "a", 1, now)
	job := parse(t, "name: a\nkeep: {builds: 1}\nstages:\n  s: {run: true}\n")
	if err := Job(ctx, st, job, logs, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Build(ctx, b.ID); err != nil {
		t.Fatal("the kept build was removed")
	}
	if err := st.UnkeepBuild(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if err := Job(ctx, st, job, logs, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Build(ctx, b.ID); err == nil {
		t.Fatal("a build that is no longer kept outlived the rule")
	}
}

func TestAKeptBuildOfARemovedJobStaysWithItsLogs(t *testing.T) {
	st, logs := openStore(t)
	ctx := context.Background()
	now := time.Now()
	seed(t, st, logs, "gone", 3, now)
	keepBuild(t, st, "gone", 1, now)
	snap := &config.Snapshot{Jobs: map[string]*config.Job{}}
	n, err := Sweep(ctx, st, snap, logs, now.AddDate(0, 0, config.RemovedKeepDays+5))
	if err != nil || n != 2 {
		t.Fatalf("err %v, removed %d: want the two unkept builds gone", err, n)
	}
	if !exists(filepath.Join(logs, "gone", "1")) {
		t.Fatal("the kept build's logs went with its removed job")
	}
	if _, err := st.BuildByNumber(ctx, "gone", 1); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactsMaxSkipsAKeptBuildAndMayStayOverTheCap(t *testing.T) {
	st, logs := openStore(t)
	ctx := context.Background()
	now := time.Now()
	arts := ArtifactsDir(logs)
	seed(t, st, logs, "a", 3, now)
	builds, _ := st.Builds(ctx, "a", 10) // newest first: 3, 2, 1
	for _, b := range builds {
		keepFile(t, st, arts, b, 10)
	}
	keepBuild(t, st, "a", 1, now) // the oldest, which the cap would take first
	n, err := ArtifactsOver(ctx, st, arts, 5, now)
	if err != nil || n != 2 {
		t.Fatalf("%d %v: want the two unkept builds' artifacts gone", n, err)
	}
	keeps(t, st, arts, map[*store.Build]bool{builds[2]: true, builds[1]: false, builds[0]: false})
}
