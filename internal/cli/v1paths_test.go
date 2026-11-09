package cli

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const hookedJob = "name: hooked\ntriggers:\n  webhook: {repo: example/app, branches: [main]}\nstages:\n  s: {run: 'true'}\n"

// deliver is a push for the job "hooked", signed with the stored secret
// and padded to pad bytes; it returns the status.
func deliver(t *testing.T, s *served, path, delivery string, pad int) int {
	t.Helper()
	payload := `{"ref":"refs/heads/main","after":"` + strings.Repeat("ab", 20) +
		`","repository":{"full_name":"example/app"},"pad":"` + strings.Repeat("x", pad) + `"}`
	mac := hmac.New(sha256.New, []byte("the hook's secret"))
	mac.Write([]byte(payload))
	req, err := http.NewRequest(http.MethodPost, s.url+path, strings.NewReader(payload))
	must(t, err)
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", delivery)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	res, err := http.DefaultClient.Do(req)
	must(t, err)
	_ = res.Body.Close()
	return res.StatusCode
}

func TestAWebhookBodyOverTheAPIsLimitIsAcceptedAtTheV1Path(t *testing.T) {
	s := start(t, map[string]string{"hooked.yml": hookedJob})
	ok(t, "the hook's secret\n", "secret", "set", "GITHUB_WEBHOOK_HOOKED", "--data", s.data)
	eq(t, "a 100 KiB push", deliver(t, s, "/api/v1/hooks/github/hooked", "big-1", 100<<10), http.StatusAccepted)
}

func TestAWebhookIsDeliveredUnderAPIV1AndNowhereElse(t *testing.T) {
	s := start(t, map[string]string{"hooked.yml": hookedJob})
	ok(t, "the hook's secret\n", "secret", "set", "GITHUB_WEBHOOK_HOOKED", "--data", s.data)
	for i, path := range []string{"/hooks/github/hooked", "/api/hooks/github/hooked"} {
		if code := deliver(t, s, path, "old-"+strconv.Itoa(i), 0); code == http.StatusAccepted {
			t.Errorf("%s took the push", path)
		}
	}
	eq(t, "builds", kippGet(t, s, "/api/v1/jobs/hooked/builds"), `{"items":[],"next_cursor":null}`+"\n")
}

// The client calls /api/v1 once and nothing else, whatever the answer.
func TestTheClientAsksAPIV1AloneAndSaysTheProblemsDetail(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"status":404,"code":"not_found","detail":"no job nojob"}`)
	}))
	defer srv.Close()
	code, _, errOut := kipp("", "run", "nojob", "--url", srv.URL, "--token", "kipp_t")
	eq(t, "exit", code, 1)
	contains(t, errOut, "no job nojob")
	code, _, _ = kipp("", "reload", "--url", srv.URL, "--token", "kipp_t")
	eq(t, "reload exit", code, 1)
	mu.Lock()
	defer mu.Unlock()
	eq(t, "requests", strings.Join(seen, ","), "POST /api/v1/jobs/nojob/builds,POST /api/v1/admin/reload")
}

// The runners' and the cache's calls are their own routes: what answers is
// their token check, not the JSON API's login, and the same paths without
// /api/v1 reach neither, on the main listener or on the runners'.
func TestRunnerAndCacheCallsAreUnderAPIV1OnBothListenersAndNowhereElse(t *testing.T) {
	s := &served{t: t, stderr: &syncBuffer{}, args: []string{"--runner-listen", "127.0.0.1:0"}}
	s.data, s.jobs, s.token = dataAndJobs(t, map[string]string{"plain.yml": "name: plain\nstages:\n  s: {run: 'true'}\n"})
	s.serve()
	var info struct {
		RunnerListen string `json:"runner_listen"`
	}
	must(t, json.Unmarshal([]byte(kippGet(t, s, "/api/v1/admin/server")), &info))
	cachePath := "/api/v1/cache/site-smoke/lookup?key=k&restore=p"
	get := func(url string) (int, string) {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		must(t, err)
		res, err := http.DefaultClient.Do(req)
		must(t, err)
		defer func() { _ = res.Body.Close() }()
		body, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(body)
	}
	for _, base := range []string{"http://" + info.RunnerListen, s.url} {
		code, body := get(base + cachePath)
		eq(t, base+" the cache at v1", code, http.StatusUnauthorized)
		contains(t, body, "the cache token is missing")
	}
	// A cache path without /api/v1 reaches no cache on either listener: the
	// runners' listener has no such route, and the main one answers as a page.
	code, _ := get("http://" + info.RunnerListen + "/cache/site-smoke/lookup?key=k&restore=p")
	eq(t, "the cache without /api/v1 on the runners' listener", code, http.StatusNotFound)
	code, _ = get("http://" + info.RunnerListen + "/runner/claim")
	eq(t, "the runner without /api/v1 on the runners' listener", code, http.StatusNotFound)
	_, body := get(s.url + "/cache/site-smoke/lookup?key=k&restore=p")
	lacks(t, body, "the cache token is missing")
	_, body = get(s.url + "/runner/claim")
	lacks(t, body, "a runner's token")
}
