package enginepool

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/agent"
	"github.com/ndthuan/kipp/internal/agenthub"
	"github.com/ndthuan/kipp/internal/engine/docker"
	"github.com/ndthuan/kipp/internal/engine/docker/dockertest"
	"github.com/ndthuan/kipp/internal/placement"
	"github.com/ndthuan/kipp/internal/store"
)

var quiet = slog.New(slog.DiscardHandler)

// fixture is a pool of one fake engine, called fake, whose containers run
// the agent in this process when they start.
type fixture struct {
	t      *testing.T
	pool   *Pool
	hub    *agenthub.Hub
	engine *dockertest.Engine
	agents sync.WaitGroup
	stop   context.CancelFunc

	mu     sync.Mutex
	killed map[string]context.CancelFunc // stops a container's agent, by container id
}

func newFixture(t *testing.T, max int, opts ...func(*Options)) *fixture {
	t.Helper()
	f := &fixture{t: t, engine: dockertest.New(t, "golang:1.27")}
	// A second between heartbeats, so that a claim that comes late on a
	// loaded machine does not mark a container's agent offline.
	f.hub = agenthub.New(agenthub.Options{Version: "v-test", Log: quiet, Heartbeat: time.Second, ClaimWait: 200 * time.Millisecond})
	srv := httptest.NewServer(f.hub.Handler())
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	f.stop = cancel
	t.Cleanup(func() { cancel(); f.agents.Wait() })
	go f.hub.Run(ctx)
	f.engine.OnStart = func(c *dockertest.Container) { f.runAgentOf(ctx, c, srv.URL) }
	o := Options{Hub: f.hub, AgentURL: srv.URL, ServerID: "http://kipp.test", Log: quiet, Probe: time.Hour, ConnectWait: 5 * time.Second,
		Binary: func(goos, goarch string) ([]byte, error) { return []byte("agent for " + goos + "/" + goarch), nil }}
	for _, fn := range opts {
		fn(&o)
	}
	f.pool = New(o)
	go f.pool.Run(ctx)
	f.pool.SetEngines([]*store.Engine{{Name: "fake", Kind: "docker", URL: f.engine.URL(), Labels: []string{"linux"}, Max: max, Enabled: true}})
	return f
}

// runAgentOf does what the container's command would: run the agent with
// the URL and the token it was given.
func (f *fixture) runAgentOf(ctx context.Context, c *dockertest.Container, url string) {
	if !slices.Contains(strs(c.Create["Cmd"]), url) {
		return // a container of a test that wants no agent
	}
	token := ""
	for _, kv := range strs(c.Create["Env"]) {
		if v, ok := strings.CutPrefix(kv, agent.TokenEnv+"="); ok {
			token = v
		}
	}
	dir := f.t.TempDir()
	ctx, cancel := context.WithCancel(ctx)
	f.mu.Lock()
	if f.killed == nil {
		f.killed = map[string]context.CancelFunc{}
	}
	f.killed[c.ID] = cancel
	f.mu.Unlock()
	f.agents.Go(func() {
		_ = agent.Run(ctx, agent.Config{URL: url, Token: token, Workdir: dir, Version: "v-test", Env: []string{"PATH=" + os.Getenv("PATH")}, Log: quiet, Retry: 20 * time.Millisecond})
	})
}

// strs is a list of strings of a create call's body.
func strs(v any) []string {
	items, _ := v.([]any)
	var out []string
	for _, it := range items {
		s, _ := it.(string)
		out = append(out, s)
	}
	return out
}

func (f *fixture) reachable() {
	f.t.Helper()
	eventually(f.t, "the engine is reachable", func() bool {
		cs := f.pool.Candidates(0, "")
		return len(cs) == 1 && cs[0].Reachable
	})
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting until %s", what)
}

func build(id int64) *store.Build { return &store.Build{ID: id, Job: "linux-tests", Number: int(id)} }

func (f *fixture) acquire(b *store.Build, image string) string {
	f.t.Helper()
	var out bytes.Buffer
	name, err := f.pool.Acquire(context.Background(), b, "fake", image, &out)
	if err != nil {
		f.t.Fatalf("%v: %s", err, out.String())
	}
	return name
}

func TestAcquireStartsAContainerWithTheAgentInItAndATokenForTheBuild(t *testing.T) {
	f := newFixture(t, 2)
	f.reachable()
	var out bytes.Buffer
	name, err := f.pool.Acquire(context.Background(), build(7), "fake", "alpine:3.22", &out)
	if err != nil || name != "fake/1" {
		t.Fatal(name, err, out.String())
	}
	if !strings.Contains(out.String(), "kipp: pulling alpine:3.22") {
		t.Fatalf("the pull is not in the stage's log: %q", out.String())
	}
	cs := f.engine.Containers()
	if len(cs) != 1 {
		t.Fatalf("%d containers", len(cs))
	}
	for _, c := range cs {
		checkContainer(t, c, f.pool.o.AgentURL)
	}
	if !f.hub.Free(agenthub.Who{Build: 7, Name: "fake/1"}) {
		t.Fatal("the container's agent is not connected")
	}
}

// checkContainer holds a container to what kipp starts: the agent with
// the server's URL, the build's token and nothing else of the server's,
// the labels a server that starts again finds it by, the agent's binary.
func checkContainer(t *testing.T, c *dockertest.Container, url string) {
	t.Helper()
	if got := strs(c.Create["Cmd"]); !slices.Equal(got, []string{"/kipp/agent", "--url", url, "--workdir", "/kipp/workspace"}) {
		t.Fatalf("cmd %v", got)
	}
	if env := strs(c.Create["Env"]); len(env) != 1 || !strings.HasPrefix(env[0], "KIPP_AGENT_TOKEN=kippa_") {
		t.Fatalf("env %v: the token and nothing of the server's", env)
	}
	if c.Labels[LabelServer] != "http://kipp.test" || c.Labels[LabelBuild] != "7" || c.Labels[LabelJob] != "linux-tests" || c.Create["WorkingDir"] != "/kipp/workspace" {
		t.Fatalf("%+v %v", c.Labels, c.Create["WorkingDir"])
	}
	if mode, data, ok := c.File("/kipp/agent"); !ok || mode != 0o755 || string(data) != "agent for linux/arm64" {
		t.Fatal(mode, string(data), ok)
	}
}

func TestAStageOfTheSameBuildAndImageRunsInTheSameContainer(t *testing.T) {
	f := newFixture(t, 2)
	f.reachable()
	first := f.acquire(build(1), "golang:1.27")
	if again := f.acquire(build(1), "golang:1.27"); again != first {
		t.Fatalf("%s then %s", first, again)
	}
	if other := f.acquire(build(1), "alpine:3.22"); other != "fake/2" {
		t.Fatalf("another image is %s", other)
	}
	if n := len(f.engine.Containers()); n != 2 {
		t.Fatalf("%d containers", n)
	}
	cs := f.pool.Candidates(1, "golang:1.27")
	if !cs[0].Reuse || cs[0].Room != 0 {
		t.Fatalf("%+v", cs[0])
	}
}

func TestAnEnginesRoomIsItsMaxLessWhatItRuns(t *testing.T) {
	f := newFixture(t, 1)
	f.reachable()
	if cs := f.pool.Candidates(1, "golang:1.27"); cs[0].Room != 1 || !cs[0].Free {
		t.Fatalf("%+v", cs[0])
	}
	f.acquire(build(1), "golang:1.27")
	if cs := f.pool.Candidates(2, "golang:1.27"); cs[0].Room != 0 || cs[0].Free || cs[0].Reuse {
		t.Fatalf("%+v", cs[0])
	}
	_, err := f.pool.Acquire(context.Background(), build(2), "fake", "golang:1.27", &bytes.Buffer{})
	if !errors.Is(err, agenthub.ErrNoAgent) {
		t.Fatalf("a full engine took a container: %v", err)
	}
}

func TestTheEndOfABuildRemovesItsContainers(t *testing.T) {
	f := newFixture(t, 2)
	f.reachable()
	f.acquire(build(3), "golang:1.27")
	f.acquire(build(4), "golang:1.27")
	f.pool.EndBuild(context.Background(), 3)
	cs := f.engine.Containers()
	if len(cs) != 1 {
		t.Fatalf("%d containers left", len(cs))
	}
	for _, c := range cs {
		if c.Labels[LabelBuild] != "4" {
			t.Fatalf("the wrong container went: %+v", c.Labels)
		}
	}
	if st := f.pool.Status(); len(st[0].Running) != 1 {
		t.Fatalf("%+v", st)
	}
}

func TestAContainerWhoseAgentNeverConnectsIsRemovedWithWhatItSaid(t *testing.T) {
	f := newFixture(t, 2, func(o *Options) { o.AgentURL = "http://nobody.invalid"; o.ConnectWait = 300 * time.Millisecond })
	f.engine.Logs = "kipp-agent: cannot reach the server\n"
	f.reachable()
	var out bytes.Buffer
	_, err := f.pool.Acquire(context.Background(), build(1), "fake", "golang:1.27", &out)
	if err == nil || !strings.Contains(err.Error(), "did not connect") || !strings.Contains(out.String(), "kipp-agent: cannot reach the server") {
		t.Fatalf("%v: %q", err, out.String())
	}
	if n := len(f.engine.Containers()); n != 0 {
		t.Fatalf("%d containers left", n)
	}
	if cs := f.pool.Candidates(1, "golang:1.27"); cs[0].Room != 2 {
		t.Fatalf("the failed container still counts: %+v", cs[0])
	}
}

func TestAProbeThatHangsLeavesTheEngineUnreachableAndHoldsNothingUp(t *testing.T) {
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	f := newFixture(t, 2, func(o *Options) {
		o.Dial = func(ctx context.Context, _ *store.Engine) (*docker.Client, error) {
			select {
			case <-hang:
			case <-ctx.Done():
			}
			return nil, errors.New("dial tcp: i/o timeout")
		}
	})
	t0 := time.Now()
	cs := f.pool.Candidates(1, "golang:1.27")
	if time.Since(t0) > 100*time.Millisecond || cs[0].Reachable || cs[0].Why != "not asked yet" {
		t.Fatalf("%+v after %v", cs[0], time.Since(t0))
	}
	if _, why, line := placement.Pick(placement.Want{Labels: []string{"linux"}, Image: "golang:1.27"}, cs); why != placement.WhyUnreachable || !strings.Contains(line, "waiting for engine fake") {
		t.Fatal(why, line)
	}
}

func TestAnEngineThatStopsAnsweringIsSaidToSinceWhenAndWhy(t *testing.T) {
	var fail sync.Mutex
	failing := false
	f := newFixture(t, 2, func(o *Options) {
		o.Probe = 20 * time.Millisecond
		o.Dial = func(ctx context.Context, e *store.Engine) (*docker.Client, error) {
			fail.Lock()
			defer fail.Unlock()
			if failing {
				return nil, errors.New("dial unix: connection refused")
			}
			return docker.Dial(ctx, e.URL, docker.Options{})
		}
	})
	f.reachable()
	name := f.acquire(build(1), "golang:1.27")
	fail.Lock()
	failing = true
	fail.Unlock()
	f.pool.mu.Lock()
	for _, en := range f.pool.engines {
		en.closeClient()
	}
	f.pool.mu.Unlock()
	eventually(t, "the engine is unreachable and its container given up", func() bool {
		st := f.pool.Status()
		return !st[0].Reachable && strings.Contains(st[0].LastError, "connection refused") && len(st[0].Running) == 0
	})
	if cs := f.pool.Candidates(2, ""); !strings.HasPrefix(cs[0].Why, "unreachable since ") {
		t.Fatal(cs[0].Why)
	}
	// The container is forgotten under the pool's lock and its agent is
	// revoked a moment after, by the goroutine that removes it.
	eventually(t, "the agent of the given-up container is forgotten", func() bool {
		for _, a := range f.hub.Agents() {
			if a.Name == name {
				return false
			}
		}
		return true
	})
}

// A server that starts again removes what a server of its name left on
// its engines, and nothing of another server's.
func TestTheContainersAServerLeftAreRemovedAndNoOneElses(t *testing.T) {
	f := newFixture(t, 2)
	left := f.engine.Add(map[string]string{LabelServer: "http://kipp.test", LabelBuild: "41"}, true)
	theirs := f.engine.Add(map[string]string{LabelServer: "http://other.test", LabelBuild: "41"}, true)
	f.reachable()
	f.acquire(build(42), "golang:1.27")
	f.pool.Wake()
	eventually(t, "the left container is gone", func() bool {
		_, ok := f.engine.Containers()[left]
		return !ok
	})
	cs := f.engine.Containers()
	if _, ok := cs[theirs]; !ok {
		t.Fatal("another server's container was removed")
	}
	if len(cs) != 2 {
		t.Fatalf("%d containers: the running build's must stay", len(cs))
	}
}

func TestAnEnginesURLIsShownWithoutAPassword(t *testing.T) {
	for in, want := range map[string]string{
		"ssh://me:pw@host?socket=/s":  "ssh://me@host?socket=/s",
		"ssh://me@host":               "ssh://me@host",
		"unix:///var/run/docker.sock": "unix:///var/run/docker.sock",
	} {
		if got := Redact(in); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
}

// killAgents stops the agents of every container, as an agent killed for
// its memory would.
func (f *fixture) killAgents() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, cancel := range f.killed {
		cancel()
	}
}

// Two stages of a build that may run at once in one image share the one
// container the first of them makes.
func TestParallelStagesOfABuildAndImageMakeOneContainer(t *testing.T) {
	f := newFixture(t, 2)
	f.reachable()
	names := make([]string, 2)
	var wg sync.WaitGroup
	for i := range names {
		wg.Go(func() { names[i] = f.acquire(build(1), "golang:1.27") })
	}
	wg.Wait()
	if names[0] != names[1] {
		t.Fatalf("two containers: %v", names)
	}
	if n := len(f.engine.Containers()); n != 1 {
		t.Fatalf("%d containers", n)
	}
	if cs := f.pool.Candidates(2, "golang:1.27"); cs[0].Room != 1 {
		t.Fatalf("%+v", cs[0])
	}
}

// A container whose agent died is not the build's container any more:
// the next stage gets a new one, and the dead one is removed.
func TestAContainerWhoseAgentIsGoneIsReplaced(t *testing.T) {
	f := newFixture(t, 1)
	f.reachable()
	first := f.acquire(build(1), "golang:1.27")
	f.killAgents()
	eventually(t, "the engine has room again", func() bool {
		cs := f.pool.Candidates(1, "golang:1.27")
		return !cs[0].Reuse && cs[0].Free
	})
	if second := f.acquire(build(1), "golang:1.27"); second == first {
		t.Fatalf("the dead container %s was given again", first)
	}
	eventually(t, "the dead container is removed", func() bool { return len(f.engine.Containers()) == 1 })
}

// An engine says which version of Docker it runs once a probe has asked.
func TestAnEnginesStatusSaysTheVersionItAnsweredWith(t *testing.T) {
	f := newFixture(t, 1)
	f.reachable()
	if st := f.pool.Status(); st[0].Version != "29.0.0" {
		t.Fatalf("%+v", st[0])
	}
}
