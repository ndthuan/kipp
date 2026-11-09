package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// The reads and the reload of an admin live below /admin (D20), and
// nowhere else.
func TestTheAdminRoutesLiveBelowAdminAndNowhereElse(t *testing.T) {
	e := newEnv(t)
	for _, name := range []string{"users", "users/admin", "secrets", "server", "engines", "runners", "cache", "audit"} {
		e.call(t, 200, e.admin, "GET", "/api/v1/admin/"+name, "")
		e.call(t, 403, e.viewer, "GET", "/api/v1/admin/"+name, "")
		e.asProblem(t, 404, "not_found", e.admin, "GET", "/api/v1/"+name, "")
	}
	e.call(t, 200, e.admin, "POST", "/api/v1/admin/reload", "")
	e.asProblem(t, 404, "not_found", e.admin, "POST", "/api/v1/reload", "")
}

func TestABuildIsFoundByItsNumberInTheJob(t *testing.T) {
	e := newEnv(t)
	id := e.trigger(t, "smoke", "")
	e.finished(t, id)
	byNumber := e.object(t, 200, e.viewer, "GET", "/api/v1/jobs/smoke/builds/1", "")
	byID := e.object(t, 200, e.viewer, "GET", "/api/v1/builds/"+id, "")
	eq(t, "the same build", canonical(t, mustJSON(t, byNumber)), canonical(t, mustJSON(t, byID)))
	for _, p := range []string{"/api/v1/jobs/smoke/builds/99", "/api/v1/jobs/smoke/builds/abc", "/api/v1/jobs/nosuch/builds/1"} {
		e.asProblem(t, 404, "not_found", e.viewer, "GET", p, "")
	}
	// A job the caller may not view has no builds to ask about.
	hidden := e.trigger(t, "gc", "")
	e.finished(t, hidden)
	e.asProblem(t, 404, "not_found", e.viewer, "GET", "/api/v1/jobs/gc/builds/1", "")
	e.call(t, 200, e.admin, "GET", "/api/v1/jobs/gc/builds/1", "")
	e.call(t, 401, "", "GET", "/api/v1/jobs/smoke/builds/1", "")
}

func TestCancelAnswers202WithTheBuild(t *testing.T) {
	e := newEnv(t)
	id := e.started(t, "slow")
	b := e.object(t, 202, e.admin, "POST", "/api/v1/builds/"+id+"/cancel", "")
	eq(t, "id", jsonInt(b["id"]), id)
	if _, ok := b["stages"]; !ok {
		t.Fatalf("not a build: %v", b)
	}
	e.finished(t, id)
}

func TestScanAnswers202WithHowTheScanStands(t *testing.T) {
	e := newEnv(t)
	builder := e.withScanner(t, "view", "build")
	scan := e.object(t, http.StatusAccepted, builder, "POST", "/api/v1/jobs/site/scan", "")
	for _, field := range []string{"token", "every_seconds", "running", "at", "result", "error"} {
		if _, ok := scan[field]; !ok {
			t.Fatalf("%s is not in %v", field, scan)
		}
	}
	e.api.deps.Scanner.Wait()
}

func TestAMetricStoreIsAnObjectAndItsRememberedValuesAreBelowIt(t *testing.T) {
	e := newEnv(t)
	metricJobs(t, e)
	carol := token(t, e.st, "carol", false, map[string][]string{"site-smoke": {"view"}})
	b := measuredBuild(t, e, "site-deploy", "seconds", map[string]float64{"a": 1})
	remember(t, e, "site", b, "seconds", map[string]float64{"a": 1, "b": 2})

	st := e.object(t, 200, carol, "GET", "/api/v1/metrics/site", "")
	eq[any](t, "store", st["store"], "site")
	eq[any](t, "orphaned", st["orphaned"], false)
	page, _ := e.page(t, carol, "/api/v1/metrics/site/remembered?metric=seconds")
	eq(t, "remembered", len(page), 2)

	// What carol may not see, and what is not there, is 404.
	e.asProblem(t, 404, "not_found", e.viewer, "GET", "/api/v1/metrics/site", "")
	e.asProblem(t, 404, "not_found", carol, "GET", "/api/v1/metrics/none", "")
	e.asProblem(t, 404, "not_found", e.viewer, "GET", "/api/v1/metrics/site/remembered", "")
}

func TestTheLogStreamIsAtEventsAndAcceptDoesNotChooseIt(t *testing.T) {
	e := newEnv(t)
	id := e.trigger(t, "smoke", "{}")
	e.finished(t, id)
	log := "/api/v1/builds/" + id + "/stages/s/log"

	got, h := e.call(t, 200, e.viewer, "GET", log+"/events", "")
	contains(t, h.Get("Content-Type"), "text/event-stream")
	contains(t, got, "data: tier=quick\n\n")
	contains(t, got, "event: end\n")

	// Accept is ignored: this is the chunk.
	got, h = e.call(t, 200, e.viewer, "GET", log, "", "Accept", sseAccept)
	contains(t, h.Get("Content-Type"), "text/plain")
	eq(t, "chunk", got, "tier=quick\ndone\n")
	if h.Get("X-Kipp-Offset") == "" {
		t.Fatal("a chunk names its offset")
	}
}

func TestALogOfAStageTheBuildDoesNotHaveIsNotFound(t *testing.T) {
	e := newEnv(t)
	id := e.trigger(t, "smoke", "{}")
	e.finished(t, id)
	for _, p := range []string{"/log", "/log/events", "/log?download=true"} {
		e.asProblem(t, 404, "not_found", e.viewer, "GET", "/api/v1/builds/"+id+"/stages/nosuch"+p, "")
	}
	// The consoles the server writes itself are the build's too.
	for _, name := range []string{"clone", "prepare", "hooks", "s"} {
		e.call(t, 200, e.viewer, "GET", "/api/v1/builds/"+id+"/stages/"+name+"/log", "")
	}
	e.call(t, 401, "", "GET", "/api/v1/builds/"+id+"/stages/s/log/events", "")
}

func TestBothStreamsSendTheSameKeepAliveComment(t *testing.T) {
	eq(t, "comment", keepAliveComment, ": keep-alive\n\n")
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	out, err := json.Marshal(v)
	must(t, err)
	return string(out)
}
