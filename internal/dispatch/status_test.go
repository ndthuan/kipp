package dispatch

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

// posted is one status the dispatcher asked to be posted, and for which job.
type posted struct {
	Job string
	CommitStatus
}

// poster is the Status of a dispatcher under test: it keeps what it is
// given, and fails with err when that is set.
type poster struct {
	mu  sync.Mutex
	got []posted
	err error
}

func (p *poster) post(_ context.Context, job *config.Job, s CommitStatus) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.got = append(p.got, posted{job.Name, s})
	return p.err
}

func (p *poster) all() []posted {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.got)
}

func (p *poster) states() []string {
	var out []string
	for _, s := range p.all() {
		out = append(out, s.State)
	}
	return out
}

// statusHarness is a dispatcher that reports to p, the way a server with
// --base-url does.
func statusHarness(t *testing.T, p *poster, parent string) *harness {
	t.Helper()
	return newHarness(t, map[string]string{"site": parent}, func(o *Options) {
		o.Status = p.post
		o.PublicURL = "https://ci.example"
	})
}

// tokenParent is a multibranch job that names the secret it calls GitHub with.
func tokenParent(dir, extra string) string {
	return "name: site\nmultibranch: {repo: acme/site, clone: '" + dir + "', token: GITHUB_TOKEN" + extra + "}\nconcurrency: parallel\n"
}

// pushed is a build of the default branch's child that the push of sha queued.
func (h *harness) pushed(t *testing.T, sha string) *store.Build {
	t.Helper()
	b, err := h.d.Trigger(context.Background(), TriggerRequest{Job: "site.main", Kind: "webhook", Detail: sha, Branch: "main", DefaultBranch: "main", By: "github"})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func oneStage(run string) string { return "stages:\n  s: {run: '" + run + "'}\n" }

func TestAChildBuildPostsPendingThenItsResult(t *testing.T) {
	r := newRepo(t)
	sha := r.commit("main", map[string]string{".kipp.yml": oneStage("true")})
	p := &poster{}
	h := statusHarness(t, p, tokenParent("file://"+r.dir, ""))
	h.withChildren(t, config.ChildRecord{Key: "main", Branch: "main"})
	b := h.wait(t, h.pushed(t, sha).ID)
	want := []posted{
		{"site.main", CommitStatus{SHA: sha, State: "pending", Context: "kipp/site", Description: "site.main #1 running", TargetURL: "https://ci.example/jobs/site.main/1"}},
		{"site.main", CommitStatus{SHA: sha, State: "success", Context: "kipp/site", Description: "site.main #1 success", TargetURL: "https://ci.example/jobs/site.main/1"}},
	}
	if got := p.all(); b.Status != store.Success || !slices.Equal(got, want) {
		t.Fatalf("%s: posted %+v, want %+v", b.Status, got, want)
	}
}

func TestAFailedStagePostsFailureOnTheCommitTheCloneBuilt(t *testing.T) {
	r := newRepo(t)
	r.commit("main", map[string]string{".kipp.yml": oneStage("exit 1")})
	p := &poster{}
	h := statusHarness(t, p, tokenParent("file://"+r.dir, ""))
	h.withChildren(t, config.ChildRecord{Key: "main", Branch: "main"})
	// Nothing names the commit: the build records the tip as of its clone.
	b, err := h.d.Trigger(context.Background(), TriggerRequest{Job: "site.main", Kind: "api", By: "ada"})
	if err != nil {
		t.Fatal(err)
	}
	b = h.wait(t, b.ID)
	got := p.all()
	if b.Status != store.Failed || len(got) != 2 || got[1].State != "failure" || got[1].SHA != b.BuiltCommit || got[0].SHA != b.BuiltCommit || b.BuiltCommit == "" {
		t.Fatalf("%s built %q: posted %+v", b.Status, b.BuiltCommit, got)
	}
}

func TestUnstablePostsSuccess(t *testing.T) {
	r := newRepo(t)
	sha := r.commit("main", map[string]string{".kipp.yml": "stages:\n  s: {run: 'exit 3', unstable_exit: [3]}\n"})
	p := &poster{}
	h := statusHarness(t, p, tokenParent("file://"+r.dir, ""))
	h.withChildren(t, config.ChildRecord{Key: "main", Branch: "main"})
	b := h.wait(t, h.pushed(t, sha).ID)
	if got := p.states(); b.Status != store.Unstable || !slices.Equal(got, []string{"pending", "success"}) {
		t.Fatalf("%s: posted %v", b.Status, got)
	}
	if last := p.all()[1]; last.Description != "site.main #1 unstable" {
		t.Fatalf("the description is %q, which says how the build ended", last.Description)
	}
}

func TestCancelledAndLostPostError(t *testing.T) {
	t.Run("cancelled", func(t *testing.T) {
		r := newRepo(t)
		sha := r.commit("main", map[string]string{".kipp.yml": oneStage("sleep 30")})
		p := &poster{}
		h := statusHarness(t, p, tokenParent("file://"+r.dir, ""))
		h.withChildren(t, config.ChildRecord{Key: "main", Branch: "main"})
		b := h.pushed(t, sha)
		h.untilStage(t, b.ID, "s", store.StageRunning)
		if err := h.d.Cancel(context.Background(), b.ID, "ada"); err != nil {
			t.Fatal(err)
		}
		b = h.wait(t, b.ID)
		if got := p.states(); b.Status != store.Cancelled || !slices.Equal(got, []string{"pending", "error"}) {
			t.Fatalf("%s: posted %v", b.Status, got)
		}
	})
	t.Run("lost", func(t *testing.T) {
		r := newRepo(t)
		sha := r.commit("main", map[string]string{".kipp.yml": oneStage("sleep 30")})
		p := &poster{}
		h := statusHarness(t, p, tokenParent("file://"+r.dir, ""))
		h.withChildren(t, config.ChildRecord{Key: "main", Branch: "main"})
		b := h.pushed(t, sha)
		h.untilStage(t, b.ID, "s", store.StageRunning)
		h.stop() // the server shuts down under the build
		if got, _ := h.st.Build(context.Background(), b.ID); got.Status != store.Lost || !slices.Equal(p.states(), []string{"pending", "error"}) {
			t.Fatalf("%s: posted %v", got.Status, p.states())
		}
	})
}

func TestATimeoutPostsFailure(t *testing.T) {
	r := newRepo(t)
	sha := r.commit("main", map[string]string{".kipp.yml": oneStage("sleep 30")})
	p := &poster{}
	h := statusHarness(t, p, tokenParent("file://"+r.dir, "")+"timeout: 1s\n")
	h.withChildren(t, config.ChildRecord{Key: "main", Branch: "main"})
	b := h.wait(t, h.pushed(t, sha).ID)
	if got := p.states(); b.Status != store.Timeout || !slices.Equal(got, []string{"pending", "failure"}) {
		t.Fatalf("%s: posted %v", b.Status, got)
	}
}

func TestAFailureBeforeAnyStagePostsError(t *testing.T) {
	r := newRepo(t)
	sha := r.commit("main", map[string]string{"README": "no stages file here\n"})
	p := &poster{}
	h := statusHarness(t, p, tokenParent("file://"+r.dir, ""))
	h.withChildren(t, config.ChildRecord{Key: "main", Branch: "main"})
	b := h.wait(t, h.pushed(t, sha).ID)
	if got := p.states(); b.Status != store.Failed || !slices.Equal(got, []string{"pending", "error"}) {
		t.Fatalf("%s: posted %v, want a build that ran no stage to end in error and not in failure", b.Status, got)
	}
}

func TestAMergeThatConflictsPostsErrorOnThePullRequestHead(t *testing.T) {
	r := newRepo(t)
	r.git("config", "uploadpack.allowFilter", "true")
	r.commit("main", map[string]string{".kipp.yml": oneStage("true"), "shared.txt": "one\n"})
	head := r.commit("feature-x", map[string]string{"shared.txt": "from the head\n"})
	r.git("checkout", "-q", "main")
	r.commit("main", map[string]string{"shared.txt": "from the base\n"})
	p := &poster{}
	h := statusHarness(t, p, tokenParent("file://"+r.dir, ""))
	h.withChildren(t, prChild(config.KindPR, "pr-7"))
	b := h.hook(t, "site.pr-7", head)
	got := p.all()
	if b.Status != store.Failed || len(got) == 0 || got[len(got)-1].State != "error" || got[len(got)-1].SHA != head {
		t.Fatalf("%s: posted %+v, want the last to be an error on %s", b.Status, got, head)
	}
}

func TestACloneThatFailsPostsErrorOnlyOnACommitSomethingNamed(t *testing.T) {
	missing := "file://" + t.TempDir() + "/no-such-repo"
	sha := "0123456789abcdef0123456789abcdef01234567"
	t.Run("a push named it", func(t *testing.T) {
		p := &poster{}
		h := statusHarness(t, p, tokenParent(missing, ""))
		h.withChildren(t, config.ChildRecord{Key: "main", Branch: "main"})
		b := h.wait(t, h.pushed(t, sha).ID)
		got := p.all()
		if b.Status != store.Failed || len(got) != 1 || got[0].State != "error" || got[0].SHA != sha {
			t.Fatalf("%s: posted %+v, want one error on the pushed commit", b.Status, got)
		}
	})
	t.Run("nothing did", func(t *testing.T) {
		p := &poster{}
		h := statusHarness(t, p, tokenParent(missing, ""))
		h.withChildren(t, config.ChildRecord{Key: "main", Branch: "main"})
		b, err := h.d.Trigger(context.Background(), TriggerRequest{Job: "site.main", Kind: "api", By: "ada"})
		if err != nil {
			t.Fatal(err)
		}
		b = h.wait(t, b.ID)
		if got := p.all(); b.Status != store.Failed || len(got) != 0 {
			t.Fatalf("%s: posted %+v for a commit nobody knows", b.Status, got)
		}
	})
}

func TestAStatusThatCannotBePostedIsALogLineAndChangesNothing(t *testing.T) {
	r := newRepo(t)
	sha := r.commit("main", map[string]string{".kipp.yml": oneStage("true")})
	p := &poster{err: errors.New("github: POST /repos/acme/site/statuses/x: 403 Resource not accessible")}
	h := statusHarness(t, p, tokenParent("file://"+r.dir, ""))
	h.withChildren(t, config.ChildRecord{Key: "main", Branch: "main"})
	b := h.wait(t, h.pushed(t, sha).ID)
	if b.Status != store.Success {
		t.Fatalf("a status that was refused ended the build %s", b.Status)
	}
	log := h.log(t, b, config.HooksLog)
	for _, want := range []string{"kipp: commit status pending not posted: github: POST", "kipp: commit status success not posted: github: POST"} {
		if !strings.Contains(log, want) {
			t.Fatalf("the hooks log is %q, want %q in it", log, want)
		}
	}
	if len(p.all()) != 2 {
		t.Fatalf("a refused pending was tried %d times in all, want once for each state", len(p.all()))
	}
}

func TestHeadBuildUnderBothUsesItsOwnContext(t *testing.T) {
	r, _, head := prRepo(t, oneStage("true"))
	p := &poster{}
	h := statusHarness(t, p, tokenParent("file://"+r.dir, ", pull_requests: both"))
	h.withChildren(t, prChild(config.KindPR, "pr-7"), prChild(config.KindPRHead, "pr-7-head"))
	h.hook(t, "site.pr-7", head)
	h.hook(t, "site.pr-7-head", head)
	contexts := map[string][]string{}
	for _, s := range p.all() {
		contexts[s.Job] = append(contexts[s.Job], s.Context)
	}
	if got := contexts["site.pr-7"]; !slices.Equal(got, []string{"kipp/site", "kipp/site"}) {
		t.Fatalf("the merge build posted under %v, want kipp/site", got)
	}
	if got := contexts["site.pr-7-head"]; !slices.Equal(got, []string{"kipp/site/head", "kipp/site/head"}) {
		t.Fatalf("the head build posted under %v, want kipp/site/head", got)
	}
	for _, s := range p.all() {
		if s.SHA != head {
			t.Fatalf("%s posted on %s, want the pull request's head %s", s.Job, s.SHA, head)
		}
	}
}

func TestHeadModeAloneUsesThePlainContext(t *testing.T) {
	r, _, head := prRepo(t, oneStage("true"))
	p := &poster{}
	h := statusHarness(t, p, tokenParent("file://"+r.dir, ", pull_requests: head"))
	h.withChildren(t, prChild(config.KindPRHead, "pr-7"))
	h.hook(t, "site.pr-7", head)
	for _, s := range p.all() {
		if s.Context != "kipp/site" {
			t.Fatalf("a head build with no merge build beside it posted under %q", s.Context)
		}
	}
	if len(p.all()) != 2 {
		t.Fatalf("posted %+v", p.all())
	}
}

func TestNoTokenPostsNothing(t *testing.T) {
	r := newRepo(t)
	sha := r.commit("main", map[string]string{".kipp.yml": oneStage("true")})
	p := &poster{}
	h := statusHarness(t, p, parentFile("file://"+r.dir, ""))
	h.withChildren(t, config.ChildRecord{Key: "main", Branch: "main"})
	b := h.wait(t, h.pushed(t, sha).ID)
	if got := p.all(); b.Status != store.Success || len(got) != 0 {
		t.Fatalf("%s: a job with no token posted %+v", b.Status, got)
	}
}

func TestNoTargetURLWithoutABaseURL(t *testing.T) {
	r := newRepo(t)
	sha := r.commit("main", map[string]string{".kipp.yml": oneStage("true")})
	p := &poster{}
	h := newHarness(t, map[string]string{"site": tokenParent("file://"+r.dir, "")}, func(o *Options) { o.Status = p.post })
	h.withChildren(t, config.ChildRecord{Key: "main", Branch: "main"})
	h.wait(t, h.pushed(t, sha).ID)
	for _, s := range p.all() {
		if s.TargetURL != "" {
			t.Fatalf("%s carries the target %q with no --base-url", s.State, s.TargetURL)
		}
	}
	if len(p.all()) != 2 {
		t.Fatalf("posted %+v", p.all())
	}
}

func TestABuildThatIsNoChildPostsNothing(t *testing.T) {
	p := &poster{}
	h := newHarness(t, map[string]string{"plain": "name: plain\nstages:\n  s: {run: 'true'}\n"}, func(o *Options) { o.Status = p.post })
	h.wait(t, h.trigger(t, "plain").ID)
	if got := p.all(); len(got) != 0 {
		t.Fatalf("a job of its own stages posted %+v", got)
	}
}
