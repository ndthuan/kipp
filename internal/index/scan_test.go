package index

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ndthuan/kipp/internal/github"
	"github.com/ndthuan/kipp/internal/store"
)

// fakeGitHub is the repo acme/site as GitHub answers for it.
type fakeGitHub struct {
	mu       sync.Mutex
	def      string
	branches map[string]string // name to head
	pulls    []fakePull
	author   string          // who opened every pull request
	files    map[string]bool // "<path>@<sha>"
	fail     map[string]int  // a path prefix to the status it answers
	calls    []string
}

type fakePull struct {
	n                           int
	title, ref, sha, repo, base string
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{def: "main", branches: map[string]string{"main": head}, files: map[string]bool{".kipp.yml@" + head: true}, author: "ann"}
}

func (g *fakeGitHub) client(t *testing.T) *github.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(srv.Close)
	return github.New(srv.URL, "scan-token")
}

func (g *fakeGitHub) called(prefix string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, c := range g.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (g *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, r.URL.Path)
	for prefix, status := range g.fail {
		if strings.HasPrefix(r.URL.Path, prefix) {
			http.Error(w, "refused", status)
			return
		}
	}
	const repo = "/repos/acme/site"
	switch p := r.URL.Path; {
	case p == repo:
		_ = json.NewEncoder(w).Encode(map[string]string{"default_branch": g.def})
	case p == repo+"/branches":
		var out []map[string]any
		for _, name := range slices.Sorted(mapKeys(g.branches)) {
			out = append(out, map[string]any{"name": name, "commit": map[string]string{"sha": g.branches[name]}})
		}
		_ = json.NewEncoder(w).Encode(out)
	case strings.HasPrefix(p, repo+"/branches/"):
		name := strings.TrimPrefix(p, repo+"/branches/")
		sha, ok := g.branches[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"name": name, "commit": map[string]string{"sha": sha}})
	case p == repo+"/pulls":
		out := []map[string]any{}
		for _, pr := range g.pulls {
			out = append(out, map[string]any{"number": pr.n, "title": pr.title,
				"html_url": "https://github.example/acme/site/pull/" + strconv.Itoa(pr.n), "user": map[string]string{"login": g.author},
				"head": map[string]any{"ref": pr.ref, "sha": pr.sha, "repo": map[string]string{"full_name": pr.repo}},
				"base": map[string]string{"ref": pr.base}})
		}
		_ = json.NewEncoder(w).Encode(out)
	case strings.HasPrefix(p, repo+"/contents/"):
		if g.files[strings.TrimPrefix(p, repo+"/contents/")+"@"+r.URL.Query().Get("ref")] {
			_, _ = w.Write([]byte(`{"type":"file"}`))
			return
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

// keyHex is the suffix a branch name that was changed into a key carries.
func keyHex(branch string) string {
	sum := sha256.Sum256([]byte(branch))
	return hex.EncodeToString(sum[:])[:6]
}

func mapKeys[K comparable, V any](m map[K]V) func(yield func(K) bool) {
	return func(yield func(K) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// pr adds the open pull request #12 from acme/site itself, with the file at its head.
func (g *fakeGitHub) pr(sha string) {
	const ref = "feature/login"
	g.pulls = append(g.pulls, fakePull{12, "PR " + ref, ref, sha, "acme/site", "main"})
	g.files[".kipp.yml@"+sha] = true
}

func (f *fixture) scan(g *fakeGitHub) *ScanReport {
	f.t.Helper()
	rep, err := f.x.Scan(ctx, "site", g.client(f.t))
	must(f.t, err)
	return rep
}

func jobsOf(rep *ScanReport) []string {
	var out []string
	for _, b := range rep.Builds {
		out = append(out, b.Job)
	}
	slices.Sort(out)
	return out
}

func TestScanMakesChildrenOnlyWhereTheFileExists(t *testing.T) {
	f := newFixture(t, map[string]string{"site": site})
	g := newFakeGitHub()
	g.pr(head2)
	g.pulls = append(g.pulls, fakePull{13, "No file", "nofile", "ffff", "acme/site", "main"}) // its head has no .kipp.yml
	rep := f.scan(g)
	if got := jobsOf(rep); !slices.Equal(got, []string{"site.main", "site.pr-12"}) {
		t.Fatalf("builds = %v", got)
	}
	var keys []string
	for _, c := range f.x.Children("site") {
		keys = append(keys, c.Key)
	}
	if !slices.Equal(keys, []string{"main", "pr-12"}) {
		t.Fatalf("children = %v: a candidate without the file gets none", keys)
	}
	if f.jobSet().Jobs["site.pr-12"] == nil || f.jobSet().Jobs["site.pr-13"] != nil {
		t.Fatal("the job set does not hold exactly the children with a file")
	}
}

func TestScanBuildsAHeadNoBuildHasSeen(t *testing.T) {
	f := newFixture(t, map[string]string{"site": site})
	g := newFakeGitHub()
	rep := f.scan(g)
	if len(rep.Builds) != 1 {
		t.Fatalf("builds = %+v", rep.Builds)
	}
	b := rep.Builds[0]
	if b.Job != "site.main" || b.Kind != "scan" || b.Detail != head || b.Branch != "main" || b.DefaultBranch != "main" {
		t.Fatalf("build %+v", b)
	}
	// It built that head, and then the branch moves on: the new head is built.
	must(t, f.st.CreateBuild(ctx, &store.Build{Job: "site.main", Parent: "site", Status: store.Success, Trigger: "scan", TriggerDetail: head, BuiltCommit: head}))
	if rep := f.scan(g); len(rep.Builds) != 0 {
		t.Fatalf("the built head was built again: %+v", rep.Builds)
	}
	g.branches["main"] = head2
	g.files[".kipp.yml@"+head2] = true
	rep = f.scan(g)
	if len(rep.Builds) != 1 || rep.Builds[0].Detail != head2 {
		t.Fatalf("a head that moved was not built: %+v", rep.Builds)
	}
}

func TestScanSeesAHeadTheWebhookBuiltBeforeAnyCommitWasRecorded(t *testing.T) {
	f := newFixture(t, map[string]string{"site": site})
	must(t, f.st.CreateBuild(ctx, &store.Build{Job: "site.main", Parent: "site", Status: store.Failed, Trigger: "webhook", TriggerDetail: head}))
	if rep := f.scan(newFakeGitHub()); len(rep.Builds) != 0 {
		t.Fatalf("a head a webhook build was started for was built again: %+v", rep.Builds)
	}
}

func TestScanDoesNotBuildAHeadAlreadyQueuedOrRunning(t *testing.T) {
	for _, status := range []store.Status{store.Queued, store.Running} {
		t.Run(string(status), func(t *testing.T) {
			f := newFixture(t, map[string]string{"site": site})
			g := newFakeGitHub()
			g.pr(head2)
			// An older build finished; a newer one of the head is on its way.
			must(t, f.st.CreateBuild(ctx, &store.Build{Job: "site.pr-12", Parent: "site", Status: store.Success, Trigger: "webhook", TriggerDetail: "old", BuiltCommit: "old"}))
			must(t, f.st.CreateBuild(ctx, &store.Build{Job: "site.pr-12", Parent: "site", Status: status, Trigger: "webhook", TriggerDetail: head2}))
			rep := f.scan(g)
			if got := jobsOf(rep); !slices.Equal(got, []string{"site.main"}) {
				t.Fatalf("builds = %v", got)
			}
		})
	}
}

func TestFirstScanBuildsEachCandidateOnce(t *testing.T) {
	f := newFixture(t, map[string]string{"site": "name: site\nmultibranch: {repo: acme/site, pull_requests: both, branches: [default, 'release/*'], branch_with_pr: build}\n"})
	g := newFakeGitHub()
	g.branches["release/1"] = head2
	g.files[".kipp.yml@"+head2] = true
	g.pr("aaaa")
	rep := f.scan(g)
	want := []string{"site.main", "site.pr-12", "site.pr-12-head", "site.release-1-" + keyHex("release/1")}
	if got := jobsOf(rep); !slices.Equal(got, want) {
		t.Fatalf("builds = %v, want %v", got, want)
	}
	seen := map[string]bool{}
	for _, b := range rep.Builds {
		if seen[b.Job] {
			t.Fatalf("%s is built twice", b.Job)
		}
		seen[b.Job] = true
	}
}

func TestScanFillsAndUpdatesAPullRequestsAuthorAndPage(t *testing.T) {
	f := newFixture(t, map[string]string{"site": site})
	g := newFakeGitHub()
	g.pr(head2)
	f.scan(g)
	author := func() (string, string) {
		for _, c := range f.x.Children("site") {
			if c.Key == "pr-12" {
				return c.Author, c.URL
			}
		}
		t.Fatal("no pr-12")
		return "", ""
	}
	if a, u := author(); a != "ann" || u != "https://github.example/acme/site/pull/12" {
		t.Fatalf("author %q, page %q", a, u)
	}
	g.author = "bob"
	f.scan(g)
	if a, _ := author(); a != "bob" {
		t.Fatalf("a rescan leaves the old author: %q", a)
	}
	for _, c := range f.x.Children("site") {
		if c.Key == "main" && (c.Author != "" || c.URL != "") {
			t.Fatalf("a branch has an author: %+v", c)
		}
	}
}

func TestScanMarksAChildGoneThatItNoLongerFinds(t *testing.T) {
	f := newFixture(t, map[string]string{"site": "name: site\nmultibranch: {repo: acme/site, branches: [default, 'topic/*']}\n"})
	g := newFakeGitHub()
	g.branches["topic/a"] = head2
	g.files[".kipp.yml@"+head2] = true
	g.pr("aaaa")
	f.scan(g)
	// The branch is deleted and the pull request is closed.
	delete(g.branches, "topic/a")
	g.pulls = nil
	f.clock = f.clock.Add(1)
	f.scan(g)
	states := map[string]string{}
	for _, c := range f.x.Children("site") {
		states[c.Key] = c.State
	}
	if states["main"] != "live" || states["pr-12"] != "gone" || states["topic-a-"+keyHex("topic/a")] != "gone" {
		t.Fatalf("states = %v", states)
	}
	// A gone pull request that is open again is the same child again.
	g.pr("aaaa")
	f.scan(g)
	for _, c := range f.x.Children("site") {
		if c.Key == "pr-12" && (c.State != "live" || c.GoneAt != nil) {
			t.Fatalf("pr-12 = %+v", c)
		}
	}
}

func TestScanMarksAChildGoneWhoseFileWentAway(t *testing.T) {
	f := newFixture(t, map[string]string{"site": site})
	g := newFakeGitHub()
	f.scan(g)
	delete(g.files, ".kipp.yml@"+head)
	f.scan(g)
	if c := f.x.Children("site"); len(c) != 1 || c[0].State != "gone" {
		t.Fatalf("children = %+v", c)
	}
}

func TestScanListsBranchesOnlyWhenBranchesNamesMoreThanDefault(t *testing.T) {
	f := newFixture(t, map[string]string{"site": site})
	g := newFakeGitHub()
	f.scan(g)
	if g.called("/repos/acme/site/branches") != 1 || g.called("/repos/acme/site/branches/main") != 1 {
		t.Fatalf("calls = %v: the default branch is asked for alone", g.calls)
	}
	f2 := newFixture(t, map[string]string{"site": "name: site\nmultibranch: {repo: acme/site, branches: [default, 'release/*']}\n"})
	g2 := newFakeGitHub()
	f2.scan(g2)
	if g2.called("/repos/acme/site/branches") != 1 || g2.called("/repos/acme/site/branches/") != 0 {
		t.Fatalf("calls = %v: the list is asked for", g2.calls)
	}
}

func TestScanSkipsForkPRs(t *testing.T) {
	f := newFixture(t, map[string]string{"site": site})
	g := newFakeGitHub()
	g.pulls = []fakePull{{9, "From afar", "patch", "ffff", "mallory/site", "main"}, {10, "Fork gone", "x", "eeee", "", "main"}}
	g.files[".kipp.yml@ffff"] = true
	rep := f.scan(g)
	if got := jobsOf(rep); !slices.Equal(got, []string{"site.main"}) {
		t.Fatalf("builds = %v: a fork's pull request was built", got)
	}
	forks := 0
	for _, c := range f.x.Children("site") {
		if c.FromFork {
			forks++
		}
	}
	if forks != 2 {
		t.Fatalf("%d fork rows, want both listed", forks)
	}
	for _, name := range []string{"site.pr-9", "site.pr-10"} {
		if f.jobSet().Jobs[name] != nil {
			t.Fatalf("%s is a job", name)
		}
	}
	if g.called("/repos/acme/site/contents/") != 1 {
		t.Fatalf("calls = %v: a fork's head is not even read", g.calls)
	}
}

func TestScanSkipsABranchWithAnOpenPullRequest(t *testing.T) {
	f := newFixture(t, map[string]string{"site": "name: site\nmultibranch: {repo: acme/site, branches: [default, 'feature/*']}\n"})
	g := newFakeGitHub()
	g.branches["feature/login"] = head2
	g.pr(head2)
	if got := jobsOf(f.scan(g)); !slices.Equal(got, []string{"site.main", "site.pr-12"}) {
		t.Fatalf("builds = %v: the pull request builds it", got)
	}
}

func TestScanErrorChangesNothing(t *testing.T) {
	f := newFixture(t, map[string]string{"site": site})
	g := newFakeGitHub()
	f.scan(g)
	before := f.x.Children("site")
	g.pr(head2)
	g.fail = map[string]int{"/repos/acme/site/pulls": http.StatusForbidden}
	rep, err := f.x.Scan(ctx, "site", g.client(t))
	if err == nil || rep != nil {
		t.Fatalf("rep = %+v, err = %v", rep, err)
	}
	if !strings.Contains(err.Error(), "/repos/acme/site/pulls") || strings.Contains(err.Error(), "scan-token") {
		t.Fatalf("err = %v", err)
	}
	if after := f.x.Children("site"); len(after) != len(before) || after[0] != before[0] {
		t.Fatalf("a failed scan changed the children: %+v", after)
	}
	rows, err := f.st.Children(ctx, "site")
	must(t, err)
	if len(rows) != 1 {
		t.Fatalf("a failed scan wrote rows: %+v", rows)
	}
}

func TestScanOfAJobThatIsNotMultibranchIsRefused(t *testing.T) {
	f := newFixture(t, map[string]string{"site": site, "plain": "name: plain\nstages:\n  s: {run: 'true'}\n"})
	if _, err := f.x.Scan(ctx, "plain", newFakeGitHub().client(t)); err == nil {
		t.Fatal("scanned a job that is not multibranch")
	}
}

func TestScanLeavesAPullRequestOffWhenPullRequestsAreOff(t *testing.T) {
	f := newFixture(t, map[string]string{"site": "name: site\nmultibranch: {repo: acme/site, pull_requests: 'off'}\n"})
	g := newFakeGitHub()
	g.pr(head2)
	f.scan(g)
	if g.called("/repos/acme/site/pulls") != 0 {
		t.Fatalf("calls = %v: pull requests are off", g.calls)
	}
}
