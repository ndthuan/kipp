package overview

import (
	"slices"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/store"
)

// build is a build that was queued at q, started at s and ended at e, each
// as an offset from now; a zero s is never started and a zero e not ended.
func build(id int64, job string, st store.Status, q, s, e time.Duration) *store.Build {
	b := &store.Build{ID: id, Job: job, Number: int(id), Status: st, QueuedAt: now.Add(q)}
	if s != 0 {
		b.StartedAt = new(now.Add(s))
	}
	if e != 0 {
		b.FinishedAt = new(now.Add(e))
	}
	return b
}

func bucketAt(t *testing.T, c *Charts, at time.Time) Bucket {
	t.Helper()
	i := c.index(at)
	if i < 0 {
		t.Fatalf("%v is outside the columns %v to %v", at, c.Start, c.End)
	}
	return c.Buckets[i]
}

func TestTheDayIsTwentyFiveHourColumnsEndingWithTheOneThatHoldsNow(t *testing.T) {
	c := Compute(nil, Config{}, now).Charts
	if len(c.Buckets) != 25 || !c.End.Equal(now.Add(time.Hour)) || !c.Start.Equal(now.Add(-24*time.Hour)) {
		t.Fatalf("%d columns, %v to %v", len(c.Buckets), c.Start, c.End)
	}
	// 12:30 in UTC+7 is 05:30 UTC: the column is the one of 05:00 UTC.
	zone := time.FixedZone("x", 7*3600)
	_, end := Bounds(Day, zone, now.Add(30*time.Minute))
	if !end.Equal(now.Add(time.Hour)) {
		t.Fatal(end)
	}
}

func TestTheWeekIsTwentyNineSixHourColumnsOnTheClockOfTheZone(t *testing.T) {
	zone := time.FixedZone("x", 7*3600)
	c := Compute(nil, Config{Range: Week, Zone: zone}, now).Charts
	// 12:00 UTC is 19:00 in the zone, so the column holding now starts at
	// 18:00 there, which is 11:00 UTC.
	if len(c.Buckets) != 29 || !c.End.Equal(now.Add(5*time.Hour)) || c.Buckets[28].Start.In(zone).Hour() != 18 {
		t.Fatalf("%d columns, end %v", len(c.Buckets), c.End)
	}
	if c.Start.After(now.Add(-7 * 24 * time.Hour)) {
		t.Fatalf("the week's first column starts at %v, after the week", c.Start)
	}
}

func TestParseRangeTakesTheTwoNamesAndNothingElse(t *testing.T) {
	for in, want := range map[string]Range{"": Day, "24h": Day, "7d": Week} {
		if got, ok := ParseRange(in); !ok || got != want {
			t.Errorf("%q: %v %v", in, got, ok)
		}
	}
	for _, in := range []string{"1h", "30d", "week", "24H"} {
		if _, ok := ParseRange(in); ok {
			t.Errorf("%q is a range", in)
		}
	}
}

func TestBuildsAreCountedByOutcomeInTheColumnTheyEndedInAcrossAnEdge(t *testing.T) {
	// now is 12:00: a build that ended a second before it is in the column of
	// 11:00, one that ended at 12:00 in the column of 12:00.
	j := Job{Name: "a", Builds: []*store.Build{
		build(1, "a", store.Success, -time.Hour, 0, -time.Second),
		build(2, "a", store.Success, -time.Hour, 0, 0+time.Millisecond),
		build(3, "a", store.Unstable, -time.Hour, 0, -30*time.Minute),
		build(4, "a", store.Failed, -time.Hour, 0, -50*time.Minute),
		build(5, "a", store.Timeout, -time.Hour, 0, -40*time.Minute),
		build(6, "a", store.Lost, -time.Hour, 0, -20*time.Minute),
		build(7, "a", store.Cancelled, -time.Hour, 0, -10*time.Minute),
		build(8, "a", store.Success, -time.Hour, 0, -25*time.Hour),
	}}
	c := Compute([]Job{j}, Config{}, now).Charts
	before, at := bucketAt(t, c, now.Add(-time.Second)), bucketAt(t, c, now)
	if before.Success != 1 || before.Unstable != 1 || before.Failed != 3 || before.Cancelled != 1 || before.Finished() != 6 {
		t.Fatalf("11:00: %+v", before)
	}
	if at.Success != 1 || at.Finished() != 1 {
		t.Fatalf("12:00: %+v", at)
	}
	total := 0
	for _, b := range c.Buckets {
		total += b.Finished()
	}
	if total != 7 {
		t.Fatalf("%d counted: the build from 25 hours ago is before the first column", total)
	}
}

func TestMaxRunningIsTheMostBuildsRunningAtOnceInAColumn(t *testing.T) {
	h := time.Hour
	j := Job{Name: "a", Builds: []*store.Build{
		// Three overlap between 10:20 and 10:30; the first runs on into 11:00.
		build(1, "a", store.Success, -2*h, -2*h+10*time.Minute, -h+15*time.Minute),
		build(2, "a", store.Success, -2*h, -2*h+20*time.Minute, -2*h+40*time.Minute),
		build(3, "a", store.Success, -2*h, -2*h+25*time.Minute, -2*h+30*time.Minute),
		// Starts where build 2 ended: no overlap with it.
		build(4, "a", store.Success, -2*h, -2*h+40*time.Minute, -2*h+50*time.Minute),
		// Still running at now.
		build(5, "a", store.Running, -h, -50*time.Minute, 0),
		// Queued and never started.
		build(6, "a", store.Queued, -h, 0, 0),
	}}
	c := Compute([]Job{j}, Config{Builds: 4}, now).Charts
	for at, want := range map[time.Duration]int{-3 * h: 0, -2 * h: 3, -h: 2, 0: 1} {
		if got := bucketAt(t, c, now.Add(at)).MaxRunning; got != want {
			t.Errorf("%v: %d running, want %d", at, got, want)
		}
	}
	if c.BuildsLimit != 4 {
		t.Fatal(c.BuildsLimit)
	}
}

func TestAColumnTellsTheMedianAndTheLongestWaitOfTheBuildsThatStartedInIt(t *testing.T) {
	m := time.Minute
	start := -90 * m // 10:30
	j := Job{Name: "a", Builds: []*store.Build{
		build(1, "a", store.Success, start-2*m, start, start+m),
		build(2, "a", store.Success, start-10*m, start, start+m),
		build(3, "a", store.Success, start-m, start, start+m),
		build(4, "a", store.Success, start-4*m, start, start+m),
		// Waited 50 minutes and started in the next column.
		build(5, "a", store.Success, -2*time.Hour, -time.Hour, -50*m),
	}}
	c := Compute([]Job{j}, Config{}, now).Charts
	b := bucketAt(t, c, now.Add(start))
	// Waits 1, 2, 4 and 10 minutes: the median is the middle of 2 and 4.
	if b.Started != 4 || b.WaitMedian != 3*m || b.WaitMax != 10*m {
		t.Fatalf("%+v", b)
	}
	if next := bucketAt(t, c, now.Add(-time.Hour)); next.Started != 1 || next.WaitMedian != 60*m || next.WaitMax != 60*m {
		t.Fatalf("%+v", next)
	}
	if empty := bucketAt(t, c, now); empty.Started != 0 || empty.WaitMax != 0 {
		t.Fatalf("%+v", empty)
	}
}

func rate(c *Charts, name string) (GroupRate, bool) {
	i := slices.IndexFunc(c.Groups, func(g GroupRate) bool { return g.Name == name })
	if i < 0 {
		return GroupRate{}, false
	}
	return c.Groups[i], true
}

func TestPassRateOfAGroupIsThisWindowAgainstTheOneBefore(t *testing.T) {
	h := time.Hour
	ci := Job{Name: "ci", Group: "ci", Before: store.Tally{Green: 8, Red: 2}, Builds: []*store.Build{
		build(1, "ci", store.Success, -2*h, 0, -h),
		build(2, "ci", store.Unstable, -2*h, 0, -h),
		build(3, "ci", store.Failed, -2*h, 0, -h),
		build(4, "ci", store.Success, -2*h, 0, -h),
		build(5, "ci", store.Cancelled, -2*h, 0, -h),
	}}
	loose := Job{Name: "loose", Builds: []*store.Build{build(6, "loose", store.Success, -2*h, 0, -h)}}
	onlyBefore := Job{Name: "was", Group: "old", Before: store.Tally{Green: 3, Red: 1}}
	idle := Job{Name: "idle", Group: "idle"}
	c := Compute([]Job{loose, ci, onlyBefore, idle}, Config{}, now).Charts
	g, _ := rate(c, "ci")
	d, ok := g.Delta()
	if g.Now != (store.Tally{Green: 3, Red: 1}) || g.Now.Percent() != 75 || g.Before.Percent() != 80 || !ok || d != -5 {
		t.Fatalf("%+v %d %v", g, d, ok)
	}
	// 4 of 5 builds is 80%; a cancelled build is in neither count.
	if g.Now.Total() != 4 {
		t.Fatal(g.Now)
	}
	if g, _ := rate(c, ""); g.Now.Percent() != 100 || g.Before.Total() != 0 {
		t.Fatalf("no group: %+v", g)
	} else if _, ok := g.Delta(); ok {
		t.Fatal("a delta with nothing before it")
	}
	if g, ok := rate(c, "old"); !ok || g.Now.Total() != 0 {
		t.Fatalf("a group with builds only before is listed: %+v %v", g, ok)
	} else if _, ok := g.Delta(); ok {
		t.Fatal("a delta with nothing now")
	}
	if g, ok := rate(c, "idle"); !ok || g.NeverBuilt != 1 || g.Now.Total()+g.Before.Total() != 0 {
		t.Fatalf("a group with no build but a job never built is listed for that: %+v %v", g, ok)
	}
	var names []string
	for _, g := range c.Groups {
		names = append(names, g.Name)
	}
	if !slices.Equal(names, []string{"ci", "idle", "old", ""}) {
		t.Fatalf("named first by name, no group last: %q", names)
	}
}

func TestTheJobsWhoseNewestBuildIsUnstableAreUnstableSinceTheStartOfTheirRun(t *testing.T) {
	j := job("u", "uuusu")
	o := compute([]Job{j}, 4, now)
	if len(o.Unstable) != 1 || o.Unstable[0].Job != "u" || len(o.Failing) != 0 {
		t.Fatalf("%+v", o.Unstable)
	}
	// The run is the three newest builds, so it started when the oldest of them ended.
	if !o.Unstable[0].Since.Equal(*j.History[2].FinishedAt) || o.Unstable[0].Build != j.History[0] || len(o.Unstable[0].Dots) != Dots {
		t.Fatalf("%+v", o.Unstable[0])
	}
}

func TestAnUnstableRunEndsAtAnyOtherColourAndStaysGreenEverywhereElse(t *testing.T) {
	off := job("off", "uus")
	off.Disabled = true
	then := job("then-red", "fuu")
	o := compute([]Job{job("rec", "uusuu"), off, then, job("ok", "sss")}, 4, now)
	if got := names(o.Unstable, func(f Failing) string { return f.Job }); got != "rec" {
		t.Fatalf("unstable: %q: a disabled job, a green one and one that went red are not in it", got)
	}
	if failing(o) != "then-red" {
		t.Fatalf("a job unstable and then failed is in Failing only: %q", failing(o))
	}
	if len(o.Groups) != 1 || o.Groups[0].Green != 2 || o.Groups[0].Red != 1 {
		t.Fatalf("unstable counts as green: %+v", o.Groups)
	}
}

func TestAnUnstableJobThatFlipsIsInUnstableAndInFlaky(t *testing.T) {
	o := compute([]Job{job("both", "ufsfsfs")}, 4, now)
	if len(o.Unstable) != 1 || len(o.Flaky) != 1 || !o.Unstable[0].Flaky {
		t.Fatalf("%+v %+v", o.Unstable, o.Flaky)
	}
}

func TestTheWeeksFailuresCoverSevenDaysAndTheDaysOnlyOne(t *testing.T) {
	h := time.Hour
	j := Job{Name: "j", History: history("j", "sfsf", now.Add(-h))}
	// Builds ended 1, 2, 3 and 4 hours ago; replace them by ones 1 hour, 3 days and 6 days ago.
	j.History[1].FinishedAt = new(now.Add(-3 * 24 * h))
	j.History[3].FinishedAt = new(now.Add(-6 * 24 * h))
	j.Builds = j.History
	day := Compute([]Job{j}, Config{Flips: 4}, now)
	week := Compute([]Job{j}, Config{Flips: 4, Range: Week}, now)
	if len(day.RecentFailures) != 0 || len(week.RecentFailures) != 2 {
		t.Fatalf("%d and %d failures", len(day.RecentFailures), len(week.RecentFailures))
	}
	if week.RecentFailures[0].Build != j.History[1] || !week.RecentFailures[0].GreenAgain {
		t.Fatalf("%+v", week.RecentFailures[0])
	}
}
