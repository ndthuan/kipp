package store

import (
	"testing"
	"time"
)

func TestARerunWaitsUntilItIsLinkedAndTheLinkReadsBothWays(t *testing.T) {
	s := open(t)
	old := &Build{Job: "flow", Status: Cancelled, Trigger: "webhook"}
	must(t, s.CreateBuild(ctx, old))
	other := &Build{Job: "deploy", Status: Cancelled, Trigger: "api"}
	must(t, s.CreateBuild(ctx, other))
	must(t, s.KeepRerun(ctx, other.ID))
	must(t, s.KeepRerun(ctx, old.ID))
	must(t, s.KeepRerun(ctx, old.ID)) // twice is once

	pending, err := s.PendingReruns(ctx)
	must(t, err)
	if len(pending) != 2 || pending[0].ID != old.ID || pending[1].ID != other.ID {
		t.Fatalf("pending reruns %+v, want flow then deploy, in the order they were queued", pending)
	}

	again := &Build{Job: "flow", Status: Queued, Trigger: "webhook", RerunOf: old.Number, QueuedAt: time.Now()}
	must(t, s.CreateBuild(ctx, again))
	must(t, s.LinkRerun(ctx, old.ID, again.Number))
	must(t, s.DropRerun(ctx, other.ID))

	pending, err = s.PendingReruns(ctx)
	must(t, err)
	if len(pending) != 0 {
		t.Fatalf("still pending: %+v", pending)
	}
	o, err := s.Build(ctx, old.ID)
	must(t, err)
	n, err := s.Build(ctx, again.ID)
	must(t, err)
	if o.RerunAs != again.Number || n.RerunOf != old.Number {
		t.Fatalf("the stopped build reads rerun as #%d, the new one rerun of #%d", o.RerunAs, n.RerunOf)
	}
}

func TestARerunGoesWithItsBuild(t *testing.T) {
	s := open(t)
	b := &Build{Job: "flow", Status: Cancelled, Trigger: "api"}
	must(t, s.CreateBuild(ctx, b))
	must(t, s.KeepRerun(ctx, b.ID))
	must(t, s.DeleteBuild(ctx, b.ID))
	pending, err := s.PendingReruns(ctx)
	must(t, err)
	if len(pending) != 0 {
		t.Fatalf("a removed build still waits to run again: %+v", pending)
	}
}
