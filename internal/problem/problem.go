// Package problem is how every machine-facing endpoint under /api/v1 answers
// a 4xx or a 5xx: an RFC 9457 problem document, application/problem+json,
// with a stable code for programs to decide by. The codes are listed, with
// their meaning, in api/problems.md, and a test holds that file to this one.
package problem

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
)

// MediaType is the Content-Type of a problem.
const MediaType = "application/problem+json"

// TypeBase is what a problem's type is, with the code after the #.
const TypeBase = "https://github.com/ndthuan/kipp/blob/main/api/problems.md#"

// Code is one kind of problem: its name, the status it is answered with and
// its fixed one-line summary.
type Code struct {
	Name   string
	Status int
	Title  string
}

var registry []Code

func define(name string, status int, title string) Code {
	c := Code{Name: name, Status: status, Title: title}
	registry = append(registry, c)
	return c
}

// All is every code, in the order api/problems.md lists them.
func All() []Code { return slices.Clone(registry) }

// The codes. A status can have several: a client decides by the code, and
// by the status when it knows none of them.
var (
	BadInput = define("bad_input", http.StatusBadRequest, "The request is not valid")
	// BadParams is a build's parameters the job refuses.
	BadParams = define("bad_params", http.StatusBadRequest, "The build parameters are refused")

	Unauthorized = define("unauthorized", http.StatusUnauthorized, "A login or a token is required")
	SignedOut    = define("signed_out", http.StatusUnauthorized, "The session or token is no longer valid")

	Forbidden     = define("forbidden", http.StatusForbidden, "Not allowed")
	CSRF          = define("csrf", http.StatusForbidden, "The CSRF token is missing or wrong")
	SudoRequired  = define("sudo_required", http.StatusForbidden, "This change needs the password again")
	WrongPassword = define("wrong_password", http.StatusForbidden, "The password is wrong")

	NotFound         = define("not_found", http.StatusNotFound, "Not found")
	MethodNotAllowed = define("method_not_allowed", http.StatusMethodNotAllowed, "Method not allowed")
	RequestTimeout   = define("request_timeout", http.StatusRequestTimeout, "The request took too long to arrive")

	Conflict          = define("conflict", http.StatusConflict, "The request conflicts with the state of the server")
	JobDisabled       = define("job_disabled", http.StatusConflict, "The job is disabled")
	Dropped           = define("dropped", http.StatusConflict, "The job is running or already queued, and drops builds")
	OwnerJob          = define("owner_job", http.StatusConflict, "An owner job builds nothing itself")
	NoDefaultChild    = define("no_default_child", http.StatusConflict, "The multibranch job has no build of its default branch yet")
	ScanRunning       = define("scan_running", http.StatusConflict, "A scan of the repo is already running")
	NoScanner         = define("no_scanner", http.StatusConflict, "This server does not scan")
	NoToken           = define("no_token", http.StatusConflict, "The job names no token to scan with")
	NotCancellable    = define("not_cancellable", http.StatusConflict, "The build can no longer be cancelled")
	DuplicateDelivery = define("duplicate_delivery", http.StatusConflict, "The delivery was received before")
	EditsOff          = define("edits_off", http.StatusConflict, "Job edits are off on this server")
	FilesChanged      = define("files_changed", http.StatusConflict, "The job file changed on disk; reload first")

	PreconditionFailed  = define("precondition_failed", http.StatusPreconditionFailed, "A condition of the request does not hold")
	PreconditionNeeded  = define("precondition_required", http.StatusPreconditionRequired, "The change needs If-Match")
	TooLarge            = define("too_large", http.StatusRequestEntityTooLarge, "The body is too large")
	RangeNotSatisfiable = define("range_not_satisfiable", http.StatusRequestedRangeNotSatisfiable, "The range is outside the file")
	JobsRefused         = define("jobs_refused", http.StatusUnprocessableEntity, "The job files were refused")
	JobRefused          = define("job_refused", http.StatusUnprocessableEntity, "The job was refused")

	TooManyStreams  = define("too_many_streams", http.StatusTooManyRequests, "Too many streams are open for this caller")
	TooManyAttempts = define("too_many_attempts", http.StatusTooManyRequests, "Too many wrong passwords; wait and try again")

	Internal       = define("internal", http.StatusInternalServerError, "Something failed inside the server")
	JobWriteFailed = define("job_write_failed", http.StatusInternalServerError, "The job file could not be written")

	Quiet = define("quiet", http.StatusServiceUnavailable, "The server is quiet and takes no new builds")
	Busy  = define("busy", http.StatusServiceUnavailable, "The server is busy; send the request again")
)

// Doc is the document a problem is answered with.
type Doc struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
	Code   string `json:"code"`
}

// Write answers c with detail, which is for a person.
func Write(w http.ResponseWriter, c Code, detail string) {
	write(w, c.Status, c.doc(detail))
}

func (c Code) doc(detail string) Doc {
	return Doc{Type: TypeBase + c.Name, Title: c.Title, Status: c.Status, Detail: detail, Code: c.Name}
}

func write(w http.ResponseWriter, status int, v any) {
	h := w.Header()
	h.Set("Content-Type", MediaType)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Listed is a problem with the list of what was refused, one item each.
type Listed struct {
	Doc
	Errors []string `json:"errors"`
}

// WriteErrors answers c with detail and every error in errs.
func WriteErrors(w http.ResponseWriter, c Code, detail string, errs []string) {
	write(w, c.Status, Listed{c.doc(detail), errs})
}

// NotFoundHandler answers every request with 404.
func NotFoundHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { Write(w, NotFound, "no such path") })
}

// probed are the methods a route may be served for.
var probed = []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete}

// Fallback is the handler of mux's catch-all pattern, prefix: the path
// nothing serves is a 404 problem, and a path some other method serves is
// a 405 problem with the methods in Allow. (A ServeMux sends a request for
// a path it has under another method to the catch-all too, which is what
// makes the two tellable apart here.)
func Fallback(mux *http.ServeMux, prefix string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var allow []string
		for _, m := range probed {
			probe := r.Clone(r.Context())
			probe.Method = m
			if _, pattern := mux.Handler(probe); pattern != "" && pattern != prefix {
				allow = append(allow, m)
			}
		}
		if len(allow) == 0 {
			Write(w, NotFound, "no such path")
			return
		}
		w.Header().Set("Allow", strings.Join(allow, ", "))
		Write(w, MethodNotAllowed, r.Method+" is not allowed here; allowed: "+strings.Join(allow, ", "))
	})
}

// Catch is w with the errors net/http writes by itself (the 416 of
// ServeContent) answered as problems.
func Catch(w http.ResponseWriter) http.ResponseWriter { return &catcher{ResponseWriter: w} }

type catcher struct {
	http.ResponseWriter
	caught bool
}

// WriteHeader answers a failure as a problem, and lets anything below 400 by.
func (c *catcher) WriteHeader(status int) {
	if status < http.StatusBadRequest {
		c.ResponseWriter.WriteHeader(status)
		return
	}
	c.caught = true
	code := Internal
	switch status {
	case RangeNotSatisfiable.Status:
		code = RangeNotSatisfiable
	case PreconditionFailed.Status:
		code = PreconditionFailed
	}
	Write(c.ResponseWriter, code, http.StatusText(status))
}

// Write drops the plain text of a failure already answered as a problem.
func (c *catcher) Write(p []byte) (int, error) {
	if c.caught { // the plain text net/http meant to say
		return len(p), nil
	}
	return c.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController reach the writer beneath.
func (c *catcher) Unwrap() http.ResponseWriter { return c.ResponseWriter }
