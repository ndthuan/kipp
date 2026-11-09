package api

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/store"
)

// metricJobs are two jobs that share the store site, and one of its own.
func metricJobs(t *testing.T, e *env) {
	t.Helper()
	e.writeJob(t, "site-smoke", "name: site-smoke\nmetrics: {store: site}\nstages:\n  s: {run: 'true'}\n")
	e.writeJob(t, "site-deploy", "name: site-deploy\nmetrics: {store: site}\nstages:\n  s: {run: 'true'}\n")
}

// measuredBuild is a finished build of the job that reported values of
// the metric into the store site, from the stage measure.
func measuredBuild(t *testing.T, e *env, job, metric string, values map[string]float64) *store.Build {
	t.Helper()
	ctx := context.Background()
	// Made as ended, so that the dispatcher never runs it.
	b := &store.Build{Job: job, Status: store.Success, Trigger: "cli"}
	must(t, e.st.CreateBuild(ctx, b))
	must(t, e.st.SetBuildStatus(ctx, b.ID, store.Success, time.Now()))
	must(t, e.st.PutMetricHistory(ctx, b.ID, "site", "measure", []store.Metric{{Name: metric, Unit: "s", Values: values}}))
	return b
}

func remember(t *testing.T, e *env, st string, b *store.Build, metric string, values map[string]float64) {
	t.Helper()
	from := store.Origin{}
	if b != nil {
		from = store.Origin{BuildID: b.ID, Job: b.Job, Number: b.Number}
	}
	must(t, e.st.Remember(context.Background(), st, from, time.Now(), []store.Metric{{Name: metric, Unit: "s", Values: values}}))
}

func storeNames(rows []map[string]any) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r["store"].(string))
	}
	return out
}

func TestAStoreIsVisibleToWhoeverMayViewAJobThatNamesIt(t *testing.T) {
	e := newEnv(t)
	metricJobs(t, e)
	carol := token(t, e.st, "carol", false, map[string][]string{"site-smoke": {"view"}})
	b := measuredBuild(t, e, "site-deploy", "seconds", map[string]float64{"a": 1})
	remember(t, e, "site", b, "seconds", map[string]float64{"a": 1, "b": 2})
	remember(t, e, "site", b, "coverage", map[string]float64{"total": 87.19})

	stores := e.list(t, carol, "/api/v1/metrics")
	if len(stores) != 1 {
		t.Fatalf("carol's stores: %v", stores)
	}
	text, _ := json.Marshal(stores[0])
	eq(t, "carol's store", string(text), `{"jobs":["site-smoke"],"metrics":[{"items":1,"name":"coverage"},{"items":2,"name":"seconds"}],"orphaned":false,"store":"site"}`)
	admins := e.list(t, e.admin, "/api/v1/metrics")
	eq(t, "the admin's stores", len(admins), 1)
	eq(t, "the admin's jobs", strings.Join(func() []string {
		var out []string
		for _, j := range admins[0]["jobs"].([]any) {
			out = append(out, j.(string))
		}
		return out
	}(), ","), "site-deploy,site-smoke")

	values := e.list(t, carol, "/api/v1/metrics/site/remembered?metric=seconds")
	if len(values) != 2 || values[0]["item"] != "a" || values[1]["value"] != 2.0 || values[0]["unit"] != "s" {
		t.Fatalf("%v", values)
	}
	ref := values[0]["build"].(map[string]any)
	eq(t, "the build it came from", ref["job"].(string)+" #"+jsonInt(ref["number"]), "site-deploy #1")
	eq(t, "every metric", len(e.list(t, carol, "/api/v1/metrics/site/remembered")), 3)
	e.list(t, carol, "/api/v1/metrics/site/history?metric=seconds&item=a")

	// The viewer may view smoke alone, which names no store but its own.
	if got := e.list(t, e.viewer, "/api/v1/metrics"); len(got) != 0 {
		t.Fatalf("the viewer's stores: %v", got)
	}
	for _, path := range []string{"/api/v1/metrics/site", "/api/v1/metrics/site/history?metric=seconds&item=a",
		"/api/v1/metrics/site/compare?metric=seconds&from=" + strconv.FormatInt(b.ID, 10) + "&to=remembered"} {
		e.asProblem(t, 404, "not_found", e.viewer, "GET", path, "")
	}
	// carol may view the store, not the build of site-deploy.
	e.call(t, 404, carol, "GET", "/api/v1/metrics/site/compare?metric=seconds&from="+strconv.FormatInt(b.ID, 10)+"&to=remembered", "")
	e.call(t, 404, carol, "GET", "/api/v1/builds/"+strconv.FormatInt(b.ID, 10)+"/metrics", "")
}

func TestAStoreNoLoadedJobNamesIsAnAdminsOnly(t *testing.T) {
	e := newEnv(t)
	metricJobs(t, e)
	carol := token(t, e.st, "carol", false, map[string][]string{"site-smoke": {"view"}, "gone": {"view"}})
	remember(t, e, "old", nil, "seconds", map[string]float64{"a": 1})
	remember(t, e, "site", nil, "seconds", map[string]float64{"a": 1})

	stores := e.list(t, e.admin, "/api/v1/metrics")
	eq(t, "the admin's stores", strings.Join(storeNames(stores), ","), "old,site")
	eq(t, "old is orphaned", stores[0]["orphaned"], any(true))
	eq(t, "site is not", stores[1]["orphaned"], any(false))
	eq(t, "the admin reads old", len(e.list(t, e.admin, "/api/v1/metrics/old/remembered")), 1)
	eq(t, "carol's stores", strings.Join(storeNames(e.list(t, carol, "/api/v1/metrics")), ","), "site")
	e.call(t, 404, carol, "GET", "/api/v1/metrics/old", "")

	// Once no loaded job names site, carol's grant on a job file that is
	// gone shows her nothing of it.
	e.removeJob(t, "site-smoke")
	e.removeJob(t, "site-deploy")
	e.call(t, 404, carol, "GET", "/api/v1/metrics/site", "")
	if got := e.list(t, carol, "/api/v1/metrics"); len(got) != 0 {
		t.Fatalf("carol's stores: %v", got)
	}
	eq(t, "the admin's stores, now", strings.Join(storeNames(e.list(t, e.admin, "/api/v1/metrics")), ","), "old,site")
}

func TestAnUnknownStoreIs404(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/api/v1/metrics/nothing", "/api/v1/metrics/nothing/history?metric=seconds&item=a",
		"/api/v1/metrics/nothing/compare?metric=seconds&from=1&to=remembered", "/api/v1/metrics/Not%20A%20Name"} {
		e.asProblem(t, 404, "not_found", e.admin, "GET", path, "")
	}
	// A loaded job names its own store, which holds nothing yet.
	body, _ := e.call(t, 200, e.admin, "GET", "/api/v1/metrics/smoke/remembered", "")
	eq(t, "an empty store", strings.TrimSpace(body), `{"items":[],"next_cursor":null}`)
	e.call(t, 200, e.viewer, "GET", "/api/v1/metrics/smoke/remembered", "")
	e.call(t, 404, e.aide, "GET", "/api/v1/metrics/gc/remembered", "")
	e.call(t, 404, e.admin, "GET", "/api/v1/builds/999/metrics", "")
}

func TestHistoryPagesByCursorAndCapsAt500(t *testing.T) {
	e := newEnv(t)
	metricJobs(t, e)
	var ids []int64
	for i := range 510 {
		ids = append(ids, measuredBuild(t, e, "site-smoke", "seconds", map[string]float64{"a": float64(i), "b": 1}).ID)
	}
	path := "/api/v1/metrics/site/history?metric=seconds&item=a"
	first := e.list(t, e.admin, path)
	eq(t, "rows by default", len(first), 100)
	eq(t, "the newest first", first[0]["value"], any(509.0))
	eq(t, "and its build", jsonInt(first[0]["build"].(map[string]any)["id"]), strconv.FormatInt(ids[509], 10))
	eq(t, "its stage", first[0]["stage"], any("measure"))
	eq(t, "rows at most", len(e.list(t, e.admin, path+"&limit=1000")), 500)
	eq(t, "rows asked for", len(e.list(t, e.admin, path+"&limit=3")), 3)
	_, cursor := e.page(t, e.admin, path+"&limit=2")
	next, _ := e.page(t, e.admin, path+"&limit=2&cursor="+cursor)
	if len(next) != 2 || next[0]["value"] != 507.0 || next[1]["value"] != 506.0 {
		t.Fatalf("the page after: %v", next)
	}
	eq(t, "the end", len(e.list(t, e.admin, path+"&cursor="+cursorFor("history", ids[0], 0))), 0)
	for _, bad := range []string{"/api/v1/metrics/site/history?metric=seconds", "/api/v1/metrics/site/history?item=a"} {
		e.call(t, 400, e.admin, "GET", bad, "")
	}
}

func TestCompareAgainstRememberedSortsByTheLargestChange(t *testing.T) {
	e := newEnv(t)
	metricJobs(t, e)
	b := measuredBuild(t, e, "site-smoke", "seconds", map[string]float64{"a": 10, "b": 20, "c": 30, "z": 5, "y": 1})
	remember(t, e, "site", b, "seconds", map[string]float64{"a": 11, "b": 10, "c": 30, "z": 9, "y": -3})
	id := strconv.FormatInt(b.ID, 10)
	body, _ := e.call(t, 200, e.admin, "GET", "/api/v1/metrics/site/compare?metric=seconds&from="+id+"&to=remembered", "")
	eq(t, "the rows", strings.TrimSpace(body), `{"items":[{"item":"b","from":20,"to":10,"delta":-10},{"item":"y","from":1,"to":-3,"delta":-4},{"item":"z","from":5,"to":9,"delta":4},{"item":"a","from":10,"to":11,"delta":1},{"item":"c","from":30,"to":30,"delta":0}],"next_cursor":null}`)

	later := measuredBuild(t, e, "site-deploy", "seconds", map[string]float64{"a": 12, "b": 20})
	rows := e.list(t, e.admin, "/api/v1/metrics/site/compare?metric=seconds&from="+id+"&to="+strconv.FormatInt(later.ID, 10))
	eq(t, "build against build", rows[0]["item"].(string)+" "+strconv.FormatFloat(rows[0]["delta"].(float64), 'f', -1, 64), "a 2")
	for _, bad := range []string{"&from=" + id, "&from=" + id + "&to=x", "&from=remembered&to=remembered", "&to=remembered", "&from=x&to=remembered"} {
		e.call(t, 400, e.admin, "GET", "/api/v1/metrics/site/compare?metric=seconds"+bad, "")
	}
	e.call(t, 400, e.admin, "GET", "/api/v1/metrics/site/compare?from="+id+"&to=remembered", "")
	e.call(t, 404, e.admin, "GET", "/api/v1/metrics/site/compare?metric=seconds&from=999&to=remembered", "")
}

func TestACompareDeltaHasTheDecimalsOfItsValues(t *testing.T) {
	for _, c := range []struct{ from, to, want float64 }{{87.19, 86.9, -0.29}, {82.16, 73.97, -8.19}, {1, 2, 1}, {0.1, 0.3, 0.2}, {1e-9, 3e-9, 2e-9}} {
		if got := delta(c.from, c.to); got != c.want {
			t.Errorf("%v to %v: %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestCompareShowsAnItemOnOneSideAsNull(t *testing.T) {
	e := newEnv(t)
	metricJobs(t, e)
	b := measuredBuild(t, e, "site-smoke", "seconds", map[string]float64{"new": 3, "both": 1})
	remember(t, e, "site", b, "seconds", map[string]float64{"gone": 2, "both": 1, "also-gone": 5})
	body, _ := e.call(t, 200, e.admin, "GET", "/api/v1/metrics/site/compare?metric=seconds&from="+strconv.FormatInt(b.ID, 10)+"&to=remembered", "")
	eq(t, "the rows", strings.TrimSpace(body), `{"items":[{"item":"both","from":1,"to":1,"delta":0},{"item":"also-gone","from":null,"to":5,"delta":null},{"item":"gone","from":null,"to":2,"delta":null},{"item":"new","from":3,"to":null,"delta":null}],"next_cursor":null}`)
}

func TestBuildMetricsHoldsTheGuardResults(t *testing.T) {
	e := newEnv(t)
	metricJobs(t, e)
	ctx := context.Background()
	b := measuredBuild(t, e, "site-smoke", "coverage", map[string]float64{"total": 86.9})
	must(t, e.st.PutMetricHistory(ctx, b.ID, "site", "lint", []store.Metric{{Name: "kipp.stage_seconds", Unit: "s", Values: map[string]float64{"lint": 2}}}))
	was, now := 87.19, 86.9
	must(t, e.st.PutGuardResults(ctx, b.ID, []store.GuardRow{
		{Stage: "measure", Metric: "coverage", Item: "total", Remembered: &was, Reported: &now, Tolerance: 0.01, Verdict: "fell", Outcome: "failed", FromJob: "site-deploy", FromNumber: 123},
		{Stage: "check", Metric: "coverage", Item: "lib/a", Verdict: "missing", Outcome: "failed"},
	}))
	body, _ := e.call(t, 200, e.admin, "GET", "/api/v1/builds/"+strconv.FormatInt(b.ID, 10)+"/metrics", "")
	want := `{"build":{"id":` + strconv.FormatInt(b.ID, 10) + `,"job":"site-smoke","number":1,"status":"success"},"store":"site","stages":[` +
		`{"name":"measure","metrics":[{"name":"coverage","unit":"s","values":{"total":86.9}}],"guards":[{"metric":"coverage","item":"total","remembered":87.19,"reported":86.9,"tolerance":0.01,"verdict":"fell","outcome":"failed","from":{"id":0,"job":"site-deploy","number":123,"status":null},"line":"kipp: metrics guard: coverage total 87.19 -> 86.90 (-0.29), remembered from site-deploy #123, tolerance 0.01: failed"}]},` +
		`{"name":"lint","metrics":[{"name":"kipp.stage_seconds","unit":"s","values":{"lint":2}}],"guards":[]},` +
		`{"name":"check","metrics":[],"guards":[{"metric":"coverage","item":"lib/a","remembered":null,"reported":null,"tolerance":0,"verdict":"missing","outcome":"failed","from":null,"line":"kipp: metrics guard: coverage lib/a no baseline -> missing, tolerance 0: failed"}]}]}`
	// The API's JSON escapes > as every answer does.
	eq(t, "the build's metrics", strings.TrimSpace(body), strings.ReplaceAll(want, ">", `\u003e`))

	// A build that measured nothing names its job's store all the same.
	none := e.object(t, 200, e.admin, "GET", "/api/v1/builds/"+e.trigger(t, "smoke", `{"params":{"TIER":"quick"}}`)+"/metrics", "")
	eq(t, "the store of a build with nothing", none["store"], any("smoke"))
}

func TestTimesAreUTCWithZ(t *testing.T) {
	e := newEnv(t)
	metricJobs(t, e)
	b := measuredBuild(t, e, "site-smoke", "seconds", map[string]float64{"a": 1})
	remember(t, e, "site", b, "seconds", map[string]float64{"a": 1})
	var got []string
	for _, path := range []string{"/api/v1/metrics/site/remembered", "/api/v1/metrics/site/history?metric=seconds&item=a"} {
		body, _ := e.call(t, 200, e.admin, "GET", path, "")
		var v any
		must(t, json.Unmarshal([]byte(body), &v))
		before := len(got)
		stamps(v, &got)
		eq(t, path+" answers a time", len(got), before+1)
	}
	for _, s := range got {
		at, err := time.Parse(time.RFC3339Nano, s)
		if err != nil || !strings.HasSuffix(s, "Z") || time.Since(at).Abs() > time.Hour {
			t.Errorf("%q is not the moment in RFC 3339 in UTC with a Z (%v)", s, err)
		}
	}
}

func TestResultsAnswersTheBuildsTestCases(t *testing.T) {
	e := newEnv(t)
	e.writeJob(t, "cases", "name: cases\nstages:\n  s:\n    run: printf '%s' '<testsuite name=\"s\"><testcase name=\"fine\" time=\"0.5\"/><testcase name=\"bad\"><failure message=\"boom\"/></testcase></testsuite>' > r.xml\n    junit: r.xml\n")
	id := e.trigger(t, "cases", "")
	e.finished(t, id)
	eventually(t, func() bool { return len(e.list(t, e.admin, "/api/v1/builds/"+id+"/results")) == 2 })
	results, _ := e.call(t, 200, e.admin, "GET", "/api/v1/builds/"+id+"/results", "")
	contains(t, results, `"name":"bad"`)
	e.call(t, 404, e.viewer, "GET", "/api/v1/builds/"+id+"/results", "")
}
