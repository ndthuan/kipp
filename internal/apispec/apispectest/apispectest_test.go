package apispectest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const tiny = `
openapi: 3.1.0
info: {title: tiny, version: 0.0.0}
servers: [{url: /api/v1}]
paths:
  /thing:
    get:
      x-kipp-package: api
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema: {type: object, additionalProperties: false, required: [a], properties: {a: {type: string}}}
        "404":
          description: none
          content:
            application/json:
              schema: {type: object}
  /empty:
    post:
      x-kipp-package: api
      responses:
        "204": {description: done}
  /never:
    get:
      x-kipp-package: api
      responses:
        "200": {description: ok}
  /files/{path}:
    get:
      x-kipp-package: other
      parameters:
        - {name: path, in: path, required: true, schema: {type: string}}
      responses:
        "200":
          description: ok
          content:
            text/plain: {schema: {type: string}}
`

func tinySpec(t *testing.T) *Spec {
	t.Helper()
	s, err := Parse([]byte(tiny))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func answer(status int, contentType, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
}

func serve(t *testing.T, s *Spec, method, path string, h http.Handler) error {
	t.Helper()
	rec := &recorder{ResponseWriter: httptest.NewRecorder()}
	r := httptest.NewRequest(method, path, nil)
	h.ServeHTTP(rec, r)
	return s.Check(r, rec.code(), rec.Header(), rec.body.Bytes())
}

func TestAResponseTheSpecDescribesPasses(t *testing.T) {
	s := tinySpec(t)
	for _, c := range []struct {
		method, path string
		h            http.Handler
	}{
		{"GET", "/api/v1/thing", answer(200, "application/json", `{"a":"x"}`)},
		{"GET", "/api/thing", answer(500, "text/plain", "outside /api/v1, so no operation")},
		{"GET", "/api/v1/thing", answer(404, "application/json", `{"anything":1}`)},
		{"POST", "/api/v1/empty", answer(204, "", "")},
		{"GET", "/api/v1/files/a/b/c.txt", answer(200, "text/plain; charset=utf-8", "hello")},
		{"GET", "/jobs/x", answer(500, "text/plain", "a page, not an operation")},
	} {
		if err := serve(t, s, c.method, c.path, c.h); err != nil {
			t.Errorf("%s %s: %v", c.method, c.path, err)
		}
	}
}

func TestAResponseTheSpecDoesNotAllowFails(t *testing.T) {
	s := tinySpec(t)
	for name, c := range map[string]struct {
		method, path string
		h            http.Handler
		want         string
	}{
		"an extra field":       {"GET", "/api/v1/thing", answer(200, "application/json", `{"a":"x","b":1}`), "b"},
		"a missing field":      {"GET", "/api/v1/thing", answer(200, "application/json", `{}`), "a"},
		"a wrong type":         {"GET", "/api/v1/thing", answer(200, "application/json", `{"a":1}`), "a"},
		"an undocumented code": {"GET", "/api/v1/thing", answer(503, "application/json", `{}`), "503"},
		"another media type":   {"GET", "/api/v1/thing", answer(200, "text/plain", `{"a":"x"}`), "text/plain"},
		"a body for no body":   {"POST", "/api/v1/empty", answer(204, "", "oops"), "empty"},
		"a file as json":       {"GET", "/api/v1/files/x", answer(200, "application/json", `{}`), "application/json"},
	} {
		err := serve(t, s, c.method, c.path, c.h)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one naming %q", name, err, c.want)
		}
	}
}

func TestAnOperationNoTestGotAGoodResponseFromIsNamed(t *testing.T) {
	s := tinySpec(t)
	if got := strings.Join(s.Uncovered("api"), ","); got != "GET /never,GET /thing,POST /empty" {
		t.Fatalf("before any answer: %s", got)
	}
	_ = serve(t, s, "GET", "/api/v1/thing", answer(404, "application/json", `{}`)) // not a good one
	_ = serve(t, s, "POST", "/api/v1/empty", answer(204, "", ""))
	if got := strings.Join(s.Uncovered("api"), ","); got != "GET /never,GET /thing" {
		t.Fatalf("after a 404 and a 204: %s", got)
	}
	_ = serve(t, s, "GET", "/api/v1/thing", answer(200, "application/json", `{"a":"x"}`))
	_ = serve(t, s, "GET", "/api/v1/thing", answer(200, "application/json", `{"a":1}`)) // refused, so not counted twice or at all
	if got := strings.Join(s.Uncovered("api"), ","); got != "GET /never" {
		t.Fatalf("after a good answer: %s", got)
	}
	if got := s.Uncovered("other"); len(got) != 1 {
		t.Fatalf("the other package's own operation: %v", got)
	}
}

func TestTheOperationWithMoreLiteralPartsIsTheOneAnswered(t *testing.T) {
	for _, c := range []struct {
		tmpl, path string
		score      int
		ok         bool
	}{
		{"/builds/{id}", "/builds/7", 1, true},
		{"/builds/{id}/artifacts/{stage}/{path}", "/builds/7/artifacts/s/a/b.txt", 2, true},
		{"/builds/{id}/artifacts/{stage}/{path}", "/builds/7/artifacts/s", 0, false},
		{"/builds/{id}", "/builds/7/results", 0, false},
		{"/health", "/health", 1, true},
	} {
		score, ok := matches(c.tmpl, c.path)
		if ok != c.ok || (ok && score != c.score) {
			t.Errorf("matches(%q, %q) = %d, %v; want %d, %v", c.tmpl, c.path, score, ok, c.score, c.ok)
		}
	}
}

func TestAnAnswerNobodyGaveIsLeftAlone(t *testing.T) {
	s := tinySpec(t)
	// A long-poll whose client left: the handler returns without writing.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest("GET", "/api/v1/thing", nil).WithContext(ctx)
	h := s.Wrap(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), r) // would fail the test at its end if it were checked
	// A path the ServeMux cleans is redirected before any handler.
	redirect := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/api/v1/thing", http.StatusTemporaryRedirect)
	})
	if err := serve(t, s, "GET", "/api/v1/thing", redirect); err != nil {
		t.Errorf("a redirect from the mux: %v", err)
	}
	if err := serve(t, s, "GET", "/api/v1/thing", answer(503, "application/json", `{}`)); err == nil {
		t.Error("an undocumented 503 passed")
	}
}
