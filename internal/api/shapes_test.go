package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/agenthub"
	"github.com/ndthuan/kipp/internal/cachesrv"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/enginepool"
	"github.com/ndthuan/kipp/internal/index"
	"github.com/ndthuan/kipp/internal/store"
)

var (
	// millis is the one form of a v1 timestamp: UTC, three fraction digits.
	millis = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`)
	// stampLike is anything that reads as the start of a timestamp.
	stampLike = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d`)
)

// stampsIn is every string under v that reads as a timestamp, by its path.
func stampsIn(v any, path string, out map[string]string) {
	switch x := v.(type) {
	case string:
		if stampLike.MatchString(x) {
			out[path] = x
		}
	case []any:
		for i, e := range x {
			stampsIn(e, fmt.Sprintf("%s[%d]", path, i), out)
		}
	case map[string]any:
		for k, e := range x {
			stampsIn(e, path+"."+k, out)
		}
	}
}

// keysOf is the field names of an object, sorted.
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// eqKeys is that the object has the fields in want, a comma-separated list
// in any order, and no other.
func eqKeys(t *testing.T, what string, m map[string]any, want string) {
	t.Helper()
	parts := strings.Split(want, ",")
	slices.Sort(parts)
	eq(t, what, strings.Join(keysOf(m), ","), strings.Join(parts, ","))
}

func (e *env) any(t *testing.T, tok, path string) any {
	t.Helper()
	text, _ := e.call(t, 200, tok, "GET", path, "")
	var v any
	must(t, json.Unmarshal([]byte(text), &v))
	return v
}

// shapeEnv is a server with something of everything a v1 answer shows: an
// owner job and its children, builds with times of nanoseconds, a build's
// metrics and guards, an engine, an agent that runs a stage, a cache entry
// and an audit row. It returns the owner job's token as well.
func shapeEnv(t *testing.T) (*env, string) {
	t.Helper()
	since := time.Date(2026, 10, 24, 14, 2, 0, 123456789, time.UTC)
	var e *env
	e = newEnv(t, func(a *API) {
		a.deps.Cache = cachesrv.New(cachesrv.Options{Dir: t.TempDir(), Store: a.deps.Store})
		a.deps.Engines = func() []enginepool.EngineStatus {
			return []enginepool.EngineStatus{{Name: "mac", Kind: "docker", URL: "ssh://me@host", Labels: []string{"linux"}, Max: 2, Enabled: true, Reachable: true, Since: since, Version: "27.3.1", Running: []string{"mac/1"}}}
		}
		a.deps.Agents = func() []agenthub.Status {
			b, err := e.st.Build(context.Background(), 1)
			if err != nil {
				return nil
			}
			return []agenthub.Status{{Name: "box", Online: true, Enabled: true, Since: since, Seen: since, Version: "test", OS: "linux", Arch: "amd64", Executors: 2, Busy: 1,
				Stages: []string{b.Job + "#" + fmt.Sprint(b.Number) + "/s"}, Running: []agenthub.StageRef{{BuildID: b.ID, Job: b.Job, Number: b.Number, Stage: "s"}}}}
		}
	})
	ctx := context.Background()
	// Build 1 is finished with times of nanoseconds, in a zone that is not UTC.
	zone := time.FixedZone("x", 7*3600)
	b := &store.Build{Job: "smoke", Status: store.Success, Trigger: "cli", QueuedAt: time.Date(2026, 10, 30, 12, 0, 0, 123456789, zone)}
	must(t, e.st.CreateBuild(ctx, b))
	must(t, e.st.SetBuildStatus(ctx, b.ID, store.Success, time.Date(2026, 10, 30, 12, 0, 5, 987654321, zone)))
	must(t, e.st.AddActivity(ctx, "smoke", "reloaded", since))
	must(t, e.st.AppendAudit(ctx, store.Audit{At: since, Actor: "cli", Action: "reload", Target: "jobs"}))
	must(t, e.st.PutEngine(ctx, store.Engine{Name: "mac", Kind: "docker", URL: "ssh://me@host", Labels: []string{"linux"}, Max: 2, Enabled: true}))
	must(t, e.st.PutAgent(ctx, store.Agent{Name: "box", Labels: []string{"linux"}, Executors: 2, Enabled: true, TokenHash: "h1"}))
	_, err := e.st.PutCacheEntry(ctx, &store.CacheEntry{Scope: "site", Namespace: "default", Key: "k", Size: 5, SavedAt: since, SavedByBuild: b.ID})
	must(t, err)
	metricJobs(t, e)
	// A job whose parameter says nothing of itself.
	e.writeJob(t, "plain", "name: plain\nparams:\n  A: {type: string}\nstages:\n  s: {run: 'true'}\n")
	m := measuredBuild(t, e, "site-smoke", "seconds", map[string]float64{"a": 1})
	remember(t, e, "site", m, "seconds", map[string]float64{"a": 3})
	was, now := 87.19, 86.9
	must(t, e.st.PutGuardResults(ctx, m.ID, []store.GuardRow{{Stage: "measure", Metric: "seconds", Item: "a", Remembered: &was, Reported: &now, Tolerance: 0.01,
		Verdict: "fell", Outcome: "failed", FromJob: "site-smoke", FromNumber: m.Number}}))
	return e, e.withOwner(t)
}

func TestEveryTimestampIsUTCWithThreeFractionDigits(t *testing.T) {
	e, owner := shapeEnv(t)
	id := itoa64(1)
	paths := []string{"/jobs", "/jobs/smoke", "/jobs/smoke/builds", "/builds/" + id, "/jobs/smoke/activity", "/admin/users", "/admin/server", "/admin/audit", "/admin/agents",
		"/admin/engines", "/admin/cache", "/queue", "/metrics/site", "/metrics/site/remembered", "/metrics/site/history?metric=seconds&item=a", "/builds/2/metrics", "/jobs/acme.site/children"}
	found := map[string]string{}
	for _, p := range paths {
		tok := e.admin
		if strings.HasPrefix(p, "/jobs/acme") {
			tok = owner
		}
		stampsIn(e.any(t, tok, "/api/v1"+p), p, found)
	}
	if len(found) < 25 {
		t.Fatalf("only %d timestamps were looked at, so the answers are not the fixture's: %v", len(found), found)
	}
	for path, s := range found {
		if !millis.MatchString(s) {
			t.Errorf("%s is %q, want RFC 3339 in UTC with three fraction digits", path, s)
		}
	}
}

func TestALogTellsWhenALineBeganInRFC3339InUTCWithMilliseconds(t *testing.T) {
	e := newEnv(t)
	id := e.trigger(t, "smoke", "{}")
	e.finished(t, id)
	path := "/api/v1/builds/" + id + "/stages/s/log"
	text, _ := e.call(t, 200, e.admin, "GET", path+"?download=true&timestamps=true", "")
	if !regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z tier=quick\n\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z done\n$`).MatchString(text) {
		t.Fatalf("the download with times: %q", text)
	}
	got, _ := e.call(t, 200, e.admin, "GET", path+"/events?timestamps=true", "")
	if !regexp.MustCompile(`^event: time\ndata: \d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z\n\nid: 11\ndata: tier=quick\n\nevent: time\ndata: \d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z\n\nid: 16\ndata: done\n\n`).MatchString(got) {
		t.Fatalf("the stream with times: %q", got)
	}
}

func TestAScansEveryIsWholeSeconds(t *testing.T) {
	e := newEnv(t)
	gh := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(gh.Close)
	parent, err := config.ParseJob("site.yml", []byte("name: site\nmultibranch: {repo: acme/site, token: GH_TOKEN, scan: 90m}\n"))
	must(t, err)
	ix := index.New(e.st, e.api.deps.Dispatch.SetSnapshot)
	must(t, ix.SetBase(context.Background(), e.api.deps.Dispatch.Snapshot().WithJobs(parent)))
	e.api.deps.Scanner = index.NewScanner(context.Background(), ix, index.ScannerOptions{API: gh.URL,
		Token: func(context.Context, string) (string, error) { return "t", nil }})
	t.Cleanup(e.api.deps.Scanner.Wait)

	scan := e.object(t, 200, e.admin, "GET", "/api/v1/jobs/site", "")["scan"].(map[string]any)
	eq(t, "every_seconds", scan["every_seconds"], any(5400.0))
	if _, ok := scan["every"]; ok {
		t.Fatalf("v1 names the unit: %v", scan)
	}
	eqKeys(t, "a scan's keys are always all there", scan, "at,error,every_seconds,result,running,token")
	eq(t, "no end yet", scan["at"], nil)
}

func TestV1RefersToABuildAsABuildRefWithItsStatusEverywhere(t *testing.T) {
	e, _ := shapeEnv(t)
	ref := func(what string, v any, job string) map[string]any {
		t.Helper()
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("%s is no object: %v", what, v)
		}
		eqKeys(t, what+" keys", m, "id,job,number,status")
		eq(t, what+" job", m["job"], any(job))
		return m
	}
	// A remembered value and a history row name the build they came from.
	values, _ := e.page(t, e.admin, "/api/v1/metrics/site/remembered?metric=seconds")
	eq(t, "status of a remembered value's build", ref("remembered build", values[0]["build"], "site-smoke")["status"], any("success"))
	history, _ := e.page(t, e.admin, "/api/v1/metrics/site/history?metric=seconds&item=a")
	eq(t, "status of a history row's build", ref("history build", history[0]["build"], "site-smoke")["status"], any("success"))
	// The build's own metrics, and a guard's origin.
	mb := e.object(t, 200, e.admin, "GET", "/api/v1/builds/2/metrics", "")
	ref("metrics build", mb["build"], "site-smoke")
	guard := mb["stages"].([]any)[0].(map[string]any)["guards"].([]any)[0].(map[string]any)
	from := ref("guard origin", guard["from"], "site-smoke")
	eq(t, "the guard's origin is the build that set it", from["number"], any(1.0))
	eq(t, "and says how it ended", from["status"], any("success"))
	// A cache entry says which build saved it.
	entries, _ := e.page(t, e.admin, "/api/v1/admin/cache")
	if _, old := entries[0]["saved_by_build"]; old {
		t.Fatalf("saved_by_build is saved_by in v1: %v", entries[0])
	}
	eq(t, "saved_by", ref("saved_by", entries[0]["saved_by"], "smoke")["id"], any(1.0))
	// An agent's stages are the build and the stage.
	agents, _ := e.page(t, e.admin, "/api/v1/admin/agents")
	stages := agents[0]["stages"].([]any)
	eq(t, "the agent's stages", len(stages), 1)
	st := stages[0].(map[string]any)
	eqKeys(t, "stage keys", st, "build,stage")
	eq(t, "stage", st["stage"], any("s"))
	ref("agent stage build", st["build"], "smoke")
}

func TestAGuardWhoseOriginBuildIsGoneHasNoStatusAndAnIdOfZero(t *testing.T) {
	e := newEnv(t)
	metricJobs(t, e)
	b := measuredBuild(t, e, "site-smoke", "seconds", map[string]float64{"a": 1})
	was := 2.0
	must(t, e.st.PutGuardResults(context.Background(), b.ID, []store.GuardRow{{Stage: "measure", Metric: "seconds", Item: "a", Remembered: &was, Verdict: "missing",
		Outcome: "failed", FromJob: "site-deploy", FromNumber: 123}}))
	mb := e.object(t, 200, e.admin, "GET", "/api/v1/builds/"+itoa64(b.ID)+"/metrics", "")
	guard := mb["stages"].([]any)[0].(map[string]any)["guards"].([]any)[0].(map[string]any)
	eq(t, "origin", fmt.Sprint(guard["from"]), "map[id:0 job:site-deploy number:123 status:<nil>]")
}

func TestV1HasBuildSummariesInListsAndABuildWithStagesAndCompleteAlone(t *testing.T) {
	e, _ := shapeEnv(t)
	summary := "base_branch,base_commit,branch,built_commit,finished_at,id,job,kept,number,params,parent,pr,queued_at,started_at,started_by,status,trigger,trigger_detail"
	builds, _ := e.page(t, e.admin, "/api/v1/jobs/smoke/builds")
	eqKeys(t, "a summary", builds[0], summary)
	// Every field is there, null where the build has none.
	for _, k := range []string{"parent", "branch", "built_commit", "pr", "base_branch", "base_commit", "kept"} {
		eq(t, k, builds[0][k], nil)
	}
	jobs, _ := e.page(t, e.admin, "/api/v1/jobs")
	for _, j := range jobs {
		if j["name"] == "smoke" {
			eqKeys(t, "last is a summary", j["last"].(map[string]any), summary)
		}
	}
	whole := e.object(t, 200, e.admin, "GET", "/api/v1/builds/1", "")
	eqKeys(t, "a build", whole, summary+",complete,stages")
	eq(t, "complete", whole["complete"], any(true))
	if _, ok := whole["stages"].([]any); !ok {
		t.Fatalf("stages is an array: %v", whole["stages"])
	}
	// What triggers a build answers its summary.
	trig := e.object(t, 201, e.admin, "POST", "/api/v1/jobs/gc/builds", "")
	eqKeys(t, "the trigger's answer", trig, summary)
}

func TestAChildsBuildTellsWhatItBuiltInV1(t *testing.T) {
	e, owner := shapeEnv(t)
	builds, _ := e.page(t, owner, "/api/v1/jobs/acme.site.main/builds")
	eq(t, "parent", builds[0]["parent"], any("acme.site"))
	eq(t, "branch", builds[0]["branch"], any("main"))
	eq(t, "no pull request", builds[0]["pr"], nil)
}

func TestV1RenamesWhatD19Renames(t *testing.T) {
	e, owner := shapeEnv(t)

	// children is children_count, on a job and on an owner job's repo.
	jobs, _ := e.page(t, owner, "/api/v1/jobs")
	eq(t, "the owner job", jobs[0]["name"], any("acme"))
	if _, old := jobs[0]["children"]; old {
		t.Fatalf("children is children_count: %v", jobs[0])
	}
	eq(t, "children_count", jobs[0]["children_count"], any(1.0))
	eq(t, "worst", jobs[0]["worst"], any("success"))
	plain, _ := e.page(t, e.admin, "/api/v1/jobs")
	for _, j := range plain {
		if j["name"] == "smoke" {
			eq(t, "no children", j["children_count"], any(0.0))
			eq(t, "no worst", j["worst"], nil)
		}
	}
	detail := e.object(t, 200, owner, "GET", "/api/v1/jobs/acme", "")
	repos := detail["owner"].(map[string]any)["repos"].([]any)
	for _, r := range repos {
		repo := r.(map[string]any)
		if _, old := repo["children"]; old {
			t.Fatalf("a repo's children is children_count: %v", repo)
		}
		eqKeys(t, "repo keys", repo, "children_count,hook,hook_note,job,repo,state,worst")
	}

	// head_sha is head_commit.
	kids, _ := e.page(t, owner, "/api/v1/jobs/acme.site/children")
	eqKeys(t, "child keys", kids[0], "author,base_branch,branch,from_fork,gone_at,head_commit,job,key,kind,last,last_seen_at,pr,state,title,url")
	eq(t, "head_commit", kids[0]["head_commit"], any("abc"))
	eq(t, "no pull request", kids[0]["pr"], nil)

	// An engine's max is its executors, and it says its version.
	engines, _ := e.page(t, e.admin, "/api/v1/admin/engines")
	if _, old := engines[0]["max"]; old {
		t.Fatalf("max is executors: %v", engines[0])
	}
	eq(t, "executors", engines[0]["executors"], any(2.0))
	eq(t, "version", engines[0]["version"], any("27.3.1"))

	// An agent says how many stages it runs and has been given.
	agents, _ := e.page(t, e.admin, "/api/v1/admin/agents")
	eq(t, "busy", agents[0]["busy"], any(1.0))
}

func TestAnOwnerJobsOverlapsAreTheRepoAndTheJobsThatShareItInV1(t *testing.T) {
	e := newEnv(t)
	e.withOwner(t)
	// A second job that indexes the same repo is the mistake an overlap names.
	ctx := context.Background()
	other, err := config.ParseJob("other.yml", []byte("name: other\nmultibranch: {repo: acme/site, clone: /nonexistent/site.git}\n"))
	must(t, err)
	snap := e.api.deps.Dispatch.Snapshot().WithJobs(other)
	ix := index.New(e.st, e.api.deps.Dispatch.SetSnapshot)
	must(t, ix.SetBase(ctx, snap))
	must(t, e.st.PutOwnerRepo(ctx, store.OwnerRepo{Owner: "other", Key: "x", Repo: "acme/site", State: config.RepoLive, LastSeenAt: time.Now()}))
	detail := e.object(t, 200, e.admin, "GET", "/api/v1/jobs/acme", "")
	owner := detail["owner"].(map[string]any)
	eqKeys(t, "owner keys", owner, "app,login,old_hooks,overlaps,public_only,repos,scan")
	overlaps, ok := owner["overlaps"].([]any)
	if !ok {
		t.Fatalf("overlaps is an array: %v", owner["overlaps"])
	}
	for _, o := range overlaps {
		m := o.(map[string]any)
		eqKeys(t, "an overlap", m, "jobs,repo")
	}
}

func TestAQueuedBuildAndARunningOneKeepAllTheirFieldsInV1(t *testing.T) {
	e := newEnv(t)
	e.started(t, "slow")
	e.trigger(t, "slow", "")
	var q map[string]any
	eventually(t, func() bool {
		q = e.object(t, 200, e.admin, "GET", "/api/v1/queue", "")
		return len(q["queued"].([]any)) == 1
	})
	eqKeys(t, "queue keys", q, "busy,executors,queued,running")
	run := q["running"].([]any)[0].(map[string]any)
	eqKeys(t, "running keys", run, "id,job,number,queued_at,started_at,started_by,stages,trigger,waiting_stages")
	if _, old := run["waiting"]; old {
		t.Fatalf("waiting is waiting_stages: %v", run)
	}
	eq(t, "no stage waits", fmt.Sprint(run["waiting_stages"]), "map[]")
	queued := q["queued"].([]any)[0].(map[string]any)
	eqKeys(t, "queued keys", queued, "blocker,id,job,lock,number,queued_at,started_by,trigger,waiting")
	eq(t, "no lock", queued["lock"], nil)
	eqKeys(t, "blocker", queued["blocker"].(map[string]any), "id,job,number,status")

	// Nobody but an admin is told the executors; the keys stay, null.
	viewer := e.object(t, 200, e.viewer, "GET", "/api/v1/queue", "")
	eqKeys(t, "a viewer's queue keys", viewer, "busy,executors,queued,running")
	eq(t, "executors", viewer["executors"], nil)
	eq(t, "busy", viewer["busy"], nil)
}

func TestNoListOfV1IsNullAndNoOptionalFieldIsLeftOut(t *testing.T) {
	e, owner := shapeEnv(t)
	detail := e.object(t, 200, e.admin, "GET", "/api/v1/jobs/plain", "")
	eqKeys(t, "detail keys", detail,
		"activity,builds,concurrency,cron,description,disabled,folder,group,locks,multibranch,name,next,owner,parent,params,removed,scan,timezone")
	for _, k := range []string{"parent", "multibranch", "scan", "folder", "owner"} {
		eq(t, k, detail[k], nil)
	}
	param := detail["params"].([]any)[0].(map[string]any)
	eqKeys(t, "param keys", param, "default,description,name,options,type")
	eq(t, "description", param["description"], nil)
	// A stage that needs nothing needs [].
	id := e.trigger(t, "plain", "")
	e.finished(t, id)
	whole := e.object(t, 200, e.admin, "GET", "/api/v1/builds/"+id, "")
	stage := whole["stages"].([]any)[0].(map[string]any)
	if needs, ok := stage["needs"].([]any); !ok || len(needs) != 0 {
		t.Fatalf("needs is [], never null: %v", stage)
	}
	// An owner job's page.
	owned := e.object(t, 200, owner, "GET", "/api/v1/jobs/acme", "")["owner"].(map[string]any)
	eq(t, "app", owned["app"], nil)
	eq(t, "public_only", owned["public_only"], any(false))
}

func TestTheAdminReadsOfV1TellTheirTimesInUTCMilliseconds(t *testing.T) {
	e := newEnv(t)
	times := map[string]string{}
	on := e.object(t, 200, e.admin, "POST", "/api/v1/admin/quiet", "")
	eq(t, "quiet", on["quiet"], any(true))
	stampsIn(on, "quiet", times)
	stampsIn(e.any(t, e.admin, "/api/v1/admin/quiet"), "quiet again", times)
	e.object(t, 200, e.admin, "DELETE", "/api/v1/admin/quiet", "")
	stampsIn(e.any(t, e.admin, "/api/v1/admin/users/admin"), "user", times)
	secrets := e.object(t, 200, e.admin, "GET", "/api/v1/admin/secrets", "")
	eqKeys(t, "secrets", secrets, "set,missing,out_of_scope")
	stampsIn(secrets, "secrets", times)
	if len(times) < 3 {
		t.Fatalf("the answers hold %d times: %v", len(times), times)
	}
	for path, s := range times {
		if !millis.MatchString(s) {
			t.Errorf("%s is %q", path, s)
		}
	}
}
