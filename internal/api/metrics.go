package api

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/metrics"
	"github.com/ndthuan/kipp/internal/store"
)

const (
	defaultMetricHistory = 100
	// Remembered is the to of a compare that reads what the store
	// remembers instead of a build.
	Remembered = "remembered"
)

// StoreView is a store as a user may see it: the loaded jobs they may
// view that name it, and what it remembers, by metric. Orphaned is a store
// no loaded job names, which only an admin sees.
type StoreView struct {
	Store    string
	Jobs     []string
	Orphaned bool
	Metrics  []store.MetricCount
}

// CompareRow is one item of a compare: its value on each side, nil on a
// side that has none, and To minus From when both do.
type CompareRow struct {
	Item            string
	From, To, Delta *float64
}

// BuildMetricsView is what a build reported, and what its guards found.
type BuildMetricsView struct {
	Build  *store.Build
	Store  string
	Rows   []store.HistoryRow
	Guards []store.GuardRow
}

// storeJobs are the loaded jobs that name each store, in name order.
func storeJobs(snap *config.Snapshot) map[string][]string {
	out := map[string][]string{}
	for _, name := range snap.Names {
		if j := snap.Jobs[name]; j != nil {
			out[j.StoreName()] = append(out[j.StoreName()], name)
		}
	}
	return out
}

// metricStore is the loaded jobs u may view that name the store. A store
// u may not view is ErrNotFound, as a job is: none of those jobs is one
// they may view, or, for an admin, no loaded job names it and it holds
// nothing.
func (s *Service) metricStore(ctx context.Context, u *store.User, name string) ([]string, error) {
	jobs, err := s.deps.Auth.Visible(ctx, u, storeJobs(s.deps.Dispatch.Snapshot())[name])
	if err != nil || len(jobs) > 0 {
		return jobs, err
	}
	if !u.Admin || !config.ValidName(name) {
		return nil, ErrNotFound
	}
	known, err := s.deps.Store.MetricStoreKnown(ctx, name)
	if err == nil && !known {
		err = ErrNotFound
	}
	return jobs, err
}

// MetricStores are the stores u may view that remember anything, by name.
func (s *Service) MetricStores(ctx context.Context, u *store.User) ([]StoreView, error) {
	all, err := s.deps.Store.MetricStores(ctx)
	if err != nil {
		return nil, err
	}
	named := storeJobs(s.deps.Dispatch.Snapshot())
	out := []StoreView{}
	for _, st := range all {
		jobs, err := s.deps.Auth.Visible(ctx, u, named[st.Store])
		if err != nil {
			return nil, err
		}
		orphaned := len(named[st.Store]) == 0
		if len(jobs) > 0 || (orphaned && u.Admin) {
			out = append(out, StoreView{Store: st.Store, Jobs: jobs, Orphaned: orphaned, Metrics: st.Metrics})
		}
	}
	return out, nil
}

// MetricStore is one store u may view, with what it remembers by metric
// but not its values.
func (s *Service) MetricStore(ctx context.Context, u *store.User, name string) (*StoreView, error) {
	jobs, err := s.metricStore(ctx, u, name)
	if err != nil {
		return nil, err
	}
	all, err := s.deps.Store.MetricStores(ctx)
	if err != nil {
		return nil, err
	}
	v := &StoreView{Store: name, Jobs: jobs, Orphaned: len(storeJobs(s.deps.Dispatch.Snapshot())[name]) == 0}
	if i := slices.IndexFunc(all, func(m store.MetricStore) bool { return m.Store == name }); i >= 0 {
		v.Metrics = all[i].Metrics
	}
	return v, nil
}

// RecentValues are the last n values of each item of the metric the
// builds reported into the store, oldest first.
func (s *Service) RecentValues(ctx context.Context, u *store.User, name, metric string, n int) (map[string][]float64, error) {
	if _, err := s.metricStore(ctx, u, name); err != nil {
		return nil, err
	}
	return s.deps.Store.RecentValues(ctx, name, metric, n)
}

// MetricBuilds are the newest builds that reported into the store, of
// the jobs u may view, limit at most: the builds a compare may name.
func (s *Service) MetricBuilds(ctx context.Context, u *store.User, name string, limit int) ([]store.MetricBuild, error) {
	if _, err := s.metricStore(ctx, u, name); err != nil {
		return nil, err
	}
	var jobs []string // all of them, for an admin
	if !u.Admin {
		reported, err := s.deps.Store.MetricJobs(ctx, name)
		if err != nil {
			return nil, err
		}
		if jobs, err = s.deps.Auth.Visible(ctx, u, reported); err != nil || len(jobs) == 0 {
			return nil, err
		}
	}
	return s.deps.Store.MetricBuilds(ctx, name, jobs, limit)
}

// RememberedValues are what the store remembers of the metric, or of
// every metric when it is empty.
func (s *Service) RememberedValues(ctx context.Context, u *store.User, name, metric string) ([]store.Remembered, error) {
	if _, err := s.metricStore(ctx, u, name); err != nil {
		return nil, err
	}
	return s.deps.Store.RememberedValues(ctx, name, metric)
}

// MetricHistory is what builds reported of one item into the store,
// newest first: 100 unless limit says otherwise, 500 at most, and with
// before only from the builds before that one, or from the rows of that
// build before the one at seq. More says there are rows beyond the page.
func (s *Service) MetricHistory(ctx context.Context, u *store.User, name, metric, item string, before, seq int64, limit int) (page []store.HistoryRow, more bool, err error) {
	if _, err := s.metricStore(ctx, u, name); err != nil {
		return nil, false, err
	}
	if metric == "" || item == "" {
		return nil, false, fmt.Errorf("%w: metric and item are both needed", ErrBadInput)
	}
	n := rows(limit, defaultMetricHistory)
	found, err := s.deps.Store.MetricHistory(ctx, name, metric, item, before, seq, n+1)
	if err != nil {
		return nil, false, err
	}
	page, more = trim(found, n)
	return page, more, nil
}

// Compare is the metric item by item between what the build from
// reported into the store and what the build to did, or what the store
// remembers when to is Remembered. The rows are sorted by the size of
// their change, largest first, then the items on one side only, each part
// by item. The store is asked about first, so that one u may not view is
// ErrNotFound whatever else the request gets wrong.
func (s *Service) Compare(ctx context.Context, u *store.User, name, metric, from, to string) ([]CompareRow, error) {
	if _, err := s.metricStore(ctx, u, name); err != nil {
		return nil, err
	}
	if metric == "" {
		return nil, fmt.Errorf("%w: metric is needed", ErrBadInput)
	}
	if from == Remembered {
		return nil, fmt.Errorf("%w: from is a build id", ErrBadInput)
	}
	left, err := s.compareSide(ctx, u, "from", from, name, metric)
	if err != nil {
		return nil, err
	}
	right, err := s.compareSide(ctx, u, "to", to, name, metric)
	if err != nil {
		return nil, err
	}
	return compareRows(left, right), nil
}

// compareSide is one side of a compare, named side in what it refuses:
// the store's remembered values, or what a build reported.
func (s *Service) compareSide(ctx context.Context, u *store.User, side, ref, name, metric string) (map[string]float64, error) {
	if ref == Remembered {
		rs, err := s.deps.Store.RememberedValues(ctx, name, metric)
		out := make(map[string]float64, len(rs))
		for _, r := range rs {
			out[r.Item] = r.Value
		}
		return out, err
	}
	id, err := strconv.ParseInt(ref, 10, 64)
	if err != nil || id < 1 {
		return nil, fmt.Errorf("%w: %s is a build id or %s", ErrBadInput, side, Remembered)
	}
	return s.reportedBy(ctx, u, id, name, metric)
}

// reportedBy is what the build u may view reported of the metric into the
// store, item to value, a later row winning.
func (s *Service) reportedBy(ctx context.Context, u *store.User, id int64, name, metric string) (map[string]float64, error) {
	if _, err := s.build(ctx, u, id, "view"); err != nil {
		return nil, err
	}
	rs, err := s.deps.Store.BuildMetric(ctx, id, name, metric)
	out := make(map[string]float64, len(rs))
	for _, r := range rs {
		out[r.Item] = r.Value
	}
	return out, err
}

func compareRows(from, to map[string]float64) []CompareRow {
	out := make([]CompareRow, 0, len(from)+len(to))
	for item, v := range from {
		row := CompareRow{Item: item, From: new(v)}
		if w, ok := to[item]; ok {
			row.To, row.Delta = new(w), new(delta(v, w))
		}
		out = append(out, row)
	}
	for item, w := range to {
		if _, ok := from[item]; !ok {
			out = append(out, CompareRow{Item: item, To: new(w)})
		}
	}
	slices.SortFunc(out, func(a, b CompareRow) int {
		if c := cmp.Compare(btoi(a.Delta == nil), btoi(b.Delta == nil)); c != 0 || a.Delta == nil {
			return cmp.Or(c, cmp.Compare(a.Item, b.Item))
		}
		return cmp.Or(cmp.Compare(math.Abs(*b.Delta), math.Abs(*a.Delta)), cmp.Compare(a.Item, b.Item))
	})
	return out
}

// delta is to minus from, rounded to as many decimals as the two are
// written with: 86.9 - 87.19 is -0.29, not the float's -0.28999999999999204.
func delta(from, to float64) float64 {
	digits := max(decimals(from), decimals(to))
	d, err := strconv.ParseFloat(strconv.FormatFloat(to-from, 'f', digits, 64), 64)
	if err != nil {
		return to - from
	}
	return d
}

// decimals is how many digits v needs after the point.
func decimals(v float64) int {
	s := strconv.FormatFloat(v, 'f', -1, 64)
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return len(s) - i - 1
	}
	return 0
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// BuildMetrics is what the build reported, stage by stage, and what its
// guards found.
func (s *Service) BuildMetrics(ctx context.Context, u *store.User, id int64) (*BuildMetricsView, error) {
	b, err := s.build(ctx, u, id, "view")
	if err != nil {
		return nil, err
	}
	v := &BuildMetricsView{Build: b}
	if v.Rows, err = s.deps.Store.BuildMetrics(ctx, id); err != nil {
		return nil, err
	}
	if v.Guards, err = s.deps.Store.GuardResults(ctx, id); err != nil {
		return nil, err
	}
	switch job := s.deps.Dispatch.Snapshot().Jobs[b.Job]; {
	case len(v.Rows) > 0:
		v.Store = v.Rows[0].Store
	case job != nil:
		v.Store = job.StoreName()
	}
	return v, nil
}

// GuardLine is a guard's result as the stage's log has it.
func GuardLine(r store.GuardRow) string {
	res := metrics.GuardResult{Metric: r.Metric, Item: r.Item, Before: r.Remembered, After: r.Reported,
		Tolerance: r.Tolerance, Verdict: r.Verdict, Outcome: r.Outcome}
	if r.FromJob != "" {
		res.From = &metrics.From{Job: r.FromJob, Number: r.FromNumber}
	}
	return res.Line()
}

type metricCountJSON struct {
	Name  string `json:"name"`
	Items int    `json:"items"`
}

type storeJSON struct {
	Store    string            `json:"store"`
	Jobs     []string          `json:"jobs"`
	Orphaned bool              `json:"orphaned"`
	Metrics  []metricCountJSON `json:"metrics"`
}

type compareJSON struct {
	Item  string   `json:"item"`
	From  *float64 `json:"from"`
	To    *float64 `json:"to"`
	Delta *float64 `json:"delta"`
}

type reportedJSON struct {
	Name   string             `json:"name"`
	Unit   string             `json:"unit"`
	Values map[string]float64 `json:"values"`
}

func toStoreJSON(v StoreView) storeJSON {
	out := storeJSON{Store: v.Store, Jobs: append([]string{}, v.Jobs...), Orphaned: v.Orphaned, Metrics: []metricCountJSON{}}
	for _, m := range v.Metrics {
		out.Metrics = append(out.Metrics, metricCountJSON(m))
	}
	return out
}

func (a *API) metricStores(w http.ResponseWriter, r *http.Request, u *store.User) {
	stores, err := a.svc.MetricStores(r.Context(), u)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := make([]storeJSON, 0, len(stores))
	for _, v := range stores {
		out = append(out, toStoreJSON(v))
	}
	writeList(w, out, "")
}

// metricStore answers one store of the list.
func (a *API) metricStore(w http.ResponseWriter, r *http.Request, u *store.User) {
	stores, err := a.svc.MetricStores(r.Context(), u)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	for _, v := range stores {
		if v.Store == r.PathValue("store") {
			writeJSON(w, http.StatusOK, toStoreJSON(v))
			return
		}
	}
	a.fail(w, r, ErrNotFound)
}

func (a *API) remembered(w http.ResponseWriter, r *http.Request, u *store.User) {
	rs, err := a.svc.RememberedValues(r.Context(), u, r.PathValue("store"), r.URL.Query().Get("metric"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := make([]rememberedV1, 0, len(rs))
	for _, v := range rs {
		out = append(out, rememberedV1{v.Metric, v.Item, v.Value, v.Unit, rememberedRef(v.BuildID, v.Job, v.Number, v.Status), stampOf(v.At)})
	}
	writeList(w, out, "")
}

// historyQuery is how many rows of a metric's history a request asks for and
// the position they are asked from: below the row at seq of the build before.
// It pages by cursor and refuses one given wrongly.
func historyQuery(r *http.Request) (limit int, before, seq int64, err error) {
	limit, at, err := pageArgs(r, "history", 2)
	if len(at) == 2 {
		before, seq = at[0], at[1]
	}
	return limit, before, seq, err
}

func (a *API) metricHistory(w http.ResponseWriter, r *http.Request, u *store.User) {
	q := r.URL.Query()
	limit, before, seq, err := historyQuery(r)
	var (
		rs   []store.HistoryRow
		more bool
	)
	if err == nil {
		rs, more, err = a.svc.MetricHistory(r.Context(), u, r.PathValue("store"), q.Get("metric"), q.Get("item"), before, seq, limit)
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	next := ""
	if more {
		last := rs[len(rs)-1]
		next = cursorFor("history", last.BuildID, last.Seq)
	}
	out := make([]historyV1, 0, len(rs))
	for _, v := range rs {
		out = append(out, historyV1{buildRefV1{v.BuildID, v.Job, v.Number, statusOrNil(v.Status)}, v.Stage, v.Value, v.Unit, stampOf(v.At)})
	}
	writeList(w, out, next)
}

func (a *API) compare(w http.ResponseWriter, r *http.Request, u *store.User) {
	q := r.URL.Query()
	rows, err := a.svc.Compare(r.Context(), u, r.PathValue("store"), q.Get("metric"), q.Get("from"), q.Get("to"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := make([]compareJSON, 0, len(rows))
	for _, v := range rows {
		out = append(out, compareJSON(v))
	}
	writeList(w, out, "")
}

func (a *API) buildMetrics(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, err := BuildID(r)
	var v *BuildMetricsView
	if err == nil {
		v, err = a.svc.BuildMetrics(r.Context(), u, id)
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toBuildMetricsV1(v))
}
