package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	openapi "github.com/ndthuan/kipp/api"
	"github.com/ndthuan/kipp/internal/apispec/apispectest"
	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/dispatch"
	"github.com/ndthuan/kipp/internal/logtime"
	"github.com/ndthuan/kipp/internal/runner"
	"github.com/ndthuan/kipp/internal/secretbox"
	"github.com/ndthuan/kipp/internal/store"
)

const (
	sseAccept = "text/event-stream"
	sseEnd    = "event: end\ndata: \n\n"
)

var jobFiles = map[string]string{
	"smoke.yml": "name: smoke\ngroup: backend\nparams:\n  TIER: {type: choice, options: [quick, full], description: How much to run.}\nstages:\n  s: {run: 'echo tier=$TIER; sleep 0.3; echo done'}\n",
	"gc.yml":    "name: gc\ntriggers: {cron: '*/10 * * * *'}\nstages:\n  s: {run: echo gc}\n",
	"slow.yml":  "name: slow\nstages:\n  s: {run: 'echo started; sleep 30'}\n",
	"once.yml":  "name: once\nconcurrency: drop\nstages:\n  s: {run: 'echo started; sleep 30'}\n",
	"cr.yml":    "name: cr\nstages:\n  s: {run: 'printf \"10%%\\r20%%\\revent: end\\nlast\"'}\n",
}

type env struct {
	srv    *httptest.Server
	st     *store.Store
	api    *API
	client *http.Client
	dir    string
	active atomic.Int64 // handlers that have not returned
	admin  string       // tokens
	agent  string       // build on smoke
	viewer string       // view on smoke
	// reloadErr is what a reload fails with; nil, the default, is one
	// that is taken.
	reloadErr atomic.Pointer[error]
}

// testSecretKey seals the secrets of a test's server.
var testSecretKey = []byte("an obvious test secret key 32 b.")

func newEnv(t *testing.T, tune ...func(*API)) *env {
	t.Helper()
	dir := t.TempDir()
	jobs := filepath.Join(dir, "jobs")
	must(t, os.MkdirAll(jobs, 0o750))
	for name, body := range jobFiles {
		must(t, os.WriteFile(filepath.Join(jobs, name), []byte(body), 0o600))
	}
	snap, err := config.Load(jobs)
	must(t, err)
	st, err := store.Open(filepath.Join(dir, "db"))
	must(t, err)
	quiet := slog.New(slog.DiscardHandler)
	ctx, stop := context.WithCancel(context.Background())
	d := dispatch.New(st, runner.Local{}, dispatch.Options{DataDir: dir, Logger: quiet})
	d.SetSnapshot(snap)
	stopped := make(chan struct{})
	go func() { d.Run(ctx); close(stopped) }()
	// The timeout turns a stream or a long-poll that never ends into a failure.
	e := &env{st: st, dir: dir, client: &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}}
	e.api = New(Deps{Auth: auth.New(st, time.Hour, []byte("an obvious test device key 32 b.")), Dispatch: d, Store: st, Version: "test", Logger: quiet,
		Reload: func(context.Context) error {
			if err := e.reloadErr.Load(); err != nil {
				return *err
			}
			return nil
		},
		Seal: func(name string, value []byte) ([]byte, error) { return secretbox.Seal(testSecretKey, name, value) },
		ServerInfo: func() ServerInfo {
			return ServerInfo{Version: "test", Listen: "127.0.0.1:8085", Executors: 4, DataDir: dir}
		},
		Listen: "127.0.0.1:0",
	})
	for _, f := range tune {
		f(e.api)
	}
	h := apispectest.Wrap(t, e.api.Handler())
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.active.Add(1)
		defer e.active.Add(-1)
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { stop(); <-stopped; e.srv.Close(); _ = st.Close() })
	e.admin = token(t, st, "admin", true, nil)
	e.agent = token(t, st, "agent", false, map[string][]string{"smoke": {"build"}})
	e.viewer = token(t, st, "viewer", false, map[string][]string{"smoke": {"view"}})
	return e
}

func token(t *testing.T, st *store.Store, name string, admin bool, grants map[string][]string) string {
	t.Helper()
	ctx := context.Background()
	u, err := st.CreateUser(ctx, name, "x", admin)
	must(t, err)
	for job, perms := range grants {
		must(t, st.Grant(ctx, u.ID, job, perms))
	}
	plain, hash, err := auth.NewToken()
	must(t, err)
	_, err = st.CreateToken(ctx, u.ID, "test", hash)
	must(t, err)
	return plain
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func eq[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
}

func contains(t *testing.T, s, sub string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Fatalf("%q is not in %q", sub, s)
	}
}

func (e *env) user(t *testing.T, name string) *store.User {
	t.Helper()
	u, err := e.st.UserByName(context.Background(), name)
	must(t, err)
	return u
}

// request builds a call; hdr is name, value pairs.
func (e *env) request(ctx context.Context, t *testing.T, tok, method, path string, body io.Reader, hdr ...string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, e.srv.URL+path, body)
	must(t, err)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	return req
}

// send is one call with its whole body.
func (e *env) send(t *testing.T, req *http.Request) (int, string, http.Header) {
	t.Helper()
	res, err := e.client.Do(req)
	must(t, err)
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(res.Body)
	must(t, err)
	return res.StatusCode, string(data), res.Header
}

func (e *env) raw(t *testing.T, tok, method, path, body string, hdr ...string) (int, string, http.Header) {
	t.Helper()
	return e.send(t, e.request(context.Background(), t, tok, method, path, strings.NewReader(body), hdr...))
}

// call is raw that must answer with the status code want.
func (e *env) call(t *testing.T, want int, tok, method, path, body string, hdr ...string) (string, http.Header) {
	t.Helper()
	code, text, h := e.raw(t, tok, method, path, body, hdr...)
	if code != want {
		t.Fatalf("%s %s: got %d, want %d: %s", method, path, code, want, text)
	}
	return text, h
}

// object is call with the JSON object decoded.
func (e *env) object(t *testing.T, want int, tok, method, path, body string, hdr ...string) map[string]any {
	t.Helper()
	text, _ := e.call(t, want, tok, method, path, body, hdr...)
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("%s %s: %v: %q", method, path, err, text)
	}
	return out
}

// list is the rows of the first page of a list.
func (e *env) list(t *testing.T, tok, path string) []map[string]any {
	t.Helper()
	rows, _ := e.page(t, tok, path)
	return rows
}

// trigger starts a build as the admin and returns its id.
func (e *env) trigger(t *testing.T, job, body string) string {
	t.Helper()
	return jsonInt(e.object(t, 201, e.admin, "POST", "/api/v1/jobs/"+job+"/builds", body)["id"])
}

func (e *env) finished(t *testing.T, id string) map[string]any {
	t.Helper()
	return e.object(t, 200, e.admin, "GET", "/api/v1/builds/"+id+"?wait=true", "")
}

// started triggers a build that sleeps and returns once its stage has printed.
func (e *env) started(t *testing.T, job string) string {
	t.Helper()
	id := e.trigger(t, job, "")
	eventually(t, func() bool {
		text, _ := e.call(t, 200, e.admin, "GET", "/api/v1/builds/"+id+"/stages/s/log", "")
		return strings.Contains(text, "started")
	})
	return id
}

func eventually(t *testing.T, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the condition never held")
		}
	}
}

// settles fails unless every handler returns and no goroutine outlives them.
func (e *env) settles(t *testing.T, baseline int) {
	t.Helper()
	busy := func() bool { return e.active.Load() != 0 || runtime.NumGoroutine() > baseline }
	for deadline := time.Now().Add(10 * time.Second); busy(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Fatalf("handlers in flight: %d, goroutines: %d, baseline %d\n%s", e.active.Load(), runtime.NumGoroutine(), baseline, buf[:runtime.Stack(buf, true)])
		}
	}
}

func jsonInt(v any) string { return strconv.Itoa(int(v.(float64))) }

func TestHealthNeedsNoLoginAndEverythingElseDoes(t *testing.T) {
	e := newEnv(t)
	text, _ := e.call(t, 200, "", "GET", "/api/v1/health", "")
	eq(t, "anonymous health", strings.TrimSpace(text), `{"ok":true}`)
	routes := [][2]string{
		{"GET", "/api/v1/jobs"}, {"GET", "/api/v1/jobs/smoke"}, {"GET", "/api/v1/jobs/smoke/builds"}, {"POST", "/api/v1/jobs/smoke/builds"},
		{"GET", "/api/v1/jobs/smoke/activity"}, {"GET", "/api/v1/builds/1"}, {"POST", "/api/v1/builds/1/cancel"},
		{"GET", "/api/v1/builds/1/stages/s/log"}, {"GET", "/api/v1/builds/1/results"}, {"POST", "/api/v1/admin/reload"},
		{"GET", "/api/v1/builds/1/metrics"}, {"GET", "/api/v1/metrics"}, {"GET", "/api/v1/metrics/smoke"},
		{"GET", "/api/v1/metrics/smoke/history"}, {"GET", "/api/v1/metrics/smoke/compare"},
	}
	for _, r := range routes {
		for _, tok := range []string{"", "kipp_wrong"} {
			if e.object(t, 401, tok, r[0], r[1], "")["detail"] == nil {
				t.Fatal(r, "no error in the body")
			}
		}
	}
}

func TestANarrowTokenSeesOneJobAndGets404ForTheRest(t *testing.T) {
	e := newEnv(t)
	list := e.list(t, e.agent, "/api/v1/jobs")
	eq(t, "jobs the agent sees", len(list), 1)
	eq(t, "name", list[0]["name"], any("smoke"))
	eq(t, "group", list[0]["group"], any("backend"))
	eq(t, "error", e.object(t, 404, e.agent, "GET", "/api/v1/jobs/gc", "")["detail"], any("not found"))
	e.call(t, 404, e.agent, "POST", "/api/v1/jobs/gc/builds", "{}")
	gc := e.object(t, 200, e.admin, "GET", "/api/v1/jobs/gc", "")
	eq(t, "cron", gc["cron"], any("*/10 * * * *"))
	eq(t, "concurrency", gc["concurrency"], any("queue"))
	if gc["next"] == nil {
		t.Fatal("a cron job has a next fire", gc)
	}
	eq(t, "jobs the admin sees", len(e.list(t, e.admin, "/api/v1/jobs")), len(jobFiles))
}

func TestAHiddenJobAnswersExactlyLikeAMissingOne(t *testing.T) {
	e := newEnv(t)
	hidden := e.trigger(t, "gc", "")
	e.finished(t, hidden)
	routes := []struct{ method, path, accept string }{
		{"GET", "/api/v1/jobs/%s", ""},
		{"GET", "/api/v1/jobs/%s/builds", ""},
		{"POST", "/api/v1/jobs/%s/builds", ""},
		{"GET", "/api/v1/jobs/%s/activity", ""},
		{"GET", "/api/v1/builds/%s", ""},
		{"GET", "/api/v1/builds/%s?wait=true", ""},
		{"POST", "/api/v1/builds/%s/cancel", ""},
		{"GET", "/api/v1/builds/%s/stages/s/log", ""},
		{"GET", "/api/v1/builds/%s/stages/s/log/events", sseAccept},
		{"GET", "/api/v1/builds/%s/results", ""},
		{"GET", "/api/v1/builds/%s/metrics", ""},
		{"GET", "/api/v1/metrics/%s", ""},
		{"GET", "/api/v1/metrics/%s/history?metric=x&item=y", ""},
		{"GET", "/api/v1/metrics/%s/compare?metric=x&from=1&to=remembered", ""},
	}
	for _, r := range routes {
		there, missing := "gc", "nope"
		if strings.Contains(r.path, "/builds/%s") {
			there, missing = hidden, "99999"
		}
		for _, tok := range []string{e.agent, e.viewer} {
			body, h := e.call(t, 404, tok, r.method, fmt.Sprintf(r.path, there), "{}", "Accept", r.accept)
			body2, h2 := e.call(t, 404, tok, r.method, fmt.Sprintf(r.path, missing), "{}", "Accept", r.accept)
			eq(t, r.method+" "+r.path+": body", body, body2)
			eq(t, r.method+" "+r.path+": content type", h.Get("Content-Type"), h2.Get("Content-Type"))
		}
		// The same path is a real route for whoever may see the job.
		if code, body, _ := e.raw(t, e.admin, r.method, fmt.Sprintf(r.path, there), "{}", "Accept", r.accept); code == 404 {
			t.Fatalf("%s %s as admin: %d %q", r.method, r.path, code, body)
		}
	}
	body, _ := e.call(t, 404, e.admin, "GET", "/api/v1/builds/abc", "")
	contains(t, body, `"not found"`)
}

func TestAJobOneMayViewButNotActOnIs403(t *testing.T) {
	e := newEnv(t)
	if e.object(t, 403, e.viewer, "POST", "/api/v1/jobs/smoke/builds", "{}")["detail"] == nil {
		t.Fatal("no error in the body")
	}
	eq(t, "builds after a refused trigger", len(e.list(t, e.admin, "/api/v1/jobs/smoke/builds")), 0)
	id := e.trigger(t, "smoke", "")
	for _, tok := range []string{e.viewer, e.agent} {
		e.call(t, 403, tok, "POST", "/api/v1/builds/"+id+"/cancel", "")
		eq(t, "job", e.object(t, 200, tok, "GET", "/api/v1/builds/"+id, "")["job"], any("smoke"))
	}
	eq(t, "status after a refused cancel", e.finished(t, id)["status"], any("success"))
}

func TestBuildWaitLogAndCancelPermissions(t *testing.T) {
	e := newEnv(t)
	b := e.object(t, 201, e.agent, "POST", "/api/v1/jobs/smoke/builds", `{"params": {"TIER": "full"}}`)
	eq(t, "number", b["number"], any(float64(1)))
	eq(t, "started_by", b["started_by"], any("agent"))
	eq(t, "trigger", b["trigger"], any("api"))
	// The agent may build but not cancel: 403, not 404 (it can see the job).
	e.call(t, 403, e.agent, "POST", "/api/v1/builds/1/cancel", "")
	done := e.object(t, 200, e.agent, "GET", "/api/v1/builds/1?wait=true", "")
	eq(t, "status", done["status"], any("success"))
	stages := done["stages"].([]any)
	eq(t, "stages", len(stages), 1)
	stage := stages[0].(map[string]any)
	eq(t, "stage name", stage["name"], any("s"))
	eq(t, "stage status", stage["status"], any("success"))
	eq(t, "stage exit code", stage["exit_code"], any(float64(0)))
	if done["finished_at"] == nil {
		t.Fatal("a finished build has no finished_at", done)
	}
	e.object(t, 403, e.agent, "POST", "/api/v1/admin/reload", "")
	e.object(t, 200, e.admin, "POST", "/api/v1/admin/reload", "")
	contains(t, e.object(t, 400, e.agent, "POST", "/api/v1/jobs/smoke/builds", `{"params": {"TIER": "nightly"}}`)["detail"].(string), "TIER")
	tests, _ := e.call(t, 200, e.agent, "GET", "/api/v1/builds/1/results", "")
	eq(t, "tests", strings.TrimSpace(tests), `{"items":[],"next_cursor":null}`)
	eq(t, "last build's status", e.list(t, e.agent, "/api/v1/jobs")[0]["last"].(map[string]any)["status"], any("success"))
	job := e.object(t, 200, e.agent, "GET", "/api/v1/jobs/smoke", "")
	param := job["params"].([]any)[0].(map[string]any)
	eq(t, "param name", param["name"], any("TIER"))
	eq(t, "param default", param["default"], any("quick"))
	eq(t, "param description", param["description"], any("How much to run."))
	eq(t, "builds on the job", len(job["builds"].(map[string]any)["items"].([]any)), 1)
}

func TestTheLogComesInChunksFromAnOffset(t *testing.T) {
	e := newEnv(t)
	id := e.trigger(t, "smoke", `{"params": {"TIER": "full"}}`)
	e.finished(t, id)
	for offset, want := range map[string]string{"0": "tier=full\ndone\n", "10": "done\n", "15": ""} {
		text, h := e.call(t, 200, e.agent, "GET", "/api/v1/builds/"+id+"/stages/s/log?offset="+offset, "")
		eq(t, "log from "+offset, text, want)
		eq(t, "next offset from "+offset, h.Get("X-Kipp-Offset"), "15")
		eq(t, "more from "+offset, h.Get("X-Kipp-More"), "false")
		eq(t, "content type", h.Get("Content-Type"), "text/plain; charset=utf-8")
	}
	for _, offset := range []string{"-3", "x"} {
		e.asProblem(t, 400, "bad_input", e.agent, "GET", "/api/v1/builds/"+id+"/stages/s/log?offset="+offset, "")
	}
	// A stage that has printed nothing yet has an empty log, not a missing one.
	text, h := e.call(t, 200, e.agent, "GET", "/api/v1/builds/"+id+"/stages/hooks/log", "")
	eq(t, "a log not written yet", text+h.Get("X-Kipp-Offset"), "0")
}

func TestAReloadThatFailsSaysWhy(t *testing.T) {
	e := newEnv(t, func(a *API) {
		a.deps.Reload = func(context.Context) error { return &JobsRefused{Err: errors.New("smoke.yml: no stages")} }
	})
	eq(t, "error", e.object(t, 422, e.admin, "POST", "/api/v1/admin/reload", "")["detail"], any("smoke.yml: no stages"))
}

func TestSSEStreamsTheLogToTheEnd(t *testing.T) {
	e := newEnv(t)
	baseline := runtime.NumGoroutine()
	id := e.trigger(t, "smoke", "{}")
	got, h := e.call(t, 200, e.admin, "GET", "/api/v1/builds/"+id+"/stages/s/log/events", "", "Accept", sseAccept)
	eq(t, "content type", h.Get("Content-Type"), sseAccept)
	eq(t, "stream", got, "id: 11\ndata: tier=quick\n\nid: 16\ndata: done\n\n"+sseEnd)
	e.finished(t, id)
	e.settles(t, baseline)
}

func TestAStreamResumesAfterTheLastLineTheClientHas(t *testing.T) {
	e := newEnv(t)
	id := e.trigger(t, "smoke", "")
	e.finished(t, id)
	path := "/api/v1/builds/" + id + "/stages/s/log/events"
	rest := "id: 16\ndata: done\n\n" + sseEnd
	// A browser that reconnects by itself sends the last id it saw.
	got, _ := e.call(t, 200, e.admin, "GET", path, "", "Accept", sseAccept, "Last-Event-ID", "11")
	eq(t, "after Last-Event-ID", got, rest)
	got, _ = e.call(t, 200, e.admin, "GET", path+"?offset=11", "", "Accept", sseAccept)
	eq(t, "from ?offset=", got, rest)
	got, _ = e.call(t, 200, e.admin, "GET", path+"?offset=0", "", "Accept", sseAccept, "Last-Event-ID", "16")
	eq(t, "with all of it", got, sseEnd)
}

func TestAQuietStreamSaysItIsAlive(t *testing.T) {
	e := newEnv(t, func(a *API) { a.logPoll, a.keepAlive = 10*time.Millisecond, 50*time.Millisecond })
	id := e.started(t, "slow")
	res, err := e.client.Do(e.request(t.Context(), t, e.admin, "GET", "/api/v1/builds/"+id+"/stages/s/log/events", nil, "Accept", sseAccept))
	must(t, err)
	defer func() { _ = res.Body.Close() }()
	// A comment line: an event-stream client drops it, a proxy sees traffic.
	want := "id: 8\ndata: started\n\n: keep-alive\n\n: keep-alive\n\n"
	got := make([]byte, len(want))
	_, err = io.ReadFull(res.Body, got)
	must(t, err)
	eq(t, "stream", string(got), want)
}

func TestSSECannotBeSteeredByWhatABuildPrints(t *testing.T) {
	e := newEnv(t)
	id := e.trigger(t, "cr", "")
	got, _ := e.call(t, 200, e.admin, "GET", "/api/v1/builds/"+id+"/stages/s/log/events", "", "Accept", sseAccept)
	// The ids are byte offsets into the log: 19 after the first newline, 23 at its end.
	eq(t, "stream", got, "data: 10%\n\ndata: 20%\n\nid: 19\ndata: event: end\n\nid: 23\ndata: last\n\n"+sseEnd)
}

func TestALongLogComesInBoundedChunks(t *testing.T) {
	e := newEnv(t)
	id := e.trigger(t, "smoke", "")
	e.finished(t, id)
	var want, events strings.Builder
	for i := 0; want.Len() < 5*logChunk/2; i++ {
		fmt.Fprintf(&want, "line %d of a long log\n", i)
		fmt.Fprintf(&events, "id: %d\ndata: line %d of a long log\n\n", want.Len(), i)
	}
	must(t, os.WriteFile(filepath.Join(e.dir, "logs", "smoke", "1", "s.log"), []byte(want.String()), 0o600))
	var got strings.Builder
	offset, more, calls := "0", "true", 0
	for ; more == "true" && calls < 10; calls++ {
		text, h := e.call(t, 200, e.admin, "GET", "/api/v1/builds/"+id+"/stages/s/log?offset="+offset, "")
		if len(text) > logChunk {
			t.Fatal("a chunk of", len(text))
		}
		got.WriteString(text)
		offset, more = h.Get("X-Kipp-Offset"), h.Get("X-Kipp-More")
	}
	eq(t, "calls", calls, 3)
	eq(t, "offset", offset, strconv.Itoa(want.Len()))
	eq(t, "the chunks are the log", got.String() == want.String(), true)
	stream, _ := e.call(t, 200, e.admin, "GET", "/api/v1/builds/"+id+"/stages/s/log/events", "", "Accept", sseAccept)
	eq(t, "the stream is the log", stream == events.String()+sseEnd, true)
}

func TestASessionNeedsTheCSRFTokenAndABearerTokenDoesNot(t *testing.T) {
	e := newEnv(t)
	sess := &store.Session{ID: "sess-live", UserID: e.user(t, "admin").ID, CSRF: "csrf-live", ExpiresAt: time.Now().Add(time.Hour)}
	must(t, e.st.CreateSession(context.Background(), sess))
	cookie := SessionCookieFor(sess).String()
	for _, m := range []string{"GET", "HEAD"} {
		e.call(t, 200, "", m, "/api/v1/jobs", "", "Cookie", cookie)
	}
	id := e.started(t, "slow")
	mutating := [][2]string{
		{"POST", "/api/v1/jobs/smoke/builds"}, {"POST", "/api/v1/builds/" + id + "/cancel"}, {"POST", "/api/v1/admin/reload"},
		{"PUT", "/api/v1/jobs/smoke"}, {"PATCH", "/api/v1/jobs/smoke"}, {"DELETE", "/api/v1/builds/" + id}, {"OPTIONS", "/api/v1/jobs"},
	}
	for _, r := range mutating {
		for _, csrf := range []string{"", "csrf-old", "csrf-liv", "csrf-live2"} {
			contains(t, e.object(t, 403, "", r[0], r[1], "{}", "Cookie", cookie, CSRFHeader, csrf)["detail"].(string), "CSRF")
		}
	}
	eq(t, "builds made without the CSRF token", len(e.list(t, e.admin, "/api/v1/jobs/smoke/builds")), 0)
	eq(t, "status after a cancel without the CSRF token", e.object(t, 200, e.admin, "GET", "/api/v1/builds/"+id, "")["status"], any("running"))

	b := e.object(t, 201, "", "POST", "/api/v1/jobs/smoke/builds", "{}", "Cookie", cookie, CSRFHeader, "csrf-live")
	eq(t, "started_by", b["started_by"], any("admin"))
	e.call(t, 202, "", "POST", "/api/v1/builds/"+id+"/cancel", "", "Cookie", cookie, CSRFHeader, "csrf-live")
	// A browser does not send a bearer token on its own, so it needs no CSRF token.
	e.call(t, 200, e.admin, "POST", "/api/v1/admin/reload", "")
}

func TestASessionNobodyHoldsIsNotLoggedIn(t *testing.T) {
	e := newEnv(t)
	old := &store.Session{ID: "sess-old", UserID: e.user(t, "admin").ID, CSRF: "csrf-old", ExpiresAt: time.Now().Add(-time.Minute)}
	must(t, e.st.CreateSession(context.Background(), old))
	for _, c := range []string{SessionCookieFor(old).String(), SessionCookie + "=nobody", "kipp_session=sess-old"} {
		e.call(t, 401, "", "GET", "/api/v1/jobs", "", "Cookie", c)
		e.call(t, 401, "", "POST", "/api/v1/admin/reload", "", "Cookie", c, CSRFHeader, "csrf-old")
	}
}

func TestTheSessionCookieIsHostBoundSecureAndHttpOnly(t *testing.T) {
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	rec := httptest.NewRecorder()
	http.SetCookie(rec, SessionCookieFor(&store.Session{ID: "abc", CSRF: "never-in-the-cookie", ExpiresAt: exp}))
	http.SetCookie(rec, ExpiredSessionCookie())
	cookies := rec.Result().Cookies()
	eq(t, "cookies", len(cookies), 2)
	for _, c := range cookies {
		// What a browser demands of the __Host- prefix: Secure, Path=/, no Domain.
		eq(t, "name", c.Name, "__Host-kipp_session")
		eq(t, "secure", c.Secure, true)
		eq(t, "http only", c.HttpOnly, true)
		eq(t, "same site", c.SameSite, http.SameSiteLaxMode)
		eq(t, "path", c.Path, "/")
		eq(t, "domain", c.Domain, "")
	}
	eq(t, "value", cookies[0].Value, "abc")
	eq(t, "expires", cookies[0].Expires.Equal(exp), true)
	eq(t, "value after logout", cookies[1].Value, "")
	eq(t, "max age after logout", cookies[1].MaxAge, -1)
	eq(t, "names", SessionCookie+" "+CSRFHeader, "__Host-kipp_session X-CSRF-Token")
}

const secret = "TOPSECRET"

// logsWithSecretsAround is a finished smoke build with a file holding secret
// at every place a stage name might be bent to reach. It returns the build's
// id and a path outside the data directory.
func logsWithSecretsAround(t *testing.T, e *env) (string, string) {
	t.Helper()
	id := e.trigger(t, "smoke", "")
	e.finished(t, id)
	e.finished(t, e.trigger(t, "gc", ""))
	logs := filepath.Join(e.dir, "logs")
	outside := filepath.Join(t.TempDir(), "passwd")
	for _, p := range []string{
		filepath.Join(logs, "smoke", "secret.log"), filepath.Join(logs, "secret.log"), filepath.Join(e.dir, "secret.log"),
		filepath.Join(logs, "gc", "1", "s.log"), outside + ".log", outside,
	} {
		must(t, os.WriteFile(p, []byte(secret), 0o600))
	}
	return id, outside
}

func TestTheLogEndpointReadsNothingOutsideTheBuildsLogs(t *testing.T) {
	e := newEnv(t)
	id, outside := logsWithSecretsAround(t, e)
	stages := []string{
		"../secret", "../../secret", "../../../secret", "../../gc/1/s", "../1/s", "./s", "s/../s", "..", ".",
		outside, strings.TrimPrefix(outside, "/"), `..\secret`, "s\x00", "s.log\x00../secret", " s", "S", "-s", ".s",
	}
	for _, stage := range stages {
		for _, enc := range []string{escapeAll(stage, "%%%02X"), escapeAll(stage, "%%%02x")} {
			for _, form := range []string{"", "/events"} {
				code, body, _ := e.raw(t, e.admin, "GET", "/api/v1/builds/"+id+"/stages/"+enc+"/log"+form, "")
				if strings.Contains(body, secret) || code == 200 {
					t.Fatalf("stage %q as %s: %d %q", stage, enc, code, body)
				}
			}
		}
	}
	// The encoded slash does reach the handler: this is its answer, not the mux's.
	eq(t, "error", e.object(t, 404, e.admin, "GET", "/api/v1/builds/"+id+"/stages/..%2Fsecret/log", "")["detail"], any("not found"))
}

func TestLogPathIsAlwaysAFileInTheBuildsOwnDirectory(t *testing.T) {
	e := newEnv(t)
	id, outside := logsWithSecretsAround(t, e)
	build, err := strconv.ParseInt(id, 10, 64)
	must(t, err)
	svc, admin := e.api.Service(), e.user(t, "admin")
	for _, stage := range []string{
		"", "../secret", "../../secret", "../../gc/1/s", "../1/s", "./s", "s/../s", "s/", "/s", "..", ".",
		outside, `..\secret`, "s\x00", "s\n", " s", "S", "-s", ".s", "s.log/../../secret",
	} {
		p, _, err := svc.LogPath(context.Background(), admin, build, stage)
		if !errors.Is(err, ErrNotFound) || p != "" {
			t.Fatalf("stage %q: path %q, err %v", stage, p, err)
		}
	}
	dir := filepath.Join(e.dir, "logs", "smoke", "1")
	for _, stage := range []string{"s", "unit-tests", "a.b_c", "prepare", "hooks"} {
		p, _, err := svc.LogPath(context.Background(), admin, build, stage)
		must(t, err)
		eq(t, "path of "+stage, p, filepath.Join(dir, stage+".log"))
	}
}

func TestALinkInTheLogDirectoryIsNotFollowedOutOfIt(t *testing.T) {
	e := newEnv(t)
	id, _ := logsWithSecretsAround(t, e)
	must(t, os.Symlink(filepath.Join(e.dir, "secret.log"), filepath.Join(e.dir, "logs", "smoke", "1", "evil.log")))
	for _, form := range []string{"", "/events"} {
		_, body, _ := e.raw(t, e.admin, "GET", "/api/v1/builds/"+id+"/stages/evil/log"+form, "")
		if strings.Contains(body, secret) {
			t.Fatalf("followed the link: %q", body)
		}
	}
}

// escapeAll encodes, with format, every byte a path segment could be split
// or cleaned on.
func escapeAll(s, format string) string {
	var b strings.Builder
	for i := range len(s) {
		if c := s[i]; strings.IndexByte(`/.\ `, c) >= 0 || c < 0x20 {
			fmt.Fprintf(&b, format, c)
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

func TestARequestBodyIsBounded(t *testing.T) {
	e := newEnv(t)
	big := `{"params": {"TIER": "` + strings.Repeat("a", 2*MaxBody) + `"}}`
	post := func(body io.Reader) (int, string) {
		t.Helper()
		code, text, _ := e.send(t, e.request(context.Background(), t, e.admin, "POST", "/api/v1/jobs/smoke/builds", body))
		return code, text
	}
	// io.MultiReader hides the length, so the body goes out chunked.
	for name, body := range map[string]io.Reader{"sized": strings.NewReader(big), "chunked": io.MultiReader(strings.NewReader(big))} {
		code, text := post(body)
		eq(t, name+" body over the limit", code, 413)
		contains(t, text, `"too_large"`)
	}
	eq(t, "builds", len(e.list(t, e.admin, "/api/v1/jobs/smoke/builds")), 0)
	id := e.started(t, "slow")
	e.call(t, 413, e.admin, "GET", "/api/v1/builds/"+id+"?wait=true", big)
	e.call(t, 413, e.admin, "GET", "/api/v1/builds/"+id+"/stages/s/log/events", big, "Accept", sseAccept)
}

func TestATriggerBodyIsReadStrictly(t *testing.T) {
	e := newEnv(t)
	for _, bad := range []string{`{"params": `, `{"parms": {"TIER": "full"}}`, `{"params": {"TIER": 1}}`, `[]`} {
		e.call(t, 400, e.admin, "POST", "/api/v1/jobs/smoke/builds", bad)
	}
	eq(t, "builds", len(e.list(t, e.admin, "/api/v1/jobs/smoke/builds")), 0)
	chunked := e.request(context.Background(), t, e.admin, "POST", "/api/v1/jobs/smoke/builds", io.MultiReader(strings.NewReader(`{"params": {"TIER": "full"}}`)))
	code, text, _ := e.send(t, chunked)
	eq(t, "a chunked body", code, 201)
	contains(t, text, `"TIER":"full"`)
	text, _ = e.call(t, 201, e.admin, "POST", "/api/v1/jobs/smoke/builds", "")
	contains(t, text, `"TIER":"quick"`)
}

func TestCancelAndWhatComesTooLate(t *testing.T) {
	e := newEnv(t)
	id := e.started(t, "slow")
	_, h := e.call(t, 200, e.admin, "GET", "/api/v1/builds/"+id+"/stages/s/log", "")
	eq(t, "more while running", h.Get("X-Kipp-More"), "true")
	eq(t, "offset", h.Get("X-Kipp-Offset"), "8")
	eq(t, "the build told to stop", jsonInt(e.object(t, 202, e.admin, "POST", "/api/v1/builds/"+id+"/cancel", "")["id"]), id)
	eq(t, "status", e.finished(t, id)["status"], any("cancelled"))
	contains(t, e.object(t, 409, e.admin, "POST", "/api/v1/builds/"+id+"/cancel", "")["detail"].(string), "cancelled")
	e.call(t, 404, e.admin, "POST", "/api/v1/builds/99999/cancel", "")
}

func TestADroppedTriggerIs409AndIsInTheActivity(t *testing.T) {
	e := newEnv(t)
	id := e.started(t, "once")
	contains(t, e.object(t, 409, e.admin, "POST", "/api/v1/jobs/once/builds", "")["detail"].(string), "dropped")
	act := e.list(t, e.admin, "/api/v1/jobs/once/activity")
	eq(t, "activity lines", len(act), 1)
	contains(t, act[0]["line"].(string), "dropped")
	if _, err := time.Parse(time.RFC3339Nano, act[0]["at"].(string)); err != nil {
		t.Fatal(act)
	}
	builds := e.list(t, e.admin, "/api/v1/jobs/once/builds?limit=1")
	eq(t, "builds", len(builds), 1)
	eq(t, "the build", jsonInt(builds[0]["id"]), id)
	e.call(t, 202, e.admin, "POST", "/api/v1/builds/"+id+"/cancel", "")
}

func TestTheLongPollGivesTheCurrentStateWhenItsTimeIsUp(t *testing.T) {
	e := newEnv(t, func(a *API) { a.wait = 100 * time.Millisecond })
	id := e.started(t, "slow")
	begin := time.Now()
	b := e.object(t, 200, e.admin, "GET", "/api/v1/builds/"+id+"?wait=true", "")
	eq(t, "status", b["status"], any("running"))
	eq(t, "finished_at", b["finished_at"], nil)
	if took := time.Since(begin); took < 100*time.Millisecond {
		t.Fatal("did not wait:", took)
	}
}

func TestTheLongPollAndTheStreamEndWhenTheClientLeaves(t *testing.T) {
	e := newEnv(t)
	id := e.started(t, "slow")
	baseline := runtime.NumGoroutine()
	// The server watches for a client that has gone only once the request
	// body is read, so each is asked with and without a body it has no use for.
	for _, c := range []struct{ path, accept, first, body string }{
		{"/api/v1/builds/" + id + "?wait=true", "", "", ""},
		{"/api/v1/builds/" + id + "?wait=true", "", "", `{"unread": true}`},
		{"/api/v1/builds/" + id + "/stages/s/log/events", sseAccept, "id: 8\ndata: started\n\n", ""},
		{"/api/v1/builds/" + id + "/stages/s/log/events", sseAccept, "id: 8\ndata: started\n\n", `{"unread": true}`},
	} {
		ctx, leave := context.WithCancel(context.Background())
		ended := make(chan error, 1)
		go func() {
			ended <- e.held(e.request(ctx, t, e.admin, "GET", c.path, strings.NewReader(c.body), "Accept", c.accept), c.first)
		}()
		eventually(t, func() bool { return e.active.Load() == 1 })
		select {
		case err := <-ended:
			t.Fatal(c.path, "ended while the build runs and the client waits:", err)
		case <-time.After(300 * time.Millisecond): // long enough to see it is held open
		}
		leave()
		if err := <-ended; !errors.Is(err, context.Canceled) {
			t.Fatal(c.path, err)
		}
		e.settles(t, baseline)
	}
	eq(t, "status after the clients left", e.object(t, 200, e.admin, "GET", "/api/v1/builds/"+id, "")["status"], any("running"))
}

// held reads first from the response and then waits for the response to
// end, which it does not while the build runs.
func (e *env) held(req *http.Request, first string) error {
	res, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if _, err := io.ReadFull(res.Body, make([]byte, len(first))); err != nil {
		return err
	}
	_, err = io.Copy(io.Discard, res.Body)
	return err
}

func TestTheLongPollAndTheStreamEndWhenTheBuildDoes(t *testing.T) {
	e := newEnv(t)
	id := e.started(t, "slow")
	baseline := runtime.NumGoroutine()
	polled, streamed := make(chan string, 1), make(chan string, 1)
	go func() {
		_, body, _ := e.raw(t, e.admin, "GET", "/api/v1/builds/"+id+"?wait=true", "")
		polled <- body
	}()
	go func() {
		_, body, _ := e.raw(t, e.admin, "GET", "/api/v1/builds/"+id+"/stages/s/log/events", "", "Accept", sseAccept)
		streamed <- body
	}()
	eventually(t, func() bool { return e.active.Load() == 2 })
	e.call(t, 202, e.admin, "POST", "/api/v1/builds/"+id+"/cancel", "")
	contains(t, <-polled, `"status":"cancelled"`)
	if body := <-streamed; !strings.HasPrefix(body, "id: 8\ndata: started\n\n") || !strings.HasSuffix(body, sseEnd) {
		t.Fatalf("%q", body)
	}
	e.settles(t, baseline)
}

func TestOnlyALoggedInCallerIsToldTheVersionAndUptime(t *testing.T) {
	e := newEnv(t)
	for _, tok := range []string{e.viewer, e.admin} {
		health := e.object(t, 200, tok, "GET", "/api/v1/health", "")
		eq(t, "version", health["version"], any("test"))
		eq(t, "ok", health["ok"], any(true))
		if _, ok := health["uptime_seconds"].(float64); !ok {
			t.Fatal(health)
		}
	}
	sess := &store.Session{ID: "sess-health", UserID: e.user(t, "viewer").ID, CSRF: "c", ExpiresAt: time.Now().Add(time.Hour)}
	must(t, e.st.CreateSession(context.Background(), sess))
	eq(t, "with a session", e.object(t, 200, "", "GET", "/api/v1/health", "", "Cookie", SessionCookieFor(sess).String())["version"], any("test"))
	for _, hdr := range [][]string{{"Authorization", "Bearer not-a-token"}, {"Cookie", SessionCookie + "=not-a-session"}} {
		text, _ := e.call(t, 200, "", "GET", "/api/v1/health", "", hdr...)
		eq(t, "health with "+hdr[1], strings.TrimSpace(text), `{"ok":true}`)
	}
}

func TestAStoreThatIsDownIsA500NotA401OrA404(t *testing.T) {
	e := newEnv(t)
	must(t, e.st.Close())
	for _, path := range []string{"/api/v1/jobs", "/api/v1/jobs/smoke", "/api/v1/builds/1"} {
		eq(t, path, e.object(t, 500, e.agent, "GET", path, "")["detail"], any("internal error"))
	}
	e.call(t, 500, "", "GET", "/api/v1/jobs", "", "Cookie", SessionCookie+"=abc")
	// A probe is answered whatever it sends along, and says only ok when
	// it cannot be told who sent it.
	e.call(t, 200, "", "GET", "/api/v1/health", "")
	text, _ := e.call(t, 200, e.admin, "GET", "/api/v1/health", "", "Cookie", SessionCookie+"=abc")
	eq(t, "health", strings.TrimSpace(text), `{"ok":true}`)
}

func TestServiceBoundsWhatItAsksTheStoreFor(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for range 51 {
		b := &store.Build{Job: "gc", Status: store.Success, Trigger: "cli"}
		must(t, e.st.CreateBuild(ctx, b))
		must(t, e.st.PutTests(ctx, b.ID, []store.Test{{Stage: "s", Suite: "x", Name: "t", Status: "passed"}}))
	}
	svc, admin := e.api.Service(), e.user(t, "admin")
	// A negative LIMIT means no limit to SQLite; neither call may pass one on.
	for asked, want := range map[int]int{-1: 50, 0: 50, 2: 2, 100000: 51} {
		bs, _, err := svc.Builds(ctx, admin, "gc", 0, asked)
		must(t, err)
		eq(t, fmt.Sprint("builds for ", asked), len(bs), want)
	}
	for asked, want := range map[int]int{-1: 20, 0: 20, 2: 2, 100000: 51} {
		rows, err := svc.TestHistory(ctx, admin, "gc", asked)
		must(t, err)
		eq(t, fmt.Sprint("test rows for ", asked), len(rows), want)
	}
	if _, err := svc.TestHistory(ctx, e.user(t, "agent"), "gc", 5); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestACookieNeitherRescuesNorWidensABearerToken(t *testing.T) {
	e := newEnv(t)
	sess := &store.Session{ID: "sess-live", UserID: e.user(t, "admin").ID, CSRF: "csrf-live", ExpiresAt: time.Now().Add(time.Hour)}
	must(t, e.st.CreateSession(context.Background(), sess))
	cookie := SessionCookieFor(sess).String()
	// An unknown token is nobody, with the admin's cookie beside it or not;
	// the scheme is case-insensitive (RFC 9110), so "bearer" is a token too.
	for _, authz := range []string{"Bearer kipp_wrong", "bearer kipp_wrong", "BEARER kipp_wrong", "Bearer", "bearer "} {
		e.call(t, 401, "", "POST", "/api/v1/jobs/smoke/builds", "{}", "Cookie", cookie, "Authorization", authz)
		e.call(t, 401, "", "POST", "/api/v1/admin/reload", "", "Cookie", cookie, "Authorization", authz, CSRFHeader, "csrf-live")
		e.call(t, 401, "", "GET", "/api/v1/jobs", "", "Cookie", cookie, "Authorization", authz)
	}
	eq(t, "builds", len(e.list(t, e.admin, "/api/v1/jobs/smoke/builds")), 0)
	// A valid narrow token acts as its own user, not as the cookie's.
	for _, scheme := range []string{"Bearer ", "bearer "} {
		e.call(t, 403, "", "POST", "/api/v1/admin/reload", "", "Cookie", cookie, "Authorization", scheme+e.agent)
		e.call(t, 404, "", "GET", "/api/v1/jobs/gc", "", "Cookie", cookie, "Authorization", scheme+e.agent)
		b := e.object(t, 201, "", "POST", "/api/v1/jobs/smoke/builds", "{}", "Cookie", cookie, "Authorization", scheme+e.agent)
		eq(t, "started_by", b["started_by"], any("agent"))
	}
}

func TestASessionWithNoCSRFTokenAcceptsNoneAtAll(t *testing.T) {
	e := newEnv(t)
	sess := &store.Session{ID: "sess-bare", UserID: e.user(t, "admin").ID, CSRF: "", ExpiresAt: time.Now().Add(time.Hour)}
	must(t, e.st.CreateSession(context.Background(), sess))
	cookie := SessionCookieFor(sess).String()
	e.call(t, 200, "", "GET", "/api/v1/jobs", "", "Cookie", cookie)
	e.call(t, 403, "", "POST", "/api/v1/admin/reload", "", "Cookie", cookie)
	e.call(t, 403, "", "POST", "/api/v1/admin/reload", "", "Cookie", cookie, CSRFHeader, "")
}

// lockedBuffer is a log sink two goroutines may use.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestAStreamThatLosesTheStoreDoesNotSayTheLogIsComplete(t *testing.T) {
	var logged lockedBuffer
	e := newEnv(t, func(a *API) {
		a.logPoll = 10 * time.Millisecond
		a.deps.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	id := e.started(t, "slow")
	res, err := e.client.Do(e.request(t.Context(), t, e.admin, "GET", "/api/v1/builds/"+id+"/stages/s/log/events", nil, "Accept", sseAccept))
	must(t, err)
	defer func() { _ = res.Body.Close() }()
	first := make([]byte, len("id: 8\ndata: started\n\n"))
	_, err = io.ReadFull(res.Body, first)
	must(t, err)
	must(t, e.st.Close())
	rest, err := io.ReadAll(res.Body) // the stream closes; the client reconnects with Last-Event-ID
	must(t, err)
	if strings.Contains(string(rest), "event: end") {
		t.Fatalf("the stream said the log was complete: %q", rest)
	}
	contains(t, logged.String(), "database is closed")
}

func TestEveryAnswerIsNotSniffedAndNotStored(t *testing.T) {
	e := newEnv(t)
	id := e.trigger(t, "smoke", "")
	e.finished(t, id)
	for _, c := range []struct {
		code                      int
		tok, method, path, accept string
	}{
		{200, "", "GET", "/api/v1/health", ""},
		{401, "", "GET", "/api/v1/jobs", ""},
		{200, e.admin, "GET", "/api/v1/jobs", ""},
		{200, e.admin, "GET", "/api/v1/builds/" + id, ""},
		{404, e.agent, "GET", "/api/v1/jobs/gc", ""},
		{403, e.agent, "POST", "/api/v1/admin/reload", ""},
		{200, e.admin, "GET", "/api/v1/builds/" + id + "/stages/s/log", ""},
		{200, e.admin, "GET", "/api/v1/builds/" + id + "/stages/s/log/events", sseAccept},
	} {
		_, h := e.call(t, c.code, c.tok, c.method, c.path, "", "Accept", c.accept)
		eq(t, c.path+" nosniff", h.Get("X-Content-Type-Options"), "nosniff")
		eq(t, c.path+" cache", h.Get("Cache-Control"), "no-store")
	}
}

func TestATriggerChecksPermissionBeforeItReadsTheBody(t *testing.T) {
	e := newEnv(t)
	for _, bad := range []string{`{"params": `, `{"params": {}} trailing`, `{"params": {}}{"params": {}}`} {
		e.call(t, 404, e.agent, "POST", "/api/v1/jobs/gc/builds", bad)
		e.call(t, 404, e.agent, "POST", "/api/v1/jobs/nope/builds", bad)
		e.call(t, 403, e.viewer, "POST", "/api/v1/jobs/smoke/builds", bad)
		msg := e.object(t, 400, e.agent, "POST", "/api/v1/jobs/smoke/builds", bad)["detail"].(string)
		if msg == "" || strings.Contains(msg, "\n") || strings.HasPrefix(msg, "bad input") {
			t.Fatalf("%q", msg)
		}
	}
	eq(t, "builds", len(e.list(t, e.admin, "/api/v1/jobs/smoke/builds")), 0)
	e.call(t, 201, e.agent, "POST", "/api/v1/jobs/smoke/builds", "{\"params\": {}}\n \n")
}

func TestReloadThatWasNotWiredIsAnInternalProblemWhoseCauseIsLogged(t *testing.T) {
	e := newEnv(t)
	a := New(Deps{Auth: e.api.deps.Auth, Dispatch: e.api.deps.Dispatch, Store: e.st})
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, e.request(context.Background(), t, e.admin, "POST", "/api/v1/admin/reload", nil))
	eq(t, "status", rec.Code, 500)
	contains(t, rec.Body.String(), `"code":"internal"`)
}

// The refusal names job files, and a job one may not view does not exist:
// whoever is not an admin is told that the set is stale and nothing more.
func TestTheReloadErrorIsToldInFullToAdminsAndInOneFixedSentenceToTheRest(t *testing.T) {
	e := newEnv(t)
	admin, narrow := &store.User{Name: "admin", Admin: true}, &store.User{Name: "agent"}
	for _, u := range []*store.User{admin, narrow, nil} {
		eq(t, "with nothing wired", e.api.Service().ReloadError(u), "")
	}
	e.api.svc.deps.ReloadError = func() string { return "hidden.yml: no stages" }
	eq(t, "an admin", e.api.Service().ReloadError(admin), "hidden.yml: no stages")
	eq(t, "a narrow user", e.api.Service().ReloadError(narrow), StaleJobs)
	eq(t, "nobody", e.api.Service().ReloadError(nil), "")
	if strings.Contains(StaleJobs, "/") || strings.Contains(StaleJobs, ".yml") || strings.Count(StaleJobs, ".") != 1 {
		t.Fatalf("not one sentence free of file names: %q", StaleJobs)
	}
}

func TestABuildIsShownWithTheNeedsItRanWithNotTheJobsOfToday(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// A build whose stages are not the job file's: the file changed since,
	// or they were read from the job's repo.
	b := &store.Build{Job: "smoke", Status: store.Success, Trigger: "api"}
	must(t, e.st.CreateBuild(ctx, b))
	must(t, e.st.PutStages(ctx, b.ID, []string{"lint", "unit"}, map[string][]string{"unit": {"lint"}}))
	v, err := e.api.Service().Build(ctx, e.user(t, "admin"), b.ID)
	must(t, err)
	if len(v.Needs) != 2 || len(v.Needs["lint"]) != 0 || !slices.Equal(v.Needs["unit"], []string{"lint"}) {
		t.Fatalf("needs %v", v.Needs)
	}
	stages := e.object(t, 200, e.admin, "GET", "/api/v1/builds/"+strconv.FormatInt(b.ID, 10), "")["stages"].([]any)
	eq(t, "lint needs", fmt.Sprint(stages[0].(map[string]any)["needs"]), "[]")
	eq(t, "unit needs", fmt.Sprint(stages[1].(map[string]any)["needs"]), "[lint]")

	// One that ended before it recorded a stage has none, whatever the job has now.
	early := &store.Build{Job: "smoke", Status: store.Failed, Trigger: "api"}
	must(t, e.st.CreateBuild(ctx, early))
	v, err = e.api.Service().Build(ctx, e.user(t, "admin"), early.ID)
	must(t, err)
	eq(t, "needs of a build that never had stages", len(v.Needs), 0)
}

func TestABuildStillQueuedIsShownWithTheJobsPlan(t *testing.T) {
	e := newEnv(t)
	e.started(t, "slow")
	queued, err := strconv.ParseInt(e.trigger(t, "slow", ""), 10, 64)
	must(t, err)
	v, err := e.api.Service().Build(context.Background(), e.user(t, "admin"), queued)
	must(t, err)
	eq(t, "status", v.Build.Status, store.Queued)
	if _, planned := v.Needs["s"]; !planned || len(v.Needs) != 1 || len(v.Stages) != 0 {
		t.Fatalf("needs %v, stages %v", v.Needs, v.Stages)
	}
}

func TestTheLogOfABuildStillQueuedIsNotComplete(t *testing.T) {
	e := newEnv(t)
	e.started(t, "slow")
	queued := e.trigger(t, "slow", "")
	text, h := e.call(t, 200, e.admin, "GET", "/api/v1/builds/"+queued+"/stages/s/log", "")
	eq(t, "log", text, "")
	eq(t, "more", h.Get("X-Kipp-More"), "true")
}

// hooked swaps the job set for one job whose on_success hook waits until
// the func it returns is called.
func (e *env) hooked(t *testing.T) (open func()) {
	t.Helper()
	gate := filepath.Join(t.TempDir(), "gate")
	jobs := filepath.Join(t.TempDir(), "jobs")
	must(t, os.MkdirAll(jobs, 0o750))
	job := "name: hooked\nstages:\n  s: {run: echo stage}\non_success: {run: 'while [ ! -e " + gate + " ]; do sleep 0.02; done; echo hook done'}\n"
	must(t, os.WriteFile(filepath.Join(jobs, "hooked.yml"), []byte(job), 0o600))
	snap, err := config.Load(jobs)
	must(t, err)
	e.api.deps.Dispatch.SetSnapshot(snap)
	return func() { must(t, os.WriteFile(gate, nil, 0o600)) }
}

// The hook's output, and a note about a junit report, are written after
// the build's status is final.
func TestALogIsCompleteOnlyOnceTheHookIsDone(t *testing.T) {
	e := newEnv(t, func(a *API) { a.logPoll = 10 * time.Millisecond })
	open := e.hooked(t)
	id := e.trigger(t, "hooked", "")
	eventually(t, func() bool {
		return e.object(t, 200, e.admin, "GET", "/api/v1/builds/"+id, "")["status"] == any("success")
	})
	for _, name := range []string{"s", "hooks"} {
		_, h := e.call(t, 200, e.admin, "GET", "/api/v1/builds/"+id+"/stages/"+name+"/log", "")
		eq(t, name+": more while the hook runs", h.Get("X-Kipp-More"), "true")
	}
	res, err := e.client.Do(e.request(t.Context(), t, e.admin, "GET", "/api/v1/builds/"+id+"/stages/hooks/log/events", nil, "Accept", sseAccept))
	must(t, err)
	defer func() { _ = res.Body.Close() }()
	stream := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(res.Body)
		stream <- string(data)
	}()
	select {
	case got := <-stream:
		t.Fatalf("the stream ended while the hook ran: %q", got)
	case <-time.After(200 * time.Millisecond):
	}
	open()
	eq(t, "stream", <-stream, "id: 10\ndata: hook done\n\n"+sseEnd)
	text, h := e.call(t, 200, e.admin, "GET", "/api/v1/builds/"+id+"/stages/hooks/log", "")
	eq(t, "the hook's log", text, "hook done\n")
	eq(t, "more once the hook is done", h.Get("X-Kipp-More"), "false")
}

// The web shows a refusal as a page, so it writes the answer itself; the
// API's own answer is the JSON the tests above pin.
func TestAuthenticateWithHandsARefusalToItsCaller(t *testing.T) {
	e := newEnv(t)
	sess := &store.Session{ID: "sess-live", UserID: e.user(t, "admin").ID, CSRF: "csrf-live", ExpiresAt: time.Now().Add(time.Hour)}
	must(t, e.st.CreateSession(context.Background(), sess))
	var refused error
	passed := false
	h := e.api.AuthenticateWith(
		func(w http.ResponseWriter, _ *http.Request, err error) {
			refused = err
			w.WriteHeader(http.StatusTeapot)
		},
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { passed = UserFrom(r.Context()) != nil }),
	)
	send := func(method string, hdr ...string) int {
		t.Helper()
		refused, passed = nil, false
		req := httptest.NewRequest(method, "/jobs/smoke/build", nil)
		for i := 0; i < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	cookie := SessionCookieFor(sess).String()

	eq(t, "a session with its token", send("POST", "Cookie", cookie, CSRFHeader, "csrf-live"), 200)
	eq(t, "it went on as the user", passed, true)
	eq(t, "a session with a stale token", send("POST", "Cookie", cookie, CSRFHeader, "csrf-old"), http.StatusTeapot)
	eq(t, "the refusal", errors.Is(refused, ErrCSRF), true)
	eq(t, "nothing went on", passed, false)
	eq(t, "nobody", send("POST"), 200)
	eq(t, "nobody is not refused, the handler decides", refused == nil && !passed, true)

	must(t, e.st.Close())
	eq(t, "a store that is down", send("GET", "Cookie", cookie), http.StatusTeapot)
	if refused == nil || errors.Is(refused, ErrCSRF) || errors.Is(refused, ErrNotFound) {
		t.Fatalf("a store failure is handed over as it is: %v", refused)
	}
}

// A page that draws a build stops refreshing by the rule the logs end by.
func TestABuildIsCompleteOnlyOnceTheHookIsDone(t *testing.T) {
	e := newEnv(t)
	open := e.hooked(t)
	admin := e.user(t, "admin")
	id, err := strconv.ParseInt(e.trigger(t, "hooked", ""), 10, 64)
	must(t, err)
	view := func() *BuildView {
		t.Helper()
		v, err := e.api.Service().Build(context.Background(), admin, id)
		must(t, err)
		return v
	}
	eventually(t, func() bool { return view().Build.Status == store.Success })
	eq(t, "complete while the hook runs", view().Complete, false)
	open()
	eventually(t, func() bool { return view().Complete })
}

// reload has the dispatcher read the job files again, as serve does.
func (e *env) reload(t *testing.T) {
	t.Helper()
	snap, err := config.Load(filepath.Join(e.dir, "jobs"))
	must(t, err)
	e.api.deps.Dispatch.SetSnapshot(snap)
}

func (e *env) writeJob(t *testing.T, name, body string) {
	t.Helper()
	must(t, os.WriteFile(filepath.Join(e.dir, "jobs", name+".yml"), []byte(body), 0o600))
	e.reload(t)
}

func (e *env) removeJob(t *testing.T, name string) {
	t.Helper()
	must(t, os.Remove(filepath.Join(e.dir, "jobs", name+".yml")))
	e.reload(t)
}

func TestARunningBuildOfARemovedJobIsStillReadAndCancelledByWhoeverMay(t *testing.T) {
	e := newEnv(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	e.writeJob(t, "gone", "name: gone\ngroup: backend\nstages:\n  s: {run: 'echo $$ > "+pidFile+"; echo started; sleep 30'}\n")
	keeper := token(t, e.st, "keeper", false, map[string][]string{"gone": {"cancel"}})
	id := e.started(t, "gone")
	data, err := os.ReadFile(pidFile)
	must(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	must(t, err)
	e.removeJob(t, "gone")

	for _, tok := range []string{e.admin, keeper} {
		eq(t, "status", e.object(t, 200, tok, "GET", "/api/v1/builds/"+id, "")["status"], any("running"))
		text, _ := e.call(t, 200, tok, "GET", "/api/v1/builds/"+id+"/stages/s/log", "")
		contains(t, text, "started")
		e.call(t, 200, tok, "GET", "/api/v1/builds/"+id+"/results", "")
		job := e.object(t, 200, tok, "GET", "/api/v1/jobs/gone", "")
		eq(t, "removed", job["removed"], any(true))
		eq(t, "builds of the removed job", len(job["builds"].(map[string]any)["items"].([]any)), 1)
		eq(t, "next", job["next"], nil)
		eq(t, "builds", len(e.list(t, tok, "/api/v1/jobs/gone/builds")), 1)
		e.call(t, 200, tok, "GET", "/api/v1/jobs/gone/activity", "")
		listed := slices.IndexFunc(e.list(t, tok, "/api/v1/jobs"), func(j map[string]any) bool { return j["name"] == "gone" && j["removed"] == true })
		if listed < 0 {
			t.Fatal("the removed job is not in the list of whoever may view it")
		}
		// It has history and no future: nobody builds it.
		e.call(t, 404, tok, "POST", "/api/v1/jobs/gone/builds", "")
	}

	// Whoever has no grant on it is told what they are told of a job that never was.
	_, missing, _ := e.raw(t, e.viewer, "GET", "/api/v1/builds/999999", "")
	for _, r := range [][2]string{
		{"GET", "/api/v1/builds/" + id}, {"POST", "/api/v1/builds/" + id + "/cancel"}, {"GET", "/api/v1/builds/" + id + "/stages/s/log"},
		{"GET", "/api/v1/builds/" + id + "/results"}, {"GET", "/api/v1/jobs/gone"}, {"GET", "/api/v1/jobs/gone/builds"}, {"GET", "/api/v1/jobs/gone/activity"},
	} {
		code, body, _ := e.raw(t, e.viewer, r[0], r[1], "")
		if code != 404 || body != missing {
			t.Fatalf("%s %s: %d %q, want 404 %q", r[0], r[1], code, body, missing)
		}
	}
	for _, j := range e.list(t, e.viewer, "/api/v1/jobs") {
		if j["name"] == "gone" {
			t.Fatal("a removed job is listed to a user with no grant on it")
		}
	}

	e.call(t, 202, keeper, "POST", "/api/v1/builds/"+id+"/cancel", "")
	eq(t, "status", e.finished(t, id)["status"], any("cancelled"))
	eventually(t, func() bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) })
}

func TestAQueuedBuildOfARemovedJobFailsWhereItCanBeSeen(t *testing.T) {
	e := newEnv(t)
	first := e.started(t, "slow")
	second := e.trigger(t, "slow", "")
	e.removeJob(t, "slow")
	eq(t, "the queued build", e.finished(t, second)["status"], any("failed"))
	job := e.object(t, 200, e.admin, "GET", "/api/v1/jobs/slow", "")
	lines := fmt.Sprint(job["activity"])
	contains(t, lines, "#2 failed before it started: the job no longer exists")
	eq(t, "the running build", e.object(t, 200, e.admin, "GET", "/api/v1/builds/"+first, "")["status"], any("running"))
	e.call(t, 202, e.admin, "POST", "/api/v1/builds/"+first+"/cancel", "")
	e.finished(t, first)
}

func TestAJobThatWasRemovedAndHasNoBuildIsGone(t *testing.T) {
	e := newEnv(t)
	e.removeJob(t, "gc")
	e.call(t, 404, e.admin, "GET", "/api/v1/jobs/gc", "")
	e.call(t, 404, e.admin, "GET", "/api/v1/jobs/gc/builds", "")
	for _, j := range e.list(t, e.admin, "/api/v1/jobs") {
		if j["name"] == "gc" {
			t.Fatal("a removed job with no build is still listed")
		}
	}
}

func TestTheLogSaysItsSizeAndComesWholeOnlyAsADownload(t *testing.T) {
	e := newEnv(t)
	id := e.trigger(t, "smoke", "")
	e.finished(t, id)
	var want strings.Builder
	for i := 0; want.Len() < 5*logChunk/2; i++ {
		fmt.Fprintf(&want, "line %d of a long log\n", i)
	}
	must(t, os.WriteFile(filepath.Join(e.dir, "logs", "smoke", "1", "s.log"), []byte(want.String()), 0o600))
	path := "/api/v1/builds/" + id + "/stages/s/log"
	size := strconv.Itoa(want.Len())

	text, h := e.call(t, 200, e.viewer, "GET", path, "")
	eq(t, "the size beside a chunk", h.Get("X-Kipp-Size"), size)
	eq(t, "a chunk", len(text), logChunk)
	// The size alone, for a page that is about to show the end of the log.
	text, h = e.call(t, 200, e.viewer, "HEAD", path, "")
	eq(t, "the size alone", h.Get("X-Kipp-Size")+text, size)

	// From where the last lines begin, the stream is the last lines.
	tail := want.String()[want.Len()-40:]
	from := want.Len() - len(tail) + strings.Index(tail, "\n") + 1
	stream, _ := e.call(t, 200, e.viewer, "GET", path+"/events?offset="+strconv.Itoa(from), "")
	last := strings.TrimSuffix(want.String()[from:], "\n")
	eq(t, "the stream of the end", stream, fmt.Sprintf("id: %d\ndata: %s\n\n", want.Len(), last)+sseEnd)

	text, h = e.call(t, 200, e.viewer, "GET", path+"?download=true", "")
	eq(t, "the download is the log", text == want.String(), true)
	eq(t, "the download's name", h.Get("Content-Disposition"), `attachment; filename="smoke-1-s.log"`)
	eq(t, "the download's type", h.Get("Content-Type"), "text/plain; charset=utf-8")
	eq(t, "the download is not sniffed", h.Get("X-Content-Type-Options"), "nosniff")

	// A log not written yet is empty in every form, and a hidden one hidden.
	text, h = e.call(t, 200, e.viewer, "GET", "/api/v1/builds/"+id+"/stages/hooks/log?download=true", "")
	eq(t, "a download of nothing", text+h.Get("X-Kipp-Size"), "")
	_, h = e.call(t, 200, e.viewer, "HEAD", "/api/v1/builds/"+id+"/stages/hooks/log", "")
	eq(t, "the size of nothing", h.Get("X-Kipp-Size"), "0")
	slow := e.started(t, "slow")
	e.call(t, 404, e.viewer, "GET", "/api/v1/builds/"+slow+"/stages/s/log?download=true", "")
	e.call(t, 404, e.viewer, "HEAD", "/api/v1/builds/"+slow+"/stages/s/log", "")
}

func TestADisabledJobSaysSoAndIsNotBuilt(t *testing.T) {
	e := newEnv(t)
	e.writeJob(t, "off", "name: off\ndisabled: true\ntriggers: {cron: '*/10 * * * *'}\nstages:\n  s: {run: 'true'}\n")
	refused := e.object(t, 409, e.admin, "POST", "/api/v1/jobs/off/builds", "")
	contains(t, fmt.Sprint(refused["detail"]), "the job is disabled")
	// What the request holds changes nothing: the job is not built.
	e.call(t, 409, e.admin, "POST", "/api/v1/jobs/off/builds", `{"params": {"NOPE": "1"}}`)
	e.call(t, 409, e.admin, "POST", "/api/v1/jobs/off/builds", `not JSON at all`)
	e.call(t, 404, e.viewer, "POST", "/api/v1/jobs/off/builds", "")
	job := e.object(t, 200, e.admin, "GET", "/api/v1/jobs/off", "")
	eq(t, "disabled", job["disabled"], any(true))
	eq(t, "a disabled job's next run", job["next"], nil)
	eq(t, "builds", len(job["builds"].(map[string]any)["items"].([]any)), 0)
	for _, j := range e.list(t, e.admin, "/api/v1/jobs") {
		eq(t, "disabled in the list: "+fmt.Sprint(j["name"]), j["disabled"], any(j["name"] == "off"))
		if j["name"] == "off" && j["next"] != nil {
			t.Fatal("a disabled job has a next run in the list")
		}
	}
	eq(t, "a job that is not disabled", e.object(t, 200, e.admin, "GET", "/api/v1/jobs/gc", "")["disabled"], any(false))
}

func TestAnUnstableBuildSaysSoWhereverAStatusIsShown(t *testing.T) {
	e := newEnv(t)
	e.writeJob(t, "shaky", "name: shaky\nstages:\n  lint: {run: 'echo warned; exit 4', unstable_exit: [4]}\n  unit: {needs: [lint], run: 'true'}\n")
	id := e.trigger(t, "shaky", "")
	b := e.finished(t, id)
	eq(t, "the build's status", b["status"], any("unstable"))
	if b["finished_at"] == nil {
		t.Fatal("an unstable build has no finished_at")
	}
	stages := b["stages"].([]any)
	lint, unit := stages[0].(map[string]any), stages[1].(map[string]any)
	eq(t, "the unstable stage's status", lint["status"], any("unstable"))
	eq(t, "the unstable stage's exit code", lint["exit_code"], any(float64(4)))
	eq(t, "the status of the stage that needs it", unit["status"], any("success"))

	job := e.object(t, 200, e.admin, "GET", "/api/v1/jobs/shaky", "")
	eq(t, "in the job's builds", job["builds"].(map[string]any)["items"].([]any)[0].(map[string]any)["status"], any("unstable"))
	eq(t, "in the builds list", e.list(t, e.admin, "/api/v1/jobs/shaky/builds")[0]["status"], any("unstable"))
	for _, j := range e.list(t, e.admin, "/api/v1/jobs") {
		if j["name"] == "shaky" {
			eq(t, "as the job's last build", j["last"].(map[string]any)["status"], any("unstable"))
		}
	}

	// It has ended: its log is complete and it is past cancelling.
	text, _ := e.call(t, 200, e.admin, "GET", "/api/v1/builds/"+id+"/stages/lint/log/events", "", "Accept", sseAccept)
	contains(t, text, "data: warned\n")
	if !strings.HasSuffix(text, sseEnd) {
		t.Fatalf("the stream of an unstable build's log does not say it is complete: %q", text)
	}
	refused := e.object(t, 409, e.admin, "POST", "/api/v1/builds/"+id+"/cancel", "")
	contains(t, fmt.Sprint(refused["detail"]), "it is unstable")
}

// stamps collects every string in a decoded JSON value that reads as a
// date and a time.
func stamps(v any, into *[]string) {
	switch x := v.(type) {
	case string:
		if regexp.MustCompile(`^\d{4}-\d\d-\d\dT`).MatchString(x) {
			*into = append(*into, x)
		}
	case []any:
		for _, e := range x {
			stamps(e, into)
		}
	case map[string]any:
		for _, e := range x {
			stamps(e, into)
		}
	}
}

func TestEveryTimeTheAPIAnswersIsUTCWithAZWhateverTheServersZone(t *testing.T) {
	e := newEnv(t)
	var got []string
	created, _ := e.call(t, 201, e.admin, "POST", "/api/v1/jobs/smoke/builds", `{"params":{"TIER":"quick"}}`)
	var b map[string]any
	must(t, json.Unmarshal([]byte(created), &b))
	stamps(b, &got)
	eq(t, "times in the answer to a trigger", len(got), 1)
	id := strconv.Itoa(int(b["id"].(float64)))
	e.finished(t, id)
	must(t, e.st.AddActivity(context.Background(), "smoke", "noted", time.Now()))
	for _, path := range []string{"/api/v1/builds/" + id, "/api/v1/jobs", "/api/v1/jobs/smoke", "/api/v1/jobs/gc", "/api/v1/jobs/smoke/builds", "/api/v1/jobs/smoke/activity"} {
		body, _ := e.call(t, 200, e.admin, "GET", path, "")
		var v any
		must(t, json.Unmarshal([]byte(body), &v))
		before := len(got)
		stamps(v, &got)
		if len(got) == before {
			t.Fatalf("%s answered no time at all: %s", path, body)
		}
	}
	for _, s := range got {
		at, err := time.Parse(time.RFC3339Nano, s)
		if err != nil || !strings.HasSuffix(s, "Z") {
			t.Errorf("%q is not RFC 3339 in UTC with a Z (%v)", s, err)
		}
		if time.Since(at).Abs() > 24*time.Hour {
			t.Errorf("%q is not the moment it stands for", s)
		}
	}
}

func TestOnlyARequestStraightFromThisMachineOverPlainHTTPIsLocal(t *testing.T) {
	for _, c := range []struct {
		what, peer, host string
		hdr              [2]string
		tls              bool
		want             bool
	}{
		{what: "a browser on this machine", peer: "127.0.0.1:50000", host: "127.0.0.1:8085", want: true},
		{what: "by name", peer: "127.0.0.1:50000", host: "localhost:8085", want: true},
		{what: "over IPv6", peer: "[::1]:50000", host: "[::1]:8085", want: true},
		{what: "no port", peer: "127.0.0.1:50000", host: "localhost", want: true},
		{what: "another machine", peer: "192.0.2.9:50000", host: "127.0.0.1:8085"},
		{what: "a proxy on this machine that passed the Host through", peer: "127.0.0.1:50000", host: "ci.example.com"},
		{what: "a name that only starts like this machine's", peer: "127.0.0.1:50000", host: "localhost.example.com"},
		{what: "a name that holds this machine's address", peer: "127.0.0.1:50000", host: "127.0.0.1.example.com"},
		{what: "a proxy that says https", peer: "127.0.0.1:50000", host: "127.0.0.1:8085", hdr: [2]string{"X-Forwarded-Proto", "https"}},
		{what: "a proxy that says http", peer: "127.0.0.1:50000", host: "127.0.0.1:8085", hdr: [2]string{"X-Forwarded-Proto", "http"}},
		{what: "a proxy that names its client", peer: "127.0.0.1:50000", host: "127.0.0.1:8085", hdr: [2]string{"X-Forwarded-For", "203.0.113.9"}},
		{what: "a proxy that names the host", peer: "127.0.0.1:50000", host: "127.0.0.1:8085", hdr: [2]string{"X-Forwarded-Host", "ci.example.com"}},
		{what: "a proxy by the standard header", peer: "127.0.0.1:50000", host: "127.0.0.1:8085", hdr: [2]string{"Forwarded", "proto=https"}},
		{what: "a proxy that signs itself", peer: "127.0.0.1:50000", host: "127.0.0.1:8085", hdr: [2]string{"Via", "1.1 edge"}},
		{what: "TLS", peer: "127.0.0.1:50000", host: "127.0.0.1:8085", tls: true},
		{what: "a peer that does not parse", peer: "somewhere", host: "127.0.0.1:8085"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr, r.Host = c.peer, c.host
		if c.hdr[0] != "" {
			r.Header.Set(c.hdr[0], c.hdr[1])
		}
		if c.tls {
			r.TLS = &tls.ConnectionState{}
		}
		eq(t, c.what, Local(r), c.want)
	}
}

func TestALoginStraightFromThisMachineOverPlainHTTPGetsACookieABrowserKeepsThereAndNoOtherLoginDoes(t *testing.T) {
	sess := &store.Session{ID: "abc", CSRF: "never-in-the-cookie", ExpiresAt: time.Now().Add(time.Hour)}
	login := func(host, origin, forwarded string) []*http.Cookie {
		r := httptest.NewRequest("POST", "/login", nil)
		r.RemoteAddr, r.Host = "127.0.0.1:50000", host
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if forwarded != "" {
			r.Header.Set("X-Forwarded-Proto", forwarded)
		}
		return (&API{loopback: true}).LoginCookies(r, sess)
	}
	// Safari keeps no Secure cookie from http://127.0.0.1, so this one is not Secure, and so cannot be __Host-.
	cookies := login("127.0.0.1:8085", "http://127.0.0.1:8085", "")
	eq(t, "cookies", len(cookies), 2)
	local := cookies[0]
	eq(t, "name", local.Name, "kipp_local_session")
	eq(t, "value", local.Value, "abc")
	eq(t, "secure", local.Secure, false)
	eq(t, "http only", local.HttpOnly, true)
	eq(t, "same site", local.SameSite, http.SameSiteLaxMode)
	eq(t, "path", local.Path, "/")
	eq(t, "domain", local.Domain, "")
	// The Secure one a browser may hold from before would win over it: it is ended.
	eq(t, "the other cookie", cookies[1].Name, SessionCookie)
	eq(t, "the other cookie ends", cookies[1].MaxAge, -1)

	for what, got := range map[string][]*http.Cookie{
		"through a proxy that says https":             login("127.0.0.1:8085", "http://127.0.0.1:8085", "https"),
		"under a public name":                         login("ci.example.com", "http://ci.example.com", ""),
		"from a page served over https":               login("127.0.0.1:8085", "https://127.0.0.1:8085", ""),
		"from a page of another name":                 login("127.0.0.1:8085", "https://ci.example.com", ""),
		"from a client that does not say its page":    login("127.0.0.1:8085", "", ""),
		"from a page on another port of this machine": login("127.0.0.1:8085", "http://127.0.0.1:9000", ""),
	} {
		if len(got) != 1 || got[0].Name != SessionCookie || !got[0].Secure || got[0].Value != "abc" {
			t.Errorf("a login %s got %v, want the Secure cookie alone", what, got)
		}
	}
}

func TestTheLocalSessionCookieLogsInOnlyARequestThatIsLocal(t *testing.T) {
	e := newEnv(t)
	sess := &store.Session{ID: "sess-local", UserID: e.user(t, "admin").ID, CSRF: "csrf-local", ExpiresAt: time.Now().Add(time.Hour)}
	must(t, e.st.CreateSession(context.Background(), sess))
	cookie := LocalSessionCookie + "=sess-local"
	// The test's server is on this machine, in plain HTTP, with no proxy.
	e.call(t, 200, "", "GET", "/api/v1/jobs", "", "Cookie", cookie)
	contains(t, e.object(t, 403, "", "POST", "/api/v1/admin/reload", "", "Cookie", cookie)["detail"].(string), "CSRF")
	e.call(t, 200, "", "POST", "/api/v1/admin/reload", "", "Cookie", cookie, CSRFHeader, "csrf-local")
	for _, hdr := range [][2]string{{"X-Forwarded-Proto", "https"}, {"X-Forwarded-For", "203.0.113.9"}, {"Forwarded", "proto=https"}} {
		e.call(t, 401, "", "GET", "/api/v1/jobs", "", "Cookie", cookie, hdr[0], hdr[1])
	}
	req := e.request(context.Background(), t, "", "GET", "/api/v1/jobs", nil, "Cookie", cookie)
	req.Host = "ci.example.com"
	code, _, _ := e.send(t, req)
	eq(t, "under a public name", code, 401)
	// The Secure cookie logs in wherever it arrives, as it did.
	e.call(t, 200, "", "GET", "/api/v1/jobs", "", "Cookie", SessionCookie+"=sess-local", "X-Forwarded-Proto", "https")
	// A dead Secure cookie beside a live local one does not shut the reader out.
	e.call(t, 200, "", "GET", "/api/v1/jobs", "", "Cookie", SessionCookie+"=nobody; "+cookie)
}

func TestAJobSaysTheZoneItsCronIsReadInAndItsNextBuildIsInIt(t *testing.T) {
	e := newEnv(t)
	jobs := filepath.Join(e.dir, "jobs")
	must(t, os.WriteFile(filepath.Join(jobs, "tokyo.yml"), []byte("name: tokyo\ntimezone: Asia/Tokyo\ntriggers: {cron: '0 9 * * *'}\nstages:\n  s: {run: 'true'}\n"), 0o600))
	snap, err := config.Load(jobs)
	must(t, err)
	e.api.deps.Dispatch.SetSnapshot(snap)

	body, _ := e.call(t, 200, e.admin, "GET", "/api/v1/jobs/tokyo", "")
	var job struct {
		TimeZone string    `json:"timezone"`
		Next     time.Time `json:"next"`
	}
	must(t, json.Unmarshal([]byte(body), &job))
	eq(t, "timezone", job.TimeZone, "Asia/Tokyo")
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	must(t, err)
	at := job.Next.In(tokyo)
	if at.Hour() != 9 || at.Minute() != 0 || time.Until(at) > 24*time.Hour || time.Until(at) <= 0 {
		t.Fatalf("next %v is not the coming 09:00 in Tokyo", at)
	}

	body, _ = e.call(t, 200, e.admin, "GET", "/api/v1/jobs/gc", "")
	must(t, json.Unmarshal([]byte(body), &job))
	eq(t, "the zone of a job that names none, on a server that names none", job.TimeZone, time.Local.String())
}

// Asked for, a stream says when each line began, in an event of its own
// before the line, which a client that did not ask never sees.
func TestAStreamSaysWhenEachLineBeganWhenAsked(t *testing.T) {
	e := newEnv(t)
	id := e.trigger(t, "smoke", "{}")
	e.finished(t, id)
	path := "/api/v1/builds/" + id + "/stages/s/log"
	got, _ := e.call(t, 200, e.admin, "GET", path+"/events?timestamps=true", "")
	stamped := regexp.MustCompile(`^event: time\ndata: (\S+)\n\nid: 11\ndata: tier=quick\n\nevent: time\ndata: (\S+)\n\nid: 16\ndata: done\n\n` + regexp.QuoteMeta(sseEnd) + `$`)
	m := stamped.FindStringSubmatch(got)
	if m == nil {
		t.Fatalf("the stream with times: %q", got)
	}
	began, err := time.Parse(millisLayout, m[1])
	if err != nil || time.Since(began) > time.Minute {
		t.Fatalf("the first line began at %q: %v", m[1], err)
	}
	got, _ = e.call(t, 200, e.admin, "GET", path+"/events", "")
	eq(t, "the stream without", got, "id: 11\ndata: tier=quick\n\nid: 16\ndata: done\n\n"+sseEnd)
	got, _ = e.call(t, 200, e.admin, "GET", path+"/events?timestamps=true&offset=11", "")
	if !regexp.MustCompile(`^event: time\ndata: \S+Z\n\nid: 16\ndata: done\n\n`).MatchString(got) {
		t.Fatalf("the stream with times from the middle: %q", got)
	}

	text, _ := e.call(t, 200, e.admin, "GET", path+"?download=true&timestamps=true", "")
	if !regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}(Z|[+-]\d\d:\d\d) tier=quick\n\S+ done\n$`).MatchString(text) {
		t.Fatalf("the download with times: %q", text)
	}
	text, _ = e.call(t, 200, e.admin, "GET", path+"?download=true", "")
	eq(t, "the download without", text, "tier=quick\ndone\n")
}

// A log written before times were kept streams and downloads as it is.
func TestALogWithNoTimesHasNone(t *testing.T) {
	e := newEnv(t)
	id := e.trigger(t, "smoke", "{}")
	e.finished(t, id)
	dir := filepath.Join(e.dir, "logs", "smoke", "1")
	must(t, os.Remove(filepath.Join(dir, "s.log"+logtime.Suffix)))
	path := "/api/v1/builds/" + id + "/stages/s/log"
	got, _ := e.call(t, 200, e.admin, "GET", path+"/events?timestamps=true", "")
	eq(t, "the stream", got, "id: 11\ndata: tier=quick\n\nid: 16\ndata: done\n\n"+sseEnd)
	text, _ := e.call(t, 200, e.admin, "GET", path+"?download=true&timestamps=true", "")
	eq(t, "the download", text, "tier=quick\ndone\n")
}

// A line too long to hold goes out unstamped, and the lines after it
// still have their times.
func TestADownloadWithTimesSendsALineTooLongToHoldAsItIs(t *testing.T) {
	e := newEnv(t)
	id := e.trigger(t, "smoke", "{}")
	e.finished(t, id)
	dir := filepath.Join(e.dir, "logs", "smoke", "1")
	var log, times bytes.Buffer
	w := logtime.NewWriter(&log, 0, &times)
	long := strings.Repeat("x", logChunk+10)
	for _, p := range []string{"first\n", long + "\n", "after\n"} {
		_, err := w.Write([]byte(p))
		must(t, err)
	}
	must(t, w.Close())
	must(t, os.WriteFile(filepath.Join(dir, "s.log"), log.Bytes(), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "s.log"+logtime.Suffix), times.Bytes(), 0o600))
	text, _ := e.call(t, 200, e.admin, "GET", "/api/v1/builds/"+id+"/stages/s/log?download=true&timestamps=true", "")
	lines := strings.Split(text, "\n")
	stamp := regexp.MustCompile(`^\S+ `)
	if len(lines) != 4 || !stamp.MatchString(lines[0]) || lines[1] != long || !stamp.MatchString(lines[2]) || !strings.HasSuffix(lines[2], " after") {
		t.Fatalf("the download: %d lines, %.80q ... %.80q", len(lines), text, lines[len(lines)-2])
	}
}

// The description of the API is served by the API it describes, to anyone:
// a client reads it before it has a token.
func TestTheSpecIsServedToAnyoneAsTheEmbeddedFile(t *testing.T) {
	e := newEnv(t)
	text, h := e.call(t, 200, "", "GET", "/api/v1/openapi.yaml", "")
	if text != string(openapi.YAML) {
		t.Fatal("the served bytes are not api/openapi.gen.yaml")
	}
	eq(t, "content type", h.Get("Content-Type"), "application/yaml")
	etag := h.Get("ETag")
	if !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) || len(etag) < 10 {
		t.Fatalf("ETag = %q, want a quoted content hash", etag)
	}

	body, h2 := e.call(t, 304, "", "GET", "/api/v1/openapi.yaml", "", "If-None-Match", etag)
	eq(t, "a 304 has no body", body, "")
	eq(t, "the 304 repeats the ETag", h2.Get("ETag"), etag)
	e.call(t, 200, "", "GET", "/api/v1/openapi.yaml", "", "If-None-Match", `"other"`)
}

func TestTheSpecIsOnlyServedUnderV1(t *testing.T) {
	e := newEnv(t)
	e.call(t, 404, "", "GET", "/api/openapi.yaml", "")
}
