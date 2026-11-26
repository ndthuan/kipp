package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/store"
)

func TestAStageReusedFromAGreenRunSaysWhichAndOneThatRanDoesNot(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := &store.Build{Job: "smoke", Status: store.Success, Trigger: "api"}
	must(t, e.st.CreateBuild(ctx, b))
	must(t, e.st.PutStages(ctx, b.ID, []string{"gate", "ship"}, nil))
	must(t, e.st.ReuseStage(ctx, b.ID, "gate", "local", store.StageRef{Job: "ci.site.pr-12", Number: 3, Stage: "run"}, time.Now()))
	var got struct {
		Stages []map[string]any `json:"stages"`
	}
	text, _ := e.call(t, 200, e.admin, "GET", "/api/v1/builds/"+itoa(b.ID), "")
	must(t, json.Unmarshal([]byte(text), &got))
	if len(got.Stages) != 2 {
		t.Fatalf("%s", text)
	}
	r, ok := got.Stages[0]["reused"].(map[string]any)
	if !ok || r["job"] != "ci.site.pr-12" || r["number"] != 3.0 || r["stage"] != "run" || got.Stages[0]["exit_code"] != nil || got.Stages[0]["status"] != "success" {
		t.Fatalf("%s", text)
	}
	if _, has := got.Stages[1]["reused"]; has {
		t.Fatalf("a stage that was not reused says reused: %s", text)
	}
}
