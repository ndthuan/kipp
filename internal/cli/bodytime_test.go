package cli

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func unsentBody(t *testing.T, srv *httptest.Server, path string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(c, "POST "+path+" HTTP/1.1\r\nHost: x\r\nContent-Length: 4\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestABodyMustArriveInTimeExceptOnTheRoutesThatTakeUploadsAndStreams(t *testing.T) {
	type read struct {
		path string
		err  error
	}
	reads := make(chan read)
	srv := httptest.NewServer(timedBodies(20*time.Millisecond, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		reads <- read{r.URL.Path, err}
	})))
	t.Cleanup(srv.Close)
	slow := map[string]net.Conn{}
	for _, path := range []string{"/api/v1/runner/logs", "/api/v1/cache/a/b", "/api/v1/hooks/github/smoke"} {
		slow[path] = unsentBody(t, srv, path)
	}
	for _, path := range []string{"/api/v1/builds", "/oauth/register", "/mcp", "/login", "/api/v1/runners"} {
		unsentBody(t, srv, path)
		if got := <-reads; got.path != path || !errors.Is(got.err, os.ErrDeadlineExceeded) {
			t.Fatalf("%s: a body that never came ended as %+v", path, got)
		}
	}
	for path, c := range slow {
		if _, err := io.WriteString(c, "late"); err != nil {
			t.Fatal(err)
		}
		if got := <-reads; got.path != path || got.err != nil {
			t.Fatalf("%s: a body that came late ended as %+v", path, got)
		}
	}
}
