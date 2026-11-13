package overview

import (
	"cmp"
	"slices"
	"time"

	"github.com/ndthuan/kipp/internal/store"
)

// Range is how far back the graphs and the list of failures look, and how
// wide a column of a graph is.
type Range struct {
	// Name is how a request names it, Label how a page does.
	Name, Label string
	Length      time.Duration
	Bucket      time.Duration
}

// The two ranges. Six hours make a column of the week, so that its 29
// columns read as well as the day's 25.
var (
	Day  = Range{Name: "24h", Label: "24 hours", Length: 24 * time.Hour, Bucket: time.Hour}
	Week = Range{Name: "7d", Label: "7 days", Length: 7 * 24 * time.Hour, Bucket: 6 * time.Hour}
)

// ParseRange is the range a request names; the empty name is Day, and
// anything but "24h" and "7d" is not one.
func ParseRange(name string) (Range, bool) {
	switch name {
	case "", Day.Name:
		return Day, true
	case Week.Name:
		return Week, true
	}
	return Day, false
}

// Config is what Compute needs besides the jobs.
type Config struct {
	// Flips is how many flips make a job flaky, 0 for no Flaky section.
	Flips int
	// Range is how far back the graphs look; the zero Range is Day.
	Range Range
	// Zone is where the columns' edges fall, so that a day's columns start
	// on the hour of the page's clock; nil is UTC.
	Zone *time.Location
}

func (c Config) rng() Range { return cmp.Or(c.Range, Day) }

// Bounds is where the graphs of a Range begin and end at now: whole
// columns of rg.Bucket on the clock of zone, from the one that holds
// now-rg.Length to the one that holds now. The builds that ended from
// start on are what Job.Builds holds, and those that ended in
// [now-2*rg.Length, now-rg.Length) are what Job.Before counts.
func Bounds(rg Range, zone *time.Location, now time.Time) (start, end time.Time) {
	if zone == nil {
		zone = time.UTC
	}
	rg = cmp.Or(rg, Day)
	t := now.In(zone)
	hours := int(rg.Bucket / time.Hour)
	end = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()-t.Hour()%hours, 0, 0, 0, zone).Add(rg.Bucket)
	return end.Add(-rg.Length - rg.Bucket), end
}

// Charts is the series of the three graphs.
type Charts struct {
	Range Range
	// The columns cover [Start, End).
	Start, End time.Time
	Buckets    []Bucket
	// Groups is the pass rate of every group that finished a build in the
	// last Range.Length or the one before it, or has a job never built or
	// disabled, the named ones by name and the jobs with no group (Name
	// empty) last.
	Groups []GroupRate
}

// Bucket is one column: the builds that ended in it by how they ended, the
// most that ran at once, and how long the builds that started in it waited.
type Bucket struct {
	Start                                time.Time
	Success, Unstable, Failed, Cancelled int
	// MaxRunning is the most builds running at one moment of the column.
	MaxRunning int
	// Started is how many builds started in the column, and WaitMedian and
	// WaitMax how long they waited in the queue; 0 when none did.
	Started             int
	WaitMedian, WaitMax time.Duration
}

// Finished is the builds that ended in the column.
func (b Bucket) Finished() int { return b.Success + b.Unstable + b.Failed + b.Cancelled }

// GroupRate is how often the builds of a group ended green in the last
// Range.Length (Now) and in the one before it (Before). Cancelled builds
// are in neither.
//
// NeverBuilt and Disabled are the group's counts of Group, and the
// ...Children how many of them are branches and pull requests: a group that
// built nothing in either range is still here when it has one of them.
type GroupRate struct {
	Name                                 string
	Now, Before                          store.Tally
	NeverBuilt, Disabled                 int
	NeverBuiltChildren, DisabledChildren int
}

// Delta is the change in whole percent from Before to Now, with false when
// either has no build to compare.
func (g GroupRate) Delta() (int, bool) {
	if g.Now.Total() == 0 || g.Before.Total() == 0 {
		return 0, false
	}
	return g.Now.Percent() - g.Before.Percent(), true
}

func chartsOf(jobs []Job, cfg Config, now time.Time) *Charts {
	rg := cfg.rng()
	start, end := Bounds(rg, cfg.Zone, now)
	c := &Charts{Range: rg, Start: start, End: end}
	for t := start; t.Before(end); t = t.Add(rg.Bucket) {
		c.Buckets = append(c.Buckets, Bucket{Start: t})
	}
	var spans []span
	waits := make([][]time.Duration, len(c.Buckets))
	rates := map[string]*GroupRate{}
	for _, j := range jobs {
		r := rates[j.Group]
		if r == nil {
			r = &GroupRate{Name: j.Group}
			rates[j.Group] = r
		}
		r.Before.Green += j.Before.Green
		r.Before.Red += j.Before.Red
		spans = append(spans, c.addJob(j, r, waits, now)...)
	}
	c.setWaits(waits)
	c.setMaxRunning(spans)
	for _, r := range rates {
		if r.Now.Total()+r.Before.Total() > 0 {
			c.Groups = append(c.Groups, *r)
		}
	}
	c.sortGroups()
	return c
}

func (c *Charts) sortGroups() {
	slices.SortFunc(c.Groups, func(a, b GroupRate) int {
		return cmp.Or(cmp.Compare(btoi(a.Name == ""), btoi(b.Name == "")), cmp.Compare(a.Name, b.Name))
	})
}

// addCounts puts the never built and disabled counts of gs onto the rows of
// the groups, and adds a row for a group that has one and no build.
func (c *Charts) addCounts(gs []Group) {
	at := map[string]int{}
	for i, r := range c.Groups {
		at[r.Name] = i
	}
	for _, g := range gs {
		if g.NeverBuilt+g.Disabled == 0 {
			continue
		}
		i, ok := at[g.Name]
		if !ok {
			i = len(c.Groups)
			c.Groups = append(c.Groups, GroupRate{Name: g.Name})
		}
		r := &c.Groups[i]
		r.NeverBuilt, r.Disabled = g.NeverBuilt, g.Disabled
		r.NeverBuiltChildren, r.DisabledChildren = g.NeverBuiltChildren, g.DisabledChildren
	}
	c.sortGroups()
}

// addJob counts the builds of j into the columns and into its group's rate,
// and returns the spans it ran.
func (c *Charts) addJob(j Job, r *GroupRate, waits [][]time.Duration, now time.Time) []span {
	var spans []span
	for _, b := range j.Builds {
		c.count(b, waits)
		if s, ok := runSpan(b, now); ok {
			spans = append(spans, s)
		}
		if b.FinishedAt != nil && !b.FinishedAt.Before(now.Add(-c.Range.Length)) {
			r.Now.Green += btoi(b.Status.Green())
			r.Now.Red += btoi(b.Status.Red())
		}
	}
	return spans
}

// index is the column that holds t, -1 when none does.
func (c *Charts) index(t time.Time) int {
	if t.Before(c.Start) || !t.Before(c.End) {
		return -1
	}
	return int(t.Sub(c.Start) / c.Range.Bucket)
}

// count puts a build into the column it ended in by its outcome, and the
// wait of one that started in a column into that column's.
func (c *Charts) count(b *store.Build, waits [][]time.Duration) {
	if b.FinishedAt != nil {
		if i := c.index(*b.FinishedAt); i >= 0 {
			bk := &c.Buckets[i]
			switch {
			case b.Status == store.Success:
				bk.Success++
			case b.Status == store.Unstable:
				bk.Unstable++
			case b.Status.Red():
				bk.Failed++
			case b.Status == store.Cancelled:
				bk.Cancelled++
			}
		}
	}
	if b.StartedAt != nil {
		if i := c.index(*b.StartedAt); i >= 0 {
			waits[i] = append(waits[i], max(0, b.StartedAt.Sub(b.QueuedAt)))
		}
	}
}

func (c *Charts) setWaits(waits [][]time.Duration) {
	for i, w := range waits {
		if len(w) == 0 {
			continue
		}
		slices.Sort(w)
		n := len(w)
		med := w[n/2]
		if n%2 == 0 {
			med = (w[n/2-1] + w[n/2]) / 2
		}
		c.Buckets[i].Started, c.Buckets[i].WaitMedian, c.Buckets[i].WaitMax = n, med, w[n-1]
	}
}

// span is the time a build ran: from its start to its end, or to now.
type span struct{ from, to time.Time }

func runSpan(b *store.Build, now time.Time) (span, bool) {
	if b.StartedAt == nil {
		return span{}, false
	}
	to := now
	if b.FinishedAt != nil {
		to = *b.FinishedAt
	}
	return span{*b.StartedAt, to}, true
}

// setMaxRunning finds, for every column, the most spans that overlap at one
// moment within it, by walking the starts and ends of the spans in order. A
// span that ends where another starts does not overlap it.
func (c *Charts) setMaxRunning(spans []span) {
	type edge struct {
		at    time.Time
		delta int
	}
	edges := make([]edge, 0, 2*len(spans))
	for _, s := range spans {
		edges = append(edges, edge{s.from, 1}, edge{s.to, -1})
	}
	slices.SortFunc(edges, func(a, b edge) int { return cmp.Or(a.at.Compare(b.at), cmp.Compare(a.delta, b.delta)) })
	running, k := 0, 0
	for i := range c.Buckets {
		bk := &c.Buckets[i]
		end := bk.Start.Add(c.Range.Bucket)
		for k < len(edges) && edges[k].at.Before(bk.Start) {
			running += edges[k].delta
			k++
		}
		bk.MaxRunning = running
		for k < len(edges) && edges[k].at.Before(end) {
			running += edges[k].delta
			bk.MaxRunning = max(bk.MaxRunning, running)
			k++
		}
	}
}
