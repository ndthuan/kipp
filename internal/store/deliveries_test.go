package store

import (
	"errors"
	"testing"
	"time"
)

func TestAKeptDeliveryIsRecordedAndKeptTogetherAndListedOldestFirst(t *testing.T) {
	s := open(t)
	at := time.Date(2026, 11, 9, 7, 0, 0, 0, time.UTC)
	first := PendingDelivery{Route: "job", Name: "a", Event: "push", ID: "d-1", Body: []byte("one"), ReceivedAt: at}
	second := PendingDelivery{Route: "app", Name: "ci", Event: "push", ID: "d-2", Body: []byte("two"), ReceivedAt: at.Add(time.Minute)}
	must(t, s.KeepDelivery(ctx, first, []string{"a/d-1", "body:1"}, "a"))
	must(t, s.KeepDelivery(ctx, second, []string{"app:ci/d-2"}, "app:ci"))

	// A recorded id keeps nothing, and records none of its siblings.
	dup := PendingDelivery{Route: "job", Name: "a", Event: "push", ID: "d-3", Body: []byte("three"), ReceivedAt: at}
	if err := s.KeepDelivery(ctx, dup, []string{"a/d-3", "body:1"}, "a"); !errors.Is(err, ErrSeen) {
		t.Fatalf("a body kept before: %v", err)
	}
	if err := s.RecordDelivery(ctx, "a/d-3", "a", at); err != nil {
		t.Fatalf("the refused keeping left an id recorded: %v", err)
	}
	if err := s.RecordDelivery(ctx, "a/d-1", "a", at); !errors.Is(err, ErrSeen) {
		t.Fatalf("a kept delivery's id is recorded: %v", err)
	}

	got, err := s.PendingDeliveries(ctx)
	must(t, err)
	if len(got) != 2 || got[0].ID != "d-1" || got[1].ID != "d-2" || string(got[0].Body) != "one" || !got[1].ReceivedAt.Equal(second.ReceivedAt) || got[1].Route != "app" {
		t.Fatalf("%+v", got)
	}
	must(t, s.DeletePendingDelivery(ctx, got[0].Seq))
	got, err = s.PendingDeliveries(ctx)
	must(t, err)
	if len(got) != 1 || got[0].ID != "d-2" {
		t.Fatalf("%+v", got)
	}
}
