package store

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/waitfor"
)

func TestAnEngineIsKeptByNameAndReplacedKeepingWhenItWasAdded(t *testing.T) {
	s := open(t)
	e := Engine{Name: "mac", Kind: "docker", URL: "ssh://me@host?socket=/d.sock", Labels: []string{"linux", "arm64"}, Max: 2, Enabled: true, SSHKey: "/k"}
	must(t, s.PutEngine(ctx, e))
	must(t, s.PutEngine(ctx, Engine{Name: "box", Kind: "docker", URL: "unix:///var/run/docker.sock", Max: 1}))
	got, err := s.Engine(ctx, "mac")
	must(t, err)
	if got.URL != e.URL || !slices.Equal(got.Labels, e.Labels) || got.Max != 2 || !got.Enabled || got.SSHKey != "/k" || got.CreatedAt.IsZero() {
		t.Fatalf("%+v", got)
	}
	added := got.CreatedAt
	// The next write is stamped later than the first, to the millisecond.
	waitfor.Until(t, "the clock moves on a millisecond", func() bool { return time.Now().After(added.Add(time.Millisecond)) })
	e.Max, e.Enabled = 3, false
	must(t, s.PutEngine(ctx, e))
	got, err = s.Engine(ctx, "mac")
	must(t, err)
	if got.Max != 3 || got.Enabled || !got.CreatedAt.Equal(added) || !got.UpdatedAt.After(added) {
		t.Fatalf("replaced %+v", got)
	}
	all, err := s.Engines(ctx)
	must(t, err)
	if len(all) != 2 || all[0].Name != "box" || all[1].Name != "mac" || all[0].Labels == nil {
		t.Fatalf("%+v", all)
	}
	must(t, s.DeleteEngine(ctx, "mac"))
	if _, err := s.Engine(ctx, "mac"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.DeleteEngine(ctx, "mac"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestAnRunnerIsFoundByTheHashOfItsToken(t *testing.T) {
	s := open(t)
	must(t, s.PutRunner(ctx, Runner{Name: "build-1", Labels: []string{"linux"}, Executors: 2, Enabled: true, TokenHash: "h1"}))
	a, err := s.RunnerByTokenHash(ctx, "h1")
	must(t, err)
	if a.Name != "build-1" || a.Executors != 2 || !a.Enabled || !slices.Equal(a.Labels, []string{"linux"}) || a.LastSeenAt != nil {
		t.Fatalf("%+v", a)
	}
	if _, err := s.RunnerByTokenHash(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.PutRunner(ctx, Runner{Name: "build-2", Executors: 1, TokenHash: "h1"}); err == nil {
		t.Fatal("two runners with one token hash")
	}
	at := time.Now()
	must(t, s.SetRunnerSeen(ctx, "build-1", at, "linux", "amd64", "v0.2.0"))
	a, err = s.Runner(ctx, "build-1")
	must(t, err)
	if a.LastSeenAt == nil || !a.LastSeenAt.Equal(at.UTC().Truncate(time.Nanosecond)) || a.OS != "linux" || a.Arch != "amd64" || a.Version != "v0.2.0" {
		t.Fatalf("%+v", a)
	}
	if err := s.SetRunnerSeen(ctx, "gone", at, "", "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

// A new token for a runner is a replace of its row: what the server saw
// of it stays, and the old hash finds nothing.
func TestReplacingAnRunnerKeepsWhatWasSeenOfIt(t *testing.T) {
	s := open(t)
	must(t, s.PutRunner(ctx, Runner{Name: "a", Executors: 1, Enabled: true, TokenHash: "old"}))
	must(t, s.SetRunnerSeen(ctx, "a", time.Now(), "linux", "arm64", "v1"))
	a, err := s.Runner(ctx, "a")
	must(t, err)
	a.TokenHash = "new"
	must(t, s.PutRunner(ctx, *a))
	if _, err := s.RunnerByTokenHash(ctx, "old"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	b, err := s.RunnerByTokenHash(ctx, "new")
	must(t, err)
	if b.OS != "linux" || b.LastSeenAt == nil || !b.CreatedAt.Equal(a.CreatedAt) {
		t.Fatalf("%+v", b)
	}
	all, err := s.Runners(ctx)
	must(t, err)
	if len(all) != 1 {
		t.Fatalf("%+v", all)
	}
	must(t, s.DeleteRunner(ctx, "a"))
	if err := s.DeleteRunner(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestAStageSaysWhereItRan(t *testing.T) {
	s := open(t)
	b := &Build{Job: "a", Status: Running, Trigger: "cli"}
	must(t, s.CreateBuild(ctx, b))
	must(t, s.PutStages(ctx, b.ID, []string{"x", "y"}, nil))
	must(t, s.SetStageRanOn(ctx, b.ID, "x", "mac/1"))
	st, err := s.Stages(ctx, b.ID)
	must(t, err)
	if st[0].RanOn != "mac/1" || st[1].RanOn != "" {
		t.Fatalf("%+v %+v", st[0], st[1])
	}
	if err := s.SetStageRanOn(ctx, b.ID, "z", "local"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestAStageSaysHowManyTimesItsCommandRan(t *testing.T) {
	s := open(t)
	b := &Build{Job: "a", Status: Running, Trigger: "cli"}
	must(t, s.CreateBuild(ctx, b))
	must(t, s.PutStages(ctx, b.ID, []string{"x", "y"}, nil))
	must(t, s.SetStageTries(ctx, b.ID, "x", 3))
	st, err := s.Stages(ctx, b.ID)
	must(t, err)
	if st[0].Tries != 3 || st[1].Tries != 1 {
		t.Fatalf("tries %d and %d: 1 is a command that ran once", st[0].Tries, st[1].Tries)
	}
	if err := s.SetStageTries(ctx, b.ID, "z", 2); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestStoppedStagesAreTheFailedAndTheCancelledOfEveryBuildAskedForInOneQuery(t *testing.T) {
	s := open(t)
	var ids []int64
	for range 3 {
		b := &Build{Job: "a", Status: Running, Trigger: "cli"}
		must(t, s.CreateBuild(ctx, b))
		must(t, s.PutStages(ctx, b.ID, []string{"x", "y", "z"}, nil))
		ids = append(ids, b.ID)
	}
	code := 2
	must(t, s.SetStage(ctx, ids[0], "x", StageSuccess, nil, time.Now()))
	must(t, s.SetStage(ctx, ids[0], "y", StageFailed, &code, time.Now()))
	must(t, s.SetStage(ctx, ids[1], "z", StageCancelled, nil, time.Now()))
	got, err := s.StoppedStages(ctx, ids)
	must(t, err)
	if len(got) != 2 || len(got[ids[0]]) != 1 || got[ids[0]][0].Name != "y" || *got[ids[0]][0].ExitCode != 2 {
		t.Fatalf("%+v", got)
	}
	if len(got[ids[1]]) != 1 || got[ids[1]][0].Name != "z" || len(got[ids[2]]) != 0 {
		t.Fatalf("%+v", got)
	}
	none, err := s.StoppedStages(ctx, nil)
	must(t, err)
	if len(none) != 0 {
		t.Fatal(none)
	}
}
