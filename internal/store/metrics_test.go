package store

import (
	"slices"
	"testing"
	"time"
)

func finishedBuild(t *testing.T, s *Store, job string) *Build {
	t.Helper()
	b := &Build{Job: job, Status: Queued, Trigger: "cli"}
	must(t, s.CreateBuild(ctx, b))
	return b
}

// values is what the store remembers of one metric, item to value.
func values(t *testing.T, s *Store, store, metric string) map[string]float64 {
	t.Helper()
	rs, err := s.RememberedValues(ctx, store, metric)
	must(t, err)
	out := map[string]float64{}
	for _, r := range rs {
		out[r.Item] = r.Value
	}
	return out
}

func sameValues(t *testing.T, what string, got, want map[string]float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %v, want %v", what, got, want)
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			t.Fatalf("%s: %v, want %v", what, got, want)
		}
	}
}

func TestACompleteReportForgetsTheItemsItDoesNotName(t *testing.T) {
	s := open(t)
	b := finishedBuild(t, s, "site-smoke")
	at := time.Date(2026, 10, 29, 3, 0, 0, 0, time.UTC)
	from := Origin{BuildID: b.ID, Job: b.Job, Number: b.Number}
	must(t, s.Remember(ctx, "site", from, at, []Metric{
		{Name: "seconds", Unit: "s", Values: map[string]float64{"a": 1, "b": 2, "c": 3}},
		{Name: "coverage", Unit: "%", Values: map[string]float64{"total": 80}},
	}))
	must(t, s.Remember(ctx, "site", from, at.Add(time.Hour), []Metric{
		{Name: "seconds", Unit: "s", Complete: true, Values: map[string]float64{"b": 5, "d": 4}},
	}))
	sameValues(t, "seconds", values(t, s, "site", "seconds"), map[string]float64{"b": 5, "d": 4})
	sameValues(t, "coverage, which the report does not hold", values(t, s, "site", "coverage"), map[string]float64{"total": 80})
	rs, err := s.RememberedValues(ctx, "site", "seconds")
	must(t, err)
	if r := rs[0]; r.Store != "site" || r.Unit != "s" || r.Job != "site-smoke" || r.Number != 1 || r.BuildID != b.ID || !r.At.Equal(at.Add(time.Hour)) {
		t.Fatalf("%+v", r)
	}
}

func TestAMergingReportKeepsTheOthers(t *testing.T) {
	s := open(t)
	first, second := finishedBuild(t, s, "site-smoke"), finishedBuild(t, s, "site-deploy")
	at := time.Date(2026, 10, 29, 3, 0, 0, 0, time.UTC)
	must(t, s.Remember(ctx, "site", Origin{BuildID: first.ID, Job: first.Job, Number: first.Number}, at, []Metric{
		{Name: "seconds", Values: map[string]float64{"a": 1, "b": 2}},
	}))
	must(t, s.Remember(ctx, "site", Origin{BuildID: second.ID, Job: second.Job, Number: second.Number}, at, []Metric{
		{Name: "seconds", Values: map[string]float64{"b": 7}},
	}))
	sameValues(t, "seconds", values(t, s, "site", "seconds"), map[string]float64{"a": 1, "b": 7})
	rs, err := s.RememberedValues(ctx, "site", "")
	must(t, err)
	if rs[0].Item != "a" || rs[0].Job != "site-smoke" || rs[1].Job != "site-deploy" {
		t.Fatalf("each item names the build that set it: %+v", rs)
	}
	sameValues(t, "another store", values(t, s, "other", "seconds"), map[string]float64{})
}

func TestHistoryGoesWithItsBuild(t *testing.T) {
	s := open(t)
	b, kept := finishedBuild(t, s, "site-smoke"), finishedBuild(t, s, "site-smoke")
	for _, id := range []int64{b.ID, kept.ID} {
		must(t, s.PutMetricHistory(ctx, id, "site", "measure", []Metric{{Name: "seconds", Unit: "s", Values: map[string]float64{"a": float64(id)}}}))
	}
	rows, err := s.BuildMetrics(ctx, b.ID)
	must(t, err)
	if len(rows) != 1 || rows[0].Stage != "measure" || rows[0].Item != "a" || rows[0].Value != float64(b.ID) || rows[0].Unit != "s" || rows[0].Number != 1 {
		t.Fatalf("%+v", rows)
	}
	hist, err := s.MetricHistory(ctx, "site", "seconds", "a", 0, 0, 100)
	must(t, err)
	if len(hist) != 2 || hist[0].BuildID != kept.ID {
		t.Fatalf("newest first: %+v", hist)
	}
	must(t, s.DeleteBuild(ctx, b.ID))
	if rows, err = s.BuildMetrics(ctx, b.ID); err != nil || len(rows) != 0 {
		t.Fatalf("%v %+v", err, rows)
	}
	hist, err = s.MetricHistory(ctx, "site", "seconds", "a", 0, 0, 100)
	must(t, err)
	if len(hist) != 1 || hist[0].BuildID != kept.ID {
		t.Fatalf("%+v", hist)
	}
	if hist, _ = s.MetricHistory(ctx, "site", "seconds", "a", kept.ID, 0, 100); len(hist) != 0 {
		t.Fatalf("before the newest: %+v", hist)
	}
}

func TestARememberedValueOutlivesItsBuild(t *testing.T) {
	s := open(t)
	b := finishedBuild(t, s, "site-deploy")
	must(t, s.Remember(ctx, "site", Origin{BuildID: b.ID, Job: b.Job, Number: b.Number}, time.Now(), []Metric{{Name: "coverage", Values: map[string]float64{"total": 87.19}}}))
	must(t, s.DeleteBuild(ctx, b.ID))
	rs, err := s.RememberedValues(ctx, "site", "coverage")
	must(t, err)
	if len(rs) != 1 || rs[0].Value != 87.19 || rs[0].Job != "site-deploy" || rs[0].Number != 1 {
		t.Fatalf("%+v", rs)
	}
}

func TestForgetTakesAnItemAMetricOrAStore(t *testing.T) {
	s := open(t)
	must(t, s.Remember(ctx, "site", Origin{Job: "cli"}, time.Now(), []Metric{
		{Name: "seconds", Values: map[string]float64{"a": 1, "b": 2}},
		{Name: "coverage", Values: map[string]float64{"total": 1}},
	}))
	must(t, s.Remember(ctx, "other", Origin{Job: "cli"}, time.Now(), []Metric{{Name: "seconds", Values: map[string]float64{"a": 1}}}))
	if n, err := s.Forget(ctx, "site", "seconds", "a"); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	sameValues(t, "seconds", values(t, s, "site", "seconds"), map[string]float64{"b": 2})
	if n, err := s.Forget(ctx, "site", "", ""); err != nil || n != 2 {
		t.Fatal(n, err)
	}
	stores, err := s.MetricStores(ctx)
	must(t, err)
	if len(stores) != 1 || stores[0].Store != "other" || !slices.Equal(stores[0].Metrics, []MetricCount{{Name: "seconds", Items: 1}}) {
		t.Fatalf("%+v", stores)
	}
}

func TestGuardResultsGoWithTheirBuild(t *testing.T) {
	s := open(t)
	b, kept := finishedBuild(t, s, "site-smoke"), finishedBuild(t, s, "site-smoke")
	was, now := 87.19, 86.9
	for _, id := range []int64{b.ID, kept.ID} {
		must(t, s.PutGuardResults(ctx, id, []GuardRow{
			{Stage: "measure", Metric: "coverage", Item: "total", Remembered: &was, Reported: &now, Tolerance: 0.01, Verdict: "fell", Outcome: "failed", FromJob: "site-deploy", FromNumber: 123},
			{Stage: "measure", Metric: "coverage", Item: "lib/a", Verdict: "missing", Outcome: "failed"},
		}))
	}
	rows, err := s.GuardResults(ctx, b.ID)
	must(t, err)
	if len(rows) != 2 || *rows[0].Remembered != was || *rows[0].Reported != now || rows[0].FromNumber != 123 || rows[1].Remembered != nil || rows[1].Reported != nil {
		t.Fatalf("%+v", rows)
	}
	must(t, s.DeleteBuild(ctx, b.ID))
	if rows, err = s.GuardResults(ctx, b.ID); err != nil || len(rows) != 0 {
		t.Fatalf("%v %+v", err, rows)
	}
	if rows, err = s.GuardResults(ctx, kept.ID); err != nil || len(rows) != 2 {
		t.Fatalf("%v %+v", err, rows)
	}
}

func TestABuildsMetricIsReadAloneAndItsHistorySaysWhenTheBuildEnded(t *testing.T) {
	s := open(t)
	b := finishedBuild(t, s, "site-smoke")
	ended := time.Date(2026, 10, 29, 3, 0, 0, 0, time.UTC)
	must(t, s.SetBuildStatus(ctx, b.ID, Success, ended))
	must(t, s.PutMetricHistory(ctx, b.ID, "site", "unit", []Metric{{Name: "seconds", Unit: "s", Values: map[string]float64{"a": 1, "b": 2}}}))
	must(t, s.PutMetricHistory(ctx, b.ID, "site", "e2e", []Metric{{Name: "seconds", Unit: "s", Values: map[string]float64{"a": 3}}, {Name: "coverage", Values: map[string]float64{"total": 80}}}))
	must(t, s.PutMetricHistory(ctx, b.ID, "other", "unit", []Metric{{Name: "seconds", Values: map[string]float64{"a": 9}}}))
	rows, err := s.BuildMetric(ctx, b.ID, "site", "seconds")
	must(t, err)
	if len(rows) != 3 || rows[2].Stage != "e2e" || rows[2].Value != 3 || !rows[0].At.Equal(ended) {
		t.Fatalf("%+v", rows)
	}
	for store, want := range map[string]bool{"site": true, "other": true, "none": false} {
		if known, err := s.MetricStoreKnown(ctx, store); err != nil || known != want {
			t.Fatalf("%s: %v %v", store, known, err)
		}
	}
	must(t, s.Remember(ctx, "kept", Origin{}, ended, []Metric{{Name: "seconds", Values: map[string]float64{"a": 1}}}))
	if known, err := s.MetricStoreKnown(ctx, "kept"); err != nil || !known {
		t.Fatalf("a store that only remembers: %v %v", known, err)
	}
}

func TestRecentValuesAreTheLastOfEachItemOldestFirst(t *testing.T) {
	s := open(t)
	for i := range 5 {
		b := finishedBuild(t, s, "site-smoke")
		must(t, s.PutMetricHistory(ctx, b.ID, "site", "measure", []Metric{{Name: "seconds", Values: map[string]float64{"a": float64(i), "b": float64(10 * i)}}}))
		if i == 4 {
			must(t, s.PutMetricHistory(ctx, b.ID, "elsewhere", "measure", []Metric{{Name: "seconds", Values: map[string]float64{"a": 99}}}))
		}
	}
	got, err := s.RecentValues(ctx, "site", "seconds", 3)
	must(t, err)
	if !slices.Equal(got["a"], []float64{2, 3, 4}) || !slices.Equal(got["b"], []float64{20, 30, 40}) || len(got) != 2 {
		t.Fatalf("%v", got)
	}
	builds, err := s.MetricBuilds(ctx, "site", []string{"site-smoke", "other"}, 2)
	must(t, err)
	if len(builds) != 2 || builds[0].Number != 5 || builds[1].Number != 4 || builds[0].Job != "site-smoke" {
		t.Fatalf("%+v", builds)
	}
	if builds, err = s.MetricBuilds(ctx, "site", []string{"other"}, 2); err != nil || len(builds) != 0 {
		t.Fatalf("%v %+v", err, builds)
	}
	if builds, err = s.MetricBuilds(ctx, "elsewhere", nil, 10); err != nil || len(builds) != 1 || builds[0].Number != 5 {
		t.Fatalf("every job's: %v %+v", err, builds)
	}
	if jobs, err := s.MetricJobs(ctx, "site"); err != nil || !slices.Equal(jobs, []string{"site-smoke"}) {
		t.Fatalf("%v %v", err, jobs)
	}
}

func TestAMetricRowSaysHowTheBuildItCameFromEndedAndNothingOnceItIsGone(t *testing.T) {
	s := open(t)
	b := finishedBuild(t, s, "site-deploy")
	must(t, s.SetBuildStatus(ctx, b.ID, Failed, time.Now()))
	must(t, s.Remember(ctx, "site", Origin{BuildID: b.ID, Job: b.Job, Number: b.Number}, time.Now(), []Metric{{Name: "coverage", Values: map[string]float64{"total": 1}}}))
	must(t, s.PutMetricHistory(ctx, b.ID, "site", "m", []Metric{{Name: "coverage", Values: map[string]float64{"total": 1}}}))
	must(t, s.PutGuardResults(ctx, b.ID, []GuardRow{{Stage: "m", Metric: "coverage", Item: "total", Verdict: "held", Outcome: "held", FromJob: b.Job, FromNumber: b.Number},
		{Stage: "m", Metric: "coverage", Item: "gone", Verdict: "held", Outcome: "held", FromJob: "pruned", FromNumber: 9}}))

	remembered, err := s.RememberedValues(ctx, "site", "coverage")
	must(t, err)
	history, err := s.BuildMetrics(ctx, b.ID)
	must(t, err)
	guards, err := s.GuardResults(ctx, b.ID)
	must(t, err)
	if remembered[0].Status != Failed || history[0].Status != Failed {
		t.Fatalf("remembered %+v, history %+v", remembered[0], history[0])
	}
	if guards[0].FromBuildID != b.ID || guards[0].FromStatus != Failed {
		t.Fatalf("the guard's origin: %+v", guards[0])
	}
	if guards[1].FromBuildID != 0 || guards[1].FromStatus != "" {
		t.Fatalf("the origin of a build that is gone: %+v", guards[1])
	}
	// The remembered value outlives the build; its status then is none.
	must(t, s.DeleteBuild(ctx, b.ID))
	if remembered, err = s.RememberedValues(ctx, "site", "coverage"); err != nil || remembered[0].Status != "" || remembered[0].Job != b.Job {
		t.Fatalf("%v %+v", err, remembered)
	}
}
