package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/problem"
)

const v1Smoke = "/api/v1/hooks/github/smoke"

// asProblem is the answer to a delivery that must be the problem code.
func asProblem(t *testing.T, code int, body string, h http.Header, status int, want string) problem.Doc {
	t.Helper()
	if code != status || h.Get("Content-Type") != problem.MediaType {
		t.Fatalf("got %d %q %s, want a %d problem", code, h.Get("Content-Type"), body, status)
	}
	var d problem.Doc
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatalf("not a problem: %q", body)
	}
	if d.Code != want || d.Status != status || d.Detail == "" {
		t.Fatalf("got %+v, want code %s", d, want)
	}
	return d
}

func TestUnderV1AnUnsignedCallerLearnsNothingAndGetsAProblem(t *testing.T) {
	srv, _ := server(t)
	var first problem.Doc
	for i, c := range []struct{ name, path, sig string }{
		{"unknown job", "/api/v1/hooks/github/zzz", sign("topsecret", push)},
		{"no webhook trigger", "/api/v1/hooks/github/plain", sign("topsecret", push)},
		{"secret not set", "/api/v1/hooks/github/nosecret", sign("topsecret", push)},
		{"missing signature", v1Smoke, ""},
		{"bad signature", v1Smoke, sign("nope", push)},
	} {
		code, body, h := send(t, srv.URL+c.path, "push", c.sig, newDelivery(), push)
		d := asProblem(t, code, body, h, 401, "unauthorized")
		if i == 0 {
			first = d
		} else if d != first {
			t.Errorf("%s differs: %+v vs %+v", c.name, d, first)
		}
	}
}

func TestUnderV1EveryFailureOfADeliveryIsAProblem(t *testing.T) {
	srv, _ := server(t)
	sig := sign("topsecret", push)
	code, body, h := send(t, srv.URL+v1Smoke, "push", sig, "", push)
	d := asProblem(t, code, body, h, 400, "bad_input")
	if !strings.Contains(d.Detail, "X-GitHub-Delivery") {
		t.Errorf("detail %q", d.Detail)
	}
	bad := `not json`
	code, body, h = send(t, srv.URL+v1Smoke, "push", sign("topsecret", bad), newDelivery(), bad)
	asProblem(t, code, body, h, 400, "bad_input")
	id := newDelivery()
	if code, _, _ := send(t, srv.URL+v1Smoke, "push", sig, id, push); code != 202 {
		t.Fatalf("first delivery: %d", code)
	}
	code, body, h = send(t, srv.URL+v1Smoke, "push", sig, id, push)
	asProblem(t, code, body, h, 409, "duplicate_delivery")
}

func TestUnderV1AFailingSecretLookupIsAnInternalProblemThatHidesTheCause(t *testing.T) {
	srv, _ := serverWith(t, func(context.Context, string) (string, error) { return "", errors.New("store down: sekrit-detail") })
	code, body, h := send(t, srv.URL+v1Smoke, "push", sign("topsecret", push), newDelivery(), push)
	d := asProblem(t, code, body, h, 500, "internal")
	if strings.Contains(body, "sekrit-detail") || d.Detail != "internal error" {
		t.Errorf("the answer tells %q", body)
	}
}

func TestUnderV1ASlowBodyIs408AndAQuietServerIs503WithRetryAfter(t *testing.T) {
	slow, _ := serverOpts(t, Options{Secret: func(context.Context, string) (string, error) { return "topsecret", nil }, ReadTimeout: 100 * time.Millisecond})
	c := head(t, slow.URL, sign("topsecret", push), len(push))
	eq(t, "answer", status(t, c), "HTTP/1.1 408")

	srv, _, d := serverDispatch(t, Options{Secret: func(context.Context, string) (string, error) { return "topsecret", nil }})
	_, err := d.SetQuiet(context.Background(), true)
	must(t, err)
	code, body, h := send(t, srv.URL+v1Smoke, "push", sign("topsecret", push), newDelivery(), push)
	asProblem(t, code, body, h, 503, "quiet")
	eq(t, "Retry-After", h.Get("Retry-After"), "60")
}

func TestUnderV1BusyReadersAreABusyProblemWithRetryAfter(t *testing.T) {
	srv, _ := server(t)
	sig := sign("topsecret", push)
	head(t, srv.URL, sig, len(push))
	head(t, srv.URL, sig, len(push))
	var (
		code int
		body string
		h    http.Header
	)
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		code, body, h = send(t, srv.URL+v1Smoke, "push", sig, newDelivery(), push)
		if code == http.StatusServiceUnavailable {
			break
		}
	}
	asProblem(t, code, body, h, 503, "busy")
	eq(t, "Retry-After", h.Get("Retry-After"), "10")
}

func TestTheVersionedTreeAnswersAWrongMethodOrPathAsAProblem(t *testing.T) {
	h := New(nil, Options{}).Handler()
	for _, c := range []struct {
		method, path, code, allow string
		status                    int
	}{
		{"GET", "/api/v1/hooks/github/smoke", "method_not_allowed", "POST", 405},
		{"PUT", "/api/v1/hooks/github-app/ci", "method_not_allowed", "POST", 405},
		{"POST", "/api/v1/hooks/gitlab/smoke", "not_found", "", 404},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		asProblem(t, rec.Code, rec.Body.String(), rec.Header(), c.status, c.code)
		eq(t, c.path+" Allow", rec.Header().Get("Allow"), c.allow)
	}
}
