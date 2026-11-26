package enginetest

import (
	"context"
	"testing"

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
	f.Wait(t, "the server is asked to be quiet", func() bool { return len(f.QuietCalls()) > 0 })
	f.SetBusy(0)
	f.Wait(t, "the server is asked to restart", func() bool { return f.Switched.Load() > 0 })
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
