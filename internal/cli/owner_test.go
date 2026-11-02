package cli

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// ownerGitHub is the account acme as GitHub answers for it: one repo,
// site, holding the stages file, and the webhooks that were made on it.
type ownerGitHub struct {
	mu    sync.Mutex
	head  string
	hooks []string // the bodies of the hooks created
	calls []string
}

func (g *ownerGitHub) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
	switch p := r.URL.Path; {
	case p == "/users/acme":
		_, _ = w.Write([]byte(`{"login":"acme","type":"Organization"}`))
	case p == "/orgs/acme/repos":
		_, _ = w.Write([]byte(`[{"full_name":"acme/site","name":"site","default_branch":"main","owner":{"login":"acme"}}]`))
	case p == "/repos/acme/site":
		_, _ = w.Write([]byte(`{"default_branch":"main"}`))
	case p == "/repos/acme/site/branches":
		_, _ = w.Write([]byte(`[{"name":"main","commit":{"sha":"` + g.head + `"}}]`))
	case p == "/repos/acme/site/pulls":
		_, _ = w.Write([]byte(`[]`))
	case strings.HasPrefix(p, "/repos/acme/site/contents/"):
		_, _ = w.Write([]byte(`{"type":"file"}`))
	case p == "/repos/acme/site/hooks" && r.Method == http.MethodGet:
		_, _ = w.Write([]byte(`[]`))
	case p == "/repos/acme/site/hooks" && r.Method == http.MethodPost:
		body, _ := io.ReadAll(r.Body)
		g.hooks = append(g.hooks, string(body))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	default:
		http.NotFound(w, r)
	}
}

func (g *ownerGitHub) made() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.hooks...)
}

// An owner job finds its repo, makes the webhook on it, and builds a
// push GitHub delivers to the owner's one URL, from the repo's own file.
func TestServeIndexesAnOwnersReposKeepsTheirWebhooksAndBuildsAPush(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on this machine")
	}
	root := t.TempDir()
	repo := filepath.Join(root, "acme", "site")
	must(t, os.MkdirAll(repo, 0o750))
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	must(t, os.WriteFile(filepath.Join(repo, ".kipp.yml"), []byte("stages:\n  s: {run: 'echo built $KIPP_BRANCH'}\n"), 0o600))
	git("add", ".kipp.yml")
	git("commit", "-q", "-m", "one")
	head := git("rev-parse", "HEAD")

	gh := &ownerGitHub{head: head}
	srv := httptest.NewServer(http.HandlerFunc(gh.serve))
	t.Cleanup(srv.Close)

	s := &served{t: t, stderr: &syncBuffer{}}
	s.data, s.jobs, s.token = dataAndJobs(t, map[string]string{
		"acme.yml": "name: acme\nowner: {login: acme, token: GH_TOKEN, scan: 0, clone: '" + root + "/%s'}\n",
	})
	s.args = []string{"--github-api", srv.URL, "--base-url", "https://ci.example"}
	s.serve()
	ok(t, "the-owner-token\n", "secret", "set", "GH_TOKEN", "--data", s.data)
	ok(t, "the hook's secret\n", "secret", "set", "GITHUB_WEBHOOK_ACME", "--data", s.data)

	req, err := http.NewRequest(http.MethodPost, s.url+"/api/v1/jobs/acme/scan", nil)
	must(t, err)
	req.Header.Set("Authorization", "Bearer "+s.token)
	res, err := http.DefaultClient.Do(req)
	must(t, err)
	_ = res.Body.Close()
	eq(t, "scan now", res.StatusCode, http.StatusAccepted)
	eventually(t, "the webhook is made", func() bool { return len(gh.made()) == 1 })

	var hook struct {
		Events []string `json:"events"`
		Config struct {
			URL    string `json:"url"`
			Secret string `json:"secret"`
		} `json:"config"`
	}
	must(t, json.Unmarshal([]byte(gh.made()[0]), &hook))
	if hook.Config.URL != "https://ci.example/api/v1/hooks/github/acme" || hook.Config.Secret != "the hook's secret" || strings.Join(hook.Events, ",") != "pull_request,push" {
		t.Fatalf("hook = %s", gh.made()[0])
	}

	payload := `{"ref":"refs/heads/main","after":"` + head + `","repository":{"full_name":"acme/site","default_branch":"main"}}`
	mac := hmac.New(sha256.New, []byte("the hook's secret"))
	mac.Write([]byte(payload))
	post := func(delivery string) int {
		req, err := http.NewRequest(http.MethodPost, s.url+"/api/v1/hooks/github/acme", strings.NewReader(payload))
		must(t, err)
		req.Header.Set("X-GitHub-Event", "push")
		req.Header.Set("X-GitHub-Delivery", delivery)
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		res, err := http.DefaultClient.Do(req)
		must(t, err)
		_ = res.Body.Close()
		return res.StatusCode
	}
	// The scan may have queued this head already; either way it is built once.
	if code := post("d-1"); code != http.StatusAccepted && code != http.StatusOK {
		t.Fatalf("a push to the owner's hook: %d", code)
	}
	eventually(t, "the repo's default branch is built", func() bool {
		return strings.Contains(kippGet(t, s, "/api/v1/jobs/acme.site.main/builds"), `"status":"success"`)
	})
	if log := s.stderr.String(); strings.Contains(log, "the-owner-token") || strings.Contains(log, "the hook's secret") {
		t.Fatalf("a secret is in the server's log: %s", log)
	}
}

func TestCheckRefusesAnOwnerJobThatGivesBothAuthsOrNeither(t *testing.T) {
	for what, body := range map[string]string{
		"both":    "name: acme\nowner: {login: acme, token: T, app: {id: 1, key_file: /k.pem}}\n",
		"neither": "name: acme\nowner: {login: acme}\n",
	} {
		dir := t.TempDir()
		write(t, filepath.Join(dir, "acme.yml"), body, 0o600)
		code, _, errOut := kipp("", "check", dir)
		if code != 1 || !strings.Contains(errOut, "owner") {
			t.Errorf("%s: %d %s", what, code, errOut)
		}
	}
	for what, body := range map[string]string{
		"token": "name: acme\nowner: {login: acme, token: T}\n",
		"app":   "name: acme\nowner: {login: acme, app: {id: 1, key_file: /k.pem}}\n",
	} {
		dir := t.TempDir()
		write(t, filepath.Join(dir, "acme.yml"), body, 0o600)
		if code, out, errOut := kipp("", "check", dir); code != 0 || out != "ok: 1 job\n" {
			t.Fatal(what, code, out, errOut)
		}
	}
}

func TestHookPlanSaysWhyItIsOff(t *testing.T) {
	s := &server{}
	plan := s.hookPlan(t.Context(), "acme")
	if !strings.Contains(plan.Off, "--base-url") || plan.URL != "" {
		t.Fatalf("plan = %+v", plan)
	}
}
