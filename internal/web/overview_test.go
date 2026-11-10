package web

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/overview"
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
		// A group that built nothing is a line with what it holds, each count a link to its list.
		`<a href="/jobs?group=backend"><text class="count"`, `<a href="/jobs?only=ungrouped"><text class="count"`)
	lacks(t, page, `<table class="jobs stacked">`, `id="jobs"`, "Removed from the job files")
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
	contains(t, page, "Nothing is failing.")
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
	contains(t, page, `<table class="jobs stacked">`, `<h2>backend</h2>`, `<h2>No group</h2>`, `id="jobs"`, `hx-get="/jobs"`)
	lacks(t, page, "Failing now")
}

// finishedAgo writes one build of a job with the status, ended ago ago.
func (e *env) finishedAgo(t *testing.T, job string, st store.Status, ago time.Duration) {
	t.Helper()
	ctx := context.Background()
	b := &store.Build{Job: job, Status: store.Running, Trigger: "cli", QueuedAt: time.Now().Add(-ago - 2*time.Minute)}
	must(t, e.st.CreateBuild(ctx, b))
	must(t, e.st.SetBuildStatus(ctx, b.ID, store.Running, time.Now().Add(-ago-time.Minute)))
	must(t, e.st.SetBuildStatus(ctx, b.ID, st, time.Now().Add(-ago)))
}

func (e *env) overviewAt(t *testing.T, query string) string {
	t.Helper()
	code, page := get(t, e.login(t, "admin"), e.srv.URL+"/"+query)
	eq(t, "the overview"+query, code, http.StatusOK)
	return page
}

func TestTheOverviewDrawsThreeGraphsAsSVGWithTheirNumbersAsText(t *testing.T) {
	e := site(t)
	e.finishedAgo(t, "smoke", store.Success, time.Hour)
	e.finishedAgo(t, "smoke", store.Success, 5*time.Hour)
	e.finishedAgo(t, "smoke", store.Failed, 2*time.Hour)
	e.finishedAgo(t, "smoke", store.Unstable, 3*time.Hour)
	page := e.overviewAt(t, "")
	eq(t, "three graphs", strings.Count(page, `<svg class="plot"`), 3)
	contains(t, page,
		// Each graph names itself and says what it shows, for a screen reader.
		`role="img" aria-labelledby="chart-builds-title chart-builds-desc"`,
		`<title id="chart-builds-title">Builds over time: how the builds that ended in the last 24 hours ended</title>`,
		"In the last 24 hours, 2 builds ended success, 1 unstable, 1 failed and 0 cancelled.",
		`aria-labelledby="chart-slots-title chart-slots-desc"`, "at most 1 builds ran at once",
		`aria-labelledby="chart-rates-title chart-rates-desc"`,
		// The pass rate of a group, in the words of the page: 3 green of 4.
		"backend 75% of 4",
		// The numbers are in a table too, and in the legend.
		`<div class="sr-only"><table>`, "<td>1</td><td>0</td><td>0</td><td>0</td>", "<td>0</td><td>1</td><td>0</td><td>0</td>", "success 2", "failed 1")
	lacks(t, page, "<script src=\"https://", "<img")
}

func TestTheOverviewTakesAWindowOfAWeekAndTheControlShowsWhichOneHolds(t *testing.T) {
	e := site(t)
	e.finishedAgo(t, "smoke", store.Failed, 3*24*time.Hour)
	e.finishedAgo(t, "smoke", store.Success, time.Hour)
	day := e.overviewAt(t, "")
	week := e.overviewAt(t, "?window=7d")

	contains(t, day, `<a href="/" aria-current="true">24 hours</a>`, `<a href="/?window=7d">7 days</a>`, `hx-get="/" hx-select="#overview"`,
		"<h2>Failed in the last 24 hours</h2>", "Nothing failed in the last 24 hours.", "ended in the last 24 hours")
	lacks(t, day, `href="/jobs/smoke/1"`)
	// The week's page keeps its window when it redraws itself.
	contains(t, week, `<a href="/">24 hours</a>`, `<a href="/?window=7d" aria-current="true">7 days</a>`, `hx-get="/?window=7d" hx-select="#overview"`,
		"<h2>Failed in the last 7 days</h2>", `<a class="link" href="/jobs/smoke/1">#1</a>`, "ended in the last 7 days")
	lacks(t, week, "Nothing failed in the last 7 days.")
	eq(t, "the week's graph has its 29 rows", strings.Count(week, "<tr><td>") >= 29, true)
}

func TestAWindowNobodyKnowsIsTheDayOnThePage(t *testing.T) {
	e := site(t)
	for _, q := range []string{"?window=1h", "?window=", "?window=7D", "?window=30d"} {
		page := e.overviewAt(t, q)
		contains(t, page, `<a href="/" aria-current="true">24 hours</a>`, "<h2>Failed in the last 24 hours</h2>")
	}
}

func TestTheUnstableSectionIsBetweenFailingAndFlakyAndHasItsRows(t *testing.T) {
	e := site(t)
	e.finishedAgo(t, "smoke", store.Success, 4*time.Hour)
	e.finishedAgo(t, "smoke", store.Unstable, 3*time.Hour)
	e.finishedAgo(t, "smoke", store.Unstable, time.Hour)
	e.finishedAgo(t, "slow", store.Unstable, 2*time.Hour)
	e.finishedAgo(t, "slow", store.Failed, time.Hour)
	page := e.overviewAt(t, "")
	_, afterFailing, _ := strings.Cut(page, "<h2>Failing now</h2>")
	failing, rest, _ := strings.Cut(afterFailing, `<h2 class="attn-unstable">Unstable now</h2>`)
	unstable, flaky, _ := strings.Cut(rest, "<h2>Flaky</h2>")
	contains(t, failing, `href="/jobs/slow"`)
	lacks(t, failing, `href="/jobs/smoke"`)
	contains(t, unstable, `href="/jobs/smoke"`, `<span class="status unstable">unstable</span>`, "Unstable since", `<time class="since"`)
	lacks(t, unstable, `href="/jobs/slow"`, "Nothing is unstable.")
	contains(t, flaky, "Nothing is flaky.")
}

// A column wrapper would hold the markup to the wide layout's order; the
// one wrapper is around Failing now and Unstable now, adjacent in both.
func TestTheOverviewHasNoColumnWrapperAndTheQueueDoesNotRefreshTwice(t *testing.T) {
	e := site(t)
	page := e.overviewAt(t, "")
	_, columns, _ := strings.Cut(page, `<div class="columns">`)
	columns, _, _ = strings.Cut(columns, `<section class="charts stack"`)
	lacks(t, columns, `class="col"`)
	red, _, _ := strings.Cut(columns, `<div class="sec sec-now">`)
	if !strings.HasPrefix(red, "\n<div class=\"red-now\">\n<section class=\"sec sec-failing\">") || !strings.Contains(red, "Unstable now</h2>") {
		t.Fatalf("Failing now and Unstable now are one grid item:\n%s", red)
	}
	contains(t, columns, `<section id="queue" class="stack">`, "<h2>Coming up</h2>", "<h2>Failing now</h2>", `Unstable now</h2>`, "<h2>Flaky</h2>", "<h2>Failed in the last 24 hours</h2>")
	lacks(t, page, "<h2>Groups</h2>")
	// The queue's own page and the job list still refresh it themselves.
	_, jobs := get(t, e.login(t, "admin"), e.srv.URL+"/jobs")
	contains(t, jobs, `<section id="queue" class="stack" hx-get="/queue"`)
	lacks(t, page, `<section id="queue" class="stack" hx-get="/queue"`)
}

// Visual order never differs from the markup's: no section is moved with
// order, and the wide layout is placed by grid areas.
func TestTheOverviewIsPlacedByGridAreasAndNeverReordered(t *testing.T) {
	b, err := staticFS.ReadFile("static/kipp.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	if regexp.MustCompile(`\.(sec|red-now)[\w-]*\b[^{]*\{[^}]*\border\s*:`).MatchString(css) {
		t.Error("kipp.css moves an overview section with order")
	}
	wide := `@media (min-width: 75rem) {
    .columns {
      grid-template-columns: repeat(2, minmax(0, 1fr));
      grid-template-rows: auto auto auto 1fr;
      grid-template-areas: "red now" "red coming" "flaky coming" "failed coming";
      align-items: start;
    }
    .columns:has(#queue tr:nth-child(2)):not(:has(.red-now table)) {
      grid-template-rows: auto auto auto auto 1fr;
      grid-template-areas: "red now" "flaky now" "flaky coming" "flaky coming" "failed coming";
    }
    .red-now { grid-area: red; }
    .sec-now { grid-area: now; }
    .sec-flaky { grid-area: flaky; }
    .sec-failed { grid-area: failed; }
    .sec-coming { grid-area: coming; }
  }`
	contains(t, css, wide)
}

func TestTheGroupsAreInThePassRateGraphWithTheirNeverBuiltAndDisabledCountsAsLinks(t *testing.T) {
	e := site(t)
	e.finishedAgo(t, "smoke", store.Success, time.Hour)
	page := e.overviewAt(t, "")
	lacks(t, page, "<h2>Groups</h2>", "<th>Running</th>")
	// Smoke built, and no other job has: the ungrouped ones are listed by
	// the count alone, with no builds.
	contains(t, page, "No group no builds, ", " never built", `<a href="/jobs?only=ungrouped"><text class="count"`, "<th>Never built</th><th>Disabled</th>")
	lacks(t, page, `<a href="/jobs?group=backend"><text`)
}

func TestACountThatIncludesBranchesAndPullRequestsSaysSoInItsTitleAndAZeroCountIsNotShown(t *testing.T) {
	c := &overview.Charts{Range: overview.Day, Groups: []overview.GroupRate{
		{Name: "backend", Now: store.Tally{Green: 9, Red: 1}, Disabled: 3, DisabledChildren: 2},
		{Name: "web", Now: store.Tally{Green: 1}, NeverBuilt: 1},
		{Name: "quiet", Now: store.Tally{Green: 1}},
	}}
	ch := ratesChart(c)
	var got []string
	for _, l := range ch.Labels {
		if l.Href != "" {
			got = append(got, l.Href+" "+l.Text+" | "+l.Title)
		}
	}
	want := []string{
		"/jobs?group=backend 3 disabled | 3 disabled (includes branches and pull requests)",
		"/jobs?group=web 1 never built | 1 never built",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("%q\nwant %q", got, want)
	}
	eq(t, "tooltip words", ch.Rows[0][1], "90% of 10, 3 disabled")
}

func TestComingUpFoldsFrequentJobsIntoOneLineThatListsThem(t *testing.T) {
	e := site(t)
	jobs := filepath.Join(t.TempDir(), "jobs")
	must(t, os.MkdirAll(jobs, 0o750))
	write := func(name, cron string) {
		body := "name: " + name + "\ntriggers: {cron: '" + cron + "'}\nstages:\n  s: {run: echo s}\n"
		must(t, os.WriteFile(filepath.Join(jobs, name+".yml"), []byte(body), 0o600))
	}
	write("pinger", "*/5 * * * *")
	write("beat", "H/2 * * * *")
	snap, err := config.Load(jobs)
	must(t, err)
	e.d.SetSnapshot(snap)
	page := e.overviewPage(t, "admin")
	_, coming, _ := strings.Cut(page, "<h2>Coming up</h2>")
	coming, _, _ = strings.Cut(coming, "</section>")
	contains(t, coming, "2 jobs run every few minutes", `<details class="frequent">`, `href="/jobs/pinger"`, `href="/jobs/beat"`)
	lacks(t, coming, "<table", "Nothing is scheduled.")

	write("daily", "0 9 * * *")
	snap, err = config.Load(jobs)
	must(t, err)
	e.d.SetSnapshot(snap)
	_, coming, _ = strings.Cut(e.overviewPage(t, "admin"), "<h2>Coming up</h2>")
	coming, _, _ = strings.Cut(coming, "</section>")
	contains(t, coming, "<table", `href="/jobs/daily"`, "2 jobs run every few minutes")
	lacks(t, strings.SplitN(coming, "</table>", 2)[0], `href="/jobs/pinger"`)
}
