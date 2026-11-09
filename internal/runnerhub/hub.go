// Package runnerhub is the server's side of the runners: who is connected,
// what each has been given, and the five calls under /runner/ they make. A
// stage placed on a runner is started here and looks to dispatch like one
// the local runner started: a runner.Proc whose output arrives as the
// runner streams it.
package runnerhub

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/placement"
	"github.com/ndthuan/kipp/internal/runner"
	"github.com/ndthuan/kipp/internal/runnerproto"
)

// Missed is how many heartbeats a runner may miss before it is offline.
const Missed = 3

// Errors a stage on a runner may end with.
var (
	ErrOffline   = errors.New("the runner went offline")
	ErrRestarted = errors.New("the runner started again, and the stage with it")
	ErrNoRunner  = errors.New("no such runner free")
	ErrRemoved   = errors.New("the runner was removed")
	ErrLost      = errors.New("the runner never got the stage")
)

// Identity is who a token says a runner is.
type Identity struct {
	Name      string
	Labels    []string
	Executors int // 0: no limit
	Enabled   bool
	// Build is the build an on-demand runner's token is for; 0 for a
	// standing runner.
	Build int64
}

// Who is a runner the hub knows: a standing runner by its name, and one
// kipp started in a container by its build and its name there, for two
// builds' containers on one engine have the same name.
type Who struct {
	Build int64 // 0 for a standing runner
	Name  string
}

func (id Identity) who() Who { return Who{Build: id.Build, Name: id.Name} }

// Options configure a Hub. Zero values take the defaults noted.
type Options struct {
	Version   string       // the server's; a runner of another is refused
	Log       *slog.Logger // default slog.Default
	Now       func() time.Time
	Heartbeat time.Duration // how often a runner connects again; default 30 s
	ClaimWait time.Duration // how long a long-poll waits; default 60 s
	// Standing finds a standing runner by the hash of its token; an error
	// refuses the token. nil knows none.
	Standing func(ctx context.Context, hash string) (*Identity, error)
	// Seen records that a standing runner connected, and what it said of
	// itself. nil records nothing.
	Seen func(ctx context.Context, name, os, arch, version string, at time.Time)
}

// Hub is the runners the server knows of.
type Hub struct {
	o    Options
	quit chan struct{} // closed when Run ends: every long-poll answers at once

	mu     sync.Mutex
	conns  map[Who]*conn
	stages map[string]*stage   // by assignment id
	grants map[string]Identity // per-build tokens, by hash
}

// conn is one runner as the hub sees it.
type conn struct {
	id                Identity
	online            bool
	since             time.Time // online or offline since
	seen              time.Time
	idle              time.Time // with nothing given to it since
	os, arch, version string
	queued            []*stage
	running           map[string]*stage
	ended             []runnerproto.Ended
	workspaces        map[int64]string // the builds that used it, to their workspaces there
	wake              chan struct{}    // closed and replaced when there is something to claim
}

// New makes a Hub. Call Run so that a runner that stops calling goes
// offline.
func New(o Options) *Hub {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Heartbeat <= 0 {
		o.Heartbeat = 30 * time.Second
	}
	if o.ClaimWait <= 0 {
		o.ClaimWait = 60 * time.Second
	}
	if o.Standing == nil {
		o.Standing = func(context.Context, string) (*Identity, error) { return nil, errors.New("no standing runners") }
	}
	return &Hub{o: o, quit: make(chan struct{}), conns: map[Who]*conn{}, stages: map[string]*stage{}, grants: map[string]Identity{}}
}

// Run marks offline every runner that stopped calling, and fails what it
// was running, until ctx ends; then every long-poll answers, so that a
// server that stops is not held up by its runners. Call it once.
func (h *Hub) Run(ctx context.Context) {
	t := time.NewTicker(h.o.Heartbeat / 2)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			close(h.quit)
			return
		case <-t.C:
			h.sweep()
		}
	}
}

// sweep is one look for runners gone silent.
func (h *Hub) sweep() {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.o.Now()
	for _, c := range h.conns {
		if c.online && now.Sub(c.seen) > Missed*h.o.Heartbeat {
			h.o.Log.Warn("runner offline", "runner", c.id.Name, "last seen", c.seen)
			h.offline(c, ErrOffline)
		}
	}
}

// offline ends what c had and marks it gone. h.mu is held.
func (h *Hub) offline(c *conn, why error) {
	c.online, c.since = false, h.o.Now()
	for _, s := range slices.Concat(c.queued, slices.Collect(maps.Values(c.running))) {
		h.end(s, -1, why)
	}
	c.queued = nil
	c.poke()
}

// Grant makes the token of an on-demand runner: the one kipp starts in a
// container for build, named name. It claims only what is started on
// that name, and dies with EndBuild.
func (h *Hub) Grant(build int64, name string) (string, error) {
	plain, hash, err := auth.NewRunnerToken()
	if err != nil {
		return "", err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.grants[hash] = Identity{Name: name, Build: build, Enabled: true}
	return plain, nil
}

// EndBuild is a build that is over: its tokens die, its on-demand runners
// are forgotten, and each standing runner it ran on is told to remove its
// workspace there.
func (h *Hub) EndBuild(build int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for hash, id := range h.grants {
		if id.Build == build {
			delete(h.grants, hash)
			if c := h.conns[id.who()]; c != nil {
				h.offline(c, ErrRemoved)
				delete(h.conns, id.who())
			}
		}
	}
	for _, c := range h.conns {
		if ws, ok := c.workspaces[build]; ok {
			delete(c.workspaces, build)
			c.ended = append(c.ended, runnerproto.Ended{Workspace: ws})
			c.poke()
		}
	}
}

// Revoke ends the token of the on-demand runner who, before its build
// ends: its container did not come up, or its engine went away. What it
// ran ends with why.
func (h *Hub) Revoke(who Who, why error) {
	if who.Build == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for hash, id := range h.grants {
		if id.who() == who {
			delete(h.grants, hash)
		}
	}
	if c := h.conns[who]; c != nil {
		h.offline(c, why)
		delete(h.conns, who)
	}
}

// SetStanding is the standing runners as the server's records now say:
// labels, executors and whether each is enabled change at once, and one
// no longer there is dropped with what it ran.
func (h *Hub) SetStanding(ids []Identity) {
	byName := make(map[string]Identity, len(ids))
	for _, id := range ids {
		byName[id.Name] = id
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for who, c := range h.conns {
		if who.Build != 0 {
			continue
		}
		id, ok := byName[who.Name]
		if !ok {
			h.offline(c, ErrRemoved)
			delete(h.conns, who)
			continue
		}
		c.id = id
		c.poke()
	}
	// One registered and never connected is offline: a stage that asks for
	// its labels waits for it rather than failing as asking for nothing.
	for name, id := range byName {
		if h.conns[Who{Name: name}] == nil {
			h.conns[Who{Name: name}] = h.newConn(id)
		}
	}
}

func (h *Hub) newConn(id Identity) *conn {
	now := h.o.Now()
	return &conn{id: id, since: now, idle: now, running: map[string]*stage{}, workspaces: map[int64]string{}, wake: make(chan struct{})}
}

// Candidates are the standing runners as places a stage may be put.
func (h *Hub) Candidates() []placement.Candidate {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []placement.Candidate
	for who, c := range h.conns {
		if who.Build != 0 {
			continue
		}
		out = append(out, placement.Candidate{
			Name: who.Name, Kind: placement.KindRunner, Labels: slices.Clone(c.id.Labels),
			Free: c.free(), Reachable: c.online, Idle: c.idle,
		})
	}
	return out
}

func (c *conn) free() bool {
	return c.online && c.id.Enabled && (c.id.Executors <= 0 || c.busy() < c.id.Executors)
}

// CheckRunner refuses a standing runner no stage could be placed on: a name
// that is not one, no label, or no executor.
func CheckRunner(name string, labels []string, executors int) error {
	switch {
	case !config.ValidName(name):
		return fmt.Errorf("runner %q: lower-case letters, digits, dot, dash and underscore only", name)
	case len(labels) == 0:
		return fmt.Errorf("runner %s: one or more labels, which a stage's runs_on chooses it by", name)
	case executors < 1:
		return fmt.Errorf("runner %s: executors is at least 1", name)
	}
	for _, l := range labels {
		if !config.ValidLabel(l) {
			return fmt.Errorf("runner %s: label %q: lower-case letters, digits, dot, dash and underscore, starting with a letter or digit", name, l)
		}
	}
	return nil
}

// Status is what may be shown of a runner.
type Status struct {
	Name              string
	Labels            []string
	Executors, Busy   int
	Enabled, Online   bool
	Since, Seen       time.Time
	OS, Arch, Version string
	Build             int64    // an on-demand runner's build; 0 for a standing one
	Stages            []string // what it runs or has been given: job#number/stage
	Running           []StageRef
}

// StageRef is a stage of a build, as Stages names it in a string.
type StageRef struct {
	BuildID int64
	Job     string
	Number  int
	Stage   string
}

// Runners is every runner that has connected and is not forgotten, by name
// and build.
func (h *Hub) Runners() []Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Status, 0, len(h.conns))
	for _, who := range slices.SortedFunc(maps.Keys(h.conns), byNameAndBuild) {
		c := h.conns[who]
		st := Status{
			Name: who.Name, Labels: slices.Clone(c.id.Labels), Executors: c.id.Executors, Busy: c.busy(),
			Enabled: c.id.Enabled, Online: c.online, Since: c.since, Seen: c.seen,
			OS: c.os, Arch: c.arch, Version: c.version, Build: c.id.Build,
		}
		for _, s := range slices.Concat(c.queued, slices.Collect(maps.Values(c.running))) {
			st.Stages = append(st.Stages, s.label())
			st.Running = append(st.Running, StageRef{BuildID: s.meta.BuildID, Job: s.meta.Job, Number: s.meta.Number, Stage: s.meta.Stage})
		}
		slices.Sort(st.Stages)
		slices.SortFunc(st.Running, func(a, b StageRef) int { return cmp.Compare(a.label(), b.label()) })
		out = append(out, st)
	}
	return out
}

func byNameAndBuild(a, b Who) int {
	return cmp.Or(strings.Compare(a.Name, b.Name), cmp.Compare(a.Build, b.Build))
}

// Free reports whether the runner is online, enabled and has an executor
// to spare.
func (h *Hub) Free(who Who) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.conns[who]
	return c != nil && c.free()
}

func (c *conn) busy() int { return len(c.queued) + len(c.running) }

// poke wakes every claim waiting on c.
func (c *conn) poke() {
	close(c.wake)
	c.wake = make(chan struct{})
}

// identify is who a token is: an on-demand runner's grant, else a standing
// runner's record.
func (h *Hub) identify(ctx context.Context, token string) (Identity, bool) {
	hash := auth.HashToken(token)
	h.mu.Lock()
	id, ok := h.grants[hash]
	h.mu.Unlock()
	if ok {
		return id, true
	}
	got, err := h.o.Standing(ctx, hash)
	if err != nil || got == nil {
		return Identity{}, false
	}
	return *got, true
}

// writer is where a stage's output goes until the stage ends: after that,
// what is still arriving is dropped, for the log is no longer the
// stage's to write.
type writer struct {
	mu     sync.Mutex
	w      io.Writer
	closed bool
}

var errClosed = errors.New("the stage is over")

func (g *writer) Write(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return 0, errClosed
	}
	return g.w.Write(p)
}

func (g *writer) close() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
}

// The proc of a stage on a runner is the hub's stage; it has no process
// group on this machine to keep.
var _ runner.Proc = (*stage)(nil)
