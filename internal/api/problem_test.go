package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/dispatch"
	"github.com/ndthuan/kipp/internal/index"
	"github.com/ndthuan/kipp/internal/problem"
	"github.com/ndthuan/kipp/internal/store"
)

// asProblem is a call that must answer with the problem code, at status.
func (e *env) asProblem(t *testing.T, status int, code, tok, method, path, body string, hdr ...string) (problem.Doc, http.Header) {
	t.Helper()
	text, h := e.call(t, status, tok, method, path, body, hdr...)
	eq(t, method+" "+path+" content type", h.Get("Content-Type"), problem.MediaType)
	var d problem.Doc
	if err := json.Unmarshal([]byte(text), &d); err != nil {
		t.Fatalf("%s %s: not a problem: %q", method, path, text)
	}
	eq(t, method+" "+path+" code", d.Code, code)
	eq(t, method+" "+path+" status member", d.Status, status)
	eq(t, method+" "+path+" type", d.Type, problem.TypeBase+code)
	if d.Title == "" || d.Detail == "" {
		t.Fatalf("%s %s: a problem with no title or no detail: %q", method, path, text)
	}
	for _, sentinel := range []string{"bad input: ", "conflict: "} {
		if strings.HasPrefix(d.Detail, sentinel) {
			t.Errorf("%s %s: detail %q keeps the Go sentinel prefix", method, path, d.Detail)
		}
	}
	return d, h
}

func TestUnderV1AFailureIsAProblemWhateverTheCause(t *testing.T) {
	e := newEnv(t)
	e.writeJob(t, "off", "name: off\ndisabled: true\ntriggers: {cron: '*/10 * * * *'}\nstages:\n  s: {run: 'true'}\n")
	e.asProblem(t, 401, "unauthorized", "", "GET", "/api/v1/jobs", "")
	e.asProblem(t, 404, "not_found", e.agent, "GET", "/api/v1/jobs/gc", "")
	e.asProblem(t, 403, "forbidden", e.viewer, "POST", "/api/v1/jobs/smoke/builds", "{}")
	e.asProblem(t, 400, "bad_params", e.agent, "POST", "/api/v1/jobs/smoke/builds", `{"params": {"TIER": "nightly"}}`)
	e.asProblem(t, 400, "bad_input", e.agent, "POST", "/api/v1/jobs/smoke/builds", `not JSON at all`)
	e.asProblem(t, 409, "job_disabled", e.admin, "POST", "/api/v1/jobs/off/builds", "")
	e.asProblem(t, 413, "too_large", e.agent, "POST", "/api/v1/jobs/smoke/builds", `{"params": {"TIER": "`+strings.Repeat("a", 2*MaxBody)+`"}}`)
	once := e.started(t, "once")
	d, _ := e.asProblem(t, 409, "dropped", e.admin, "POST", "/api/v1/jobs/once/builds", "")
	contains(t, d.Detail, "dropped")
	e.call(t, 202, e.admin, "POST", "/api/v1/builds/"+once+"/cancel", "")
	e.finished(t, once)
	e.asProblem(t, 409, "not_cancellable", e.admin, "POST", "/api/v1/builds/"+once+"/cancel", "")
}

func TestAnArtifactRangeOutsideTheFileIsAProblem(t *testing.T) {
	e := newEnv(t)
	id := e.kept(t, "smoke", store.Failed, "s", map[string]string{"a.txt": "short"})
	url := "/api/v1/builds/" + itoa64(id) + "/stages/s/artifacts/a.txt"
	_, h := e.asProblem(t, 416, "range_not_satisfiable", e.viewer, "GET", url, "", "Range", "bytes=100-200")
	eq(t, "content range", h.Get("Content-Range"), "bytes */5")
	// A range inside it is a part.
	body, _ := e.call(t, 206, e.viewer, "GET", url, "", "Range", "bytes=1-2")
	eq(t, "part", body, "ho")
}

func TestACsrfRefusalAndAReloadAreProblemsToo(t *testing.T) {
	e := newEnv(t)
	sess := &store.Session{ID: "sess-p", UserID: e.user(t, "admin").ID, CSRF: "c", ExpiresAt: time.Now().Add(time.Hour)}
	must(t, e.st.CreateSession(t.Context(), sess))
	e.asProblem(t, 403, "csrf", "", "POST", "/api/v1/admin/reload", "", "Cookie", SessionCookieFor(sess).String())
	d, _ := e.asProblem(t, 403, "forbidden", e.agent, "POST", "/api/v1/admin/reload", "")
	contains(t, d.Detail, "not allowed")
}

func TestAReloadIs422OnlyForRefusedJobFilesAndOtherFailuresAre500(t *testing.T) {
	refused := &JobsRefused{Err: errors.New("smoke.yml: no stages")}
	e := newEnv(t, func(a *API) {
		a.deps.Reload = func(context.Context) error { return refused }
	})
	d, _ := e.asProblem(t, 422, "jobs_refused", e.admin, "POST", "/api/v1/admin/reload", "")
	eq(t, "detail", d.Detail, "smoke.yml: no stages")

	other := newEnv(t, func(a *API) {
		a.deps.Reload = func(context.Context) error { return errors.New("disk on fire") }
	})
	d, _ = other.asProblem(t, 500, "internal", other.admin, "POST", "/api/v1/admin/reload", "")
	eq(t, "the cause stays in the log", d.Detail, "internal error")
}

func TestAnUnknownPathIs404AndAWrongMethodIs405WithAllow(t *testing.T) {
	e := newEnv(t)
	e.asProblem(t, 404, "not_found", e.admin, "GET", "/api/v1/nothing", "")
	e.asProblem(t, 404, "not_found", "", "GET", "/api/v1/jobs/smoke/nothing", "")
	_, h := e.asProblem(t, 405, "method_not_allowed", e.admin, "POST", "/api/v1/jobs", "")
	eq(t, "Allow", h.Get("Allow"), "GET, HEAD")
	_, h = e.asProblem(t, 405, "method_not_allowed", e.admin, "DELETE", "/api/v1/jobs/smoke/builds", "")
	eq(t, "Allow", h.Get("Allow"), "GET, HEAD, POST")
	_, h = e.asProblem(t, 405, "method_not_allowed", e.admin, "PUT", "/api/v1/admin/quiet", "")
	eq(t, "Allow", h.Get("Allow"), "GET, HEAD, POST, DELETE")
	// So is a path outside /api/v1.
	e.asProblem(t, 404, "not_found", e.admin, "GET", "/api/nothing", "")
}

func TestEveryServiceErrorMapsToItsCode(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		err    error
		code   string
		status int
		detail string
	}{
		{ErrNotFound, "not_found", 404, "not found"},
		{ErrForbidden, "forbidden", 403, "not allowed"},
		{fmt.Errorf("%w: %w", ErrConflict, dispatch.ErrDisabled), "job_disabled", 409, "the job is disabled"},
		{fmt.Errorf("%w: %w", ErrConflict, dispatch.ErrDropped), "dropped", 409, dispatch.ErrDropped.Error()},
		{fmt.Errorf("%w: %w", ErrConflict, dispatch.ErrOwnerJob), "owner_job", 409, dispatch.ErrOwnerJob.Error()},
		{fmt.Errorf("%w: %w", ErrConflict, dispatch.ErrTooLate), "not_cancellable", 409, dispatch.ErrTooLate.Error()},
		{fmt.Errorf("%w: %w", ErrConflict, index.ErrScanning), "scan_running", 409, index.ErrScanning.Error()},
		{fmt.Errorf("%w: %w", ErrConflict, index.ErrNoToken), "no_token", 409, index.ErrNoToken.Error()},
		{fmt.Errorf("%w: %w", ErrConflict, ErrNoScanner), "no_scanner", 409, ErrNoScanner.Error()},
		{fmt.Errorf("%w: user %q already exists", ErrConflict, "x"), "conflict", 409, `user "x" already exists`},
		{fmt.Errorf("%w: %w", ErrBadInput, dispatch.ErrBadParams), "bad_params", 400, dispatch.ErrBadParams.Error()},
		{fmt.Errorf("%w: before is a build id", ErrBadInput), "bad_input", 400, "before is a build id"},
		{ErrQuiet, "quiet", 503, ErrQuiet.Error()},
		{errTooManyStreams, "too_many_streams", 429, errTooManyStreams.Error()},
		{errSignedOut, "signed_out", 401, errSignedOut.Error()},
		{ErrSudoRequired, "sudo_required", 403, ErrSudoRequired.Error()},
		{ErrWrongPassword, "wrong_password", 403, "wrong password"},
		{&auth.ThrottledError{RetryAfter: time.Second}, "too_many_attempts", 429, ""},
		{auth.ErrBusy, "busy", 503, auth.ErrBusy.Error()},
		{errors.New("disk on fire"), "internal", 500, "internal error"},
		{fmt.Errorf("%w: apps", dispatch.ErrNoDefaultChild), "no_default_child", 409, dispatch.ErrNoDefaultChild.Error() + ": apps"},
	} {
		w := httptest.NewRecorder()
		e.api.fail(w, httptest.NewRequest("POST", "/api/v1/x", nil), c.err)
		var d problem.Doc
		must(t, json.Unmarshal(w.Body.Bytes(), &d))
		if w.Code != c.status || d.Code != c.code || d.Status != c.status || w.Header().Get("Content-Type") != problem.MediaType {
			t.Errorf("%v: %d %s", c.err, w.Code, w.Body)
		}
		if c.detail != "" && d.Detail != c.detail {
			t.Errorf("%v: detail %q, want %q", c.err, d.Detail, c.detail)
		}
	}
}

func TestATooLargeBodyIsAProblemUnderV1(t *testing.T) {
	e := newEnv(t)
	w := httptest.NewRecorder()
	e.api.fail(w, httptest.NewRequest("POST", "/api/v1/x", nil), &http.MaxBytesError{Limit: MaxBody})
	var d problem.Doc
	must(t, json.Unmarshal(w.Body.Bytes(), &d))
	eq(t, "code", d.Code, "too_large")
	eq(t, "status", w.Code, 413)
}
