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
	"github.com/ndthuan/kipp/internal/jobstore"
	"github.com/ndthuan/kipp/internal/overview"
	"github.com/ndthuan/kipp/internal/store"
)

type group struct {
	Name string
	Jobs []api.JobSummary
}

type jobsData struct {
	Queue         queueData
	Groups        []*group
	Self, Refresh string
	Status        string
	Statuses      []statusTab
	Heading       string
	Edits         bool
}

func (g *group) href() string {
	if g.Name == "" {
		return "/jobs?only=ungrouped"
	}
	return "/jobs?group=" + url.QueryEscape(g.Name)
}

type crumbTo struct{ Href, Label string }

func crumb(name string, removed, admin bool) crumbTo {
	switch {
	case removed && admin:
		return crumbTo{deletedPath, "Deleted jobs"}
	case removed:
		return crumbTo{"/jobs", "Jobs"}
	}
	g := &group{Name: name}
	return crumbTo{g.href(), g.label()}
}

func (g *group) label() string {
	if g.Name == "" {
		return "No group"
	}
	return g.Name
}

func picked(all []*group, q url.Values) []*group {
	var want func(*group) bool
	switch only, name := q.Get("only"), q.Get("group"); {
	case only == "ungrouped":
		want = func(g *group) bool { return g.Name == "" }
	case name != "":
		want = func(g *group) bool { return g.Name == name }
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

type statusTab struct {
	Label, Value, Href string
	Current            bool
}

var statusChoices = []statusTab{{Label: "All"}, {Label: "Failing", Value: "failing"}, {Label: "Unstable", Value: "unstable"}, {Label: "Running", Value: "running"}, {Label: "Disabled", Value: "disabled"}}

func statusOf(q url.Values) string {
	v := q.Get("status")
	if slices.ContainsFunc(statusChoices, func(c statusTab) bool { return c.Value == v }) {
		return v
	}
	return ""
}

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

func withStatus(all []*group, status string) []*group {
	if status == "" {
		return all
	}
	var out []*group
	for _, g := range all {
		jobs := slices.DeleteFunc(slices.Clone(g.Jobs), func(j api.JobSummary) bool { return !hasStatus(j, status) })
		if len(jobs) > 0 {
			out = append(out, &group{Name: g.Name, Jobs: jobs})
		}
	}
	return out
}

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

type menu struct {
	Label, Href string
	Here, Page  bool
	Jobs        []api.JobSummary
}

type navData struct {
	Home, All bool
	Job       string
	Menus     []menu
}

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

func (s *Site) home(w http.ResponseWriter, r *http.Request, u *store.User) {
	query := r.URL.Query()
	if query.Has("group") || query.Has("only") {
		redirect(w, r, "/jobs?"+r.URL.RawQuery, http.StatusSeeOther)
		return
	}
	rg, _ := overview.ParseRange(query.Get("window"))
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
	tab := openTab(r)
	d := overviewData{Tab: tab, Overview: o, Range: rg, Ranges: rangeLinks(rg), Charts: chartsOf(o.Charts, s.api.Zone())}
	d.Tabs = tabsOf(o, q, tab)
	if err := s.loadPanel(r, u, &d, q); err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, "overview", http.StatusOK, page{Title: "Overview", Data: d, Here: "/", Queue: q})
}

func (s *Site) overviewTabPanel(w http.ResponseWriter, r *http.Request, u *store.User) {
	tab := r.PathValue("name")
	if !validTab(tab) {
		http.NotFound(w, r)
		return
	}
	http.SetCookie(w, tabCookie(tab))
	d := overviewData{Tab: tab}
	var q *api.QueueView
	var err error
	if tab == "now" {
		q, err = s.api.Service().Queue(r.Context(), u)
	} else {
		d.Overview, err = s.api.Service().Overview(r.Context(), u, overview.Day)
	}
	if err == nil {
		err = s.loadPanel(r, u, &d, q)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.fragment(w, "overview", "overview-panel", d)
}

func (s *Site) overviewCharts(w http.ResponseWriter, r *http.Request, u *store.User) {
	rg, _ := overview.ParseRange(r.URL.Query().Get("window"))
	o, err := s.api.Service().Overview(r.Context(), u, rg)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d := overviewData{Range: rg, Ranges: rangeLinks(rg), Charts: chartsOf(o.Charts, s.api.Zone())}
	s.fragment(w, "overview", "overview-charts", d)
}

func (s *Site) loadPanel(r *http.Request, u *store.User, d *overviewData, q *api.QueueView) error {
	switch d.Tab {
	case "now":
		ended, err := s.api.Service().NewestEnded(r.Context(), u)
		if err != nil {
			return err
		}
		d.Queue = queueData{Queue: q, Ended: ended, CSRF: csrf(r), Next: thisPage(r), Full: true, Nested: true}
	case "failing":
		why, err := s.whys(r.Context(), d.Overview)
		if err != nil {
			return err
		}
		d.Why = why
	}
	return nil
}

var overviewTabs = []struct{ Name, Label string }{{"now", "Now"}, {"failing", "Failing"}, {"unstable", "Unstable"}, {"flaky", "Flaky"}}

func validTab(name string) bool {
	return slices.ContainsFunc(overviewTabs, func(t struct{ Name, Label string }) bool { return t.Name == name })
}

const overviewTabCookie = "kipp_overview_tab"

func tabCookie(tab string) *http.Cookie {
	return &http.Cookie{Name: overviewTabCookie, Value: tab, Path: "/", MaxAge: overviewTabKept, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}
}

const overviewTabKept = 400 * 24 * 60 * 60

func openTab(r *http.Request) string {
	if tab := r.URL.Query().Get("tab"); validTab(tab) {
		return tab
	}
	if c, err := r.Cookie(overviewTabCookie); err == nil && validTab(c.Value) {
		return c.Value
	}
	return "now"
}

type overviewTab struct {
	Name, Label, Tone string
	Count             int
	Current           bool
}

func tabsOf(o *overview.Overview, q *api.QueueView, open string) []overviewTab {
	now := nowOf(q)
	counts := map[string]int{"now": len(now.Running) + now.Waiting(), "failing": len(o.Failing), "unstable": len(o.Unstable), "flaky": len(o.Flaky)}
	tones := map[string]string{"failing": "bad", "unstable": "warn"}
	tabs := make([]overviewTab, len(overviewTabs))
	for i, t := range overviewTabs {
		tabs[i] = overviewTab{Name: t.Name, Label: t.Label, Tone: tones[t.Name], Count: counts[t.Name], Current: t.Name == open}
	}
	return tabs
}

type overviewData struct {
	Tab      string
	Tabs     []overviewTab
	Queue    queueData
	Overview *overview.Overview
	Range    overview.Range
	Ranges   []rangeLink
	Charts   []*chart
	Why      map[int64]*why
}

type rangeLink struct {
	Label, Name, Href string
	Current           bool
}

func rangeLinks(rg overview.Range) []rangeLink {
	var links []rangeLink
	for _, c := range []overview.Range{overview.Day, overview.Week} {
		links = append(links, rangeLink{Label: c.Label, Name: c.Name, Href: windowHref(overview.Day, c), Current: c == rg})
	}
	return links
}

func windowHref(def, rg overview.Range) string {
	if rg == def {
		return "/"
	}
	return "/?window=" + rg.Name
}

func (s *Site) jobs(w http.ResponseWriter, r *http.Request, u *store.User) {
	if r.URL.Query().Get("only") == "removed" {
		redirect(w, r, deletedPath, http.StatusMovedPermanently)
		return
	}
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
	d.Edits = u.Admin && s.api.Service().JobEditMode() != jobstore.Off
	s.render(w, r, "jobs", http.StatusOK, page{Title: d.Heading, Data: d, Here: d.Self, groups: all, Queue: q})
}

type queueData struct {
	Queue  *api.QueueView
	Ended  []*store.Build
	CSRF   string
	Next   string
	Full   bool
	Nested bool
}

func (s *Site) queue(w http.ResponseWriter, r *http.Request, u *store.User) {
	q, err := s.api.Service().Queue(r.Context(), u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ended, err := s.api.Service().NewestEnded(r.Context(), u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, "queue", http.StatusOK, page{Title: "Queue", Data: queueData{Queue: q, Ended: ended, CSRF: csrf(r), Next: thisPage(r), Full: true}, Queue: q})
}

type cancelForm struct {
	Build *store.Build
	Next  string
}

func cancelOf(b *store.Build, next string) cancelForm { return cancelForm{b, next} }

func thisPage(r *http.Request) string {
	if from := r.Header.Get("HX-Current-URL"); from != "" {
		if u, err := url.Parse(from); err == nil {
			return localPath(u.RequestURI())
		}
	}
	return localPath(r.URL.RequestURI())
}

func csrf(r *http.Request) string {
	if sess := api.SessionFrom(r.Context()); sess != nil {
		return sess.CSRF
	}
	return ""
}

type moment struct{ Kind, Word, At, Title, Text string }

func momentIn(kind string, zone *time.Location, now func() time.Time) func(v any, word string) *moment {
	return func(v any, word string) *moment {
		var t time.Time
		switch at := v.(type) {
		case time.Time:
			t = at
		case *time.Time:
			if at == nil {
				return nil
			}
			t = *at
		}
		if t.IsZero() {
			return nil
		}
		d := now().Sub(t)
		text := coarse(d)
		switch kind {
		case "elapsed":
			text = short(d)
		case "past":
			text += " ago"
		}
		return &moment{Kind: kind, Word: word, At: t.UTC().Format(time.RFC3339), Title: t.In(zone).Format("2006-01-02 15:04:05 MST"), Text: text}
	}
}

func elapsedIn(zone *time.Location, now func() time.Time) func(v any, word string) *moment {
	return momentIn("elapsed", zone, now)
}

func sinceIn(zone *time.Location, now func() time.Time) func(v any, word string) *moment {
	return momentIn("since", zone, now)
}

func pastIn(zone *time.Location, now func() time.Time) func(v any, word string) *moment {
	return momentIn("past", zone, now)
}

type nowView struct {
	Running, Placing []api.RunningView
	Queued           []api.QueuedView
	Runners          []api.RunnerLoad
}

// Waiting counts the builds that run nothing.
func (n *nowView) Waiting() int { return len(n.Placing) + len(n.Queued) }

func nowOf(q *api.QueueView) *nowView {
	n := &nowView{Queued: q.Queued, Runners: q.Runners}
	for _, r := range q.Running {
		if placing(r) {
			n.Placing = append(n.Placing, r)
		} else {
			n.Running = append(n.Running, r)
		}
	}
	return n
}

func endedAt(b *store.Build) time.Time {
	switch {
	case b.FinishedAt != nil:
		return *b.FinishedAt
	case b.StartedAt != nil:
		return *b.StartedAt
	}
	return b.QueuedAt
}

func loadText(l api.RunnerLoad) string {
	if l.Weight != 0 {
		return loadWithoutWeight(l) + fmt.Sprintf(" weight %d", l.Weight)
	}
	return loadWithoutWeight(l)
}

func loadWithoutWeight(l api.RunnerLoad) string {
	switch {
	case l.State != api.StateOnline:
		return l.Name + " " + l.State
	case l.Limit == 0:
		return fmt.Sprintf("%s %d/no limit", l.Name, l.Busy)
	case l.PreparesOnly:
		return fmt.Sprintf("%s %d/%d prepares", l.Name, l.Busy, l.Limit)
	}
	return fmt.Sprintf("%s %d/%d", l.Name, l.Busy, l.Limit)
}

func placing(r api.RunningView) bool {
	if len(r.Stages) == 0 {
		return false
	}
	for _, s := range r.Stages {
		if _, waits := r.Waiting[s]; !waits {
			return false
		}
	}
	return true
}

func localStages(r api.RunningView) []string {
	var out []string
	for _, s := range r.Stages {
		_, waits := r.Waiting[s]
		_, away := r.Remote[s]
		if !waits && !away {
			out = append(out, s)
		}
	}
	return out
}

func placeLines(r api.RunningView) []string {
	var out []string
	for _, s := range r.Stages {
		if why, waits := r.Waiting[s]; waits {
			out = append(out, s+": "+placeWait(why))
		} else if where, away := r.Remote[s]; away {
			out = append(out, s+": "+onWhere(where))
		}
	}
	return out
}

func onWhere(place string) string {
	if place == "" {
		return "on another machine"
	}
	return "on " + place
}

func placeWait(line string) string {
	for _, kind := range []string{"runner", "engine"} {
		for _, art := range []string{"a ", "an "} {
			rest, ok := strings.CutPrefix(line, "waiting for "+art+kind+" with labels [")
			if !ok || !strings.HasSuffix(rest, "]") {
				continue
			}
			labels := strings.TrimSuffix(rest, "]")
			word := "label"
			if strings.Contains(labels, ",") {
				word = "labels"
			}
			return "waiting for " + kind + " (" + word + " " + labels + ")"
		}
	}
	return line
}

func join(names []string) string { return strings.Join(names, ", ") }

func groups(jobs []api.JobSummary) []*group {
	var out []*group
	byName := map[string]*group{}
	for _, j := range jobs {
		if j.Removed {
			continue
		}
		g := byName[j.Group]
		if g == nil {
			g = &group{Name: j.Group}
			byName[j.Group] = g
			out = append(out, g)
		}
		g.Jobs = append(g.Jobs, j)
	}
	slices.SortFunc(out, func(a, b *group) int {
		return cmp.Or(cmp.Compare(btoi(a.Name == ""), btoi(b.Name == "")), cmp.Compare(a.Name, b.Name))
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
	View              *api.JobView
	Admin             bool
	CanBuild          bool
	Tests             testsView
	Why               map[int64]*why
	Store, StoreName  string
	Live, Gone, Forks []api.ChildView
	Tabs              *tabsView
	EditsOff          bool
	Conflict          *conflictView
	Notice            string
}

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

type testsView struct {
	Rows          []historyRow
	Total, Failed int
	Builds        string
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
	here := ""
	if v.Job != nil {
		here = (&group{Name: v.Job.Group}).href()
	}
	whys, err := s.jobWhys(r.Context(), v.Builds)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	data := jobData{Admin: u.Admin, Why: whys, View: v, CanBuild: canBuild, Tests: testsOf(historyRows(hist), r.URL.Query().Get("results") == "all")}
	if v.Job != nil {
		data.StoreName = v.Job.StoreName()
		data.Store = metricsPath(data.StoreName, "")
	}
	data.split(v.Children)
	if err := s.jobEdits(r, u, &data); err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, "job", http.StatusOK, page{Title: v.Name, Here: here, Job: v.Name, Data: data})
}

type historyRow struct {
	Suite, Name string
	Dots        []dot
}

type dot struct {
	Build  int
	Status string
}

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

func (s *Site) scan(w http.ResponseWriter, r *http.Request, u *store.User) {
	name := r.PathValue("name")
	if err := s.api.Service().ScanNow(r.Context(), u, name); err != nil {
		s.fail(w, r, err)
		return
	}
	redirect(w, r, "/jobs/"+url.PathEscape(name), http.StatusSeeOther)
}

func buildPath(b *store.Build) string {
	return "/jobs/" + url.PathEscape(b.Job) + "/" + strconv.Itoa(b.Number)
}

func (s *Site) numbered(r *http.Request, u *store.User) (*store.Build, error) {
	n, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		return nil, api.ErrNotFound
	}
	return s.api.Service().BuildNumbered(r.Context(), u, r.PathValue("name"), n)
}

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
	View      *api.BuildView
	Admin     bool
	Status    string
	Live      bool
	Waited    string
	Exit      string
	CanCancel bool
	CanAgain  bool
	CanKeep   bool
	Logs      []logView
	Notice    *notice
	Tests     []store.Test
	Total     int
	AllTests  bool
	Counts    map[string]int
	Guards    []guardView
	Artifacts []artifactsView
}

type artifactsView struct {
	Stage, Zip string
	Files      []fileView
}

type fileView struct {
	Path, Size, Href string
}

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

type guardView struct {
	Stage, Line, Outcome string
}

func guardViews(rows []store.GuardRow) []guardView {
	out := make([]guardView, len(rows))
	for i, r := range rows {
		out[i] = guardView{Stage: r.Stage, Line: api.GuardLine(r), Outcome: r.Outcome}
	}
	return out
}

type logView struct {
	Name   string
	Open   bool
	Status store.StageStatus
}

func (s *Site) buildPage(w http.ResponseWriter, r *http.Request, u *store.User) {
	data, err := s.buildData(r, u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	b := data.View.Build
	here := ""
	if !data.View.Removed {
		here = (&group{Name: data.View.Group}).href()
	}
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
	status := showWaiting(v)
	data := &buildData{View: v, Admin: u.Admin, Status: status, Tests: tests, Counts: map[string]int{}, Logs: s.logs(ctx, u, v), Guards: guardViews(guards), Artifacts: artifactViews(id, kept)}
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
		data.Tests = data.Tests[:data.Counts["failed"]]
	}
	return data, nil
}

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

func exitKnown(st *store.Stage, b *store.Build) bool {
	if st.Status == store.StageCancelled {
		return false
	}
	return st.ExitCode == nil || *st.ExitCode >= 0 || (b.Status != store.Timeout && b.Status != store.Lost)
}

func exitText(st *store.Stage, b *store.Build) string {
	if st.ExitCode == nil || !exitKnown(st, b) {
		return ""
	}
	return strconv.Itoa(*st.ExitCode)
}

func planned(needs map[string][]string) []*store.Stage {
	var out []*store.Stage
	for _, name := range slices.Sorted(maps.Keys(needs)) {
		out = append(out, &store.Stage{Name: name, Status: store.StagePending})
	}
	return out
}

func testRank(status string) int {
	return max(slices.Index([]string{"failed", "skipped", "passed"}, status), 0)
}

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
	if last := len(out) - 1; !anyOpen && last >= first && v.Build.Status.Terminal() {
		out[last].Open = true
	}
	own(config.HooksLog, false)
	return out
}

const stageWaiting store.StageStatus = "waiting"

func showWaiting(v *api.BuildView) string {
	running, waiting := false, false
	for _, st := range v.Stages {
		if st.Status != store.StageRunning {
			continue
		}
		if _, ok := v.Waiting[st.Name]; ok {
			st.Status, waiting = stageWaiting, true
		} else {
			running = true
		}
	}
	if v.Build.Status == store.Running && waiting && !running {
		return string(stageWaiting)
	}
	return string(v.Build.Status)
}

func worthReading(status store.StageStatus) bool {
	switch status {
	case store.StageRunning, stageWaiting, store.StageFailed, store.StageCancelled, store.StageUnstable:
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

type whenView struct{ At, Title, Abs, Text string }

func whenIn(zone *time.Location, now func() time.Time) func(v any) *whenView {
	return func(v any) *whenView {
		if t, ok := v.(*time.Time); ok && t != nil {
			v = *t
		}
		t, ok := v.(time.Time)
		if !ok || t.IsZero() {
			return nil
		}
		abs := t.In(zone).Format("2006-01-02 15:04")
		text := abs
		if d := t.Sub(now()); d > -24*time.Hour && d < 24*time.Hour {
			if d < 0 {
				text = coarse(-d) + " ago"
			} else {
				text = "in " + coarse(d)
			}
		}
		return &whenView{At: t.UTC().Format(time.RFC3339), Title: t.In(zone).Format("2006-01-02 15:04:05 MST"), Abs: abs, Text: text}
	}
}

func zoned(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format("2006-01-02 15:04:05 MST")
}

func ago(t *time.Time) string {
	if t == nil {
		return ""
	}
	return short(time.Since(*t)) + " ago"
}

func took(start, end *time.Time) string {
	switch {
	case start == nil:
		return ""
	case end == nil:
		return short(time.Since(*start))
	}
	return short(end.Sub(*start))
}

func coarse(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", d/time.Minute)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", d/time.Hour, d%time.Hour/time.Minute)
	}
	return fmt.Sprintf("%dd", d/(24*time.Hour))
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

func lines(value string) bool { return strings.Contains(value, "\n") }

func millis(ms int) string {
	return (time.Duration(ms) * time.Millisecond).String()
}

func percent(f overview.FlakyJob) string { return strconv.Itoa(f.Percent()) + "%" }
