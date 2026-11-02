package store

import (
	"testing"
	"time"
)

func TestARepoRowOfAnOwnerJobIsKeptAndItsChildrenAreFoundThroughIt(t *testing.T) {
	s := open(t)
	seen := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	site := OwnerRepo{Owner: "acme", Key: "site", Repo: "acme/site", State: "live", LastSeenAt: seen, Hook: HookOK, HookFP: "fp"}
	must(t, s.PutOwnerRepo(ctx, site))
	must(t, s.PutOwnerRepo(ctx, OwnerRepo{Owner: "acme", Key: "api", Repo: "acme/api", State: "live", LastSeenAt: seen, Hook: HookOff, HookNote: "no Webhooks permission"}))
	gone := seen.Add(time.Hour)
	site.State, site.GoneAt = "gone", &gone
	must(t, s.PutOwnerRepo(ctx, site))
	rows, err := s.OwnerRepos(ctx, "acme")
	must(t, err)
	if len(rows) != 2 || rows[0].Key != "api" || rows[0].HookNote != "no Webhooks permission" || rows[1].State != "gone" ||
		rows[1].GoneAt == nil || !rows[1].GoneAt.Equal(gone) || rows[1].HookFP != "fp" || rows[1].Node() != "acme.site" {
		t.Fatalf("%+v", rows)
	}
	must(t, s.PutChild(ctx, Child{Parent: "acme.site", Key: "pr-3", Kind: "pr", PR: 3, State: "live", LastSeenAt: seen}))
	for job, want := range map[string]string{"acme.site": "acme", "acme.site.pr-3": "acme", "acme.api": "acme", "acme": "", "acme.site.other": "", "x.y": ""} {
		if got, err := s.FolderOf(ctx, job); err != nil || got != want {
			t.Errorf("FolderOf(%s) = %q, %v; want %q", job, got, err, want)
		}
	}
	all, err := s.Folders(ctx)
	must(t, err)
	if len(all) != 3 || all["acme.site.pr-3"] != "acme" || all["acme.api"] != "acme" {
		t.Fatalf("folders %v", all)
	}
	// The row stays while a child of it does, and goes with the last one.
	must(t, s.DeleteOwnerRepo(ctx, "acme", "site"))
	if rows, _ := s.OwnerRepos(ctx, "acme"); len(rows) != 2 {
		t.Fatal("a repo with a child lost its row")
	}
	must(t, s.DeleteChild(ctx, "acme.site", "pr-3"))
	must(t, s.SetDefaultBranch(ctx, "acme.site", "main", seen))
	must(t, s.DeleteOwnerRepo(ctx, "acme", "site"))
	rows, err = s.OwnerRepos(ctx, "acme")
	must(t, err)
	if len(rows) != 1 || rows[0].Key != "api" {
		t.Fatalf("%+v", rows)
	}
	if d, _ := s.DefaultBranches(ctx); d["acme.site"] != "" {
		t.Fatalf("the default branch of a deleted repo stayed: %v", d)
	}
}
