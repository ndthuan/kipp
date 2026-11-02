// Package apispectest holds a handler under test to the OpenAPI
// description: every response it gives is checked against the operation it
// answers, and a package's TestMain fails naming the operations of its own
// that no test got a good answer from. It is for tests only.
package apispectest

import (
	"bytes"
	"context"
	"fmt"
	"mime"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	openapi "github.com/ndthuan/kipp/api"
	"github.com/ndthuan/kipp/internal/apispec"
)

var (
	loadOnce sync.Once
	spec     *Spec
	loadErr  error
)

// Spec is a description to check responses against, and which of its
// operations have had a good one.
type Spec struct {
	Doc *openapi3.T
	ops []apispec.Op

	mu      sync.Mutex
	covered map[string]bool // operation key -> a good response was checked
}

// Parse is the spec in data, a bundled file.
func Parse(data []byte) (*Spec, error) {
	doc, err := apispec.Parse(data)
	if err != nil {
		return nil, err
	}
	return &Spec{Doc: doc, ops: apispec.Operations(doc), covered: map[string]bool{}}, nil
}

// Load is the committed bundle, api/openapi.gen.yaml, as embedded.
func Load() (*Spec, error) {
	loadOnce.Do(func() { spec, loadErr = Parse(openapi.YAML) })
	return spec, loadErr
}

// Operations are the operations of the spec.
func (s *Spec) Operations() []apispec.Op { return s.ops }

// Wrap is next with each response it gives checked against the spec: a
// status the operation does not document, a type or a header it does not
// allow, a JSON body its schema refuses (an extra field, a missing one, a
// wrong type). A request that names no operation is not checked. A failure
// is reported when t's test ends, so a stream that outlives the test
// cannot fail it from another goroutine. A good response (below 400) is
// counted for the operation, which Main reads.
func Wrap(t testing.TB, next http.Handler) http.Handler {
	t.Helper()
	s, err := Load()
	if err != nil {
		t.Fatalf("the spec does not load: %v", err)
	}
	return s.Wrap(t, next)
}

// Wrap is the package's Wrap against s.
func (s *Spec) Wrap(t testing.TB, next http.Handler) http.Handler {
	t.Helper()
	var (
		failures []string
		failMu   sync.Mutex
		ended    bool
	)
	t.Cleanup(func() {
		failMu.Lock()
		defer failMu.Unlock()
		ended = true
		if len(failures) > 0 {
			t.Errorf("%d response(s) the spec does not describe:\n%s", len(failures), strings.Join(failures, "\n"))
		}
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &recorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 && r.Context().Err() != nil {
			return // a long-poll whose client left: nothing was answered
		}
		if err := s.Check(r, rec.code(), w.Header(), rec.body.Bytes()); err != nil {
			failMu.Lock()
			defer failMu.Unlock()
			if !ended {
				failures = append(failures, fmt.Sprintf("  %s %s: %v", r.Method, r.URL.RequestURI(), err))
			}
		}
	})
}

// recorder keeps what a handler writes, and passes it on.
type recorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

// WriteHeader keeps the first final status, as the server does.
func (r *recorder) WriteHeader(code int) {
	if r.status == 0 && (code >= 200 || code == http.StatusSwitchingProtocols) {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

// Write keeps what the handler writes, and passes it on.
func (r *recorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	r.body.Write(p)
	return r.ResponseWriter.Write(p)
}

// Unwrap lets http.NewResponseController reach the connection: streams
// flush through it.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *recorder) code() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

// Check is the answer given to r checked against the operation it is for,
// nil when it is allowed or when r is for none.
func (s *Spec) Check(r *http.Request, status int, header http.Header, body []byte) error {
	op, ok := s.find(r)
	if !ok || r.Method == http.MethodHead {
		return nil
	}
	if err := check(s.Doc, op, r, status, header, body); err != nil {
		return err
	}
	if status < http.StatusBadRequest {
		s.mu.Lock()
		s.covered[op.Key()] = true
		s.mu.Unlock()
	}
	return nil
}

func check(doc *openapi3.T, op apispec.Op, r *http.Request, status int, header http.Header, body []byte) error {
	responses := op.Operation.Responses
	ref := responses.Status(status)
	if ref == nil {
		ref = responses.Default()
	}
	if ref == nil && muxRedirect(status, header) {
		return nil // the ServeMux cleaning a path, before any handler
	}
	if ref == nil || ref.Value == nil {
		return fmt.Errorf("%s answered %d, which the spec does not document", op.Key(), status)
	}
	in := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{
			Request: r,
			Route: &routers.Route{Spec: doc, Path: op.Path, PathItem: doc.Paths.Value(op.Path), Method: op.Method,
				Operation: op.Operation},
		},
		Status:  status,
		Header:  typed(header, body),
		Options: &openapi3filter.Options{IncludeResponseStatus: true, ExcludeResponseBody: true},
	}
	in.SetBodyBytes(body)
	content := ref.Value.Content
	switch {
	case status >= 300 && status < 400:
	case len(content) == 0 && len(body) > 0:
		return fmt.Errorf("%s answered %d with a %d-byte body, which the spec describes as empty", op.Key(), status, len(body))
	case len(content) > 0:
		ct := in.Header.Get("Content-Type")
		if content.Get(ct) == nil {
			return fmt.Errorf("%s answered %d as %q, which the spec does not allow (%s)", op.Key(), status, ct, strings.Join(types(content), ", "))
		}
		in.Options.ExcludeResponseBody = !jsonType(ct)
	}
	return openapi3filter.ValidateResponse(context.Background(), in)
}

// muxRedirect is whether an answer is the redirect a ServeMux gives a path
// it cleans (a.. or a double slash), which no operation answers.
func muxRedirect(status int, header http.Header) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return header.Get("Location") != ""
	}
	return false
}

func types(c openapi3.Content) []string {
	var out []string
	for k := range c {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// typed is header with the Content-Type the server would put on a response
// that names none: what the body looks like.
func typed(header http.Header, body []byte) http.Header {
	if header.Get("Content-Type") != "" || len(body) == 0 {
		return header
	}
	out := header.Clone()
	out.Set("Content-Type", http.DetectContentType(body))
	return out
}

func jsonType(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	return err == nil && (mt == "application/json" || strings.HasSuffix(mt, "+json"))
}

// find is the operation r is for.
func (s *Spec) find(r *http.Request) (apispec.Op, bool) {
	p, ok := strings.CutPrefix(r.URL.Path, "/api/v1")
	if !ok || p != "" && !strings.HasPrefix(p, "/") {
		return apispec.Op{}, false
	}
	var best apispec.Op
	bestScore := -1
	for _, op := range s.ops {
		if op.Method != r.Method {
			continue
		}
		if score, ok := matches(op.Path, p); ok && score > bestScore {
			best, bestScore = op, score
		}
	}
	return best, bestScore >= 0
}

// matches is whether the request path p fits the template, and how many of
// its parts are literal: the most literal template wins, as a ServeMux's
// most specific pattern does. A last part called {path} takes the rest.
func matches(tmpl, p string) (int, bool) {
	want, got := strings.Split(strings.Trim(tmpl, "/"), "/"), strings.Split(strings.Trim(p, "/"), "/")
	score := 0
	for i, w := range want {
		variable := strings.HasPrefix(w, "{")
		if variable && w == "{path}" && i == len(want)-1 {
			return score, len(got) > i
		}
		if i >= len(got) || (!variable && w != got[i]) || (variable && got[i] == "") {
			return 0, false
		}
		if !variable {
			score++
		}
	}
	return score, len(got) == len(want)
}
