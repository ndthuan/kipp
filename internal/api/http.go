package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	openapi "github.com/ndthuan/kipp/api"
	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/dispatch"
	"github.com/ndthuan/kipp/internal/index"
	"github.com/ndthuan/kipp/internal/problem"
	"github.com/ndthuan/kipp/internal/store"
)

// MaxBody is the most any request may send, in bytes; the one body read
// is a trigger's params.
const MaxBody = 64 << 10

// PrefixV1 is where the API is served.
const PrefixV1 = "/api/v1"

// Route is one row of the API: a method and path (without the prefix) and
// the handler.
type Route struct {
	Method  string
	Path    string
	handler http.HandlerFunc
}

// Pattern is the row as a ServeMux pattern.
func (r Route) Pattern() string { return r.Method + " " + PrefixV1 + r.Path }

// Routes is every route of the API.
func (a *API) Routes() []Route {
	user := a.withUser
	row := func(method, path string, h http.HandlerFunc) Route { return Route{method, path, h} }
	return []Route{
		row("GET", "/health", a.health),
		row("GET", "/openapi.yaml", a.openapi),
		row("GET", "/jobs", user(a.jobs)),
		row("GET", "/jobs/{name}", user(a.jobDetail)),
		row("GET", "/jobs/{name}/builds", user(a.builds)),
		row("POST", "/jobs/{name}/builds", user(a.trigger)),
		row("GET", "/jobs/{name}/builds/{number}", user(a.buildNumbered)),
		row("POST", "/jobs/{name}/scan", user(a.scan)),
		row("GET", "/jobs/{name}/activity", user(a.activity)),
		row("GET", "/jobs/{name}/children", user(a.children)),
		row("GET", "/builds/{id}", user(a.build)),
		row("POST", "/builds/{id}/cancel", user(a.cancel)),
		row("POST", "/builds/{id}/again", user(a.again)),
		row("PUT", "/builds/{id}/keep", user(a.keep)),
		row("DELETE", "/builds/{id}/keep", user(a.keep)),
		row("GET", "/builds/{id}/stages/{stage}/log", user(a.log)),
		row("GET", "/builds/{id}/stages/{stage}/log/events", user(a.logEvents)),
		row("GET", "/builds/{id}/results", user(a.results)),
		row("GET", "/builds/{id}/metrics", user(a.buildMetrics)),
		row("GET", "/builds/{id}/artifacts", user(a.artifactList)),
		row("GET", "/builds/{id}/stages/{stage}/artifacts", user(a.stageArtifactList)),
		row("GET", "/builds/{id}/stages/{stage}/artifacts.zip", user(a.artifactZip)),
		row("GET", "/builds/{id}/stages/{stage}/artifacts/{path...}", user(a.artifactFile)),
		row("GET", "/jobs/{name}/artifacts/latest", user(a.latestArtifacts)),
		row("GET", "/metrics", user(a.metricStores)),
		row("GET", "/metrics/{store}", user(a.metricStore)),
		row("GET", "/metrics/{store}/remembered", user(a.remembered)),
		row("GET", "/metrics/{store}/history", user(a.metricHistory)),
		row("GET", "/metrics/{store}/compare", user(a.compare)),
		row("POST", "/admin/reload", user(a.reload)),
		row("GET", "/admin/quiet", user(a.quiet)),
		row("POST", "/admin/quiet", user(a.quietOn)),
		row("DELETE", "/admin/quiet", user(a.quietOff)),
		row("GET", "/admin/upgrade", user(a.upgrade)),
		row("POST", "/admin/upgrade", user(a.upgradeStart)),
		row("DELETE", "/admin/upgrade", user(a.upgradeCancel)),
		row("POST", "/admin/upgrade/rollback", user(a.rollbackStart)),
		row("GET", "/overview", user(a.overview)),
		row("GET", "/queue", user(a.queue)),
		row("GET", "/queue/events", user(a.queueEvents)),
		row("GET", "/admin/users", user(a.users)),
		row("GET", "/admin/users/{name}", user(a.userDetail)),
		row("GET", "/admin/secrets", user(a.secrets)),
		row("GET", "/admin/server", user(a.server)),
		row("GET", "/admin/engines", user(a.engines)),
		row("GET", "/admin/agents", user(a.agents)),
		row("GET", "/admin/cache", user(a.cacheList)),
		row("GET", "/admin/audit", user(a.audit)),
	}
}

// Handler is every /api/v1/ route, authentication included.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, r := range a.Routes() {
		mux.HandleFunc(r.Pattern(), r.handler)
	}
	// What /api/ has no route for is a problem, not the mux's plain text.
	mux.Handle("/api/", problem.Fallback(mux, "/api/"))
	bounded := http.MaxBytesHandler(mux, MaxBody)
	authed := a.Authenticate(bounded)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A probe is answered even when the store is not, and the description of the API before the caller has a token.
		if r.URL.Path == PrefixV1+"/health" || r.URL.Path == PrefixV1+"/openapi.yaml" {
			bounded.ServeHTTP(w, r)
			return
		}
		authed.ServeHTTP(w, r)
	})
}

// sealed marks an answer as not to be sniffed into another type and not
// to be kept by a cache.
func sealed(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
}

type handler func(w http.ResponseWriter, r *http.Request, u *store.User)

func (a *API) withUser(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := UserFrom(r.Context())
		if u == nil {
			problem.Write(w, problem.Unauthorized, "login or a token is required")
			return
		}
		h(w, r, u)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	sealed(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// fail maps a Service error to a status. A hidden job says only "not
// found", and what went wrong inside the server goes to the log, not out.
// A password refused is 403, never 401: 401 says the token is bad.
func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	if refusedPassword(w, err) {
		return
	}
	var tooBig *http.MaxBytesError
	msg := err.Error()
	switch {
	case errors.Is(err, ErrNotFound):
		problem.Write(w, problem.NotFound, "not found")
	case errors.Is(err, ErrForbidden):
		problem.Write(w, problem.Forbidden, "not allowed")
	case errors.Is(err, dispatch.ErrNoDefaultChild):
		problem.Write(w, problem.NoDefaultChild, msg)
	case errors.Is(err, ErrConflict):
		problem.Write(w, conflictCode(err), without(msg, ErrConflict))
	case errors.Is(err, ErrQuiet):
		retryAfter(w, quietRetry)
		problem.Write(w, problem.Quiet, msg)
	case errors.Is(err, errTooManyStreams):
		problem.Write(w, problem.TooManyStreams, msg)
	case errors.Is(err, errSignedOut):
		problem.Write(w, problem.SignedOut, msg)
	case errors.As(err, &tooBig):
		problem.Write(w, problem.TooLarge, "the request body is over "+strconv.Itoa(MaxBody)+" bytes")
	case errors.Is(err, ErrBadInput):
		code := problem.BadInput
		if errors.Is(err, dispatch.ErrBadParams) {
			code = problem.BadParams
		}
		problem.Write(w, code, without(msg, ErrBadInput))
	case r.Context().Err() != nil: // the client left; nobody to answer
	default:
		a.deps.Logger.Error("api", "method", r.Method, "path", r.URL.Path, "err", err)
		problem.Write(w, problem.Internal, "internal error")
	}
}

// without is msg without the sentinel it begins with, which is for the
// program that wrapped the error and not for the person who reads it.
func without(msg string, sentinel error) string {
	return strings.TrimPrefix(msg, sentinel.Error()+": ")
}

// conflictCode is the code of a conflict: the specific one when the error
// names why, the common one when it does not.
func conflictCode(err error) problem.Code {
	switch {
	case errors.Is(err, dispatch.ErrDisabled):
		return problem.JobDisabled
	case errors.Is(err, dispatch.ErrDropped):
		return problem.Dropped
	case errors.Is(err, dispatch.ErrOwnerJob):
		return problem.OwnerJob
	case errors.Is(err, dispatch.ErrTooLate):
		return problem.NotCancellable
	case errors.Is(err, index.ErrScanning):
		return problem.ScanRunning
	case errors.Is(err, index.ErrNoToken):
		return problem.NoToken
	case errors.Is(err, ErrNoScanner):
		return problem.NoScanner
	}
	return problem.Conflict
}

// refusedPassword answers err when it is about a password: one needed
// again, a wrong one, or one that was not checked, with when to try again.
func refusedPassword(w http.ResponseWriter, err error) bool {
	var throttled *auth.ThrottledError
	switch {
	case errors.Is(err, ErrSudoRequired):
		problem.Write(w, problem.SudoRequired, err.Error())
	case errors.Is(err, ErrWrongPassword):
		problem.Write(w, problem.WrongPassword, "wrong password")
	case errors.As(err, &throttled):
		retryAfter(w, throttled.RetryAfter)
		problem.Write(w, problem.TooManyAttempts, throttled.Error())
	case errors.Is(err, auth.ErrBusy):
		retryAfter(w, busyWait)
		problem.Write(w, problem.Busy, auth.ErrBusy.Error())
	default:
		return false
	}
	return true
}

// busyWait is when a password that found every check taken is worth
// sending again.
const busyWait = 2 * time.Second

// retryAfter says in whole seconds, rounded up so that it is never too
// early, when to send the request again.
func retryAfter(w http.ResponseWriter, d time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(d.Seconds()))))
}

// stamp is a time as the API answers it: RFC 3339 in UTC with exactly
// three fraction digits and a Z, whatever zone the server runs in and
// wherever the time came from (the store's are UTC, the clock's and the
// schedule's are local). Every time in an answer is one, so that none can
// be left in the server's zone.
type stamp time.Time

// millisLayout is the one form of a time in an answer.
const millisLayout = "2006-01-02T15:04:05.000Z"

// MarshalJSON implements json.Marshaler.
func (s stamp) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Time(s).UTC().Format(millisLayout))
}

func stampOf(t time.Time) stamp { return stamp(t) }

func stampPtr(t *time.Time) *stamp {
	if t == nil {
		return nil
	}
	return new(stampOf(*t))
}

// stampSince is the time of an event that may not have happened: none for
// the zero time.
func stampSince(t time.Time) *stamp {
	if t.IsZero() {
		return nil
	}
	return new(stampOf(t))
}

type stageJSON struct {
	Name       string            `json:"name"`
	Needs      []string          `json:"needs"`
	Status     store.StageStatus `json:"status"`
	ExitCode   *int              `json:"exit_code"`
	StartedAt  *stamp            `json:"started_at"`
	FinishedAt *stamp            `json:"finished_at"`
	// RanOn is where it ran: local, an agent, or engine/n for a container.
	RanOn string `json:"ran_on"`
	// Tries is how many times its command ran: 1 unless retries ran it again.
	Tries int `json:"tries"`
}

type activityJSON struct {
	At   stamp  `json:"at"`
	Line string `json:"line"`
}

// resultJSON is one JUnit case of a build.
type resultJSON struct {
	Stage      string `json:"stage"`
	Suite      string `json:"suite"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	DurationMs int    `json:"duration_ms"`
	Message    string `json:"message"`
}

func toActivityJSON(act []store.Activity) []activityJSON {
	out := make([]activityJSON, 0, len(act))
	for _, a := range act {
		out = append(out, activityJSON{At: stampOf(a.At), Line: a.Line})
	}
	return out
}

// health says the server answers. Which version it runs, and since when,
// is told to whoever is logged in alone: a stranger would read in it what
// to try. One whose login cannot be checked is told ok, and the store is
// not asked at all when the request carries no login.
func (a *API) health(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"ok": true}
	if u, _, err := a.who(r); err == nil && u != nil {
		out["version"], out["uptime_seconds"] = a.deps.Version, int(time.Since(a.started).Seconds())
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) jobs(w http.ResponseWriter, r *http.Request, u *store.User) {
	jobs, err := a.svc.Jobs(r.Context(), u)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := make([]jobV1, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, toJobV1(j))
	}
	writeList(w, out, "")
}

func (a *API) jobDetail(w http.ResponseWriter, r *http.Request, u *store.User) {
	v, err := a.svc.Job(r.Context(), u, r.PathValue("name"), 0)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toJobDetailV1(v))
}

// Before is the build number a request's ?before= names, 0 when it has
// none. One that is not a build number is ErrBadInput: read as none, it
// would answer the first page again to a client that is paging.
func Before(r *http.Request) (int, error) {
	raw := r.URL.Query().Get("before")
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%w: before is a build number, 1 or more", ErrBadInput)
	}
	return n, nil
}

// buildsQuery is how many builds a request asks for and the number it asks
// for those below, paged by cursor.
func buildsQuery(r *http.Request) (limit, before int, err error) {
	limit, at, err := pageArgs(r, "builds", 1)
	if err != nil {
		return 0, 0, err
	}
	if at == nil {
		return limit, 0, nil
	}
	n := firstOf(at)
	if n < 1 || n > math.MaxInt32 {
		return 0, 0, fmt.Errorf("%w: cursor is not one this server gave for this list", ErrBadInput)
	}
	return limit, int(n), nil
}

func (a *API) builds(w http.ResponseWriter, r *http.Request, u *store.User) {
	limit, before, err := buildsQuery(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	bs, more, err := a.svc.Builds(r.Context(), u, r.PathValue("name"), before, limit)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	next := ""
	if more {
		next = cursorFor("builds", int64(bs[len(bs)-1].Number))
	}
	writeList(w, toBuildsV1(bs), next)
}

// triggerParams reads {"params": {...}} and nothing after it; no body at
// all is no params.
func triggerParams(r *http.Request) (map[string]string, error) {
	var body struct {
		Params map[string]string `json:"params"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(&body)
	if errors.Is(err, io.EOF) {
		return nil, nil
	}
	if err == nil {
		if _, more := dec.Token(); !errors.Is(more, io.EOF) {
			err = errors.New("more than one JSON value in the body")
		}
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadInput, err)
	}
	return body.Params, nil
}

func (a *API) trigger(w http.ResponseWriter, r *http.Request, u *store.User) {
	name := r.PathValue("name")
	// Before the body is read: what the caller may not do, and what nobody
	// may, is refused the same way whatever they sent.
	err := a.svc.buildable(r.Context(), u, name)
	var params map[string]string
	if err == nil {
		params, err = triggerParams(r)
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	b, err := a.svc.Trigger(r.Context(), u, name, params, "api")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toBuildSummaryV1(b))
}

// scan has a multibranch job's repo read now: 202, the scan being under
// way.
func (a *API) scan(w http.ResponseWriter, r *http.Request, u *store.User) {
	name := r.PathValue("name")
	if err := a.svc.ScanNow(r.Context(), u, name); err != nil {
		a.fail(w, r, err)
		return
	}
	// It says how the scan stands, as the job's page does.
	writeJSON(w, http.StatusAccepted, toScanV1(a.svc.ScanStatus(name)))
}

// activityQuery is how many lines of activity a request asks for and the id
// of the line it asks for those older than.
func activityQuery(r *http.Request) (limit int, before int64, err error) {
	limit, at, err := pageArgs(r, "activity", 1)
	return limit, firstOf(at), err
}

func (a *API) activity(w http.ResponseWriter, r *http.Request, u *store.User) {
	limit, before, err := activityQuery(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	act, more, err := a.svc.Activity(r.Context(), u, r.PathValue("name"), before, limit)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	next := ""
	if more {
		next = cursorFor("activity", act[len(act)-1].ID)
	}
	writeList(w, toActivityJSON(act), next)
}

// BuildID is the {id} of the request's path; one that is not a number
// names no build, which is ErrNotFound.
func BuildID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, ErrNotFound
	}
	return id, nil
}

func (a *API) build(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, err := BuildID(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var v *BuildView
	wait, err := flagOf(r, "wait")
	switch {
	case err != nil:
	case wait:
		v, err = a.waitBuild(r, u, id)
	default:
		v, err = a.svc.Build(r.Context(), u, id)
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toBuildV1(v))
}

// buildNumbered answers a job's build by its number, as /builds/{id}
// does by its id.
func (a *API) buildNumbered(w http.ResponseWriter, r *http.Request, u *store.User) {
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		err = ErrNotFound
	}
	var b *store.Build
	if err == nil {
		b, err = a.svc.BuildNumbered(r.Context(), u, r.PathValue("name"), number)
	}
	var v *BuildView
	if err == nil {
		v, err = a.svc.Build(r.Context(), u, b.ID)
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toBuildV1(v))
}

// waitBuild is the long-poll: the build once it has ended, or as it is when
// the wait is over.
func (a *API) waitBuild(r *http.Request, u *store.User, id int64) (*BuildView, error) {
	if err := drain(r); err != nil {
		return nil, err
	}
	stream, done, err := a.openStream(r, u, func(ctx context.Context, u *store.User) error {
		_, err := a.svc.Build(ctx, u, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	defer done()
	ctx, cancel := context.WithTimeout(stream, a.wait)
	defer cancel()
	v, err := a.svc.Wait(ctx, u, id)
	if why := endedBy(stream); why != nil {
		return nil, why
	}
	return v, err
}

// drain reads what is left of the request body. The server only notices
// that a client has gone once the body has been read, so a handler that
// waits calls this first; without it a GET sent with a body would hold its
// handler until the build ends.
func drain(r *http.Request) error {
	_, err := io.Copy(io.Discard, r.Body)
	return err
}

func (a *API) cancel(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, err := BuildID(r)
	if err == nil {
		err = a.svc.Cancel(r.Context(), u, id)
	}
	var v *BuildView
	if err == nil { // it answers with the build, told to stop
		v, err = a.svc.Build(r.Context(), u, id)
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, toBuildV1(v))
}

// again queues a build like a finished one: 202, with the new build as a
// trigger answers it.
func (a *API) again(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, err := BuildID(r)
	var b *store.Build
	if err == nil {
		b, err = a.svc.Again(r.Context(), u, id, "api")
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, toBuildSummaryV1(b))
}

// keep keeps a finished build out of retention (PUT) or puts it back under
// its job's rules (DELETE), and answers with the build as it is now.
func (a *API) keep(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, err := BuildID(r)
	var v *BuildView
	if err == nil {
		err = a.svc.Keep(r.Context(), u, id, r.Method == http.MethodPut)
	}
	if err == nil {
		v, err = a.svc.Build(r.Context(), u, id)
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toBuildV1(v))
}

func (a *API) results(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, err := BuildID(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var (
		cases []store.Test
		more  bool
	)
	limit, at, err := pageArgs(r, "results", 1)
	if err == nil {
		cases, more, err = a.svc.TestsPage(r.Context(), u, id, firstOf(at), limit)
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := make([]resultJSON, 0, len(cases))
	for _, c := range cases {
		out = append(out, resultJSON{c.Stage, c.Suite, c.Name, c.Status, c.DurationMs, c.Message})
	}
	next := ""
	if more {
		next = cursorFor("results", cases[len(cases)-1].ID)
	}
	writeList(w, out, next)
}

func (a *API) reload(w http.ResponseWriter, r *http.Request, u *store.User) {
	if !u.Admin {
		problem.Write(w, problem.Forbidden, "not allowed")
		return
	}
	if err := a.svc.Reload(r.Context(), u); err != nil {
		// 422 is for job files that are refused.
		if _, refused := errors.AsType[*JobsRefused](err); refused {
			problem.Write(w, problem.JobsRefused, err.Error())
			return
		}
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// quietJSON is the quiet of the server: on, since when, and what still
// runs or waits.
type quietJSON struct {
	Quiet   bool   `json:"quiet"`
	Since   *stamp `json:"since"`
	Running int    `json:"running"`
	Queued  int    `json:"queued"`
	// Idle is true when nothing runs or waits: the server may be restarted.
	Idle bool `json:"idle"`
}

func toQuietJSON(q dispatch.QuietState) quietJSON {
	out := quietJSON{Quiet: q.On, Running: q.Running, Queued: q.Queued, Idle: q.Running+q.Queued == 0}
	if q.On {
		t := stampOf(q.Since)
		out.Since = &t
	}
	return out
}

func (a *API) quiet(w http.ResponseWriter, r *http.Request, u *store.User) {
	q, err := a.svc.Quiet(r.Context(), u)
	a.answerQuiet(w, r, q, err)
}

func (a *API) quietOn(w http.ResponseWriter, r *http.Request, u *store.User) {
	q, err := a.svc.SetQuiet(r.Context(), u, true)
	a.answerQuiet(w, r, q, err)
}

func (a *API) quietOff(w http.ResponseWriter, r *http.Request, u *store.User) {
	q, err := a.svc.SetQuiet(r.Context(), u, false)
	a.answerQuiet(w, r, q, err)
}

func (a *API) answerQuiet(w http.ResponseWriter, r *http.Request, q dispatch.QuietState, err error) {
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toQuietJSON(q))
}

// quietRetry is when a trigger refused for quiet is worth sending again.
const quietRetry = time.Minute

// specETag is the strong validator of the embedded description: a hash of
// its bytes, so a client that has the file asks for it again for nothing.
var specETag = func() string {
	sum := sha256.Sum256(openapi.YAML)
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}()

// openapi serves the description of the API, api/openapi.gen.yaml as it was
// built into this binary. ServeContent answers If-None-Match with a 304.
func (a *API) openapi(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("ETag", specETag)
	w.Header().Set("Cache-Control", "no-cache") // keep it, but ask before using it
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(openapi.YAML))
}
