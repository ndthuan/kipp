package dispatch

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

type posted struct {
	Job string
	CommitStatus
}

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

func statusHarness(t *testing.T, p *poster, parent string) *harness {
	t.Helper()
	return newHarness(t, map[string]string{"site": parent}, func(o *Options) {
		o.Status = p.post
		o.PublicURL = "https://ci.example"
	})
}

func tokenParent(dir, extra string) string {
	return "name: site\nmultibranch: {repo: acme/site, clone: '" + dir + "', token: GITHUB_TOKEN" + extra + "}\nconcurrency: parallel\n"
}

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
		h.stop()
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

func supersedeParent(dir string) string {
	return "name: site\nmultibranch: {repo: acme/site, clone: '" + dir + "', token: GITHUB_TOKEN}\nconcurrency: supersede\n"
}

func (p *poster) of(number string) []posted {
	return slices.DeleteFunc(p.all(), func(s posted) bool { return !strings.Contains(s.Description, "#"+number+" ") })
}

func TestABuildSupersededByOneOfTheSameCommitPostsNoErrorAndTheReplacementOwnsTheCommit(t *testing.T) {
	r := newRepo(t)
	sha := r.commit("main", map[string]string{".kipp.yml": oneStage("sleep 30")})
	p := &poster{}
	h := statusHarness(t, p, supersedeParent("file://"+r.dir))
	h.withChildren(t, config.ChildRecord{Key: "main", Branch: "main"})
	old := h.pushed(t, sha)
	h.untilStage(t, old.ID, "s", store.StageRunning)
	fresh := h.pushed(t, sha)
	if got := h.wait(t, old.ID); got.Status != store.Cancelled {
		t.Fatalf("the old build is %s, want cancelled", got.Status)
	}
	h.untilStage(t, fresh.ID, "s", store.StageRunning)
	if got := p.of("1"); len(got) != 1 || got[0].State != "pending" {
		t.Fatalf("the superseded build posted %+v, want its pending alone", got)
	}
	if got := p.of("2"); len(got) < 2 || got[0].State != "pending" || got[0].Description != "site.main #2 queued" || got[len(got)-1].State != "pending" || got[len(got)-1].SHA != sha {
		t.Fatalf("the replacement posted %+v, want pending at queue and at its start", got)
	}
	if err := h.d.Cancel(context.Background(), fresh.ID, "ada"); err != nil {
		t.Fatal(err)
	}
	h.wait(t, fresh.ID)
	if got := p.all(); got[len(got)-1].State != "error" || got[len(got)-1].Description != "site.main #2 cancelled" {
		t.Fatalf("a by-hand cancel of the replacement posted %+v last", got)
	}
}

func TestABuildSupersededByOneOfAnotherCommitStillPostsErrorOnItsOwn(t *testing.T) {
	r := newRepo(t)
	first := r.commit("main", map[string]string{".kipp.yml": oneStage("sleep 30")})
	second := r.commit("main", map[string]string{".kipp.yml": oneStage("true")})
	p := &poster{}
	h := statusHarness(t, p, supersedeParent("file://"+r.dir))
	h.withChildren(t, config.ChildRecord{Key: "main", Branch: "main"})
	old := h.pushed(t, first)
	h.untilStage(t, old.ID, "s", store.StageRunning)
	fresh := h.pushed(t, second)
	h.wait(t, old.ID)
	h.wait(t, fresh.ID)
	got := p.of("1")
	if last := got[len(got)-1]; last.State != "error" || last.SHA != first {
		t.Fatalf("the superseded build posted %+v, want an error on its own commit", got)
	}
	for _, s := range p.of("2") {
		if s.SHA != second || s.State == "error" {
			t.Fatalf("the replacement posted %+v", s)
		}
	}
}

func TestACommitStatusPostedAtTheQueueHoldsUpNoOtherBuildAndIsTheReplacementsFirst(t *testing.T) {
	r := newRepo(t)
	sha := r.commit("main", map[string]string{".kipp.yml": oneStage("sleep 30")})
	p := &poster{}
	entered, release := make(chan struct{}), make(chan struct{})
	var gaveUp atomic.Bool
	h := newHarness(t, map[string]string{"site": supersedeParent("file://" + r.dir), "other": "name: other\n" + oneStage("true")}, func(o *Options) {
		o.Status = func(ctx context.Context, job *config.Job, s CommitStatus) error {
			if s.Description == "site.main #2 queued" {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					gaveUp.Store(true)
				}
			}
			return p.post(ctx, job, s)
		}
	})
	h.withChildren(t, config.ChildRecord{Key: "main", Branch: "main"})
	old := h.pushed(t, sha)
	h.untilStage(t, old.ID, "s", store.StageRunning)
	queued := make(chan *store.Build)
	go func() { queued <- h.pushed(t, sha) }()
	<-entered
	if b := h.wait(t, h.trigger(t, "other").ID); b.Status != store.Success || gaveUp.Load() {
		t.Fatalf("a build of another job ended %s, the post given up on: %v; it waits for no commit status", b.Status, gaveUp.Load())
	}
	h.wait(t, old.ID)
	eventually(t, "the replacement has cloned its commit", func() bool {
		builds, err := h.st.Builds(context.Background(), "site.main", 5)
		return err == nil && len(builds) == 2 && builds[0].BuiltCommit == sha
	})
	close(release)
	fresh := <-queued
	h.untilStage(t, fresh.ID, "s", store.StageRunning)
	eventually(t, "the replacement said that it runs", func() bool { return len(p.of("2")) == 2 })
	if got := p.of("2"); got[0].Description != "site.main #2 queued" || got[1].Description != "site.main #2 running" {
		t.Fatalf("the replacement posted %+v, want queued before running", got)
	}
}
