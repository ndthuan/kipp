package overview

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/store"
)

var now = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

// history reads a string newest first, as the builds API gives them: s is
// success, u unstable, f failed, t timeout, l lost. A build ended `gap`
// before the one to its right.
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
	o := Compute([]Job{
		job("units", "ssssfssssfsssssfsssf"),
		job("pensor", "ffffssffssf"),
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
	// docs-mirror, newest first, is green with one flip: in neither section.
	if failing(o) != "pensor" || !o.Failing[0].Flaky {
		t.Fatalf("failing: %v %+v", failing(o), o.Failing)
	}
}

func TestAJobNeedsFiveFinishedBuildsToBeFlaky(t *testing.T) {
	o := Compute([]Job{job("four", "sfsf"), job("five", "sfsfs")}, 2, now)
	if flaky(o) != "five" {
		t.Fatalf("flaky: %q", flaky(o))
	}
}

func TestFlipsAreCountedOverTheNewestTwentyOnly(t *testing.T) {
	// Twenty steady builds, then a flapping history that is out of the window.
	o := Compute([]Job{job("old", strings.Repeat("s", 20)+"fsfsfsfsfs")}, 1, now)
	if flaky(o) != "" {
		t.Fatalf("flips from outside the window counted: %v", o.Flaky)
	}
	o = Compute([]Job{job("win", "sfsfsfsfsfsfsfsfsfsf"+"ssss")}, 4, now)
	if len(o.Flaky) != 1 || o.Flaky[0].Finished != Window || o.Flaky[0].Flips != 19 || o.Flaky[0].Failed != 10 {
		t.Fatalf("%+v", o.Flaky)
	}
}

func TestUnstableIsGreenAndLostAndTimeoutAreRedForFlips(t *testing.T) {
	o := Compute([]Job{job("mix", "ustlsus")}, 4, now)
	// u s t l s u s: green green red red green green green = 2 flips.
	if flaky(o) != "" {
		t.Fatalf("2 flips: %v", o.Flaky)
	}
	o = Compute([]Job{job("mix", "uftlsuf"), job("flat", "uusuusu")}, 2, now)
	// u f t l s u f: g r r r g g r = 3 flips; the newest is green.
	if flaky(o) != "mix" || o.Flaky[0].Flips != 3 || o.Flaky[0].Failed != 4 {
		t.Fatalf("%+v", o.Flaky)
	}
}

func TestAJobWhoseNewestFinishedBuildIsRedIsFailingNotFlaky(t *testing.T) {
	o := Compute([]Job{job("red", "fsfsfsfs"), job("lost", "lsfsfsfs"), job("timed", "tsfsfsfs")}, 4, now)
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
	o := Compute([]Job{job("a", "ssssfssssfsssssfsssf"), job("b", "fsfsfsfsfs")}, 0, now)
	if len(o.Flaky) != 0 {
		t.Fatalf("%v", flaky(o))
	}
	if len(o.Failing) != 1 || o.Failing[0].Flaky {
		t.Fatalf("%+v", o.Failing)
	}
}

func TestRedSinceIsTheFirstRedBuildOfTheCurrentStreak(t *testing.T) {
	// f f f s f f: the streak is the three newest; the red before the green is another one.
	j := job("j", "fffsff")
	o := Compute([]Job{j}, DefaultFlips, now)
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
	if got := failing(Compute([]Job{a, b, c}, 4, now)); got != "b,a,c" {
		t.Fatal(got)
	}
}

func TestFlakyIsSortedByFlipsThenFailedShareThenName(t *testing.T) {
	o := Compute([]Job{
		job("tie", "sfsfsfsfs"),        // 8 flips, 4 of 9
		job("few", "sfsfsfsfs"),        // the same: by name
		job("lessred", "ssfsfsfsfs"),   // 8 flips, 4 of 10
		job("most", "sfsfsfsfsfsfsfs"), // 14 flips
	}, 4, now)
	if got := flaky(o); got != "most,few,tie,lessred" {
		t.Fatal(got)
	}
}

func TestRecentFailuresAreTheLast24HoursNewestFirstAtMostTen(t *testing.T) {
	var jobs []Job
	for i := range 12 {
		name := fmt.Sprintf("j%02d", i)
		// Ended i hours ago, red, then green before it.
		jobs = append(jobs, Job{Name: name, History: history(name, "fs", now.Add(-time.Duration(i)*time.Hour-time.Minute))})
	}
	jobs = append(jobs, Job{Name: "old", History: history("old", "f", now.Add(-25*time.Hour))})
	o := Compute(jobs, 4, now)
	if len(o.RecentFailures) != RecentMax || o.MoreFailures != 2 {
		t.Fatalf("%d shown, %d more", len(o.RecentFailures), o.MoreFailures)
	}
	if o.RecentFailures[0].Build.Job != "j00" || o.RecentFailures[9].Build.Job != "j09" {
		t.Fatalf("order: %s ... %s", o.RecentFailures[0].Build.Job, o.RecentFailures[9].Build.Job)
	}
	for _, f := range o.RecentFailures {
		if f.Build.Job == "old" || f.GreenAgain {
			t.Errorf("%+v", f)
		}
	}
}

func TestARecentFailureSaysWhetherItsJobIsGreenAgain(t *testing.T) {
	j := Job{Name: "j", History: history("j", "sfs", now.Add(-time.Hour))}
	o := Compute([]Job{j}, 4, now)
	if len(o.RecentFailures) != 1 || !o.RecentFailures[0].GreenAgain || o.MoreFailures != 0 {
		t.Fatalf("%+v", o.RecentFailures)
	}
}

func TestComingUpIsTheNextEightByTime(t *testing.T) {
	var jobs []Job
	for i := range 10 {
		at := now.Add(time.Duration(10-i) * time.Minute)
		jobs = append(jobs, Job{Name: fmt.Sprintf("j%d", i), Cron: "* * * * *", Next: &at})
	}
	jobs = append(jobs, Job{Name: "never", Cron: ""})
	o := Compute(jobs, 4, now)
	if len(o.Upcoming) != UpcomingMax || o.Upcoming[0].Job != "j9" || o.Upcoming[7].Job != "j2" || o.Upcoming[0].Cron != "* * * * *" {
		t.Fatalf("%+v", o.Upcoming)
	}
}

func TestDisabledJobsAreInEverySectionButTheGroupStrip(t *testing.T) {
	at := now.Add(time.Minute)
	j := job("off", "fsfsfsfsfs")
	j.Disabled, j.Next, j.Group = true, &at, "ci"
	o := Compute([]Job{j}, 4, now)
	if len(o.Failing)+len(o.Flaky)+len(o.RecentFailures)+len(o.Upcoming) != 0 {
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
	o := Compute(jobs, 4, now)
	want := []Group{
		{Name: "a", Green: 1},
		{Name: "b", Green: 2, Red: 1, NeverBuilt: 1, Running: 2, Disabled: 1},
		{Name: "", Green: 1},
	}
	if fmt.Sprint(o.Groups) != fmt.Sprint(want) {
		t.Fatalf("%+v\nwant %+v", o.Groups, want)
	}
}

func TestADotRunIsTheFiveNewestBuilds(t *testing.T) {
	o := Compute([]Job{job("a", "ffffffffff")}, 4, now)
	if len(o.Failing[0].Dots) != Dots {
		t.Fatalf("%d dots", len(o.Failing[0].Dots))
	}
}
