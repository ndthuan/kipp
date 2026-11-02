package cli

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// The server's index makes a multibranch job's child from a push to its
// hook, builds it from the repo's own file, keeps it across a restart, and
// a reload keeps it too.
func TestServeBuildsAMultibranchJobsDefaultBranchFromItsRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on this machine")
	}
	repo := t.TempDir()
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
	must(t, os.WriteFile(filepath.Join(repo, ".kipp.yml"), []byte("stages:\n  s: {run: 'echo built $KIPP_COMMIT on $KIPP_BRANCH'}\n"), 0o600))
	git("add", ".kipp.yml")
	git("commit", "-q", "-m", "one")
	head := git("rev-parse", "HEAD")
	s := start(t, map[string]string{"site.yml": "name: site\nmultibranch: {repo: acme/site, clone: '" + repo + "'}\n"})
	ok(t, "the hook's secret\n", "secret", "set", "GITHUB_WEBHOOK_SITE", "--data", s.data)
	payload := `{"ref":"refs/heads/main","after":"` + head + `","repository":{"full_name":"acme/site","default_branch":"main"}}`
	mac := hmac.New(sha256.New, []byte("the hook's secret"))
	mac.Write([]byte(payload))
	req, err := http.NewRequest(http.MethodPost, s.url+"/api/v1/hooks/github/site", strings.NewReader(payload))
	must(t, err)
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", "d-1")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	res, err := http.DefaultClient.Do(req)
	must(t, err)
	_ = res.Body.Close()
	eq(t, "a push to the default branch", res.StatusCode, http.StatusAccepted)
	eventually(t, "the child's build succeeds", func() bool {
		return strings.Contains(kippGet(t, s, "/api/v1/jobs/site.main/builds"), `"status":"success"`)
	})
	if body := kippGet(t, s, "/api/v1/jobs/site.main/builds"); !strings.Contains(body, `"built_commit":"`+head+`"`) {
		t.Fatalf("%s", body)
	}
	code, _, errOut := s.kipp("reload")
	eq(t, "reload: "+errOut, code, 0)
	// A build of the parent by hand builds the tip of its default branch.
	code, out, errOut := s.kipp("run", "site", "--wait")
	eq(t, "kipp run site: "+errOut, code, 0)
	contains(t, out, "built "+head+" on main")
	eq(t, "the server stops", s.stop(), 0)
	s.serve()
	if body := kippGet(t, s, "/api/v1/jobs/site/children"); !strings.Contains(body, `"job":"site.main"`) {
		t.Fatalf("after a restart: %s", body)
	}
}

// A child's build says its state to the GitHub API the server was given,
// with the secret the job names, on the commit it built, and links to its
// own page by --base-url.
func TestServePostsCommitStatusesToTheGitHubAPIWithTheJobsToken(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on this machine")
	}
	repo := t.TempDir()
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
	must(t, os.WriteFile(filepath.Join(repo, ".kipp.yml"), []byte("stages:\n  s: {run: 'true'}\n"), 0o600))
	git("add", ".kipp.yml")
	git("commit", "-q", "-m", "one")
	head := git("rev-parse", "HEAD")

	var mu sync.Mutex
	var got []string
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization")+" "+string(data))
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(gh.Close)
	seen := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}

	s := &served{t: t, stderr: &syncBuffer{}}
	s.data, s.jobs, s.token = dataAndJobs(t, map[string]string{
		"site.yml": "name: site\nmultibranch: {repo: acme/site, clone: '" + repo + "', token: GITHUB_TOKEN, scan: 0}\n",
	})
	s.args = []string{"--github-api", gh.URL, "--base-url", "https://ci.example"}
	s.serve()
	ok(t, "the hook's secret\n", "secret", "set", "GITHUB_WEBHOOK_SITE", "--data", s.data)
	ok(t, "the-api-token\n", "secret", "set", "GITHUB_TOKEN", "--data", s.data)
	payload := `{"ref":"refs/heads/main","after":"` + head + `","repository":{"full_name":"acme/site","default_branch":"main"}}`
	mac := hmac.New(sha256.New, []byte("the hook's secret"))
	mac.Write([]byte(payload))
	req, err := http.NewRequest(http.MethodPost, s.url+"/api/v1/hooks/github/site", strings.NewReader(payload))
	must(t, err)
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", "d-1")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	res, err := http.DefaultClient.Do(req)
	must(t, err)
	_ = res.Body.Close()
	eq(t, "a push to the default branch", res.StatusCode, http.StatusAccepted)
	eventually(t, "both states are posted", func() bool { return len(seen()) >= 2 })

	path := "POST /repos/acme/site/statuses/" + head + " Bearer the-api-token "
	want := []string{
		path + `{"state":"pending","context":"kipp/site","description":"site.main #1 running","target_url":"https://ci.example/jobs/site.main/1"}`,
		path + `{"state":"success","context":"kipp/site","description":"site.main #1 success","target_url":"https://ci.example/jobs/site.main/1"}`,
	}
	if calls := seen(); !slices.Equal(calls, want) {
		t.Fatalf("GitHub was called with\n%s\nwant\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
	if log := s.stderr.String(); strings.Contains(log, "the-api-token") {
		t.Fatalf("the token is in the server's log: %s", log)
	}
}
