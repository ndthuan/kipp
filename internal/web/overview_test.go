package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/store"
)

// finished writes builds of a job oldest first from a shape read newest
// first (s success, f failed), the newest ending an hour ago.
func (e *env) finished(t *testing.T, job, shape string) {
	t.Helper()
	ctx := context.Background()
	for i := len(shape) - 1; i >= 0; i-- {
		at := time.Now().Add(-time.Duration(i+1) * time.Hour)
		b := &store.Build{Job: job, Status: store.Running, Trigger: "cli", QueuedAt: at}
		must(t, e.st.CreateBuild(ctx, b))
		st := store.Success
		if shape[i] == 'f' {
			st = store.Failed
		}
		must(t, e.st.SetBuildStatus(ctx, b.ID, st, at))
	}
}

// overviewPage is the front page below the header, whose menus list the
// jobs the viewer may see.
func (e *env) overviewPage(t *testing.T, user string) string {
	t.Helper()
	_, page := get(t, e.login(t, user), e.srv.URL+"/")
	_, body, _ := strings.Cut(page, `<div id="overview"`)
	return body
}

func TestTheFrontPageIsTheOverviewAndSaysSoInEverySectionWhenThereIsNothingToShow(t *testing.T) {
	e := site(t)
	code, page := get(t, e.login(t, "admin"), e.srv.URL+"/")
	eq(t, "the front page", code, http.StatusOK)
	contains(t, page, "<title>Overview - kipp</title>", `<h1 class="sr-only">Overview</h1>`, `<section id="queue"`,
		"Nothing runs and nothing waits.", "Nothing is failing.", "Nothing is flaky.", "Nothing failed in the last 24 hours.",
		`hx-get="/" hx-select="#overview"`, `hx-trigger="kipp-changed from:body, every 60s"`,
		// The one job with a cron.
		`<code>0 3 * * *</code>`, `<a class="link" href="/jobs/nightly">nightly</a>`,
		// A group is a line with its counts and a link to its list.
		`<a class="link" href="/jobs?group=backend">backend</a>`, `<a class="link" href="/jobs?only=ungrouped">No group</a>`)
	lacks(t, page, `<table class="jobs">`, `id="jobs"`, "Removed from the job files")
}

func TestTheOverviewShowsWhatIsRedAndWhatFlapsAndWhatFailedLately(t *testing.T) {
	e := site(t)
	e.finished(t, "smoke", "fsss")                // red now
	e.finished(t, "slow", "ssssfssssfsssssfsssf") // flaky, green now: 4 of 20
	e.finished(t, "quoted", "ssssssfffff")        // green now, one flip
	page := e.overviewPage(t, "admin")
	failing, rest, _ := strings.Cut(page, "<h2>Flaky</h2>")
	contains(t, failing, `<a class="link" href="/jobs/smoke">smoke</a>`, `<a class="link" href="/jobs/smoke/4">#4</a> <span class="status failed">failed</span>`, `<time class="since"`)
	lacks(t, failing, "Nothing is failing.", `href="/jobs/slow"`, `<span class="tag">flaky</span>`)
	flaky, recent, _ := strings.Cut(rest, "<h2>Failed in the last 24 hours</h2>")
	contains(t, flaky, `<a class="link" href="/jobs/slow">slow</a>`, "20% failed", "(4/20)", "7 flips")
	lacks(t, flaky, `href="/jobs/quoted"`, `href="/jobs/smoke"`, "Nothing is flaky.")
	contains(t, recent, `<a class="link" href="/jobs/smoke/4">#4</a>`, `<a class="link" href="/jobs/slow/16">#16</a>`, "green again", "still red")
	lacks(t, recent, "Nothing failed in the last 24 hours.", "more failure")
	if got := strings.Count(failing, `<a class="dot `); got != 4 {
		t.Fatalf("%d dots beside smoke, want its 4 builds", got)
	}
}

func TestAFailingJobThatFlapsWearsTheFlakyTagAndStaysOutOfFlaky(t *testing.T) {
	e := site(t)
	e.finished(t, "smoke", "ffffssffssf")
	page := e.overviewPage(t, "admin")
	failing, flaky, _ := strings.Cut(page, "<h2>Flaky</h2>")
	contains(t, failing, `<span class="tag">flaky</span>`)
	contains(t, flaky, "Nothing is flaky.")
}

func TestTheOverviewKeepsToWhatTheViewerMayView(t *testing.T) {
	e := site(t)
	e.finished(t, "smoke", "ffff")
	e.finished(t, "slow", "ffff")
	// alice may view smoke alone.
	page := e.overviewPage(t, "alice")
	contains(t, page, `href="/jobs/smoke"`)
	lacks(t, page, `href="/jobs/slow"`, `href="/jobs/nightly"`, "No group")
	// A user with no grant is told there is nothing, not that anything is hidden.
	ctx := context.Background()
	_, err := e.st.CreateUser(ctx, "carol", hashOnce(), false)
	must(t, err)
	page = e.overviewPage(t, "carol")
	contains(t, page, "Nothing is failing.", "There is no job you may see.")
	lacks(t, page, `href="/jobs/smoke"`)
}

func TestTheOldFrontPageLinksLeadOnToTheJobList(t *testing.T) {
	e := site(t)
	c := e.login(t, "admin")
	for from, to := range map[string]string{
		"/?group=ci":           "/jobs?group=ci",
		"/?only=x":             "/jobs?only=x",
		"/?only=removed":       "/jobs?only=removed",
		"/?group=a%20b&only=c": "/jobs?group=a+b&only=c",
	} {
		res, _ := do(t, c, mustGet(t, e.srv.URL+from))
		eq(t, from+" status", res.StatusCode, http.StatusSeeOther)
		eq(t, from+" leads to", res.Header.Get("Location"), to)
	}
	// The list itself answers where the redirect leads.
	code, page := get(t, c, e.srv.URL+"/jobs?group=backend")
	eq(t, "the group's list", code, http.StatusOK)
	contains(t, page, `href="/jobs/smoke"`)
}

func TestTheJobListMovedToJobsAndKeepsEveryGroup(t *testing.T) {
	e := site(t)
	code, page := get(t, e.login(t, "admin"), e.srv.URL+"/jobs")
	eq(t, "the list", code, http.StatusOK)
	contains(t, page, `<table class="jobs">`, `<h2>backend</h2>`, `<h2>No group</h2>`, `id="jobs"`, `hx-get="/jobs"`)
	lacks(t, page, "Failing now")
}
