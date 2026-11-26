package index

import (
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/store"
)

const (
	oldBase = "c5a9762c5a9762c5a9762c5a9762c5a9762c5a97"
	newBase = "ee9808bee9808bee9808bee9808bee9808bee9808"
)

// A base push that lands while a build of the pull request's head runs on
// the old base has to be built again on the new one: the running build
// merged the old base, so it is not on its way to the new one.
func TestABasePushDuringAPRBuildOnTheOldBaseIsRebuilt(t *testing.T) {
	f := newFixture(t, map[string]string{"site": "name: site\nmultibranch: {repo: acme/site, rebuild_on_base: true}\n"})
	f.pull(pr("opened", head))
	running := &store.Build{Job: "site.pr-7", Status: store.Running, Trigger: "webhook", TriggerDetail: head,
		BuiltCommit: head, BaseCommit: oldBase, PR: 7, BaseBranch: "main"}
	must(t, f.st.CreateBuild(ctx, running))

	plan := f.push("main", newBase)
	if plan.Base != newBase {
		t.Fatalf("the plan does not name the base it rebuilds on: %q", plan.Base)
	}
	cases := []struct {
		name  string
		base  string
		queue bool
		want  bool
	}{
		{"running on the old base", newBase, false, false},
		{"a delivery with no base, as before", "", false, true},
		{"running on the same base", oldBase, false, true},
	}
	for _, c := range cases {
		on, err := f.x.OnItsWay(ctx, "site.pr-7", head, "", c.base)
		must(t, err)
		if on != c.want {
			t.Errorf("%s: on its way %v, want %v", c.name, on, c.want)
		}
	}
}

// A queued build has not cloned yet, so it merges the base as it is then,
// and a running one that has not recorded its base has not cloned either.
func TestABuildThatHasNotMergedABaseIsOnItsWayOnAnyBase(t *testing.T) {
	f := newFixture(t, map[string]string{"site": "name: site\nmultibranch: {repo: acme/site, rebuild_on_base: true}\n"})
	f.pull(pr("opened", head))
	for _, st := range []store.Status{store.Queued, store.Running} {
		b := &store.Build{Job: "site.pr-7", Status: st, Trigger: "webhook", TriggerDetail: head, PR: 7, BaseBranch: "main"}
		must(t, f.st.CreateBuild(ctx, b))
		on, err := f.x.OnItsWay(ctx, "site.pr-7", head, "", newBase)
		must(t, err)
		if !on {
			t.Errorf("%s build with no base merged yet is not on its way", st)
		}
		must(t, f.st.SetBuildStatus(ctx, b.ID, store.Success, time.Now()))
	}
}
