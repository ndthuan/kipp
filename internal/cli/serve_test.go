package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/secretbox"
	"github.com/ndthuan/kipp/internal/store"
	"github.com/ndthuan/kipp/internal/version"
	"github.com/ndthuan/kipp/internal/waitfor"
)

const adminPassword = "the boss's own password"

var listening = regexp.MustCompile(`listening on (http://[^,]+),`)

// served is a server running in this process, with an admin to talk to it.
type served struct {
	t          *testing.T
	data, jobs string
	url, token string
	stderr     *syncBuffer
	stop       func() int // ends it and returns its exit status
	args       []string   // given to serve beside the data, the jobs and the address
}

// dataAndJobs makes a data directory with an admin and a jobs directory
// with the given files; it returns the admin's token.
func dataAndJobs(t *testing.T, jobs map[string]string) (data, jobsDir, token string) {
	t.Helper()
	data, jobsDir = newData(t), t.TempDir()
	for name, body := range jobs {
		write(t, filepath.Join(jobsDir, name), body, 0o600)
	}
	ok(t, adminPassword+"\n", "user", "add", "admin", "--admin", "--password-stdin", "--data", data)
	out := ok(t, "", "token", "create", "admin", "--label", "test", "--data", data)
	return data, jobsDir, strings.TrimSpace(out[strings.LastIndex(out, "kipp_"):])
}

// address reads the line a server prints once it listens.
func address(t *testing.T, stdout io.Reader, stderr *syncBuffer) string {
	t.Helper()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	m := listening.FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("the server did not say where it listens: %q, %v\nstderr: %s", line, err, stderr)
	}
	return m[1]
}

func start(t *testing.T, jobs map[string]string) *served {
	t.Helper()
	s := &served{t: t, stderr: &syncBuffer{}}
	s.data, s.jobs, s.token = dataAndJobs(t, jobs)
	s.serve()
	return s
}

// serve starts the server on a port the system picks.
func (s *served) serve() {
	s.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	done := make(chan int, 1)
	args := append([]string{"--data", s.data, "--jobs", s.jobs, "--listen", "127.0.0.1:0"}, s.args...)
	go func() {
		code := serve(ctx, args, pw, s.stderr)
		_ = pw.Close()
		done <- code
	}()
	code := -1
	s.stop = func() int {
		cancel()
		if code >= 0 {
			return code
		}
		select {
		case code = <-done:
		case <-time.After(30 * time.Second):
			s.t.Fatalf("the server did not stop\nstderr: %s", s.stderr)
		}
		return code
	}
	s.t.Cleanup(func() { s.stop() })
	s.url = address(s.t, pr, s.stderr)
	go func() { _, _ = io.Copy(io.Discard, pr) }()
}

// kipp runs a client command against the server as the admin.
func (s *served) kipp(args ...string) (code int, out, errOut string) {
	return kipp("", append(args, "--url", s.url, "--token", s.token)...)
}

// login is the admin's browser session.
func (s *served) login() *http.Cookie {
	s.t.Helper()
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := noFollow.PostForm(s.url+"/login", url.Values{"name": {"admin"}, "password": {adminPassword}})
	must(s.t, err)
	defer func() { _ = res.Body.Close() }()
	for _, c := range res.Cookies() {
		if c.Name == api.SessionCookie {
			return c
		}
	}
	s.t.Fatalf("no session: %s", res.Status)
	return nil
}

func (s *served) get(path string, session *http.Cookie) (int, string) {
	s.t.Helper()
	req, err := http.NewRequest(http.MethodGet, s.url+path, nil)
	must(s.t, err)
	if session != nil {
		req.Header.Set("Cookie", session.Name+"="+session.Value)
	}
	res, err := http.DefaultClient.Do(req)
	must(s.t, err)
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	must(s.t, err)
	return res.StatusCode, string(body)
}

// soon ends a server that started when it should have refused to, so that
// the test fails on its exit status and does not hang.
func soon(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitfor.Safety)
	t.Cleanup(cancel)
	return ctx
}

const helloJob = `name: hello
params:
  WHO: {default: world}
secrets: [API_KEY]
stages:
  greet: {run: 'echo hello $WHO; echo key=$API_KEY; echo url=$KIPP_URL'}
`

func TestServeRefusesABadJobsDirectoryAndSaysEveryError(t *testing.T) {
	jobs, data := t.TempDir(), filepath.Join(t.TempDir(), "data")
	write(t, filepath.Join(jobs, "good.yml"), "name: good\nstages:\n  s: {run: 'true'}\n", 0o600)
	write(t, filepath.Join(jobs, "first.yml"), "name: other\nstages:\n  s: {run: 'true'}\n", 0o600)
	write(t, filepath.Join(jobs, "second.yml"), "name: second\nstages: {}\n", 0o600)
	var out, errOut syncBuffer
	code := serve(soon(t), []string{"--data", data, "--jobs", jobs, "--listen", "127.0.0.1:0"}, &out, &errOut)
	eq(t, "exit", code, 1)
	contains(t, errOut.String(), "not starting", "first.yml", "second.yml")
	lacks(t, out.String(), "listening")
	if _, err := os.Stat(data); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a server that refused to start left a data directory: %v", err)
	}
}

func TestServeSaysWhatItCannotStartWith(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	defer func() { _ = taken.Close() }()
	data, jobs, _ := dataAndJobs(t, map[string]string{"hello.yml": helloJob})
	for what, c := range map[string]struct {
		args []string
		code int
		says string
	}{
		"no data directory":          {[]string{"--jobs", jobs}, 2, "--data is required"},
		"no jobs directory, off":     {[]string{"--data", data, "--job-edits", "off"}, 2, "--jobs is required with --job-edits off"},
		"no jobs directory, files":   {[]string{"--data", data, "--job-edits", "files"}, 2, "--jobs is required with --job-edits files"},
		"a mode that is none":        {[]string{"--data", data, "--jobs", jobs, "--job-edits", "git"}, 2, "--job-edits is off, store or files"},
		"executors below zero":       {[]string{"--data", data, "--jobs", jobs, "--executors", "-1"}, 2, "--executors is 0 or more"},
		"builds below zero":          {[]string{"--data", data, "--jobs", jobs, "--builds", "-1"}, 2, "--builds"},
		"flaky flips below zero":     {[]string{"--data", data, "--jobs", jobs, "--flaky-flips", "-1"}, 2, "--flaky-flips is 0"},
		"a label that is none":       {[]string{"--data", data, "--jobs", jobs, "--default-runs-on", "Linux"}, 2, `--default-runs-on label "Linux"`},
		"a runner-env prefix":        {[]string{"--data", data, "--jobs", jobs, "--runner-env", "LC_*"}, 2, `--runner-env "LC_*"`},
		"a runner URL that is none":  {[]string{"--data", data, "--jobs", jobs, "--runner-url", "192.0.2.1:8087"}, 2, "--runner-url is an http:// or https:// URL"},
		"a runner address in use":    {[]string{"--data", data, "--jobs", jobs, "--listen", "127.0.0.1:0", "--runner-listen", taken.Addr().String()}, 1, "--runner-listen"},
		"a zone that is none":        {[]string{"--data", data, "--jobs", jobs, "--timezone", "Mars/Olympus"}, 2, "--timezone"},
		"the host's by name":         {[]string{"--data", data, "--jobs", jobs, "--timezone", "Local"}, 2, "--timezone"},
		"an argument":                {[]string{"--data", data, "--jobs", jobs, "extra"}, 2, "usage: kipp serve"},
		"an address in use":          {[]string{"--data", data, "--jobs", jobs, "--listen", taken.Addr().String()}, 1, "address already in use"},
		"a jobs dir not here":        {[]string{"--data", data, "--jobs", filepath.Join(jobs, "missing")}, 1, "missing"},
		"a proxy that is no CIDR":    {[]string{"--data", data, "--jobs", jobs, "--trusted-proxy", "127.0.0.1/8,proxy.example"}, 2, `--trusted-proxy "proxy.example" is not a CIDR`},
		"a header that is no name":   {[]string{"--data", data, "--jobs", jobs, "--client-ip-header", "X-Real-IP: 1"}, 2, "--client-ip-header"},
		"a variable that is no name": {[]string{"--data", data, "--jobs", jobs, "--pass-env", "PATH,AWS KEY"}, 2, `--pass-env "AWS KEY"`},
	} {
		var out, errOut syncBuffer
		eq(t, what, serve(soon(t), c.args, &out, &errOut), c.code)
		contains(t, errOut.String(), c.says)
	}
}

func TestStagesAreGivenTheServerVariablesPassEnvNamesAndNoOthers(t *testing.T) {
	t.Setenv("KIPP_TEST_SERVE_VAR", "from-the-server")
	job := map[string]string{"env.yml": "name: env\nstages:\n  s: {run: 'echo var=$KIPP_TEST_SERVE_VAR path=${PATH:+set}'}\n"}
	for pass, want := range map[string]string{
		"":                       "var= path=set",
		"PATH,KIPP_TEST_SERVE_*": "var=from-the-server path=set",
		"*":                      "var=from-the-server path=set",
	} {
		s := &served{t: t, stderr: &syncBuffer{}}
		s.data, s.jobs, s.token = dataAndJobs(t, job)
		if pass != "" {
			s.args = []string{"--pass-env", pass}
		}
		s.serve()
		code, out, errOut := s.kipp("run", "env", "--wait")
		eq(t, "exit: "+errOut, code, 0)
		contains(t, out, "[s] "+want+"\n")
	}
}

func TestServeHelpSaysWhatAReverseProxyMustDo(t *testing.T) {
	var out, errOut syncBuffer
	eq(t, "exit", serve(soon(t), []string{"-h"}, &out, &errOut), 0)
	contains(t, errOut.String(), "Host header", "HTTPS", "-session-ttl", "-shell", "-client-ip-header", "-trusted-proxy", "-pass-env")
}

func TestATrustedProxyIsACIDROrOneAddress(t *testing.T) {
	got, err := parsePrefixes(" 10.0.0.1/8, 192.0.2.7 ,::1, ")
	must(t, err)
	eq(t, "prefixes", fmt.Sprint(got), "[10.0.0.0/8 192.0.2.7/32 ::1/128]")
	none, err := parsePrefixes("")
	must(t, err)
	eq(t, "none", len(none), 0)
}

func TestRunWaitStreamsTheBuildAndMasksItsSecret(t *testing.T) {
	s := start(t, map[string]string{"hello.yml": helloJob})
	// Set while the server runs: a build reads its secrets when it starts.
	ok(t, "s3cret-enough\n", "secret", "set", "API_KEY", "--data", s.data)

	code, out, errOut := s.kipp("run", "hello", "-p", "WHO=kipp", "--wait")
	eq(t, "stderr", errOut, "")
	eq(t, "exit", code, 0)
	eq(t, "stdout", out, "hello #1 queued (build 1)\n[greet] hello kipp\n[greet] key=***\n[greet] url="+s.url+"\nhello #1: success\n")

	code, out, errOut = s.kipp("run", "hello")
	eq(t, "exit without --wait", code, 0)
	eq(t, "stdout without --wait", out+errOut, "hello #2 queued (build 2)\n")

	eq(t, "exit", s.stop(), 0)
	contains(t, s.stderr.String(), "shutting down")
	lacks(t, s.stderr.String(), "s3cret-enough", s.token, adminPassword)
}

func TestRunWaitPrintsWhatAStageWritesWhileItStillRuns(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "gate")
	s := start(t, map[string]string{"gated.yml": "name: gated\nstages:\n  s: {run: 'echo first; printf half; while [ ! -e " + gate + " ]; do sleep 0.05; done; echo second'}\n"})
	var out, errOut syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- Main([]string{"run", "gated", "--wait", "--url", s.url, "--token", s.token}, strings.NewReader(""), &out, &errOut)
	}()
	eventually(t, "the first line is printed", func() bool { return strings.Contains(out.String(), "[s] first\n") })
	select {
	case code := <-done:
		t.Fatalf("run --wait returned %d before the build ended: %s%s", code, out.String(), errOut.String())
	default:
	}
	lacks(t, out.String(), "second", "half")
	write(t, gate, "", 0o600)
	eq(t, "exit", <-done, 0)
	// A line that arrives in two reads is still one line.
	eq(t, "stdout", out.String(), "gated #1 queued (build 1)\n[s] first\n[s] halfsecond\ngated #1: success\n")
}

// The hook writes after the build's status is final; a gate that reads
// run --wait's output must still see what the hook said.
func TestRunWaitPrintsTheHooksOutputBeforeItReturns(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "gate")
	s := start(t, map[string]string{"hooked.yml": "name: hooked\nstages:\n  s: {run: echo stage}\non_success: {run: 'while [ ! -e " + gate + " ]; do sleep 0.05; done; echo hook done'}\n"})
	var out, errOut syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- Main([]string{"run", "hooked", "--wait", "--url", s.url, "--token", s.token}, strings.NewReader(""), &out, &errOut)
	}()
	eventually(t, "the build's status is final", func() bool { return strings.Contains(kippGet(t, s, "/api/v1/builds/1"), `"status":"success"`) })
	select {
	case code := <-done:
		t.Fatalf("run --wait returned %d while the hook ran: %s%s", code, out.String(), errOut.String())
	case <-time.After(10 * tailEvery):
	}
	write(t, gate, "", 0o600)
	eq(t, "exit", <-done, 0)
	eq(t, "stdout", out.String(), "hooked #1 queued (build 1)\n[s] stage\n[hooks] hook done\nhooked #1: success\n")
}

// A build read as running whose logs the server then calls complete ended
// in between: it is read again, not reported as it was.
func TestRunWaitReadsTheBuildAgainWhenItsLogsWereCompleteFirst(t *testing.T) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":1,"job":"j","number":1,"status":"queued"}`)
		case strings.HasSuffix(r.URL.Path, "/log"):
			w.Header().Set("X-Kipp-Offset", "0")
			w.Header().Set("X-Kipp-More", "false")
		case polls.Add(1) == 1:
			_, _ = io.WriteString(w, `{"id":1,"job":"j","number":1,"status":"running","stages":[{"name":"s","status":"running"}]}`)
		default:
			_, _ = io.WriteString(w, `{"id":1,"job":"j","number":1,"status":"success","stages":[{"name":"s","status":"success"}]}`)
		}
	}))
	defer srv.Close()
	code, out, errOut := kipp("", "run", "j", "--wait", "--url", srv.URL, "--token", "kipp_t")
	eq(t, "stderr", errOut, "")
	eq(t, "exit", code, 0)
	eq(t, "stdout", out, "j #1 queued (build 1)\nj #1: success\n")
}

func TestRunExitsWithTheBuildsFate(t *testing.T) {
	s := start(t, map[string]string{
		"hello.yml":  helloJob,
		"broken.yml": "name: broken\nstages:\n  s: {run: 'echo before; exit 7'}\n",
		"plain.yml":  "name: plain\nstages:\n  s: {run: 'printf unfinished'}\n",
	})
	code, out, _ := s.kipp("run", "broken", "--wait")
	eq(t, "a failed build", code, 1)
	contains(t, out, "[s] before\n", "broken #1: failed\n")

	code, out, _ = s.kipp("run", "plain", "--wait")
	eq(t, "a build that passes", code, 0)
	contains(t, out, "[s] unfinished\nplain #1: success\n")

	// The one account of a build that never reached a stage is what the
	// server wrote before them; the secret is named, and it has no value.
	code, out, _ = s.kipp("run", "hello", "--wait")
	eq(t, "a build with a secret nobody set", code, 1)
	contains(t, out, "[prepare] ", "secret API_KEY is not set", "hello #1: failed\n")

	for what, c := range map[string]struct {
		args []string
		code int
		says string
	}{
		"a token nobody holds":   {[]string{"run", "plain", "--wait", "--url", s.url, "--token", "kipp_nobody"}, 3, "refused"},
		"a job that is not one":  {[]string{"run", "nope", "--url", s.url, "--token", s.token}, 1, "not found"},
		"a parameter refused":    {[]string{"run", "hello", "-p", "NOPE=1", "--url", s.url, "--token", s.token}, 1, "NOPE"},
		"a parameter with no =":  {[]string{"run", "hello", "-p", "WHO", "--url", s.url, "--token", s.token}, 2, "KEY=VALUE"},
		"no job":                 {[]string{"run", "--url", s.url, "--token", s.token}, 2, "usage: kipp run"},
		"a reload nobody may do": {[]string{"reload", "--url", s.url, "--token", "kipp_nobody"}, 3, "refused"},
		"a server that is away":  {[]string{"reload", "--url", "http://127.0.0.1:1", "--token", s.token}, 1, "kipp reload:"},
	} {
		code, out, errOut := kipp("", c.args...)
		eq(t, what, code, c.code)
		contains(t, errOut, c.says)
		lacks(t, out+errOut, s.token)
	}
}

func TestTheTokenComesFromTheFlagTheEnvironmentOrAPrivateFile(t *testing.T) {
	s := start(t, map[string]string{"hello.yml": helloJob})
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	t.Setenv("KIPP_URL", s.url)
	t.Setenv("KIPP_TOKEN", "")
	file := filepath.Join(config, "kipp", "token")

	code, _, errOut := kipp("", "reload")
	eq(t, "no token anywhere", code, 2)
	contains(t, errOut, "KIPP_TOKEN", file)

	write(t, file, s.token+"\n", 0o644)
	code, _, errOut = kipp("", "reload")
	eq(t, "a token file others can read", code, 2)
	contains(t, errOut, file, "chmod 600")
	lacks(t, errOut, s.token)

	must(t, os.Chmod(file, 0o600))
	eq(t, "a private token file", ok(t, "", "reload"), "reloaded\n")

	// The environment wins over the file, and the flag over both.
	t.Setenv("KIPP_TOKEN", "kipp_nobody")
	code, _, _ = kipp("", "reload")
	eq(t, "a bad token in the environment", code, 3)
	eq(t, "the flag", ok(t, "", "reload", "--token", s.token), "reloaded\n")
}

func TestABadReloadKeepsTheOldJobsAndSaysSoUntilAGoodOne(t *testing.T) {
	s := start(t, map[string]string{"plain.yml": "name: plain\nstages:\n  s: {run: 'echo still here'}\n"})
	session := s.login()
	_, page := s.get("/", session)
	contains(t, page, "plain")
	lacks(t, page, `role="alert"`)

	write(t, filepath.Join(s.jobs, "wrong.yml"), "name: other\nstages:\n  s: {run: 'true'}\n", 0o600)
	write(t, filepath.Join(s.jobs, "added.yml"), "name: added\nstages:\n  s: {run: 'true'}\n", 0o600)
	must(t, syscall.Kill(os.Getpid(), syscall.SIGHUP))
	eventually(t, "the banner names the bad file", func() bool {
		_, page = s.get("/", session)
		return strings.Contains(page, `role="alert"`) && strings.Contains(page, "wrong.yml")
	})
	lacks(t, page, "added")
	contains(t, s.stderr.String(), "reload refused")
	_, login := s.get("/login", nil)
	lacks(t, login, "wrong.yml")

	code, out, _ := s.kipp("run", "plain", "--wait")
	eq(t, "the old set still builds", code, 0)
	contains(t, out, "[s] still here\n")
	code, _, _ = s.kipp("run", "added")
	eq(t, "nothing of the bad set was taken", code, 1)

	code, _, errOut := s.kipp("reload")
	eq(t, "a reload of the bad set", code, 1)
	contains(t, errOut, "wrong.yml")
	_, page = s.get("/", session)
	contains(t, page, `role="alert"`, "wrong.yml")

	must(t, os.Remove(filepath.Join(s.jobs, "wrong.yml")))
	code, out, errOut = s.kipp("reload")
	eq(t, "a reload of the mended set", code, 0)
	eq(t, "what it says", out+errOut, "reloaded\n")
	_, page = s.get("/", session)
	lacks(t, page, `role="alert"`, "wrong.yml")
	contains(t, page, "added", "plain")

	// SIGHUP takes a good set as the API does.
	must(t, os.Remove(filepath.Join(s.jobs, "added.yml")))
	must(t, syscall.Kill(os.Getpid(), syscall.SIGHUP))
	eventually(t, "the removed job is gone", func() bool {
		_, page = s.get("/", session)
		return !strings.Contains(page, "added")
	})
}

func TestServeMountsTheSiteTheAPITheHookAndRetention(t *testing.T) {
	s := start(t, map[string]string{
		"hooked.yml": "name: hooked\ntriggers:\n  webhook: {repo: example/app, branches: [main]}\nkeep: {builds: 1}\nstages:\n  s: {run: 'echo commit=$KIPP_COMMIT'}\n",
	})
	code, body := s.get("/api/v1/health", nil)
	eq(t, "health", code, http.StatusOK)
	contains(t, body, `"ok":true`)
	for _, path := range []string{"/static/kipp.css", "/favicon.ico", "/login"} {
		code, _ := s.get(path, nil)
		eq(t, path, code, http.StatusOK)
	}
	code, _ = s.get("/api/v1/jobs", nil)
	eq(t, "the API without a token", code, http.StatusUnauthorized)

	ok(t, "the hook's secret\n", "secret", "set", "GITHUB_WEBHOOK_HOOKED", "--data", s.data)
	push := func(key, delivery string) int {
		commit := strings.Repeat("ab", 20)
		payload := `{"ref":"refs/heads/main","after":"` + commit + `","repository":{"full_name":"example/app"}}`
		mac := hmac.New(sha256.New, []byte(key))
		mac.Write([]byte(payload))
		req, err := http.NewRequest(http.MethodPost, s.url+"/api/v1/hooks/github/hooked", strings.NewReader(payload))
		must(t, err)
		req.Header.Set("X-GitHub-Event", "push")
		req.Header.Set("X-GitHub-Delivery", delivery)
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		res, err := http.DefaultClient.Do(req)
		must(t, err)
		_ = res.Body.Close()
		return res.StatusCode
	}
	eq(t, "a push signed with another key", push("not the hook's secret", "d-1"), http.StatusUnauthorized)
	eq(t, "a push signed with the stored secret", push("the hook's secret", "d-1"), http.StatusAccepted)
	eq(t, "the same delivery again", push("the hook's secret", "d-1"), http.StatusConflict)

	// keep: {builds: 1}: the second build's end removes the first.
	code, _, errOut := s.kipp("run", "hooked", "--wait")
	eq(t, "a second build: "+errOut, code, 0)
	eventually(t, "retention leaves one build", func() bool {
		body := kippGet(t, s, "/api/v1/jobs/hooked/builds")
		return strings.Count(body, `"number":`) == 1 && strings.Contains(body, `"number":2`)
	})
}

// kippGet is a GET as the admin's token.
func kippGet(t *testing.T, s *served, path string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, s.url+path, nil)
	must(t, err)
	req.Header.Set("Authorization", "Bearer "+s.token)
	res, err := http.DefaultClient.Do(req)
	must(t, err)
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	must(t, err)
	return string(body)
}

func TestServeStartsOverWhatTheLastProcessLeft(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root removes anything")
	}
	s := &served{t: t, stderr: &syncBuffer{}}
	s.data, s.jobs, s.token = dataAndJobs(t, map[string]string{"plain.yml": "name: plain\nstages:\n  s: {run: 'true'}\n"})
	ctx := context.Background()
	st, err := store.Open(filepath.Join(s.data, "db.sqlite"))
	must(t, err)
	b := &store.Build{Job: "plain", Status: store.Queued, Trigger: "api"}
	must(t, st.CreateBuild(ctx, b))
	must(t, st.SetBuildStatus(ctx, b.ID, store.Running, time.Now()))
	must(t, st.Close())
	// A workspace the server cannot remove: its folder does not let it.
	locked := filepath.Join(s.data, "workspace", "plain")
	must(t, os.MkdirAll(filepath.Join(locked, "1"), 0o750))
	must(t, os.Chmod(locked, 0o500))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	s.serve()
	contains(t, s.stderr.String(), "leftover workspace")
	body := kippGet(t, s, "/api/v1/builds/"+strconv.FormatInt(b.ID, 10))
	contains(t, body, `"status":"lost"`)
}

func TestSIGTERMEndsARunningBuildAsLostAndLeavesNothingRunning(t *testing.T) {
	scratch := t.TempDir()
	pids := filepath.Join(scratch, "pids")
	// The stage and a child of it, both of which must go.
	write(t, filepath.Join(scratch, "stage.sh"), "sleep 300 &\necho \"$$ $!\" > "+pids+".tmp\nmv "+pids+".tmp "+pids+"\nwait\n", 0o600)
	data, jobs, token := dataAndJobs(t, map[string]string{"slow.yml": "name: slow\nstages:\n  s: {run: 'sh " + filepath.Join(scratch, "stage.sh") + "'}\n"})

	cmd := exec.Command(os.Args[0], "serve", "--data", data, "--jobs", jobs, "--listen", "127.0.0.1:0")
	cmd.Env = append(os.Environ(), asKipp+"=1")
	stderr := &syncBuffer{}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	must(t, err)
	must(t, cmd.Start())
	exited := make(chan error, 1)
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	addr := address(t, stdout, stderr)
	go func() { exited <- cmd.Wait() }()

	ok(t, "", "run", "slow", "--url", addr, "--token", token)
	var stage, child int
	eventually(t, "the stage is running", func() bool {
		text, err := os.ReadFile(pids)
		if err != nil {
			return false
		}
		fields := strings.Fields(string(text))
		stage, _ = strconv.Atoi(fields[0])
		child, _ = strconv.Atoi(fields[1])
		return stage > 0 && child > 0
	})
	must(t, syscall.Kill(stage, 0))
	must(t, syscall.Kill(child, 0))

	asked := time.Now()
	must(t, cmd.Process.Signal(syscall.SIGTERM))
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("the server did not exit 0: %v\nstderr: %s", err, stderr)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("the server was still up 20s after SIGTERM\nstderr: %s", stderr)
	}
	t.Logf("exited %v after SIGTERM", time.Since(asked).Round(time.Millisecond))
	for _, pid := range []int{stage, child} {
		eventually(t, "process "+strconv.Itoa(pid)+" is gone", func() bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) })
	}
	builds, err := openDB(t, data).Builds(context.Background(), "slow", 10)
	must(t, err)
	eq(t, "builds", len(builds), 1)
	eq(t, "the build's status", builds[0].Status, store.Lost)
}

func TestTheSweepIsAtHalfPastThreeLocal(t *testing.T) {
	at := func(day, hour, minute int) time.Time { return time.Date(2026, 10, day, hour, minute, 0, 0, time.Local) }
	for now, want := range map[time.Time]time.Time{
		at(11, 0, 0):   at(11, 3, 30),
		at(11, 3, 29):  at(11, 3, 30),
		at(11, 3, 30):  at(12, 3, 30),
		at(11, 23, 59): at(12, 3, 30),
		at(31, 12, 0):  time.Date(2026, 11, 1, 3, 30, 0, 0, time.Local),
	} {
		if got := nextSweep(now); !got.Equal(want) {
			t.Errorf("after %v: got %v, want %v", now, got, want)
		}
	}
}

// The lock is what keeps a second server from calling the first one's
// running builds lost and removing the workspaces they run in.
func TestASecondServeOnTheSameDataDirectoryIsRefusedAndTouchesNothing(t *testing.T) {
	s := start(t, map[string]string{"slow.yml": "name: slow\nstages:\n  s: {run: 'sleep 30'}\n"})
	ok(t, "", "run", "slow", "--url", s.url, "--token", s.token)
	workspace := filepath.Join(s.data, "workspace", "slow", "1")
	eventually(t, "the build runs", func() bool {
		_, err := os.Stat(workspace)
		return err == nil && strings.Contains(kippGet(t, s, "/api/v1/builds/1"), `"status":"running"`)
	})

	var out, errOut syncBuffer
	code := serve(soon(t), []string{"--data", s.data, "--jobs", s.jobs, "--listen", "127.0.0.1:0"}, &out, &errOut)
	eq(t, "a second server's exit", code, 1)
	contains(t, errOut.String(), "another kipp serve is running", s.data)
	lacks(t, out.String(), "listening")
	contains(t, kippGet(t, s, "/api/v1/builds/1"), `"status":"running"`)
	if _, err := os.Stat(workspace); err != nil {
		t.Fatalf("the running build's workspace: %v", err)
	}
	// The other commands work beside a running server, as they must.
	ok(t, "", "user", "list", "--data", s.data)

	eq(t, "the first server's exit", s.stop(), 0)
	s.serve() // the lock went with the server that held it
	contains(t, kippGet(t, s, "/api/v1/builds/1"), `"status":"lost"`)
}

// The first signal ends the builds, which takes as long as a stage takes
// to die; the second is not waited out.
func TestASecondSignalKillsAServerThatIsStillShuttingDown(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	// A stage that ignores SIGTERM holds the shutdown for the runner's grace.
	data, jobs, token := dataAndJobs(t, map[string]string{"deaf.yml": "name: deaf\nstages:\n  s: {run: \"trap '' TERM; echo $$ > " + pidFile + ".tmp; mv " + pidFile + ".tmp " + pidFile + "; sleep 60\"}\n"})
	cmd := exec.Command(os.Args[0], "serve", "--data", data, "--jobs", jobs, "--listen", "127.0.0.1:0")
	cmd.Env = append(os.Environ(), asKipp+"=1")
	stderr := &syncBuffer{}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	must(t, err)
	must(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	addr := address(t, stdout, stderr)
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	ok(t, "", "run", "deaf", "--url", addr, "--token", token)
	stage := 0
	eventually(t, "the stage is running", func() bool {
		text, _ := os.ReadFile(pidFile)
		stage, _ = strconv.Atoi(strings.TrimSpace(string(text)))
		return stage > 0
	})
	// The server is killed before it ends the stage, so the test does: the
	// stage leads its own process group.
	t.Cleanup(func() { _ = syscall.Kill(-stage, syscall.SIGKILL) })

	must(t, cmd.Process.Signal(syscall.SIGTERM))
	eventually(t, "the server is shutting down", func() bool { return strings.Contains(stderr.String(), "shutting down") })
	asked := time.Now()
	must(t, cmd.Process.Signal(syscall.SIGTERM))
	select {
	case err := <-exited:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || !exit.Sys().(syscall.WaitStatus).Signaled() {
			t.Fatalf("the second SIGTERM did not kill the server (it took %v): %v\nstderr: %s", time.Since(asked).Round(time.Millisecond), err, stderr)
		}
	case <-time.After(waitfor.Safety):
		t.Fatalf("the server was still up 30s after a second SIGTERM\nstderr: %s", stderr)
	}
}

// fast makes one of the server's two clocks tick every few milliseconds.
func fast(t *testing.T, clock *func(time.Time) time.Time) {
	t.Helper()
	before := *clock
	*clock = func(now time.Time) time.Time { return now.Add(20 * time.Millisecond) }
	t.Cleanup(func() { *clock = before })
}

func TestServeFiresTheCronJobsAtEveryMinuteOfItsClock(t *testing.T) {
	fast(t, &cronAt)
	s := start(t, map[string]string{
		"often.yml": "name: often\ntriggers: {cron: '* * * * *'}\nstages:\n  s: {run: 'true'}\n",
		"never.yml": "name: never\nstages:\n  s: {run: 'true'}\n",
	})
	eventually(t, "the cron job is built", func() bool {
		return strings.Contains(kippGet(t, s, "/api/v1/jobs/often/builds"), `"trigger":"cron"`)
	})
	eq(t, "builds of a job with no cron", kippGet(t, s, "/api/v1/jobs/never/builds"), `{"items":[],"next_cursor":null}`+"\n")
}

func TestTheCronClockIsTheNextWholeMinute(t *testing.T) {
	at := func(minute, second, nano int) time.Time {
		return time.Date(2026, 10, 11, 9, minute, second, nano, time.Local)
	}
	for now, want := range map[time.Time]time.Time{
		at(5, 0, 0):   at(6, 0, 0),
		at(5, 0, 1):   at(6, 0, 0),
		at(5, 59, 99): at(6, 0, 0),
		at(59, 30, 0): at(60, 0, 0),
	} {
		if got := nextMinute(now); !got.Equal(want) {
			t.Errorf("after %v: got %v, want %v", now, got, want)
		}
	}
}

// fakeTime is a clock that reads what the test says and a wait that ends
// at once: each wait sets the clock to the next reading, and with none
// left the loop is over.
type fakeTime struct {
	now    time.Time
	after  []time.Time // what the clock reads when each wait ends
	waited []time.Duration
}

func (f *fakeTime) read() time.Time { return f.now }

func (f *fakeTime) wait(_ context.Context, d time.Duration) bool {
	f.waited = append(f.waited, d)
	if len(f.after) == 0 {
		return false
	}
	f.now, f.after = f.after[0], f.after[1:]
	return true
}

func TestEveryGivesTheTimeThatWasDueAndNoMinuteTwice(t *testing.T) {
	at := func(minute, second, milli int) time.Time {
		return time.Date(2026, 10, 11, 9, minute, second, milli*int(time.Millisecond), time.Local)
	}
	clock := &fakeTime{now: at(5, 30, 0), after: []time.Time{
		at(6, 0, 300),  // the timer fired late: the minute that was due is still 09:06
		at(6, 59, 900), // the wall clock is behind the timer: 09:07 was due and is not yet read
		at(7, 59, 950), // and again, so that the minute after is not skipped either
	}}
	var got []time.Time
	every(context.Background(), clock.read, clock.wait, nextMinute, func(_ context.Context, due time.Time) { got = append(got, due) })
	want := []time.Time{at(6, 0, 0), at(7, 0, 0), at(8, 0, 0)}
	if !slices.Equal(got, want) {
		t.Fatalf("called for %v, want %v", got, want)
	}
	if want := []time.Duration{30 * time.Second, 59700 * time.Millisecond, 60100 * time.Millisecond, 60050 * time.Millisecond}; !slices.Equal(clock.waited, want) {
		t.Fatalf("waited %v, want %v", clock.waited, want)
	}
}

func TestServeSweepsOldBuildsAtEveryTickOfItsDailyClock(t *testing.T) {
	s := &served{t: t, stderr: &syncBuffer{}}
	s.data, s.jobs, s.token = dataAndJobs(t, map[string]string{"kept.yml": "name: kept\nkeep: {builds: 1}\nstages:\n  s: {run: 'true'}\n"})
	// Two builds that ended without the server's own end-of-build pass.
	ctx := context.Background()
	st, err := store.Open(filepath.Join(s.data, "db.sqlite"))
	must(t, err)
	for range 2 {
		b := &store.Build{Job: "kept", Status: store.Queued, Trigger: "api"}
		must(t, st.CreateBuild(ctx, b))
		must(t, st.SetBuildStatus(ctx, b.ID, store.Success, time.Now()))
	}
	must(t, st.Close())

	fast(t, &sweepAt)
	s.serve()
	eventually(t, "the sweep leaves the newest build", func() bool {
		body := kippGet(t, s, "/api/v1/jobs/kept/builds")
		return strings.Count(body, `"number":`) == 1 && strings.Contains(body, `"number":2`)
	})
	// The sweep says what it deleted once it is done, which may be after
	// the build is gone from the list.
	eventually(t, "the sweep says what it deleted", func() bool {
		return strings.Contains(s.stderr.String(), `msg="retention sweep" deleted=1`)
	})
}

// A compact still waiting for its builds when the server stops is
// cancelled, not waited out, and has written its end before serve returns:
// nothing of the server's is left writing into the data directory.
func TestServeCancelsACompactStillWaitingAndReturnsOnlyOnceItHasEnded(t *testing.T) {
	s := start(t, map[string]string{"slow.yml": "name: slow\nstages:\n  s: {run: 'sleep 30'}\n"})
	ok(t, "", "run", "slow", "--url", s.url, "--token", s.token)
	eventually(t, "the build runs", func() bool {
		return strings.Contains(kippGet(t, s, "/api/v1/builds/1"), `"status":"running"`)
	})
	req, err := http.NewRequest(http.MethodPost, s.url+"/api/v1/admin/storage/compact", nil)
	must(t, err)
	req.Header.Set("Authorization", "Bearer "+s.token)
	res, err := http.DefaultClient.Do(req)
	must(t, err)
	_ = res.Body.Close()
	eq(t, "start the compact", res.StatusCode, http.StatusAccepted)
	eventually(t, "the compact waits for the build", func() bool {
		return strings.Contains(kippGet(t, s, "/api/v1/admin/storage/compact"), `"holds_quiet":true`)
	})

	eq(t, "exit", s.stop(), 0)
	st, err := store.Open(filepath.Join(s.data, "db.sqlite"))
	must(t, err)
	defer func() { _ = st.Close() }()
	rows, err := st.AuditBefore(context.Background(), 0, 1)
	must(t, err)
	if len(rows) == 0 || rows[0].Action != "db.compact" || !strings.Contains(rows[0].Detail, "the server stopped") {
		t.Fatalf("the newest audit is not the compact's end: %+v", rows)
	}
}

// A deploy script greps the server's log for these two lines after it
// sends SIGHUP, so their wording is a promise: whoever changes either
// breaks such a script.
func TestTheTwoReloadLinesInTheLogKeepTheirWording(t *testing.T) {
	s := start(t, map[string]string{"plain.yml": "name: plain\nstages:\n  s: {run: 'true'}\n"})
	must(t, syscall.Kill(os.Getpid(), syscall.SIGHUP))
	eventually(t, "a reload that is taken is logged", func() bool {
		return strings.Contains(s.stderr.String(), `level=INFO msg="jobs reloaded" count=1 conflicts=0`)
	})
	lacks(t, s.stderr.String(), "reload refused")
	write(t, filepath.Join(s.jobs, "wrong.yml"), "name: other\nstages:\n  s: {run: 'true'}\n", 0o600)
	must(t, syscall.Kill(os.Getpid(), syscall.SIGHUP))
	eventually(t, "a reload that is refused is logged", func() bool {
		return strings.Contains(s.stderr.String(), `level=ERROR msg="reload refused; the previous job set stays" err=`)
	})
}

func TestStagesAreGivenAnAddressTheyCanCallWhenTheServerListensOnEveryInterface(t *testing.T) {
	for listen, want := range map[string]string{
		"0.0.0.0:8085":   "http://127.0.0.1:8085",
		"[::]:8085":      "http://127.0.0.1:8085",
		"127.0.0.1:8085": "http://127.0.0.1:8085",
		"[::1]:8085":     "http://[::1]:8085",
		"192.0.2.10:443": "http://192.0.2.10:443",
	} {
		addr, err := net.ResolveTCPAddr("tcp", listen)
		must(t, err)
		eq(t, listen, baseURLFor(addr), want)
	}
}

// everyInterface is a loopback listener that says it listens everywhere,
// as one made for ":8085" would: no test binds a real wildcard address.
type everyInterface struct{ net.Listener }

func (l everyInterface) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4zero, Port: l.Listener.Addr().(*net.TCPAddr).Port}
}

func TestAServerListeningOnEveryInterfaceGivesItsStagesTheLoopback(t *testing.T) {
	listen = func(network, address string) (net.Listener, error) {
		ln, err := net.Listen(network, address)
		return everyInterface{ln}, err
	}
	t.Cleanup(func() { listen = net.Listen })
	s := start(t, map[string]string{"plain.yml": "name: plain\nstages:\n  s: {run: 'echo url=$KIPP_URL'}\n"})
	contains(t, s.url, "http://0.0.0.0:")
	s.url = strings.Replace(s.url, "0.0.0.0", "127.0.0.1", 1)
	code, out, errOut := s.kipp("run", "plain", "--wait")
	eq(t, "exit: "+errOut, code, 0)
	contains(t, out, "[s] url="+s.url+"\n")
}

// What serve creates, and nothing else does: the first run is kipp serve,
// then kipp user add against the same --data.
func TestOnlyServeCreatesTheDataDirectoryAndItIsItsOwnersAlone(t *testing.T) {
	data := filepath.Join(t.TempDir(), "fresh", "data")
	for _, args := range [][]string{{"user", "add", "admin", "--admin"}, {"user", "list"}, {"token", "list", "admin"}, {"secret", "list"}, {"secret", "set", "NAME"}} {
		code, _, errOut := kipp("a value long enough\n", append(args, "--data", data)...)
		eq(t, strings.Join(args, " ")+" before any server", code, 1)
		contains(t, errOut, data, "kipp serve")
	}
	if _, err := os.Stat(filepath.Dir(data)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a command that is not serve made something: %v", err)
	}
	// A directory that is there and is not a server's stays empty.
	empty := t.TempDir()
	code, _, errOut := kipp("", "user", "add", "admin", "--admin", "--data", empty)
	eq(t, "user add in an empty directory", code, 1)
	contains(t, errOut, empty, "kipp serve")
	left, err := os.ReadDir(empty)
	must(t, err)
	eq(t, "files a command that is not serve left", len(left), 0)

	s := &served{t: t, stderr: &syncBuffer{}, data: data, jobs: t.TempDir()}
	write(t, filepath.Join(s.jobs, "plain.yml"), "name: plain\nstages:\n  s: {run: 'echo ran'}\n", 0o600)
	s.serve()
	lacks(t, s.stderr.String(), "group or others")
	for name, mode := range map[string]os.FileMode{".": 0o700, "db.sqlite": 0o600, "secret.key": 0o600, "device.key": 0o600, "lock": 0o600} {
		info, err := os.Stat(filepath.Join(data, name))
		must(t, err)
		eq(t, "mode of "+name, info.Mode().Perm(), mode)
	}
	ok(t, adminPassword+"\n", "user", "add", "admin", "--admin", "--password-stdin", "--data", data)
	out := ok(t, "", "token", "create", "admin", "--label", "first", "--data", data)
	s.token = strings.TrimSpace(out[strings.LastIndex(out, "kipp_"):])
	code, out, errOut = s.kipp("run", "plain", "--wait")
	eq(t, "the first build: "+errOut, code, 0)
	contains(t, out, "[s] ran\n")
}

func TestADatabaseOrADataDirectoryOthersCanReadIsNotUsedQuietly(t *testing.T) {
	data, jobs, _ := dataAndJobs(t, map[string]string{"plain.yml": "name: plain\nstages:\n  s: {run: 'true'}\n"})
	db := filepath.Join(data, "db.sqlite")
	must(t, os.Chmod(db, 0o640))
	code, _, errOut := kipp("", "user", "list", "--data", data)
	eq(t, "a command", code, 1)
	contains(t, errOut, db, "group or others", "chmod 600")
	var out, serveErr syncBuffer
	eq(t, "serve", serve(soon(t), []string{"--data", data, "--jobs", jobs, "--listen", "127.0.0.1:0"}, &out, &serveErr), 1)
	contains(t, serveErr.String(), db, "group or others", "chmod 600")
	lacks(t, out.String(), "listening")

	// A directory that was there before is the operator's; serve says so
	// and does not change it.
	must(t, os.Chmod(db, 0o600))
	must(t, os.Chmod(data, 0o755))
	s := &served{t: t, stderr: &syncBuffer{}, data: data, jobs: jobs}
	s.serve()
	contains(t, s.stderr.String(), "level=WARN", "group or others", "chmod 700", data)
	info, err := os.Stat(data)
	must(t, err)
	eq(t, "the directory's mode", info.Mode().Perm(), os.FileMode(0o755))
}

func TestADeviceKeyOthersCanReadStopsTheServer(t *testing.T) {
	data, jobs, _ := dataAndJobs(t, map[string]string{"plain.yml": "name: plain\nstages:\n  s: {run: 'true'}\n"})
	key := filepath.Join(data, "device.key")
	write(t, key, strings.Repeat("k", 32), 0o644)
	must(t, os.Chmod(key, 0o644))
	var out, serveErr syncBuffer
	eq(t, "serve", serve(soon(t), []string{"--data", data, "--jobs", jobs, "--listen", "127.0.0.1:0"}, &out, &serveErr), 1)
	contains(t, serveErr.String(), key, "chmod 600")
	lacks(t, out.String(), "listening")
}

// redirecting answers every request with a redirect to target.
func redirecting(t *testing.T, status int, target string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target+r.URL.Path, status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A redirect followed would carry the token to wherever it leads (another
// port, or http after https), and a 301 would turn the POST into a GET.
func TestTheClientFollowsNoRedirectAndSaysWhereItLed(t *testing.T) {
	var asked atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { asked.Add(1) }))
	defer elsewhere.Close()
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		from := redirecting(t, status, elsewhere.URL)
		for _, args := range [][]string{{"run", "hello"}, {"run", "hello", "--wait"}, {"reload"}} {
			code, out, errOut := kipp("", append(args, "--url", from.URL, "--token", "kipp_secret_token")...)
			what := strconv.Itoa(status) + " " + strings.Join(args, " ")
			eq(t, what, code, 1)
			eq(t, what+" stdout", out, "")
			contains(t, errOut, "redirect", elsewhere.URL+"/api/")
			lacks(t, errOut, "kipp_secret_token", "trying again")
		}
	}
	eq(t, "requests that followed a redirect", asked.Load(), 0)

	// A redirect met while a build is followed is no reason to ask again.
	var polls atomic.Int32
	later := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":1,"job":"j","number":1,"status":"queued"}`)
			return
		}
		polls.Add(1)
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusFound)
	}))
	defer later.Close()
	code, _, errOut := kipp("", "run", "j", "--wait", "--url", later.URL, "--token", "kipp_secret_token")
	eq(t, "a redirect while following", code, 4)
	contains(t, errOut, "redirect", elsewhere.URL+"/api/v1/builds/1", "build 1 may still be running", later.URL+"/jobs/j/1")
	eq(t, "lines on stderr: "+errOut, strings.Count(errOut, "\n"), 1)
	lacks(t, errOut, "asking again")
	eq(t, "polls", polls.Load(), 1)
	eq(t, "requests that followed a redirect", asked.Load(), 0)
}

// flaky is a server whose build of job j runs until poll number last,
// counted from 1, and has then succeeded; the polls fail says fail.
func flaky(t *testing.T, last int32, fail func(poll int32) bool) *httptest.Server {
	t.Helper()
	var polls, broken atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":1,"job":"j","number":1,"status":"queued"}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/log") {
			w.Header().Set("X-Kipp-Offset", "0")
			w.Header().Set("X-Kipp-More", "false")
			return
		}
		poll := polls.Add(1)
		switch {
		case !fail(poll) && poll < last:
			_, _ = io.WriteString(w, `{"id":1,"job":"j","number":1,"status":"running","stages":[{"name":"s","status":"running"}]}`)
		case !fail(poll):
			_, _ = io.WriteString(w, `{"id":1,"job":"j","number":1,"status":"success","stages":[{"name":"s","status":"success"}]}`)
		case broken.Add(1)%2 == 1: // every other failure is an answer, the rest a connection cut
			w.WriteHeader(http.StatusBadGateway)
		default:
			// Cut in the middle of the answer: the client's transport sends
			// again by itself a request it knows was never answered at all.
			w.Header().Set("Content-Length", "100")
			_, _ = io.WriteString(w, `{"id":1,`)
			_ = http.NewResponseController(w).Flush()
			panic(http.ErrAbortHandler)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A gate reads exit 1 as a build that failed; a server restarting under
// the poll is not that.
func TestRunWaitAsksAgainWhenTheServerDoesNotAnswer(t *testing.T) {
	retryFirst = time.Millisecond
	t.Cleanup(func() { retryFirst = time.Second })

	twice := flaky(t, 4, func(poll int32) bool { return poll == 2 || poll == 3 })
	code, out, errOut := kipp("", "run", "j", "--wait", "--url", twice.URL, "--token", "kipp_t")
	eq(t, "exit after two failures: "+errOut, code, 0)
	eq(t, "stdout", out, "j #1 queued (build 1)\nj #1: success\n")
	eq(t, "what stderr says, once for each failure: "+errOut, strings.Count(errOut, "asking again"), 2)
	contains(t, errOut, "502 Bad Gateway", "the build goes on")

	// Failures apart from each other are each forgiven: the count starts
	// over with every answer.
	var total atomic.Int32
	apart := flaky(t, 4*retries+1, func(poll int32) bool { total.Store(poll); return poll%2 == 0 })
	code, _, errOut = kipp("", "run", "j", "--wait", "--url", apart.URL, "--token", "kipp_t")
	eq(t, "exit after many failures apart: "+errOut, code, 0)
	eq(t, "polls", total.Load(), 4*retries+1)

	var asked atomic.Int32
	down := flaky(t, 2, func(poll int32) bool { asked.Store(poll); return true })
	code, out, errOut = kipp("", "run", "j", "--wait", "--url", down.URL, "--token", "kipp_t")
	eq(t, "exit when the server stays away", code, 4)
	eq(t, "stdout", out, "j #1 queued (build 1)\n")
	contains(t, errOut, "gave up", "build 1 may still be running", down.URL+"/jobs/j/1")
	eq(t, "what stderr says before it gives up", strings.Count(errOut, "asking again"), retries)
	eq(t, "polls before it gives up", asked.Load(), retries+1)
}

// polled is a server that queues build 1 of job j and answers every poll
// with answer; it counts the polls.
func polled(t *testing.T, answer http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	polls := &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":1,"job":"j","number":1,"status":"queued"}`)
			return
		}
		polls.Add(1)
		answer(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, polls
}

// What the server refuses is not asked again. A token refused is 3 at any
// time; a build that cannot be found any more has a fate nobody learned.
func TestRunWaitDoesNotAskAgainForWhatTheServerRefuses(t *testing.T) {
	for status, c := range map[int]struct {
		code int
		says []string
	}{
		http.StatusNotFound:     {4, []string{"kipp run: not found; build 1 may still be running", "/jobs/j/1"}},
		http.StatusUnauthorized: {3, []string{"kipp run: the server refused the token: login or a token is required\n"}},
		http.StatusForbidden:    {3, []string{"kipp run: the server refused the token: 403 Forbidden\n"}},
	} {
		body := map[int]string{http.StatusNotFound: `{"detail":"not found"}`, http.StatusUnauthorized: `{"detail":"login or a token is required"}`}[status]
		srv, polls := polled(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		})
		code, out, errOut := kipp("", "run", "j", "--wait", "--url", srv.URL, "--token", "kipp_t")
		eq(t, strconv.Itoa(status)+" exit", code, c.code)
		eq(t, "stdout", out, "j #1 queued (build 1)\n")
		contains(t, errOut, c.says...)
		eq(t, strconv.Itoa(status)+" lines on stderr: "+errOut, strings.Count(errOut, "\n"), 1)
		eq(t, strconv.Itoa(status)+" polls", polls.Load(), 1)
	}
}

// Once the build is known to have ended, the exit status is its fate, also
// when the rest of its output could not be read: 4 is only for an end
// nobody learned.
func TestRunWaitExitsByAStatusItKnowsEvenWhenTheLogsCannotBeRead(t *testing.T) {
	retryFirst = time.Millisecond
	t.Cleanup(func() { retryFirst = time.Second })
	for what, c := range map[string]struct {
		status string
		logs   int // what the log endpoint answers
		code   int
	}{
		"failed, the server away":      {"failed", http.StatusBadGateway, 1},
		"success, the server away":     {"success", http.StatusBadGateway, 0},
		"success, the logs gone":       {"success", http.StatusNotFound, 0},
		"cancelled, the token refused": {"cancelled", http.StatusUnauthorized, 1},
	} {
		srv, _ := polled(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/log") {
				w.WriteHeader(c.logs)
				return
			}
			_, _ = io.WriteString(w, `{"id":1,"job":"j","number":1,"status":"`+c.status+`","stages":[{"name":"s","status":"`+c.status+`"}]}`)
		})
		// A trailing slash on --url is not printed twice.
		code, out, errOut := kipp("", "run", "j", "--wait", "--url", srv.URL+"/", "--token", "kipp_t")
		eq(t, what+": exit", code, c.code)
		eq(t, what+": stdout", out, "j #1 queued (build 1)\nj #1: "+c.status+"\n")
		last := errOut[strings.LastIndex(strings.TrimSuffix(errOut, "\n"), "\n")+1:]
		contains(t, last, "kipp run: ", "build 1 ended as "+c.status, "its output is incomplete", srv.URL+"/jobs/j/1\n")
		lacks(t, errOut, "may still be running", srv.URL+"//")
	}
}

// A log read that fails after part of the log was printed goes on from
// where it was: no line twice, none lost.
func TestRunWaitPrintsEachLineOnceAcrossARetry(t *testing.T) {
	retryFirst = time.Millisecond
	t.Cleanup(func() { retryFirst = time.Second })
	const log = "one\ntwo\nthree\n"
	var reads atomic.Int32
	srv, _ := polled(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/stages/s/log") {
			status := "running"
			if reads.Load() > 3 {
				status = "success"
			}
			if strings.HasSuffix(r.URL.Path, "/log") { // clone, prepare, hooks: empty
				w.Header().Set("X-Kipp-Offset", "0")
				w.Header().Set("X-Kipp-More", "false")
				return
			}
			_, _ = io.WriteString(w, `{"id":1,"job":"j","number":1,"status":"`+status+`","stages":[{"name":"s","status":"`+status+`"}]}`)
			return
		}
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		if reads.Add(1) == 2 { // the first read gave "one\n"; this one fails
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"detail":"the proxy lost the server"}`)
			return
		}
		end := min(offset+4, len(log))
		if offset > 4 {
			end = len(log)
		}
		w.Header().Set("X-Kipp-Offset", strconv.Itoa(end))
		w.Header().Set("X-Kipp-More", strconv.FormatBool(end < len(log)))
		_, _ = io.WriteString(w, log[min(offset, len(log)):end])
	})
	code, out, errOut := kipp("", "run", "j", "--wait", "--url", srv.URL, "--token", "kipp_t")
	eq(t, "exit: "+errOut, code, 0)
	eq(t, "stdout", out, "j #1 queued (build 1)\n[s] one\n[s] two\n[s] three\nj #1: success\n")
	// A 5xx that carries the API's own message shows that, not the status line.
	eq(t, "stderr", errOut, "kipp run: no answer from the server: the proxy lost the server; the build goes on, asking again in 1ms\n")
}

func TestRunTakesAParamFromAFileWithEveryLineItHoldsAndAValueAsItIsWritten(t *testing.T) {
	s := start(t, map[string]string{"ask.yml": "name: ask\nparams:\n  REQUEST: {type: text}\n  WHO: {default: world}\nstages:\n  s: {run: 'printf \"%s\" \"$REQUEST\"; echo; echo who=$WHO'}\n"})
	dir := t.TempDir()
	file := filepath.Join(dir, "request.txt")
	write(t, file, "first line\n  second, indented\n\nlast\n", 0o600)
	// -p is the value as written, also when it names a file.
	code, out, errOut := s.kipp("run", "ask", "--param-file", "REQUEST="+file, "-p", "WHO=@"+file, "--wait")
	eq(t, "exit: "+errOut, code, 0)
	eq(t, "stdout", out, "ask #1 queued (build 1)\n[s] first line\n[s]   second, indented\n[s] \n[s] last\n[s] \n[s] who=@"+file+"\nask #1: success\n")

	big := filepath.Join(dir, "big.txt")
	write(t, big, strings.Repeat("x", 64<<10+1), 0o600)
	// Under the limit as a file, over it once it is written as JSON.
	quotes := filepath.Join(dir, "quotes.txt")
	write(t, quotes, strings.Repeat(`"`, 40<<10), 0o600)
	oneLine := filepath.Join(dir, "who.txt")
	write(t, oneLine, "kipp", 0o600)
	missing := filepath.Join(dir, "no-such-file")
	for what, c := range map[string]struct {
		args []string
		code int
		says []string
	}{
		"a file that is not there":       {[]string{"--param-file", "REQUEST=" + missing}, 2, []string{missing}},
		"a directory":                    {[]string{"--param-file", "REQUEST=" + dir}, 2, []string{dir, "directory"}},
		"a file over the request's size": {[]string{"--param-file", "REQUEST=" + big}, 2, []string{"params", "at most 65536 bytes"}},
		"a file that grows over it":      {[]string{"--param-file", "REQUEST=" + quotes}, 2, []string{"params", "at most 65536 bytes"}},
		"a value over it":                {[]string{"-p", "REQUEST=" + strings.Repeat("x", 64<<10)}, 2, []string{"params", "at most 65536 bytes"}},
		"no file named":                  {[]string{"--param-file", "REQUEST"}, 2, []string{"KEY=FILE"}},
		"a key given by both flags":      {[]string{"--param-file", "WHO=" + oneLine, "-p", "WHO=me"}, 2, []string{"WHO", "-p", "--param-file"}},
		"several lines for a string":     {[]string{"--param-file", "WHO=" + file}, 1, []string{"param WHO", "one line"}},
	} {
		code, _, errOut := s.kipp(append([]string{"run", "ask"}, c.args...)...)
		eq(t, what, code, c.code)
		contains(t, errOut, c.says...)
	}
	eq(t, "builds started by what was refused", strings.Count(kippGet(t, s, "/api/v1/jobs/ask/builds"), `"number":`), 1)
	_, _, help := kipp("", "run", "-h")
	contains(t, help, "-param-file", "one line", "exits 4", "could not be learned")
}

// A SIGHUP is a reload for as long as the process lives. One that arrives
// while the builds are being ended must not fall back to the signal's
// default, which would kill the server with a build still marked running
// and its stage alive.
func TestASIGHUPWhileShuttingDownDoesNotKillTheServer(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	// A stage that ignores SIGTERM holds the shutdown for the runner's grace.
	data, jobs, token := dataAndJobs(t, map[string]string{"deaf.yml": "name: deaf\nstages:\n  s: {run: \"trap '' TERM; echo $$ > " + pidFile + ".tmp; mv " + pidFile + ".tmp " + pidFile + "; sleep 60\"}\n"})
	cmd := exec.Command(os.Args[0], "serve", "--data", data, "--jobs", jobs, "--listen", "127.0.0.1:0")
	cmd.Env = append(os.Environ(), asKipp+"=1")
	stderr := &syncBuffer{}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	must(t, err)
	must(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	addr := address(t, stdout, stderr)
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	ok(t, "", "run", "deaf", "--url", addr, "--token", token)
	stage := 0
	eventually(t, "the stage is running", func() bool {
		text, _ := os.ReadFile(pidFile)
		stage, _ = strconv.Atoi(strings.TrimSpace(string(text)))
		return stage > 0
	})
	// Should the server die before it ends the stage, the test does.
	t.Cleanup(func() { _ = syscall.Kill(-stage, syscall.SIGKILL) })

	must(t, cmd.Process.Signal(syscall.SIGTERM))
	eventually(t, "the server is shutting down", func() bool { return strings.Contains(stderr.String(), "shutting down") })
	must(t, cmd.Process.Signal(syscall.SIGHUP))
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("the server did not exit 0: %v\nstderr: %s", err, stderr)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the server was still up 30s after SIGTERM\nstderr: %s", stderr)
	}
	eventually(t, "the stage is gone", func() bool { return errors.Is(syscall.Kill(stage, 0), syscall.ESRCH) })
	builds, err := openDB(t, data).Builds(context.Background(), "deaf", 10)
	must(t, err)
	if len(builds) != 1 || builds[0].Status != store.Lost {
		t.Fatalf("builds: %+v", builds)
	}
}

// A crash, unlike a shutdown, kills nothing the stages started. The server
// that starts next must, before it runs anything: the job's next build
// would otherwise run beside the last one's processes.
func TestAfterACrashTheNextServerKillsWhatTheStagesLeftBeforeItBuildsAgain(t *testing.T) {
	scratch := t.TempDir()
	pids, second := filepath.Join(scratch, "pids"), filepath.Join(scratch, "second")
	// The first build hangs with a child; the second says whether it found
	// either of them alive.
	write(t, filepath.Join(scratch, "stage.sh"), `if [ -e `+second+` ]; then
  read stage child < `+pids+`
  if kill -0 "$stage" 2>/dev/null || kill -0 "$child" 2>/dev/null; then echo beside-the-orphan; else echo alone; fi
  exit 0
fi
touch `+second+`
sleep 300 &
echo "$$ $!" > `+pids+`.tmp
mv `+pids+`.tmp `+pids+`
wait
`, 0o600)
	// exec, so that the script is the stage's own process and its $$ the
	// group's id: whether a shell runs its last command in its own place is
	// the shell's choice, and not every sh makes it.
	data, jobs, token := dataAndJobs(t, map[string]string{"slow.yml": "name: slow\nstages:\n  s: {run: 'exec sh " + filepath.Join(scratch, "stage.sh") + "'}\n"})

	cmd := exec.Command(os.Args[0], "serve", "--data", data, "--jobs", jobs, "--listen", "127.0.0.1:0")
	cmd.Env = append(os.Environ(), asKipp+"=1")
	stderr := &syncBuffer{}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	must(t, err)
	must(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	addr := address(t, stdout, stderr)
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	ok(t, "", "run", "slow", "--url", addr, "--token", token)
	ok(t, "", "run", "slow", "--url", addr, "--token", token) // waits its turn: the job queues
	var stage, child int
	eventually(t, "the stage is running", func() bool {
		text, err := os.ReadFile(pids)
		if err != nil {
			return false
		}
		fields := strings.Fields(string(text))
		stage, _ = strconv.Atoi(fields[0])
		child, _ = strconv.Atoi(fields[1])
		return stage > 0 && child > 0
	})
	// Whatever the test finds, it leaves no process behind: the stage leads
	// its own process group.
	t.Cleanup(func() { _ = syscall.Kill(-stage, syscall.SIGKILL) })
	// The store has no reader for it but the server that starts after a crash.
	db, err := sql.Open("sqlite", "file:"+filepath.Join(data, "db.sqlite"))
	must(t, err)
	eventually(t, "the stage's process group is recorded", func() bool {
		var pgid int
		return db.QueryRow(`SELECT pgid FROM stages WHERE status = 'running'`).Scan(&pgid) == nil && pgid == stage
	})
	must(t, db.Close())

	must(t, cmd.Process.Kill())
	<-exited
	must(t, syscall.Kill(stage, 0))
	must(t, syscall.Kill(child, 0))

	s := &served{t: t, stderr: &syncBuffer{}, data: data, jobs: jobs, token: token}
	s.serve()
	// With no waiting: the server listens only once the orphans are gone.
	for _, pid := range []int{stage, child} {
		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("process %d, left by the crash, is running beside the new server: %v", pid, err)
		}
	}
	contains(t, kippGet(t, s, "/api/v1/builds/1"), `"status":"lost"`)
	contains(t, kippGet(t, s, "/api/v1/builds/2?wait=true"), `"status":"success"`)
	first, err := os.ReadFile(filepath.Join(data, "logs", "slow", "1", "s.log"))
	must(t, err)
	if want := "kipp: the server stopped while this ran; what it had left running was killed when the server started again\n"; !strings.HasSuffix(string(first), want) {
		t.Fatalf("the lost build's log ends %q, want %q", first, want)
	}
	again, err := os.ReadFile(filepath.Join(data, "logs", "slow", "2", "s.log"))
	must(t, err)
	eq(t, "what the next build found", string(again), "alone\n")
}

func TestADisabledJobPassesCheckAndIsNotRun(t *testing.T) {
	s := start(t, map[string]string{"off.yml": "name: off\ndisabled: true\ntriggers: {cron: '* * * * *'}\nstages:\n  s: {run: 'true'}\n"})
	eq(t, "check of one job", ok(t, "", "check", s.jobs), "ok: 1 job\n")
	code, out, errOut := s.kipp("run", "off", "--wait")
	eq(t, "running a disabled job", code, 1)
	contains(t, errOut, "the job is disabled")
	lacks(t, out, "queued")
}

func TestASealedValueMovedToAnotherSecretsRowIsNotGivenToABuild(t *testing.T) {
	s := &served{t: t, stderr: &syncBuffer{}}
	s.data, s.jobs, s.token = dataAndJobs(t, map[string]string{"hello.yml": helloJob})
	const other = "the value of another secret"
	ok(t, other, "secret", "set", "OTHER", "--data", s.data)
	st := openDB(t, s.data)
	sealed, err := st.Secret(context.Background(), "OTHER")
	must(t, err)
	must(t, st.PutSecret(context.Background(), "API_KEY", sealed))
	s.serve()
	code, out, errOut := s.kipp("run", "hello", "--wait")
	eq(t, "a build whose secret is another's sealed value", code, 1)
	contains(t, out, "[prepare] ", "secret API_KEY does not open")
	lacks(t, out+errOut, other)
}

func TestRunWaitExitsZeroForAnUnstableBuildAndNamesItsUnstableStages(t *testing.T) {
	s := start(t, map[string]string{"shaky.yml": `name: shaky
stages:
  lint: {run: 'echo warned; exit 4', unstable_exit: [4]}
  unit: {needs: [lint], run: 'echo fine'}
  docs: {needs: [unit], run: 'exit 9', unstable_exit: [8, 9]}
`})
	code, out, errOut := s.kipp("run", "shaky", "--wait")
	eq(t, "exit", code, 0)
	eq(t, "stdout", out, "shaky #1 queued (build 1)\n[lint] warned\n[unit] fine\nshaky #1: unstable\n")
	eq(t, "stderr", errOut, "kipp run: shaky #1 ended unstable; unstable stages: lint, docs\n")
}

// The exit status is by the status alone, each one listed: only a success
// and an unstable build are 0, and a status this command has never heard
// of is not taken for either.
func TestRunWaitExitsByEachStatusABuildCanEndWith(t *testing.T) {
	for status, want := range map[string]int{
		"success": 0, "unstable": 0, "failed": 1, "cancelled": 1, "timeout": 1, "lost": 1, "a-status-of-the-future": 1,
	} {
		srv, _ := polled(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/log") {
				w.Header().Set("X-Kipp-Offset", "0")
				w.Header().Set("X-Kipp-More", "false")
				return
			}
			_, _ = io.WriteString(w, `{"id":1,"job":"j","number":1,"status":"`+status+`","stages":[{"name":"a","status":"success"},{"name":"b","status":"`+status+`"}]}`)
		})
		code, out, errOut := kipp("", "run", "j", "--wait", "--url", srv.URL, "--token", "kipp_t")
		eq(t, status+": exit", code, want)
		eq(t, status+": stdout", out, "j #1 queued (build 1)\nj #1: "+status+"\n")
		said := ""
		if status == "unstable" {
			said = "kipp run: j #1 ended unstable; unstable stages: b\n"
		}
		eq(t, status+": stderr", errOut, said)
	}
}

// An unstable build whose logs could not be read is still 0, and stderr
// says both things.
func TestRunWaitExitsZeroForAnUnstableBuildEvenWhenTheLogsCannotBeRead(t *testing.T) {
	retryFirst = time.Millisecond
	t.Cleanup(func() { retryFirst = time.Second })
	srv, _ := polled(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/log") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"id":1,"job":"j","number":1,"status":"unstable","stages":[{"name":"s","status":"unstable"}]}`)
	})
	code, out, errOut := kipp("", "run", "j", "--wait", "--url", srv.URL, "--token", "kipp_t")
	eq(t, "exit", code, 0)
	eq(t, "stdout", out, "j #1 queued (build 1)\nj #1: unstable\n")
	contains(t, errOut, "build 1 ended as unstable and its output is incomplete", "kipp run: j #1 ended unstable; unstable stages: s\n")
}

func TestRunsHelpSaysAnUnstableBuildExitsZero(t *testing.T) {
	_, out, errOut := kipp("", "run", "-h")
	contains(t, out+errOut, "0 if the build succeeds or ends unstable")
}

func TestRunWaitNamesNoStagesWhenTheReplyOfAnUnstableBuildListsNoneUnstable(t *testing.T) {
	srv, _ := polled(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/log") {
			w.Header().Set("X-Kipp-Offset", "0")
			w.Header().Set("X-Kipp-More", "false")
			return
		}
		_, _ = io.WriteString(w, `{"id":1,"job":"j","number":1,"status":"unstable","stages":[{"name":"s","status":"success"}]}`)
	})
	code, _, errOut := kipp("", "run", "j", "--wait", "--url", srv.URL, "--token", "kipp_t")
	eq(t, "exit", code, 0)
	eq(t, "stderr", errOut, "kipp run: j #1 ended unstable\n")
}

func TestServesTimeZoneIsTheOneAJobWithNoneOfItsOwnIsReadIn(t *testing.T) {
	s := &served{t: t, stderr: &syncBuffer{}, args: []string{"--timezone", "Asia/Tokyo"}}
	s.data, s.jobs, s.token = dataAndJobs(t, map[string]string{
		"kipps.yml": "name: kipps\ntriggers: {cron: '0 9 * * *'}\nstages:\n  s: {run: 'true'}\n",
		"own.yml":   "name: own\ntimezone: Europe/Paris\ntriggers: {cron: '0 9 * * *'}\nstages:\n  s: {run: 'true'}\n",
	})
	s.serve()
	contains(t, kippGet(t, s, "/api/v1/jobs/kipps"), `"timezone":"Asia/Tokyo"`)
	contains(t, kippGet(t, s, "/api/v1/jobs/own"), `"timezone":"Europe/Paris"`)
}

func TestWithNoTimeZoneGivenServeNamesTheHosts(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "localtime")
	must(t, os.Symlink("/var/db/timezone/zoneinfo/Asia/Tokyo", link))
	for what, c := range map[string]struct {
		tz        string
		set       bool
		localtime string
		want      string
	}{
		"TZ names one":                 {"Europe/Paris", true, link, "Europe/Paris"},
		"TZ with the colon POSIX lets": {":Europe/Paris", true, link, "Europe/Paris"},
		"no TZ, the localtime link":    {"", false, link, "Asia/Tokyo"},
		"no TZ, no link":               {"", false, filepath.Join(dir, "none"), ""},
		"a TZ that is no zone":         {"Mars/Olympus", true, link, ""},
	} {
		z := hostZone(c.tz, c.set, c.localtime)
		got := ""
		if z != nil {
			got = z.String()
		}
		eq(t, what, got, c.want)
	}
}

func TestTheSweepIsAtHalfPastThreeInTheServersZone(t *testing.T) {
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	must(t, err)
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC) // 09:00 in Tokyo
	got := nextSweep(now.In(tokyo))
	eq(t, "sweep", got.In(tokyo).Format("2006-01-02 15:04"), "2026-10-12 03:30")
}

// setSchema writes the database's schema version as a kipp of another
// release would have left it.
func setSchema(t *testing.T, data, query string) string {
	t.Helper()
	path := filepath.Join(data, "db.sqlite")
	db, err := sql.Open("sqlite", "file:"+path)
	must(t, err)
	_, err = db.Exec(query)
	must(t, err)
	must(t, db.Close())
	return path
}

// The commands beside a server may run under a server of the kipp
// before, so only serve upgrades: the others name it and change nothing.
func TestOnlyServeUpgradesTheDatabase(t *testing.T) {
	data := newData(t)
	db := setSchema(t, data, `DELETE FROM schema WHERE version = (SELECT MAX(version) FROM schema)`)
	for _, args := range [][]string{{"user", "list"}, {"secret", "list"}, {"user", "add", "ann"}} {
		code, _, errOut := kipp("", append(args, "--data", data)...)
		eq(t, strings.Join(args, " ")+" on an older schema", code, 1)
		contains(t, errOut, db, "start this kipp's serve on it")
	}
	st, err := sql.Open("sqlite", "file:"+db)
	must(t, err)
	var users int
	must(t, st.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users))
	must(t, st.Close())
	eq(t, "users made on an older schema", users, 0)
}

func TestADatabaseOfANewerKippIsRefusedByEveryCommandAndServe(t *testing.T) {
	data, jobs, _ := dataAndJobs(t, map[string]string{"plain.yml": "name: plain\nstages:\n  s: {run: 'true'}\n"})
	db := setSchema(t, data, `INSERT INTO schema (version) VALUES (1000)`)
	code, _, errOut := kipp("", "user", "list", "--data", data)
	eq(t, "a command", code, 1)
	contains(t, errOut, db, "a newer kipp upgraded it")
	var out, serveErr syncBuffer
	eq(t, "serve", serve(soon(t), []string{"--data", data, "--jobs", jobs, "--listen", "127.0.0.1:0"}, &out, &serveErr), 1)
	contains(t, serveErr.String(), db, "at schema 1000", "a newer kipp upgraded it")
	lacks(t, out.String(), "listening")
}

func TestAnUpgradeIsLoggedWithWhereTheCopyIs(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	logUpgrade(log, store.Upgrade{From: 0, To: 3})
	logUpgrade(log, store.Upgrade{From: 3, To: 3})
	eq(t, "the log of a new or an up-to-date database", buf.String(), "")
	logUpgrade(log, store.Upgrade{From: 2, To: 3, Backup: "/data/db.schema-2.sqlite"})
	contains(t, buf.String(), "database upgraded", "from_schema=2", "to_schema=3", "copy=/data/db.schema-2.sqlite")
}

func TestTheServiceIsGivenWhatServeRunsWithAndSealsWithItsKey(t *testing.T) {
	ctx := context.Background()
	data, jobs, _ := dataAndJobs(t, map[string]string{"hello.yml": helloJob})
	f, err := parseServeFlags([]string{"--data", data, "--jobs", jobs, "--listen", "127.0.0.1:0", "--executors", "3", "--shell", "bash", "--timezone", "Europe/Paris"}, io.Discard)
	must(t, err)
	s := &server{f: f, log: slog.New(slog.DiscardHandler)}
	ln, err := s.open(ctx)
	must(t, err)
	t.Cleanup(func() { _ = ln.Close(); s.close() })
	d := s.deps(auth.New(s.st, time.Hour, s.deviceKey))
	eq(t, "the address the API is told it listens on", d.Listen, ln.Addr().String())
	eq(t, "the flips that make a job flaky, by default", d.FlakyFlips, 4)

	info := d.ServerInfo()
	abs, err := filepath.Abs(data)
	must(t, err)
	if info.Version != version.Version || info.GoVersion != runtime.Version() || info.Schema < 1 || info.StartedAt.IsZero() ||
		info.Listen != ln.Addr().String() || info.BaseURL != "http://"+ln.Addr().String() || info.Executors != 3 || info.Shell != "bash" ||
		info.SessionTTL != 30*24*time.Hour || info.TimeZone != "Europe/Paris" || info.DataDir != abs || info.JobsDir != jobs ||
		info.FreeDisk == 0 || !info.FreeDiskKnown || info.DBSize <= 0 || info.LastReloadAt.IsZero() || info.LastReloadError != "" {
		t.Fatalf("%+v", info)
	}
	write(t, filepath.Join(jobs, "wrong.yml"), "name: wrong\n", 0o600)
	if s.reload(ctx) == nil {
		t.Fatal("a bad set was taken")
	}
	after := d.ServerInfo()
	if !after.LastReloadAt.After(info.LastReloadAt) || !strings.Contains(after.LastReloadError, "wrong.yml") {
		t.Fatalf("after a refused reload: %v %q", after.LastReloadAt, after.LastReloadError)
	}

	sealed, err := d.Seal("API_KEY", []byte("an obvious test value"))
	must(t, err)
	must(t, s.st.PutSecret(ctx, "API_KEY", sealed))
	opened, err := s.secret(ctx, "API_KEY")
	must(t, err)
	eq(t, "the secret serve opens", opened, "an obvious test value")
}

// The webhook asks for the secret of every name it is sent: one that is
// not set must cost the open a set one costs, or the time of the answer
// tells which jobs have a webhook.
func TestASecretThatIsNotSetIsAnsweredAfterTheSameOpenAsOneThatIs(t *testing.T) {
	ctx := context.Background()
	data, jobs, _ := dataAndJobs(t, map[string]string{"hello.yml": helloJob})
	f, err := parseServeFlags([]string{"--data", data, "--jobs", jobs, "--listen", "127.0.0.1:0"}, io.Discard)
	must(t, err)
	s := &server{f: f, log: slog.New(slog.DiscardHandler)}
	ln, err := s.open(ctx)
	must(t, err)
	t.Cleanup(func() { _ = ln.Close(); s.close() })
	sealed, err := secretbox.Seal(s.key, "GITHUB_WEBHOOK_HELLO", []byte("an obvious test value"))
	must(t, err)
	must(t, s.st.PutSecret(ctx, "GITHUB_WEBHOOK_HELLO", sealed))

	var opened []error
	real := openSecret
	t.Cleanup(func() { openSecret = real })
	openSecret = func(key []byte, name string, sealed []byte) ([]byte, error) {
		v, err := real(key, name, sealed)
		opened = append(opened, err)
		return v, err
	}
	if _, err := s.secret(ctx, "GITHUB_WEBHOOK_HELLO"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.secret(ctx, "GITHUB_WEBHOOK_NONE"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	if len(opened) != 2 || opened[0] != nil || opened[1] != nil {
		t.Fatalf("opens %v, want one that succeeds for each", opened)
	}
}

// Scope is what a stage receives: a secret kipp reads for itself, a hook's
// signature or a token, is used whatever its scope.
func TestAServerReadsASecretWhateverItsScope(t *testing.T) {
	ctx := context.Background()
	data, jobs, _ := dataAndJobs(t, map[string]string{"hello.yml": helloJob})
	f, err := parseServeFlags([]string{"--data", data, "--jobs", jobs, "--listen", "127.0.0.1:0"}, io.Discard)
	must(t, err)
	s := &server{f: f, log: slog.New(slog.DiscardHandler)}
	ln, err := s.open(ctx)
	must(t, err)
	t.Cleanup(func() { _ = ln.Close(); s.close() })
	sealed, err := secretbox.Seal(s.key, "GITHUB_WEBHOOK_HELLO", []byte("an obvious test value"))
	must(t, err)
	must(t, s.st.PutSecretKind(ctx, "GITHUB_WEBHOOK_HELLO", "text", "", sealed, []string{"job:no-such-job"}))
	got, err := s.secret(ctx, "GITHUB_WEBHOOK_HELLO")
	must(t, err)
	eq(t, "the webhook secret", got, "an obvious test value")
	rec, err := s.openSecretRecord(ctx, "GITHUB_WEBHOOK_HELLO")
	must(t, err)
	eq(t, "its scopes travel with it", strings.Join(rec.Scopes, ","), "job:no-such-job")
}
