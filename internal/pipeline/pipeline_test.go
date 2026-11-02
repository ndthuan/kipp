package pipeline

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func lintUnitSmoke() map[string][]string {
	return map[string][]string{"lint": nil, "unit": nil, "smoke": {"lint", "unit"}}
}

func TestRootsAreReadyAtOnceAndDependantsWait(t *testing.T) {
	d, err := New(lintUnitSmoke(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Ready(); !reflect.DeepEqual(got, []string{"lint", "unit"}) {
		t.Fatalf("ready %v", got)
	}
	d.Start("lint")
	d.Start("unit")
	if got := d.Ready(); len(got) != 0 {
		t.Fatalf("ready while running: %v", got)
	}
	d.Finish("lint", Succeeded)
	if got := d.Ready(); len(got) != 0 {
		t.Fatalf("smoke ready before unit finished: %v", got)
	}
	d.Finish("unit", Succeeded)
	if got := d.Ready(); !reflect.DeepEqual(got, []string{"smoke"}) {
		t.Fatalf("ready %v", got)
	}
	d.Start("smoke")
	d.Finish("smoke", Succeeded)
	if !d.Done() || d.Failed() {
		t.Fatalf("done %v failed %v", d.Done(), d.Failed())
	}
}

func TestAFailureSkipsWhatDependsOnItAndOtherBranchesGoOn(t *testing.T) {
	d, err := New(map[string][]string{"a": nil, "b": nil, "c": {"a"}, "d": {"c"}, "e": {"b"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	d.Start("a")
	d.Start("b")
	skipped := d.Finish("a", Failed)
	if !reflect.DeepEqual(skipped, []string{"c", "d"}) {
		t.Fatalf("skipped %v", skipped)
	}
	d.Finish("b", Succeeded)
	if got := d.Ready(); !reflect.DeepEqual(got, []string{"e"}) {
		t.Fatalf("ready %v", got)
	}
	d.Start("e")
	d.Finish("e", Succeeded)
	if !d.Done() || !d.Failed() {
		t.Fatalf("done %v failed %v", d.Done(), d.Failed())
	}
}

// The stage both branches lead to is skipped once, and only when one of
// them failed: the branch that succeeded does not release it.
func TestAStageWithTwoNeedsIsSkippedWhenEitherFails(t *testing.T) {
	d, err := New(map[string][]string{"root": nil, "left": {"root"}, "right": {"root"}, "join": {"left", "right"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	d.Start("root")
	d.Finish("root", Succeeded)
	d.Start("left")
	d.Start("right")
	if skipped := d.Finish("left", Succeeded); skipped != nil {
		t.Fatalf("skipped after a success: %v", skipped)
	}
	if got := d.Ready(); len(got) != 0 {
		t.Fatalf("join ready with right still running: %v", got)
	}
	if skipped := d.Finish("right", Failed); !reflect.DeepEqual(skipped, []string{"join"}) {
		t.Fatalf("skipped %v", skipped)
	}
	if !d.Done() || !d.Failed() {
		t.Fatalf("done %v failed %v", d.Done(), d.Failed())
	}
}

// A map is walked in no order, and an error that names a different stage
// each time reads as a different error.
func TestValidateNamesTheFirstBadStageByNameEveryTime(t *testing.T) {
	needs := map[string][]string{"d": {"zzz"}, "c": {"zzz"}, "b": {"zzz"}, "a": {"zzz"}}
	for range 50 {
		err := Validate(needs)
		if err == nil || !strings.Contains(err.Error(), `stage "a"`) {
			t.Fatalf("got %v, want the error to name stage a", err)
		}
	}
}

func TestValidateRefusesCyclesAndUnknownNeeds(t *testing.T) {
	if err := Validate(map[string][]string{"a": {"b"}, "b": {"a"}}); err == nil {
		t.Fatal("cycle accepted")
	}
	if err := Validate(map[string][]string{"a": {"zzz"}}); err == nil {
		t.Fatal("unknown need accepted")
	}
	if err := Validate(map[string][]string{"a": {"a"}}); err == nil {
		t.Fatal("self need accepted")
	}
	if err := Validate(lintUnitSmoke()); err != nil {
		t.Fatal(err)
	}
}

// "z" has no needs and "a" needs it, so depth and the alphabet disagree.
func TestNamesAreOrderedByDepthThenByName(t *testing.T) {
	d, err := New(map[string][]string{"z": nil, "m": nil, "a": {"z"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.Depth("z") != 0 || d.Depth("a") != 1 {
		t.Fatalf("z %d a %d", d.Depth("z"), d.Depth("a"))
	}
	if got := d.Names(); !reflect.DeepEqual(got, []string{"m", "z", "a"}) {
		t.Fatalf("names %v", got)
	}
}

// An unstable stage releases what needs it, as a success does, and is
// still told apart from one when the build is summed up.
func TestAnUnstableStageReleasesItsDependantsAndIsCounted(t *testing.T) {
	d, err := New(map[string][]string{"a": nil, "b": {"a"}, "c": {"b"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	d.Start("a")
	if skipped := d.Finish("a", Unstable); skipped != nil {
		t.Fatalf("an unstable stage skipped %v", skipped)
	}
	if got := d.Ready(); !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("ready after an unstable need: %v", got)
	}
	d.Start("b")
	d.Finish("b", Succeeded)
	d.Start("c")
	d.Finish("c", Succeeded)
	if !d.Done() || d.Failed() || !d.Unstable() {
		t.Fatalf("done %v failed %v unstable %v", d.Done(), d.Failed(), d.Unstable())
	}
}

func TestOnlyAStageThatEndedUnstableMakesTheDAGUnstable(t *testing.T) {
	d, err := New(map[string][]string{"a": nil, "b": nil, "c": {"b"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.Unstable() {
		t.Fatal("unstable before any stage ran")
	}
	d.Start("a")
	d.Start("b")
	d.Finish("a", Succeeded)
	if d.Unstable() {
		t.Fatal("unstable after a success")
	}
	d.Finish("b", Failed)
	if d.Unstable() || !d.Failed() {
		t.Fatalf("after a failure and its skip: unstable %v failed %v", d.Unstable(), d.Failed())
	}
}

// A failure beside an unstable stage is still a failure: both are told.
func TestAFailureAndAnUnstableStageAreBothReported(t *testing.T) {
	d, err := New(map[string][]string{"a": nil, "b": nil, "c": {"a", "b"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	d.Start("a")
	d.Start("b")
	d.Finish("a", Unstable)
	if skipped := d.Finish("b", Failed); !reflect.DeepEqual(skipped, []string{"c"}) {
		t.Fatalf("skipped %v: an unstable need does not make up for a failed one", skipped)
	}
	if !d.Failed() || !d.Unstable() || !d.Done() {
		t.Fatalf("failed %v unstable %v done %v", d.Failed(), d.Unstable(), d.Done())
	}
}

func TestAnOutcomeNotSetIsAFailure(t *testing.T) {
	var how Outcome
	if how != Failed {
		t.Fatal("the zero Outcome must be Failed: a stage nobody vouched for did not succeed")
	}
}

// A stage whose condition does not hold is skipped before it runs, with
// what needs it; the rest runs, and a build of skipped stages has not failed.
func TestASkippedStageSkipsWhatNeedsItAndFailsNothing(t *testing.T) {
	d, err := New(map[string][]string{"ci": nil, "deploy": {"ci"}, "smoke": {"deploy"}, "lint": nil}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Skip("deploy"); !reflect.DeepEqual(got, []string{"deploy", "smoke"}) {
		t.Fatalf("skipped %v", got)
	}
	if got := d.Skip("deploy"); got != nil {
		t.Fatalf("a stage skipped twice gave %v", got)
	}
	if got := d.Ready(); !reflect.DeepEqual(got, []string{"ci", "lint"}) {
		t.Fatalf("ready %v", got)
	}
	d.Start("ci")
	d.Start("lint")
	if got := d.Skip("ci"); got != nil {
		t.Fatalf("a running stage was skipped: %v", got)
	}
	d.Finish("ci", Succeeded)
	d.Finish("lint", Succeeded)
	if !d.Done() || d.Failed() || d.Unstable() {
		t.Fatalf("done %v failed %v unstable %v", d.Done(), d.Failed(), d.Unstable())
	}
}

// report runs after unit, whatever needs_status says of it; a cleanup on
// failure, beside it, runs only once unit failed.
func TestNeedsStatusFailureRunsAfterAFailedNeed(t *testing.T) {
	d, err := New(map[string][]string{"unit": nil, "lint": nil, "cleanup": {"unit", "lint"}, "deploy": {"unit"}},
		map[string]string{"cleanup": NeedsFailure})
	if err != nil {
		t.Fatal(err)
	}
	d.Start("unit")
	d.Start("lint")
	if skipped := d.Finish("unit", Failed); !reflect.DeepEqual(skipped, []string{"deploy"}) {
		t.Fatalf("skipped %v", skipped)
	}
	if got := d.Ready(); len(got) != 0 {
		t.Fatalf("cleanup ready while lint still runs: %v", got)
	}
	d.Finish("lint", Succeeded)
	if got := d.Ready(); !reflect.DeepEqual(got, []string{"cleanup"}) {
		t.Fatalf("ready %v", got)
	}
	d.Start("cleanup")
	d.Finish("cleanup", Succeeded)
	if !d.Done() || !d.Failed() {
		t.Fatalf("done %v failed %v", d.Done(), d.Failed())
	}
}

// deploy is skipped because unit failed, and so counts as a failure for
// the stage that needs it.
func TestNeedsStatusFailureRunsAfterANeedSkippedByAFailure(t *testing.T) {
	d, err := New(map[string][]string{"unit": nil, "deploy": {"unit"}, "rollback": {"deploy"}},
		map[string]string{"rollback": NeedsFailure})
	if err != nil {
		t.Fatal(err)
	}
	d.Start("unit")
	if skipped := d.Finish("unit", Failed); !reflect.DeepEqual(skipped, []string{"deploy"}) {
		t.Fatalf("skipped %v", skipped)
	}
	if got := d.Ready(); !reflect.DeepEqual(got, []string{"rollback"}) {
		t.Fatalf("ready %v", got)
	}
}

// A stage its own when skipped neither failed nor succeeded: a stage that
// runs on failure has nothing to answer, and nor has what was skipped
// because of it, or what needs that.
func TestNeedsStatusFailureDoesNotRunAfterANeedItsOwnWhenSkipped(t *testing.T) {
	d, err := New(map[string][]string{"deploy": nil, "smoke": {"deploy"}, "rollback": {"smoke"}, "page": {"deploy"}, "after": {"rollback"}},
		map[string]string{"rollback": NeedsFailure, "page": NeedsFailure, "after": NeedsFailure})
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Skip("deploy"); !reflect.DeepEqual(got, []string{"deploy", "after", "page", "rollback", "smoke"}) {
		t.Fatalf("skipped %v", got)
	}
	if got := d.Ready(); len(got) != 0 {
		t.Fatalf("ready %v", got)
	}
	if !d.Done() || d.Failed() {
		t.Fatalf("done %v failed %v", d.Done(), d.Failed())
	}
}

// always runs after a success, a failure, a skip by a failure and a skip
// by a condition alike, once every need has ended.
func TestNeedsStatusAlwaysRunsAfterAnyEnd(t *testing.T) {
	for _, c := range []struct {
		name string
		end  func(d *DAG)
	}{
		{"succeeded", func(d *DAG) { d.Start("a"); d.Finish("a", Succeeded) }},
		{"unstable", func(d *DAG) { d.Start("a"); d.Finish("a", Unstable) }},
		{"failed", func(d *DAG) { d.Start("a"); d.Finish("a", Failed) }},
		{"skipped by its own when", func(d *DAG) { d.Skip("a") }},
		{"skipped by a failure", func(d *DAG) { d.Start("other"); d.Finish("other", Failed) }},
	} {
		d, err := New(map[string][]string{"other": nil, "a": {"other"}, "cleanup": {"a"}},
			map[string]string{"cleanup": NeedsAlways, "a": NeedsSuccess})
		if err != nil {
			t.Fatal(err)
		}
		if c.name != "skipped by a failure" {
			d.Start("other")
			d.Finish("other", Succeeded)
		}
		if got := d.Ready(); slices.Contains(got, "cleanup") {
			t.Fatalf("%s: cleanup ready before a ended: %v", c.name, got)
		}
		c.end(d)
		if got := d.Ready(); !slices.Contains(got, "cleanup") {
			t.Errorf("%s: ready %v, want cleanup", c.name, got)
		}
	}
}

func TestARuleKippDoesNotHaveOrWithoutNeedsIsRefused(t *testing.T) {
	for on, want := range map[string]string{"often": `"often"`, "": "needs nothing"} {
		rule := map[string]string{"b": on}
		if on == "" {
			rule = map[string]string{"a": NeedsAlways}
		}
		if _, err := New(map[string][]string{"a": nil, "b": {"a"}}, rule); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v: got %v, want %q", rule, err, want)
		}
	}
	if _, err := New(map[string][]string{"a": nil}, map[string]string{"zzz": NeedsAlways}); err == nil {
		t.Error("a rule for no stage was accepted")
	}
}

// join needs a stage its condition skipped and one still running: what it
// counts as for a stage on failure must not hang on which ends first.
func TestAStageBesideASkipAndAFailureIsSkippedAsAFailureInEitherOrder(t *testing.T) {
	for _, skipFirst := range []bool{true, false} {
		d, err := New(map[string][]string{"deploy": nil, "test": nil, "join": {"deploy", "test"}, "rollback": {"join"}},
			map[string]string{"rollback": NeedsFailure})
		if err != nil {
			t.Fatal(err)
		}
		d.Start("test")
		if skipFirst {
			d.Skip("deploy")
			d.Finish("test", Failed)
		} else {
			d.Finish("test", Failed)
			d.Skip("deploy")
		}
		if got := d.Ready(); !reflect.DeepEqual(got, []string{"rollback"}) {
			t.Errorf("skip first %v: ready %v", skipFirst, got)
		}
	}
}
