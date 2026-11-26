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
	"github.com/ndthuan/kipp/internal/placement"
	"github.com/ndthuan/kipp/internal/runnerd"
	"github.com/ndthuan/kipp/internal/runnerhub"
	"github.com/ndthuan/kipp/internal/store"
	"github.com/ndthuan/kipp/internal/waitfor"
)

// A stage's workdir is a path inside the directory its build runs in, on
// the server as on a runner.
func TestALocalStagesWorkdirIsInsideTheBuildsDirectory(t *testing.T) {
	h := newHarness(t, map[string]string{"a": "name: a\nstages:\n  s: {workdir: sub/dir, run: pwd}\n"})
	b := h.wait(t, h.trigger(t, "a").ID)
	if b.Status != store.Success {
		t.Fatalf("%s: %s", b.Status, h.log(t, b, "s"))
	}
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

type runners struct {
	t       *testing.T
	hub     *runnerhub.Hub
	srv     *httptest.Server
	ids     map[string]*runnerhub.Identity
	down    atomic.Bool
	clock   atomic.Int64
	stopHub context.CancelFunc
}

func (a *runners) now() time.Time { return time.Unix(0, a.clock.Load()) }

func (a *runners) advance(d time.Duration) { a.clock.Add(int64(d)) }

func newRunners(t *testing.T, ids ...runnerhub.Identity) *runners {
	t.Helper()
	a := &runners{t: t, ids: map[string]*runnerhub.Identity{}}
	a.clock.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	for _, id := range ids {
		a.ids[auth.HashToken(tokenOf(id.Name))] = &id
	}
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
	a.stopHub = cancel
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

func (a *runners) runBox(workdir string, env ...string) {
	a.t.Helper()
	a.run("box", workdir, env...)
	eventually(a.t, "box is free", func() bool { return a.hub.Free(runnerhub.Who{Name: "box"}) })
}

func (a *runners) run(name, workdir string, env ...string) {
	a.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		_ = runnerd.Run(ctx, runnerd.Config{URL: a.srv.URL, Token: tokenOf(name), Workdir: workdir, Version: "v-test",
			Env: append([]string{"PATH=" + os.Getenv("PATH")}, env...), Log: slog.New(slog.DiscardHandler), Retry: 20 * time.Millisecond})
	}()
	a.t.Cleanup(func() { cancel(); <-ended })
	eventually(a.t, name+" is online", func() bool {
		return slices.ContainsFunc(a.hub.Candidates(), func(c placement.Candidate) bool { return c.Name == name && c.Reachable })
	})
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
	want := "on box in " + filepath.Join(dir, "a", "1", "s") + " as a/s ***"
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

// Two stages of one build that run at once on one runner each have a
// workspace of their own there, so both may clone into src.
func TestParallelStagesOnOneRunnerEachHaveTheirOwnWorkspace(t *testing.T) {
	ag := newRunners(t, runnerhub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 2, Enabled: true})
	dir := t.TempDir()
	ag.runBox(dir)
	meet := t.TempDir()
	stage := func(me, other string) string {
		return "  " + me + ": {runs_on: linux, run: 'mkdir -p src && echo " + me + " > src/who && touch " + filepath.Join(meet, me) +
			" && while [ ! -e " + filepath.Join(meet, other) + " ]; do sleep 0.05; done; echo \"$KIPP_WORKSPACE $(cat src/who)\"'}\n"
	}
	h := newHarness(t, map[string]string{"a": "name: a\nstages:\n" + stage("one", "two") + stage("two", "one")}, ag.opts)
	b := h.wait(t, h.trigger(t, "a").ID)
	if b.Status != store.Success {
		t.Fatalf("%s: %s %s", b.Status, h.log(t, b, "one"), h.log(t, b, "two"))
	}
	for _, s := range []string{"one", "two"} {
		if got, want := strings.TrimSpace(h.log(t, b, s)), filepath.Join(dir, "a", "1", s)+" "+s; got != want {
			t.Errorf("stage %s: %q, want %q", s, got, want)
		}
	}
	eventually(t, "the build's directory on the runner is gone", func() bool {
		_, err := os.Stat(filepath.Join(dir, "a", "1"))
		return errors.Is(err, os.ErrNotExist)
	})
}

// A line of needs on a runner carries on in one workspace, as it did when
// every stage of a build shared one, and artifacts_from still gives a
// stage what the one it needs kept.
func TestALineOfNeedsOnARunnerSharesItsWorkspaceAndGetsItsArtifacts(t *testing.T) {
	ag := newRunners(t, runnerhub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
	dir := t.TempDir()
	ag.runBox(dir)
	h := newHarness(t, map[string]string{"a": `name: a
stages:
  build:
    runs_on: linux
    artifacts: {paths: [out/app.txt]}
    run: 'mkdir -p src out && echo tree > src/f && echo app > out/app.txt'
  deploy:
    needs: [build]
    runs_on: linux
    artifacts_from: [build]
    run: 'echo "$KIPP_WORKSPACE $(cat src/f) $(cat "$KIPP_WORKSPACE/.kipp/artifacts/build/out/app.txt")"'
`}, ag.opts)
	b := h.wait(t, h.trigger(t, "a").ID)
	log := h.log(t, b, "deploy")
	if want := filepath.Join(dir, "a", "1", "build") + " tree app"; b.Status != store.Success || strings.TrimSpace(log) != want {
		t.Fatalf("%s: %q, want %q", b.Status, log, want)
	}
}

func TestAStageThatRunnersCarryButCannotTakeWaitsAndSaysWhy(t *testing.T) {
	ag := newRunners(t, runnerhub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
	ag.runBox(t.TempDir())
	ready := filepath.Join(t.TempDir(), "go")
	hold := "'while [ ! -e " + ready + " ]; do sleep 0.05; done'"
	h := newHarness(t, map[string]string{"a": "name: a\nstages:\n  one: {runs_on: linux, run: " + hold + "}\n  two: {runs_on: linux, run: " + hold + "}\n"}, ag.opts)
	b := h.trigger(t, "a")
	eventually(t, "a stage waits for the runner", func() bool {
		q, err := h.d.Queue(context.Background())
		return err == nil && len(q.Running) == 1 && len(q.Running[0].Waiting) == 1 &&
			(q.Running[0].Waiting["one"] == "waiting for a runner with labels [linux]" || q.Running[0].Waiting["two"] == "waiting for a runner with labels [linux]")
	})
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
	ag.advance(4 * time.Second)
	b = h.wait(t, b.ID)
	cancel()
	<-ended
	if log := h.log(t, b, "s"); b.Status != store.Failed || !strings.Contains(log, "kipp: the runner went offline") {
		t.Fatalf("%s: %q", b.Status, log)
	}
}

func TestAStageGoesToItsFallbackWhenNoRunnerOfItsLabelsIsOnlineAndEnabled(t *testing.T) {
	for _, c := range []struct {
		name  string
		linux runnerhub.Identity
		run   bool
	}{
		{"none registered", runnerhub.Identity{Name: "other", Labels: []string{"arm"}, Executors: 1, Enabled: true}, false},
		{"offline", runnerhub.Identity{Name: "pc", Labels: []string{"linux"}, Executors: 1, Enabled: true}, false},
		{"online and disabled", runnerhub.Identity{Name: "pc", Labels: []string{"linux"}, Executors: 1}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			ag := newRunners(t, runnerhub.Identity{Name: "box", Labels: []string{"mac"}, Executors: 1, Enabled: true}, c.linux)
			ag.runBox(t.TempDir())
			if c.run {
				ag.run(c.linux.Name, t.TempDir())
			}
			h := newHarness(t, map[string]string{"a": "name: a\nstages:\n  s: {runs_on: linux, fallback_runs_on: mac, run: 'echo \"fallback=$KIPP_FALLBACK on $KIPP_RUNNER\"'}\n"}, ag.opts)
			b := h.wait(t, h.trigger(t, "a").ID)
			log := h.log(t, b, "s")
			if b.Status != store.Success || h.stages(t, b.ID)["s"].RanOn != "box" {
				t.Fatalf("%s on %q: %s", b.Status, h.stages(t, b.ID)["s"].RanOn, log)
			}
			if !strings.Contains(log, "kipp: no runner with labels [linux] is online and enabled, so the stage runs on one with labels [mac] (fallback_runs_on)\n") || !strings.Contains(log, "fallback=1 on box") {
				t.Fatalf("log %q", log)
			}
		})
	}
}

func TestAStageWithAFallbackRunsOnItsOwnLabelsWhenARunnerIsFree(t *testing.T) {
	ag := newRunners(t, runnerhub.Identity{Name: "box", Labels: []string{"mac"}, Executors: 1, Enabled: true}, runnerhub.Identity{Name: "pc", Labels: []string{"linux"}, Executors: 1, Enabled: true})
	ag.runBox(t.TempDir())
	ag.run("pc", t.TempDir())
	h := newHarness(t, map[string]string{"a": "name: a\nstages:\n  s: {runs_on: linux, fallback_runs_on: mac, run: 'echo \"fallback=[$KIPP_FALLBACK] on $KIPP_RUNNER\"'}\n"}, ag.opts)
	b := h.wait(t, h.trigger(t, "a").ID)
	log := h.log(t, b, "s")
	if b.Status != store.Success || !strings.Contains(log, "fallback=[] on pc") || strings.Contains(log, "fallback_runs_on") {
		t.Fatalf("%s: %q", b.Status, log)
	}
}

func TestAStageWithAFallbackWaitsForABusyRunnerOfItsOwnLabels(t *testing.T) {
	ag := newRunners(t, runnerhub.Identity{Name: "box", Labels: []string{"mac"}, Executors: 1, Enabled: true}, runnerhub.Identity{Name: "pc", Labels: []string{"linux"}, Executors: 1, Enabled: true})
	ag.runBox(t.TempDir())
	ag.run("pc", t.TempDir())
	ready := filepath.Join(t.TempDir(), "go")
	hold := "'while [ ! -e " + ready + " ]; do sleep 0.05; done; echo \"on $KIPP_RUNNER\"'"
	h := newHarness(t, map[string]string{"a": "name: a\nstages:\n  one: {runs_on: linux, fallback_runs_on: mac, run: " + hold + "}\n  two: {runs_on: linux, fallback_runs_on: mac, run: " + hold + "}\n"}, ag.opts)
	b := h.trigger(t, "a")
	eventually(t, "one stage runs on pc and the other waits for it", func() bool {
		w := h.d.Waiting(b.ID)
		return len(w) == 1 && (w["one"] == "waiting for a runner with labels [linux]" || w["two"] == "waiting for a runner with labels [linux]")
	})
	if err := os.WriteFile(ready, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	b = h.wait(t, b.ID)
	st := h.stages(t, b.ID)
	if b.Status != store.Success || st["one"].RanOn != "pc" || st["two"].RanOn != "pc" {
		t.Fatalf("%s: one on %q, two on %q", b.Status, st["one"].RanOn, st["two"].RanOn)
	}
}

func TestAServerThatStopsIsNotHeldUpByAStageOnARunnerItNoLongerHears(t *testing.T) {
	ag := newRunners(t, runnerhub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
	ag.runBox(t.TempDir())
	h := newHarness(t, map[string]string{"a": "name: a\nstages:\n  s: {runs_on: linux, run: 'echo started; sleep 30'}\n"}, ag.opts)
	b := h.trigger(t, "a")
	eventually(t, "the stage started", func() bool {
		data, _ := os.ReadFile(h.d.LogPath("a", 1, "s"))
		return strings.Contains(string(data), "started")
	})
	ag.down.Store(true)
	ag.srv.CloseClientConnections()
	stopped := make(chan struct{})
	go func() { ag.stopHub(); h.stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(waitfor.Safety):
		t.Fatal("the dispatcher still waits for the runner to say the stage ended")
	}
	got, err := h.st.Build(context.Background(), b.ID)
	if err != nil || got.Status != store.Lost {
		t.Fatalf("the build is %v, %v", got, err)
	}
}

func TestAStageItsRunnerNeverTookIsPlacedAgainWhenThatRunnerGoesOffline(t *testing.T) {
	quickPlaces(t)
	ag := newRunners(t,
		runnerhub.Identity{Name: "silent", Labels: []string{"linux"}, Executors: 1, Enabled: true},
		runnerhub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
	req, err := http.NewRequest(http.MethodPost, ag.srv.URL+"/api/v1/runner/connect", strings.NewReader(`{"Version":"v-test"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tokenOf("silent"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatal(resp, err)
	}
	_ = resp.Body.Close()
	h := newHarness(t, map[string]string{"a": "name: a\nstages:\n  s: {runs_on: linux, run: 'echo ran'}\n"}, ag.opts)
	b := h.trigger(t, "a")
	eventually(t, "the stage is given to the runner that will not take it", func() bool {
		return slices.ContainsFunc(ag.hub.Runners(), func(r runnerhub.Status) bool { return r.Name == "silent" && len(r.Stages) == 1 })
	})
	ag.advance(4 * time.Second)
	eventually(t, "the stage waits for a runner again", func() bool { return h.d.Waiting(b.ID)["s"] != "" })
	ag.run("box", t.TempDir())
	b = h.wait(t, b.ID)
	if log := h.log(t, b, "s"); b.Status != store.Success || !strings.Contains(log, "kipp: the runner went offline before it took the stage") || !strings.HasSuffix(log, "ran\n") {
		t.Fatalf("%s: %q", b.Status, log)
	}
	if ran := h.stages(t, b.ID)["s"]; ran.RanOn != "box" {
		t.Fatalf("the stage is recorded on %q", ran.RanOn)
	}
}
