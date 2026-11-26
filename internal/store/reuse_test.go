package store

import (
	"testing"
	"time"
)

type green struct {
	job, stage, tree, fp, class string
	status                      StageStatus
}

func (g green) put(t *testing.T, s *Store) *Build {
	t.Helper()
	b := &Build{Job: g.job, Status: Success, Trigger: "cli"}
	must(t, s.CreateBuild(ctx, b))
	must(t, s.PutStages(ctx, b.ID, []string{g.stage}, nil))
	must(t, s.SetStage(ctx, b.ID, g.stage, g.status, new(0), time.Now()))
	must(t, s.SetStageRun(ctx, b.ID, g.stage, g.class, ""))
	must(t, s.SetBuildTree(ctx, b.ID, g.tree))
	if g.fp != "" {
		must(t, s.SetBuildFingerprint(ctx, b.ID, "go1", g.fp))
	}
	return b
}

func ask() GreenQuery {
	return GreenQuery{Tree: "tree-a", FingerprintSum: "fp-a", Class: "local", From: []string{"ci.site"}, Stage: "run", Job: "deploy", JobStage: "gate"}
}

func TestAGreenRunMatchesOnTreeFingerprintClassAndSuccess(t *testing.T) {
	base := green{job: "ci.site.pr-12", stage: "run", tree: "tree-a", fp: "fp-a", class: "local", status: StageSuccess}
	for name, c := range map[string]struct {
		change func(*green)
		match  bool
	}{
		"equal in all":                   {func(*green) {}, true},
		"the node's own build":           {func(g *green) { g.job = "ci.site" }, true},
		"another tree":                   {func(g *green) { g.tree = "tree-b" }, false},
		"another fingerprint":            {func(g *green) { g.fp = "fp-b" }, false},
		"no fingerprint":                 {func(g *green) { g.fp = "" }, false},
		"another runner class":           {func(g *green) { g.class = "runs_on linux" }, false},
		"a stage ended unstable":         {func(g *green) { g.status = StageUnstable }, false},
		"a stage ended failed":           {func(g *green) { g.status = StageFailed }, false},
		"another stage":                  {func(g *green) { g.stage = "lint" }, false},
		"a job not under from":           {func(g *green) { g.job = "ci.sitex.pr-1" }, false},
		"a job that only starts like it": {func(g *green) { g.job = "ci.site-old" }, false},
	} {
		s := open(t)
		g := base
		c.change(&g)
		b := g.put(t, s)
		got, err := s.GreenStage(ctx, ask())
		must(t, err)
		if c.match != (got != nil) {
			t.Errorf("%s: matched %+v, want a match %v", name, got, c.match)
		}
		if got != nil && (got.Job != b.Job || got.Number != b.Number || got.Stage != g.stage) {
			t.Errorf("%s: matched %+v", name, got)
		}
	}
}

func TestTheOwnJobMatchesOnTheStagesOwnNameAndTheNewestWins(t *testing.T) {
	s := open(t)
	green{job: "deploy", stage: "run", tree: "tree-a", fp: "fp-a", class: "local", status: StageSuccess}.put(t, s)
	if got, _ := s.GreenStage(ctx, ask()); got != nil {
		t.Fatalf("a stage of the job's own named as from's stage matched: %+v", got)
	}
	green{job: "ci.site.main", stage: "run", tree: "tree-a", fp: "fp-a", class: "local", status: StageSuccess}.put(t, s)
	own := green{job: "deploy", stage: "gate", tree: "", fp: "fp-a", class: "local", status: StageSuccess}.put(t, s)
	must(t, s.SetStageRun(ctx, own.ID, "gate", "local", "tree-a"))
	got, err := s.GreenStage(ctx, ask())
	must(t, err)
	if got == nil || got.Job != "deploy" || got.Number != own.Number || got.Stage != "gate" {
		t.Fatalf("matched %+v, want the job's own newer gate, by the stage's tree", got)
	}
	q := ask()
	q.Build = own.ID
	if got, _ := s.GreenStage(ctx, q); got == nil || got.Job != "ci.site.main" {
		t.Fatalf("the asking build matched itself, or nothing older: %+v", got)
	}
}

func TestAReusedStageIsNoGreenRunAndReadsWhereItCameFrom(t *testing.T) {
	s := open(t)
	b := &Build{Job: "deploy", Status: Success, Trigger: "cli"}
	must(t, s.CreateBuild(ctx, b))
	must(t, s.PutStages(ctx, b.ID, []string{"gate"}, nil))
	must(t, s.SetBuildFingerprint(ctx, b.ID, "go1", "fp-a"))
	must(t, s.SetBuildTree(ctx, b.ID, "tree-a"))
	at := time.Now()
	must(t, s.ReuseStage(ctx, b.ID, "gate", "local", StageRef{Job: "ci.site.pr-1", Number: 3, Stage: "run"}, at))
	if got, _ := s.GreenStage(ctx, ask()); got != nil {
		t.Fatalf("a reused stage was found as a green run: %+v", got)
	}
	st, err := s.Stages(ctx, b.ID)
	must(t, err)
	g := st[0]
	if g.Status != StageSuccess || g.ExitCode != nil || g.Reused == nil || *g.Reused != (StageRef{Job: "ci.site.pr-1", Number: 3, Stage: "run"}) ||
		g.StartedAt == nil || g.FinishedAt == nil || !g.StartedAt.Equal(*g.FinishedAt) {
		t.Fatalf("the reused stage reads %+v", g)
	}
}
