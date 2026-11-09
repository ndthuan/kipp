package runnerhub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/apispec/apispectest"
	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/problem"
	"github.com/ndthuan/kipp/internal/runner"
	"github.com/ndthuan/kipp/internal/runnerd"
	"github.com/ndthuan/kipp/internal/waitfor"
)

const boxToken = "kippa_box-token-for-tests"

var quiet = slog.New(slog.DiscardHandler)

// fixture is a hub behind an httptest server, and standing runners it
// knows by their tokens.
type fixture struct {
	t       *testing.T
	hub     *Hub
	srv     *httptest.Server
	handler atomic.Pointer[http.Handler]
	mu      sync.Mutex
	known   map[string]*Identity // by token hash
	// clock is the hub's time, which stands still until a test moves it: a
	// loaded machine delaying a runner's heartbeat then cannot make the hub
	// think it silent.
	clock atomic.Int64
}

func (f *fixture) now() time.Time { return time.Unix(0, f.clock.Load()) }

// advance moves the hub's time on by d.
func (f *fixture) advance(d time.Duration) { f.clock.Add(int64(d)) }

func newFixture(t *testing.T, opts ...func(*Options)) *fixture {
	t.Helper()
	f := &fixture{t: t, known: map[string]*Identity{
		auth.HashToken(boxToken): {Name: "box", Labels: []string{"linux"}, Executors: 2, Enabled: true},
	}}
	f.clock.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		(*f.handler.Load()).ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	f.hub = f.newHub(opts...)
	return f
}

// newHub puts a new hub behind the server, as a server that started again
// would be.
func (f *fixture) newHub(opts ...func(*Options)) *Hub {
	o := Options{Version: "v-test", Log: quiet, Now: f.now, Heartbeat: 100 * time.Millisecond, ClaimWait: 200 * time.Millisecond,
		Standing: func(_ context.Context, hash string) (*Identity, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if id := f.known[hash]; id != nil {
				c := *id
				return &c, nil
			}
			return nil, errors.New("unknown")
		}}
	for _, fn := range opts {
		fn(&o)
	}
	h := New(o)
	ctx, cancel := context.WithCancel(context.Background())
	f.t.Cleanup(cancel)
	go h.Run(ctx)
	f.handler.Store(new(apispectest.Wrap(f.t, h.Handler())))
	return h
}

// runRunner starts the standing runner box; the returned func stops it and
// gives what Run returned.
func (f *fixture) runRunner(workdir string) func() error {
	stop, _ := f.runRunnerEnded(boxToken, workdir)
	return stop
}

// runRunnerEnded is runRunner, and a channel closed when Run returns by
// itself.
func (f *fixture) runRunnerEnded(token, workdir string) (func() error, <-chan struct{}) {
	c := runnerd.Config{URL: f.srv.URL, Token: token, Workdir: workdir, Version: "v-test", Env: []string{"PATH=" + os.Getenv("PATH")}, Log: quiet, Retry: 20 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	ended := make(chan struct{})
	var err error
	go func() { err = runnerd.Run(ctx, c); close(ended) }()
	stop := func() error {
		cancel()
		<-ended
		return err
	}
	f.t.Cleanup(func() { _ = stop() })
	return stop, ended
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	waitfor.Until(t, what, ok)
}

// online waits until the standing runner box is online.
func (f *fixture) online() {
	f.t.Helper()
	f.onlineWho(Who{Name: "box"})
}

func (f *fixture) onlineWho(who Who) {
	f.t.Helper()
	eventually(f.t, who.Name+" is online", func() bool { return f.hub.Free(who) })
}

// syncBuffer is a log the hub writes and the test reads.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// start gives a stage to the standing runner box.
func (f *fixture) start(ctx context.Context, command string, meta StageMeta) (runner.Proc, *syncBuffer) {
	f.t.Helper()
	return f.startWho(ctx, Who{Name: "box"}, command, meta)
}

func (f *fixture) startWho(ctx context.Context, who Who, command string, meta StageMeta) (runner.Proc, *syncBuffer) {
	f.t.Helper()
	out := &syncBuffer{}
	if meta.Job == "" {
		meta = StageMeta{BuildID: 1, Job: "j", Number: 1, Stage: "s", Workspace: "j/1"}
	}
	p, err := f.hub.Start(ctx, who, runner.Spec{Command: command, Env: []string{"FROM_SERVER=yes"}, Output: out}, meta)
	if err != nil {
		f.t.Fatal(err)
	}
	return p, out
}

func TestAnRunnerThatConnectsIsOnlineWithWhatItSaysOfItself(t *testing.T) {
	var seen atomic.Value
	f := newFixture(t, func(o *Options) {
		o.Seen = func(_ context.Context, name, os, arch, version string, _ time.Time) { seen.Store(name + " " + version) }
	})
	f.runRunner(t.TempDir())
	f.online()
	got := f.hub.Runners()
	if len(got) != 1 || got[0].Name != "box" || got[0].Labels[0] != "linux" || got[0].OS == "" || got[0].Arch == "" || got[0].Version != "v-test" || !got[0].Online {
		t.Fatalf("%+v", got)
	}
	if seen.Load() != "box v-test" {
		t.Fatalf("seen %v", seen.Load())
	}
}

func TestAStageOnAnRunnerStreamsItsOutputBeforeItEnds(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	f.runRunner(dir)
	f.online()
	p, out := f.start(context.Background(), `echo "one $FROM_SERVER $KIPP_RUNNER"; pwd; sleep 2; echo two`, StageMeta{})
	eventually(t, "the first line is in the log", func() bool { return strings.Contains(out.String(), "one yes box") })
	if strings.Contains(out.String(), "two") {
		t.Fatal("the stage ended before its first line arrived")
	}
	if code, err := p.Wait(); code != 0 || err != nil {
		t.Fatalf("code %d err %v: %s", code, err, out.String())
	}
	if want := filepath.Join(dir, "j", "1") + "\n"; !strings.Contains(out.String(), want) || !strings.HasSuffix(out.String(), "two\n") {
		t.Fatalf("log %q, want the workspace %q", out.String(), want)
	}
}

func TestAStageEndsWithTheExitCodeOfItsCommand(t *testing.T) {
	f := newFixture(t)
	f.runRunner(t.TempDir())
	f.online()
	p, _ := f.start(context.Background(), "exit 3", StageMeta{})
	if code, err := p.Wait(); code != 3 || err != nil {
		t.Fatalf("code %d err %v", code, err)
	}
}

func TestCancellingAStageOnAnRunnerKillsItThere(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	f.runRunner(dir)
	f.online()
	ctx, cancel := context.WithCancel(context.Background())
	p, out := f.start(ctx, "echo started; sleep 60 & echo $! > pid; wait", StageMeta{})
	eventually(t, "the stage started", func() bool { return strings.Contains(out.String(), "started") })
	t0 := time.Now()
	cancel()
	code, _ := p.Wait()
	if code == 0 || time.Since(t0) > waitfor.Safety {
		t.Fatalf("code %d after %v", code, time.Since(t0))
	}
}

func TestAStageNotClaimedYetNeverStartsOnceKilled(t *testing.T) {
	f := newFixture(t)
	// A runner that connected and has not claimed anything yet.
	req, err := http.NewRequest(http.MethodPost, f.srv.URL+"/api/v1/runner/connect", strings.NewReader(`{"Version":"v-test"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+boxToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatal(resp, err)
	}
	_ = resp.Body.Close()
	p, out := f.start(context.Background(), "echo ran", StageMeta{})
	p.Kill(0)
	if code, _ := p.Wait(); code != -1 || out.String() != "" {
		t.Fatalf("code %d log %q", code, out.String())
	}
	if !f.hub.Free(Who{Name: "box"}) {
		t.Fatal("the killed stage still holds an executor")
	}
}

func TestAnRunnerThatStopsCallingGoesOfflineAndFailsItsStage(t *testing.T) {
	f := newFixture(t)
	stop := f.runRunner(t.TempDir())
	f.online()
	p, out := f.start(context.Background(), "echo started; sleep 30", StageMeta{})
	eventually(t, "the stage started", func() bool { return strings.Contains(out.String(), "started") })
	f.handler.Store(new(http.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))))
	f.advance((Missed + 1) * 100 * time.Millisecond)
	code, err := p.Wait()
	if code != -1 || !errors.Is(err, ErrOffline) {
		t.Fatalf("code %d err %v", code, err)
	}
	if f.hub.Free(Who{Name: "box"}) || f.hub.Runners()[0].Online {
		t.Fatal("still online")
	}
	_ = stop()
}

func TestAnRunnerOfAnotherVersionIsRefused(t *testing.T) {
	f := newFixture(t)
	err := runnerd.Run(context.Background(), runnerd.Config{URL: f.srv.URL, Token: boxToken, Workdir: t.TempDir(), Version: "v-other", Log: quiet})
	ve, ok := errors.AsType[*runnerd.VersionError](err)
	if !ok || ve.Runner != "v-other" || ve.Server != "v-test" || !strings.Contains(err.Error(), "kipp v-other") || !strings.Contains(err.Error(), "kipp v-test") {
		t.Fatalf("%v", err)
	}
}

func TestAWrongTokenIsRefused(t *testing.T) {
	f := newFixture(t)
	for _, token := range []string{"kippa_not-a-token", "kipp_an-api-token"} {
		ended := make(chan error, 1)
		go func() {
			ended <- runnerd.Run(context.Background(), runnerd.Config{URL: f.srv.URL, Token: token, Workdir: t.TempDir(), Version: "v-test", Log: quiet})
		}()
		select {
		case err := <-ended:
			if !errors.Is(err, runnerd.ErrTokenRefused) {
				t.Fatalf("%s: %v", token, err)
			}
		case <-time.After(waitfor.Safety):
			t.Fatalf("%s: the runner kept trying", token)
		}
	}
}

// The token of a runner kipp started for one build is that runner's alone:
// it claims the stages given to it and none other, and dies with the build.
func TestABuildsTokenClaimsOnlyItsOwnStagesAndEndsWithTheBuild(t *testing.T) {
	f := newFixture(t)
	f.runRunner(t.TempDir())
	token, err := f.hub.Grant(7, "mac/1")
	if err != nil {
		t.Fatal(err)
	}
	stop, ended := f.runRunnerEnded(token, t.TempDir())
	f.online()
	f.onlineWho(Who{Build: 7, Name: "mac/1"})
	pBox, outBox := f.start(context.Background(), "echo $KIPP_RUNNER", StageMeta{})
	pMac, outMac := f.startWho(context.Background(), Who{Build: 7, Name: "mac/1"}, "echo $KIPP_RUNNER", StageMeta{BuildID: 7, Job: "j", Number: 2, Stage: "s"})
	for _, p := range []runner.Proc{pBox, pMac} {
		if code, err := p.Wait(); code != 0 || err != nil {
			t.Fatal(code, err)
		}
	}
	if outBox.String() != "box\n" || outMac.String() != "mac/1\n" {
		t.Fatalf("%q %q", outBox.String(), outMac.String())
	}
	f.hub.EndBuild(7)
	select {
	case <-ended:
	case <-time.After(waitfor.Safety):
		t.Fatal("the runner of an ended build kept running")
	}
	if err := stop(); !errors.Is(err, runnerd.ErrTokenRefused) {
		t.Fatalf("the runner of an ended build kept its token: %v", err)
	}
	for _, a := range f.hub.Runners() {
		if a.Name == "mac/1" {
			t.Fatal("the runner of an ended build is still listed")
		}
	}
}

// A server that started again knows none of what its runners were running:
// they kill it at their next connect, and remove its workspace.
func TestAnRunnerKillsWhatTheServerNoLongerKnows(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	f.runRunner(dir)
	f.online()
	_, out := f.start(context.Background(), "echo started; sleep 60", StageMeta{BuildID: 3, Job: "j", Number: 3, Stage: "s", Workspace: "j/3"})
	eventually(t, "the stage started", func() bool { return strings.Contains(out.String(), "started") })
	f.hub = f.newHub()
	eventually(t, "the workspace of the lost build is gone", func() bool {
		_, err := os.Stat(filepath.Join(dir, "j", "3"))
		return errors.Is(err, os.ErrNotExist)
	})
	f.online()
}

// A server that started again has no connection for a runner whose token
// it still honours: the runner's claim, made before its next heartbeat,
// is told to connect again, never that its token is refused, which would
// end the runner for good.
func TestAClaimBeforeTheFirstConnectToThisServerIsToldToConnect(t *testing.T) {
	f := newFixture(t)
	f.call(http.MethodPost, "/api/v1/runner/connect", `{"Version":"v-test"}`)
	f.hub = f.newHub()
	req, err := http.NewRequest(http.MethodGet, f.srv.URL+"/api/v1/runner/claim", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+boxToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("answered %d", resp.StatusCode)
	}
}

func TestAnRunnerThatStartsAgainFailsWhatItRan(t *testing.T) {
	f := newFixture(t)
	stop := f.runRunner(t.TempDir())
	f.online()
	p, out := f.start(context.Background(), "echo started; sleep 60", StageMeta{})
	eventually(t, "the stage started", func() bool { return strings.Contains(out.String(), "started") })
	_ = stop()
	f.runRunner(t.TempDir())
	code, err := p.Wait()
	// Stopping the runner kills the stage, and the runner may report that
	// before the new one connects: the stage then ends -1 with no error,
	// or with the log that did not get through. What holds is that it failed.
	if code != -1 {
		t.Fatalf("code %d err %v", code, err)
	}
}

func TestAnEndedBuildsWorkspaceIsRemovedOnTheStandingRunner(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	f.runRunner(dir)
	f.online()
	p, _ := f.start(context.Background(), "echo x > file", StageMeta{BuildID: 5, Job: "j", Number: 5, Stage: "s", Workspace: "j/5"})
	if code, err := p.Wait(); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "j", "5", "file")); err != nil {
		t.Fatal(err)
	}
	f.hub.EndBuild(5)
	eventually(t, "the workspace is gone", func() bool {
		_, err := os.Stat(filepath.Join(dir, "j", "5"))
		return errors.Is(err, os.ErrNotExist)
	})
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("the workdir itself went")
	}
}

func TestTheJunitFilesAStageLeftOnTheRunnerReachTheServer(t *testing.T) {
	f := newFixture(t)
	f.runRunner(t.TempDir())
	f.online()
	into := filepath.Join(t.TempDir(), "junit")
	p, out := f.start(context.Background(), "mkdir -p reports && echo '<testsuite/>' > reports/a.xml && echo no > reports/b.txt",
		StageMeta{BuildID: 1, Job: "j", Number: 1, Stage: "s", Workspace: "j/1", Workdir: "src", Junit: "reports/*.xml", JunitDir: into})
	if code, err := p.Wait(); code != 0 || err != nil {
		t.Fatal(code, err, out.String())
	}
	got, err := os.ReadFile(filepath.Join(into, "reports", "a.xml"))
	if err != nil || string(got) != "<testsuite/>\n" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(into, "reports", "b.txt")); err == nil {
		t.Fatal("a file the glob does not match came back")
	}
}

// A disabled runner takes nothing new: what it runs finishes.
func TestADisabledRunnerIsNotFreeAndFinishesWhatItRuns(t *testing.T) {
	f := newFixture(t)
	f.runRunner(t.TempDir())
	f.online()
	p, out := f.start(context.Background(), "echo started; sleep 1; echo done", StageMeta{})
	eventually(t, "the stage started", func() bool { return strings.Contains(out.String(), "started") })
	f.hub.SetStanding([]Identity{{Name: "box", Labels: []string{"linux"}, Executors: 2, Enabled: false}})
	if f.hub.Free(Who{Name: "box"}) {
		t.Fatal("a disabled runner is free")
	}
	if code, err := p.Wait(); code != 0 || err != nil || !strings.HasSuffix(out.String(), "done\n") {
		t.Fatal(code, err, out.String())
	}
}

func TestAnRunnerIsBusyUpToItsExecutors(t *testing.T) {
	f := newFixture(t)
	f.runRunner(t.TempDir())
	f.online()
	ctx := t.Context()
	f.start(ctx, "sleep 30", StageMeta{})
	if !f.hub.Free(Who{Name: "box"}) {
		t.Fatal("one of two executors busy and not free")
	}
	f.start(ctx, "sleep 30", StageMeta{})
	if f.hub.Free(Who{Name: "box"}) {
		t.Fatal("both executors busy and free")
	}
}

func TestAStageCannotBeGivenToAnRunnerThatIsNotOnline(t *testing.T) {
	f := newFixture(t)
	if _, err := f.hub.Start(context.Background(), Who{Name: "box"}, runner.Spec{Command: "true", Output: &syncBuffer{}}, StageMeta{}); !errors.Is(err, ErrNoRunner) {
		t.Fatal(err)
	}
}

// physicalTempDir is t.TempDir() as pwd prints it: on macOS the temp dir's
// /var is a link to /private/var.
func physicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// Two builds' containers on one engine have the same name, the engine's
// and their number in the build, and are still two runners: each claims
// only its own build's stages, and one build ending leaves the other's.
func TestContainersOfTwoBuildsWithOneNameAreTwoRunners(t *testing.T) {
	f := newFixture(t)
	dirs := map[int64]string{7: physicalTempDir(t), 8: physicalTempDir(t)}
	for _, build := range []int64{7, 8} {
		token, err := f.hub.Grant(build, "mac/1")
		if err != nil {
			t.Fatal(err)
		}
		f.runRunnerEnded(token, dirs[build])
		f.onlineWho(Who{Build: build, Name: "mac/1"})
	}
	var procs []runner.Proc
	var outs []*syncBuffer
	for n := range 6 {
		p, out := f.startWho(context.Background(), Who{Build: 7, Name: "mac/1"}, "pwd", StageMeta{BuildID: 7, Job: "j", Number: 1, Stage: fmt.Sprintf("s%d", n)})
		procs, outs = append(procs, p), append(outs, out)
	}
	for i, p := range procs {
		if code, err := p.Wait(); code != 0 || err != nil {
			t.Fatal(code, err)
		}
		if got := strings.TrimSpace(outs[i].String()); got != dirs[7] {
			t.Fatalf("a stage of build 7 ran in %s, not in build 7's container %s", got, dirs[7])
		}
	}
	f.hub.EndBuild(7)
	p, out := f.startWho(context.Background(), Who{Build: 8, Name: "mac/1"}, "pwd", StageMeta{BuildID: 8, Job: "j", Number: 2, Stage: "s"})
	if code, err := p.Wait(); code != 0 || err != nil || strings.TrimSpace(out.String()) != dirs[8] {
		t.Fatalf("build 8's container after build 7 ended: %d %v %q", code, err, out.String())
	}
	f.hub.Revoke(Who{Build: 8, Name: "mac/1"}, errors.New("gone"))
	if f.hub.Free(Who{Build: 8, Name: "mac/1"}) {
		t.Fatal("a revoked container's runner is still free")
	}
}

// call makes one call under /runner/ as box.
func (f *fixture) call(method, path, body string) {
	f.t.Helper()
	req, err := http.NewRequest(method, f.srv.URL+path, strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+boxToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	_ = resp.Body.Close()
}

// A stage whose claim never reached the runner is one the runner does not
// say it runs: it ends once a heartbeat has had time to list it, rather
// than holding an executor until the job's timeout.
func TestAStageTheRunnerNeverGotEndsAtItsHeartbeats(t *testing.T) {
	f := newFixture(t)
	f.call(http.MethodPost, "/api/v1/runner/connect", `{"Version":"v-test"}`)
	p, _ := f.start(context.Background(), "echo ran", StageMeta{})
	f.call(http.MethodGet, "/api/v1/runner/claim", "") // the answer is lost on the way
	// A heartbeat later the runner lists nothing it runs, which a stage claimed
	// that long ago should be among.
	f.advance(2 * 100 * time.Millisecond)
	f.call(http.MethodPost, "/api/v1/runner/connect", `{"Version":"v-test","Running":[]}`)
	if code, err := p.Wait(); code != -1 || !errors.Is(err, ErrLost) {
		t.Fatalf("code %d err %v", code, err)
	}
	if !f.hub.Free(Who{Name: "box"}) {
		t.Fatal("the lost stage still holds an executor")
	}
}

func TestTheRunnerRoutesAreUnderAPIV1(t *testing.T) {
	f := newFixture(t)
	rows := f.hub.Routes()
	if len(rows) != 7 {
		t.Fatalf("%d routes", len(rows))
	}
	for _, row := range rows {
		if !strings.HasPrefix(row.Path, "/api/v1/runner/") {
			t.Errorf("route %s", row.Pattern())
		}
	}
}

func TestAPathOrMethodTheRunnerProtocolHasNoneForIsAProblemNotARefusal(t *testing.T) {
	f := newFixture(t)
	for _, c := range []struct {
		method, path, code, allow string
		status                    int
	}{
		{"GET", "/api/v1/runner/nothing", "not_found", "", 404},
		{"GET", "/api/v1/runner/connect", "method_not_allowed", "POST", 405},
	} {
		rec := httptest.NewRecorder()
		f.hub.Handler().ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		var d problem.Doc
		if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil || rec.Code != c.status || d.Code != c.code ||
			rec.Header().Get("Content-Type") != problem.MediaType || rec.Header().Get("Allow") != c.allow {
			t.Errorf("%s %s: %d %v %s", c.method, c.path, rec.Code, rec.Header(), rec.Body)
		}
	}
	// A runner's refusal is still the protocol's own.
	rec := httptest.NewRecorder()
	f.hub.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/runner/connect", nil))
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), `"Error"`) {
		t.Errorf("a missing token: %d %s", rec.Code, rec.Body)
	}
}

func TestAnRunnersStatusNamesEachStageItRunsByItsBuild(t *testing.T) {
	f := newFixture(t)
	f.runRunner(t.TempDir())
	f.online()
	p, _ := f.start(context.Background(), "sleep 2", StageMeta{BuildID: 7, Job: "site", Number: 3, Stage: "lint", Workspace: "site/3"})
	defer p.Kill(time.Second)
	eventually(t, "the stage is on the runner", func() bool {
		got := f.hub.Runners()
		return len(got) == 1 && got[0].Busy == 1
	})
	got := f.hub.Runners()[0]
	if want := []StageRef{{BuildID: 7, Job: "site", Number: 3, Stage: "lint"}}; !slices.Equal(got.Running, want) || !slices.Equal(got.Stages, []string{"site#3/lint"}) {
		t.Fatalf("%+v", got)
	}
}
