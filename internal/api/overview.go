package api

import (
	"context"
	"net/http"
	"time"

	"github.com/ndthuan/kipp/internal/overview"
	"github.com/ndthuan/kipp/internal/store"
)

// Overview is what is red, flaky, recently failed, coming up and how each
// group stands, over the jobs u may view that build themselves.
func (s *Service) Overview(ctx context.Context, u *store.User) (*overview.Overview, error) {
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
	jobs := make([]overview.Job, 0, len(names))
	for _, n := range names {
		j := snap.Jobs[n]
		running := last[n] != nil && !last[n].Status.Terminal()
		jobs = append(jobs, overview.Job{Name: n, Group: j.Group, Disabled: j.Disabled, Cron: j.Triggers.Cron,
			Next: nextFire(j, now, s.deps.Dispatch.Zone(j)), Running: running, History: history[n]})
	}
	return overview.Compute(jobs, s.deps.FlakyFlips, now), nil
}

func (a *API) overview(w http.ResponseWriter, r *http.Request, u *store.User) {
	v, err := a.svc.Overview(r.Context(), u)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toOverviewV1(v))
}

type overviewV1 struct {
	Failing        []failingV1  `json:"failing"`
	Flaky          []flakyV1    `json:"flaky"`
	RecentFailures []recentV1   `json:"recent_failures"`
	MoreFailures   int          `json:"more_failures"`
	Upcoming       []upcomingV1 `json:"upcoming"`
	Groups         []groupV1    `json:"groups"`
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

func toOverviewV1(v *overview.Overview) overviewV1 {
	out := overviewV1{Failing: []failingV1{}, Flaky: []flakyV1{}, RecentFailures: []recentV1{}, MoreFailures: v.MoreFailures, Upcoming: []upcomingV1{}, Groups: []groupV1{}}
	for _, f := range v.Failing {
		out.Failing = append(out.Failing, failingV1{f.Job, toBuildSummaryV1(f.Build), stampOf(f.Since), f.Flaky, summaries(f.Dots)})
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
	return out
}
