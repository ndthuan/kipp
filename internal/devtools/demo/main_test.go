package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/overview"
	"github.com/ndthuan/kipp/internal/store"
)

var at0 = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

func demoIn(t *testing.T) (dir string, out string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "demo")
	var buf bytes.Buffer
	if err := run([]string{"-dir", dir, "-jobs", "30", "-days", "3", "-per-day", "6"}, &buf, at0); err != nil {
		t.Fatal(err)
	}
	return dir, buf.String()
}

func jobFile(t *testing.T, dir, name string) *config.Job {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "jobs", name+".yml"))
	if err != nil {
		t.Fatal(err)
	}
	j, err := config.ParseJob(name+".yml", raw)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestTheDemoMakesJobFilesTheServerReads(t *testing.T) {
	dir, _ := demoIn(t)
	files, err := filepath.Glob(filepath.Join(dir, "jobs", "*.yml"))
	if err != nil || len(files) != 31 {
		t.Fatalf("%d job files: %v", len(files), err)
	}
	groups := map[string]bool{}
	for _, f := range files {
		groups[jobFile(t, dir, strings.TrimSuffix(filepath.Base(f), ".yml")).Group] = true
	}
	if mb := jobFile(t, dir, multibranch); !mb.IsParent() {
		t.Fatalf("the multibranch job is %+v", mb)
	}
	if len(groups) < 4 || !groups[""] {
		t.Fatalf("groups %v: several, and the jobs with none", groups)
	}
}

func TestTheDemoStoreHasBuildsInOrderAndSomethingOfEachKind(t *testing.T) {
	dir, out := demoIn(t)
	st, err := store.Open(filepath.Join(dir, "data", "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ended, err := st.FinishedSince(context.Background(), at0.Add(-72*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(ended) < 200 || !strings.Contains(out, "30 jobs and ") {
		t.Fatalf("%d builds, said %q", len(ended), out)
	}
	kinds := map[string]int{}
	for _, b := range ended {
		kinds[kindOf(b)]++
		if b.Status != store.Queued && (b.StartedAt == nil || b.FinishedAt == nil || b.StartedAt.Before(b.QueuedAt) || b.FinishedAt.Before(*b.StartedAt)) {
			t.Fatalf("build %d has times out of order: %+v", b.ID, b)
		}
	}
	if kinds["queued"] == 0 || kinds["red"] == 0 || kinds["unstable"] == 0 {
		t.Fatalf("%v: the demo has something of each", kinds)
	}
}

func kindOf(b *store.Build) string {
	switch {
	case b.Status == store.Queued:
		return "queued"
	case b.Status.Red():
		return "red"
	case b.Status == store.Unstable:
		return "unstable"
	}
	return "other"
}

// groupOf is the group of a job, a child's being its parent's.
func groupOf(t *testing.T, dir, name string) string {
	t.Helper()
	parent, _, _ := strings.Cut(name, ".")
	return jobFile(t, dir, parent).Group
}

func TestTheOverviewOfTheDemoHasSomethingInEveryGraph(t *testing.T) {
	dir, _ := demoIn(t)
	st, err := store.Open(filepath.Join(dir, "data", "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	hist, err := st.RecentColoured(ctx, overview.Window)
	if err != nil {
		t.Fatal(err)
	}
	start, _ := overview.Bounds(overview.Day, time.UTC, at0)
	ended, err := st.FinishedSince(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	before, err := st.TalliesBetween(ctx, at0.Add(-48*time.Hour), at0.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var jobs []overview.Job
	for name, h := range hist {
		j := overview.Job{Name: name, Group: groupOf(t, dir, name), History: h, Before: before[name]}
		for _, b := range ended {
			if b.Job == name {
				j.Builds = append(j.Builds, b)
			}
		}
		jobs = append(jobs, j)
	}
	o := overview.Compute(jobs, overview.Config{Flips: overview.DefaultFlips}, at0)
	var finished, peak int
	for _, b := range o.Charts.Buckets {
		finished += b.Finished()
		peak = max(peak, b.MaxRunning)
	}
	if finished < 50 || peak < 2 || peak > slots || len(o.Charts.Groups) < 4 {
		t.Fatalf("finished %d, peak %d, %d groups", finished, peak, len(o.Charts.Groups))
	}
	if len(o.Failing) == 0 || len(o.Unstable) == 0 || len(o.Flaky) == 0 {
		t.Fatalf("%d failing, %d unstable, %d flaky", len(o.Failing), len(o.Unstable), len(o.Flaky))
	}
}

func TestTheMultibranchJobHasLiveAndGoneChildrenAndTheGoneOnesAreNoDisabledJob(t *testing.T) {
	dir, _ := demoIn(t)
	st, err := store.Open(filepath.Join(dir, "data", "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	kids, err := st.Children(context.Background(), multibranch)
	if err != nil {
		t.Fatal(err)
	}
	parent := jobFile(t, dir, multibranch)
	var jobs []overview.Job
	live, gone := 0, 0
	for _, c := range kids {
		child := parent.Child(config.ChildRecord{Parent: c.Parent, Key: c.Key, Kind: c.Kind, State: c.State})
		live, gone = live+1-btoi(child.Gone), gone+btoi(child.Gone)
		jobs = append(jobs, overview.Job{Name: child.Name, Group: child.Group, Disabled: child.Disabled, Gone: child.Gone, Child: true})
	}
	if live < 2 || gone < 2 {
		t.Fatalf("%d live and %d gone children", live, gone)
	}
	g := overview.Compute(jobs, overview.Config{}, at0).Groups[0]
	if g.Disabled != 0 || g.NeverBuilt != live {
		t.Fatalf("%+v", g)
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestTheSameSeedAndTimeMakeTheSameStore(t *testing.T) {
	a, _ := demoIn(t)
	b, _ := demoIn(t)
	sum := func(dir string) int {
		st, err := store.Open(filepath.Join(dir, "data", "db.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = st.Close() }()
		bs, err := st.FinishedSince(context.Background(), at0.Add(-72*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, x := range bs {
			n += int(x.ID) * len(x.Status)
		}
		return n
	}
	if sum(a) != sum(b) {
		t.Fatal("two runs of one seed differ")
	}
}

func TestTheDemoRefusesADirectoryThatExistsAndNonsense(t *testing.T) {
	var buf bytes.Buffer
	if err := run([]string{"-dir", t.TempDir()}, &buf, at0); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Fatalf("an existing directory: %v", err)
	}
	for _, args := range [][]string{{}, {"-dir", "x", "-jobs", "0"}, {"-nope"}} {
		if err := run(args, &buf, at0); err == nil {
			t.Fatalf("%v is accepted", args)
		}
	}
}
