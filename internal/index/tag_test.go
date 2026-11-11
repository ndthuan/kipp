package index

import (
	"strings"
	"testing"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

func (f *fixture) pushTag(tag, commit string, deleted bool) *TriggerPlan {
	f.t.Helper()
	plan, err := f.x.Push(ctx, "site", PushEvent{Repo: "acme/site", Ref: "refs/tags/" + tag, After: commit, Deleted: deleted, DefaultBranch: "main"})
	must(f.t, err)
	return plan
}

func TestATagPushIsBuiltByTheTagsChildAtTheTaggedCommit(t *testing.T) {
	f := newFixture(t, map[string]string{"site": site})
	plan := f.pushTag("v1.2.0", head, false)
	if len(plan.Builds) != 1 {
		t.Fatalf("plan %+v", plan)
	}
	b := plan.Builds[0]
	if b.Job != "site.tags" || b.Kind != "webhook" || b.Detail != head || b.Tag != "v1.2.0" || b.Branch != "" || b.DefaultBranch != "main" {
		t.Fatalf("build %+v", b)
	}
	job := f.jobSet().Jobs["site.tags"]
	if job == nil || !job.BuildsTags() || job.OnDefaultBranch() || job.Parent != "site" || job.Disabled {
		t.Fatalf("the tags' child is not in the job set: %+v", job)
	}
	again := f.pushTag("v1.3.0", head2, false)
	if len(again.Builds) != 1 || again.Builds[0].Job != "site.tags" || again.Builds[0].Tag != "v1.3.0" {
		t.Fatalf("a second tag: %+v", again)
	}
	if kids := f.x.Children("site"); len(kids) != 1 || kids[0].Key != config.TagsKey || kids[0].HeadSHA != head2 {
		t.Fatalf("every tag is one child: %+v", kids)
	}
}

func TestATagDeletionOrATagTheFilterLeavesOutBuildsNothing(t *testing.T) {
	f := newFixture(t, map[string]string{"site": "name: site\nmultibranch: {repo: acme/site, tags: {include: [\"v*\"], exclude: [\"*-rc*\"]}}\n"})
	cases := map[string]*TriggerPlan{
		"deleted":         f.pushTag("v1", "", true),
		"outside include": f.pushTag("nightly", head, false),
		"inside exclude":  f.pushTag("v2-rc1", head, false),
	}
	for name, plan := range cases {
		if len(plan.Builds) != 0 || !strings.Contains(plan.Ignored, "tag ") {
			t.Errorf("%s: %+v", name, plan)
		}
	}
	if plan := f.pushTag("v2", head, false); len(plan.Builds) != 1 {
		t.Fatalf("a tag the filter builds: %+v", plan)
	}
	off := newFixture(t, map[string]string{"site": "name: site\nmultibranch: {repo: acme/site, tags: false}\n"})
	if plan := off.pushTag("v1", head, false); len(plan.Builds) != 0 {
		t.Fatalf("tags: false built %+v", plan)
	}
	if kids := off.x.Children("site"); len(kids) != 0 {
		t.Fatalf("tags: false made a child: %+v", kids)
	}
}

func TestABranchPushNeverMakesATagBuild(t *testing.T) {
	f := newFixture(t, map[string]string{"site": site})
	plan := f.push("main", head)
	if len(plan.Builds) != 1 || plan.Builds[0].Tag != "" || plan.Builds[0].Job != "site.main" {
		t.Fatalf("plan %+v", plan)
	}
	if f.jobSet().Jobs["site.tags"] != nil {
		t.Fatal("a branch push made the tags' child")
	}
}

func TestAScanLeavesTheTagsChildLive(t *testing.T) {
	f := newFixture(t, map[string]string{"site": site})
	f.pushTag("v1", head, false)
	g := newFakeGitHub()
	f.scan(g)
	for _, c := range f.x.Children("site") {
		if c.Kind == config.KindTag && c.State != config.ChildLive {
			t.Fatalf("the scan made the tags' child %s", c.State)
		}
	}
}

func TestATagsBuildOfTheSameCommitIsOnItsWayOnlyForTheSameTag(t *testing.T) {
	f := newFixture(t, map[string]string{"site": site})
	b := &store.Build{Job: "site.tags", Status: store.Queued, Trigger: "webhook", TriggerDetail: head, TriggerTag: "v1"}
	must(t, f.st.CreateBuild(ctx, b))
	for tag, want := range map[string]bool{"v1": true, "latest": false, "": false} {
		on, err := f.x.OnItsWay(ctx, "site.tags", head, tag)
		must(t, err)
		if on != want {
			t.Errorf("tag %q: on its way %v, want %v", tag, on, want)
		}
	}
}
