package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/store"
)

func TestGetBuildSaysWhichGreenRunAStageWasReusedFrom(t *testing.T) {
	e := newEnv(t, nil)
	s := e.session(t, e.admin)
	b := e.built(t, "smoke", nil)
	from := store.StageRef{Job: "ci.site.pr-12", Number: 3, Stage: "run"}
	if err := e.st.ReuseStage(context.Background(), b.ID, "t", "local", from, time.Now()); err != nil {
		t.Fatal(err)
	}
	var got getBuildOut
	ok(t, s, "get_build", map[string]any{"job": "smoke", "number": b.Number}, &got)
	byName := map[string]stageOut{}
	for _, st := range got.Stages {
		byName[st.Name] = st
	}
	r := byName["t"]
	if r.Reused == nil || *r.Reused != (reusedOut{Job: "ci.site.pr-12", Number: 3, Stage: "run"}) || r.Status != "success" || r.ExitCode != nil ||
		r.DurationSeconds == nil || *r.DurationSeconds != 0 {
		t.Fatalf("the reused stage reads %+v", r)
	}
	if byName["s"].Reused != nil {
		t.Fatalf("a stage that ran reads as reused: %+v", byName["s"])
	}
}
