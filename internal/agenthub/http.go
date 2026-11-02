package agenthub

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

	"github.com/ndthuan/kipp/internal/agentproto"
	"github.com/ndthuan/kipp/internal/artifacts"
	"github.com/ndthuan/kipp/internal/problem"
	"github.com/ndthuan/kipp/internal/route"
)

// maxJunitBody is the most the junit and metrics files of one stage may
// come to.
const maxJunitBody = 64 << 20

// Routes are the calls under /api/v1/agent/, each with its own token check.
func (h *Hub) Routes() []route.Route {
	row := func(method, path string, next func(http.ResponseWriter, *http.Request, Identity)) route.Route {
		return route.Route{Method: method, Path: path, Handler: h.authed(next)}
	}
	stages := agentproto.StagePath + "{id}"
	return []route.Route{
		row(http.MethodPost, agentproto.ConnectPath, h.connect),
		row(http.MethodGet, agentproto.ClaimPath, h.claim),
		row(http.MethodPost, stages+agentproto.LogSuffix, h.stageCall(h.log)),
		row(http.MethodGet, stages+agentproto.ControlSuffix, h.stageCall(h.control)),
		row(http.MethodPost, stages+agentproto.DoneSuffix, h.stageCall(h.done)),
		row(http.MethodPut, stages+agentproto.ArtifactsSuffix, h.stageCall(h.artifacts)),
		row(http.MethodGet, stages+agentproto.InputsSuffix+"{stage}", h.stageCall(h.inputs)),
	}
}

// Handler serves the calls under /api/v1/agent/ and nothing else.
func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	route.Mount(mux, h.Routes())
	mux.Handle(agentproto.Root, problem.Fallback(mux, agentproto.Root)) // plain HTTP failures are problems; the protocol's own stay Refusals
	return mux
}

// refuse answers status with why, as JSON.
func refuse(w http.ResponseWriter, status int, r agentproto.Refusal) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(r)
}

func answer(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// authed is a handler for a caller with an agent's token.
func (h *Hub) authed(next func(http.ResponseWriter, *http.Request, Identity)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || !strings.HasPrefix(token, agentproto.TokenPrefix) {
			refuse(w, http.StatusUnauthorized, agentproto.Refusal{Error: "an agent's token"})
			return
		}
		id, ok := h.identify(r.Context(), token)
		if !ok {
			refuse(w, http.StatusUnauthorized, agentproto.Refusal{Error: "an agent's token"})
			return
		}
		next(w, r, id)
	}
}

func (h *Hub) connect(w http.ResponseWriter, r *http.Request, id Identity) {
	var req agentproto.ConnectRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		refuse(w, http.StatusBadRequest, agentproto.Refusal{Error: "a connect request: " + err.Error()})
		return
	}
	if req.Version != h.o.Version {
		refuse(w, http.StatusConflict, agentproto.Refusal{
			Error:         fmt.Sprintf("the agent is kipp %s and the server kipp %s", req.Version, h.o.Version),
			ServerVersion: h.o.Version,
		})
		return
	}
	now := h.o.Now()
	kill := h.connected(id, &req, now)
	if id.Build == 0 && h.o.Seen != nil {
		h.o.Seen(r.Context(), id.Name, req.OS, req.Arch, req.Version, now)
	}
	answer(w, agentproto.ConnectResponse{
		Name: id.Name, Labels: id.Labels, Executors: id.Executors, ServerVersion: h.o.Version,
		Heartbeat: h.o.Heartbeat, Kill: kill,
	})
}

// connected records a connect, and is the assignments the agent runs that
// the hub does not know as running there.
func (h *Hub) connected(id Identity, req *agentproto.ConnectRequest, now time.Time) []string {
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
		h.o.Log.Info("agent online", "agent", id.Name, "os", req.OS, "arch", req.Arch)
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

// endLost ends what c claimed and does not say it runs, a heartbeat after
// the claim: the claim's answer never reached it. The heartbeat is the
// grace for a connect that set out before the claim's answer came. h.mu
// is held.
func (h *Hub) endLost(c *conn, running []string, now time.Time) {
	for id, s := range c.running {
		if !slices.Contains(running, id) && now.Sub(s.claimedAt) > h.o.Heartbeat {
			h.o.Log.Warn("a stage its agent never got", "agent", c.id.Name, "stage", s.label())
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
			refuse(w, status, agentproto.Refusal{Error: http.StatusText(status)})
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

// take is what the agent may claim now: the oldest stage given to it, and
// the builds that ended. An agent the hub forgot (its build ended, or it
// was removed) is 401, for its token is no more; one that went offline is
// 400 until it connects again.
func (h *Hub) take(who Who) (agentproto.Claim, <-chan struct{}, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.conns[who]
	switch {
	case c == nil:
		return agentproto.Claim{}, nil, http.StatusUnauthorized
	case !c.online:
		return agentproto.Claim{}, nil, http.StatusBadRequest
	}
	claim := agentproto.Claim{Ended: c.ended}
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

// stageCall is a handler about one assignment, which must be the caller's.
func (h *Hub) stageCall(next func(http.ResponseWriter, *http.Request, *stage)) func(http.ResponseWriter, *http.Request, Identity) {
	return func(w http.ResponseWriter, r *http.Request, id Identity) {
		h.mu.Lock()
		s := h.stages[r.PathValue("id")]
		mine := s != nil && s.agent == id.who() && s.claimed
		h.mu.Unlock()
		if !mine {
			refuse(w, http.StatusGone, agentproto.Refusal{Error: "no such stage running on this agent"})
			return
		}
		next(w, r, s)
	}
}

// log copies the stage's output into its log as it arrives.
func (h *Hub) log(w http.ResponseWriter, r *http.Request, s *stage) {
	h.mu.Lock()
	again := s.logged
	s.logged = true
	h.mu.Unlock()
	if again {
		refuse(w, http.StatusConflict, agentproto.Refusal{Error: "the stage's log is already being sent"})
		return
	}
	if _, err := io.Copy(s.out, r.Body); err != nil && !errors.Is(err, errClosed) {
		h.o.Log.Warn("reading a stage's log from its agent", "agent", s.agent.Name, "stage", s.label(), "err", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// control answers cancel once the stage is to be killed, and none when
// the long-poll runs out first.
func (h *Hub) control(w http.ResponseWriter, r *http.Request, s *stage) {
	timer := time.NewTimer(h.o.ClaimWait)
	defer timer.Stop()
	action := agentproto.ActionNone
	select {
	case <-s.cancel:
		action = agentproto.ActionCancel
	case <-s.done:
		action = agentproto.ActionCancel
	case <-timer.C:
	case <-h.quit:
	case <-r.Context().Done():
		return
	}
	answer(w, agentproto.Control{Action: action})
}

// done ends the stage with the exit code the agent says, after writing
// the junit files it sent where the server reads them.
func (h *Hub) done(w http.ResponseWriter, r *http.Request, s *stage) {
	code, err := strconv.Atoi(r.URL.Query().Get("exit"))
	if err != nil {
		refuse(w, http.StatusBadRequest, agentproto.Refusal{Error: "exit: a number"})
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

// inputs sends the files one of the stages the stage is given kept, as a
// tar: those of a stage it names in artifacts_from, and none other.
func (h *Hub) inputs(w http.ResponseWriter, r *http.Request, s *stage) {
	dir, ok := s.meta.InputDirs[r.PathValue("stage")]
	if !ok {
		refuse(w, http.StatusNotFound, agentproto.Refusal{Error: "the stage is given no artifacts of that stage"})
		return
	}
	w.Header().Set("Content-Type", agentproto.JunitType)
	if err := artifacts.Tar(w, dir); err != nil {
		h.o.Log.Warn("sending a stage's inputs to its agent", "agent", s.agent.Name, "stage", s.label(), "err", err)
	}
}

// artifacts writes the files the stage keeps where the server reads them,
// refusing a tar the stage's files could not have made, or one over its
// max_size: in a container the stage runs as root beside the agent, and
// can write what the agent sends.
func (h *Hub) artifacts(w http.ResponseWriter, r *http.Request, s *stage) {
	h.mu.Lock()
	again := s.kept
	s.kept = true
	h.mu.Unlock()
	switch {
	case again:
		refuse(w, http.StatusConflict, agentproto.Refusal{Error: "the stage's artifacts are already being sent"})
		return
	case s.meta.Artifacts == nil || s.meta.ArtifactsDir == "":
		refuse(w, http.StatusBadRequest, agentproto.Refusal{Error: "the stage keeps no artifacts"})
		return
	}
	declared := int64(-1)
	if v := r.URL.Query().Get("size"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			refuse(w, http.StatusBadRequest, agentproto.Refusal{Error: "size: a number of bytes"})
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
	refuse(w, status, agentproto.Refusal{Error: err.Error()})
}

// untar writes the regular files of a tar under dir, which it makes;
// nothing outside it, and nothing at all when dir is empty.
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

// openOnce is root, or dir made and opened as one when there is none yet:
// a stage that left no report leaves no directory.
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
