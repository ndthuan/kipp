package store

import (
	"testing"
	"time"
)

func TestAChildRowIsKeptWhileABuildOfItIs(t *testing.T) {
	s := open(t)
	seen := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	main := Child{Parent: "site", Key: "main", Kind: "branch", Branch: "main", HeadSHA: "abc", State: "live", LastSeenAt: seen}
	pr := Child{Parent: "site", Key: "pr-12", Kind: "pr", Branch: "feature/login", PR: 12, Title: "Log in", BaseBranch: "main", State: "live", LastSeenAt: seen}
	must(t, s.PutChild(ctx, main))
	must(t, s.PutChild(ctx, pr))
	gone := seen.Add(time.Hour)
	pr.State, pr.GoneAt = "gone", &gone
	must(t, s.PutChild(ctx, pr))
	kids, err := s.Children(ctx, "site")
	must(t, err)
	if len(kids) != 2 || kids[1].State != "gone" || kids[1].GoneAt == nil || !kids[1].GoneAt.Equal(gone) || kids[1].Title != "Log in" || kids[1].PR != 12 {
		t.Fatalf("%+v", kids)
	}
	for job, want := range map[string]string{"site.main": "site", "site.pr-12": "site", "site": "", "site.other": ""} {
		if got, err := s.ParentOf(ctx, job); err != nil || got != want {
			t.Errorf("ParentOf(%s) = %q, %v; want %q", job, got, err, want)
		}
	}
	must(t, s.CreateBuild(ctx, &Build{Job: "site.pr-12", Parent: "site", Status: Success, Trigger: "webhook", PR: 12, BaseBranch: "main"}))
	must(t, s.DeleteChild(ctx, "site", "pr-12"))
	must(t, s.DeleteChild(ctx, "site", "main"))
	parents, err := s.Parents(ctx)
	must(t, err)
	if len(parents) != 1 || parents["site.pr-12"] != "site" {
		t.Fatalf("the row of a child with a build went, or one without stayed: %v", parents)
	}
	must(t, s.SetDefaultBranch(ctx, "site", "trunk", seen))
	defaults, err := s.DefaultBranches(ctx)
	must(t, err)
	if defaults["site"] != "trunk" {
		t.Fatalf("%v", defaults)
	}
}

func TestABuildRecordsTheCommitsItBuilt(t *testing.T) {
	s := open(t)
	b := &Build{Job: "site.pr-12", Parent: "site", Status: Queued, Trigger: "api", PR: 12, BaseBranch: "main"}
	must(t, s.CreateBuild(ctx, b))
	must(t, s.SetBuildCommits(ctx, b.ID, "head1", "base1"))
	got, err := s.Build(ctx, b.ID)
	must(t, err)
	if got.Parent != "site" || got.BuiltCommit != "head1" || got.BaseCommit != "base1" || got.PR != 12 || got.BaseBranch != "main" {
		t.Fatalf("%+v", got)
	}
}
