package runnerd

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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

	"github.com/ndthuan/kipp/internal/runnerproto"
	"github.com/ndthuan/kipp/internal/waitfor"
)

// The runner's own environment comes first, what the server sends after it
// and the runner's two variables last: a later entry of a name wins.
func TestAStagesEnvironmentIsTheRunnersThenTheServersThenTheRunnersOwnTwo(t *testing.T) {
	a := &runnerproto.Assignment{Env: []string{"HOME=/from/server", "KIPP_JOB=j"}}
	got := stageEnv([]string{"PATH=/runner/bin", "HOME=/runner"}, a, "/w/j/1", "box")
	want := []string{"PATH=/runner/bin", "HOME=/runner", "HOME=/from/server", "KIPP_JOB=j", "KIPP_WORKSPACE=/w/j/1", "KIPP_RUNNER=box"}
	if !slices.Equal(got, want) {
		t.Fatalf("%v", got)
	}
}

func TestTheRunnersTokenIsNotInWhatAStageIsGiven(t *testing.T) {
	t.Setenv(TokenEnv, "kippa_secret")
	t.Setenv("KIPP_TEST_KEPT", "yes")
	c := defaults(Config{})
	if slices.ContainsFunc(c.Env, func(kv string) bool { return strings.HasPrefix(kv, TokenEnv+"=") }) {
		t.Fatal("the token is in the stage's environment")
	}
	if !slices.Contains(c.Env, "KIPP_TEST_KEPT=yes") {
		t.Fatal("the runner's own environment is not passed on")
	}
}

func TestAStagesDirectoryIsInsideTheWorkdir(t *testing.T) {
	dir := t.TempDir()
	ws, sd, err := stageDirs(dir, &runnerproto.Assignment{Workspace: "j/1", Workdir: "src/api"})
	if err != nil || ws != filepath.Join(dir, "j", "1") || sd != filepath.Join(dir, "j", "1", "src", "api") {
		t.Fatal(ws, sd, err)
	}
	if info, err := os.Stat(sd); err != nil || !info.IsDir() {
		t.Fatal(err)
	}
	ws, sd, err = stageDirs(dir, &runnerproto.Assignment{})
	if err != nil || ws != dir || sd != dir {
		t.Fatal(ws, sd, err)
	}
	for _, a := range []runnerproto.Assignment{{Workspace: "../x"}, {Workspace: "/abs"}, {Workdir: "../../x"}} {
		if _, _, err := stageDirs(dir, &a); err == nil {
			t.Fatalf("%+v made", a)
		}
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "out")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := stageDirs(dir, &runnerproto.Assignment{Workspace: "out/j"}); err == nil {
		t.Fatal("a link led the workspace out of the workdir")
	}
}

func TestRemovingABuildsWorkspaceNeverRemovesTheWorkdir(t *testing.T) {
	dir := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(dir, "j", "1"), 0o750))
	for _, ws := range []string{"", ".", "..", "/"} {
		must(t, removeWorkspace(dir, ws))
	}
	if _, err := os.Stat(filepath.Join(dir, "j", "1")); err != nil {
		t.Fatal(err)
	}
	must(t, removeWorkspace(dir, "j/1"))
	if _, err := os.Stat(filepath.Join(dir, "j", "1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestAJunitPatternStaysInsideTheStagesDirectory(t *testing.T) {
	if _, err := reportTar(t.TempDir(), nil, "../*.xml"); err == nil {
		t.Fatal("a pattern out of the directory was read")
	}
	b, err := reportTar(t.TempDir(), nil, "none/*.xml", "")
	if err != nil || b != nil {
		t.Fatal(b, err)
	}
}

func TestTheStagesKippEnvFileGoesInTheReportTarUnderItsOwnName(t *testing.T) {
	ws := t.TempDir()
	path, err := resetEnvFile(ws, "build")
	must(t, err)
	if want := filepath.Join(ws, ".kipp", "env-build"); path != want {
		t.Fatalf("%s, want %s", path, want)
	}
	must(t, os.WriteFile(path, []byte("V=1\n"), 0o600))
	b, err := reportTar(t.TempDir(), readEnvFile(ws, "build"))
	must(t, err)
	tr := tar.NewReader(bytes.NewReader(b))
	hd, err := tr.Next()
	if err != nil || hd.Name != runnerproto.EnvTarName {
		t.Fatalf("%v %+v", err, hd)
	}
	if body, _ := io.ReadAll(tr); string(body) != "V=1\n" {
		t.Fatalf("%q", body)
	}
	if b, err := reportTar(t.TempDir(), readEnvFile(t.TempDir(), "build")); err != nil || b != nil {
		t.Fatalf("a stage with no file and no report sends no tar: %v %v", b, err)
	}
}

func TestTheKippEnvFileIsReadUpToOneByteOverTheLimit(t *testing.T) {
	ws := t.TempDir()
	path, err := resetEnvFile(ws, "build")
	must(t, err)
	must(t, os.WriteFile(path, bytes.Repeat([]byte("a"), runnerproto.MaxEnvFile*2), 0o600))
	if got := len(readEnvFile(ws, "build")); got != runnerproto.MaxEnvFile+1 {
		t.Fatalf("%d bytes", got)
	}
}

// A log upload that broke is no reason to end the stage: its writes go on
// succeeding, and the failure is said once the stage is over.
func TestAStagesWritesSucceedAfterTheLogUploadBroke(t *testing.T) {
	pr, pw := io.Pipe()
	_ = pr.Close()
	s := &sink{w: pw}
	for range 2 {
		if n, err := s.Write([]byte("line\n")); n != 5 || err != nil {
			t.Fatal(n, err)
		}
	}
	if s.err() == nil {
		t.Fatal("the broken upload is not said")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestAnRunnerIsGivenItsServerItsWorkdirAndAToken(t *testing.T) {
	var out strings.Builder
	if code := Main([]string{"--workdir", t.TempDir()}, strings.NewReader(""), &out); code != 2 || !strings.Contains(out.String(), "--url and --workdir") {
		t.Fatal(code, out.String())
	}
	t.Setenv(TokenEnv, "")
	out.Reset()
	if code := Main([]string{"--url", "http://x", "--workdir", t.TempDir()}, strings.NewReader(""), &out); code != 2 || !strings.Contains(out.String(), "no token") {
		t.Fatal(code, out.String())
	}
}

func TestAnRunnersTokenComesFromItsFlagStdinOrItsVariable(t *testing.T) {
	c, err := configFrom("http://x", "-", "/w", "sh", strings.NewReader("kippa_from-stdin\n"))
	if err != nil || c.Token != "kippa_from-stdin" {
		t.Fatal(c.Token, err)
	}
	t.Setenv(TokenEnv, "kippa_from-env")
	if c, err = configFrom("http://x", "", "/w", "sh", nil); err != nil || c.Token != "kippa_from-env" {
		t.Fatal(c.Token, err)
	}
	if c, err = configFrom("http://x", "kippa_flag", "/w", "sh", nil); err != nil || c.Token != "kippa_flag" {
		t.Fatal(c.Token, err)
	}
}

// A heartbeat that listed a stage before it said it was done may come
// back with the stage to kill, for the server ended it meanwhile: that
// is no stage the server lost, and its build's workspace stays.
func TestAStageThatSaidItWasDoneIsNotKilledAfterwards(t *testing.T) {
	a := &daemon{c: defaults(Config{Log: nil}), running: map[string]*job{}}
	j := a.add(&runnerproto.Assignment{ID: "x", Workspace: "j/1"})
	a.said(j)
	a.kill("x")
	if j.killed {
		t.Fatal("the stage's workspace would be removed")
	}
	select {
	case <-j.cancel:
		t.Fatal("the stage was stopped")
	default:
	}
}

// A server that counts the runner offline (it started again, or a partition
// outlasted the heartbeats) answers a claim 400: the runner connects again
// and goes on, rather than ending and killing what it runs.
func TestAClaimRefusedAsOfflineConnectsAgain(t *testing.T) {
	var connects, claims atomic.Int32
	secondClaim := make(chan struct{}) // closed when the runner claims again
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case runnerproto.ConnectPath:
			connects.Add(1)
			_ = json.NewEncoder(w).Encode(runnerproto.ConnectResponse{Name: "box", Heartbeat: time.Hour})
		case runnerproto.ClaimPath:
			if claims.Add(1) == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(runnerproto.Refusal{Error: "Bad Request"})
				return
			}
			once.Do(func() { close(secondClaim) })
			<-r.Context().Done() // a long-poll with nothing to give
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ended := make(chan error, 1)
	go func() {
		ended <- Run(ctx, Config{URL: srv.URL, Token: "kippa_x", Workdir: t.TempDir(), Version: "v", Log: slog.New(slog.DiscardHandler), Retry: 10 * time.Millisecond})
	}()
	select {
	case <-secondClaim:
	case err := <-ended:
		t.Fatalf("the runner ended on a claim refused as offline: %v", err)
	case <-time.After(waitfor.Safety):
		t.Fatal("the runner never claimed again")
	}
	cancel()
	if err := <-ended; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("the runner ended with %v", err)
	}
	if connects.Load() < 2 || claims.Load() < 2 {
		t.Fatalf("connects %d claims %d", connects.Load(), claims.Load())
	}
}

// A server that started again while a stage was saying it was done knows
// nothing of the stage, nor of its build: the done is refused as gone, and
// the runner removes the build's workspace, which no one else will.
func TestAStageWhoseDoneTheServerNoLongerKnowsTakesItsWorkspace(t *testing.T) {
	var claims atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == runnerproto.ConnectPath:
			_ = json.NewEncoder(w).Encode(runnerproto.ConnectResponse{Name: "box", Heartbeat: time.Hour})
		case r.URL.Path == runnerproto.ClaimPath:
			claim := runnerproto.Claim{}
			if claims.Add(1) == 1 {
				claim.Assignment = &runnerproto.Assignment{ID: "x", Job: "j", Number: 1, Stage: "s", Command: "echo x > f", Workspace: "j/1"}
			} else {
				<-r.Context().Done() // a long-poll with nothing to give
			}
			_ = json.NewEncoder(w).Encode(claim)
		case strings.HasSuffix(r.URL.Path, runnerproto.LogSuffix):
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, runnerproto.ControlSuffix):
			<-r.Context().Done()
		case strings.HasSuffix(r.URL.Path, runnerproto.DoneSuffix):
			w.WriteHeader(http.StatusGone)
			_ = json.NewEncoder(w).Encode(runnerproto.Refusal{Error: "no such stage running on this runner"})
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		_ = Run(ctx, Config{URL: srv.URL, Token: "kippa_x", Workdir: dir, Version: "v", Log: slog.New(slog.DiscardHandler), Retry: 10 * time.Millisecond})
	}()
	defer func() { cancel(); <-ended }()
	ws := filepath.Join(dir, "j", "1")
	waitfor.Until(t, "the workspace of the stage the server forgot is removed", func() bool {
		_, err := os.Stat(filepath.Join(ws, "f"))
		if claims.Load() <= 1 || !errors.Is(err, os.ErrNotExist) {
			return false
		}
		_, err = os.Stat(ws)
		return errors.Is(err, os.ErrNotExist)
	})
}

func TestKippRunnerRunsMetricsCheck(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "metrics.json")
	if err := os.WriteFile(base, []byte(`{"store": "site", "metrics": [{"name": "coverage", "values": {"total": 87.19}, "from": {"total": {"job": "site-deploy", "number": 123}}}],
		"guards": [{"metric": "coverage", "item": "total", "no_lower": true, "on_fall": "failed", "required": true}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "m.json"), []byte(`{"metrics": [{"name": "coverage", "values": {"total": 87.5}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KIPP_METRICS", base)
	var out, errOut strings.Builder
	code := Command([]string{"metrics", "check", filepath.Join(dir, "m.json")}, strings.NewReader(""), &out, &errOut)
	want := "kipp: metrics guard: coverage total 87.19 -> 87.50 (+0.31), remembered from site-deploy #123, tolerance 0: held\n"
	if code != 0 || out.String() != want || errOut.Len() != 0 {
		t.Fatalf("%d %q %q", code, out.String(), errOut.String())
	}
	if code := Command([]string{"metrics", "ls"}, strings.NewReader(""), &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "usage: kipp-runner metrics check <glob>") {
		t.Fatalf("%d %q", code, errOut.String())
	}
	// Anything else is the runner itself, which needs its flags.
	errOut.Reset()
	if code := Command(nil, strings.NewReader(""), &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "--url and --workdir are needed") {
		t.Fatalf("%d %q", code, errOut.String())
	}
}

// A crash mid-stage leaves its file secrets behind; a runner that starts
// removes them before it takes a stage.
func TestAnRunnerThatStartsRemovesTheSecretFilesADeadOneLeft(t *testing.T) {
	c := Config{URL: "http://127.0.0.1:1", Token: "kippa_x", Workdir: t.TempDir(), Log: slog.New(slog.DiscardHandler)}
	left := filepath.Join(c.secretFilesDir(), "s-dead", "KEYFILE")
	must(t, os.MkdirAll(filepath.Dir(left), 0o700))
	must(t, os.WriteFile(left, []byte("bytes"), 0o600))
	t.Cleanup(func() { _ = os.RemoveAll(c.secretFilesDir()) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = Run(ctx, c)
	if _, err := os.Stat(left); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the file a dead runner left is still there: %v", err)
	}
	if strings.HasPrefix(c.secretFilesDir(), c.Workdir) {
		t.Fatal("the secret files are inside the workdir")
	}
}
