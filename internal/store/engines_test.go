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

func TestAnAgentIsFoundByTheHashOfItsToken(t *testing.T) {
	s := open(t)
	must(t, s.PutAgent(ctx, Agent{Name: "build-1", Labels: []string{"linux"}, Executors: 2, Enabled: true, TokenHash: "h1"}))
	a, err := s.AgentByTokenHash(ctx, "h1")
	must(t, err)
	if a.Name != "build-1" || a.Executors != 2 || !a.Enabled || !slices.Equal(a.Labels, []string{"linux"}) || a.LastSeenAt != nil {
		t.Fatalf("%+v", a)
	}
	if _, err := s.AgentByTokenHash(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.PutAgent(ctx, Agent{Name: "build-2", Executors: 1, TokenHash: "h1"}); err == nil {
		t.Fatal("two agents with one token hash")
	}
	at := time.Now()
	must(t, s.SetAgentSeen(ctx, "build-1", at, "linux", "amd64", "v0.2.0"))
	a, err = s.Agent(ctx, "build-1")
	must(t, err)
	if a.LastSeenAt == nil || !a.LastSeenAt.Equal(at.UTC().Truncate(time.Nanosecond)) || a.OS != "linux" || a.Arch != "amd64" || a.Version != "v0.2.0" {
		t.Fatalf("%+v", a)
	}
	if err := s.SetAgentSeen(ctx, "gone", at, "", "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

// A new token for an agent is a replace of its row: what the server saw
// of it stays, and the old hash finds nothing.
func TestReplacingAnAgentKeepsWhatWasSeenOfIt(t *testing.T) {
	s := open(t)
	must(t, s.PutAgent(ctx, Agent{Name: "a", Executors: 1, Enabled: true, TokenHash: "old"}))
	must(t, s.SetAgentSeen(ctx, "a", time.Now(), "linux", "arm64", "v1"))
	a, err := s.Agent(ctx, "a")
	must(t, err)
	a.TokenHash = "new"
	must(t, s.PutAgent(ctx, *a))
	if _, err := s.AgentByTokenHash(ctx, "old"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	b, err := s.AgentByTokenHash(ctx, "new")
	must(t, err)
	if b.OS != "linux" || b.LastSeenAt == nil || !b.CreatedAt.Equal(a.CreatedAt) {
		t.Fatalf("%+v", b)
	}
	all, err := s.Agents(ctx)
	must(t, err)
	if len(all) != 1 {
		t.Fatalf("%+v", all)
	}
	must(t, s.DeleteAgent(ctx, "a"))
	if err := s.DeleteAgent(ctx, "a"); !errors.Is(err, ErrNotFound) {
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
