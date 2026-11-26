package route

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

const safety = 30 * time.Second

func open(t *testing.T, srv *httptest.Server, request string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.SetDeadline(time.Now().Add(safety)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(c, request); err != nil {
		t.Fatal(err)
	}
	return c
}

func serve(t *testing.T, within time.Duration, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(BodyWithin(within, h))
	t.Cleanup(srv.Close)
	return srv
}

const unsent = "POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 10\r\n\r\n"

func TestABodyThatDoesNotArriveInTimeEndsTheHandlersRead(t *testing.T) {
	read := make(chan error, 1)
	srv := serve(t, 20*time.Millisecond, func(_ http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		read <- err
	})
	open(t, srv, unsent)
	select {
	case err := <-read:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("the read ended with %v", err)
		}
	case <-time.After(safety):
		t.Fatal("the handler still reads a body that never came")
	}
}

func TestARequestRefusedBeforeItsBodyIsReadIsAnsweredAndItsConnectionClosedInTime(t *testing.T) {
	srv := serve(t, 20*time.Millisecond, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	c := open(t, srv, unsent)
	answer, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("the connection stayed open for a body that never came: %v", err)
	}
	if !strings.HasPrefix(string(answer), "HTTP/1.1 401") {
		t.Fatalf("answer %q", answer)
	}
}

func TestAnAnswerThatOutlastsTheTimeIsNotCutOnceTheBodyWasRead(t *testing.T) {
	read, stalled, release, ended := make(chan error), make(chan error), make(chan struct{}), make(chan error)
	srv := serve(t, 20*time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if r.URL.Path != "/late" {
			stalled <- err
			return
		}
		read <- err
		<-release
		ended <- r.Context().Err()
		_, _ = io.WriteString(w, "late")
	})
	for name, request := range map[string]string{
		"a body of a known length": "POST /late HTTP/1.1\r\nHost: x\r\nContent-Length: 4\r\n\r\nbody",
		"a chunked body":           "POST /late HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\n4\r\nbody\r\n0\r\n\r\n",
		"no body":                  "GET /late HTTP/1.1\r\nHost: x\r\n\r\n",
	} {
		c := open(t, srv, request)
		if err := <-read; err != nil {
			t.Fatalf("%s: reading it: %v", name, err)
		}
		for range 2 {
			open(t, srv, unsent)
			if err := <-stalled; !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("a body that never came ended with %v", err)
			}
		}
		release <- struct{}{}
		if err := <-ended; err != nil {
			t.Fatalf("%s: its request ended while it was answered: %v", name, err)
		}
		line := make([]byte, len("HTTP/1.1 200"))
		if _, err := io.ReadFull(c, line); err != nil || string(line) != "HTTP/1.1 200" {
			t.Fatalf("%s: %q %v", name, line, err)
		}
	}
}
