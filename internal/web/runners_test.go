package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/dispatch"
	"github.com/ndthuan/kipp/internal/store"
)

func TestTheRunnersPageIsForAnAdminOnly(t *testing.T) {
	e := site(t)
	bob := e.login(t, "bob")
	code, _ := get(t, bob, e.srv.URL+"/admin/runners")
	eq(t, "a user", code, http.StatusForbidden)
	res, _ := e.form(t, bob, "/admin/runners/engines", url.Values{"name": {"mac"}, "url": {"unix:///d.sock"}, "labels": {"linux"}, "max": {"1"}})
	eq(t, "a user adding an engine", res.StatusCode, http.StatusForbidden)
	_, page := get(t, e.login(t, "admin"), e.srv.URL+"/jobs/smoke")
	contains(t, page, `<a href="/admin/runners">Runners</a>`)
	if strings.Index(page, `href="/admin/secrets"`) > strings.Index(page, `href="/admin/runners"`) || strings.Index(page, `href="/admin/runners"`) > strings.Index(page, `href="/admin/server"`) {
		t.Fatal("Runners is not between Secrets and Server")
	}
}

func TestTheRunnersPageAddsAnEngineAndAnRunnerWhoseTokenIsShownOnce(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	code, page := get(t, admin, e.srv.URL+"/admin/runners")
	eq(t, "runners", code, http.StatusOK)
	selfContained(t, page)
	contains(t, page, "No engine is registered.", "No standing runner is registered.", `action="/admin/runners/engines"`, `action="/admin/runners/runners"`)

	engine := url.Values{"name": {"mac"}, "url": {"ssh://me@192.0.2.1?socket=/s.sock"}, "labels": {"linux,arm64"}, "max": {"2"}}
	res, _ := e.form(t, admin, "/admin/runners/engines", engine)
	eq(t, "without sudo", res.StatusCode, http.StatusForbidden)
	e.sudo(t, admin)
	res, _ = e.form(t, admin, "/admin/runners/engines", engine)
	eq(t, "add an engine", res.StatusCode, http.StatusSeeOther)
	got, err := e.st.Engine(context.Background(), "mac")
	must(t, err)
	if got.URL != "ssh://me@192.0.2.1?socket=/s.sock" || got.Max != 2 || len(got.Labels) != 2 || !got.Enabled {
		t.Fatalf("%+v", got)
	}
	res, page = e.form(t, admin, "/admin/runners/engines", url.Values{"name": {"tcp"}, "url": {"tcp://host:2376"}, "labels": {"linux"}, "max": {"1"}})
	eq(t, "an engine kipp would not dial", res.StatusCode, http.StatusBadRequest)
	contains(t, page, "a tcp engine needs --tls-ca, --tls-cert and --tls-key", `value="tcp://host:2376"`, `role="alert"`)

	res, page = e.form(t, admin, "/admin/runners/runners", url.Values{"name": {"box"}, "labels": {"linux"}, "executors": {"2"}})
	eq(t, "add a runner", res.StatusCode, http.StatusOK)
	contains(t, page, "Token for the runner box", "KIPP_RUNNER_TOKEN")
	token := onceValue(t, page)
	a, err := e.st.RunnerByTokenHash(context.Background(), auth.HashToken(token))
	must(t, err)
	eq(t, "the runner", a.Name, "box")
	_, page = get(t, admin, e.srv.URL+"/admin/runners")
	contains(t, page, "<code>linux, arm64</code>", "box", "never seen", "0 of 2", "not asked yet")
	lacks(t, page, token)
	lacks(t, e.audit(t), token)
	contains(t, e.audit(t), "admin engine.add mac", "admin runner.add box")
}

func TestAnEngineAndAnRunnerAreDisabledAndRemovedAfterAPageThatAsks(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	e.sudo(t, admin)
	ctx := context.Background()
	must(t, e.st.PutEngine(ctx, store.Engine{Name: "mac", Kind: "docker", URL: "unix:///d.sock", Labels: []string{"linux"}, Max: 1, Enabled: true}))
	must(t, e.st.PutRunner(ctx, store.Runner{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true, TokenHash: "old"}))
	for _, path := range []string{"/admin/runners/engines/mac/disable", "/admin/runners/runners/box/disable"} {
		res, _ := e.form(t, admin, path, nil)
		eq(t, path, res.StatusCode, http.StatusSeeOther)
	}
	en, err := e.st.Engine(ctx, "mac")
	must(t, err)
	ag, err := e.st.Runner(ctx, "box")
	must(t, err)
	if en.Enabled || ag.Enabled {
		t.Fatal("not disabled")
	}
	_, page := get(t, admin, e.srv.URL+"/admin/runners")
	contains(t, page, `<span class="tag">disabled</span>`, `action="/admin/runners/engines/mac/enable"`, `action="/admin/runners/runners/box/enable"`)

	code, page := get(t, admin, e.srv.URL+"/admin/runners/runners/box/token")
	eq(t, "confirm a new token", code, http.StatusOK)
	contains(t, page, "A new token for the runner box?")
	res, page := e.form(t, admin, "/admin/runners/runners/box/token", nil)
	eq(t, "a new token", res.StatusCode, http.StatusOK)
	if _, err := e.st.RunnerByTokenHash(ctx, auth.HashToken(onceValue(t, page))); err != nil {
		t.Fatal(err)
	}

	for _, which := range []string{"engines/mac", "runners/box"} {
		code, page := get(t, admin, e.srv.URL+"/admin/runners/"+which+"/delete")
		eq(t, "confirm "+which, code, http.StatusOK)
		contains(t, page, "Remove the")
		res, _ := e.form(t, admin, "/admin/runners/"+which+"/delete", nil)
		eq(t, "remove "+which, res.StatusCode, http.StatusSeeOther)
	}
	if _, err := e.st.Engine(ctx, "mac"); err == nil {
		t.Fatal("the engine is still there")
	}
	if _, err := e.st.Runner(ctx, "box"); err == nil {
		t.Fatal("the runner is still there")
	}
	code, _ = get(t, admin, e.srv.URL+"/admin/runners/runners/none/delete")
	eq(t, "no such runner", code, http.StatusNotFound)
}

func TestTheBuildPageSaysWhereEachStageRan(t *testing.T) {
	e := site(t)
	ctx := context.Background()
	b := &store.Build{Job: "smoke", Status: store.Success, Trigger: "api"}
	must(t, e.st.CreateBuild(ctx, b))
	must(t, e.st.PutStages(ctx, b.ID, []string{"a", "b"}, nil))
	must(t, e.st.SetStageRanOn(ctx, b.ID, "a", "mac/1"))
	must(t, e.st.SetStageRanOn(ctx, b.ID, "b", "local"))
	_, page := get(t, e.login(t, "admin"), e.srv.URL+"/jobs/smoke/"+strconv.Itoa(b.Number))
	contains(t, page, "<th>On</th>", "<code>mac/1</code>", `<span class="sub">the server</span>`)
}

func TestTheQueueSaysWhatAStageWaitsForToBePlaced(t *testing.T) {
	e := site(t)
	rec := httptest.NewRecorder()
	q := &api.QueueView{Running: []api.RunningView{{
		Build:   &store.Build{ID: 1, Job: "smoke", Number: 3, Status: store.Running},
		Stages:  []string{"s"},
		Waiting: map[string]string{"s": "waiting for a runner with labels [linux]"},
	}}}
	e.site.render(rec, httptest.NewRequest(http.MethodGet, "/queue", nil), "queue", http.StatusOK, page{Title: "Queue", Data: queueData{Queue: q, Full: true}, Queue: q})
	eq(t, "status", rec.Code, http.StatusOK)
	contains(t, rec.Body.String(), `<span class="sub">s: waiting for runner (label linux)</span>`)
}

func TestTheBuildPageSaysHowManyTriesAStageTookOnlyWhenItWasMoreThanOne(t *testing.T) {
	e := site(t)
	ctx := context.Background()
	b := &store.Build{Job: "smoke", Status: store.Success, Trigger: "api"}
	must(t, e.st.CreateBuild(ctx, b))
	must(t, e.st.PutStages(ctx, b.ID, []string{"a", "b"}, nil))
	must(t, e.st.SetStage(ctx, b.ID, "a", store.StageSuccess, nil, time.Now()))
	must(t, e.st.SetStage(ctx, b.ID, "b", store.StageSuccess, nil, time.Now()))
	must(t, e.st.SetStageTries(ctx, b.ID, "a", 2))
	_, page := get(t, e.login(t, "admin"), e.srv.URL+"/jobs/smoke/"+strconv.Itoa(b.Number))
	contains(t, page, `>success</span><span class="sub">, 2 tries</span>`)
	if strings.Count(page, " tries</span>") != 1 {
		t.Fatalf("a stage that ran once says its tries:\n%s", page)
	}
}

func renderQueue(t *testing.T, e *env, q *api.QueueView) string {
	t.Helper()
	rec := httptest.NewRecorder()
	e.site.render(rec, httptest.NewRequest(http.MethodGet, "/queue", nil), "queue", http.StatusOK, page{Title: "Queue", Data: queueData{Queue: q, Full: true}, Queue: q})
	eq(t, "status", rec.Code, http.StatusOK)
	return rec.Body.String()
}

func runningBuilds(n int) []api.RunningView {
	var out []api.RunningView
	for i := 1; i <= n; i++ {
		out = append(out, api.RunningView{Build: &store.Build{ID: int64(i), Job: "smoke", Number: i, Status: store.Running}, Stages: []string{"s"}})
	}
	return out
}

func TestTheQueueHeadingCountsBuildsFirstAndSlotsHereOnlyForAnAdmin(t *testing.T) {
	e := site(t)
	contains(t, renderQueue(t, e, &api.QueueView{Running: runningBuilds(3), Executors: 8, Busy: 2}),
		`<h2>Running 3 builds <span class="sub">2/8 slots here</span></h2>`)
	body := renderQueue(t, e, &api.QueueView{Running: runningBuilds(1)})
	contains(t, body, `<h2>Running 1 build</h2>`)
	lacks(t, body, "slots", "<h2>Waiting")
}

func TestTheQueueShowsABuildWhoseEveryStageWaitsToBePlacedWithTheBuildsThatWait(t *testing.T) {
	e := site(t)
	r := runningBuilds(3)
	r[1].Waiting = map[string]string{"s": "waiting for a runner with labels [mac]"}
	started := time.Now().Add(-time.Hour)
	r[1].Build.StartedAt = &started
	r[2].Stages = []string{"s", "t"}
	r[2].Waiting = map[string]string{"s": "waiting for a runner with labels [mac]"}
	queued := api.QueuedView{Build: &store.Build{ID: 9, Job: "later", Number: 1, Status: store.Queued}, Waiting: dispatch.WaitExecutor}
	body := renderQueue(t, e, &api.QueueView{Running: r, Queued: []api.QueuedView{queued}, Executors: 8, Busy: 2})
	contains(t, body,
		`<h2>Running 2 builds <span class="sub">2/8 slots here</span></h2>`,
		`<h2>Waiting 2 builds</h2>`,
		`<span class="status queued">waiting</span><br><span class="sub">s: waiting for runner (label mac)</span>`,
		`<span class="tl">started </span><time class="elapsed"`)
	// the header's badge counts the same way
	if n := nowOf(&api.QueueView{Running: r, Queued: []api.QueuedView{queued}}); len(n.Running) != 2 || n.Waiting() != 2 {
		t.Fatalf("badge counts %d running and %d waiting, want 2 and 2", len(n.Running), n.Waiting())
	}
	waiting := strings.Index(body, "<h2>Waiting")
	for _, b := range []string{"smoke #1", "smoke #3"} {
		if i := strings.Index(body, b); i < 0 || i > waiting {
			t.Fatalf("%s, which runs a stage, is not above the builds that wait:\n%s", b, body)
		}
	}
	for _, b := range []string{"smoke #2", "later #1"} {
		if strings.Index(body, b) < waiting {
			t.Fatalf("%s, which runs nothing, is not with the builds that wait:\n%s", b, body)
		}
	}

	body = renderQueue(t, e, &api.QueueView{Running: r[1:2], Executors: 8})
	contains(t, body, `<h2>Waiting 1 build <span class="sub">0/8 slots here</span></h2>`)
	lacks(t, body, "<h2>Running")
}

func TestTheQueueSaysWhereAStageRunsAndWhatItWaitsForInFewWords(t *testing.T) {
	e := site(t)
	r := runningBuilds(1)[0]
	r.Stages = []string{"a", "b", "c", "d"}
	r.Waiting = map[string]string{"b": "waiting for a runner with labels [mac]", "c": "waiting for a runner with labels [linux, arm64]"}
	r.Remote = map[string]string{"a": "mac"}
	body := renderQueue(t, e, &api.QueueView{Running: []api.RunningView{r}})
	contains(t, body, `<span class="sub stages">stage: d</span>`,
		`<span class="sub">a: on mac</span>`,
		`<span class="sub">b: waiting for runner (label mac)</span>`,
		`<span class="sub">c: waiting for runner (labels linux, arm64)</span>`)
	lacks(t, body, "with labels")
	r.Remote = map[string]string{"a": ""}
	contains(t, renderQueue(t, e, &api.QueueView{Running: []api.RunningView{r}}), `<span class="sub">a: on another machine</span>`)
}

func waitingBuild() *api.BuildView {
	started := time.Now().Add(-time.Minute)
	return &api.BuildView{
		Build:   &store.Build{ID: 1, Job: "site", Number: 21, Status: store.Running, Trigger: "api", StartedAt: &started},
		Stages:  []*store.Stage{{Name: "clone", Status: store.StageSuccess}, {Name: "run", Status: store.StageRunning}},
		Needs:   map[string][]string{"clone": nil, "run": {"clone"}},
		Waiting: map[string]string{"run": "waiting for a runner with labels [mac]"},
	}
}

func renderBuild(t *testing.T, e *env, v *api.BuildView) string {
	t.Helper()
	data := &buildData{View: v}
	data.Status = showWaiting(v)
	data.Logs = []logView{{Name: "clone", Status: v.Stages[0].Status}, {Name: "run", Open: true, Status: v.Stages[1].Status}}
	rec := httptest.NewRecorder()
	e.site.render(rec, httptest.NewRequest(http.MethodGet, "/jobs/site/21", nil), "build", http.StatusOK, page{Title: "site #21", Data: data})
	eq(t, "status", rec.Code, http.StatusOK)
	return rec.Body.String()
}

func TestABuildWhoseEveryRunningStageWaitsForAPlaceSaysWaitingAndWhatFor(t *testing.T) {
	e := site(t)
	body := renderBuild(t, e, waitingBuild())
	contains(t, body,
		`<span>#21</span> <span class="status waiting">waiting</span>`,
		`<span class="status waiting">waiting</span><span class="sub">, waiting for runner (label mac)</span>`,
		` run=waiting `)
	lacks(t, body, `status running`)
}

func TestABuildThatRunsAStageWhileAnotherWaitsSaysRunning(t *testing.T) {
	e := site(t)
	v := waitingBuild()
	v.Stages = append(v.Stages, &store.Stage{Name: "lint", Status: store.StageRunning})
	v.Needs["lint"] = []string{"clone"}
	body := renderBuild(t, e, v)
	contains(t, body, `<span>#21</span> <span class="status running">running</span>`, `<span class="status waiting">waiting</span>`)
}
