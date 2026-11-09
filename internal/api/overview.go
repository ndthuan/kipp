package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/ndthuan/kipp/internal/overview"
	"github.com/ndthuan/kipp/internal/store"
)

// Overview is what is red, unstable, flaky, recently failed, coming up and
// how each group stands, with the graphs of the last rg, over the jobs u may
// view that build themselves.
func (s *Service) Overview(ctx context.Context, u *store.User, rg overview.Range) (*overview.Overview, error) {
	snap := s.deps.Dispatch.Snapshot()
	var leaves []string
	for _, n := range snap.Names {
		if snap.Jobs[n].IsLeaf() {
			leaves = append(leaves, n)
		}
	}
	names, err := s.deps.Auth.Visible(ctx, u, leaves)
	if err != nil {
		return nil, err
	}
	last, err := s.deps.Store.LastBuilds(ctx)
	if err != nil {
		return nil, err
	}
	history, err := s.deps.Store.RecentColoured(ctx, overview.Window)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	zone := s.deps.Dispatch.Zone(nil)
	start, _ := overview.Bounds(rg, zone, now)
	ended, err := s.deps.Store.FinishedSince(ctx, start)
	if err != nil {
		return nil, err
	}
	byJob := map[string][]*store.Build{}
	for _, b := range ended {
		byJob[b.Job] = append(byJob[b.Job], b)
	}
	before, err := s.deps.Store.TalliesBetween(ctx, now.Add(-2*rg.Length), now.Add(-rg.Length))
	if err != nil {
		return nil, err
	}
	jobs := make([]overview.Job, 0, len(names))
	for _, n := range names {
		j := snap.Jobs[n]
		running := last[n] != nil && !last[n].Status.Terminal()
		jobs = append(jobs, overview.Job{Name: n, Group: j.Group, Disabled: j.Disabled, Gone: j.Gone, Child: j.Parent != "", Cron: j.Triggers.Cron,
			Next: nextFire(j, now, s.deps.Dispatch.Zone(j)), Running: running, History: history[n],
			Builds: byJob[n], Before: before[n]})
	}
	cfg := overview.Config{Flips: s.deps.FlakyFlips, Range: rg, Builds: s.deps.Dispatch.Builds(), Zone: zone}
	return overview.Compute(jobs, cfg, now), nil
}

// OverviewRange is the range a request's ?window= names, Day for none; one
// that is neither 24h nor 7d is ErrBadInput.
func OverviewRange(r *http.Request) (overview.Range, error) {
	rg, ok := overview.ParseRange(r.URL.Query().Get("window"))
	if !ok {
		return rg, fmt.Errorf("%w: window is 24h or 7d", ErrBadInput)
	}
	return rg, nil
}

func (a *API) overview(w http.ResponseWriter, r *http.Request, u *store.User) {
	rg, err := OverviewRange(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	v, err := a.svc.Overview(r.Context(), u, rg)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toOverviewV1(v))
}

type overviewV1 struct {
	Failing        []failingV1  `json:"failing"`
	Unstable       []failingV1  `json:"unstable"`
	Flaky          []flakyV1    `json:"flaky"`
	RecentFailures []recentV1   `json:"recent_failures"`
	MoreFailures   int          `json:"more_failures"`
	Upcoming       []upcomingV1 `json:"upcoming"`
	Groups         []groupV1    `json:"groups"`
	Charts         chartsV1     `json:"charts"`
}

type chartsV1 struct {
	Window        string        `json:"window"`
	BucketSeconds int           `json:"bucket_seconds"`
	Start         stamp         `json:"start"`
	End           stamp         `json:"end"`
	BuildsLimit   int           `json:"builds_limit"`
	Buckets       []bucketV1    `json:"buckets"`
	Groups        []groupRateV1 `json:"groups"`
}

type bucketV1 struct {
	Start             stamp `json:"start"`
	Success           int   `json:"success"`
	Unstable          int   `json:"unstable"`
	Failed            int   `json:"failed"`
	Cancelled         int   `json:"cancelled"`
	MaxRunning        int   `json:"max_running"`
	Started           int   `json:"started"`
	WaitMedianSeconds int   `json:"wait_median_seconds"`
	WaitMaxSeconds    int   `json:"wait_max_seconds"`
}

type tallyV1 struct {
	Green   int `json:"green"`
	Red     int `json:"red"`
	Percent int `json:"percent"`
}

type groupRateV1 struct {
	Name   string  `json:"name"`
	Now    tallyV1 `json:"now"`
	Before tallyV1 `json:"before"`
	Change *int    `json:"change"`
}

type failingV1 struct {
	Job   string           `json:"job"`
	Build buildSummaryV1   `json:"build"`
	Since stamp            `json:"since"`
	Flaky bool             `json:"flaky"`
	Dots  []buildSummaryV1 `json:"recent"`
}

type flakyV1 struct {
	Job      string           `json:"job"`
	Failed   int              `json:"failed"`
	Finished int              `json:"finished"`
	Percent  int              `json:"failed_percent"`
	Flips    int              `json:"flips"`
	Dots     []buildSummaryV1 `json:"recent"`
}

type recentV1 struct {
	Build      buildSummaryV1 `json:"build"`
	GreenAgain bool           `json:"green_again"`
}

type upcomingV1 struct {
	Job  string `json:"job"`
	Cron string `json:"cron"`
	At   stamp  `json:"at"`
}

type groupV1 struct {
	Name       string `json:"name"`
	Green      int    `json:"green"`
	Red        int    `json:"red"`
	NeverBuilt int    `json:"never_built"`
	Running    int    `json:"running"`
	Disabled   int    `json:"disabled"`
}

func summaries(builds []*store.Build) []buildSummaryV1 {
	out := make([]buildSummaryV1, 0, len(builds))
	for _, b := range builds {
		out = append(out, toBuildSummaryV1(b))
	}
	return out
}

func tallyOf(t store.Tally) tallyV1 { return tallyV1{t.Green, t.Red, t.Percent()} }

func toChartsV1(c *overview.Charts) chartsV1 {
	out := chartsV1{Window: c.Range.Name, BucketSeconds: int(c.Range.Bucket.Seconds()), Start: stampOf(c.Start), End: stampOf(c.End),
		BuildsLimit: c.BuildsLimit, Buckets: make([]bucketV1, 0, len(c.Buckets)), Groups: []groupRateV1{}}
	for _, b := range c.Buckets {
		out.Buckets = append(out.Buckets, bucketV1{stampOf(b.Start), b.Success, b.Unstable, b.Failed, b.Cancelled, b.MaxRunning, b.Started,
			int(b.WaitMedian.Round(time.Second).Seconds()), int(b.WaitMax.Round(time.Second).Seconds())})
	}
	for _, g := range c.Groups {
		if g.Now.Total()+g.Before.Total() == 0 {
			continue
		}
		r := groupRateV1{Name: g.Name, Now: tallyOf(g.Now), Before: tallyOf(g.Before)}
		if d, ok := g.Delta(); ok {
			r.Change = &d
		}
		out.Groups = append(out.Groups, r)
	}
	return out
}

func toOverviewV1(v *overview.Overview) overviewV1 {
	out := overviewV1{Failing: []failingV1{}, Unstable: []failingV1{}, Flaky: []flakyV1{}, RecentFailures: []recentV1{}, MoreFailures: v.MoreFailures, Upcoming: []upcomingV1{}, Groups: []groupV1{}}
	for _, f := range v.Failing {
		out.Failing = append(out.Failing, failingV1{f.Job, toBuildSummaryV1(f.Build), stampOf(f.Since), f.Flaky, summaries(f.Dots)})
	}
	for _, f := range v.Unstable {
		out.Unstable = append(out.Unstable, failingV1{f.Job, toBuildSummaryV1(f.Build), stampOf(f.Since), f.Flaky, summaries(f.Dots)})
	}
	for _, f := range v.Flaky {
		out.Flaky = append(out.Flaky, flakyV1{f.Job, f.Failed, f.Finished, f.Percent(), f.Flips, summaries(f.Dots)})
	}
	for _, f := range v.RecentFailures {
		out.RecentFailures = append(out.RecentFailures, recentV1{toBuildSummaryV1(f.Build), f.GreenAgain})
	}
	for _, u := range v.Upcoming {
		out.Upcoming = append(out.Upcoming, upcomingV1{u.Job, u.Cron, stampOf(u.At)})
	}
	for _, g := range v.Groups {
		out.Groups = append(out.Groups, groupV1{g.Name, g.Green, g.Red, g.NeverBuilt, g.Running, g.Disabled})
	}
	out.Charts = toChartsV1(v.Charts)
	return out
}
