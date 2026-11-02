package store

import (
	"testing"
	"time"
)

func TestAKeptBuildSaysWhoKeptItAndWhen(t *testing.T) {
	s := open(t)
	b := &Build{Job: "a", Status: Success, Trigger: "cli"}
	must(t, s.CreateBuild(ctx, b))
	got, err := s.Build(ctx, b.ID)
	must(t, err)
	if got.KeptBy != "" || got.KeptAt != nil {
		t.Fatalf("a new build is kept: %+v", got)
	}
	at := time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC)
	must(t, s.KeepBuild(ctx, b.ID, "ada", at))
	got, err = s.Build(ctx, b.ID)
	must(t, err)
	if got.KeptBy != "ada" || got.KeptAt == nil || !got.KeptAt.Equal(at) {
		t.Fatalf("%+v", got)
	}
	must(t, s.UnkeepBuild(ctx, b.ID))
	got, err = s.Build(ctx, b.ID)
	must(t, err)
	if got.KeptBy != "" || got.KeptAt != nil {
		t.Fatalf("still kept: %+v", got)
	}
	if err := s.KeepBuild(ctx, b.ID+99, "ada", at); err == nil {
		t.Fatal("kept a build that is not there")
	}
}

func TestOldBuildsNeverOffersAKeptBuildAndDoesNotCountItToTheLimit(t *testing.T) {
	s := open(t)
	now := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	var ids []int64
	for i := range 6 {
		b := &Build{Job: "a", Status: Success, Trigger: "cli", QueuedAt: now.AddDate(0, 0, -10+i)}
		must(t, s.CreateBuild(ctx, b))
		ids = append(ids, b.ID)
	}
	// The oldest and the newest are kept: the newest 2 of the others stay.
	must(t, s.KeepBuild(ctx, ids[0], "ada", now))
	must(t, s.KeepBuild(ctx, ids[5], "ada", now))
	byCount, err := s.OldBuilds(ctx, "a", 2, 0, now)
	must(t, err)
	if len(byCount) != 2 || byCount[0] != ids[1] || byCount[1] != ids[2] {
		t.Fatalf("by count: %v, want %v", byCount, ids[1:3])
	}
	byAge, err := s.OldBuilds(ctx, "a", 0, 8, now)
	must(t, err)
	if len(byAge) != 1 || byAge[0] != ids[1] {
		t.Fatalf("by age: %v, want only %d", byAge, ids[1])
	}
	must(t, s.UnkeepBuild(ctx, ids[0]))
	byAge, err = s.OldBuilds(ctx, "a", 0, 8, now)
	must(t, err)
	if len(byAge) != 2 || byAge[0] != ids[0] {
		t.Fatalf("stop keeping did not put the build back under the rules: %v", byAge)
	}
}

func TestArtifactBuildsLeavesAKeptBuildOutButCountsItsBytes(t *testing.T) {
	s := open(t)
	var ids []int64
	for range 2 {
		b := &Build{Job: "a", Status: Success, Trigger: "cli"}
		must(t, s.CreateBuild(ctx, b))
		must(t, s.PutArtifacts(ctx, b.ID, "s", []Artifact{{Stage: "s", Path: "out.bin", Size: 10, SHA256: "0"}}))
		ids = append(ids, b.ID)
	}
	must(t, s.KeepBuild(ctx, ids[0], "ada", time.Now()))
	builds, total, err := s.ArtifactBuilds(ctx)
	must(t, err)
	if total != 20 || len(builds) != 1 || builds[0].ID != ids[1] {
		t.Fatalf("%d %+v", total, builds)
	}
}
