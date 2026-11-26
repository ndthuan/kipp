package api

import "testing"

func TestBuildAgainQueuesACopyAndNamesTheOriginal(t *testing.T) {
	e := newEnv(t)
	id := e.trigger(t, "smoke", `{"params": {"TIER": "full"}}`)
	e.finished(t, id)
	b := e.object(t, 202, e.aide, "POST", "/api/v1/builds/"+id+"/again", "")
	eq(t, "number", b["number"], any(float64(2)))
	eq(t, "trigger", b["trigger"], any("api"))
	eq(t, "trigger_detail", b["trigger_detail"], any("again #1"))
	eq(t, "started_by", b["started_by"], any("aide"))
	done := e.object(t, 200, e.aide, "GET", "/api/v1/builds/"+jsonInt(b["id"])+"?wait=true", "")
	original := e.object(t, 200, e.aide, "GET", "/api/v1/builds/"+id, "")
	eq(t, "params", canonical(t, mustJSON(t, done["params"])), canonical(t, mustJSON(t, original["params"])))
}

func TestBuildAgainNeedsTheBuildPermissionAndAKnownBuild(t *testing.T) {
	e := newEnv(t)
	id := e.trigger(t, "smoke", "")
	e.finished(t, id)
	e.asProblem(t, 403, "forbidden", e.viewer, "POST", "/api/v1/builds/"+id+"/again", "")
	e.asProblem(t, 404, "not_found", e.admin, "POST", "/api/v1/builds/99999/again", "")
	e.call(t, 401, "", "POST", "/api/v1/builds/"+id+"/again", "")
	eq(t, "builds after the refusals", len(e.list(t, e.admin, "/api/v1/jobs/smoke/builds")), 1)
}

func TestBuildAgainIsAConflictWhileTheBuildRuns(t *testing.T) {
	e := newEnv(t)
	id := e.started(t, "slow")
	e.asProblem(t, 409, "conflict", e.admin, "POST", "/api/v1/builds/"+id+"/again", "")
	e.call(t, 202, e.admin, "POST", "/api/v1/builds/"+id+"/cancel", "")
	e.finished(t, id)
}

func TestBuildAgainIsRefusedWhileTheServerIsQuiet(t *testing.T) {
	e := newEnv(t)
	id := e.trigger(t, "smoke", "")
	e.finished(t, id)
	e.call(t, 200, e.admin, "POST", "/api/v1/admin/quiet", "")
	e.asProblem(t, 503, "quiet", e.admin, "POST", "/api/v1/builds/"+id+"/again", "")
}
