package enginetest

import (
	"context"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/update"
	"github.com/ndthuan/kipp/internal/upgrade/engine"
	"github.com/ndthuan/kipp/internal/upgrade/state"
)

func TestTheFixtureRunsAnEngineThroughAnUpgradeToTheSwitch(t *testing.T) {
	rel := NewRelease(t, GoodBinary(Newer))
	f := NewServer()
	f.SetQuietOn(false)
	f.SetBusy(1)
	e, data := Engine(t, rel, f, nil)
	if _, err := e.Start(context.Background(), engine.Request{Safe: true}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(f.QuietCalls()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("never quiet")
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.SetBusy(0)
	for f.Switched.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("never switched")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if p, err := state.ReadPending(data); err != nil || p == nil || p.Version != Newer {
		t.Fatalf("%+v, %v", p, err)
	}
	if rel.Hits.Load() < 4 {
		t.Fatalf("%d requests", rel.Hits.Load())
	}
	if c := rel.Checker(Running); c.Status().Latest != Newer || update.Release(Running) == false {
		t.Fatal("the checker does not know the release")
	}
}
