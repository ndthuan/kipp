package store

import "testing"

func TestPreviousCommitsAreTheLatestEarlierBuildsWithACommitAndTheLatestSuccessful(t *testing.T) {
	s := open(t)
	add := func(job string, status Status, built, pushed string) *Build {
		b := &Build{Job: job, Status: status, Trigger: "webhook", TriggerDetail: pushed, BuiltCommit: built}
		must(t, s.CreateBuild(ctx, b))
		return b
	}
	add("a", Success, "", "p1")
	add("a", Failed, "b2", "p2") // the commit it checked out wins over the push's
	add("a", Success, "", "")    // no commit
	add("other", Success, "", "elsewhere")
	cur := add("a", Running, "", "p4")
	last, ok, err := s.PreviousCommits(ctx, "a", cur.ID)
	must(t, err)
	if last != "b2" || ok != "p1" {
		t.Fatalf("last %q, successful %q, want b2 and p1", last, ok)
	}
	first, firstOK, err := s.PreviousCommits(ctx, "a", 1)
	must(t, err)
	if first != "" || firstOK != "" {
		t.Fatalf("before the first build: %q %q", first, firstOK)
	}
}

func TestPreviousCommitsLookPastAPageOfBuildsWithNone(t *testing.T) {
	s := open(t)
	old := &Build{Job: "a", Status: Success, Trigger: "webhook", TriggerDetail: "old"}
	must(t, s.CreateBuild(ctx, old))
	for range 120 {
		must(t, s.CreateBuild(ctx, &Build{Job: "a", Status: Failed, Trigger: "cron"}))
	}
	cur := &Build{Job: "a", Status: Running, Trigger: "cron"}
	must(t, s.CreateBuild(ctx, cur))
	last, ok, err := s.PreviousCommits(ctx, "a", cur.ID)
	must(t, err)
	if last != "old" || ok != "old" {
		t.Fatalf("last %q, successful %q", last, ok)
	}
}
