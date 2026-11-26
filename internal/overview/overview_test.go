package overview

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/store"
)

var now = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

func history(job, shape string, newest time.Time) []*store.Build {
	var out []*store.Build
	for i, c := range shape {
		st := map[rune]store.Status{'s': store.Success, 'u': store.Unstable, 'f': store.Failed, 't': store.Timeout, 'l': store.Lost}[c]
		at := newest.Add(-time.Duration(i) * time.Hour)
		out = append(out, &store.Build{ID: int64(1000 - i), Job: job, Number: len(shape) - i, Status: st, QueuedAt: at, FinishedAt: &at})
	}
	return out
}

func job(name, shape string) Job {
	return Job{Name: name, History: history(name, shape, now.Add(-time.Hour))}
}

func names[T any](rows []T, name func(T) string) string {
	var out []string
	for _, r := range rows {
		out = append(out, name(r))
	}
	return strings.Join(out, ",")
}

func failing(o *Overview) string { return names(o.Failing, func(f Failing) string { return f.Job }) }
func flaky(o *Overview) string   { return names(o.Flaky, func(f FlakyJob) string { return f.Job }) }

func TestMeasuredShapesLandInTheirSections(t *testing.T) {
	o := compute([]Job{
		job("units", "ssssfssssfsssssfsssf"),
		job("deploy", "ffffssffssf"),
		job("docs", "ssssssfffff"),
		job("short", "ssfs"),
	}, DefaultFlips, now)
	if flaky(o) != "units" || len(o.Flaky) != 1 {
		t.Fatalf("flaky: %v", flaky(o))
	}
	f := o.Flaky[0]
	if f.Flips != 7 || f.Failed != 4 || f.Finished != 20 || f.Percent() != 20 {
		t.Fatalf("%+v", f)
	}
	if failing(o) != "deploy" || !o.Failing[0].Flaky {
		t.Fatalf("failing: %v %+v", failing(o), o.Failing)
	}
}

func TestAJobNeedsFiveFinishedBuildsToBeFlaky(t *testing.T) {
	o := compute([]Job{job("four", "sfsf"), job("five", "sfsfs")}, 2, now)
	if flaky(o) != "five" {
		t.Fatalf("flaky: %q", flaky(o))
	}
}

func TestFlipsAreCountedOverTheNewestTwentyOnly(t *testing.T) {
	o := compute([]Job{job("old", strings.Repeat("s", 20)+"fsfsfsfsfs")}, 1, now)
	if flaky(o) != "" {
		t.Fatalf("flips from outside the window counted: %v", o.Flaky)
	}
	o = compute([]Job{job("win", "sfsfsfsfsfsfsfsfsfsf"+"ssss")}, 4, now)
	if len(o.Flaky) != 1 || o.Flaky[0].Finished != Window || o.Flaky[0].Flips != 19 || o.Flaky[0].Failed != 10 {
		t.Fatalf("%+v", o.Flaky)
	}
}

func TestUnstableIsGreenAndLostAndTimeoutAreRedForFlips(t *testing.T) {
	o := compute([]Job{job("mix", "ustlsus")}, 4, now)
	if flaky(o) != "" {
		t.Fatalf("2 flips: %v", o.Flaky)
	}
	o = compute([]Job{job("mix", "uftlsuf"), job("flat", "uusuusu")}, 2, now)
	if flaky(o) != "mix" || o.Flaky[0].Flips != 3 || o.Flaky[0].Failed != 4 {
		t.Fatalf("%+v", o.Flaky)
	}
}

func TestAJobWhoseNewestFinishedBuildIsRedIsFailingNotFlaky(t *testing.T) {
	o := compute([]Job{job("red", "fsfsfsfs"), job("lost", "lsfsfsfs"), job("timed", "tsfsfsfs")}, 4, now)
	if len(o.Flaky) != 0 || len(o.Failing) != 3 {
		t.Fatalf("flaky %v failing %v", flaky(o), failing(o))
	}
	for _, f := range o.Failing {
		if !f.Flaky {
			t.Errorf("%s has no flaky tag", f.Job)
		}
	}
}

func TestZeroFlipsSwitchesFlakyOffAndTheTagWithIt(t *testing.T) {
	o := compute([]Job{job("a", "ssssfssssfsssssfsssf"), job("b", "fsfsfsfsfs")}, 0, now)
	if len(o.Flaky) != 0 {
		t.Fatalf("%v", flaky(o))
	}
	if len(o.Failing) != 1 || o.Failing[0].Flaky {
		t.Fatalf("%+v", o.Failing)
	}
}

func TestRedSinceIsTheFirstRedBuildOfTheCurrentStreak(t *testing.T) {
	j := job("j", "fffsff")
	o := compute([]Job{j}, DefaultFlips, now)
	third := j.History[2]
	if len(o.Failing) != 1 || !o.Failing[0].Since.Equal(*third.FinishedAt) {
		t.Fatalf("%+v want %v", o.Failing, third.FinishedAt)
	}
	if o.Failing[0].Build != j.History[0] {
		t.Fatalf("the red build is the newest")
	}
}

func TestFailingIsSortedByRedSinceNewestFirst(t *testing.T) {
	a := Job{Name: "a", History: history("a", "fs", now.Add(-5*time.Hour))}
	b := Job{Name: "b", History: history("b", "fs", now.Add(-2*time.Hour))}
	c := Job{Name: "c", History: history("c", "fs", now.Add(-9*time.Hour))}
	if got := failing(compute([]Job{a, b, c}, 4, now)); got != "b,a,c" {
		t.Fatal(got)
	}
}

func TestFlakyIsSortedByFlipsThenFailedShareThenName(t *testing.T) {
	o := compute([]Job{
		job("tie", "sfsfsfsfs"),
		job("few", "sfsfsfsfs"),
		job("lessred", "ssfsfsfsfs"),
		job("most", "sfsfsfsfsfsfsfs"),
	}, 4, now)
	if got := flaky(o); got != "most,few,tie,lessred" {
		t.Fatal(got)
	}
}

func TestDisabledJobsAreInEverySectionButTheGroupStrip(t *testing.T) {
	j := job("off", "fsfsfsfsfs")
	j.Disabled, j.Group = true, "ci"
	o := compute([]Job{j}, 4, now)
	if len(o.Failing)+len(o.Flaky) != 0 {
		t.Fatalf("%+v", o)
	}
	if len(o.Groups) != 1 || o.Groups[0] != (Group{Name: "ci", Disabled: 1}) {
		t.Fatalf("%+v", o.Groups)
	}
}

func TestGroupsCountGreenRedNeverBuiltRunningAndDisabledNamedFirstThenNoGroup(t *testing.T) {
	run := job("run", "ssss")
	run.Running = true
	first := Job{Name: "first", Running: true}
	off := Job{Name: "off", Disabled: true}
	jobs := []Job{job("g", "s"), job("r", "f"), run, first, off, {Name: "none"}}
	for i := range jobs[:6] {
		jobs[i].Group = "b"
	}
	jobs = append(jobs, Job{Name: "loose", History: history("loose", "s", now)}, Job{Name: "a1", Group: "a", History: history("a1", "u", now)})
	o := compute(jobs, 4, now)
	want := []Group{
		{Name: "a", Green: 1},
		{Name: "b", Green: 2, Red: 1, NeverBuilt: 1, Running: 2, Disabled: 1},
		{Name: "", Green: 1},
	}
	if fmt.Sprint(o.Groups) != fmt.Sprint(want) {
		t.Fatalf("%+v\nwant %+v", o.Groups, want)
	}
}

func TestAGoneJobIsInNoCountOfAGroupAndADisabledChildIsCountedAsOne(t *testing.T) {
	gone := Job{Name: "p.pr-1", Group: "b", Disabled: true, Gone: true, Child: true}
	goneNeverBuilt := Job{Name: "p.pr-2", Group: "b", Gone: true, Child: true}
	off := Job{Name: "p.pr-3", Group: "b", Disabled: true, Child: true}
	never := Job{Name: "p.pr-4", Group: "b", Child: true}
	o := compute([]Job{gone, goneNeverBuilt, off, never, {Name: "top", Group: "b", Disabled: true}}, 4, now)
	want := Group{Name: "b", NeverBuilt: 1, NeverBuiltChildren: 1, Disabled: 2, DisabledChildren: 1}
	if len(o.Groups) != 1 || o.Groups[0] != want {
		t.Fatalf("%+v\nwant %+v", o.Groups, want)
	}
}

func TestTheRatesCarryTheNeverBuiltAndDisabledCountsOnlyWhenNotZeroEvenWithoutBuilds(t *testing.T) {
	jobs := []Job{
		job("a1", "ss"), {Name: "a2", Group: "a", Disabled: true},
		{Name: "idle", Group: "b", Child: true}, {Name: "gone", Group: "c", Disabled: true, Gone: true},
		{Name: "ok", Group: "d", History: history("ok", "s", now)},
	}
	jobs[0].Group = "a"
	o := compute(jobs, 4, now)
	var names []string
	for _, g := range o.Charts.Groups {
		names = append(names, g.Name)
	}
	if fmt.Sprint(names) != "[a b d]" {
		t.Fatalf("groups %v: a gone job alone makes no row", names)
	}
	a, b, d := o.Charts.Groups[0], o.Charts.Groups[1], o.Charts.Groups[2]
	if a.Disabled != 1 || a.NeverBuilt != 0 || a.Now.Total() == 0 {
		t.Fatalf("%+v", a)
	}
	if b.NeverBuilt != 1 || b.NeverBuiltChildren != 1 || b.Now.Total()+b.Before.Total() != 0 {
		t.Fatalf("%+v", b)
	}
	if d.NeverBuilt != 0 || d.Disabled != 0 {
		t.Fatalf("%+v", d)
	}
}

func TestADotRunIsTheFiveNewestBuilds(t *testing.T) {
	o := compute([]Job{job("a", "ffffffffff")}, 4, now)
	if len(o.Failing[0].Dots) != Dots {
		t.Fatalf("%d dots", len(o.Failing[0].Dots))
	}
}

func compute(jobs []Job, flips int, at time.Time) *Overview {
	for i := range jobs {
		if jobs[i].Builds == nil {
			jobs[i].Builds = jobs[i].History
		}
	}
	return Compute(jobs, Config{Flips: flips}, at)
}

func TestAJobWhoseNewestBuildsAreAllGreenIsNotFlaky(t *testing.T) {
	flappy := "sssss" + "fsfsfsfsfsfsfsf"
	for name, tc := range map[string]struct {
		shape string
		quiet int
		want  string
	}{
		"newest five green leaves":        {flappy, 5, ""},
		"one red among the newest five":   {"ssfss" + "fsfsfsfsfsfsfsf", 5, "j"},
		"quiet 0 keeps the old result":    {flappy, 0, "j"},
		"unstable counts as green":        {"usuus" + "fsfsfsfsfsfsfsf", 5, ""},
		"a larger quiet needs more green": {flappy, 6, "j"},
		"fewer than five builds":          {"sfsf", 5, ""},
		"exactly five green builds":       {"sssss", 5, ""},
	} {
		t.Run(name, func(t *testing.T) {
			jobs := []Job{job("j", tc.shape)}
			jobs[0].Builds = jobs[0].History
			o := Compute(jobs, Config{Flips: 2, Quiet: tc.quiet}, now)
			if got := flaky(o); got != tc.want {
				t.Fatalf("flaky %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOnlyTrunkJobsAreListedAndSideJobsAreCountedHiddenInTheSameSection(t *testing.T) {
	side := func(name, shape string) Job {
		j := job(name, shape)
		j.Child, j.Side = true, true
		return j
	}
	trunk := func(name, shape string) Job {
		j := job(name, shape)
		j.Child = true
		return j
	}
	flakyShape := "ssssfssssfsssssfsssf"
	for _, c := range []struct {
		name                                       string
		jobs                                       []Job
		failing, unstable, flaky                   string
		hiddenFailing, hiddenUnstable, hiddenFlaky int
	}{
		{name: "a red pull request, a red side branch and a red default branch",
			jobs:    []Job{side("pr", "f"), side("feature", "f"), trunk("main", "f")},
			failing: "main", hiddenFailing: 2},
		{name: "a red tag child or default-branch-unknown child or file job is trunk",
			jobs:    []Job{trunk("tag", "f"), trunk("unknown", "f"), job("file", "f")},
			failing: "file,tag,unknown"},
		{name: "unstable",
			jobs:     []Job{side("pr", "u"), side("feature", "u"), trunk("main", "u")},
			unstable: "main", hiddenUnstable: 2},
		{name: "flaky",
			jobs:  []Job{side("pr", flakyShape), trunk("main", flakyShape), job("file", flakyShape)},
			flaky: "file,main", hiddenFlaky: 1},
		{name: "a green quiet side job counts nowhere",
			jobs: []Job{side("pr", "ssss")}},
	} {
		t.Run(c.name, func(t *testing.T) {
			o := compute(c.jobs, DefaultFlips, now)
			sorted := func(s string) string {
				parts := strings.Split(s, ",")
				if s == "" {
					return ""
				}
				slices.Sort(parts)
				return strings.Join(parts, ",")
			}
			unstable := names(o.Unstable, func(f Failing) string { return f.Job })
			if sorted(failing(o)) != c.failing || sorted(unstable) != c.unstable || sorted(flaky(o)) != c.flaky {
				t.Fatalf("failing %q unstable %q flaky %q", failing(o), unstable, flaky(o))
			}
			want := Hidden{c.hiddenFailing, c.hiddenUnstable, c.hiddenFlaky}
			if o.Hidden != want {
				t.Fatalf("hidden %+v, want %+v", o.Hidden, want)
			}
		})
	}
}

func TestTheFlakyTagOfAnUnstableRowFollowsTheQuietRule(t *testing.T) {
	shape := "uuuuu" + "fsfsfsfsfsfsfsf"
	for quiet, want := range map[int]bool{0: true, 5: false} {
		jobs := []Job{job("j", shape)}
		jobs[0].Builds = jobs[0].History
		o := Compute(jobs, Config{Flips: 2, Quiet: quiet}, now)
		if len(o.Unstable) != 1 || o.Unstable[0].Flaky != want {
			t.Fatalf("quiet %d: %+v", quiet, o.Unstable)
		}
	}
}

func TestASideJobStaysInItsGroupCounts(t *testing.T) {
	j := job("pr", "f")
	j.Child, j.Side, j.Group = true, true, "web"
	o := compute([]Job{j}, DefaultFlips, now)
	if len(o.Groups) != 1 || o.Groups[0].Red != 1 {
		t.Fatalf("%+v", o.Groups)
	}
}
