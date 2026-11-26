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

func (e *env) overviewPage(t *testing.T) string {
	t.Helper()
	return e.overviewTabPage(t, "admin", "")
}

func (e *env) overviewTabPage(t *testing.T, user, tab string) string {
	t.Helper()
	path := "/"
	if tab != "" {
		path += "?tab=" + tab
	}
	_, page := get(t, e.login(t, user), e.srv.URL+path)
	_, body, _ := strings.Cut(page, `<div id="overview"`)
	return body
}

func panelOf(t *testing.T, page string) string {
	t.Helper()
	_, panel, ok := strings.Cut(page, `<div id="overview-panel"`)
	if !ok {
		t.Fatalf("no panel:\n%s", page)
	}
	panel, _, _ = strings.Cut(panel, `<section id="charts"`)
	return panel
}

func TestTheFrontPageIsTheOverviewAndSaysSoInEverySectionWhenThereIsNothingToShow(t *testing.T) {
	e := site(t)
	code, page := get(t, e.login(t, "admin"), e.srv.URL+"/")
	eq(t, "the front page", code, http.StatusOK)
	contains(t, page, "<title>Overview - kipp</title>", `<h1 class="sr-only">Overview</h1>`, `<section id="queue"`,
		"Nothing runs and nothing waits.",
		`hx-get="/" hx-select="#overview"`, `hx-trigger="kipp-changed from:body, every 60s"`,
		`<a href="/jobs?group=backend"><text class="count"`, `<a href="/jobs?only=ungrouped"><text class="count"`)
	lacks(t, page, "Nothing is failing.", "Nothing is flaky.", "Failed in the last", "Nothing failed in the last", "Coming up", "Nothing is scheduled.", `<table class="jobs stacked">`, `id="jobs"`, "Removed from the job files")
}

func TestTheOverviewShowsWhatIsRedAndWhatFlaps(t *testing.T) {
	e := site(t)
	e.finished(t, "smoke", "fsss")
	e.finished(t, "slow", "ssssfssssfsssssfsssf")
	e.finished(t, "quoted", "ssssssfffff")
	failing := panelOf(t, e.overviewTabPage(t, "admin", "failing"))
	contains(t, failing, `<a class="link" href="/jobs/smoke">smoke</a>`, `<a class="link" href="/jobs/smoke/4">#4</a> <span class="status failed">failed</span>`, `<span class="tl">red </span><time class="since" datetime="`)
	lacks(t, failing, "Nothing is failing.", `href="/jobs/slow"`, `<span class="tag">flaky</span>`)
	flaky := panelOf(t, e.overviewTabPage(t, "admin", "flaky"))
	contains(t, flaky, `<a class="link" href="/jobs/slow">slow</a>`, "20% failed", "(4/20)", "7 flips")
	lacks(t, flaky, `href="/jobs/quoted"`, `href="/jobs/smoke"`, "Nothing is flaky.")
	lacks(t, failing+flaky, "Failed in the last", "green again", "still red", "more failure")
	if got := strings.Count(failing, `<a class="dot `); got != 4 {
		t.Fatalf("%d dots beside smoke, want its 4 builds", got)
	}
}

func TestAFailingJobThatFlapsWearsTheFlakyTagAndStaysOutOfFlaky(t *testing.T) {
	e := site(t)
	e.finished(t, "smoke", "ffffssffssf")
	contains(t, panelOf(t, e.overviewTabPage(t, "admin", "failing")), `<span class="tag">flaky</span>`)
	contains(t, panelOf(t, e.overviewTabPage(t, "admin", "flaky")), "Nothing is flaky.")
}

func TestTheOverviewKeepsToWhatTheViewerMayView(t *testing.T) {
	e := site(t)
	e.finished(t, "smoke", "ffff")
	e.finished(t, "slow", "ffff")
	page := e.overviewTabPage(t, "alice", "failing")
	contains(t, page, `href="/jobs/smoke"`)
	lacks(t, page, `href="/jobs/slow"`, `href="/jobs/nightly"`, "No group")
	ctx := context.Background()
	_, err := e.st.CreateUser(ctx, "carol", hashOnce(), false)
	must(t, err)
	page = e.overviewTabPage(t, "carol", "failing")
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
		`role="img" aria-labelledby="chart-builds-title chart-builds-desc"`,
		`<title id="chart-builds-title">Builds over time: how the builds that ended in the last 24 hours ended</title>`,
		"In the last 24 hours, 2 builds ended success, 1 unstable, 1 failed and 0 cancelled.",
		`aria-labelledby="chart-slots-title chart-slots-desc"`, "at most 1 builds ran at once",
		"each runner has a limit of its own", "Running and queue",
		`aria-labelledby="chart-rates-title chart-rates-desc"`,
		"backend 75% of 4",
		`<div class="sr-only"><table>`, "<td>1</td><td>0</td><td>0</td><td>0</td>", "<td>0</td><td>1</td><td>0</td><td>0</td>", "success 2", "failed 1")
	lacks(t, page, "<script src=\"https://", "<img", `class="limit"`, "Slots and queue", "the limit is")
}

func TestTheOverviewTakesAWindowOfAWeekAndTheControlShowsWhichOneHolds(t *testing.T) {
	e := site(t)
	e.finishedAgo(t, "smoke", store.Failed, 3*24*time.Hour)
	e.finishedAgo(t, "smoke", store.Success, time.Hour)
	day := e.overviewAt(t, "")
	week := e.overviewAt(t, "?window=7d")

	contains(t, day, `<a href="/" data-range="24h" hx-get="/overview/charts?window=24h" hx-target="#charts" hx-swap="outerHTML" hx-select="unset" aria-current="true">24 hours</a>`,
		`<a href="/?window=7d" data-range="7d" hx-get="/overview/charts?window=7d"`, `hx-get="/" hx-select="#overview"`,
		`<h2 id="charts-heading">The last 24 hours</h2>`)
	lacks(t, day, "Failed in the last", "Nothing failed", "Coming up")
	contains(t, week, `<a href="/" data-range="24h"`, `hx-select="unset" aria-current="true">7 days</a>`,
		`<h2 id="charts-heading">The last 7 days</h2>`)
	lacks(t, week, "Failed in the last", "Nothing failed", "Coming up")
	eq(t, "the week's graph has its 29 rows", strings.Count(week, "<tr><td>") >= 29, true)
}

func TestAWindowNobodyKnowsIsTheDayOnThePage(t *testing.T) {
	e := site(t)
	for _, q := range []string{"?window=1h", "?window=", "?window=7D", "?window=30d"} {
		page := e.overviewAt(t, q)
		contains(t, page, `hx-select="unset" aria-current="true">24 hours</a>`, `<h2 id="charts-heading">The last 24 hours</h2>`)
	}
}

func TestTheUnstableTabHasItsRowsAndFailingNamesOnlyTheFailed(t *testing.T) {
	e := site(t)
	e.finishedAgo(t, "smoke", store.Success, 4*time.Hour)
	e.finishedAgo(t, "smoke", store.Unstable, 3*time.Hour)
	e.finishedAgo(t, "smoke", store.Unstable, time.Hour)
	e.finishedAgo(t, "slow", store.Unstable, 2*time.Hour)
	e.finishedAgo(t, "slow", store.Failed, time.Hour)
	failing := panelOf(t, e.overviewAt(t, "?tab=failing"))
	unstable := panelOf(t, e.overviewAt(t, "?tab=unstable"))
	flaky := panelOf(t, e.overviewAt(t, "?tab=flaky"))
	contains(t, failing, `href="/jobs/slow"`)
	lacks(t, failing, `href="/jobs/smoke"`)
	contains(t, unstable, `href="/jobs/smoke"`, `<span class="status unstable">unstable</span>`, "Unstable since", `<span class="tl">unstable </span><time class="since" datetime="`)
	if !regexp.MustCompile(`unstable </span><time class="since"[^>]*>\d+h \d+m</time>`).MatchString(unstable) {
		t.Errorf("a state that holds reads as a duration with no seconds:\n%s", unstable)
	}
	lacks(t, unstable, `href="/jobs/slow"`, "Nothing is unstable.")
	contains(t, flaky, "Nothing is flaky.")
}

func TestTheOverviewIsOneTabbedSectionWhoseTabsCarryTheirCountsAndOnlyTheOpenPanelIsInTheHTML(t *testing.T) {
	e := site(t)
	e.finished(t, "smoke", "fs")
	e.finished(t, "slow", "fs")
	e.finishedAgo(t, "quoted", store.Unstable, time.Minute)
	page := e.overviewAt(t, "?tab=failing")
	contains(t, page, `role="tablist"`,
		`aria-selected="false" tabindex="-1" hx-get="/overview/tabs/now" hx-target="#overview-panel" hx-swap="outerHTML" hx-select="unset">Now <span class="tab-count">0</span></button>`,
		`id="tab-btn-failing" aria-controls="overview-panel" data-tab="failing" aria-selected="true" hx-get="/overview/tabs/failing"`,
		`Failing <span class="tab-count tab-bad">2</span>`, `Unstable <span class="tab-count tab-warn">1</span>`, `Flaky <span class="tab-count">0</span>`,
		`<div id="overview-panel" class="stack" role="tabpanel" aria-labelledby="tab-btn-failing" data-tab="failing">`)
	lacks(t, panelOf(t, page), "Nothing is flaky.", "Nothing is unstable.", `<section id="queue"`, `href="/jobs/quoted"`)
	lacks(t, page, "hx-push-url", "?tab=")
}

func TestTheDefaultTabIsNowWhateverFailsAndAnUnknownTabIsTheDefault(t *testing.T) {
	e := site(t)
	quiet := e.overviewAt(t, "")
	contains(t, quiet, `data-tab="now" aria-selected="true"`, `<div id="overview-panel" class="stack" role="tabpanel" aria-labelledby="tab-btn-now" data-tab="now">`)
	e.finished(t, "smoke", "f")
	red := e.overviewAt(t, "")
	contains(t, red, `data-tab="now" aria-selected="true"`)
	lacks(t, red, `data-tab="failing" aria-selected="true"`)
	contains(t, e.overviewAt(t, "?tab=flaky"), `data-tab="flaky" aria-selected="true"`, "Nothing is flaky.")
	contains(t, e.overviewAt(t, "?tab=failing"), `data-tab="failing" aria-selected="true"`)
	contains(t, e.overviewAt(t, "?tab=nope"), `data-tab="now" aria-selected="true"`)
}

func TestTheTabPickedLastOpensOnTheNextVisitAndTheAddressStillWins(t *testing.T) {
	e := site(t)
	e.finished(t, "smoke", "f")
	c := e.login(t, "admin")
	code, _ := get(t, c, e.srv.URL+"/overview/tabs/flaky")
	eq(t, "the flaky panel", code, http.StatusOK)
	_, page := get(t, c, e.srv.URL+"/")
	contains(t, page, `data-tab="flaky" aria-selected="true"`, "Nothing is flaky.")
	_, page = get(t, c, e.srv.URL+"/?tab=failing")
	contains(t, page, `data-tab="failing" aria-selected="true"`)
	_, page = get(t, c, e.srv.URL+"/")
	contains(t, page, `data-tab="flaky" aria-selected="true"`)
	other := e.login(t, "admin")
	_, page = get(t, other, e.srv.URL+"/")
	contains(t, page, `data-tab="now" aria-selected="true"`)
}

func TestTheRememberedTabIsALaxCookieForTheWholeSiteAndABadOneIsIgnored(t *testing.T) {
	e := site(t)
	c := e.login(t, "admin")
	req, err := http.NewRequest(http.MethodGet, e.srv.URL+"/overview/tabs/unstable", nil)
	must(t, err)
	res, _ := do(t, c, req)
	var tab *http.Cookie
	for _, k := range res.Cookies() {
		if k.Name == "kipp_overview_tab" {
			tab = k
		}
	}
	if tab == nil {
		t.Fatal("picking a tab sets no cookie")
	}
	eq(t, "value", tab.Value, "unstable")
	eq(t, "path", tab.Path, "/")
	eq(t, "same site", tab.SameSite, http.SameSiteLaxMode)
	eq(t, "http only", tab.HttpOnly, true)
	if tab.MaxAge < 365*24*60*60 {
		t.Errorf("the cookie lasts %ds, less than a year", tab.MaxAge)
	}
	req, err = http.NewRequest(http.MethodGet, e.srv.URL+"/", nil)
	must(t, err)
	req.AddCookie(&http.Cookie{Name: "kipp_overview_tab", Value: "nope", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	_, page := do(t, e.login(t, "admin"), req)
	contains(t, page, `data-tab="now" aria-selected="true"`)
}

func TestEachTabHasItsOwnFragmentRouteThatAnswersThePanelAlone(t *testing.T) {
	e := site(t)
	e.lockJobs(t)
	c := e.login(t, "admin")
	e.build(t, c, "hold", nil)
	e.queued(t, 1, 0)
	e.finished(t, "smoke", "fsss")
	e.finished(t, "slow", "ssssfssssfsssssfsssf")
	e.finishedAgo(t, "quoted", store.Unstable, time.Minute)
	for tab, want := range map[string][]string{
		"now":      {`data-tab="now"`, `<h2>Now `, `<h2>Recent</h2>`, "hold #1"},
		"failing":  {`data-tab="failing"`, `href="/jobs/smoke"`, "Red since"},
		"unstable": {`data-tab="unstable"`, `href="/jobs/quoted"`, "Unstable since"},
		"flaky":    {`data-tab="flaky"`, `href="/jobs/slow"`, "Flips"},
	} {
		code, body := get(t, c, e.srv.URL+"/overview/tabs/"+tab)
		eq(t, tab, code, http.StatusOK)
		if !strings.HasPrefix(body, `<div id="overview-panel" class="stack" role="tabpanel"`) {
			t.Errorf("%s is not the panel alone:\n%.200s", tab, body)
		}
		contains(t, body, want...)
		lacks(t, body, "<html", `role="tablist"`, `<section id="charts"`)
	}
	code, _ := get(t, c, e.srv.URL+"/overview/tabs/nope")
	eq(t, "an unknown tab", code, http.StatusNotFound)
	anon := e.srv.Client()
	anon.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	for _, path := range []string{"/overview/tabs/now", "/overview/charts"} {
		res, _ := do(t, anon, mustGet(t, e.srv.URL+path))
		if res.StatusCode == http.StatusOK {
			t.Errorf("%s is served to nobody who is signed in", path)
		}
	}
}

func TestTheChartsHaveTheirOwnFragmentRouteForBothRangesAndTheHeadingSaysWhichHolds(t *testing.T) {
	e := site(t)
	e.finishedAgo(t, "smoke", store.Success, time.Hour)
	c := e.login(t, "admin")
	for query, label := range map[string]string{"": "24 hours", "?window=24h": "24 hours", "?window=7d": "7 days", "?window=x": "24 hours"} {
		code, body := get(t, c, e.srv.URL+"/overview/charts"+query)
		eq(t, query, code, http.StatusOK)
		if !strings.HasPrefix(body, `<section id="charts" class="charts stack"`) {
			t.Errorf("%s is not the charts alone:\n%.200s", query, body)
		}
		contains(t, body, `<h2 id="charts-heading">The last `+label+`</h2>`, `hx-select="unset" aria-current="true">`+label+`</a>`)
		eq(t, query+" graphs", strings.Count(body, `<svg class="plot"`), 3)
		lacks(t, body, "<html", `role="tablist"`)
	}
}

func TestThePageLoadsTheScriptThatKeepsTheOpenTabAndRangeThroughARefresh(t *testing.T) {
	e := site(t)
	e.finished(t, "smoke", "fs")
	e.finishedAgo(t, "quoted", store.Unstable, time.Minute)
	page := e.overviewAt(t, "")
	contains(t, page, `<script src="/static/overview.js`)
	b, err := staticFS.ReadFile("static/overview.js")
	if err != nil {
		t.Fatal(err)
	}
	contains(t, string(b), `htmx:configRequest`, "parameters.tab", "parameters.window")
}

func TestTheGroupsAreInThePassRateGraphWithTheirNeverBuiltAndDisabledCountsAsLinks(t *testing.T) {
	e := site(t)
	e.finishedAgo(t, "smoke", store.Success, time.Hour)
	page := e.overviewAt(t, "")
	lacks(t, page, "<h2>Groups</h2>", "<th>Running</th>")
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
	for window, label := range map[string]string{"": "24 hours", "?window=7d": "7 days"} {
		c := e.login(t, "admin")
		code, page := get(t, c, e.srv.URL+"/"+window)
		eq(t, "the front page "+window, code, http.StatusOK)
		contains(t, page, "Nothing runs and nothing waits.",
			"The last "+label+"</h2>", `aria-current="true">`+label+"</a>")
		eq(t, "three graphs "+window, strings.Count(page, `<svg class="plot"`), 3)
		lacks(t, page, "Failed in the last", "Coming up", "Internal Server Error")
		for tab, text := range map[string]string{"failing": "Nothing is failing.", "unstable": "Nothing is unstable.", "flaky": "Nothing is flaky."} {
			code, body := get(t, c, e.srv.URL+"/overview/tabs/"+tab)
			eq(t, tab, code, http.StatusOK)
			contains(t, body, text)
		}
	}
	c := e.login(t, "admin")
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

func TestTheOverviewHasNoStatusLineAboveItsTabs(t *testing.T) {
	e := site(t)
	e.finished(t, "smoke", "fs")
	e.finishedAgo(t, "quoted", store.Unstable, time.Minute)
	page := e.overviewAt(t, "")
	lacks(t, page, `class="health`, "data-tab-link", "Nothing failing", "1 failing")
	lacks(t, kippCSS(t), ".health")
}

func TestTheOverviewIsNotPlacedByColumnsAnyMore(t *testing.T) {
	e := site(t)
	page := e.overviewAt(t, "")
	lacks(t, page, `class="columns"`, "sec-failing", "sec-now", "sec-quiet", "red-now")
	css := kippCSS(t)
	lacks(t, css, ".columns", ".sec-failing", ".sec-quiet")
}

// A strip of dots is newest first, and says so to the eye (a word at its
// newest end and a ring round the newest dot) and to a screen reader (each
// dot's own text).
func TestEveryStripOfDotsSaysWhichEndIsNewest(t *testing.T) {
	e := site(t)
	e.finished(t, "smoke", "fsss")
	e.finished(t, "slow", "ssssfssssfsssssfsssf")
	e.finishedAgo(t, "quoted", store.Unstable, time.Minute)
	var page string
	for _, tab := range []string{"failing", "unstable", "flaky"} {
		page += panelOf(t, e.overviewTabPage(t, "admin", tab))
	}
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
	contains(t, panelOf(t, e.overviewTabPage(t, "admin", "now")), `<table class="queue stacked two-line">`)
	contains(t, panelOf(t, e.overviewTabPage(t, "admin", "failing")), `<table class="rows stacked two-line">`)
	contains(t, panelOf(t, e.overviewTabPage(t, "admin", "unstable")), `<table class="rows stacked two-line">`)
	lacks(t, panelOf(t, e.overviewTabPage(t, "admin", "flaky")), "two-line")
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

func TestTheOverviewLeavesOutRedPullRequestsAndSideBranchesAndLinksTheirCount(t *testing.T) {
	e := site(t)
	e.add(t, map[string]string{"site.yml": "name: site\ngroup: web\nmultibranch: {repo: acme/site, clone: /nonexistent/site.git}\n"})
	parent := e.d.Snapshot().Jobs["site"]
	var kids []*config.Job
	for _, c := range []config.ChildRecord{
		{Key: "main", Kind: config.KindBranch, Branch: "main"},
		{Key: "feature", Kind: config.KindBranch, Branch: "feature"},
		{Key: "pr-12", Kind: config.KindPR, Branch: "feature", PR: 12},
	} {
		c.Parent, c.State = "site", config.ChildLive
		k := parent.Child(c)
		k.DefaultBranch = "main"
		kids = append(kids, k)
		e.finished(t, k.Name, "fs")
	}
	e.d.SetSnapshot(e.d.Snapshot().WithJobs(kids...))
	failing := panelOf(t, e.overviewTabPage(t, "admin", "failing"))
	contains(t, failing, `href="/jobs/site.main"`)
	lacks(t, failing, `pull requests and branches`, `side-count`)
	lacks(t, failing, `href="/jobs/site.feature"`, `href="/jobs/site.pr-12"`)
}
