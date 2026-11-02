// Package dockertest is a fake Docker engine: an HTTP server on a unix
// socket that answers the calls kipp makes the way an engine does, and
// keeps what it was asked.
package dockertest

import (
	"archive/tar"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Request is one call the engine got.
type Request struct {
	Method, Path, Query string
	Body                []byte
}

// Container is one the engine made.
type Container struct {
	ID      string
	Create  map[string]any // the body of the create call
	Files   map[string]fileAt
	Running bool
	Labels  map[string]string
	stopped chan struct{} // closed when killed or removed: a wait returns then
}

type fileAt struct {
	Mode int64
	Data []byte
}

// File is what was put at name in the container, and its mode.
func (c *Container) File(name string) (int64, []byte, bool) {
	f, ok := c.Files[name]
	return f.Mode, f.Data, ok
}

// Engine is the fake.
type Engine struct {
	Socket string
	// OnStart runs when a container is started, outside the engine's lock.
	OnStart func(c *Container)
	// Logs is what a container's logs call answers.
	Logs string

	mu         sync.Mutex
	requests   []Request
	images     map[string]bool
	containers map[string]*Container
	next       int
}

// New starts a fake engine on a socket in a temporary directory.
func New(t *testing.T, images ...string) *Engine {
	t.Helper()
	dir, err := shortDir(t)
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{Socket: filepath.Join(dir, "docker.sock"), images: map[string]bool{}, containers: map[string]*Container{}}
	for _, im := range images {
		e.images[im] = true
	}
	ln, err := net.Listen("unix", e.Socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(e.serve), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return e
}

// URL is the engine's unix:// URL.
func (e *Engine) URL() string { return "unix://" + e.Socket }

// Requests are the calls made so far, without the API version.
func (e *Engine) Requests() []Request {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Request(nil), e.requests...)
}

// Containers are those made and not removed, by id.
func (e *Engine) Containers() map[string]*Container {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[string]*Container{}
	for id, c := range e.containers {
		cc := *c
		out[id] = &cc
	}
	return out
}

// Add is a container made by someone else, as one a crashed server left.
func (e *Engine) Add(labels map[string]string, running bool) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.next++
	id := fmt.Sprintf("c%d", e.next)
	e.containers[id] = &Container{ID: id, Labels: labels, Running: running, Files: map[string]fileAt{}, stopped: make(chan struct{})}
	return id
}

func (e *Engine) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	p := strings.TrimPrefix(r.URL.Path, "/v1.47")
	e.mu.Lock()
	e.requests = append(e.requests, Request{Method: r.Method, Path: p, Query: r.URL.RawQuery, Body: body})
	e.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && p == "/version":
		reply(w, map[string]string{"Version": "29.0.0", "Os": "linux", "Arch": "arm64"})
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/json"):
		e.image(w, strings.TrimSuffix(strings.TrimPrefix(p, "/images/"), "/json"))
	case r.Method == http.MethodPost && p == "/images/create":
		e.pull(w, r.URL.Query().Get("fromImage"))
	case r.Method == http.MethodPost && p == "/containers/create":
		e.create(w, body)
	case r.Method == http.MethodGet && p == "/containers/json":
		e.list(w, r.URL.Query().Get("filters"))
	case strings.HasPrefix(p, "/containers/"):
		e.container(w, r.Method, strings.Split(strings.TrimPrefix(p, "/containers/"), "/"), body, r.Context().Done())
	default:
		http.Error(w, `{"message":"no such call"}`, http.StatusNotFound)
	}
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func notFound(w http.ResponseWriter, what string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	reply(w, map[string]string{"message": "No such " + what})
}

func (e *Engine) image(w http.ResponseWriter, name string) {
	e.mu.Lock()
	ok := e.images[name]
	e.mu.Unlock()
	if !ok {
		notFound(w, "image: "+name)
		return
	}
	reply(w, map[string]string{"Id": "sha256:x"})
}

func (e *Engine) pull(w http.ResponseWriter, ref string) {
	e.mu.Lock()
	e.images[strings.TrimSuffix(ref, ":latest")] = true
	e.images[ref] = true
	e.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	for _, line := range []map[string]string{
		{"status": "Pulling from library/x", "id": "latest"},
		{"status": "Downloading", "id": "abc", "progress": "[=>  ] 1MB/2MB"},
		{"status": "Downloading", "id": "abc", "progress": "[==> ] 2MB/2MB"},
		{"status": "Pull complete", "id": "abc"},
		{"status": "Status: Downloaded newer image"},
	} {
		_ = enc.Encode(line)
	}
}

func (e *Engine) create(w http.ResponseWriter, body []byte) {
	var spec map[string]any
	if err := json.Unmarshal(body, &spec); err != nil {
		http.Error(w, `{"message":"bad body"}`, http.StatusBadRequest)
		return
	}
	labels := map[string]string{}
	if m, ok := spec["Labels"].(map[string]any); ok {
		for k, v := range m {
			labels[k], _ = v.(string)
		}
	}
	e.mu.Lock()
	e.next++
	id := fmt.Sprintf("c%d", e.next)
	e.containers[id] = &Container{ID: id, Create: spec, Labels: labels, Files: map[string]fileAt{}, stopped: make(chan struct{})}
	e.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
	reply(w, map[string]string{"Id": id})
}

func (e *Engine) list(w http.ResponseWriter, filters string) {
	var f map[string][]string
	_ = json.Unmarshal([]byte(filters), &f)
	e.mu.Lock()
	defer e.mu.Unlock()
	out := []map[string]any{}
	for id, c := range e.containers {
		if matches(c.Labels, f["label"]) {
			state := "exited"
			if c.Running {
				state = "running"
			}
			out = append(out, map[string]any{"Id": id, "Labels": c.Labels, "State": state})
		}
	}
	reply(w, out)
}

func matches(labels map[string]string, want []string) bool {
	for _, kv := range want {
		k, v, _ := strings.Cut(kv, "=")
		if labels[k] != v {
			return false
		}
	}
	return true
}

func (e *Engine) container(w http.ResponseWriter, method string, parts []string, body []byte, ctxDone <-chan struct{}) {
	e.mu.Lock()
	c := e.containers[parts[0]]
	e.mu.Unlock()
	if c == nil {
		notFound(w, "container: "+parts[0])
		return
	}
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	if method == http.MethodPost && action == "wait" {
		e.wait(w, c, ctxDone)
		return
	}
	switch method + " " + action {
	case "PUT archive":
		if err := e.untar(c, body); err != nil {
			http.Error(w, `{"message":"`+err.Error()+`"}`, http.StatusBadRequest)
		}
	case "POST start":
		e.setRunning(c, true)
		if e.OnStart != nil {
			e.OnStart(c)
		}
		w.WriteHeader(http.StatusNoContent)
	case "POST kill":
		e.stop(c)
		w.WriteHeader(http.StatusNoContent)
	case "DELETE ":
		e.stop(c)
		e.mu.Lock()
		delete(e.containers, c.ID)
		e.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case "GET logs":
		frame(w, 2, e.Logs)
	default:
		notFound(w, "call")
	}
}

// wait answers once the container is killed or removed, as an engine
// answers once a container's process ends; it exits 137, killed.
func (e *Engine) wait(w http.ResponseWriter, c *Container, done <-chan struct{}) {
	select {
	case <-c.stopped:
		reply(w, map[string]int{"StatusCode": 137})
	case <-done:
	}
}

func (e *Engine) stop(c *Container) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c.Running = false
	select {
	case <-c.stopped:
	default:
		close(c.stopped)
	}
}

func (e *Engine) setRunning(c *Container, running bool) {
	e.mu.Lock()
	c.Running = running
	e.mu.Unlock()
}

func (e *Engine) untar(c *Container, body []byte) error {
	tr := tar.NewReader(strings.NewReader(string(body)))
	for {
		hd, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		e.mu.Lock()
		c.Files["/"+strings.TrimSuffix(hd.Name, "/")] = fileAt{Mode: hd.Mode, Data: data}
		e.mu.Unlock()
	}
}

// frame writes s as one frame of the engine's multiplexed stream.
func frame(w io.Writer, stream byte, s string) {
	n := len(s)
	if n < 0 || n > math.MaxUint32 {
		panic("a frame holds at most 4 GiB")
	}
	hd := make([]byte, 8)
	hd[0] = stream
	binary.BigEndian.PutUint32(hd[4:], uint32(n))
	_, _ = w.Write(hd)
	_, _ = io.WriteString(w, s)
}

// shortDir is a temporary directory with a short path: a unix socket's
// path is limited to about a hundred bytes, and t.TempDir's on macOS is
// most of that.
func shortDir(t *testing.T) (string, error) {
	dir, err := os.MkdirTemp("/tmp", "kd")
	if err != nil {
		return "", err
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir, nil
}
