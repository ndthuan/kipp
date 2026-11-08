package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/dispatch"
	"github.com/ndthuan/kipp/internal/overview"
	"github.com/ndthuan/kipp/internal/runner"
	"github.com/ndthuan/kipp/internal/store"
	"github.com/ndthuan/kipp/internal/waitfor"
)

const secretValue = "an-obvious-test-secret"

var jobFiles = map[string]string{
	"smoke.yml": "name: smoke\ngroup: backend\ndescription: A quick one.\nparams:\n  TIER: {type: choice, options: [quick, full], default: quick, description: How much to run.}\n" +
		"stages:\n  s: {run: 'echo tier=$TIER'}\n  t: {run: 'echo second', needs: [s]}\n",
	"other.yml":  "name: other\ngroup: backend\nstages:\n  s: {run: echo other}\n",
	"slow.yml":   "name: slow\nstages:\n  s: {run: 'echo started; sleep 30'}\n",
	"secret.yml": "name: secret\nsecrets: [API_KEY]\nstages:\n  s: {run: 'echo key=$API_KEY'}\n",
	"long.yml":   "name: long\nstages:\n  s: {run: 'i=1; while [ $i -le 3000 ]; do echo line $i; i=$((i+1)); done'}\n",
	"nohit.yml":  "name: nohit\ntriggers: {cron: '0 3 * * *'}\nstages:\n  s: {run: 'exit 3'}\n",
}

type env struct {
	d       *dispatch.Dispatcher
	srv     *httptest.Server
	st      *store.Store
	au      *auth.Service
	admin   string
	viewer  string // view on smoke
	builder string // build on smoke
	stopper string // cancel on slow
}

func newEnv(t *testing.T, verify Verifier) *env { return newEnvAt(t, verify, "") }

// newEnvAt is newEnv with the address of the resource metadata a 401 points to.
func newEnvAt(t *testing.T, verify Verifier, metadata string) *env {
	t.Helper()
	dir := t.TempDir()
	jobs := filepath.Join(dir, "jobs")
	if err := os.MkdirAll(jobs, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, body := range jobFiles {
		if err := os.WriteFile(filepath.Join(jobs, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	snap, err := config.Load(jobs)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	quiet := slog.New(slog.DiscardHandler)
	d := dispatch.New(st, runner.Local{}, dispatch.Options{DataDir: dir, Logger: quiet, Executors: 4,
		Secrets: func(_ context.Context, names []string) (map[string]dispatch.Secret, error) {
			m := map[string]dispatch.Secret{}
			for _, n := range names {
				m[n] = dispatch.Secret{Kind: config.SecretText, Value: secretValue, Scopes: []string{config.ScopeGlobal}}
			}
			return m, nil
		}})
	d.SetSnapshot(snap)
	ctx, stop := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { d.Run(ctx); close(stopped) }()
	au := auth.New(st, time.Hour, []byte("an obvious test device key 32 b."))
	a := api.New(api.Deps{Auth: au, Dispatch: d, Store: st, Version: "test", Logger: quiet, FlakyFlips: overview.DefaultFlips})
	m := New(Config{Service: a.Service(), Auth: au, Verify: verify, ResourceMetadataURL: metadata, Version: "test", BaseURL: "https://ci.example.com", Logger: quiet})
	mux := http.NewServeMux()
	mux.Handle(Path, m.Handler())
	e := &env{d: d, st: st, au: au, srv: httptest.NewServer(mux)}
	t.Cleanup(func() { stop(); <-stopped; e.srv.Close(); _ = st.Close() })
	e.admin = e.token(t, "admin", true, nil, nil)
	e.viewer = e.token(t, "viewer", false, map[string][]string{"smoke": {"view"}}, nil)
	e.builder = e.token(t, "builder", false, map[string][]string{"smoke": {"build"}}, nil)
	e.stopper = e.token(t, "stopper", false, map[string][]string{"slow": {"cancel"}}, nil)
	return e
}

// token makes a user with grants and a token for them.
func (e *env) token(t *testing.T, name string, admin bool, grants map[string][]string, expires *time.Time) string {
	t.Helper()
	ctx := context.Background()
	u, err := e.st.CreateUser(ctx, name, "x", admin)
	if err != nil {
		t.Fatal(err)
	}
	for job, perms := range grants {
		if err := e.st.Grant(ctx, u.ID, job, perms); err != nil {
			t.Fatal(err)
		}
	}
	plain, hash, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.CreateTokenUntil(ctx, u.ID, "test", hash, expires); err != nil {
		t.Fatal(err)
	}
	return plain
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// session is an MCP client connected as the token's user.
func (e *env) session(t *testing.T, token string) *sdk.ClientSession {
	t.Helper()
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	s, err := client.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: e.srv.URL + Path, HTTPClient: &http.Client{Transport: bearer{token}}, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// call runs a tool and decodes its structured answer into out (when out is
// not nil); it returns the text of a tool error, empty for a good answer.
func call(t *testing.T, s *sdk.ClientSession, tool string, args map[string]any, out any) string {
	t.Helper()
	res, err := s.CallTool(context.Background(), &sdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	if res.IsError {
		var text []string
		for _, c := range res.Content {
			if tc, ok := c.(*sdk.TextContent); ok {
				text = append(text, tc.Text)
			}
		}
		return strings.Join(text, " ")
	}
	if out != nil {
		raw, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s: %v in %s", tool, err, raw)
		}
	}
	return ""
}

func ok(t *testing.T, s *sdk.ClientSession, tool string, args map[string]any, out any) {
	t.Helper()
	if msg := call(t, s, tool, args, out); msg != "" {
		t.Fatalf("%s refused: %s", tool, msg)
	}
}

func refused(t *testing.T, s *sdk.ClientSession, tool string, args map[string]any, want string) {
	t.Helper()
	msg := call(t, s, tool, args, nil)
	if msg == "" || !strings.Contains(msg, want) {
		t.Fatalf("%s: got error %q, want one saying %q", tool, msg, want)
	}
}

// ended waits for the build to end, and for what is written after its
// status, and returns it.
func (e *env) ended(t *testing.T, job string, number int) *store.Build {
	t.Helper()
	b, err := e.st.BuildByNumber(context.Background(), job, number)
	if err != nil {
		t.Fatalf("%s #%d: %v", job, number, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), waitfor.Safety)
	defer cancel()
	b, err = e.d.Wait(ctx, b.ID)
	if err != nil {
		t.Fatalf("%s #%d did not end: %v", job, number, err)
	}
	return b
}

// built runs a job as the admin and waits for its build to end.
func (e *env) built(t *testing.T, job string, params map[string]any) *store.Build {
	t.Helper()
	var b buildOut
	args := map[string]any{"job": job}
	if params != nil {
		args["params"] = params
	}
	ok(t, e.session(t, e.admin), "run_build", args, &b)
	return e.ended(t, job, b.Number)
}

func TestToolsListIsExactlyTheDocumentedSetWithAnnotations(t *testing.T) {
	e := newEnv(t, nil)
	res, err := e.session(t, e.admin).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	type want struct{ readOnly, destructive *bool }
	f, tr := new(bool), new(bool)
	*tr = true
	reads, starts, stops := want{readOnly: tr}, want{destructive: f}, want{destructive: tr}
	wants := map[string]want{"overview": reads, "list_jobs": reads, "get_job": reads, "list_builds": reads, "get_build": reads, "read_log": reads,
		"test_results": reads, "list_artifacts": reads, "queue": reads, "run_build": starts, "build_again": starts, "cancel_build": stops}
	var got []string
	for _, tool := range res.Tools {
		got = append(got, tool.Name)
		w, known := wants[tool.Name]
		if !known {
			t.Fatalf("unexpected tool %s", tool.Name)
		}
		a := tool.Annotations
		if a == nil || tool.Description == "" || tool.InputSchema == nil {
			t.Fatalf("%s lacks annotations, a description or an input schema", tool.Name)
		}
		if a.ReadOnlyHint != (w.readOnly != nil) {
			t.Errorf("%s: readOnlyHint %v", tool.Name, a.ReadOnlyHint)
		}
		if w.readOnly == nil && (a.DestructiveHint == nil || *a.DestructiveHint != *w.destructive || a.IdempotentHint) {
			t.Errorf("%s: destructive %v, idempotent %v", tool.Name, a.DestructiveHint, a.IdempotentHint)
		}
	}
	if len(got) != len(wants) {
		t.Fatalf("tools %v, want %d of them", got, len(wants))
	}
}

func TestTheToolsThatListAnswerForAnAdmin(t *testing.T) {
	e := newEnv(t, nil)
	s := e.session(t, e.admin)
	e.built(t, "smoke", map[string]any{"TIER": "full"})
	e.built(t, "nohit", nil)

	var ov overviewOut
	ok(t, s, "overview", nil, &ov)
	if len(ov.Failing) != 1 || ov.Failing[0].Job != "nohit" || len(ov.Groups) == 0 {
		t.Fatalf("overview: %+v", ov)
	}

	var jobs listJobsOut
	ok(t, s, "list_jobs", nil, &jobs)
	if len(jobs.Jobs) != 6 {
		t.Fatalf("list_jobs: %+v", jobs)
	}
	ok(t, s, "list_jobs", map[string]any{"group": "backend"}, &jobs)
	if len(jobs.Jobs) != 2 || jobs.Jobs[0].Last == nil && jobs.Jobs[1].Last == nil {
		t.Fatalf("list_jobs by group: %+v", jobs)
	}

	var job getJobOut
	ok(t, s, "get_job", map[string]any{"job": "smoke"}, &job)
	if len(job.Params) != 1 || job.Params[0].Name != "TIER" || !slices.Equal(job.Params[0].Choices, []string{"quick", "full"}) || len(job.Stages) != 2 || len(job.RecentBuilds) != 1 {
		t.Fatalf("get_job: %+v", job)
	}

}

func TestEveryBuildToolAnswersForAnAdmin(t *testing.T) {
	e := newEnv(t, nil)
	s := e.session(t, e.admin)
	b := e.built(t, "smoke", map[string]any{"TIER": "full"})
	var list listBuildsOut
	ok(t, s, "list_builds", map[string]any{"job": "smoke"}, &list)
	if len(list.Builds) != 1 || list.Builds[0].Status != "success" {
		t.Fatalf("list_builds: %+v", list)
	}

	var got getBuildOut
	ok(t, s, "get_build", map[string]any{"job": "smoke", "number": b.Number}, &got)
	if got.Params["TIER"] != "full" || len(got.Stages) != 2 || !got.Complete || got.URL != "https://ci.example.com/jobs/smoke/1" {
		t.Fatalf("get_build: %+v", got)
	}

	var log readLogOut
	ok(t, s, "read_log", map[string]any{"job": "smoke", "number": b.Number, "stage": "s"}, &log)
	if !strings.Contains(log.Text, "tier=full") || log.Truncated || log.Size == 0 {
		t.Fatalf("read_log: %+v", log)
	}
}

func TestTheToolsOnABuildsFilesAndTestsAnswerForAnAdmin(t *testing.T) {
	e := newEnv(t, nil)
	s := e.session(t, e.admin)
	b := e.built(t, "smoke", nil)
	if err := e.st.PutTests(context.Background(), b.ID, []store.Test{{Stage: "s", Suite: "a", Name: "ok", Status: "passed"}, {Stage: "s", Suite: "a", Name: "bad", Status: "failed", Message: "boom"}, {Stage: "s", Name: "later", Status: "skipped"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.PutArtifacts(context.Background(), b.ID, "s", []store.Artifact{{Stage: "s", Path: "out/report.txt", Size: 12, SHA256: "ab"}}); err != nil {
		t.Fatal(err)
	}
	var tests testResultsOut
	ok(t, s, "test_results", map[string]any{"job": "smoke", "number": b.Number}, &tests)
	if tests.Total != 3 || tests.Failed != 1 || tests.Cases[0].Name != "bad" || tests.Cases[0].Message != "boom" || tests.Cases[1].Status != "skipped" {
		t.Fatalf("test_results (failures first): %+v", tests)
	}
	ok(t, s, "test_results", map[string]any{"job": "smoke", "number": b.Number, "limit": 1}, &tests)
	if len(tests.Cases) != 1 || !tests.Cut {
		t.Fatalf("test_results limit: %+v", tests)
	}

	var files listArtifactsOut
	ok(t, s, "list_artifacts", map[string]any{"job": "smoke", "number": b.Number}, &files)
	if len(files.Files) != 1 || files.Files[0].Path != "out/report.txt" || files.Files[0].Size != 12 {
		t.Fatalf("list_artifacts: %+v", files)
	}

	var q queueOut
	ok(t, s, "queue", nil, &q)
	if q.Executors == nil || *q.Executors != 4 {
		t.Fatalf("queue: %+v", q)
	}
}

func TestEveryStartingToolAnswersForAnAdmin(t *testing.T) {
	e := newEnv(t, nil)
	s := e.session(t, e.admin)
	b := e.built(t, "smoke", map[string]any{"TIER": "full"})
	var again, started buildOut
	ok(t, s, "build_again", map[string]any{"job": "smoke", "number": b.Number}, &again)
	if again.Number != 2 || again.Params["TIER"] != "full" || again.StartedBy != "admin" || again.Trigger != "api" {
		t.Fatalf("build_again: %+v", again)
	}
	e.ended(t, "smoke", again.Number)
	ok(t, s, "run_build", map[string]any{"job": "slow"}, &started)
	var cancelled cancelOut
	ok(t, s, "cancel_build", map[string]any{"job": "slow", "number": started.Number}, &cancelled)
	if !cancelled.Cancelled {
		t.Fatalf("cancel_build: %+v", cancelled)
	}
	if end := e.ended(t, "slow", started.Number); end.Status != store.Cancelled {
		t.Fatalf("the build ended %s, not cancelled", end.Status)
	}
}

func TestGrantsDecideWhatEveryToolShows(t *testing.T) {
	e := newEnv(t, nil)
	b := e.built(t, "smoke", nil)
	other := e.built(t, "other", nil)
	s := e.session(t, e.viewer)

	var jobs listJobsOut
	ok(t, s, "list_jobs", nil, &jobs)
	if len(jobs.Jobs) != 1 || jobs.Jobs[0].Name != "smoke" {
		t.Fatalf("a viewer of smoke lists %+v", jobs)
	}
	var ov overviewOut
	ok(t, s, "overview", nil, &ov)
	if len(ov.Groups) != 1 || ov.Groups[0].Green+ov.Groups[0].Red+ov.Groups[0].NeverBuilt != 1 {
		t.Fatalf("the overview of a viewer of one job: %+v", ov.Groups)
	}

	hidden := map[string]any{"job": "other", "number": other.Number}
	for _, tool := range []string{"get_build", "test_results", "list_artifacts", "build_again", "cancel_build"} {
		refused(t, s, tool, hidden, "not found")
	}
	refused(t, s, "read_log", map[string]any{"job": "other", "number": other.Number, "stage": "s"}, "not found")
	refused(t, s, "get_job", map[string]any{"job": "other"}, "not found")
	refused(t, s, "list_builds", map[string]any{"job": "other"}, "not found")
	refused(t, s, "run_build", map[string]any{"job": "other"}, "not found")

	// A job that is hidden answers as one that does not exist.
	refused(t, s, "get_job", map[string]any{"job": "nothing-like-it"}, "not found")

	// Seen but not built.
	refused(t, s, "run_build", map[string]any{"job": "smoke"}, "not allowed")
	refused(t, s, "build_again", map[string]any{"job": "smoke", "number": b.Number}, "not allowed")
	refused(t, s, "cancel_build", map[string]any{"job": "smoke", "number": b.Number}, "not allowed")

	// build lets a user run, and so see, smoke; not cancel.
	bs := e.session(t, e.builder)
	var started buildOut
	ok(t, bs, "run_build", map[string]any{"job": "smoke"}, &started)
	if started.StartedBy != "builder" {
		t.Fatalf("a build started through MCP is recorded under %q", started.StartedBy)
	}
	e.ended(t, "smoke", started.Number)
	refused(t, bs, "cancel_build", map[string]any{"job": "smoke", "number": started.Number}, "not allowed")

	// cancel on slow stops a build somebody else started.
	var slow buildOut
	ok(t, e.session(t, e.admin), "run_build", map[string]any{"job": "slow"}, &slow)
	ok(t, e.session(t, e.stopper), "cancel_build", map[string]any{"job": "slow", "number": slow.Number}, nil)
	e.ended(t, "slow", slow.Number)
}

func TestAViewerOfNothingSeesAnEmptyQueueAndNoJobs(t *testing.T) {
	e := newEnv(t, nil)
	nobody := e.token(t, "nobody", false, nil, nil)
	s := e.session(t, nobody)
	var jobs listJobsOut
	ok(t, s, "list_jobs", nil, &jobs)
	var q queueOut
	ok(t, s, "queue", nil, &q)
	if len(jobs.Jobs) != 0 || len(q.Running)+len(q.Queued) != 0 {
		t.Fatalf("%+v %+v", jobs, q)
	}
}

func TestACeilingBelowTheGrantsStopsATool(t *testing.T) {
	// The gate in front would answer these calls first; the tool's own
	// check is what this test is about.
	defer func(m map[string]string) { toolActions = m }(toolActions)
	toolActions = nil
	var au *auth.Service
	readOnly := func(ctx context.Context, token string) (*Caller, error) {
		c, err := APITokens(au)(ctx, token)
		if c != nil {
			c.Ceiling = Ceiling{View: true}
		}
		return c, err
	}
	e := newEnv(t, readOnly)
	au = e.au
	s := e.session(t, e.admin)
	var jobs listJobsOut
	ok(t, s, "list_jobs", nil, &jobs)
	if len(jobs.Jobs) == 0 {
		t.Fatal("a read-only ceiling hides what it allows to see")
	}
	refused(t, s, "run_build", map[string]any{"job": "smoke"}, "not allowed")
	refused(t, s, "cancel_build", map[string]any{"job": "smoke", "number": 1}, "not allowed")

	noView := func(ctx context.Context, token string) (*Caller, error) {
		c, err := APITokens(au)(ctx, token)
		if c != nil {
			c.Ceiling = Ceiling{}
		}
		return c, err
	}
	e2 := newEnv(t, noView)
	au = e2.au
	s2 := e2.session(t, e2.admin)
	ok(t, s2, "list_jobs", nil, &jobs)
	refused(t, s2, "get_job", map[string]any{"job": "smoke"}, "not found")
	if len(jobs.Jobs) != 0 {
		t.Fatalf("a ceiling with no view lists %+v", jobs)
	}
}

func TestReadLogTailsByLinesAndPagesByBytes(t *testing.T) {
	e := newEnv(t, nil)
	b := e.built(t, "long", nil)
	s := e.session(t, e.admin)
	ref := map[string]any{"job": "long", "number": b.Number, "stage": "s"}

	var tail readLogOut
	ok(t, s, "read_log", ref, &tail)
	lines := strings.Split(strings.TrimSuffix(tail.Text, "\n"), "\n")
	if len(lines) != 200 || lines[0] != "line 2801" || lines[199] != "line 3000" || !tail.Truncated || tail.Next != tail.Size || tail.Offset == 0 {
		t.Fatalf("default tail: %d lines from %q to %q, %+v", len(lines), lines[0], lines[len(lines)-1], tail)
	}
	ref["tail_lines"] = 3
	ok(t, s, "read_log", ref, &tail)
	if tail.Text != "line 2998\nline 2999\nline 3000\n" {
		t.Fatalf("three lines: %q", tail.Text)
	}
	ref["tail_lines"] = 2000
	ok(t, s, "read_log", ref, &tail)
	if n := strings.Count(tail.Text, "\n"); n != 2000 || tail.Size == 0 || len(tail.Text) > maxBytes {
		t.Fatalf("2000 lines: %d, %d bytes", n, len(tail.Text))
	}

	delete(ref, "tail_lines")
	ref["offset"], ref["limit_bytes"] = 0, 10
	var head readLogOut
	ok(t, s, "read_log", ref, &head)
	if head.Text != "line 1\nlin" || head.Offset != 0 || head.Next != 10 || !head.Truncated {
		t.Fatalf("a range: %+v", head)
	}
	ref["offset"], ref["limit_bytes"] = tail.Size-5, 1000
	ok(t, s, "read_log", ref, &head)
	if head.Next != tail.Size || head.Truncated && head.Next < head.Size {
		t.Fatalf("the end of a range: %+v", head)
	}

	for what, args := range map[string]map[string]any{
		"tail_lines over the max":  {"tail_lines": maxTail + 1},
		"limit_bytes over the max": {"offset": 0, "limit_bytes": maxBytes + 1},
		"negative offset":          {"offset": -1},
		"no stage":                 {"stage": ""},
	} {
		a := map[string]any{"job": "long", "number": b.Number, "stage": "s"}
		maps.Copy(a, args)
		if msg := call(t, s, "read_log", a, nil); msg == "" {
			t.Errorf("%s was taken", what)
		}
	}
	refused(t, s, "read_log", map[string]any{"job": "long", "number": b.Number, "stage": "nope"}, "not found")
}

func TestReadLogShowsASecretMasked(t *testing.T) {
	e := newEnv(t, nil)
	b := e.built(t, "secret", nil)
	var log readLogOut
	ok(t, e.session(t, e.admin), "read_log", map[string]any{"job": "secret", "number": b.Number, "stage": "s"}, &log)
	if strings.Contains(log.Text, secretValue) || !strings.Contains(log.Text, "key=***") {
		t.Fatalf("the log shows %q", log.Text)
	}
}

func TestRunBuildPassesParamsAndRefusesUnknownOnes(t *testing.T) {
	e := newEnv(t, nil)
	s := e.session(t, e.admin)
	var b buildOut
	ok(t, s, "run_build", map[string]any{"job": "smoke", "params": map[string]any{"TIER": "full"}}, &b)
	if b.Params["TIER"] != "full" {
		t.Fatalf("%+v", b)
	}
	end := e.ended(t, "smoke", b.Number)
	if end.Params["TIER"] != "full" {
		t.Fatalf("the build ran with %v", end.Params)
	}
	refused(t, s, "run_build", map[string]any{"job": "smoke", "params": map[string]any{"NOPE": "x"}}, "bad input")
	refused(t, s, "run_build", map[string]any{"job": "smoke", "params": map[string]any{"TIER": "huge"}}, "bad input")
}

func TestListBuildsPagesAndRefusesAnOversizeLimit(t *testing.T) {
	e := newEnv(t, nil)
	for range 3 {
		e.built(t, "other", nil)
	}
	s := e.session(t, e.admin)
	var page listBuildsOut
	ok(t, s, "list_builds", map[string]any{"job": "other", "limit": 2}, &page)
	if len(page.Builds) != 2 || page.Builds[0].Number != 3 || page.NextCursor != "2" {
		t.Fatalf("%+v", page)
	}
	cursor := page.NextCursor
	page = listBuildsOut{}
	ok(t, s, "list_builds", map[string]any{"job": "other", "limit": 2, "cursor": cursor}, &page)
	if len(page.Builds) != 1 || page.Builds[0].Number != 1 || page.NextCursor != "" {
		t.Fatalf("%+v", page)
	}
	refused(t, s, "list_builds", map[string]any{"job": "other", "limit": maxBuilds + 1}, "limit")
	refused(t, s, "list_builds", map[string]any{"job": "other", "cursor": "x"}, "cursor")
}

func TestGetBuildForANumberThatDoesNotExistIsNotFound(t *testing.T) {
	e := newEnv(t, nil)
	s := e.session(t, e.admin)
	refused(t, s, "get_build", map[string]any{"job": "smoke", "number": 99}, "not found")
	refused(t, s, "get_build", map[string]any{"job": "smoke", "number": 0}, "number")
}

func TestAnAnswerClipsALongTestMessage(t *testing.T) {
	if got := clip(strings.Repeat("é", 400), 5); got != "éé..." {
		t.Fatalf("%q", got)
	}
	if clip("short", 10) != "short" {
		t.Fatal("a short text was changed")
	}
}

func TestTheTokenIsCheckedOnEveryRequest(t *testing.T) {
	e := newEnv(t, nil)
	past := time.Now().Add(-time.Hour)
	expired := e.token(t, "old", true, nil, &past)
	gone := e.token(t, "gone", true, nil, nil)
	u, err := e.st.UserByName(context.Background(), "gone")
	if err != nil {
		t.Fatal(err)
	}
	toks, err := e.st.Tokens(context.Background(), u.ID)
	if err != nil || len(toks) != 1 {
		t.Fatalf("%v %v", toks, err)
	}
	live := e.session(t, gone)
	ok(t, live, "queue", nil, nil)
	if err := e.st.DeleteToken(context.Background(), toks[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := live.CallTool(context.Background(), &sdk.CallToolParams{Name: "queue"}); err == nil {
		t.Fatal("a revoked token still worked on a connection made before")
	}

	for what, c := range map[string]struct {
		header string
		cookie string
	}{
		"no token":                        {},
		"garbage":                         {header: "Bearer kipp_garbage"},
		"wrong scheme":                    {header: "Basic abc"},
		"revoked":                         {header: "Bearer " + gone},
		"expired":                         {header: "Bearer " + expired},
		"cookie alone":                    {cookie: "__Host-kipp_session=whatever"},
		"empty bearer":                    {header: "Bearer "},
		"two words":                       {header: "Bearer a b"},
		"admin as cookie plus bad bearer": {header: "Bearer nope", cookie: "kipp_local_session=x"},
	} {
		res := e.rawCall(t, c.header, c.cookie)
		if res.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(res.Header.Get("WWW-Authenticate"), "Bearer") {
			t.Errorf("%s: %d, WWW-Authenticate %q", what, res.StatusCode, res.Header.Get("WWW-Authenticate"))
		}
	}
}

// rawCall posts a tools/list with the given Authorization and Cookie headers.
func (e *env) rawCall(t *testing.T, header, cookie string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+Path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if header != "" {
		req.Header.Set("Authorization", header)
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	return res
}

func TestABodyOverTheApisLimitIsRefused(t *testing.T) {
	e := newEnv(t, nil)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"pad":"` + strings.Repeat("x", api.MaxBody) + `"}}`
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+Path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.admin)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 400 {
		t.Fatalf("a body of %d bytes was taken: %s", len(body), res.Status)
	}
}

func TestAServerFaultInTheVerifierIsNotTheCallersToKnow(t *testing.T) {
	e := newEnv(t, func(context.Context, string) (*Caller, error) { return nil, fmt.Errorf("disk on fire at /secret/path") })
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+Path, strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer x")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusInternalServerError || strings.Contains(string(body), "disk") {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
}

// call posts one JSON-RPC body with the bearer token.
func (e *env) post(t *testing.T, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+Path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	return res
}

const metadataURL = "https://ci.example.com/.well-known/oauth-protected-resource/mcp"

func TestACallTheCeilingForbidsIsA403ThatNamesTheScopeNeeded(t *testing.T) {
	var au *auth.Service
	e := newEnvAt(t, func(ctx context.Context, token string) (*Caller, error) {
		c, err := APITokens(au)(ctx, token)
		if c != nil {
			c.Ceiling = Ceiling{View: true}
		}
		return c, err
	}, metadataURL)
	au = e.au
	call := func(name string) string {
		return `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":{"job":"smoke"}}}`
	}
	for _, tool := range []string{"run_build", "build_again", "cancel_build"} {
		res := e.post(t, e.admin, call(tool))
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("%s: status %d", tool, res.StatusCode)
		}
		want := `Bearer error="insufficient_scope", scope="kipp:build", resource_metadata="` + metadataURL + `"`
		if got := res.Header.Get("WWW-Authenticate"); got != want {
			t.Errorf("%s: WWW-Authenticate %q", tool, got)
		}
	}
	// In a batch too.
	res := e.post(t, e.admin, "["+call("overview")+","+call("run_build")+"]")
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("a batch: status %d", res.StatusCode)
	}
	// What the ceiling allows goes through, and so does a tool of the same name in another method.
	for _, body := range []string{call("get_job"), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, `{"jsonrpc":"2.0","id":1,"method":"prompts/get","params":{"name":"run_build"}}`} {
		if res := e.post(t, e.admin, body); res.StatusCode == http.StatusForbidden {
			t.Errorf("%s was forbidden", body)
		}
	}
}

func TestAnAPITokenIsNeverStoppedByTheScopeGate(t *testing.T) {
	e := newEnv(t, nil)
	res := e.post(t, e.admin, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"run_build","arguments":{"job":"smoke"}}}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
}

func TestA401PointsToTheResourceMetadataOnlyWhenOAuthIsOn(t *testing.T) {
	for metadata, want := range map[string]string{"": "Bearer", metadataURL: `Bearer resource_metadata="` + metadataURL + `"`} {
		e := newEnvAt(t, nil, metadata)
		res := e.rawCall(t, "", "")
		if res.StatusCode != http.StatusUnauthorized || res.Header.Get("WWW-Authenticate") != want {
			t.Errorf("with %q: %d, %q", metadata, res.StatusCode, res.Header.Get("WWW-Authenticate"))
		}
		res = e.rawCall(t, "Bearer kipp_garbage", "")
		if res.StatusCode != http.StatusUnauthorized || res.Header.Get("WWW-Authenticate") != want {
			t.Errorf("a bad token with %q: %d, %q", metadata, res.StatusCode, res.Header.Get("WWW-Authenticate"))
		}
	}
}
