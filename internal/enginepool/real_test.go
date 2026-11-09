//go:build dockerreal

package enginepool

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/runner"
	"github.com/ndthuan/kipp/internal/runnerbin"
	"github.com/ndthuan/kipp/internal/runnerhub"
	"github.com/ndthuan/kipp/internal/runnerproto"
	"github.com/ndthuan/kipp/internal/store"
	"github.com/ndthuan/kipp/internal/version"
	"github.com/ndthuan/kipp/internal/waitfor"
)

// TestARealContainerRunsAStageThroughItsRunner is the whole path on the
// engine KIPP_TEST_DOCKER names: the image pulled, the embedded runner put
// in, the runner dialling this test's hub at KIPP_TEST_RUNNER_HOST (an
// address of this machine a container there reaches), a stage run and its
// output streamed back, and the container removed with the build.
func TestARealContainerRunsAStageThroughItsRunner(t *testing.T) {
	r := realEngine(t)
	// KIPP_BIN is the kipp-runner kipp put in, and KIPP_METRICS the file it
	// wrote of what the server sent.
	command := `echo "in $(uname -s) as $KIPP_RUNNER at $KIPP_WORKSPACE"; test -x "$KIPP_BIN" && echo "bin $(basename "$KIPP_BIN")"; cat "$KIPP_METRICS"`
	out, code := r.run(t, command, []byte(`{"store":"real"}`+"\n"))
	if code != 0 {
		t.Fatalf("%d: %s", code, out)
	}
	if want := "in Linux as real/1 at /kipp/workspace\nbin runner\n{\"store\":\"real\"}\n"; out != want {
		t.Fatalf("%q, want %q", out, want)
	}
	r.end(t)
}

// TestKippRunnerRunsMetricsCheckInAContainerStage is "$KIPP_BIN" metrics
// check inside a container: the kipp-runner there judges the guards of the
// KIPP_METRICS the server sent, offline.
func TestKippRunnerRunsMetricsCheckInAContainerStage(t *testing.T) {
	r := realEngine(t)
	baseline := []byte(`{"store":"site","metrics":[{"name":"coverage","unit":"%","values":{"total":87.19},` +
		`"from":{"total":{"job":"site-deploy","number":123,"at":"2026-10-29T03:00:00Z"}}}],` +
		`"guards":[{"metric":"coverage","item":"total","no_lower":true,"tolerance":0.01,"on_fall":"failed","required":true}]}` + "\n")
	command := `echo '{"metrics":[{"name":"coverage","values":{"total":86.90}}]}' > m.json; "$KIPP_BIN" metrics check m.json; echo "exit $?"`
	out, code := r.run(t, command, baseline)
	want := "kipp: metrics guard: coverage total 87.19 -> 86.90 (-0.29), remembered from site-deploy #123, tolerance 0.01: failed\nexit 1\n"
	if code != 0 || out != want {
		t.Fatalf("%d %q, want %q", code, out, want)
	}
	r.end(t)
}

// TestAContainerStageFindsTheSameInputsAtTheSamePath is artifacts_from in
// a container: the runner there fetches what the stage is given from the
// server and puts it under $KIPP_WORKSPACE/.kipp/artifacts/<stage>/, as
// the server does for a stage of its own.
func TestAContainerStageFindsTheSameInputsAtTheSamePath(t *testing.T) {
	r := realEngine(t)
	kept := t.TempDir()
	if err := os.MkdirAll(filepath.Join(kept, "dist", "css"), 0o750); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"dist/app.js": "app\n", "dist/css/site.css": "css\n"} {
		if err := os.WriteFile(filepath.Join(kept, filepath.FromSlash(name)), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	meta := runnerhub.StageMeta{BuildID: 1, Job: "real", Number: 1, Stage: "deploy",
		Inputs:    []runnerproto.Input{{Stage: "build", Size: 8}, {Stage: "empty"}},
		InputDirs: map[string]string{"build": kept, "empty": filepath.Join(kept, "none")}}
	command := `cd "$KIPP_WORKSPACE/.kipp/artifacts" && echo "app=$(cat build/dist/app.js) css=$(cat build/dist/css/site.css) empty=[$(ls -A empty)]"`
	out, code := r.runMeta(t, command, meta)
	if want := "app=app css=css empty=[]\n"; code != 0 || out != want {
		t.Fatalf("%d %q, want %q", code, out, want)
	}
	r.end(t)
}

// TestAFileSecretInAContainerStageIsAPrivateFileOutsideTheWorkspaceAndGoneAfter
// is a file secret in a container: the runner there writes it, of the exact
// bytes, outside $KIPP_WORKSPACE, and removes it when the stage ends.
func TestAFileSecretInAContainerStageIsAPrivateFileOutsideTheWorkspaceAndGoneAfter(t *testing.T) {
	r := realEngine(t)
	meta := runnerhub.StageMeta{BuildID: 1, Job: "real", Number: 1, Stage: "s",
		SecretFiles: []runnerproto.SecretFile{{Name: "KEYFILE", Data: []byte("\x00\xff key\n")}}}
	command := `case "$KEYFILE" in "$KIPP_WORKSPACE"/*) echo inside;; *) echo outside;; esac; ls -l "$KEYFILE" | cut -c1-10; od -An -tx1 "$KEYFILE" | tr -s ' \n' ' '; echo`
	out, code := r.runMeta(t, command, meta)
	if want := "outside\n-rw-------\n 00 ff 20 6b 65 79 0a \n"; code != 0 || out != want {
		t.Fatalf("%d %q, want %q", code, out, want)
	}
	out, code = r.run(t, `find /tmp/kipp-runner-secrets-* -type f | wc -l`, nil)
	if code != 0 || strings.TrimSpace(out) != "0" {
		t.Fatalf("%d: files are left after the stage: %q", code, out)
	}
	r.end(t)
}

// TestAContainerStagesKippEnvIsEmptyBeforeEveryTryAndItsFileReachesTheServer
// is KIPP_ENV in a container: the runner there makes the file empty before
// each try, so that a try that failed leaves nothing, and sends the file of
// the try that ended with the stage's done call, to be read on the server.
func TestAContainerStagesKippEnvIsEmptyBeforeEveryTryAndItsFileReachesTheServer(t *testing.T) {
	r := realEngine(t)
	dir := t.TempDir()
	meta := runnerhub.StageMeta{BuildID: 1, Job: "real", Number: 1, Stage: "s", JunitDir: dir, Retries: 1}
	command := `test -f "$KIPP_ENV" && test ! -s "$KIPP_ENV" && echo empty
n=$(cat runs 2>/dev/null || echo 0); n=$((n+1)); echo $n > runs
if [ $n -lt 2 ]; then echo OLD=1 >> "$KIPP_ENV"; exit 7; fi
printf 'NEW<<E\nline one\nline two\nE\n' >> "$KIPP_ENV"`
	out, code := r.runMeta(t, command, meta)
	if want := "empty\nkipp: try 2 of 2\nempty\n"; code != 0 || out != want {
		t.Fatalf("%d %q, want %q", code, out, want)
	}
	got, err := os.ReadFile(filepath.Clean(filepath.Join(dir, filepath.FromSlash(runnerproto.EnvTarName))))
	if want := "NEW<<E\nline one\nline two\nE\n"; err != nil || string(got) != want {
		t.Fatalf("%v: the file the server got is %q, want %q", err, got, want)
	}
	r.end(t)
}

// TestAContainerStageIsGivenItsOwnTmpOsArchAndTryNumber is KIPP_TMP,
// KIPP_OS, KIPP_ARCH and KIPP_ATTEMPT in a container: the runner there makes
// the directory, empty and outside the workspace, one for the stage and not
// for each try, removes it when the stage ends, and gives its own system,
// architecture and try.
func TestAContainerStageIsGivenItsOwnTmpOsArchAndTryNumber(t *testing.T) {
	r := realEngine(t)
	meta := runnerhub.StageMeta{BuildID: 1, Job: "real", Number: 1, Stage: "s", Retries: 1}
	command := `echo "os=$KIPP_OS arch=$KIPP_ARCH attempt=$KIPP_ATTEMPT"
case "$KIPP_TMP" in "$KIPP_WORKSPACE"/*) echo inside;; *) echo outside;; esac
test -d "$KIPP_TMP" && if test -z "$(ls -A "$KIPP_TMP")"; then echo empty; else echo kept; fi
touch "$KIPP_TMP/x"
n=$(cat runs 2>/dev/null || echo 0); n=$((n+1)); echo $n > runs
if [ $n -lt 2 ]; then exit 7; fi`
	out, code := r.runMeta(t, command, meta)
	arch := "amd64"
	if strings.Contains(out, "arch=arm64") {
		arch = "arm64"
	}
	line := "os=linux arch=" + arch + " attempt="
	if want := line + "1\noutside\nempty\nkipp: try 2 of 2\n" + line + "2\noutside\nkept\n"; code != 0 || out != want {
		t.Fatalf("%d %q, want %q", code, out, want)
	}
	out, code = r.run(t, `find /tmp/kipp-runner-tmp-* -mindepth 1 ! -path "$KIPP_TMP" | wc -l`, nil)
	if code != 0 || strings.TrimSpace(out) != "0" {
		t.Fatalf("%d: directories are left after the stage: %q", code, out)
	}
	r.end(t)
}

// onEngine is a hub and a pool on the engine KIPP_TEST_DOCKER names, with the
// container of build 1 of the job real.
type onEngine struct {
	ctx  context.Context
	hub  *runnerhub.Hub
	pool *Pool
	b    *store.Build
	name string
}

// realEngine is the whole path on the engine KIPP_TEST_DOCKER names: the
// image pulled, the embedded runner put in, the runner dialling this test's
// hub at KIPP_TEST_RUNNER_HOST (an address of this machine a container
// there reaches).
func realEngine(t *testing.T) *onEngine {
	t.Helper()
	url := os.Getenv("KIPP_TEST_DOCKER")
	if url == "" {
		t.Skip("KIPP_TEST_DOCKER names no engine")
	}
	host := os.Getenv("KIPP_TEST_RUNNER_HOST")
	if host == "" {
		t.Fatal("KIPP_TEST_RUNNER_HOST: the address a container on that engine reaches this machine at")
	}
	image := os.Getenv("KIPP_TEST_DOCKER_IMAGE")
	if image == "" {
		image = "alpine:3.22"
	}
	hub := runnerhub.New(runnerhub.Options{Version: version.Version, Log: quiet, Heartbeat: time.Second})
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: hub.Handler(), ReadHeaderTimeout: waitfor.Safety}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	go hub.Run(ctx)
	runnerURL := "http://" + net.JoinHostPort(host, strconv.Itoa(ln.Addr().(*net.TCPAddr).Port))
	pool := New(Options{Hub: hub, RunnerURL: runnerURL, ServerID: "kipp-real-test-" + time.Now().Format("150405.000"), Binary: runnerbin.For, Log: quiet, SSHKey: os.Getenv("KIPP_TEST_DOCKER_SSH_KEY")})
	go pool.Run(ctx)
	pool.SetEngines([]*store.Engine{{Name: "real", Kind: "docker", URL: url, Labels: []string{"linux"}, Max: 1, Enabled: true}})
	eventually(t, "the engine is reachable", func() bool { cs := pool.Candidates(0, ""); return len(cs) == 1 && cs[0].Reachable })

	b := &store.Build{ID: 1, Job: "real", Number: 1}
	var log bytes.Buffer
	name, err := pool.Acquire(ctx, b, "real", image, &log)
	if err != nil {
		t.Fatalf("%v: %s", err, log.String())
	}
	return &onEngine{ctx: ctx, hub: hub, pool: pool, b: b, name: name}
}

// run runs the command as stage s of the build in its container, given
// baseline as its KIPP_METRICS, and is what it printed and its exit code.
func (r *onEngine) run(t *testing.T, command string, baseline []byte) (string, int) {
	t.Helper()
	return r.runMeta(t, command, runnerhub.StageMeta{BuildID: 1, Job: "real", Number: 1, Stage: "s", Baseline: baseline})
}

// runMeta runs the command as the stage meta says in the build's container.
func (r *onEngine) runMeta(t *testing.T, command string, meta runnerhub.StageMeta) (string, int) {
	t.Helper()
	out := &syncBuffer{}
	proc, err := r.hub.Start(r.ctx, runnerhub.Who{Build: r.b.ID, Name: r.name}, runner.Spec{Command: command, Output: out}, meta)
	if err != nil {
		t.Fatal(err)
	}
	code, err := proc.Wait()
	if err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	return out.String(), code
}

// end ends the build, which removes its container.
func (r *onEngine) end(t *testing.T) {
	t.Helper()
	r.pool.EndBuild(r.ctx, 1)
	r.hub.EndBuild(1)
	if st := r.pool.Status(); len(st[0].Running) != 0 {
		t.Fatalf("%+v", st)
	}
}

// syncBuffer is a log the hub writes and a test reads.
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
