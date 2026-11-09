// Package runnerd runs the stages a kipp server gives it, on the machine or
// in the container it runs in. It dials the server and holds nothing of
// its own: the server says who it is, what it carries and what to run.
// The standing runner (kipp runner) and the one kipp starts in a container
// (kipp-runner) are this package, and it imports nothing of the server.
package runnerd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ndthuan/kipp/internal/runnerproto"
	"github.com/ndthuan/kipp/internal/version"
)

// ErrTokenRefused is the server refusing the runner's token: it was never
// one, or it was replaced, or the runner or the build it was for is gone.
var ErrTokenRefused = errors.New("the server refused the runner's token")

// VersionError is a server of another kipp version: the two speak the
// protocol of their own version and no other.
type VersionError struct{ Runner, Server string }

func (e *VersionError) Error() string {
	return fmt.Sprintf("the runner is kipp %s and the server kipp %s: run the server's version on the runner", e.Runner, e.Server)
}

// Config is how a runner runs.
type Config struct {
	URL     string // the server, as the runner reaches it
	Token   string
	Workdir string // the builds' workspaces go under it
	Shell   string // default sh
	// Env is the runner's own environment, put before what the server
	// sends a stage; nil is the process's, without the runner's token.
	Env     []string
	Version string // default version.Version
	Client  *http.Client
	Log     *slog.Logger
	// Retry is the wait after a call that did not reach the server, before
	// it is made again; default 2 s.
	Retry time.Duration
	// Bin is KIPP_BIN for the stages: the runner's own executable, which
	// runs kipp's commands inside a stage; default this process's.
	Bin string
}

// daemon is one running Run.
type daemon struct {
	c    Config
	name string
	exec int // executors; 0 is no limit

	mu      sync.Mutex
	running map[string]*job // by assignment id
	beat    time.Duration
}

// job is an assignment this runner runs.
type job struct {
	a      *runnerproto.Assignment
	cancel chan struct{}
	once   sync.Once
	killed bool // the server no longer knows it: its workspace goes too
	said   bool // its end is being told to the server
	tries  int  // how many times its command ran
}

// said marks j as one whose end the runner is telling the server: a kill
// that comes after it is the server having ended it, not lost it.
func (a *daemon) said(j *job) {
	a.mu.Lock()
	j.said = true
	a.mu.Unlock()
}

func (j *job) stop() { j.once.Do(func() { close(j.cancel) }) }

// TokenEnv is where a runner may be given its token instead of a flag,
// which every process of the machine can read; a stage never sees it.
const TokenEnv = "KIPP_RUNNER_TOKEN"

// Run connects and runs what the server gives until ctx ends, which kills
// what still runs. It returns ErrTokenRefused or a *VersionError when the
// server will not have it; a server it cannot reach is tried again.
func Run(ctx context.Context, c Config) error {
	c = defaults(c)
	if err := os.MkdirAll(c.Workdir, 0o750); err != nil {
		return fmt.Errorf("the runner's workdir: %w", err)
	}
	a := &daemon{c: c, running: map[string]*job{}}
	// Nothing runs yet: what a stage of a runner that died left is stale.
	if err := os.RemoveAll(c.secretFilesDir()); err != nil {
		c.Log.Warn("leftover secret files could not be removed", "err", err)
	}
	if err := os.RemoveAll(c.tmpDir()); err != nil {
		c.Log.Warn("leftover KIPP_TMP directories could not be removed", "err", err)
	}
	resp, err := a.connect(ctx, true)
	if err != nil {
		return err
	}
	a.adopt(resp)
	c.Log.Info("runner connected", "name", resp.Name, "labels", strings.Join(resp.Labels, ","), "server", c.URL)
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() { stop(a.heartbeat(ctx)) })
	err = a.claimLoop(ctx, &wg)
	stop(err)
	if cause := context.Cause(ctx); !errors.Is(cause, context.Canceled) {
		return cause
	}
	return err
}

func defaults(c Config) Config {
	if c.Shell == "" {
		c.Shell = "sh"
	}
	if c.Env == nil {
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, TokenEnv+"=") {
				c.Env = append(c.Env, kv)
			}
		}
	}
	if c.Version == "" {
		c.Version = version.Version
	}
	if c.Client == nil {
		c.Client = &http.Client{}
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	if c.Retry <= 0 {
		c.Retry = 2 * time.Second
	}
	if c.Bin == "" {
		// A system that cannot say leaves KIPP_BIN unset, not wrong.
		c.Bin, _ = os.Executable()
	}
	c.URL = strings.TrimRight(c.URL, "/")
	return c
}

// connect says who the runner is, until the server answers or ctx ends.
func (a *daemon) connect(ctx context.Context, fresh bool) (*runnerproto.ConnectResponse, error) {
	for {
		// What runs is read again on every try: the server ends what a
		// connect does not list.
		req := runnerproto.ConnectRequest{OS: runtime.GOOS, Arch: runtime.GOARCH, Version: a.c.Version, Fresh: fresh, Running: a.runningIDs()}
		var resp runnerproto.ConnectResponse
		err := a.call(ctx, http.MethodPost, runnerproto.ConnectPath, req, &resp)
		if !retryable(err) {
			return &resp, err
		}
		a.c.Log.Warn("runner cannot reach the server", "url", a.c.URL, "err", err)
		if !sleep(ctx, a.c.Retry) {
			return nil, ctx.Err()
		}
	}
}

// adopt takes what the server said of the runner.
func (a *daemon) adopt(resp *runnerproto.ConnectResponse) {
	a.mu.Lock()
	a.name, a.exec, a.beat = resp.Name, resp.Executors, resp.Heartbeat
	if a.beat <= 0 {
		a.beat = 30 * time.Second
	}
	a.mu.Unlock()
	for _, id := range resp.Kill {
		a.kill(id)
	}
}

// nameOf is the name the server last gave the runner.
func (a *daemon) nameOf() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.name
}

// heartbeat connects again every beat, so that the server knows the runner
// is there, and kills what the server says it no longer knows.
func (a *daemon) heartbeat(ctx context.Context) error {
	for {
		a.mu.Lock()
		beat := a.beat
		a.mu.Unlock()
		if !sleep(ctx, beat) {
			return nil
		}
		resp, err := a.connect(ctx, false)
		if err != nil {
			return err
		}
		a.adopt(resp)
	}
}

func (a *daemon) runningIDs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	ids := make([]string, 0, len(a.running))
	for id := range a.running {
		ids = append(ids, id)
	}
	return ids
}

func (a *daemon) kill(id string) {
	a.mu.Lock()
	j := a.running[id]
	if j != nil && j.said {
		j = nil
	}
	if j != nil {
		j.killed = true
	}
	a.mu.Unlock()
	if j != nil {
		a.c.Log.Info("killing a stage the server no longer runs", "stage", j.a.Stage, "job", j.a.Job, "number", j.a.Number)
		j.stop()
	}
}

// claimLoop asks for a stage whenever an executor is free, and runs each
// one it is given in a goroutine of wg.
func (a *daemon) claimLoop(ctx context.Context, wg *sync.WaitGroup) error {
	free := make(chan struct{}, 1)
	for ctx.Err() == nil {
		if !a.hasRoom() {
			select {
			case <-free:
			case <-ctx.Done():
				return nil
			}
			continue
		}
		claim, err := a.claim(ctx)
		if err != nil {
			return err
		}
		a.endBuilds(claim.Ended)
		if as := claim.Assignment; as != nil {
			j := a.add(as)
			wg.Go(func() {
				a.runJob(ctx, j)
				select {
				case free <- struct{}{}:
				default:
				}
			})
		}
	}
	return nil
}

func (a *daemon) hasRoom() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.exec <= 0 || len(a.running) < a.exec
}

func (a *daemon) add(as *runnerproto.Assignment) *job {
	j := &job{a: as, cancel: make(chan struct{})}
	a.mu.Lock()
	a.running[as.ID] = j
	a.mu.Unlock()
	return j
}

// claim waits for a stage, until the server answers or ctx ends.
func (a *daemon) claim(ctx context.Context) (*runnerproto.Claim, error) {
	for {
		var claim runnerproto.Claim
		err := a.call(ctx, http.MethodGet, runnerproto.ClaimPath, nil, &claim)
		if se, ok := errors.AsType[*statusError](err); ok && se.code == http.StatusBadRequest {
			// The server counts the runner offline: it started again, or
			// heartbeats were lost. Connecting again is the answer.
			resp, err := a.connect(ctx, false)
			if err != nil {
				return &runnerproto.Claim{}, err
			}
			a.adopt(resp)
			continue
		}
		if !retryable(err) {
			return &claim, err
		}
		if !sleep(ctx, a.c.Retry) {
			return &runnerproto.Claim{}, nil
		}
	}
}

func (a *daemon) endBuilds(ended []runnerproto.Ended) {
	for _, e := range ended {
		if err := removeWorkspace(a.c.Workdir, e.Workspace); err != nil {
			a.c.Log.Warn("removing a build's workspace", "workspace", e.Workspace, "err", err)
		}
	}
}

// sleep waits d, and reports false when ctx ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
