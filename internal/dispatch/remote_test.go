package dispatch

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/runnerd"
	"github.com/ndthuan/kipp/internal/runnerhub"
	"github.com/ndthuan/kipp/internal/store"
)

// A stage's workdir is a path inside the directory its build runs in, on
// the server as on a runner.
func TestALocalStagesWorkdirIsInsideTheBuildsDirectory(t *testing.T) {
	h := newHarness(t, map[string]string{"a": "name: a\nstages:\n  s: {workdir: sub/dir, run: pwd}\n"})
	b := h.wait(t, h.trigger(t, "a").ID)
	if b.Status != store.Success {
		t.Fatalf("%s: %s", b.Status, h.log(t, b, "s"))
	}
	// pwd prints the physical path: on macOS the temp dir's /var is a link to /private/var.
	root, err := filepath.EvalSymlinks(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "workspace", "a", "1", "sub", "dir")
	if got := strings.TrimSpace(h.log(t, b, "s")); got != want {
		t.Fatalf("pwd %q, want %q", got, want)
	}
}

// Nothing carries a label on a server that knows no runner or engine:
// that is a mistake in the file, and waiting would hide it.
func TestAStageWhoseLabelsNothingCarriesFailsAtOnce(t *testing.T) {
	h := newHarness(t, map[string]string{"a": "name: a\nstages:\n  s: {runs_on: [mars, arm64], run: echo ran}\n"})
	b := h.wait(t, h.trigger(t, "a").ID)
	log := h.log(t, b, "s")
	if b.Status != store.Failed || h.stages(t, b.ID)["s"].Status != store.StageFailed {
		t.Fatalf("%s: %s", b.Status, log)
	}
	if !strings.Contains(log, "kipp: no runner or engine carries labels [mars, arm64]") || strings.Contains(log, "ran") {
		t.Fatalf("log %q", log)
	}
}

// runners is a hub behind an httptest server, its standing runners by token.
type runners struct {
	t    *testing.T
	hub  *runnerhub.Hub
	srv  *httptest.Server
	ids  map[string]*runnerhub.Identity // by token hash
	down atomic.Bool                    // every call is answered 503
	// clock is the hub's time, which stands still until a test moves it: a
	// loaded machine that delays a runner's call cannot make the hub think
	// it silent.
	clock atomic.Int64
}

func (a *runners) now() time.Time { return time.Unix(0, a.clock.Load()) }

// advance moves the hub's time on by d.
func (a *runners) advance(d time.Duration) { a.clock.Add(int64(d)) }

func newRunners(t *testing.T, ids ...runnerhub.Identity) *runners {
	t.Helper()
	a := &runners{t: t, ids: map[string]*runnerhub.Identity{}}
	a.clock.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	for _, id := range ids {
		a.ids[auth.HashToken(tokenOf(id.Name))] = &id
	}
	// A runner is offline after three heartbeats without a call, by the
	// hub's clock, which only a test moves.
	a.hub = runnerhub.New(runnerhub.Options{Version: "v-test", Log: slog.New(slog.DiscardHandler), Now: a.now, Heartbeat: time.Second, ClaimWait: 200 * time.Millisecond,
		Standing: func(_ context.Context, hash string) (*runnerhub.Identity, error) {
			if id := a.ids[hash]; id != nil {
				c := *id
				return &c, nil
			}
			return nil, errors.New("unknown")
		}})
	a.hub.SetStanding(ids)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.hub.Run(ctx)
	hubHandler := a.hub.Handler()
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		hubHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func tokenOf(name string) string { return "kippa_token-of-" + name }

// runBox starts the runner called box, with env as its own environment,
// and waits until it is online.
func (a *runners) runBox(workdir string, env ...string) {
	a.t.Helper()
	const name = "box"
	ctx, cancel := context.WithCancel(context.Background())
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		_ = runnerd.Run(ctx, runnerd.Config{URL: a.srv.URL, Token: tokenOf(name), Workdir: workdir, Version: "v-test",
			Env: append([]string{"PATH=" + os.Getenv("PATH")}, env...), Log: slog.New(slog.DiscardHandler), Retry: 20 * time.Millisecond})
	}()
	a.t.Cleanup(func() { cancel(); <-ended })
	eventually(a.t, name+" is online", func() bool { return a.hub.Free(runnerhub.Who{Name: name}) })
}

func (a *runners) opts(o *Options) { o.Hub = a.hub }

func TestAStageWithRunsOnRunsOnTheRunnerAndSaysWhere(t *testing.T) {
	ag := newRunners(t, runnerhub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
	dir := t.TempDir()
	ag.runBox(dir)
	h := newHarness(t, map[string]string{"a": "name: a\nsecrets: [TOKEN]\nstages:\n  s: {runs_on: linux, run: 'echo \"on $KIPP_RUNNER in $KIPP_WORKSPACE as $KIPP_JOB/$KIPP_STAGE $TOKEN\"'}\n  t: {needs: [s], run: echo local}\n"}, ag.opts)
	b := h.wait(t, h.trigger(t, "a").ID)
	log := h.log(t, b, "s")
	if b.Status != store.Success {
		t.Fatalf("%s: %s", b.Status, log)
	}
	want := "on box in " + filepath.Join(dir, "a", "1") + " as a/s ***"
	if !strings.Contains(log, want) || strings.Contains(log, "value-of-TOKEN") {
		t.Fatalf("log %q, want %q masked", log, want)
	}
	st := h.stages(t, b.ID)
	if st["s"].RanOn != "box" || st["t"].RanOn != "local" {
		t.Fatalf("ran on %q and %q", st["s"].RanOn, st["t"].RanOn)
	}
	eventually(t, "the build's workspace on the runner is gone", func() bool {
		_, err := os.Stat(filepath.Join(dir, "a", "1"))
		return errors.Is(err, os.ErrNotExist)
	})
}

func TestAStageThatRunnersCarryButCannotTakeWaitsAndSaysWhy(t *testing.T) {
	ag := newRunners(t, runnerhub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
	ag.runBox(t.TempDir())
	ready := filepath.Join(t.TempDir(), "go")
	// Both stages hold the one executor until ready: either may take it
	// first, and the other must then wait for as long as the test looks.
	hold := "'while [ ! -e " + ready + " ]; do sleep 0.05; done'"
	h := newHarness(t, map[string]string{"a": "name: a\nstages:\n  one: {runs_on: linux, run: " + hold + "}\n  two: {runs_on: linux, run: " + hold + "}\n"}, ag.opts)
	b := h.trigger(t, "a")
	eventually(t, "a stage waits for the runner", func() bool {
		q, err := h.d.Queue(context.Background())
		return err == nil && len(q.Running) == 1 && len(q.Running[0].Waiting) == 1 &&
			(q.Running[0].Waiting["one"] == "waiting for a runner with labels [linux]" || q.Running[0].Waiting["two"] == "waiting for a runner with labels [linux]")
	})
	// the build's own page asks for the same, by build
	if w := h.d.Waiting(b.ID); len(w) != 1 || (w["one"] == "" && w["two"] == "") {
		t.Fatalf("the build's stages wait for %v", w)
	}
	eventually(t, "the stage that holds the runner says where it runs", func() bool {
		q, err := h.d.Queue(context.Background())
		if err != nil || len(q.Running) != 1 {
			return false
		}
		r := q.Running[0]
		return len(r.Remote) == 1 && (r.Remote["one"] == "box" || r.Remote["two"] == "box")
	})
	if err := os.WriteFile(ready, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	b = h.wait(t, b.ID)
	if b.Status != store.Success {
		t.Fatalf("%s", b.Status)
	}
	if w := h.d.Waiting(b.ID); w != nil {
		t.Fatalf("an ended build still waits for %v", w)
	}
	logs := h.log(t, b, "one") + h.log(t, b, "two")
	if !strings.Contains(logs, "kipp: waiting for a runner with labels [linux]\n") {
		t.Fatalf("the wait is not in the log: %q", logs)
	}
}

func TestAServerThatRunsNoStagesRefusesAStageThatNamesNoPlace(t *testing.T) {
	h := newHarness(t, map[string]string{"a": "name: a\nstages:\n  s: {run: echo ran}\n"}, func(o *Options) { o.RunsNoStages = true })
	b := h.wait(t, h.trigger(t, "a").ID)
	if log := h.log(t, b, "s"); b.Status != store.Failed || !strings.Contains(log, "kipp: the server runs no stages and the stage names no runs_on") {
		t.Fatalf("%s: %q", b.Status, log)
	}
}

func TestDefaultRunsOnSendsAStageThatNamesNoPlaceToAnRunner(t *testing.T) {
	ag := newRunners(t, runnerhub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
	ag.runBox(t.TempDir())
	h := newHarness(t, map[string]string{"a": "name: a\nstages:\n  s: {run: echo on $KIPP_RUNNER}\non_success: {run: echo hook}\n"}, ag.opts, func(o *Options) {
		o.RunsNoStages, o.DefaultRunsOn = true, []string{"linux"}
	})
	b := h.wait(t, h.trigger(t, "a").ID)
	if log := h.log(t, b, "s"); b.Status != store.Success || !strings.Contains(log, "on box") {
		t.Fatalf("%s: %q", b.Status, log)
	}
	if log := h.log(t, b, "hooks"); !strings.Contains(log, "hook") {
		t.Fatalf("the hook did not run on the server: %q", log)
	}
}

func TestTheJunitOfAStageOnAnRunnerIsRead(t *testing.T) {
	ag := newRunners(t, runnerhub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
	ag.runBox(t.TempDir())
	h := newHarness(t, map[string]string{"a": `name: a
stages:
  s:
    runs_on: linux
    workdir: src
    junit: "r/*.xml"
    run: |
      mkdir -p r
      echo '<testsuite name="s"><testcase name="ok"/><testcase name="bad"><failure message="no"/></testcase></testsuite>' > r/t.xml
`}, ag.opts)
	b := h.wait(t, h.trigger(t, "a").ID)
	tests, err := h.st.Tests(context.Background(), b.ID)
	if err != nil || len(tests) != 2 || tests[1].Status != "failed" {
		t.Fatalf("%v %+v: %s", err, tests, h.log(t, b, "s"))
	}
}

func TestAStageOnAnRunnerGetsOnlyTheServersVariablesItIsGivenByName(t *testing.T) {
	ag := newRunners(t, runnerhub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
	ag.runBox(t.TempDir(), "HOME=/runner/home")
	t.Setenv("KIPP_DRY", "0")
	t.Setenv("KIPP_TEST_SERVER_ONLY", "leak")
	h := newHarness(t, map[string]string{"a": "name: a\nstages:\n  s: {runs_on: linux, run: 'echo \"[$KIPP_DRY][$KIPP_TEST_SERVER_ONLY][$HOME][$TMPDIR]\"'}\n"}, ag.opts, func(o *Options) {
		o.RunnerEnv = []string{"KIPP_DRY"}
		o.PassEnv = []string{"*"}
	})
	b := h.wait(t, h.trigger(t, "a").ID)
	if log := h.log(t, b, "s"); !strings.Contains(log, "[0][][/runner/home][]") {
		t.Fatalf("%q", log)
	}
}

func TestRunnerEnvKeepsExactNamesOnly(t *testing.T) {
	got := RunnerEnv([]string{"KIPP_DRY=0", "KIPP_DRYER=1", "PATH=/bin", "HOME=/h", "TMPDIR=/t"}, []string{"KIPP_DRY"})
	if !slices.Equal(got, []string{"KIPP_DRY=0"}) {
		t.Fatal(got)
	}
	if _, err := ParseRunnerEnv("KIPP_DRY, LC_*"); err == nil {
		t.Fatal("a prefix was taken")
	}
	if names, err := ParseRunnerEnv("KIPP_DRY, HTTP_PROXY"); err != nil || !slices.Equal(names, []string{"KIPP_DRY", "HTTP_PROXY"}) {
		t.Fatal(names, err)
	}
}

// A stage given to a runner that goes away mid-stage fails, with why.
func TestAStageOnAnRunnerThatGoesOfflineFails(t *testing.T) {
	ag := newRunners(t, runnerhub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
	ctx, cancel := context.WithCancel(context.Background())
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		_ = runnerd.Run(ctx, runnerd.Config{URL: ag.srv.URL, Token: tokenOf("box"), Workdir: t.TempDir(), Version: "v-test", Env: []string{"PATH=" + os.Getenv("PATH")}, Log: slog.New(slog.DiscardHandler)})
	}()
	eventually(t, "box is online", func() bool { return ag.hub.Free(runnerhub.Who{Name: "box"}) })
	h := newHarness(t, map[string]string{"a": "name: a\nstages:\n  s: {runs_on: linux, run: 'echo started; sleep 30'}\n"}, ag.opts)
	b := h.trigger(t, "a")
	eventually(t, "the stage started", func() bool {
		data, _ := os.ReadFile(h.d.LogPath("a", 1, "s"))
		return strings.Contains(string(data), "started")
	})
	ag.down.Store(true)
	ag.srv.CloseClientConnections()
	ag.advance(4 * time.Second) // past three heartbeats without a call
	b = h.wait(t, b.ID)
	cancel()
	<-ended
	if log := h.log(t, b, "s"); b.Status != store.Failed || !strings.Contains(log, "kipp: the runner went offline") {
		t.Fatalf("%s: %q", b.Status, log)
	}
}
