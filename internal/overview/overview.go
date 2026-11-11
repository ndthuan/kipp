// Package overview works out what the front page and GET /api/v1/overview
// show: what is red, what flaps, how each group stands and the graphs. It is given the jobs a user may view, with their
// finished builds, and decides nothing about who may view what.
package overview

import (
	"cmp"
	"slices"
	"time"

	"github.com/ndthuan/kipp/internal/store"
)

// The numbers of the sections, written once.
const (
	// Window is how many of a job's newest finished builds the sections
	// look at, and MinFinished how many of them a job needs to be flaky.
	Window      = 20
	MinFinished = 5
	// DefaultFlips is how many times the colour must change inside the
	// window for a job to be flaky; 0 switches the section off.
	DefaultFlips = 4
	// Dots is the run of dots beside a job.
	Dots = 5
)

// Job is one leaf job the viewer may see: a job that builds itself, so
// neither a multibranch job, an owner job nor a repo's node.
type Job struct {
	Name, Group string
	// Disabled is a job turned off, in its file or through its parent, and
	// Gone a branch, pull request or repo that no longer exists: it is
	// Disabled as well, but no count of a group has it.
	Disabled, Gone bool
	// Child is a branch or pull request of a multibranch job.
	Child bool
	// Running: the job's newest build is queued or running.
	Running bool
	// History is the job's newest builds that ended green or red, newest
	// first (store.Store.RecentColoured).
	History []*store.Build
	// Builds is every build of the job that ended at or after the start of
	// the graphs (Bounds), and those that have not ended
	// (store.Store.FinishedSince). Before is how many ended green and red
	// in the Range.Length before the last one.
	Builds []*store.Build
	Before store.Tally
}

// Overview is every section but the queue.
type Overview struct {
	Failing []Failing
	// Unstable are the jobs whose newest finished build is unstable, in the
	// shape of Failing: Since is the end of the oldest build of the run of
	// unstable ones it ends.
	Unstable []Failing
	Flaky    []FlakyJob
	Groups   []Group
	Charts   *Charts
}

// Failing is a job whose newest finished build is red. Build is that build,
// Since the end of the first red build of the streak it ends (the oldest
// the window shows, if all of it is red), Flaky that the job would be
// flaky if it were green, and Dots its newest builds.
type Failing struct {
	Job   string
	Build *store.Build
	Since time.Time
	Flaky bool
	Dots  []*store.Build
}

// FlakyJob is a job that is green now and has flipped colour often: Failed
// of Finished builds in its window are red, in Flips changes of colour.
type FlakyJob struct {
	Job              string
	Failed, Finished int
	Flips            int
	Dots             []*store.Build
}

// Percent is the share of the window that is red, in whole percent.
func (f FlakyJob) Percent() int { return (f.Failed*100 + f.Finished/2) / f.Finished }

// Group is how the jobs of a group stand, Name "" being the jobs with no
// group. A running job is also counted by its colour, so the counts of one
// group do not add up to its jobs; a disabled job is only disabled. A gone
// job is in no count. NeverBuiltChildren and DisabledChildren are how many
// of NeverBuilt and Disabled are branches and pull requests.
type Group struct {
	Name                                      string
	Green, Red, NeverBuilt, Running, Disabled int
	NeverBuiltChildren, DisabledChildren      int
}

// Compute is the overview of jobs at now.
func Compute(jobs []Job, cfg Config, now time.Time) *Overview {
	o := &Overview{Groups: groups(jobs), Charts: chartsOf(jobs, cfg, now)}
	o.Charts.addCounts(o.Groups)
	flips := cfg.Flips
	for _, j := range jobs {
		if j.Disabled || j.Gone {
			continue
		}
		h := j.History
		if len(h) > Window {
			h = h[:Window]
		}
		o.addStanding(j, h, flips)
	}
	o.sort()
	return o
}

// addStanding puts a job into Failing, Unstable or Flaky, by its newest
// build; a job that is unstable now and flaky is in both of the last two.
func (o *Overview) addStanding(j Job, h []*store.Build, flips int) {
	if len(h) == 0 {
		return
	}
	n := flipsOf(h)
	flaky := flips > 0 && len(h) >= MinFinished && n >= flips
	dots := h[:min(len(h), Dots)]
	switch {
	case h[0].Status.Red():
		o.Failing = append(o.Failing, Failing{Job: j.Name, Build: h[0], Since: streakStart(h, store.Status.Red), Flaky: flaky, Dots: dots})
		return
	case h[0].Status == store.Unstable:
		o.Unstable = append(o.Unstable, Failing{Job: j.Name, Build: h[0], Since: streakStart(h, isUnstable), Flaky: flaky, Dots: dots})
	}
	if flaky {
		o.Flaky = append(o.Flaky, FlakyJob{Job: j.Name, Failed: redCount(h), Finished: len(h), Flips: n, Dots: dots})
	}
}

func (o *Overview) sort() {
	byRecency := func(a, b Failing) int { return cmp.Or(b.Since.Compare(a.Since), cmp.Compare(a.Job, b.Job)) }
	slices.SortFunc(o.Failing, byRecency)
	slices.SortFunc(o.Unstable, byRecency)
	slices.SortFunc(o.Flaky, func(a, b FlakyJob) int {
		// Failed/Finished compared without division.
		return cmp.Or(cmp.Compare(b.Flips, a.Flips), cmp.Compare(b.Failed*a.Finished, a.Failed*b.Finished), cmp.Compare(a.Job, b.Job))
	})
}

// endOf is when a build ended.
func endOf(b *store.Build) time.Time {
	if b.FinishedAt != nil {
		return *b.FinishedAt
	}
	return b.QueuedAt
}

func isUnstable(s store.Status) bool { return s == store.Unstable }

// streakStart is the end of the oldest build of the run of builds whose
// status is in that h, newest first, starts with.
func streakStart(h []*store.Build, in func(store.Status) bool) time.Time {
	i := 0
	for i+1 < len(h) && in(h[i+1].Status) {
		i++
	}
	return endOf(h[i])
}

func flipsOf(h []*store.Build) int {
	n := 0
	for i := 1; i < len(h); i++ {
		if h[i].Status.Red() != h[i-1].Status.Red() {
			n++
		}
	}
	return n
}

func redCount(h []*store.Build) int {
	n := 0
	for _, b := range h {
		if b.Status.Red() {
			n++
		}
	}
	return n
}

// groups counts the jobs of every group, the named ones by name and the
// jobs with no group last.
func groups(jobs []Job) []Group {
	byName := map[string]*Group{}
	var out []*Group
	for _, j := range jobs {
		g := byName[j.Group]
		if g == nil {
			g = &Group{Name: j.Group}
			byName[j.Group] = g
			out = append(out, g)
		}
		g.count(j)
	}
	slices.SortFunc(out, func(a, b *Group) int {
		return cmp.Or(cmp.Compare(btoi(a.Name == ""), btoi(b.Name == "")), cmp.Compare(a.Name, b.Name))
	})
	flat := make([]Group, len(out))
	for i, g := range out {
		flat[i] = *g
	}
	return flat
}

func (g *Group) count(j Job) {
	switch {
	case j.Gone:
		return
	case j.Disabled:
		g.Disabled++
		g.DisabledChildren += btoi(j.Child)
		return
	case j.Running:
		g.Running++
	}
	switch {
	case len(j.History) > 0 && j.History[0].Status.Red():
		g.Red++
	case len(j.History) > 0:
		g.Green++
	case !j.Running:
		g.NeverBuilt++
		g.NeverBuiltChildren += btoi(j.Child)
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
