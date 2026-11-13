package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/ndthuan/kipp/internal/auth"
)

// tokenIn is the token a command printed once.
func tokenIn(t *testing.T, out string) string {
	t.Helper()
	i := strings.LastIndex(out, "kippa_")
	if i < 0 {
		t.Fatalf("no token in %q", out)
	}
	return strings.TrimSpace(out[i:])
}

func TestAnRunnersTokenIsShownOnceAndOnlyItsHashKept(t *testing.T) {
	data := newData(t)
	out := ok(t, "", "runner", "add", "build-1", "--labels", "linux,amd64", "--executors", "2", "--data", data)
	contains(t, out, "token for runner build-1, shown once:\n")
	token := tokenIn(t, out)
	st := openDB(t, data)
	a, err := st.RunnerByTokenHash(context.Background(), auth.HashToken(token))
	must(t, err)
	eq(t, "labels", strings.Join(a.Labels, ","), "linux,amd64")
	eq(t, "executors", a.Executors, 2)
	eq(t, "enabled", a.Enabled, true)
	lacks(t, ok(t, "", "runner", "ls", "--data", data), token)
	code, _, errOut := kipp("", "runner", "add", "build-1", "--labels", "linux", "--data", data)
	eq(t, "a second add", code, 2)
	contains(t, errOut, "kipp runner set changes it")
}

func TestANewTokenForAnRunnerEndsTheOldOne(t *testing.T) {
	data := newData(t)
	old := tokenIn(t, ok(t, "", "runner", "add", "a", "--labels", "linux", "--data", data))
	fresh := tokenIn(t, ok(t, "", "runner", "token", "a", "--data", data))
	if old == fresh {
		t.Fatal("the same token again")
	}
	st := openDB(t, data)
	if _, err := st.RunnerByTokenHash(context.Background(), auth.HashToken(old)); err == nil {
		t.Fatal("the old token still finds the runner")
	}
	if _, err := st.RunnerByTokenHash(context.Background(), auth.HashToken(fresh)); err != nil {
		t.Fatal(err)
	}
}

func TestAnRunnerIsDrainedChangedAndRemovedOnTheDataDirectory(t *testing.T) {
	data := newData(t)
	ok(t, "", "runner", "add", "a", "--labels", "linux", "--data", data)
	ok(t, "", "runner", "set", "a", "--disable", "--executors", "3", "--data", data)
	contains(t, ok(t, "", "runner", "ls", "--data", data), "a\tlinux\texecutors 3\tdisabled\tnever seen")
	ok(t, "", "runner", "set", "a", "--enable", "--labels", "linux,arm64", "--data", data)
	contains(t, ok(t, "", "runner", "ls", "--data", data), "a\tlinux,arm64\texecutors 3\tenabled")
	ok(t, "", "runner", "rm", "a", "--data", data)
	eq(t, "ls", ok(t, "", "runner", "ls", "--data", data), "")
	code, _, errOut := kipp("", "runner", "rm", "a", "--data", data)
	eq(t, "rm again", code, 1)
	contains(t, errOut, "no runner called a")
	for args, want := range map[string]string{
		"runner add b --data D":                              "runner add needs --labels",
		"runner add B --labels linux --data D":               `runner "B"`,
		"runner add b --labels Linux --data D":               `--labels label "Linux"`,
		"runner add b --labels linux --executors 0 --data D": "executors is at least 1",
	} {
		code, _, errOut := kipp("", strings.Fields(strings.ReplaceAll(args, " D", " "+data))...)
		eq(t, args, code, 2)
		contains(t, errOut, want)
	}
}

func TestAnEngineIsOneKippCanDialAndChooseByLabel(t *testing.T) {
	data := newData(t)
	ok(t, "", "engine", "add", "mac", "--docker", "ssh://builder@192.0.2.1?socket=/run/user/1000/docker.sock", "--labels", "linux,arm64", "--max", "2", "--data", data)
	contains(t, ok(t, "", "engine", "ls", "--data", data), "mac\tdocker\tssh://builder@192.0.2.1?socket=/run/user/1000/docker.sock\tlinux,arm64\tmax 2\tenabled")
	for args, want := range map[string]string{
		"engine add e --docker tcp://host:2376 --labels linux --data D":    "a tcp engine needs --tls-ca, --tls-cert and --tls-key",
		"engine add e --docker ssh://me:pw@host --labels linux --data D":   "an engine URL names no password",
		"engine add e --docker http://host --labels linux --data D":        "unix:///path/to/docker.sock",
		"engine add e --docker unix:///d.sock --data D":                    "engine add needs --docker and --labels",
		"engine add e --docker unix:///d.sock --labels l --max 0 --data D": "max is at least 1",
		"engine add mac --docker unix:///d.sock --labels linux --data D":   "kipp engine set changes it",
		"engine set mac --enable --disable --data D":                       "--disable or --enable, not both",
	} {
		code, _, errOut := kipp("", strings.Fields(strings.ReplaceAll(args, " D", " "+data))...)
		eq(t, args, code, 2)
		contains(t, errOut, want)
	}
	ok(t, "", "engine", "set", "mac", "--max", "1", "--disable", "--data", data)
	contains(t, ok(t, "", "engine", "ls", "--data", data), "linux,arm64\tmax 1\tdisabled")
	ok(t, "", "engine", "rm", "mac", "--data", data)
	eq(t, "ls", ok(t, "", "engine", "ls", "--data", data), "")
}

func TestEveryChangeToAnEngineOrAnRunnerIsAudited(t *testing.T) {
	data := newData(t)
	ok(t, "", "engine", "add", "mac", "--docker", "unix:///var/run/docker.sock", "--labels", "linux", "--data", data)
	ok(t, "", "engine", "set", "mac", "--max", "3", "--data", data)
	ok(t, "", "engine", "rm", "mac", "--data", data)
	token := tokenIn(t, ok(t, "", "runner", "add", "a", "--labels", "linux", "--data", data))
	ok(t, "", "runner", "set", "a", "--disable", "--data", data)
	fresh := tokenIn(t, ok(t, "", "runner", "token", "a", "--data", data))
	ok(t, "", "runner", "rm", "a", "--data", data)
	got := strings.Join(audit(t, openDB(t, data)), "\n")
	want := strings.Join([]string{
		"cli engine.add mac: unix:///var/run/docker.sock, labels linux, max 1, enabled",
		"cli engine.set mac: unix:///var/run/docker.sock, labels linux, max 3, enabled",
		"cli engine.rm mac: ",
		"cli runner.add a: labels linux, executors 1, enabled",
		"cli runner.set a: labels linux, executors 1, disabled",
		"cli runner.token a: the old token no longer works",
		"cli runner.rm a: ",
	}, "\n")
	eq(t, "the audit", got, want)
	lacks(t, got, token)
	lacks(t, got, fresh)
}

func TestKippRunnerWithNoChangeToMakeRunsAnRunner(t *testing.T) {
	code, _, errOut := kipp("", "runner", "--url", "http://127.0.0.1:1", "--workdir", t.TempDir(), "--token", "kippa_x", "--bogus")
	eq(t, "a bad flag", code, 2)
	contains(t, errOut, "bogus")
	code, _, errOut = kipp("", "runner")
	eq(t, "nothing", code, 2)
	contains(t, errOut, "usage: kipp runner")
}

// A server whose /runner/ is on a listener of its own serves it there and
// nowhere else, and serves nothing else there.
func TestTheRunnerListenerServesOnlyTheRunnersCalls(t *testing.T) {
	s := &served{t: t, stderr: &syncBuffer{}, args: []string{"--runner-listen", "127.0.0.1:0"}}
	s.data, s.jobs, s.token = dataAndJobs(t, map[string]string{"plain.yml": "name: plain\nstages:\n  s: {run: 'true'}\n"})
	s.serve()
	var info struct {
		RunnerListen string `json:"runner_listen"`
	}
	must(t, json.Unmarshal([]byte(kippGet(t, s, "/api/v1/admin/server")), &info))
	addr := info.RunnerListen
	for path, want := range map[string]int{"/": http.StatusNotFound, "/api/v1/admin/server": http.StatusNotFound, "/api/v1/runner/claim": http.StatusUnauthorized} {
		res, err := http.Get("http://" + addr + path)
		must(t, err)
		_ = res.Body.Close()
		eq(t, "runner listener "+path, res.StatusCode, want)
	}
	code, _ := s.get("/api/v1/runner/claim", nil)
	eq(t, "/runner/ on the main listener", code, http.StatusNotFound)
}

func TestAServerWithNoListenerOfItsOwnForRunnersServesThemOnItsAddress(t *testing.T) {
	s := start(t, map[string]string{"plain.yml": "name: plain\nstages:\n  s: {run: 'true'}\n"})
	code, _ := s.get("/api/v1/runner/claim", nil)
	eq(t, "/runner/claim with no token", code, http.StatusUnauthorized)
}

func TestAReloadReadsTheEnginesAndTheRunners(t *testing.T) {
	s := start(t, map[string]string{"plain.yml": "name: plain\nstages:\n  s: {run: 'true'}\n"})
	ok(t, "", "engine", "add", "mac", "--docker", "unix:///no/such.sock", "--labels", "linux", "--data", s.data)
	ok(t, "", "runner", "add", "a", "--labels", "linux", "--data", s.data)
	must(t, syscall.Kill(os.Getpid(), syscall.SIGHUP))
	eventually(t, "the reload read them", func() bool {
		return strings.Contains(s.stderr.String(), `msg="engines reloaded" count=1 runners=1`)
	})
}

func TestAServerOfNoExecutorsSaysSoAndWhereItsStagesGo(t *testing.T) {
	s := &served{t: t, stderr: &syncBuffer{}, args: []string{"--executors", "0", "--default-runs-on", "linux", "--runner-env", "KIPP_DRY"}}
	s.data, s.jobs, s.token = dataAndJobs(t, map[string]string{"plain.yml": "name: plain\nstages:\n  s: {run: 'true'}\n"})
	s.serve()
	info := kippGet(t, s, "/api/v1/admin/server")
	for _, want := range []string{`"executors":0`, `"default_runs_on":["linux"]`, `"runner_env":["KIPP_DRY"]`, `"runner_url":"` + s.url + `"`} {
		contains(t, info, want)
	}
}

// The cache is reached with a build's token, on the main listener and on
// the runners' alike, and with none it answers 401 on both.
func TestTheCacheIsOnTheMainListenerAndTheRunnersOne(t *testing.T) {
	s := &served{t: t, stderr: &syncBuffer{}, args: []string{"--runner-listen", "127.0.0.1:0"}}
	s.data, s.jobs, s.token = dataAndJobs(t, map[string]string{"plain.yml": "name: plain\nstages:\n  s: {run: 'true'}\n"})
	s.serve()
	var info struct {
		RunnerListen string `json:"runner_listen"`
	}
	must(t, json.Unmarshal([]byte(kippGet(t, s, "/api/v1/admin/server")), &info))
	const path = "/api/v1/cache/site-smoke/lookup?key=k&restore=p"
	code, _ := s.get(path, nil)
	eq(t, "the cache on the main listener", code, http.StatusUnauthorized)
	res, err := http.Get("http://" + info.RunnerListen + path)
	must(t, err)
	_ = res.Body.Close()
	eq(t, "the cache on the runners' listener", res.StatusCode, http.StatusUnauthorized)
}

func TestServeTakesACacheMaxAndRefusesABadOne(t *testing.T) {
	data, jobs, _ := dataAndJobs(t, map[string]string{"plain.yml": "name: plain\nstages:\n  s: {run: 'true'}\n"})
	code, _, errOut := kipp("", "serve", "--data", data, "--jobs", jobs, "--cache-max", "lots")
	eq(t, "a bad size", code, 2)
	contains(t, errOut, "cache-max")
}
