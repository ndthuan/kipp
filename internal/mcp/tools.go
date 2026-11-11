package mcp

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/overview"
	"github.com/ndthuan/kipp/internal/store"
)

// Limits on what a tool takes and returns, so that no answer outgrows a
// model's context.
const (
	defaultBuilds = 10
	maxBuilds     = 50
	defaultTests  = 50
	maxTests      = 200
	maxMessage    = 500
	maxChildren   = 50
)

// may is the one place that decides whether c may do action (view, build or
// cancel) on a job: the credential's ceiling and the user's grant must both
// allow it. Every tool asks it, through guard or directly, before it acts or
// answers about a job; a new way to authenticate only supplies a Ceiling.
func (s *Server) may(ctx context.Context, c *Caller, job, action string) (bool, error) {
	if !c.Ceiling.Allows(action) {
		return false, nil
	}
	return s.cfg.Auth.Can(ctx, c.User, job, action)
}

// guard is nil when c may do action on the job. A job c may not view is
// not found, whatever the action, as in the API; one it may view and not do
// action on is forbidden.
func (s *Server) guard(ctx context.Context, c *Caller, job, action string) error {
	ok, err := s.may(ctx, c, job, "view")
	switch {
	case err != nil:
		return err
	case !ok:
		return api.ErrNotFound
	case action == "view":
		return nil
	}
	if ok, err = s.may(ctx, c, job, action); err != nil {
		return err
	}
	if !ok {
		return api.ErrForbidden
	}
	return nil
}

// sees reports whether c may view anything at all; a list over every job
// is empty for a credential that may not.
func sees(c *Caller) bool { return c.Ceiling.Allows("view") }

func badInput(format string, args ...any) error {
	return fmt.Errorf("%w: %s", api.ErrBadInput, fmt.Sprintf(format, args...))
}

func reads() *sdk.ToolAnnotations {
	return &sdk.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(false)}
}

func starts() *sdk.ToolAnnotations {
	return &sdk.ToolAnnotations{DestructiveHint: new(false), IdempotentHint: false, OpenWorldHint: new(false)}
}

func stops() *sdk.ToolAnnotations {
	return &sdk.ToolAnnotations{DestructiveHint: new(true), IdempotentHint: false, OpenWorldHint: new(false)}
}

// add registers a tool whose handler is given the verified caller.
func add[In, Out any](s *Server, srv *sdk.Server, name, desc string, ann *sdk.ToolAnnotations, h func(context.Context, *Caller, In) (Out, error)) {
	sdk.AddTool(srv, &sdk.Tool{Name: name, Description: desc, Annotations: ann},
		func(ctx context.Context, req *sdk.CallToolRequest, in In) (*sdk.CallToolResult, Out, error) {
			var none Out
			c, err := callerOf(req)
			if err != nil {
				return nil, none, err
			}
			out, err := h(ctx, c, in)
			if err != nil {
				return nil, none, s.refusal(err)
			}
			return nil, out, nil
		})
}

// implementation names the server to a client. A client such as a Claude
// connector otherwise guesses an icon from the host's registrable domain,
// which is another site's when kipp runs on a subdomain.
func (s *Server) implementation() *sdk.Implementation {
	impl := &sdk.Implementation{Name: "kipp", Title: "kipp", Version: s.cfg.Version}
	if s.cfg.BaseURL == "" {
		return impl
	}
	base := strings.TrimSuffix(s.cfg.BaseURL, "/")
	impl.WebsiteURL = base
	impl.Icons = []sdk.Icon{{Source: base + "/static/favicon.svg", MIMEType: "image/svg+xml", Sizes: []string{"any"}}}
	return impl
}

func (s *Server) newServer() *sdk.Server {
	srv := sdk.NewServer(s.implementation(), &sdk.ServerOptions{
		Instructions: "kipp is a CI server. Start with overview to see what is red; read_log with a build's number shows why. " +
			"What you can see and do is what the token's user may.",
	})
	add(s, srv, "overview", "What needs attention now: failing, unstable and flaky jobs, recent failures, what runs next, how each group stands, a summary of the graphs, and the queue. window is 24h (default) or 7d.", reads(), s.overview)
	add(s, srv, "list_jobs", "List the jobs you may see, with each one's last build and schedule. Optionally only one group.", reads(), s.listJobs)
	add(s, srv, "get_job", "One job: its parameters (names, defaults, choices) to pass to run_build, its stages, schedule, concurrency, recent builds and, for a multibranch job, its branches and pull requests.", reads(), s.getJob)
	add(s, srv, "list_builds", "A job's builds, newest first. Pass next_cursor back as cursor for older ones.", reads(), s.listBuilds)
	add(s, srv, "get_build", "One build: status, cause, parameters, times, and each stage with its status and duration.", reads(), s.getBuild)
	add(s, srv, "read_log", "A stage's console. By default the last tail_lines lines; with offset, a byte range. Says the log's total size and whether the text was cut, so you can page.", reads(), s.readLog)
	add(s, srv, "test_results", "A build's test cases from its JUnit results, failures first, with counts.", reads(), s.testResults)
	add(s, srv, "list_artifacts", "The files a build's stages kept, with sizes. Names only, not content.", reads(), s.listArtifacts)
	add(s, srv, "queue", "What runs now and what waits, and why.", reads(), s.queue)
	add(s, srv, "run_build", "Start a build of a job, with parameters if it has any (see get_job). Needs the build grant.", starts(), s.runBuild)
	add(s, srv, "build_again", "Start a build like an earlier one: same parameters, same checkout. Needs the build grant.", starts(), s.buildAgain)
	add(s, srv, "cancel_build", "Stop a build that is queued or running. Needs the cancel grant.", stops(), s.cancelBuild)
	return srv
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func stampOf(t *time.Time) string {
	if t == nil {
		return ""
	}
	return stamp(*t)
}

func (s *Server) buildURL(job string, number int) string {
	if s.cfg.BaseURL == "" {
		return ""
	}
	return strings.TrimSuffix(s.cfg.BaseURL, "/") + "/jobs/" + url.PathEscape(job) + "/" + strconv.Itoa(number)
}

// buildOut is a build wherever a tool answers with one.
type buildOut struct {
	ID            int64             `json:"id"`
	Job           string            `json:"job"`
	Number        int               `json:"number"`
	Status        string            `json:"status"`
	Trigger       string            `json:"trigger"`
	TriggerDetail string            `json:"trigger_detail,omitempty"`
	StartedBy     string            `json:"started_by,omitempty"`
	Params        map[string]string `json:"params,omitempty"`
	QueuedAt      string            `json:"queued_at"`
	StartedAt     string            `json:"started_at,omitempty"`
	FinishedAt    string            `json:"finished_at,omitempty"`
	Branch        string            `json:"branch,omitempty"`
	PR            int               `json:"pr,omitempty"`
	Commit        string            `json:"commit,omitempty"`
	URL           string            `json:"url,omitempty"`
}

func (s *Server) buildOf(b *store.Build) buildOut {
	return buildOut{ID: b.ID, Job: b.Job, Number: b.Number, Status: string(b.Status), Trigger: b.Trigger, TriggerDetail: b.TriggerDetail,
		StartedBy: b.StartedBy, Params: b.Params, QueuedAt: stamp(b.QueuedAt), StartedAt: stampOf(b.StartedAt), FinishedAt: stampOf(b.FinishedAt),
		Branch: b.TriggerBranch, PR: b.PR, Commit: b.BuiltCommit, URL: s.buildURL(b.Job, b.Number)}
}

func (s *Server) buildsOf(bs []*store.Build) []buildOut {
	out := make([]buildOut, 0, len(bs))
	for _, b := range bs {
		out = append(out, s.buildOf(b))
	}
	return out
}

// lastOut is a job's last build in a list of jobs.
type lastOut struct {
	Number     int    `json:"number"`
	Status     string `json:"status"`
	FinishedAt string `json:"finished_at,omitempty"`
}

func lastOf(b *store.Build) *lastOut {
	if b == nil {
		return nil
	}
	return &lastOut{b.Number, string(b.Status), stampOf(b.FinishedAt)}
}

// jobRef names a build by its job and number, as a model passes it back.
type jobRef struct {
	Job    string `json:"job" jsonschema:"the job's name"`
	Number int    `json:"number" jsonschema:"the build's number within the job"`
}

// build is the build jobRef names, if the caller may view its job.
func (s *Server) build(ctx context.Context, c *Caller, r jobRef) (*store.Build, error) {
	if err := s.guard(ctx, c, r.Job, "view"); err != nil {
		return nil, err
	}
	if r.Number <= 0 {
		return nil, badInput("number is a build number, 1 or more")
	}
	return s.cfg.Service.BuildNumbered(ctx, c.User, r.Job, r.Number)
}

type none struct{}

// --- overview -------------------------------------------------------------

type overviewIn struct {
	Window string `json:"window,omitempty" jsonschema:"how far back the graphs look: 24h (default) or 7d"`
}

type overviewOut struct {
	Failing  []failingOut  `json:"failing"`
	Unstable []unstableOut `json:"unstable"`
	Flaky    []flakyOut    `json:"flaky"`
	Groups   []groupOut    `json:"groups"`
	Charts   chartsOut     `json:"charts"`
	Queue    queueOut      `json:"queue"`
}

// chartsOut is each graph in a line or two: what ended in the window, how
// busy the server was, and how each group's builds fared.
type chartsOut struct {
	Window            string        `json:"window"`
	Success           int           `json:"success"`
	Unstable          int           `json:"unstable"`
	Failed            int           `json:"failed"`
	Cancelled         int           `json:"cancelled"`
	PeakRunning       int           `json:"peak_running"`
	BuildsLimit       int           `json:"builds_limit"`
	LongestWaitSecond int           `json:"longest_wait_seconds"`
	PassRates         []passRateOut `json:"pass_rates"`
}

type passRateOut struct {
	Group   string `json:"group"`
	Percent int    `json:"percent"`
	Builds  int    `json:"builds"`
	// Change is the points against the window before, absent when either
	// window has no build to compare.
	Change *int `json:"change,omitempty"`
}

func chartsSummary(c *overview.Charts) chartsOut {
	out := chartsOut{Window: c.Range.Name, BuildsLimit: c.BuildsLimit, PassRates: []passRateOut{}}
	for _, b := range c.Buckets {
		out.Success += b.Success
		out.Unstable += b.Unstable
		out.Failed += b.Failed
		out.Cancelled += b.Cancelled
		out.PeakRunning = max(out.PeakRunning, b.MaxRunning)
		out.LongestWaitSecond = max(out.LongestWaitSecond, int(b.WaitMax.Seconds()))
	}
	for _, g := range c.Groups {
		if g.Now.Total() == 0 {
			continue
		}
		r := passRateOut{Group: g.Name, Percent: g.Now.Percent(), Builds: g.Now.Total()}
		if d, ok := g.Delta(); ok {
			r.Change = &d
		}
		out.PassRates = append(out.PassRates, r)
	}
	return out
}

type failingOut struct {
	Job    string   `json:"job"`
	Build  lastOut  `json:"build"`
	Since  string   `json:"red_since"`
	Flaky  bool     `json:"flaky"`
	Recent []string `json:"recent_statuses"`
}

// unstableOut is a job whose newest finished build is unstable, and when the
// run of unstable builds it ends began.
type unstableOut struct {
	Job    string   `json:"job"`
	Build  lastOut  `json:"build"`
	Since  string   `json:"unstable_since"`
	Flaky  bool     `json:"flaky"`
	Recent []string `json:"recent_statuses"`
}

type flakyOut struct {
	Job           string `json:"job"`
	Failed        int    `json:"failed"`
	Finished      int    `json:"finished"`
	FailedPercent int    `json:"failed_percent"`
	Flips         int    `json:"flips"`
}

type groupOut struct {
	Name       string `json:"name"`
	Green      int    `json:"green"`
	Red        int    `json:"red"`
	NeverBuilt int    `json:"never_built"`
	Running    int    `json:"running"`
	Disabled   int    `json:"disabled"`
}

func statuses(bs []*store.Build) []string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, string(b.Status))
	}
	return out
}

func (s *Server) overview(ctx context.Context, c *Caller, in overviewIn) (overviewOut, error) {
	rg, ok := overview.ParseRange(in.Window)
	if !ok {
		return overviewOut{}, badInput("window is 24h or 7d")
	}
	out := overviewOut{Failing: []failingOut{}, Unstable: []unstableOut{}, Charts: chartsOut{Window: rg.Name, PassRates: []passRateOut{}}, Flaky: []flakyOut{}, Groups: []groupOut{}, Queue: emptyQueue()}
	if !sees(c) {
		return out, nil
	}
	v, err := s.cfg.Service.Overview(ctx, c.User, rg)
	if err != nil {
		return out, err
	}
	for _, f := range v.Failing {
		out.Failing = append(out.Failing, failingOut{f.Job, *lastOf(f.Build), stamp(f.Since), f.Flaky, statuses(f.Dots)})
	}
	for _, f := range v.Unstable {
		out.Unstable = append(out.Unstable, unstableOut{f.Job, *lastOf(f.Build), stamp(f.Since), f.Flaky, statuses(f.Dots)})
	}
	for _, f := range v.Flaky {
		out.Flaky = append(out.Flaky, flakyOut{f.Job, f.Failed, f.Finished, f.Percent(), f.Flips})
	}
	for _, g := range v.Groups {
		out.Groups = append(out.Groups, groupOut{g.Name, g.Green, g.Red, g.NeverBuilt, g.Running, g.Disabled})
	}
	out.Charts = chartsSummary(v.Charts)
	out.Queue, err = s.queueOf(ctx, c)
	return out, err
}

// --- jobs -----------------------------------------------------------------

type listJobsIn struct {
	Group string `json:"group,omitempty" jsonschema:"only the jobs of this group"`
}

type jobRow struct {
	Name        string   `json:"name"`
	Group       string   `json:"group,omitempty"`
	Description string   `json:"description,omitempty"`
	Disabled    bool     `json:"disabled"`
	Removed     bool     `json:"removed,omitempty"`
	Schedule    string   `json:"schedule,omitempty"`
	Next        string   `json:"next,omitempty"`
	Children    int      `json:"children,omitempty"`
	Last        *lastOut `json:"last_build"`
}

type listJobsOut struct {
	Jobs []jobRow `json:"jobs"`
}

func (s *Server) listJobs(ctx context.Context, c *Caller, in listJobsIn) (listJobsOut, error) {
	out := listJobsOut{Jobs: []jobRow{}}
	if !sees(c) {
		return out, nil
	}
	jobs, err := s.cfg.Service.Jobs(ctx, c.User)
	if err != nil {
		return out, err
	}
	for _, j := range jobs {
		if in.Group != "" && j.Group != in.Group {
			continue
		}
		out.Jobs = append(out.Jobs, jobRow{j.Name, j.Group, j.Description, j.Disabled, j.Removed, j.Cron, stampOf(j.Next), j.Children, lastOf(j.Last)})
	}
	return out, nil
}

type getJobIn struct {
	Job string `json:"job" jsonschema:"the job's name"`
}

type paramOut struct {
	Name        string   `json:"name"`
	Type        string   `json:"type,omitempty"`
	Default     string   `json:"default"`
	Choices     []string `json:"choices,omitempty"`
	Description string   `json:"description,omitempty"`
}

type stageDef struct {
	Name  string   `json:"name"`
	Needs []string `json:"needs,omitempty"`
}

type childOut struct {
	Job    string   `json:"job"`
	Kind   string   `json:"kind"`
	Branch string   `json:"branch,omitempty"`
	PR     int      `json:"pr,omitempty"`
	Title  string   `json:"title,omitempty"`
	State  string   `json:"state"`
	Last   *lastOut `json:"last_build"`
}

type getJobOut struct {
	Name          string     `json:"name"`
	Group         string     `json:"group,omitempty"`
	Description   string     `json:"description,omitempty"`
	Disabled      bool       `json:"disabled"`
	Removed       bool       `json:"removed,omitempty"`
	Params        []paramOut `json:"params"`
	Stages        []stageDef `json:"stages"`
	Schedule      string     `json:"schedule,omitempty"`
	Next          string     `json:"next,omitempty"`
	Concurrency   string     `json:"concurrency,omitempty"`
	RecentBuilds  []buildOut `json:"recent_builds"`
	Children      []childOut `json:"children,omitempty"`
	ChildrenCount int        `json:"children_count,omitempty"`
	MultibranchOf string     `json:"multibranch_repo,omitempty"`
	ChildOf       string     `json:"child_of,omitempty"`
}

func (s *Server) getJob(ctx context.Context, c *Caller, in getJobIn) (getJobOut, error) {
	if err := s.guard(ctx, c, in.Job, "view"); err != nil {
		return getJobOut{}, err
	}
	v, err := s.cfg.Service.Job(ctx, c.User, in.Job, 0)
	if err != nil {
		return getJobOut{}, err
	}
	out := getJobOut{Name: v.Name, Removed: v.Removed, Params: []paramOut{}, Stages: []stageDef{}, Next: stampOf(v.Next), ChildOf: v.Parent,
		RecentBuilds: s.buildsOf(v.Builds[:min(len(v.Builds), defaultBuilds)])}
	if j := v.Job; j != nil {
		out.Group, out.Description, out.Disabled, out.Schedule, out.Concurrency = j.Group, j.Description, j.Disabled, j.Triggers.Cron, string(j.Concurrency)
		for _, p := range j.Params {
			out.Params = append(out.Params, paramOut{p.Name, p.Type, p.Default, p.Options, p.Description})
		}
		for _, st := range j.Stages {
			out.Stages = append(out.Stages, stageDef{st.Name, st.Needs})
		}
		if j.Multibranch != nil {
			out.MultibranchOf = j.Multibranch.Repo
		}
	}
	out.ChildrenCount = len(v.Children)
	for _, ch := range v.Children[:min(len(v.Children), maxChildren)] {
		out.Children = append(out.Children, childOut{ch.Job, ch.Kind, ch.Branch, ch.PR, ch.Title, ch.State, lastOf(ch.Last)})
	}
	return out, nil
}

// --- builds ---------------------------------------------------------------

type listBuildsIn struct {
	Job    string `json:"job" jsonschema:"the job's name"`
	Limit  int    `json:"limit,omitempty" jsonschema:"how many builds, default 10, at most 50"`
	Cursor string `json:"cursor,omitempty" jsonschema:"a next_cursor of an earlier answer, for older builds"`
}

type listBuildsOut struct {
	Builds     []buildOut `json:"builds"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

func (s *Server) listBuilds(ctx context.Context, c *Caller, in listBuildsIn) (listBuildsOut, error) {
	out := listBuildsOut{Builds: []buildOut{}}
	if err := s.guard(ctx, c, in.Job, "view"); err != nil {
		return out, err
	}
	limit := in.Limit
	switch {
	case limit == 0:
		limit = defaultBuilds
	case limit < 0 || limit > maxBuilds:
		return out, badInput("limit is 1 to %d", maxBuilds)
	}
	var before int
	if in.Cursor != "" {
		n, err := strconv.Atoi(in.Cursor)
		if err != nil || n <= 0 {
			return out, badInput("cursor is a next_cursor from an earlier answer")
		}
		before = n
	}
	page, more, err := s.cfg.Service.Builds(ctx, c.User, in.Job, before, limit)
	if err != nil {
		return out, err
	}
	out.Builds = s.buildsOf(page)
	if more && len(page) > 0 {
		out.NextCursor = strconv.Itoa(page[len(page)-1].Number)
	}
	return out, nil
}

type stageOut struct {
	Name            string   `json:"name"`
	Status          string   `json:"status"`
	ExitCode        *int     `json:"exit_code,omitempty"`
	Needs           []string `json:"needs,omitempty"`
	StartedAt       string   `json:"started_at,omitempty"`
	FinishedAt      string   `json:"finished_at,omitempty"`
	DurationSeconds *int     `json:"duration_seconds,omitempty"`
	RanOn           string   `json:"ran_on,omitempty"`
	Tries           int      `json:"tries,omitempty"`
	// Reused is the green run the stage was reused from instead of running.
	Reused *reusedOut `json:"reused,omitempty"`
}

type reusedOut struct {
	Job    string `json:"job"`
	Number int    `json:"number"`
	Stage  string `json:"stage"`
}

type getBuildOut struct {
	buildOut
	Stages   []stageOut `json:"stages"`
	Complete bool       `json:"complete"`
}

func (s *Server) getBuild(ctx context.Context, c *Caller, in jobRef) (getBuildOut, error) {
	b, err := s.build(ctx, c, in)
	if err != nil {
		return getBuildOut{}, err
	}
	v, err := s.cfg.Service.Build(ctx, c.User, b.ID)
	if err != nil {
		return getBuildOut{}, err
	}
	out := getBuildOut{buildOut: s.buildOf(v.Build), Stages: []stageOut{}, Complete: v.Complete}
	for _, st := range v.Stages {
		row := stageOut{Name: st.Name, Status: string(st.Status), ExitCode: st.ExitCode, Needs: st.Needs, StartedAt: stampOf(st.StartedAt),
			FinishedAt: stampOf(st.FinishedAt), RanOn: st.RanOn, Tries: st.Tries}
		if r := st.Reused; r != nil {
			row.Reused = &reusedOut{Job: r.Job, Number: r.Number, Stage: r.Stage}
		}
		if st.StartedAt != nil && st.FinishedAt != nil {
			row.DurationSeconds = new(int(st.FinishedAt.Sub(*st.StartedAt).Seconds()))
		}
		out.Stages = append(out.Stages, row)
	}
	return out, nil
}

// --- tests and artifacts --------------------------------------------------

type testResultsIn struct {
	jobRef
	Limit int `json:"limit,omitempty" jsonschema:"how many cases to list, default 50, at most 200"`
}

type caseOut struct {
	Stage      string `json:"stage,omitempty"`
	Suite      string `json:"suite,omitempty"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	DurationMs int    `json:"duration_ms"`
	Message    string `json:"message,omitempty"`
}

type testResultsOut struct {
	Total   int       `json:"total"`
	Passed  int       `json:"passed"`
	Failed  int       `json:"failed"`
	Skipped int       `json:"skipped"`
	Cases   []caseOut `json:"cases"`
	// Cut says Cases leaves some out: they are the first of failed, then
	// skipped, then passed.
	Cut bool `json:"cut"`
}

func (s *Server) testResults(ctx context.Context, c *Caller, in testResultsIn) (testResultsOut, error) {
	out := testResultsOut{Cases: []caseOut{}}
	limit := in.Limit
	switch {
	case limit == 0:
		limit = defaultTests
	case limit < 0 || limit > maxTests:
		return out, badInput("limit is 1 to %d", maxTests)
	}
	b, err := s.build(ctx, c, in.jobRef)
	if err != nil {
		return out, err
	}
	tests, err := s.cfg.Service.Tests(ctx, c.User, b.ID)
	if err != nil {
		return out, err
	}
	byStatus := map[string][]store.Test{}
	for _, t := range tests {
		byStatus[t.Status] = append(byStatus[t.Status], t)
	}
	out.Total, out.Passed, out.Failed, out.Skipped = len(tests), len(byStatus["passed"]), len(byStatus["failed"]), len(byStatus["skipped"])
	for _, status := range []string{"failed", "skipped", "passed"} {
		for _, t := range byStatus[status] {
			if len(out.Cases) == limit {
				out.Cut = true
				return out, nil
			}
			out.Cases = append(out.Cases, caseOut{t.Stage, t.Suite, t.Name, t.Status, t.DurationMs, clip(t.Message, maxMessage)})
		}
	}
	return out, nil
}

// clip is s cut to n bytes at a rune boundary, marked when it was.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "..."
}

type fileOut struct {
	Stage  string `json:"stage"`
	Path   string `json:"path"`
	Size   int64  `json:"size_bytes"`
	SHA256 string `json:"sha256"`
}

type listArtifactsOut struct {
	Files []fileOut `json:"files"`
}

func (s *Server) listArtifacts(ctx context.Context, c *Caller, in jobRef) (listArtifactsOut, error) {
	out := listArtifactsOut{Files: []fileOut{}}
	b, err := s.build(ctx, c, in)
	if err != nil {
		return out, err
	}
	_, stages, err := s.cfg.Service.Artifacts(ctx, c.User, b.ID)
	if err != nil {
		return out, err
	}
	for _, st := range stages {
		for _, f := range st.Files {
			out.Files = append(out.Files, fileOut{st.Stage, f.Path, f.Size, f.SHA256})
		}
	}
	return out, nil
}

// --- queue ----------------------------------------------------------------

type queueOut struct {
	Executors *int         `json:"executors,omitempty"`
	Busy      *int         `json:"busy,omitempty"`
	Running   []runningOut `json:"running"`
	Queued    []queuedOut  `json:"queued"`
}

type runningOut struct {
	Job       string   `json:"job"`
	Number    int      `json:"number"`
	StartedAt string   `json:"started_at,omitempty"`
	Stages    []string `json:"running_stages"`
}

type queuedOut struct {
	Job      string `json:"job"`
	Number   int    `json:"number"`
	QueuedAt string `json:"queued_at"`
	Waiting  string `json:"waiting_for"`
	Blocker  string `json:"blocked_by,omitempty"`
}

func emptyQueue() queueOut { return queueOut{Running: []runningOut{}, Queued: []queuedOut{}} }

func (s *Server) queue(ctx context.Context, c *Caller, _ none) (queueOut, error) {
	return s.queueOf(ctx, c)
}

func (s *Server) queueOf(ctx context.Context, c *Caller) (queueOut, error) {
	out := emptyQueue()
	if !sees(c) {
		return out, nil
	}
	v, err := s.cfg.Service.Queue(ctx, c.User)
	if err != nil {
		return out, err
	}
	if v.Executors > 0 {
		out.Executors, out.Busy = &v.Executors, &v.Busy
	}
	for _, r := range v.Running {
		stages := r.Stages
		if stages == nil {
			stages = []string{}
		}
		out.Running = append(out.Running, runningOut{r.Build.Job, r.Build.Number, stampOf(r.Build.StartedAt), stages})
	}
	for _, w := range v.Queued {
		row := queuedOut{Job: w.Build.Job, Number: w.Build.Number, QueuedAt: stamp(w.Build.QueuedAt), Waiting: string(w.Waiting)}
		if w.Blocker != nil {
			row.Blocker = fmt.Sprintf("%s #%d", w.Blocker.Job, w.Blocker.Number)
		}
		out.Queued = append(out.Queued, row)
	}
	return out, nil
}

// --- starting and stopping ------------------------------------------------

type runBuildIn struct {
	Job    string            `json:"job" jsonschema:"the job's name"`
	Params map[string]string `json:"params,omitempty" jsonschema:"the job's parameters by name; those left out take their defaults"`
}

func (s *Server) runBuild(ctx context.Context, c *Caller, in runBuildIn) (buildOut, error) {
	if err := s.guard(ctx, c, in.Job, "build"); err != nil {
		return buildOut{}, err
	}
	b, err := s.cfg.Service.Trigger(ctx, c.User, in.Job, in.Params, "api")
	if err != nil {
		return buildOut{}, err
	}
	return s.buildOf(b), nil
}

func (s *Server) buildAgain(ctx context.Context, c *Caller, in jobRef) (buildOut, error) {
	if err := s.guard(ctx, c, in.Job, "build"); err != nil {
		return buildOut{}, err
	}
	old, err := s.build(ctx, c, in)
	if err != nil {
		return buildOut{}, err
	}
	b, err := s.cfg.Service.Again(ctx, c.User, old.ID, "api")
	if err != nil {
		return buildOut{}, err
	}
	return s.buildOf(b), nil
}

type cancelOut struct {
	Job       string `json:"job"`
	Number    int    `json:"number"`
	Cancelled bool   `json:"cancel_requested"`
}

func (s *Server) cancelBuild(ctx context.Context, c *Caller, in jobRef) (cancelOut, error) {
	if err := s.guard(ctx, c, in.Job, "cancel"); err != nil {
		return cancelOut{}, err
	}
	b, err := s.build(ctx, c, in)
	if err != nil {
		return cancelOut{}, err
	}
	if err := s.cfg.Service.Cancel(ctx, c.User, b.ID); err != nil {
		return cancelOut{}, err
	}
	return cancelOut{b.Job, b.Number, true}, nil
}
