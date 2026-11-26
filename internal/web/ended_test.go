package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/store"
)

func renderNow(t *testing.T, e *env, q *api.QueueView, ended []*store.Build, full bool) string {
	t.Helper()
	rec := httptest.NewRecorder()
	e.site.render(rec, httptest.NewRequest(http.MethodGet, "/queue", nil), "queue", http.StatusOK, page{Title: "Queue", Data: queueData{Queue: q, Ended: ended, Full: full}, Queue: q})
	eq(t, "status", rec.Code, http.StatusOK)
	return rec.Body.String()
}

func endedBuild(id int64, job string, st store.Status, ago time.Duration) *store.Build {
	start := time.Now().Add(-ago - 90*time.Second)
	end := time.Now().Add(-ago)
	return &store.Build{ID: id, Job: job, Number: int(id), Status: st, StartedAt: &start, FinishedAt: &end, QueuedAt: start}
}

func TestNowIsOneTableOfRunningThenWaitingAndRecentIsATableOfItsOwnUnderItsOwnHeadingNewestEndFirst(t *testing.T) {
	e := site(t)
	r := runningBuilds(2)
	r[1].Waiting = map[string]string{"s": "waiting for a runner"}
	queued := api.QueuedView{Build: &store.Build{ID: 9, Job: "later", Number: 1, Status: store.Queued}, Waiting: api.WaitStart}
	q := &api.QueueView{Running: r, Queued: []api.QueuedView{queued}, Executors: 8, Busy: 1, Runners: loads(1, 8)}
	ended := []*store.Build{endedBuild(21, "newer", store.Failed, time.Hour), endedBuild(20, "older", store.Success, 2*time.Hour)}
	body := renderNow(t, e, q, ended, true)
	contains(t, body, `<h2>Now <span class="sub">1 running &middot; 2 waiting</span> <span class="sub">built-in 1/8</span></h2>`)
	contains(t, body, `<h2>Recent</h2>`)
	eq(t, "two tables", strings.Count(body, "<table"), 2)
	nowTable, recentTable, _ := strings.Cut(body, `<h2>Recent</h2>`)
	contains(t, nowTable, "smoke #1", "later #1")
	lacks(t, nowTable, "newer #21", "older #20")
	contains(t, recentTable, "newer #21", "older #20")
	lacks(t, recentTable, "smoke #1", "later #1")
	lacks(t, body, "<h2>Running", "<h2>Waiting")
	last := -1
	for _, b := range []string{"smoke #1", "smoke #2", "later #1", "newer #21", "older #20"} {
		i := strings.Index(body, b)
		if i <= last {
			t.Fatalf("%s is not below the row before it:\n%s", b, body)
		}
		last = i
	}
}

func TestAnEndedRowHasTheStatusTheTimeItTookAndWhenItEndedAndNoCancel(t *testing.T) {
	e := site(t)
	body := renderNow(t, e, &api.QueueView{}, []*store.Build{endedBuild(7, "smoke", store.Cancelled, 3*time.Hour), endedBuild(6, "smoke", store.Lost, 4*time.Hour)}, true)
	contains(t, body,
		`<a class="link" href="/jobs/smoke/7">smoke #7</a>`,
		`<span class="status cancelled">cancelled</span> <span class="sub">took 1m 30s</span>`,
		`<span class="status lost">lost</span>`,
		`<span class="tl">cancelled </span><time class="past" datetime="`,
		`<span class="tl">lost </span><time class="past" datetime="`)
	lacks(t, body, "/cancel", "<h2>Running", "<h2>Waiting")
	contains(t, body, `<h2>Recent</h2>`)
	lacks(t, body, `<h2>Now`)
}

func TestAnEndedBuildThatNeverStartedHasNoTookAndOneWithNoEndStillShows(t *testing.T) {
	e := site(t)
	queued := time.Now().Add(-time.Hour)
	b := &store.Build{ID: 3, Job: "smoke", Number: 3, Status: store.Cancelled, QueuedAt: queued}
	body := renderNow(t, e, &api.QueueView{}, []*store.Build{b}, true)
	contains(t, body, `smoke #3`, `<span class="status cancelled">cancelled</span>`, `<span class="tl">cancelled </span>`)
	lacks(t, body, "took")
}

func TestNowSaysNothingRunsAboveRecentOnAnIdleServerAndAloneOnAnEmptyOne(t *testing.T) {
	e := site(t)
	body := renderNow(t, e, &api.QueueView{}, []*store.Build{endedBuild(1, "smoke", store.Success, time.Hour)}, true)
	contains(t, body, `<p class="sub">Nothing runs and nothing waits.</p>`, `smoke #1`, `<h2>Recent</h2>`)
	lacks(t, body, `<h2>Now`)
	if strings.Index(body, "Nothing runs") > strings.Index(body, "<table") {
		t.Fatalf("the line is below the table:\n%s", body)
	}
	body = renderNow(t, e, &api.QueueView{}, nil, true)
	contains(t, body, `<p class="sub">Nothing runs and nothing waits.</p>`)
	lacks(t, body, "<table", "<h2>")
}

func TestTheJobListShowsNoEndedRowAndTheNowHeadingOnlyWhenSomethingRuns(t *testing.T) {
	e := site(t)
	ended := []*store.Build{endedBuild(1, "smoke", store.Success, time.Hour)}
	body := renderNow(t, e, &api.QueueView{}, ended, false)
	lacks(t, body, "smoke #1", "<h2>", "Nothing runs")
	body = renderNow(t, e, &api.QueueView{Running: runningBuilds(1)}, ended, false)
	contains(t, body, `<h2>Now <span class="sub">1 running</span></h2>`, "smoke #1")
	lacks(t, body, "ended")
}

func endedRows(page string) int { return strings.Count(page, `<tr class="done `) }

func TestAnEndedRowNamesHowItEndedInItsStatusColourOnAPhoneWhereTheStatusBadgeIsHidden(t *testing.T) {
	e := site(t)
	for _, st := range []store.Status{store.Success, store.Unstable, store.Failed, store.Cancelled, store.Lost, store.Timeout} {
		body := renderNow(t, e, &api.QueueView{}, []*store.Build{endedBuild(5, "smoke", st, time.Hour)}, true)
		contains(t, body, `<tr class="done `+string(st)+`">`, `<span class="tl">`+string(st)+` </span><time class="past" datetime="`)
		lacks(t, body, `<span class="tl">ended `)
	}
}

func TestTheOverviewAndTheQueueListTheTenNewestEndedBuildsOfEveryTerminalStatus(t *testing.T) {
	e := site(t)
	e.finishedAgo(t, "smoke", store.Cancelled, 10*time.Minute)
	e.finishedAgo(t, "slow", store.Lost, 20*time.Minute)
	for i := 3; i <= 12; i++ {
		e.finishedAgo(t, "quoted", store.Success, time.Duration(i)*time.Hour)
	}
	c := e.login(t, "admin")
	for _, path := range []string{"/?tab=now", "/queue"} {
		code, page := get(t, c, e.srv.URL+path)
		eq(t, path, code, http.StatusOK)
		contains(t, page, `<h2>Recent</h2>`)
		eq(t, path+" ended rows", endedRows(page), 10)
		contains(t, page, `<span class="status cancelled">cancelled</span>`, `<span class="status lost">lost</span>`)
	}
	code, page := get(t, c, e.srv.URL+"/jobs")
	eq(t, "/jobs", code, http.StatusOK)
	eq(t, "/jobs ended rows", endedRows(page), 0)
}

func TestAViewerNeverGetsTheEndedBuildsOfAJobTheyMayNotSeeAndStillGetsTen(t *testing.T) {
	e := site(t)
	for i := 1; i <= 10; i++ {
		e.finishedAgo(t, "nightly", store.Failed, time.Duration(i)*time.Minute)
	}
	for i := 1; i <= 11; i++ {
		job := "smoke"
		if i%2 == 0 {
			job = "slow"
		}
		e.finishedAgo(t, job, store.Success, time.Duration(i)*time.Hour)
	}
	bob := e.login(t, "bob")
	for _, path := range []string{"/?tab=now", "/queue"} {
		_, page := get(t, bob, e.srv.URL+path)
		eq(t, path+" ended rows", endedRows(page), 10)
		lacks(t, page, "nightly #")
	}
	_, page := get(t, e.login(t, "admin"), e.srv.URL+"/queue")
	contains(t, page, "nightly #10")
}
