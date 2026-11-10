package web

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/overview"
	"github.com/ndthuan/kipp/internal/store"
)

// group is one heading of the jobs page. Removed is the one that holds the
// jobs whose files are gone, whatever group they were in.
type group struct {
	Name    string
	Removed bool
	Jobs    []api.JobSummary
}

type jobsData struct {
	Queue  queueData
	Groups []*group
	// Self is the address of the group the view is in, Refresh the address the
	// page asks for again, which also keeps its status.
	Self, Refresh string
	// Status is the status choice in force, "" for All, and Statuses the
	// choices, each leading to the same group with its own status.
	Status   string
	Statuses []statusTab
	// Heading names the view for a reader that goes by headings.
	Heading string
}

// href is where a group's tab leads. A group's name is a job file's to
// choose, so the jobs with none and the removed ones are asked for by
// another parameter and cannot meet a group of the same name.
func (g *group) href() string {
	switch {
	case g.Removed:
		return "/jobs?only=removed"
	case g.Name == "":
		return "/jobs?only=ungrouped"
	}
	return "/jobs?group=" + url.QueryEscape(g.Name)
}

// crumb is the group a job's page and its builds' pages lead back to: the
// tab of the job list that holds the job.
func crumb(name string, removed bool) *group {
	return &group{Name: name, Removed: removed}
}

// Href is href for a template.
func (g *group) Href() string { return g.href() }

// Label is label for a template.
func (g *group) Label() string { return g.label() }

func (g *group) label() string {
	switch {
	case g.Removed:
		return "Removed"
	case g.Name == "":
		return "No group"
	}
	return g.Name
}

// picked is the group the query asks for, or nil for all of them. All the
// removed jobs share one tab, whatever group each was in.
func picked(all []*group, q url.Values) []*group {
	var want func(*group) bool
	switch only, name := q.Get("only"), q.Get("group"); {
	case only == "removed":
		want = func(g *group) bool { return g.Removed }
	case only == "ungrouped":
		want = func(g *group) bool { return !g.Removed && g.Name == "" }
	case name != "":
		want = func(g *group) bool { return !g.Removed && g.Name == name }
	default:
		return nil
	}
	var out []*group
	for _, g := range all {
		if want(g) {
			out = append(out, g)
		}
	}
	return out
}

// statusTab is one choice of the status control.
type statusTab struct {
	Label, Value, Href string
	Current            bool
}

// statusChoices are the values of ?status=, in the order the control shows
// them; the first, All, is the one an unknown value means.
var statusChoices = []statusTab{{Label: "All"}, {Label: "Failing", Value: "failing"}, {Label: "Unstable", Value: "unstable"}, {Label: "Running", Value: "running"}, {Label: "Disabled", Value: "disabled"}}

// statusOf is the status the query asks for, "" for All.
func statusOf(q url.Values) string {
	v := q.Get("status")
	if slices.ContainsFunc(statusChoices, func(c statusTab) bool { return c.Value == v }) {
		return v
	}
	return ""
}

// hasStatus reports whether the job is in the status. Failing is the status
// of the job's row being red, as for the row's colour.
func hasStatus(j api.JobSummary, status string) bool {
	switch status {
	case "failing":
		return j.Status().Red()
	case "unstable":
		return j.Status() == store.Unstable
	case "running":
		return j.Running
	case "disabled":
		return j.Disabled
	}
	return true
}

// withStatus is the groups with only the jobs in the status, and none left
// empty.
func withStatus(all []*group, status string) []*group {
	if status == "" {
		return all
	}
	var out []*group
	for _, g := range all {
		jobs := slices.DeleteFunc(slices.Clone(g.Jobs), func(j api.JobSummary) bool { return !hasStatus(j, status) })
		if len(jobs) > 0 {
			out = append(out, &group{Name: g.Name, Removed: g.Removed, Jobs: jobs})
		}
	}
	return out
}

// shown is the page for a query: every group, or the one it picks, and in
// them the jobs of the status it asks for.
func shown(all []*group, q url.Values) jobsData {
	d := jobsData{Groups: all, Self: "/jobs", Heading: "Jobs", Status: statusOf(q)}
	if one := picked(all, q); len(one) > 0 {
		d.Groups, d.Self, d.Heading = one, one[0].href(), one[0].label()
	}
	d.Groups = withStatus(d.Groups, d.Status)
	d.Refresh = d.Self
	sep := "?"
	if strings.Contains(d.Self, "?") {
		sep = "&"
	}
	for _, c := range statusChoices {
		c.Current = c.Value == d.Status
		c.Href = d.Self
		if c.Value != "" {
			c.Href += sep + "status=" + c.Value
		}
		if c.Current {
			d.Refresh = c.Href
		}
		d.Statuses = append(d.Statuses, c)
	}
	return d
}

// menu is one group in the header: its jobs under its name.
type menu struct {
	Label, Href string
	// Here: the page is in this group, as its list or as one of its jobs;
	// Page: the page is the group's list.
	Here, Page bool
	Jobs       []api.JobSummary
}

// navData is the header's menus. Home: the page is the overview. All: the
// page is the list of every job. Job is the job the page is about, marked in
// its menu.
type navData struct {
	Home, All bool
	Job       string
	Menus     []menu
}

// navOf is the header for a page in the group at here ("/jobs" for all of
// them, "/" for the overview, "" for none), about job.
func navOf(all []*group, here, job string) *navData {
	n := &navData{Home: here == "/", All: here == "/jobs", Job: job}
	seen := map[string]bool{}
	for _, g := range all {
		h := g.href()
		if seen[h] {
			continue
		}
		seen[h] = true
		n.Menus = append(n.Menus, menu{Label: g.label(), Href: h, Here: h == here, Page: h == here && job == "", Jobs: g.Jobs})
	}
	return n
}

// home is the overview. The job list used to be here, so a link to it with
// a group or an only leads on to the list.
func (s *Site) home(w http.ResponseWriter, r *http.Request, u *store.User) {
	if q := r.URL.Query(); q.Has("group") || q.Has("only") {
		redirect(w, r, "/jobs?"+r.URL.RawQuery, http.StatusSeeOther)
		return
	}
	// A window nobody knows is the day, not an error page: the address is a
	// link someone typed.
	rg, _ := overview.ParseRange(r.URL.Query().Get("window"))
	o, err := s.api.Service().Overview(r.Context(), u, rg)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	q, err := s.api.Service().Queue(r.Context(), u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	why, err := s.whys(r.Context(), o)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d := overviewData{Why: why, Queue: queueData{Queue: q, CSRF: csrf(r), Next: thisPage(r), Full: true, Nested: true}, Overview: o, Range: rg,
		Charts: chartsOf(o.Charts, s.api.Zone()), Self: windowHref(overview.Day, rg)}
	for _, c := range []overview.Range{overview.Day, overview.Week} {
		d.Ranges = append(d.Ranges, rangeLink{Label: c.Label, Href: windowHref(overview.Day, c), Current: c == rg})
	}
	s.render(w, r, "overview", http.StatusOK, page{Title: "Overview", Data: d, Here: "/", Queue: q})
}

// overviewData is the overview page: the queue, the graphs and the sections. Self is
// the page's own address, which its refresh asks for again, so that a week
// stays a week.
type overviewData struct {
	Queue    queueData
	Overview *overview.Overview
	Range    overview.Range
	Ranges   []rangeLink
	Charts   []*chart
	Self     string
	// Why says, by build, where each failing job's newest build went wrong.
	Why map[int64]*why
}

// rangeLink is one choice of the control that sets how far back the graphs
// look; Current is the one the page shows.
type rangeLink struct {
	Label, Href string
	Current     bool
}

// windowHref is the address of the overview for rg, "/" for the day that
// is the default.
func windowHref(def, rg overview.Range) string {
	if rg == def {
		return "/"
	}
	return "/?window=" + rg.Name
}

func (s *Site) jobs(w http.ResponseWriter, r *http.Request, u *store.User) {
	jobs, err := s.api.Service().Jobs(r.Context(), u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	q, err := s.api.Service().Queue(r.Context(), u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	all := groups(jobs)
	d := shown(all, r.URL.Query())
	d.Queue = queueData{Queue: q, CSRF: csrf(r), Next: thisPage(r)}
	s.render(w, r, "jobs", http.StatusOK, page{Title: d.Heading, Data: d, Here: d.Self, groups: all, Queue: q})
}

// queueData is the queue as a page shows it. Full is the queue's own page,
// which says so when the queue is empty; elsewhere an empty one is not shown.
type queueData struct {
	Queue *api.QueueView
	CSRF  string
	// Next is the page the reader is on, where a cancel leads back to.
	Next string
	Full bool
	// Nested: the queue is a part of a page that refreshes itself whole, so
	// it does not ask for itself again.
	Nested bool
}

func (s *Site) queue(w http.ResponseWriter, r *http.Request, u *store.User) {
	q, err := s.api.Service().Queue(r.Context(), u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, "queue", http.StatusOK, page{Title: "Queue", Data: queueData{Queue: q, CSRF: csrf(r), Next: thisPage(r), Full: true}, Queue: q})
}

// cancelForm is what a cancel button's form needs.
type cancelForm struct {
	Build *store.Build
	Next  string
}

func cancelOf(b *store.Build, next string) cancelForm { return cancelForm{b, next} }

// thisPage is the page the reader is on: the request's own address, or for
// a part htmx refreshes, the address of the page that asked for it.
func thisPage(r *http.Request) string {
	if from := r.Header.Get("HX-Current-URL"); from != "" {
		if u, err := url.Parse(from); err == nil {
			return localPath(u.RequestURI())
		}
	}
	return localPath(r.URL.RequestURI())
}

// csrf is the session's CSRF token, for a form a page holds.
func csrf(r *http.Request) string {
	if sess := api.SessionFrom(r.Context()); sess != nil {
		return sess.CSRF
	}
	return ""
}

// moment is what the "since" template shows: how long ago a time was,
// kept up to date by queue.js from the time in its datetime.
type moment struct{ At, Ago string }

// since is the moment of v, a time.Time or a *time.Time; there is none of
// a nil one or of anything else.
func since(v any) *moment {
	var t time.Time
	switch at := v.(type) {
	case time.Time:
		t = at
	case *time.Time:
		if at == nil {
			return nil
		}
		t = *at
	default:
		return nil
	}
	return &moment{At: t.UTC().Format(time.RFC3339), Ago: short(time.Since(t))}
}

// join is a list of names as a page shows it.
func join(names []string) string { return strings.Join(names, ", ") }

// groups sorts the jobs into their groups by name, the jobs with no group
// after them and the removed jobs last.
func groups(jobs []api.JobSummary) []*group {
	var out []*group
	type key struct {
		name    string
		removed bool
	}
	byKey := map[key]*group{}
	for _, j := range jobs {
		k := key{j.Group, j.Removed}
		g := byKey[k]
		if g == nil {
			g = &group{Name: j.Group, Removed: j.Removed}
			byKey[k] = g
			out = append(out, g)
		}
		g.Jobs = append(g.Jobs, j)
	}
	slices.SortFunc(out, func(a, b *group) int {
		return cmp.Or(cmp.Compare(btoi(a.Removed), btoi(b.Removed)), cmp.Compare(btoi(a.Name == ""), btoi(b.Name == "")), cmp.Compare(a.Name, b.Name))
	})
	return out
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

type jobData struct {
	View     *api.JobView
	CanBuild bool
	Tests    testsView
	// Store is the page of the store the job's builds report into, for a
	// job its files still hold: whoever may view the job may view it.
	Store, StoreName string
	// A multibranch job's children, apart: the ones it builds, the gone
	// ones, and the pull requests from forks it does not build.
	Live, Gone, Forks []api.ChildView
}

// split sorts a multibranch job's children into the page's three lists.
func (d *jobData) split(kids []api.ChildView) {
	for _, c := range kids {
		switch {
		case c.FromFork:
			d.Forks = append(d.Forks, c)
		case c.State == config.ChildGone:
			d.Gone = append(d.Gone, c)
		default:
			d.Live = append(d.Live, c)
		}
	}
}

// testsView is the job's tests across its last builds. A job may have
// thousands, nearly all of them passing every time, so the page lists the
// ones that failed in any of those builds and counts the rest, until it is
// asked for all of them.
type testsView struct {
	Rows          []historyRow
	Total, Failed int
	Builds        string // "build", or "3 builds"
	All           bool
}

func testsOf(rows []historyRow, all bool) testsView {
	v := testsView{Total: len(rows), All: all, Builds: "build"}
	if len(rows) > 0 && len(rows[0].Dots) > 1 {
		v.Builds = strconv.Itoa(len(rows[0].Dots)) + " builds"
	}
	var failed []historyRow
	for _, r := range rows {
		if slices.ContainsFunc(r.Dots, func(d dot) bool { return d.Status == "failed" }) {
			failed = append(failed, r)
		}
	}
	v.Failed, v.Rows = len(failed), failed
	if all {
		v.Rows = rows
	}
	return v
}

func (s *Site) job(w http.ResponseWriter, r *http.Request, u *store.User) {
	before, err := api.Before(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := s.api.Service().Job(r.Context(), u, r.PathValue("name"), before)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	hist, err := s.api.Service().TestHistory(r.Context(), u, v.Name, 20)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	canBuild := false
	if !v.Removed && !v.Job.Disabled {
		if canBuild, err = s.auth.Can(r.Context(), u, v.Name, "build"); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	here := crumb("", true).href()
	if v.Job != nil {
		here = crumb(v.Job.Group, false).href()
	}
	data := jobData{View: v, CanBuild: canBuild, Tests: testsOf(historyRows(hist), r.URL.Query().Get("results") == "all")}
	if v.Job != nil {
		data.StoreName = v.Job.StoreName()
		data.Store = metricsPath(data.StoreName, "")
	}
	data.split(v.Children)
	s.render(w, r, "job", http.StatusOK, page{Title: v.Name, Here: here, Job: v.Name, Data: data})
}

// historyRow is one test across the job's last builds.
type historyRow struct {
	Suite, Name string
	Dots        []dot // one per build, newest first
}

// dot is a test's outcome in one build; no Status when it did not run there.
type dot struct {
	Build  int
	Status string
}

// historyRows lines every test up against the same builds. A test that ran
// more than once in a build shows as failed if any run failed.
func historyRows(rows []store.TestRow) []historyRow {
	var builds []int
	column := map[int]int{}
	for _, r := range rows {
		if _, ok := column[r.BuildNumber]; !ok {
			column[r.BuildNumber] = len(builds)
			builds = append(builds, r.BuildNumber)
		}
	}
	var out []historyRow
	index := map[[2]string]int{}
	for _, r := range rows {
		key := [2]string{r.Suite, r.Name}
		i, ok := index[key]
		if !ok {
			i = len(out)
			index[key] = i
			out = append(out, historyRow{Suite: r.Suite, Name: r.Name, Dots: make([]dot, len(builds))})
			for c, n := range builds {
				out[i].Dots[c].Build = n
			}
		}
		if d := &out[i].Dots[column[r.BuildNumber]]; d.Status != "failed" {
			d.Status = r.Status
		}
	}
	return out
}

// build starts a build from the params form. A checkbox sends its value
// after the hidden "false" before it, so the last value of a name wins. A
// browser sends each line break of a textarea as CR LF; the build gets the
// line feed that was typed.
func (s *Site) build(w http.ResponseWriter, r *http.Request, u *store.User) {
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, fmt.Errorf("%w: %w", api.ErrBadInput, err))
		return
	}
	params := map[string]string{}
	for k, v := range r.PostForm {
		if k != config.CSRFField {
			params[k] = strings.ReplaceAll(v[len(v)-1], "\r\n", "\n")
		}
	}
	b, err := s.api.Service().Trigger(r.Context(), u, r.PathValue("name"), params, "web")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	redirect(w, r, buildPath(b), http.StatusSeeOther)
}

// scan has a multibranch job's repo read now, and goes back to its page,
// which says how the scan went.
func (s *Site) scan(w http.ResponseWriter, r *http.Request, u *store.User) {
	name := r.PathValue("name")
	if err := s.api.Service().ScanNow(r.Context(), u, name); err != nil {
		s.fail(w, r, err)
		return
	}
	redirect(w, r, "/jobs/"+url.PathEscape(name), http.StatusSeeOther)
}

// buildPath is the address of a build's page: under its job, by its number
// there, which is what a reader knows it by. The id stays the API's.
func buildPath(b *store.Build) string {
	return "/jobs/" + url.PathEscape(b.Job) + "/" + strconv.Itoa(b.Number)
}

// numbered is the build the request's path names by job and number; a
// number that is not one names no build, which is ErrNotFound.
func (s *Site) numbered(r *http.Request, u *store.User) (*store.Build, error) {
	n, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		return nil, api.ErrNotFound
	}
	return s.api.Service().BuildNumbered(r.Context(), u, r.PathValue("name"), n)
}

// buildByID answers the address a build's page had while it went by id:
// links to it were handed out, in messages and in terminals, and still lead
// to the page.
func (s *Site) buildByID(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, err := api.BuildID(r)
	var v *api.BuildView
	if err == nil {
		v, err = s.api.Service().Build(r.Context(), u, id)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	to := buildPath(v.Build)
	if r.URL.RawQuery != "" {
		to += "?" + r.URL.RawQuery
	}
	redirect(w, r, to, http.StatusMovedPermanently)
}

type buildData struct {
	View   *api.BuildView
	Live   bool   // the page still refreshes itself: more may be written
	Waited string // how long it was queued, when that was a second or more
	// Exit is the exit code of a build of one stage that did not end with
	// 0: such a build's page has no table of its stages to say it.
	Exit      string
	CanCancel bool
	// CanAgain is set on a build that has ended, for who may build its job
	// and while the job can be built.
	CanAgain bool
	// CanKeep is set on a build that has ended, for who may build its job,
	// kept or not, whether or not the job's file is still there.
	CanKeep bool
	Logs    []logView
	// Notice says at the top why a red build is red; nil for any other.
	Notice *notice
	// Tests are the ones the page lists, the failed first: those that
	// failed, or with AllTests every one. A build may have thousands, nearly
	// all passed, so the rest are counted and loaded when asked for.
	Tests    []store.Test
	Total    int
	AllTests bool
	Counts   map[string]int
	// Guards are what the guards of its stages found, each as the stage's
	// log says it.
	Guards []guardView
	// Artifacts are the files its stages kept, stage by stage.
	Artifacts []artifactsView
}

// artifactsView is one stage's kept files on the build's page.
type artifactsView struct {
	Stage, Zip string
	Files      []fileView
}

// fileView is one kept file, with where it downloads.
type fileView struct {
	Path, Size, Href string
}

// artifactViews are the rows of the build's Artifacts section.
func artifactViews(id int64, stages []api.StageFiles) []artifactsView {
	out := make([]artifactsView, len(stages))
	for i, st := range stages {
		v := artifactsView{Stage: st.Stage, Zip: api.ArtifactsZipURL(id, st.Stage), Files: make([]fileView, len(st.Files))}
		for j, f := range st.Files {
			v.Files[j] = fileView{Path: f.Path, Size: byteSize(f.Size), Href: api.ArtifactURL(id, st.Stage, f.Path)}
		}
		out[i] = v
	}
	return out
}

// guardView is one result of a guard on the build's page.
type guardView struct {
	Stage, Line, Outcome string
}

// guardViews are the rows of the build's guards, each with the line the
// stage's log has.
func guardViews(rows []store.GuardRow) []guardView {
	out := make([]guardView, len(rows))
	for i, r := range rows {
		out[i] = guardView{Stage: r.Stage, Line: api.GuardLine(r), Outcome: r.Outcome}
	}
	return out
}

// logView is one console on the build's page.
type logView struct {
	Name string
	Open bool
	// Status is the stage's, drawn beside its name; empty for the logs
	// kipp writes itself.
	Status store.StageStatus
}

func (s *Site) buildPage(w http.ResponseWriter, r *http.Request, u *store.User) {
	data, err := s.buildData(r, u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	b := data.View.Build
	here := crumb(data.View.Group, data.View.Removed).href()
	s.render(w, r, "build", http.StatusOK, page{Title: b.Job + " #" + strconv.Itoa(b.Number), Here: here, Job: b.Job, Data: data})
}

func (s *Site) buildData(r *http.Request, u *store.User) (*buildData, error) {
	ctx := r.Context()
	nb, err := s.numbered(r, u)
	if err != nil {
		return nil, err
	}
	id := nb.ID
	v, err := s.api.Service().Build(ctx, u, id)
	if err != nil {
		return nil, err
	}
	tests, err := s.api.Service().Tests(ctx, u, id)
	if err != nil {
		return nil, err
	}
	guards, err := s.api.Service().GuardResults(ctx, u, id)
	if err != nil {
		return nil, err
	}
	_, kept, err := s.api.Service().Artifacts(ctx, u, id)
	if err != nil {
		return nil, err
	}
	b := v.Build
	ended := b.Status.Terminal()
	if len(v.Stages) == 0 && !ended {
		v.Stages = planned(v.Needs)
	}
	data := &buildData{View: v, Tests: tests, Counts: map[string]int{}, Logs: s.logs(ctx, u, v), Guards: guardViews(guards), Artifacts: artifactViews(id, kept)}
	data.Notice = redNotice(v, data.Logs)
	data.Live = !v.Complete
	data.Waited, data.Exit = headline(v)
	if !ended {
		if data.CanCancel, err = s.auth.Can(ctx, u, b.Job, "cancel"); err != nil {
			return nil, err
		}
	} else {
		canBuild, err := s.auth.Can(ctx, u, b.Job, "build")
		if err != nil {
			return nil, err
		}
		data.CanKeep, data.CanAgain = canBuild, canBuild && !v.Removed
	}
	for _, t := range tests {
		data.Counts[t.Status]++
	}
	slices.SortStableFunc(data.Tests, func(a, b store.Test) int { return cmp.Compare(testRank(a.Status), testRank(b.Status)) })
	data.Total, data.AllTests = len(tests), r.URL.Query().Get("results") == "all"
	if !data.AllTests {
		// Sorted, so the failed are the head of the list.
		data.Tests = data.Tests[:data.Counts["failed"]]
	}
	return data, nil
}

// headline is what the line under a build's name says beside its times:
// how long it was queued, from a second up, and the exit code of a build
// of one stage that did not end with 0.
func headline(v *api.BuildView) (waited, exit string) {
	if b := v.Build; b.StartedAt != nil {
		if wait := b.StartedAt.Sub(b.QueuedAt); wait >= time.Second {
			waited = short(wait)
		}
	}
	if len(v.Stages) == 1 {
		if code := v.Stages[0].ExitCode; code != nil && *code != 0 && exitKnown(v.Stages[0], v.Build) {
			exit = strconv.Itoa(*code)
		}
	}
	return waited, exit
}

// exitKnown reports whether a stage's exit code says something: a stage the
// kipp killed (cancelled, or in a build that timed out or was lost) has
// the code of the signal, -1, and no exit of its own.
func exitKnown(st *store.Stage, b *store.Build) bool {
	if st.Status == store.StageCancelled {
		return false
	}
	return st.ExitCode == nil || *st.ExitCode >= 0 || (b.Status != store.Timeout && b.Status != store.Lost)
}

// exitText is the exit code a stage's row shows, empty where it says nothing.
func exitText(st *store.Stage, b *store.Build) string {
	if st.ExitCode == nil || !exitKnown(st, b) {
		return ""
	}
	return strconv.Itoa(*st.ExitCode)
}

// planned is the job file's stages as a build that has not recorded its
// own yet will most likely run them, so that its page has them from the start.
func planned(needs map[string][]string) []*store.Stage {
	var out []*store.Stage
	for _, name := range slices.Sorted(maps.Keys(needs)) {
		out = append(out, &store.Stage{Name: name, Status: store.StagePending})
	}
	return out
}

// testRank sorts the failed first; a status not known here is one of them.
func testRank(status string) int {
	return max(slices.Index([]string{"failed", "skipped", "passed"}, status), 0)
}

// logs lists the build's consoles: its stages, and around them the logs
// the dispatcher writes itself, where there is one. A stage that is
// running, went wrong or ended unstable starts open, and when a build that
// is over has none, its last stage does; the browser streams only open logs.
func (s *Site) logs(ctx context.Context, u *store.User, v *api.BuildView) []logView {
	var out []logView
	own := func(name string, open bool) {
		if p, _, err := s.api.Service().LogPath(ctx, u, v.Build.ID, name); err == nil && exists(p) {
			out = append(out, logView{Name: name, Open: open})
		}
	}
	own(config.CloneLog, false)
	own(config.PrepareLog, true)
	first, anyOpen := len(out), false
	for _, st := range v.Stages {
		open := worthReading(st.Status)
		anyOpen = anyOpen || open
		out = append(out, logView{Name: st.Name, Open: open, Status: st.Status})
	}
	// When none went wrong, how the build ended is what a reader came for.
	if last := len(out) - 1; !anyOpen && last >= first && v.Build.Status.Terminal() {
		out[last].Open = true
	}
	own(config.HooksLog, false)
	return out
}

// worthReading reports whether a stage in the status has a log a reader
// came for: one still being written, or one that says why the build is
// not a plain success.
func worthReading(status store.StageStatus) bool {
	switch status {
	case store.StageRunning, store.StageFailed, store.StageCancelled, store.StageUnstable:
		return true
	}
	return false
}

func exists(file string) bool {
	root, err := os.OpenRoot(filepath.Dir(file))
	if err != nil {
		return false
	}
	defer func() { _ = root.Close() }()
	_, err = root.Stat(filepath.Base(file))
	return err == nil
}

// confirmCancel asks before a build is killed: a cancel cannot be undone.
func (s *Site) confirmCancel(w http.ResponseWriter, r *http.Request, u *store.User) {
	b, err := s.numbered(r, u)
	if err == nil {
		var ok bool
		if ok, err = s.auth.Can(r.Context(), u, b.Job, "cancel"); err == nil && !ok {
			err = api.ErrForbidden
		}
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if b.Status.Terminal() {
		redirect(w, r, buildPath(b), http.StatusSeeOther)
		return
	}
	back := nextOr(r.URL.Query().Get("next"), buildPath(b))
	line := fmt.Sprintf("%s #%d has run for %s.", b.Job, b.Number, took(b.StartedAt, nil))
	if b.StartedAt == nil {
		line = fmt.Sprintf("%s #%d has been queued for %s.", b.Job, b.Number, short(time.Since(b.QueuedAt)))
	}
	s.confirm(w, r, "", confirmData{
		Heading:   fmt.Sprintf("Cancel %s #%d?", b.Job, b.Number),
		Lines:     []string{line, "Its processes are stopped and it cannot be resumed."},
		Button:    "Cancel the build",
		Back:      back,
		BackLabel: "Keep running",
		Hidden:    []hiddenField{{"next", back}},
	})
}

func (s *Site) cancel(w http.ResponseWriter, r *http.Request, u *store.User) {
	b, err := s.numbered(r, u)
	if err == nil {
		err = s.api.Service().Cancel(r.Context(), u, b.ID)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	redirect(w, r, nextOr(r.PostFormValue("next"), buildPath(b)), http.StatusSeeOther)
}

// again queues a build like a finished one and leads to it.
func (s *Site) again(w http.ResponseWriter, r *http.Request, u *store.User) {
	b, err := s.numbered(r, u)
	if err == nil {
		b, err = s.api.Service().Again(r.Context(), u, b.ID, "web")
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	redirect(w, r, buildPath(b), http.StatusSeeOther)
}

// keep keeps a finished build out of retention, or with stop puts it back
// under its job's rules, and leads back to it.
func (s *Site) keep(stop bool) func(http.ResponseWriter, *http.Request, *store.User) {
	return func(w http.ResponseWriter, r *http.Request, u *store.User) {
		b, err := s.numbered(r, u)
		if err == nil {
			err = s.api.Service().Keep(r.Context(), u, b.ID, !stop)
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		redirect(w, r, buildPath(b), http.StatusSeeOther)
	}
}

// clockIn shows a time in zone, the server's; a time that is not set is
// empty.
func clockIn(zone *time.Location) func(v any) string {
	return func(v any) string {
		if t, ok := v.(*time.Time); ok && t != nil {
			v = *t
		}
		if t, ok := v.(time.Time); ok {
			return t.In(zone).Format("2006-01-02 15:04:05")
		}
		return ""
	}
}

// zoned shows a time in its own zone, and names the zone: a job's next
// build, which is in the zone its cron is read in.
func zoned(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format("2006-01-02 15:04:05 MST")
}

// ago is how long ago t was, in its largest unit.
func ago(t *time.Time) string {
	if t == nil {
		return ""
	}
	return short(time.Since(*t)) + " ago"
}

// took is how long something ran, up to now if it has not ended.
func took(start, end *time.Time) string {
	switch {
	case start == nil:
		return ""
	case end == nil:
		return short(time.Since(*start))
	}
	return short(end.Sub(*start))
}

func short(d time.Duration) string {
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(max(d, 0)/time.Second)) + "s"
	case d < time.Hour:
		return fmt.Sprintf("%dm %ds", d/time.Minute, d%time.Minute/time.Second)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", d/time.Hour, d%time.Hour/time.Minute)
	}
	return fmt.Sprintf("%dd %dh", d/(24*time.Hour), d%(24*time.Hour)/time.Hour)
}

// lines reports whether a param's value is more than one line, which a
// page shows as it was written.
func lines(value string) bool { return strings.Contains(value, "\n") }

func millis(ms int) string {
	return (time.Duration(ms) * time.Millisecond).String()
}

// percent is the share of the window a flaky job failed, as the page says it.
func percent(f overview.FlakyJob) string { return strconv.Itoa(f.Percent()) + "%" }
