package store

import "testing"

func TestAKeptAfterTriggerWaitsUntilDroppedInTheOrderItWasKept(t *testing.T) {
	s := open(t)
	first := &Build{Job: "build", Status: Success, Trigger: "api"}
	must(t, s.CreateBuild(ctx, first))
	second := &Build{Job: "test", Status: Failed, Trigger: "api"}
	must(t, s.CreateBuild(ctx, second))
	must(t, s.KeepAfter(ctx, second.ID, "notify"))
	must(t, s.KeepAfter(ctx, first.ID, "deploy"))
	must(t, s.KeepAfter(ctx, first.ID, "deploy"))

	pending, err := s.PendingAfters(ctx)
	must(t, err)
	if len(pending) != 2 || pending[0].Upstream.ID != second.ID || pending[0].Job != "notify" ||
		pending[1].Upstream.ID != first.ID || pending[1].Job != "deploy" {
		t.Fatalf("pending afters %+v, want notify after test, then deploy after build", pending)
	}
	if pending[0].Upstream.Status != Failed {
		t.Fatalf("the upstream reads %v, want how it ended", pending[0].Upstream.Status)
	}
	must(t, s.DropAfter(ctx, second.ID, "notify"))
	pending, err = s.PendingAfters(ctx)
	must(t, err)
	if len(pending) != 1 || pending[0].Job != "deploy" {
		t.Fatalf("pending afters %+v, want deploy alone", pending)
	}
}

func TestAKeptAfterTriggerGoesWithItsUpstreamBuild(t *testing.T) {
	s := open(t)
	b := &Build{Job: "build", Status: Success, Trigger: "api"}
	must(t, s.CreateBuild(ctx, b))
	must(t, s.KeepAfter(ctx, b.ID, "deploy"))
	must(t, s.DeleteBuild(ctx, b.ID))
	pending, err := s.PendingAfters(ctx)
	must(t, err)
	if len(pending) != 0 {
		t.Fatalf("pending afters %+v of a removed build", pending)
	}
}
