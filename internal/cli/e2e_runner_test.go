package cli

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/runnerd"
	"github.com/ndthuan/kipp/internal/version"
)

// A server that runs no stage itself gives a stage with runs_on to the
// standing runner registered for its labels, which runs it with the
// server's variables --runner-env names and none of the others, and the
// build says where it ran.
func TestServeWithAStandingRunnerRunsAStageThere(t *testing.T) {
	t.Setenv("KIPP_DRY", "0")
	t.Setenv("KIPP_TEST_SERVER_ONLY", "leak")
	job := "name: remote\nstages:\n  s: {runs_on: linux, run: 'echo \"on $KIPP_RUNNER dry=$KIPP_DRY only=[$KIPP_TEST_SERVER_ONLY] tmp=[$TMPDIR] in $PWD\"'}\n"
	s := &served{t: t, stderr: &syncBuffer{}, args: []string{"--executors", "0", "--runner-env", "KIPP_DRY", "--pass-env", "*"}}
	s.data, s.jobs, s.token = dataAndJobs(t, map[string]string{"remote.yml": job})
	s.serve()
	token := tokenIn(t, ok(t, "", "runner", "add", "box", "--labels", "linux", "--data", s.data))
	must(t, syscall.Kill(os.Getpid(), syscall.SIGHUP))
	eventually(t, "the server read the runner", func() bool {
		return strings.Contains(s.stderr.String(), `msg="engines reloaded" count=0 runners=1`)
	})
	work := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	ended := make(chan error, 1)
	go func() {
		ended <- runnerd.Run(ctx, runnerd.Config{URL: s.url, Token: token, Workdir: work, Version: version.Version,
			Env: []string{"PATH=" + os.Getenv("PATH")}, Log: slog.New(slog.DiscardHandler), Retry: 50 * time.Millisecond})
	}()
	t.Cleanup(func() { cancel(); <-ended })

	code, out, errOut := s.kipp("run", "remote", "--wait")
	eq(t, "kipp run --wait: "+errOut, code, 0)
	// $PWD is the physical path: on macOS the temp dir's /var is a link to /private/var.
	physical, err := filepath.EvalSymlinks(work)
	must(t, err)
	contains(t, out, "on box dry=0 only=[] tmp=[] in "+filepath.Join(physical, "remote", "1"))
	var b struct {
		Status string `json:"status"`
		Stages []struct {
			RanOn string `json:"ran_on"`
		} `json:"stages"`
	}
	must(t, json.Unmarshal([]byte(kippGet(t, s, "/api/v1/builds/1")), &b))
	eq(t, "status", b.Status, "success")
	eq(t, "ran on", b.Stages[0].RanOn, "box")
	var list struct {
		Items []struct {
			Name   string `json:"name"`
			Online bool   `json:"online"`
		} `json:"items"`
	}
	must(t, json.Unmarshal([]byte(kippGet(t, s, "/api/v1/admin/runners")), &list))
	runners := list.Items
	if len(runners) != 1 || !runners[0].Online {
		t.Fatalf("%+v", runners)
	}
	eventually(t, "the build's workspace on the runner is gone", func() bool {
		_, err := os.Stat(filepath.Join(work, "remote", "1"))
		return os.IsNotExist(err)
	})
}
