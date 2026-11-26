package runnerhub

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ndthuan/kipp/internal/artifacts"
	"github.com/ndthuan/kipp/internal/problem"
	"github.com/ndthuan/kipp/internal/route"
	"github.com/ndthuan/kipp/internal/runnerproto"
)

const maxJunitBody = 64 << 20

// Routes are the calls under /api/v1/runner/, each with its own token check.
func (h *Hub) Routes() []route.Route {
	row := func(method, path string, next func(http.ResponseWriter, *http.Request, Identity)) route.Route {
		return route.Route{Method: method, Path: path, Handler: h.authed(next)}
	}
	stages := runnerproto.StagePath + "{id}"
	return []route.Route{
		row(http.MethodPost, runnerproto.ConnectPath, h.connect),
		row(http.MethodGet, runnerproto.ClaimPath, h.claim),
		row(http.MethodPost, stages+runnerproto.LogSuffix, h.stageCall(h.log)),
		row(http.MethodGet, stages+runnerproto.ControlSuffix, h.stageCall(h.control)),
		row(http.MethodPost, stages+runnerproto.DoneSuffix, h.stageCall(h.done)),
		row(http.MethodPut, stages+runnerproto.ArtifactsSuffix, h.stageCall(h.artifacts)),
		row(http.MethodGet, stages+runnerproto.InputsSuffix+"{stage}", h.stageCall(h.inputs)),
	}
}

// Handler serves the calls under /api/v1/runner/ and nothing else.
func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	route.Mount(mux, h.Routes())
	mux.Handle(runnerproto.Root, problem.Fallback(mux, runnerproto.Root))
	return mux
}

func refuse(w http.ResponseWriter, status int, r runnerproto.Refusal) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(r)
}

func answer(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (h *Hub) authed(next func(http.ResponseWriter, *http.Request, Identity)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || !strings.HasPrefix(token, runnerproto.TokenPrefix) {
			refuse(w, http.StatusUnauthorized, runnerproto.Refusal{Error: "a runner's token"})
			return
		}
		id, ok := h.identify(r.Context(), token)
		if !ok {
			refuse(w, http.StatusUnauthorized, runnerproto.Refusal{Error: "a runner's token"})
			return
		}
		next(w, r, id)
	}
}

func (h *Hub) connect(w http.ResponseWriter, r *http.Request, id Identity) {
	var req runnerproto.ConnectRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		refuse(w, http.StatusBadRequest, runnerproto.Refusal{Error: "a connect request: " + err.Error()})
		return
	}
	if req.Version != h.o.Version {
		refuse(w, http.StatusConflict, runnerproto.Refusal{
			Error:         fmt.Sprintf("the runner is kipp %s and the server kipp %s", req.Version, h.o.Version),
			ServerVersion: h.o.Version,
		})
		return
	}
	now := h.o.Now()
	kill := h.connected(id, &req, now)
	if id.Build == 0 && h.o.Seen != nil {
		h.o.Seen(r.Context(), id.Name, req.OS, req.Arch, req.Version, now)
	}
	answer(w, runnerproto.ConnectResponse{
		Name: id.Name, Labels: id.Labels, Executors: id.Executors, ServerVersion: h.o.Version,
		Heartbeat: h.o.Heartbeat, Kill: kill,
	})
}

func (h *Hub) connected(id Identity, req *runnerproto.ConnectRequest, now time.Time) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.conns[id.who()]
	if c == nil {
		c = h.newConn(id)
		h.conns[id.who()] = c
	}
	if req.Fresh {
		for _, s := range c.running {
			h.end(s, -1, ErrRestarted)
		}
	}
	if !c.online {
		c.online, c.since = true, now
		h.o.Log.Info("runner online", "runner", id.Name, "os", req.OS, "arch", req.Arch)
	}
	c.id, c.seen, c.os, c.arch, c.version = id, now, req.OS, req.Arch, req.Version
	var kill []string
	for _, rid := range req.Running {
		if _, ok := c.running[rid]; !ok {
			kill = append(kill, rid)
		}
	}
	h.endLost(c, req.Running, now)
	c.poke()
	return kill
}

func (h *Hub) endLost(c *conn, running []string, now time.Time) {
	for id, s := range c.running {
		if !slices.Contains(running, id) && now.Sub(s.claimedAt) > h.o.Heartbeat {
			h.o.Log.Warn("a stage its runner never got", "runner", c.id.Name, "stage", s.label())
			h.end(s, -1, ErrLost)
		}
	}
}

func (h *Hub) claim(w http.ResponseWriter, r *http.Request, id Identity) {
	timer := time.NewTimer(h.o.ClaimWait)
	defer timer.Stop()
	for {
		claim, wake, status := h.take(id.who())
		if status != http.StatusOK {
			refuse(w, status, runnerproto.Refusal{Error: http.StatusText(status)})
			return
		}
		if claim.Assignment != nil || len(claim.Ended) > 0 {
			answer(w, claim)
			return
		}
		select {
		case <-wake:
		case <-timer.C:
			answer(w, claim)
			return
		case <-h.quit:
			answer(w, claim)
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (h *Hub) take(who Who) (runnerproto.Claim, <-chan struct{}, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.conns[who]
	if c == nil || !c.online {
		return runnerproto.Claim{}, nil, http.StatusBadRequest
	}
	claim := runnerproto.Claim{Ended: c.ended}
	c.ended = nil
	if len(c.queued) > 0 {
		s := c.queued[0]
		c.queued = c.queued[1:]
		s.claimed, s.claimedAt = true, h.o.Now()
		c.running[s.id] = s
		claim.Assignment = s.a
	}
	return claim, c.wake, http.StatusOK
}

func (h *Hub) stageCall(next func(http.ResponseWriter, *http.Request, *stage)) func(http.ResponseWriter, *http.Request, Identity) {
	return func(w http.ResponseWriter, r *http.Request, id Identity) {
		h.mu.Lock()
		s := h.stages[r.PathValue("id")]
		mine := s != nil && s.runner == id.who() && s.claimed
		h.mu.Unlock()
		if !mine {
			refuse(w, http.StatusGone, runnerproto.Refusal{Error: "no such stage running on this runner"})
			return
		}
		next(w, r, s)
	}
}

func (h *Hub) log(w http.ResponseWriter, r *http.Request, s *stage) {
	h.mu.Lock()
	again := s.logged
	s.logged = true
	h.mu.Unlock()
	if again {
		refuse(w, http.StatusConflict, runnerproto.Refusal{Error: "the stage's log is already being sent"})
		return
	}
	if _, err := io.Copy(s.out, r.Body); err != nil && !errors.Is(err, errClosed) {
		h.o.Log.Warn("reading a stage's log from its runner", "runner", s.runner.Name, "stage", s.label(), "err", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Hub) control(w http.ResponseWriter, r *http.Request, s *stage) {
	timer := time.NewTimer(h.o.ClaimWait)
	defer timer.Stop()
	action := runnerproto.ActionNone
	select {
	case <-s.cancel:
		action = runnerproto.ActionCancel
	case <-s.done:
		action = runnerproto.ActionCancel
	case <-timer.C:
	case <-h.quit:
	case <-r.Context().Done():
		return
	}
	answer(w, runnerproto.Control{Action: action})
}

func (h *Hub) done(w http.ResponseWriter, r *http.Request, s *stage) {
	code, err := strconv.Atoi(r.URL.Query().Get("exit"))
	if err != nil {
		refuse(w, http.StatusBadRequest, runnerproto.Refusal{Error: "exit: a number"})
		return
	}
	tries, _ := strconv.Atoi(r.URL.Query().Get("tries"))
	var why error
	if msg := r.URL.Query().Get("error"); msg != "" {
		why = errors.New(msg)
	}
	if err := untar(io.LimitReader(r.Body, maxJunitBody+1), s.meta.JunitDir); err != nil {
		why = errors.Join(why, fmt.Errorf("the junit and metrics files: %w", err))
	}
	h.mu.Lock()
	s.tries = tries
	h.end(s, code, errors.Join(why, s.keptErr))
	h.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (h *Hub) inputs(w http.ResponseWriter, r *http.Request, s *stage) {
	dir, ok := s.meta.InputDirs[r.PathValue("stage")]
	if !ok {
		refuse(w, http.StatusNotFound, runnerproto.Refusal{Error: "the stage is given no artifacts of that stage"})
		return
	}
	w.Header().Set("Content-Type", runnerproto.JunitType)
	if err := artifacts.Tar(w, dir); err != nil {
		h.o.Log.Warn("sending a stage's inputs to its runner", "runner", s.runner.Name, "stage", s.label(), "err", err)
	}
}

func (h *Hub) artifacts(w http.ResponseWriter, r *http.Request, s *stage) {
	h.mu.Lock()
	again := s.kept
	s.kept = true
	h.mu.Unlock()
	switch {
	case again:
		refuse(w, http.StatusConflict, runnerproto.Refusal{Error: "the stage's artifacts are already being sent"})
		return
	case s.meta.Artifacts == nil || s.meta.ArtifactsDir == "":
		refuse(w, http.StatusBadRequest, runnerproto.Refusal{Error: "the stage keeps no artifacts"})
		return
	}
	declared := int64(-1)
	if v := r.URL.Query().Get("size"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			refuse(w, http.StatusBadRequest, runnerproto.Refusal{Error: "size: a number of bytes"})
			return
		}
		declared = n
	}
	_, err := artifacts.Untar(r.Body, s.meta.ArtifactsDir, s.meta.Artifacts.Max, declared)
	if err == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h.mu.Lock()
	s.keptErr = err
	h.mu.Unlock()
	status := http.StatusBadRequest
	if _, big := errors.AsType[*artifacts.TooLargeError](err); big {
		status = http.StatusRequestEntityTooLarge
	}
	refuse(w, status, runnerproto.Refusal{Error: err.Error()})
}

func untar(r io.Reader, dir string) error {
	if dir == "" {
		_, err := io.Copy(io.Discard, r)
		return err
	}
	tr := tar.NewReader(r)
	var root *os.Root
	defer func() {
		if root != nil {
			_ = root.Close()
		}
	}()
	var total int64
	for {
		hd, err := tr.Next()
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return err
		case hd.Typeflag != tar.TypeReg || !filepath.IsLocal(hd.Name):
			continue
		}
		if total += hd.Size; total > maxJunitBody {
			return fmt.Errorf("more than %d bytes", maxJunitBody)
		}
		if root, err = openOnce(root, dir); err != nil {
			return err
		}
		if err := writeIn(root, filepath.FromSlash(hd.Name), tr); err != nil {
			return err
		}
	}
}

func openOnce(root *os.Root, dir string) (*os.Root, error) {
	if root != nil {
		return root, nil
	}
	return openDir(dir)
}

func openDir(dir string) (*os.Root, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return os.OpenRoot(dir)
}

func writeIn(root *os.Root, name string, r io.Reader) error {
	if err := root.MkdirAll(filepath.Dir(name), 0o750); err != nil {
		return err
	}
	f, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	return errors.Join(err, f.Close())
}
