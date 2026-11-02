package index

import (
	"strings"
	"testing"

	"github.com/ndthuan/kipp/internal/config"
)

// pr is a pull request event of acme/site from its own repo, #7 from
// feature-x into main.
func pr(action, sha string) PullRequestEvent {
	return PullRequestEvent{Repo: "acme/site", DefaultBranch: "main", Action: action, Number: 7, Title: "Add login",
		Author: "ann", URL: "https://github.example/acme/site/pull/7",
		HeadRef: "feature-x", HeadSHA: sha, HeadRepo: "acme/site", BaseRef: "main"}
}

func (f *fixture) pull(ev PullRequestEvent) *TriggerPlan {
	f.t.Helper()
	plan, err := f.x.PullRequest(ctx, "site", ev)
	must(f.t, err)
	return plan
}

// jobs are the names of the builds a plan queues.
func jobs(plan *TriggerPlan) []string {
	var out []string
	for _, b := range plan.Builds {
		out = append(out, b.Job)
	}
	return out
}

func TestSynchronizeBuildsAgainAndPushToItsBranchDoesNotByDefault(t *testing.T) {
	f := newFixture(t, map[string]string{"site": site})
	plan := f.pull(pr("opened", head))
	if got := jobs(plan); len(got) != 1 || got[0] != "site.pr-7" || plan.Builds[0].Detail != head || plan.Builds[0].Branch != "feature-x" {
		t.Fatalf("opened: %+v", plan)
	}
	job := f.jobSet().Jobs["site.pr-7"]
	if job == nil || job.Origin.Kind != config.KindPR || job.Origin.BaseBranch != "main" || job.Description != "Add login" || job.OnDefaultBranch() {
		t.Fatalf("the pull request's child: %+v", job)
	}
	if plan := f.pull(pr("synchronize", head2)); len(plan.Builds) != 1 || plan.Builds[0].Job != "site.pr-7" || plan.Builds[0].Detail != head2 {
		t.Fatalf("synchronize: %+v", plan)
	}
	plan = f.push("feature-x", head2)
	if len(plan.Builds) != 0 || !strings.Contains(plan.Ignored, "has an open pull request") {
		t.Fatalf("a push to the pull request's branch: %+v", plan)
	}
	if plan := f.push("main", head2); len(plan.Builds) != 1 || plan.Builds[0].Job != "site.main" {
		t.Fatalf("the default branch builds whatever pull request it heads or is the base of: %+v", plan)
	}
	if plan := f.pull(pr("labeled", head2)); len(plan.Builds) != 0 || plan.Ignored == "" {
		t.Fatalf("a label builds: %+v", plan)
	}
}

func TestBranchWithPRBuildBuildsBoth(t *testing.T) {
	f := newFixture(t, map[string]string{"site": "name: site\nmultibranch: {repo: acme/site, branch_with_pr: build}\n"})
	f.pull(pr("opened", head))
	plan := f.push("feature-x", head2)
	if got := jobs(plan); len(got) != 1 || got[0] != "site.feature-x" {
		t.Fatalf("a push to a branch with a pull request, branch_with_pr build: %+v", plan)
	}
	if got := jobs(f.pull(pr("synchronize", head2))); len(got) != 1 || got[0] != "site.pr-7" {
		t.Fatalf("its synchronize: %v", got)
	}
	// Once the pull request is closed the branch is one branches does not name.
	f.pull(pr("closed", head2))
	if plan := f.push("feature-x", head); len(plan.Builds) != 0 {
		t.Fatalf("a branch whose pull request closed builds: %+v", plan)
	}
}

func TestBothModeMakesTwoChildren(t *testing.T) {
	f := newFixture(t, map[string]string{"site": "name: site\nmultibranch: {repo: acme/site, pull_requests: both}\n"})
	plan := f.pull(pr("opened", head))
	if got := jobs(plan); len(got) != 2 || got[0] != "site.pr-7" || got[1] != "site.pr-7-head" {
		t.Fatalf("%v", got)
	}
	set := f.jobSet()
	if set.Jobs["site.pr-7"].Origin.Kind != config.KindPR || set.Jobs["site.pr-7-head"].Origin.Kind != config.KindPRHead {
		t.Fatalf("the kinds: %+v %+v", set.Jobs["site.pr-7"].Origin, set.Jobs["site.pr-7-head"].Origin)
	}
	f.pull(pr("closed", head))
	for _, name := range []string{"site.pr-7", "site.pr-7-head"} {
		if !f.jobSet().Jobs[name].Disabled {
			t.Fatalf("%s outlived its closed pull request", name)
		}
	}
	head := newFixture(t, map[string]string{"site": "name: site\nmultibranch: {repo: acme/site, pull_requests: head}\n"})
	plan = head.pull(pr("opened", "0123456789abcdef0123456789abcdef01234567"))
	if got := jobs(plan); len(got) != 1 || got[0] != "site.pr-7" || head.jobSet().Jobs["site.pr-7"].Origin.Kind != config.KindPRHead {
		t.Fatalf("head mode makes one child that builds the head: %v", got)
	}
}

func TestAForkPRIsNeverBuilt(t *testing.T) {
	f := newFixture(t, map[string]string{"site": "name: site\nmultibranch: {repo: acme/site, pull_requests: both, branch_with_pr: build}\n"})
	for _, from := range []string{"stranger/site", ""} {
		ev := pr("opened", head)
		ev.HeadRepo = from
		plan := f.pull(ev)
		if len(plan.Builds) != 0 || plan.Ignored != "pull request #7 is from a fork: not built" {
			t.Fatalf("from %q: %+v", from, plan)
		}
	}
	kids := f.x.Children("site")
	if len(kids) != 1 || !kids[0].FromFork || kids[0].Title != "Add login" {
		t.Fatalf("a fork's pull request is listed, once: %+v", kids)
	}
	if f.jobSet().Jobs["site.pr-7"] != nil {
		t.Fatal("a fork's pull request is a job")
	}
	// Its branch is the fork's: a push to the repo's own feature-x is not its.
	if plan := f.push("feature-x", head); len(plan.Builds) != 0 || strings.Contains(plan.Ignored, "pull request") {
		t.Fatalf("%+v", plan)
	}
	again := pr("synchronize", head2)
	again.HeadRepo = "stranger/site"
	if plan := f.pull(again); len(plan.Builds) != 0 || f.jobSet().Jobs["site.pr-7"] != nil {
		t.Fatalf("a push to the fork's pull request: %+v", plan)
	}
}

func TestClosedPRMakesItsChildGoneAndReopenedRevivesIt(t *testing.T) {
	f := newFixture(t, map[string]string{"site": site})
	f.pull(pr("opened", head))
	plan := f.pull(pr("closed", head))
	if len(plan.Builds) != 0 || plan.Ignored != "pull request #7 was closed, and site.pr-7 gone" {
		t.Fatalf("%+v", plan)
	}
	kids := f.x.Children("site")
	if len(kids) != 1 || kids[0].State != config.ChildGone || !kids[0].GoneAt.Equal(f.clock) {
		t.Fatalf("%+v", kids)
	}
	if !f.jobSet().Jobs["site.pr-7"].Disabled {
		t.Fatal("a closed pull request's child still builds")
	}
	if plan := f.pull(pr("reopened", head2)); len(plan.Builds) != 1 || plan.Builds[0].Job != "site.pr-7" || plan.Builds[0].Detail != head2 {
		t.Fatalf("reopened: %+v", plan)
	}
	if f.jobSet().Jobs["site.pr-7"].Disabled || f.x.Children("site")[0].GoneAt != nil {
		t.Fatal("a reopened pull request's child stays gone")
	}
}

func TestBaseChangeRebuildsThePR(t *testing.T) {
	f := newFixture(t, map[string]string{"site": site})
	f.pull(pr("opened", head))
	ev := pr("edited", head)
	ev.Title = "Add login, retitled"
	ev.Author, ev.URL = "bob", "https://github.example/acme/site/pull/70"
	if plan := f.pull(ev); len(plan.Builds) != 0 {
		t.Fatalf("an edit of the title builds: %+v", plan)
	}
	ev.BaseRef, ev.BaseChanged = "release", true
	plan := f.pull(ev)
	if len(plan.Builds) != 1 || plan.Builds[0].Job != "site.pr-7" {
		t.Fatalf("a new base: %+v", plan)
	}
	if o := f.jobSet().Jobs["site.pr-7"].Origin; o.Author != "bob" || o.URL != "https://github.example/acme/site/pull/70" {
		t.Fatalf("an event fills the author and page again: %+v", o)
	}
	if job := f.jobSet().Jobs["site.pr-7"]; job.Origin.BaseBranch != "release" || job.Description != "Add login, retitled" {
		t.Fatalf("the child merges into %q: %+v", job.Origin.BaseBranch, job.Origin)
	}
}

func TestRebuildOnBaseQueuesOpenPRsOfThatBase(t *testing.T) {
	f := newFixture(t, map[string]string{"site": "name: site\nmultibranch: {repo: acme/site, rebuild_on_base: true}\n"})
	f.pull(pr("opened", head))
	other := pr("opened", head2)
	other.Number, other.HeadRef, other.BaseRef = 8, "feature-y", "release"
	f.pull(other)
	gone := pr("opened", head)
	gone.Number, gone.HeadRef = 9, "feature-z"
	f.pull(gone)
	f.pull(PullRequestEvent{Repo: "acme/site", Action: "closed", Number: 9})
	plan := f.push("main", head2)
	if got := jobs(plan); len(got) != 2 || got[0] != "site.main" || got[1] != "site.pr-7" || plan.Builds[1].Detail != head {
		t.Fatalf("a push to main rebuilds main and its open pull request, at the head it last saw: %+v", plan)
	}
	if got := jobs(newFixtureWith(t, site).push("main", head)); len(got) != 1 {
		t.Fatalf("without rebuild_on_base: %v", got)
	}
	// A base no branches name still rebuilds its pull requests.
	plan = f.push("release", head)
	if got := jobs(plan); len(got) != 1 || got[0] != "site.pr-8" || !strings.Contains(plan.Ignored, "not built") {
		t.Fatalf("a push to release: %+v", plan)
	}
}

// newFixtureWith is a fixture whose one job is site, with an open pull
// request #7.
func newFixtureWith(t *testing.T, body string) *fixture {
	f := newFixture(t, map[string]string{"site": body})
	f.pull(pr("opened", head))
	return f
}

func TestPullRequestsOffIgnoresPRs(t *testing.T) {
	f := newFixture(t, map[string]string{"site": "name: site\nmultibranch: {repo: acme/site, branches: [\"*\"], pull_requests: off}\n"})
	plan := f.pull(pr("opened", head))
	if len(plan.Builds) != 0 || plan.Ignored != "multibranch.pull_requests is off" || len(f.x.Children("site")) != 0 {
		t.Fatalf("%+v", plan)
	}
	if got := jobs(f.push("feature-x", head)); len(got) != 1 || got[0] != "site.feature-x" {
		t.Fatalf("the branches build as branches: %v", got)
	}
	if plan, _ := f.x.PullRequest(ctx, "site", PullRequestEvent{Repo: "acme/other", Action: "opened", Number: 1}); plan.Ignored == "" {
		t.Fatal("a pull request of another repo")
	}
}
