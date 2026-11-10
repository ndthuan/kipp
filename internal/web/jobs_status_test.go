package web

import (
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/store"
)

func summary(name, group string, st store.Status) api.JobSummary {
	j := api.JobSummary{Name: name, Group: group}
	if st != "" {
		j.Last = &store.Build{Status: st, Number: 1, FinishedAt: new(time.Now())}
	}
	return j
}

func names(gs []*group) []string {
	var out []string
	for _, g := range gs {
		for _, j := range g.Jobs {
			out = append(out, g.Name+"/"+j.Name)
		}
	}
	return out
}

func TestTheStatusChoiceKeepsTheJobsInThatStatusAndDropsEmptyGroups(t *testing.T) {
	run := summary("run", "ci", store.Success)
	run.Running = true
	off := summary("off", "ops", store.Failed)
	off.Disabled = true
	parent := api.JobSummary{Name: "site", Group: "ci", Children: 2, Worst: store.Timeout}
	all := groups([]api.JobSummary{
		summary("red", "ci", store.Failed), summary("lost", "ci", store.Lost), summary("shaky", "ci", store.Unstable),
		summary("fine", "ci", store.Success), summary("new", "ci", ""), run, off, parent,
	})
	for status, want := range map[string][]string{
		"failing":  {"ci/lost", "ci/red", "ci/site", "ops/off"},
		"unstable": {"ci/shaky"},
		"running":  {"ci/run"},
		"disabled": {"ops/off"},
		"":         names(all),
		"nonsense": names(all),
	} {
		got := names(shown(all, url.Values{"status": {status}}).Groups)
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("status %q: %v, want %v", status, got, want)
		}
	}
	d := shown(all, url.Values{"status": {"disabled"}})
	if len(d.Groups) != 1 || d.Groups[0].Name != "ops" {
		t.Fatalf("a group left empty is not shown: %v", names(d.Groups))
	}
}

func TestTheStatusChoiceCombinesWithTheGroupAndLeadsToIt(t *testing.T) {
	all := groups([]api.JobSummary{summary("a", "ci", store.Failed), summary("b", "ci", store.Success), summary("c", "ops", store.Failed)})
	d := shown(all, url.Values{"group": {"ci"}, "status": {"failing"}})
	if got := names(d.Groups); len(got) != 1 || got[0] != "ci/a" {
		t.Fatalf("%v", got)
	}
	hrefs := map[string]string{}
	for _, s := range d.Statuses {
		hrefs[s.Label] = s.Href
		if s.Current != (s.Label == "Failing") {
			t.Errorf("%s current: %v", s.Label, s.Current)
		}
	}
	if hrefs["All"] != "/jobs?group=ci" || hrefs["Running"] != "/jobs?group=ci&status=running" || d.Refresh != "/jobs?group=ci&status=failing" {
		t.Fatalf("%v, refresh %s", hrefs, d.Refresh)
	}
	if all := shown(all, url.Values{}); all.Refresh != "/jobs" || all.Statuses[1].Href != "/jobs?status=failing" {
		t.Fatalf("with no parameter the page asks for itself as it was: %+v", all)
	}
}

var currentTab = regexp.MustCompile(`<a href="([^"]*)" aria-current="true">([^<]*)</a>`)

func TestTheJobsPageServesTheStatusChoice(t *testing.T) {
	e := site(t)
	e.finished(t, "smoke", "fss")
	e.finished(t, "quoted", "sf")
	c := e.login(t, "admin")

	code, page := get(t, c, e.srv.URL+"/jobs")
	eq(t, "all", code, http.StatusOK)
	contains(t, page, `aria-label="Show jobs by status"`, `href="/jobs?status=failing"`, `data-name="smoke"`, `data-name="quoted"`, `<script src="/static/jobs.js?v=`, `hx-get="/jobs"`, `data-name="smoke"`)
	eq(t, "current", currentTab.FindStringSubmatch(page)[2], "All")

	_, page = get(t, c, e.srv.URL+"/jobs?status=failing")
	contains(t, page, `data-name="smoke"`, `hx-get="/jobs?status=failing"`)
	lacks(t, page, `data-name="quoted"`, `data-name="hooked"`)
	eq(t, "current", currentTab.FindStringSubmatch(page)[2], "Failing")

	_, page = get(t, c, e.srv.URL+"/jobs?group=backend&status=failing")
	contains(t, page, `data-name="smoke"`, `hx-get="/jobs?group=backend&amp;status=failing"`)

	_, page = get(t, c, e.srv.URL+"/jobs?status=running")
	contains(t, page, "No job matches this status.", `aria-label="Show jobs by status"`)
	lacks(t, page, `data-name="smoke"`)

	_, page = get(t, c, e.srv.URL+"/jobs?status=bogus")
	contains(t, page, `data-name="smoke"`, `data-name="quoted"`, `hx-get="/jobs"`)
	eq(t, "current", currentTab.FindStringSubmatch(page)[2], "All")
}
