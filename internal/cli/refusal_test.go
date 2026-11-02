package cli

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/ndthuan/kipp/internal/problem"
)

func TestARefusalSaysTheDetailOfAProblem(t *testing.T) {
	for name, c := range map[string]struct {
		status int
		body   string
		want   string
		is     error
	}{
		"a problem":       {409, `{"type":"x","title":"t","status":409,"detail":"the job is disabled","code":"job_disabled"}`, "the job is disabled", nil},
		"neither":         {400, `not json`, "400 Bad Request", nil},
		"a problem 401":   {401, `{"detail":"login or a token is required"}`, "login or a token is required", errRefused},
		"a problem 5xx":   {503, `{"detail":"kipp is quiet"}`, "kipp is quiet", errNoAnswer},
		"an empty detail": {500, `{"detail":""}`, "500 Internal Server Error", errNoAnswer},
	} {
		res := &http.Response{StatusCode: c.status, Status: strconv.Itoa(c.status) + " " + http.StatusText(c.status)}
		err := refusal(res, []byte(c.body))
		if !strings.Contains(err.Error(), c.want) || (c.is != nil && !errors.Is(err, c.is)) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
}

func TestANotFoundIsNeverSentAgain(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		problem.Write(w, problem.NotFound, "not found")
	}))
	defer srv.Close()
	c := &client{url: srv.URL, token: "t", http: srv.Client()}
	if err := c.call("POST", "/jobs/nope/builds", nil, nil); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v", err)
	}
	if len(paths) != 1 || paths[0] != "/api/v1/jobs/nope/builds" {
		t.Errorf("the call went to %v: a write must not be sent twice", paths)
	}
}
