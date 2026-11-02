package api

import (
	"testing"
	"time"
)

func TestKeepMarksAFinishedBuildWithWhoAndWhenAndStopKeepingClearsIt(t *testing.T) {
	e := newEnv(t)
	id := e.trigger(t, "smoke", "")
	e.finished(t, id)
	before := e.object(t, 200, e.agent, "GET", "/api/v1/builds/"+id, "")
	if before["kept"] != nil {
		t.Fatalf("a build nobody kept is kept: %v", before["kept"])
	}
	kept := e.object(t, 200, e.agent, "PUT", "/api/v1/builds/"+id+"/keep", "")
	by := kept["kept"].(map[string]any)
	eq(t, "kept by", by["by"], any("agent"))
	if _, err := time.Parse(time.RFC3339Nano, by["at"].(string)); err != nil {
		t.Fatal(by)
	}
	eq(t, "stages come with the build", len(kept["stages"].([]any)) > 0, true)
	again := e.object(t, 200, e.agent, "GET", "/api/v1/builds/"+id, "")
	eq(t, "kept on the next read", again["kept"].(map[string]any)["by"], any("agent"))
	listed := e.object(t, 200, e.agent, "GET", "/api/v1/jobs/smoke/builds", "")["items"].([]any)
	eq(t, "kept in the list", listed[0].(map[string]any)["kept"].(map[string]any)["by"], any("agent"))

	cleared := e.object(t, 200, e.agent, "DELETE", "/api/v1/builds/"+id+"/keep", "")
	if cleared["kept"] != nil {
		t.Fatalf("still kept: %v", cleared["kept"])
	}
	act := e.list(t, e.admin, "/api/v1/jobs/smoke/activity")
	eq(t, "activity lines", len(act), 2)
	lines := act[0]["line"].(string) + "\n" + act[1]["line"].(string)
	contains(t, lines, "#1 kept by agent")
	contains(t, lines, "#1 no longer kept, by agent")
}

func TestKeepNeedsTheBuildPermissionAndAKnownBuild(t *testing.T) {
	e := newEnv(t)
	id := e.trigger(t, "smoke", "")
	e.finished(t, id)
	for _, method := range []string{"PUT", "DELETE"} {
		e.asProblem(t, 403, "forbidden", e.viewer, method, "/api/v1/builds/"+id+"/keep", "")
		e.asProblem(t, 404, "not_found", e.admin, method, "/api/v1/builds/99999/keep", "")
		e.call(t, 401, "", method, "/api/v1/builds/"+id+"/keep", "")
	}
	after := e.object(t, 200, e.admin, "GET", "/api/v1/builds/"+id, "")
	if after["kept"] != nil {
		t.Fatalf("a refused request kept the build: %v", after["kept"])
	}
}

func TestKeepIsAConflictWhileTheBuildRuns(t *testing.T) {
	e := newEnv(t)
	id := e.started(t, "slow")
	e.asProblem(t, 409, "conflict", e.admin, "PUT", "/api/v1/builds/"+id+"/keep", "")
	e.asProblem(t, 409, "conflict", e.admin, "DELETE", "/api/v1/builds/"+id+"/keep", "")
	e.call(t, 202, e.admin, "POST", "/api/v1/builds/"+id+"/cancel", "")
	e.finished(t, id)
}

func TestKeepingTwiceOrStoppingTwiceIsFineAndKeepsTheFirstKeeper(t *testing.T) {
	e := newEnv(t)
	id := e.trigger(t, "smoke", "")
	e.finished(t, id)
	e.call(t, 200, e.agent, "DELETE", "/api/v1/builds/"+id+"/keep", "")
	e.call(t, 200, e.admin, "PUT", "/api/v1/builds/"+id+"/keep", "")
	second := e.object(t, 200, e.agent, "PUT", "/api/v1/builds/"+id+"/keep", "")
	eq(t, "the first keeper stays", second["kept"].(map[string]any)["by"], any("admin"))
	act := e.list(t, e.admin, "/api/v1/jobs/smoke/activity")
	eq(t, "one line for the one change", len(act), 1)
}
