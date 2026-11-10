package cli

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The deadlock of a safe restart, end to end: a job polls for what the
// build of a webhook makes, the webhook arrives while quiet and is kept, and
// quiet would wait for the job for ever. With rerun_on_restart the wait is
// over at once, the restart stops the job, and the next start queues it
// again ahead of the kept webhook, whose build lets it end.
func TestASafeRestartStopsAJobThatWaitsOnAKeptWebhookAndRunsItAgainBeforeTheWebhook(t *testing.T) {
	old := quietPoll
	quietPoll = 50 * time.Millisecond
	t.Cleanup(func() { quietPoll = old })
	made := filepath.Join(t.TempDir(), "made")
	s := start(t, map[string]string{
		"flow.yml":   "name: flow\nrerun_on_restart: true\nstages:\n  s: {run: 'while [ ! -e " + made + " ]; do sleep 0.02; done'}\n",
		"hooked.yml": "name: hooked\ntriggers:\n  webhook: {repo: example/app, branches: [main]}\nstages:\n  s: {run: 'touch " + made + "'}\n",
	})
	ok(t, "the hook's secret\n", "secret", "set", "GITHUB_WEBHOOK_HOOKED", "--data", s.data)
	eq(t, "start", func() int { c, _, _ := s.kipp("run", "flow"); return c }(), 0)
	eventually(t, "the flow runs", func() bool {
		return strings.Contains(kippGet(t, s, "/api/v1/jobs/flow/builds/1"), `"status":"running"`)
	})

	code, out, _ := s.kipp("quiet", "on", "--wait", "--timeout", "30s")
	eq(t, "the wait", code, 0)
	eq(t, "what it says", out, "quiet on: 0 running, 0 queued, 1 will run again after the restart\n")

	payload := `{"ref":"refs/heads/main","after":"` + strings.Repeat("ab", 20) + `","repository":{"full_name":"example/app"}}`
	mac := hmac.New(sha256.New, []byte("the hook's secret"))
	mac.Write([]byte(payload))
	req, err := http.NewRequest(http.MethodPost, s.url+"/api/v1/hooks/github/hooked", strings.NewReader(payload))
	must(t, err)
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", "d-1")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	res, err := http.DefaultClient.Do(req)
	must(t, err)
	_ = res.Body.Close()
	eq(t, "the push while quiet is kept", res.StatusCode, http.StatusAccepted)

	eq(t, "the stop", s.stop(), 0)
	s.serve()

	type build struct {
		ID      int64  `json:"id"`
		Status  string `json:"status"`
		RerunOf *int   `json:"rerun_of"`
		RerunAs *int   `json:"rerun_as"`
	}
	read := func(path string) build {
		var b build
		must(t, json.Unmarshal([]byte(kippGet(t, s, path)), &b))
		return b
	}
	var again, hooked build
	eventually(t, "both end", func() bool {
		again, hooked = read("/api/v1/jobs/flow/builds/2"), read("/api/v1/jobs/hooked/builds/1")
		return again.Status == "success" && hooked.Status == "success"
	})
	stopped := read("/api/v1/jobs/flow/builds/1")
	if stopped.Status != "cancelled" || stopped.RerunAs == nil || *stopped.RerunAs != 2 || again.RerunOf == nil || *again.RerunOf != 1 {
		t.Fatalf("flow #1 %+v and #2 %+v do not link as stopped and run again", stopped, again)
	}
	if again.ID > hooked.ID {
		t.Fatalf("the kept webhook's build (id %d) was queued before the build run again (id %d)", hooked.ID, again.ID)
	}
}
