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

	"github.com/ndthuan/kipp/internal/agent"
	"github.com/ndthuan/kipp/internal/agenthub"
	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/store"
)

// A stage's workdir is a path inside the directory its build runs in, on
// the server as on an agent.
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

// Nothing carries a label on a server that knows no agent or engine:
// that is a mistake in the file, and waiting would hide it.
func TestAStageWhoseLabelsNothingCarriesFailsAtOnce(t *testing.T) {
	h := newHarness(t, map[string]string{"a": "name: a\nstages:\n  s: {runs_on: [mars, arm64], run: echo ran}\n"})
	b := h.wait(t, h.trigger(t, "a").ID)
	log := h.log(t, b, "s")
	if b.Status != store.Failed || h.stages(t, b.ID)["s"].Status != store.StageFailed {
		t.Fatalf("%s: %s", b.Status, log)
	}
	if !strings.Contains(log, "kipp: no agent or engine carries labels [mars, arm64]") || strings.Contains(log, "ran") {
		t.Fatalf("log %q", log)
	}
}

// agents is a hub behind an httptest server, its standing agents by token.
type agents struct {
	t    *testing.T
	hub  *agenthub.Hub
	srv  *httptest.Server
	ids  map[string]*agenthub.Identity // by token hash
	down atomic.Bool                   // every call is answered 503
}

func newAgents(t *testing.T, ids ...agenthub.Identity) *agents {
	t.Helper()
	a := &agents{t: t, ids: map[string]*agenthub.Identity{}}
	for _, id := range ids {
		a.ids[auth.HashToken(tokenOf(id.Name))] = &id
	}
	// An agent is offline after three heartbeats without a call. A second
	// leaves room for a claim that comes late on a loaded machine, where a
	// tenth marked the agent offline between two of its claims.
	a.hub = agenthub.New(agenthub.Options{Version: "v-test", Log: slog.New(slog.DiscardHandler), Heartbeat: time.Second, ClaimWait: 200 * time.Millisecond,
		Standing: func(_ context.Context, hash string) (*agenthub.Identity, error) {
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

// runBox starts the agent called box, with env as its own environment,
// and waits until it is online.
func (a *agents) runBox(workdir string, env ...string) {
	a.t.Helper()
	const name = "box"
	ctx, cancel := context.WithCancel(context.Background())
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		_ = agent.Run(ctx, agent.Config{URL: a.srv.URL, Token: tokenOf(name), Workdir: workdir, Version: "v-test",
			Env: append([]string{"PATH=" + os.Getenv("PATH")}, env...), Log: slog.New(slog.DiscardHandler), Retry: 20 * time.Millisecond})
	}()
	a.t.Cleanup(func() { cancel(); <-ended })
	eventually(a.t, name+" is online", func() bool { return a.hub.Free(agenthub.Who{Name: name}) })
}

func (a *agents) opts(o *Options) { o.Hub = a.hub }

func TestAStageWithRunsOnRunsOnTheAgentAndSaysWhere(t *testing.T) {
	ag := newAgents(t, agenthub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
	dir := t.TempDir()
	ag.runBox(dir)
	h := newHarness(t, map[string]string{"a": "name: a\nsecrets: [TOKEN]\nstages:\n  s: {runs_on: linux, run: 'echo \"on $KIPP_AGENT in $KIPP_WORKSPACE as $KIPP_JOB/$KIPP_STAGE $TOKEN\"'}\n  t: {needs: [s], run: echo local}\n"}, ag.opts)
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
	eventually(t, "the build's workspace on the agent is gone", func() bool {
		_, err := os.Stat(filepath.Join(dir, "a", "1"))
		return errors.Is(err, os.ErrNotExist)
	})
}

func TestAStageThatAgentsCarryButCannotTakeWaitsAndSaysWhy(t *testing.T) {
	ag := newAgents(t, agenthub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
	ag.runBox(t.TempDir())
	ready := filepath.Join(t.TempDir(), "go")
	h := newHarness(t, map[string]string{"a": "name: a\nstages:\n  one: {runs_on: linux, run: 'while [ ! -e " + ready + " ]; do sleep 0.05; done'}\n  two: {runs_on: linux, run: echo two}\n"}, ag.opts)
	b := h.trigger(t, "a")
	eventually(t, "a stage waits for the agent", func() bool {
		q, err := h.d.Queue(context.Background())
		return err == nil && len(q.Running) == 1 && len(q.Running[0].Waiting) == 1 &&
			(q.Running[0].Waiting["one"] == "waiting for an agent with labels [linux]" || q.Running[0].Waiting["two"] == "waiting for an agent with labels [linux]")
	})
	if err := os.WriteFile(ready, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	b = h.wait(t, b.ID)
	if b.Status != store.Success {
		t.Fatalf("%s", b.Status)
	}
	logs := h.log(t, b, "one") + h.log(t, b, "two")
	if !strings.Contains(logs, "kipp: waiting for an agent with labels [linux]\n") {
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

func TestDefaultRunsOnSendsAStageThatNamesNoPlaceToAnAgent(t *testing.T) {
	ag := newAgents(t, agenthub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
	ag.runBox(t.TempDir())
	h := newHarness(t, map[string]string{"a": "name: a\nstages:\n  s: {run: echo on $KIPP_AGENT}\non_success: {run: echo hook}\n"}, ag.opts, func(o *Options) {
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

func TestAStageOnAnAgentTakesNoneOfTheServersExecutors(t *testing.T) {
	ag := newAgents(t, agenthub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
	ag.runBox(t.TempDir())
	ready := filepath.Join(t.TempDir(), "go")
	h := newHarness(t, map[string]string{
		"busy":   "name: busy\nstages:\n  s: {run: 'while [ ! -e " + ready + " ]; do sleep 0.05; done'}\n",
		"remote": "name: remote\nstages:\n  s: {runs_on: linux, run: echo remote}\n",
	}, ag.opts, func(o *Options) { o.Executors, o.Builds = 1, 2 })
	busy := h.trigger(t, "busy")
	h.untilRunning(t, busy.ID)
	if b := h.wait(t, h.trigger(t, "remote").ID); b.Status != store.Success {
		t.Fatalf("%s", b.Status)
	}
	if err := os.WriteFile(ready, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h.wait(t, busy.ID)
}

func TestTheBuildsLimitIsApartFromTheServersExecutors(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "go")
	h := newHarness(t, map[string]string{
		"a": "name: a\nstages:\n  s: {run: 'while [ ! -e " + ready + " ]; do sleep 0.05; done'}\n",
		"b": "name: b\nstages:\n  s: {run: 'true'}\n",
	}, func(o *Options) { o.Executors, o.Builds = 4, 1 })
	first := h.trigger(t, "a")
	h.untilRunning(t, first.ID)
	second := h.trigger(t, "b")
	eventually(t, "the second build waits for the builds limit", func() bool {
		q, err := h.d.Queue(context.Background())
		return err == nil && len(q.Queued) == 1 && q.Queued[0].Waiting == WaitExecutor
	})
	if err := os.WriteFile(ready, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h.wait(t, first.ID)
	h.wait(t, second.ID)
}

func TestTheJunitOfAStageOnAnAgentIsRead(t *testing.T) {
	ag := newAgents(t, agenthub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
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

func TestAStageOnAnAgentGetsOnlyTheServersVariablesItIsGivenByName(t *testing.T) {
	ag := newAgents(t, agenthub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
	ag.runBox(t.TempDir(), "HOME=/agent/home")
	t.Setenv("KIPP_DRY", "0")
	t.Setenv("KIPP_TEST_SERVER_ONLY", "leak")
	h := newHarness(t, map[string]string{"a": "name: a\nstages:\n  s: {runs_on: linux, run: 'echo \"[$KIPP_DRY][$KIPP_TEST_SERVER_ONLY][$HOME][$TMPDIR]\"'}\n"}, ag.opts, func(o *Options) {
		o.AgentEnv = []string{"KIPP_DRY"}
		o.PassEnv = []string{"*"}
	})
	b := h.wait(t, h.trigger(t, "a").ID)
	if log := h.log(t, b, "s"); !strings.Contains(log, "[0][][/agent/home][]") {
		t.Fatalf("%q", log)
	}
}

func TestAgentEnvKeepsExactNamesOnly(t *testing.T) {
	got := AgentEnv([]string{"KIPP_DRY=0", "KIPP_DRYER=1", "PATH=/bin", "HOME=/h", "TMPDIR=/t"}, []string{"KIPP_DRY"})
	if !slices.Equal(got, []string{"KIPP_DRY=0"}) {
		t.Fatal(got)
	}
	if _, err := ParseAgentEnv("KIPP_DRY, LC_*"); err == nil {
		t.Fatal("a prefix was taken")
	}
	if names, err := ParseAgentEnv("KIPP_DRY, HTTP_PROXY"); err != nil || !slices.Equal(names, []string{"KIPP_DRY", "HTTP_PROXY"}) {
		t.Fatal(names, err)
	}
}

// A stage given to an agent that goes away mid-stage fails, with why.
func TestAStageOnAnAgentThatGoesOfflineFails(t *testing.T) {
	ag := newAgents(t, agenthub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
	ctx, cancel := context.WithCancel(context.Background())
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		_ = agent.Run(ctx, agent.Config{URL: ag.srv.URL, Token: tokenOf("box"), Workdir: t.TempDir(), Version: "v-test", Env: []string{"PATH=" + os.Getenv("PATH")}, Log: slog.New(slog.DiscardHandler)})
	}()
	eventually(t, "box is online", func() bool { return ag.hub.Free(agenthub.Who{Name: "box"}) })
	h := newHarness(t, map[string]string{"a": "name: a\nstages:\n  s: {runs_on: linux, run: 'echo started; sleep 30'}\n"}, ag.opts)
	b := h.trigger(t, "a")
	eventually(t, "the stage started", func() bool {
		data, _ := os.ReadFile(h.d.LogPath("a", 1, "s"))
		return strings.Contains(string(data), "started")
	})
	ag.down.Store(true)
	ag.srv.CloseClientConnections()
	b = h.wait(t, b.ID)
	cancel()
	<-ended
	if log := h.log(t, b, "s"); b.Status != store.Failed || !strings.Contains(log, "kipp: the agent went offline") {
		t.Fatalf("%s: %q", b.Status, log)
	}
}
