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
		"Nothing runs and nothing waits.", "Nothing is failing.", "Nothing is flaky.",
		`hx-get="/" hx-select="#overview"`, `hx-trigger="kipp-changed from:body, every 60s"`,
		// A group that built nothing is a line with what it holds, each count a link to its list.
		`<a href="/jobs?group=backend"><text class="count"`, `<a href="/jobs?only=ungrouped"><text class="count"`)
	lacks(t, page, "Failed in the last", "Nothing failed in the last", "Coming up", "Nothing is scheduled.", `<table class="jobs stacked">`, `id="jobs"`, "Removed from the job files")
}

func TestTheOverviewShowsWhatIsRedAndWhatFlaps(t *testing.T) {
	e := site(t)
	e.finished(t, "smoke", "fsss")                // red now
	e.finished(t, "slow", "ssssfssssfsssssfsssf") // flaky, green now: 4 of 20
	e.finished(t, "quoted", "ssssssfffff")        // green now, one flip
	page := e.overviewPage(t, "admin")
	failing, rest, _ := strings.Cut(page, "<h2>Flaky</h2>")
	contains(t, failing, `<a class="link" href="/jobs/smoke">smoke</a>`, `<a class="link" href="/jobs/smoke/4">#4</a> <span class="status failed">failed</span>`, `<span class="tl">red </span><time class="since" datetime="`)
	lacks(t, failing, "Nothing is failing.", `href="/jobs/slow"`, `<span class="tag">flaky</span>`)
	flaky, _, _ := strings.Cut(rest, `<section class="charts`)
	contains(t, flaky, `<a class="link" href="/jobs/slow">slow</a>`, "20% failed", "(4/20)", "7 flips")
	lacks(t, flaky, `href="/jobs/quoted"`, `href="/jobs/smoke"`, "Nothing is flaky.")
	lacks(t, page, "Failed in the last", "green again", "still red", "more failure")
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
		`<h2 id="charts-heading">The last 24 hours</h2>`)
	lacks(t, day, "Failed in the last", "Nothing failed", "Coming up")
	// The week's page keeps its window when it redraws itself.
	contains(t, week, `<a href="/">24 hours</a>`, `<a href="/?window=7d" aria-current="true">7 days</a>`, `hx-get="/?window=7d" hx-select="#overview"`,
		`<h2 id="charts-heading">The last 7 days</h2>`)
	lacks(t, week, "Failed in the last", "Nothing failed", "Coming up", `href="/jobs/smoke/1"`)
	eq(t, "the week's graph has its 29 rows", strings.Count(week, "<tr><td>") >= 29, true)
}

func TestAWindowNobodyKnowsIsTheDayOnThePage(t *testing.T) {
	e := site(t)
	for _, q := range []string{"?window=1h", "?window=", "?window=7D", "?window=30d"} {
		page := e.overviewAt(t, q)
		contains(t, page, `<a href="/" aria-current="true">24 hours</a>`, `<h2 id="charts-heading">The last 24 hours</h2>`)
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
	contains(t, unstable, `href="/jobs/smoke"`, `<span class="status unstable">unstable</span>`, "Unstable since", `<span class="tl">unstable </span><time class="since" datetime="`)
	if !regexp.MustCompile(`unstable </span><time class="since"[^>]*>\d+h \d+m</time>`).MatchString(unstable) {
		t.Errorf("a state that holds reads as a duration with no seconds:\n%s", unstable)
	}
	lacks(t, unstable, `href="/jobs/slow"`, "Nothing is unstable.")
	contains(t, flaky, "Nothing is flaky.")
}

// A column wrapper would hold the markup to the wide layout's order: every
// section is a grid item of its own, and the queue refreshes once.
func TestTheOverviewHasNoColumnWrapperAndTheQueueDoesNotRefreshTwice(t *testing.T) {
	e := site(t)
	page := e.overviewAt(t, "")
	_, columns, _ := strings.Cut(page, `<div class="columns">`)
	columns, _, _ = strings.Cut(columns, `<section class="charts stack"`)
	lacks(t, columns, `class="col"`, `class="red-now"`)
	if !strings.HasPrefix(columns, "\n<section class=\"sec sec-failing") {
		t.Fatalf("Failing now is the first grid item:\n%s", columns)
	}
	contains(t, columns, `<div class="sec sec-now">`, `<section id="queue" class="stack">`, "<h2>Failing now</h2>", `Unstable now</h2>`, "<h2>Flaky</h2>")
	lacks(t, columns, "Coming up", "Failed in the last")
	lacks(t, page, "<h2>Groups</h2>")
	// The queue's own page and the job list still refresh it themselves.
	_, jobs := get(t, e.login(t, "admin"), e.srv.URL+"/jobs")
	contains(t, jobs, `<section id="queue" class="stack" hx-get="/queue"`)
	lacks(t, page, `<section id="queue" class="stack" hx-get="/queue"`)
}

// No section is moved with order: the layouts from 48rem are placed by grid
// areas (TestTheMarkupIsInThePhonesOrderAndGridAreasKeepTheWideLayout).
func TestTheOverviewIsPlacedByGridAreasAndNeverReordered(t *testing.T) {
	css := kippCSS(t)
	if regexp.MustCompile(`\.(sec|columns)[\w-]*\b[^{]*\{[^}]*\border\s*:`).MatchString(css) {
		t.Error("kipp.css moves an overview section with order")
	}
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

// emptySite is a server with no job file and no build ever.
func emptySite(t *testing.T) *env {
	t.Helper()
	e := site(t)
	jobs := filepath.Join(t.TempDir(), "jobs")
	must(t, os.MkdirAll(jobs, 0o750))
	snap, err := config.Load(jobs)
	must(t, err)
	e.d.SetSnapshot(snap)
	return e
}

func TestTheOverviewOfAServerWithNoJobAndNoBuildAnswersOkInBothWindowsWithEmptyTextsAndGraphsOfZero(t *testing.T) {
	e := emptySite(t)
	c := e.login(t, "admin")
	for window, label := range map[string]string{"": "24 hours", "?window=7d": "7 days"} {
		code, page := get(t, c, e.srv.URL+"/"+window)
		eq(t, "the front page "+window, code, http.StatusOK)
		contains(t, page, "Nothing runs and nothing waits.", "Nothing is failing.", "Nothing is unstable.", "Nothing is flaky.",
			"The last "+label+"</h2>", `aria-current="true">`+label+"</a>")
		eq(t, "three graphs "+window, strings.Count(page, `<svg class="plot"`), 3)
		lacks(t, page, "Failed in the last", "Coming up", "Internal Server Error", "<h2>Failing now</h2><table")
	}
	_, page := get(t, c, e.srv.URL+"/")
	contains(t, page, "In the last 24 hours, 0 builds ended success, 0 unstable, 0 failed and 0 cancelled.")
	for _, path := range []string{"/jobs", "/queue"} {
		code, _ := get(t, c, e.srv.URL+path)
		eq(t, path, code, http.StatusOK)
	}
}

// On a phone each time names itself in its own text and the stage names
// of a running build are not drawn; a stacked table's box never scrolls down.
func TestOnAPhoneTimesNameThemselvesAndStackedBoxesDoNotScroll(t *testing.T) {
	b, err := staticFS.ReadFile("static/kipp.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	_, phone, ok := strings.Cut(css, "@media (max-width: 47.99rem) {\n    .stacked, .stacked tbody")
	if !ok {
		t.Fatal("the stacked-table rules are not found")
	}
	if !regexp.MustCompile(`\.tl\s*\{[^}]*display:\s*none`).MatchString(css) {
		t.Error("the word before a time is drawn on a wide screen")
	}
	for _, rule := range []string{`\.stacked \.tl\s*\{[^}]*display:\s*inline`, `\.stages\s*\{[^}]*display:\s*none`, `\.scroll:has\(> \.stacked\)\s*\{[^}]*overflow:\s*visible`} {
		if !regexp.MustCompile(rule).MatchString(phone) {
			t.Errorf("under 48rem there is no rule %s", rule)
		}
	}
}

// healthLine is the status line a phone reads before every section.
func healthLine(t *testing.T, page string) string {
	t.Helper()
	_, line, ok := strings.Cut(page, `<p class="health" role="status">`)
	if !ok {
		t.Fatalf("the overview has no status line:\n%s", page)
	}
	line, _, _ = strings.Cut(line, "</p>")
	return line
}

func TestTheStatusLineSaysNothingFailingOrHowManyFailAndHowManyAreUnstable(t *testing.T) {
	e := site(t)
	page := e.overviewAt(t, "")
	_, overview, _ := strings.Cut(page, `<div id="overview"`)
	_, overview, _ = strings.Cut(overview, ">\n")
	if !strings.HasPrefix(overview, `<p class="health" role="status">`) {
		t.Errorf("the status line is not the overview's first thing:\n%s", overview)
	}
	eq(t, "nothing red", healthLine(t, page), `<span class="health-ok">Nothing failing</span>`)

	e.finished(t, "smoke", "fs")
	e.finished(t, "slow", "fs")
	eq(t, "two failing", healthLine(t, e.overviewAt(t, "")), `<a class="link health-bad" href="#failing">2 failing</a>`)

	e.finishedAgo(t, "quoted", store.Unstable, time.Minute)
	eq(t, "two failing, one unstable", healthLine(t, e.overviewAt(t, "")),
		`<a class="link health-bad" href="#failing">2 failing</a> <a class="link health-warn" href="#unstable">1 unstable</a>`)

	e.finishedAgo(t, "smoke", store.Success, 0)
	e.finishedAgo(t, "slow", store.Success, 0)
	eq(t, "one unstable", healthLine(t, e.overviewAt(t, "")),
		`<span class="health-ok">Nothing failing</span> <a class="link health-warn" href="#unstable">1 unstable</a>`)
}

// Under 48rem the status line stands for an empty Failing or Unstable
// section, which is then not drawn; on a wide screen the line is not drawn.
func TestOnAPhoneAnEmptyFailingOrUnstableSectionIsNotDrawnAndTheStatusLineOnlyThere(t *testing.T) {
	e := site(t)
	page := e.overviewAt(t, "")
	contains(t, page, `<section class="sec sec-failing sec-quiet" id="failing">`, `<section class="sec sec-unstable sec-quiet" id="unstable">`,
		`<section class="sec sec-flaky">`, "Nothing is failing.", "Nothing is unstable.")
	e.finished(t, "smoke", "f")
	e.finishedAgo(t, "quoted", store.Unstable, time.Minute)
	page = e.overviewAt(t, "")
	contains(t, page, `<section class="sec sec-failing" id="failing">`, `<section class="sec sec-unstable" id="unstable">`)
	lacks(t, page, "sec-quiet")

	css := kippCSS(t)
	if !regexp.MustCompile(`\.sec-quiet\s*\{[^}]*display:\s*none`).MatchString(phoneRules(t, css)) {
		t.Error("under 48rem an empty section is drawn")
	}
	if !regexp.MustCompile(`@media \(min-width: 48rem\) \{\s*\.health \{ display: none; \}`).MatchString(css) {
		t.Error("the status line is drawn on a wide screen")
	}
}

// The markup is in the phone's order (Failing, Now, Unstable, Flaky), so a
// phone reads it as it is written; from 48rem grid areas place each section
// where it stood before.
func TestTheMarkupIsInThePhonesOrderAndGridAreasKeepTheWideLayout(t *testing.T) {
	e := site(t)
	page := e.overviewAt(t, "")
	order := []string{`<p class="health"`, `id="failing"`, `<section id="queue"`, `id="unstable"`, `<section class="sec sec-flaky">`, `<section class="charts stack"`}
	at := -1
	for _, s := range order {
		i := strings.Index(page, s)
		if i <= at {
			t.Fatalf("%s is not after %s", s, order)
		}
		at = i
	}
	lacks(t, page, "red-now")
	css := kippCSS(t)
	contains(t, css, `@media (min-width: 48rem) {
    .columns { grid-template-areas: "failing" "unstable" "now" "flaky"; }
    .sec-failing { grid-area: failing; }
    .sec-unstable { grid-area: unstable; }
    .sec-now { grid-area: now; }
    .sec-flaky { grid-area: flaky; }
  }`, `@media (min-width: 75rem) {
    .columns {
      grid-template-columns: repeat(2, minmax(0, 1fr));
      grid-template-rows: auto auto 1fr;
      grid-template-areas: "failing now" "unstable now" "flaky now";
      align-items: start;
    }
  }`)
}

// A strip of dots is newest first, and says so to the eye (a word at its
// newest end and a ring round the newest dot) and to a screen reader (each
// dot's own text).
func TestEveryStripOfDotsSaysWhichEndIsNewest(t *testing.T) {
	e := site(t)
	e.finished(t, "smoke", "fsss")
	e.finished(t, "slow", "ssssfssssfsssssfsssf")
	e.finishedAgo(t, "quoted", store.Unstable, time.Minute)
	page := e.overviewAt(t, "")
	_, jobs := get(t, e.login(t, "admin"), e.srv.URL+"/jobs")
	strip := regexp.MustCompile(`<span class="dots"><span class="dots-newest" aria-hidden="true">newest</span>(<a [^>]*><span class="sr-only">[^<]*</span></a>)+</span>`)
	for what, want := range map[string]struct {
		page    string
		strips  int
		smokeAt string
	}{"overview": {page, 3, `<a class="dot failed newest" href="/jobs/smoke/4" title="#4 failed"><span class="sr-only">#4 failed, newest</span></a><a class="dot success" href="/jobs/smoke/3" title="#3 success"><span class="sr-only">#3 success</span></a>`},
		"jobs": {jobs, 3, `<a class="dot failed newest" href="/jobs/smoke/4" title="#4 failed"><span class="sr-only">#4 failed, newest</span></a>`}} {
		if n := len(strip.FindAllString(want.page, -1)); n != want.strips {
			t.Errorf("%s: %d strips with the newest cue, want %d", what, n, want.strips)
		}
		eq(t, what+" strips", strings.Count(want.page, `<span class="dots">`), want.strips)
		contains(t, want.page, want.smokeAt)
		eq(t, what+" newest dots", strings.Count(want.page, ` newest" href=`), want.strips)
	}
	css := kippCSS(t)
	for _, rule := range []string{`\.dots-newest\s*\{[^}]*color:\s*var\(--fg-subtle\)`, `\.dot\.newest\s*\{[^}]*outline:`} {
		if !regexp.MustCompile(rule).MatchString(css) {
			t.Errorf("kipp.css has no rule %s", rule)
		}
	}
}

// On a phone a row of Now, Failing and Unstable is two lines: what it is and
// what to do with it, then its state and time.
func TestOnAPhoneTheRowsOfNowFailingAndUnstableAreTwoLines(t *testing.T) {
	e := site(t)
	e.lockJobs(t)
	c := e.login(t, "admin")
	e.build(t, c, "hold", nil)
	e.queued(t, 1, 0)
	e.finished(t, "smoke", "fs")
	e.finishedAgo(t, "quoted", store.Unstable, time.Minute)
	page := e.overviewAt(t, "")
	contains(t, page, `<table class="queue stacked two-line">`, `<table class="rows stacked two-line">`)
	eq(t, "two-line tables", strings.Count(page, "two-line"), 3)
	_, flaky, _ := strings.Cut(page, `<section class="sec sec-flaky">`)
	lacks(t, flaky, "two-line")
	_, queue := get(t, c, e.srv.URL+"/queue")
	contains(t, queue, `<table class="queue stacked two-line">`)

	phone := phoneRules(t, kippCSS(t))
	for _, rule := range []string{
		`\.two-line tr\s*\{[^}]*display:\s*grid`,
		`\.two-line \.status\s*\{[^}]*display:\s*none`,
		`\.queue td:first-child \.link\s*\{[^}]*text-overflow:\s*ellipsis`,
		`\.queue td:nth-child\(3\)::before\s*\{[^}]*background-color:\s*var\(--busy\)[^}]*animation:`,
		`\.cancel-word\s*\{[^}]*display:\s*none`,
		`\.cancel\s*\{[^}]*min-width:\s*44px[^}]*min-height:\s*44px`,
	} {
		if !regexp.MustCompile(rule).MatchString(phone) {
			t.Errorf("under 48rem there is no rule %s", rule)
		}
	}
	css := kippCSS(t)
	if !regexp.MustCompile(`@media \(prefers-reduced-motion: reduce\) \{\s*\.queue td:nth-child\(3\)::before \{ animation: none; \}`).MatchString(css) {
		t.Error("the live dot pulses under prefers-reduced-motion: reduce")
	}
}

// The cancel control is a cross on a phone and the word on a wide screen,
// one link named for the build it cancels, leading to the same question.
func TestTheQueuesCancelControlIsNamedForItsBuildAndAsksTheSameQuestion(t *testing.T) {
	e := site(t)
	e.lockJobs(t)
	c := e.login(t, "admin")
	e.build(t, c, "hold", nil)
	e.queued(t, 1, 0)
	_, page := get(t, c, e.srv.URL+"/queue")
	if !regexp.MustCompile(`<a class="action action-small cancel" href="/jobs/hold/1/cancel\?next=%2fqueue" aria-label="Cancel hold #1"><span class="cancel-word">Cancel...</span><svg class="icon cancel-x" viewBox="0 0 24 24" aria-hidden="true">`).MatchString(page) {
		_, row, _ := strings.Cut(page, `data-label="Cancel"`)
		t.Fatalf("the cancel control:\n%.400s", row)
	}
}

func kippCSS(t *testing.T) string {
	t.Helper()
	b, err := staticFS.ReadFile("static/kipp.css")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// phoneRules is every block of kipp.css under 48rem, one after another.
func phoneRules(t *testing.T, css string) string {
	t.Helper()
	var out strings.Builder
	for rest := css; ; {
		_, after, ok := strings.Cut(rest, "@media (max-width: 47.99rem")
		if !ok {
			break
		}
		depth := 0
		for i, r := range after {
			if r == '{' {
				depth++
			} else if r == '}' {
				if depth--; depth == 0 {
					out.WriteString(after[:i+1])
					break
				}
			}
		}
		rest = after
	}
	if out.Len() == 0 {
		t.Fatal("kipp.css has no rule under 48rem")
	}
	return out.String()
}
