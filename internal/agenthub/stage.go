package agenthub

import (
	"context"
	"crypto/rand"
	"fmt"
	"slices"
	"time"

	"github.com/ndthuan/kipp/internal/agentproto"
	"github.com/ndthuan/kipp/internal/runner"
)

// StageMeta is what the hub needs of a stage beyond its runner.Spec.
type StageMeta struct {
	BuildID int64
	Job     string
	Number  int
	Stage   string
	// Workspace is the build's directory under the agent's workdir;
	// Workdir the stage's own, inside it.
	Workspace, Workdir string
	// Junit is the stage's glob; the files it matched on the agent are
	// written under JunitDir, on the server, before Wait returns. The
	// files Metrics, the stage's metrics.file, matched are written there
	// too.
	Junit, JunitDir string
	Metrics         string
	// Baseline is the stage's KIPP_METRICS, for the agent to write; nil
	// for a stage that declares no metrics.
	Baseline []byte
	// Artifacts is the stage's artifacts key, nil for none; the files the
	// agent sends of it are written under ArtifactsDir, on the server,
	// before Wait returns.
	Artifacts    *agentproto.Artifacts
	ArtifactsDir string
	// Inputs are the stages whose artifacts the stage is given, and
	// InputDirs where the server keeps each one's: the agent fetches them
	// before the command.
	Inputs []agentproto.Input
	// Cache are the stage's declared caches, and UnstableExit the codes
	// after which the agent saves them as it does after 0.
	Cache        []agentproto.Cache
	UnstableExit []int
	InputDirs    map[string]string
	// SecretFiles are the file secrets the agent writes for the stage.
	SecretFiles []agentproto.SecretFile
	// Retries is the stage's retries, which the agent acts on.
	Retries int
}

// stage is one stage given to an agent, from Start to its end.
type stage struct {
	h      *Hub
	id     string
	agent  Who
	meta   StageMeta
	a      *agentproto.Assignment
	out    *writer
	logged bool // a log upload has begun; there is one
	kept   bool // an artifacts upload has begun; there is one
	// keptErr is why the artifacts the agent sent were refused; the stage
	// ends failed with it.
	keptErr error

	cancel    chan struct{} // closed when the stage is to be killed
	done      chan struct{} // closed when it has ended
	code      int
	tries     int // how many times the agent ran the command
	err       error
	claimed   bool
	claimedAt time.Time
	finished  bool
}

// Start gives a stage to the agent who. The stage is the agent's
// to claim; its output reaches spec.Output as the agent sends it. ctx
// ending kills it, as it kills a local stage.
func (h *Hub) Start(ctx context.Context, agent Who, spec runner.Spec, meta StageMeta) (runner.Proc, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s := &stage{
		h: h, id: rand.Text(), agent: agent, meta: meta, out: &writer{w: spec.Output},
		cancel: make(chan struct{}), done: make(chan struct{}),
		a: &agentproto.Assignment{
			Job: meta.Job, Number: meta.Number, Stage: meta.Stage, Command: spec.Command, Env: spec.Env,
			Workspace: meta.Workspace, Workdir: meta.Workdir, Junit: meta.Junit,
			Metrics: meta.Metrics, Baseline: meta.Baseline, Artifacts: meta.Artifacts, Inputs: meta.Inputs,
			Cache: meta.Cache, UnstableExit: meta.UnstableExit, Retries: meta.Retries, SecretFiles: meta.SecretFiles,
		},
	}
	s.a.ID = s.id
	h.mu.Lock()
	c := h.conns[agent]
	if c == nil || !c.free() {
		h.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrNoAgent, agent.Name)
	}
	h.stages[s.id] = s
	c.queued = append(c.queued, s)
	if c.id.Build == 0 {
		c.workspaces[meta.BuildID] = meta.Workspace
	}
	c.poke()
	h.mu.Unlock()
	go func() {
		select {
		case <-ctx.Done():
			s.Kill(0)
		case <-s.done:
		}
	}()
	return s, nil
}

// Wait implements runner.Proc: the exit code the agent said, or -1 with
// why when the stage ended without one.
func (s *stage) Wait() (int, error) {
	<-s.done
	return s.code, s.err
}

// Kill implements runner.Proc. The agent kills the stage's process group
// with its own grace; one not claimed yet never starts. Kill does not
// wait: Wait does.
func (s *stage) Kill(time.Duration) {
	h := s.h
	h.mu.Lock()
	defer h.mu.Unlock()
	if s.finished {
		return
	}
	if !s.claimed {
		if c := h.conns[s.agent]; c != nil {
			c.queued = slices.DeleteFunc(c.queued, func(x *stage) bool { return x == s })
		}
		h.end(s, -1, nil)
		return
	}
	select {
	case <-s.cancel:
	default:
		close(s.cancel)
	}
}

// Tries is how many times the agent ran the command: 1 until it says more.
func (s *stage) Tries() int {
	if s.tries < 1 {
		return 1
	}
	return s.tries
}

// Group implements runner.Proc: there is no process group of the stage
// on this machine.
func (s *stage) Group() runner.Group { return runner.Group{} }

func (s *stage) label() string {
	return StageRef{Job: s.meta.Job, Number: s.meta.Number, Stage: s.meta.Stage}.label()
}

func (r StageRef) label() string { return fmt.Sprintf("%s#%d/%s", r.Job, r.Number, r.Stage) }

// end finishes s with code and why. h.mu is held.
func (h *Hub) end(s *stage, code int, why error) {
	if s.finished {
		return
	}
	s.finished, s.code, s.err = true, code, why
	s.out.close()
	delete(h.stages, s.id)
	if c := h.conns[s.agent]; c != nil {
		delete(c.running, s.id)
		if c.busy() == 0 {
			c.idle = h.o.Now()
		}
	}
	close(s.done)
}
