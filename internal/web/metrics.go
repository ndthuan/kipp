package web

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

const (
	// sparkValues is how many of an item's last values its sparkline draws.
	sparkValues = 50
	// shownRows is the most rows a table of the page lists: a metric may
	// hold 100,000 items, a page that long helps nobody, and the API has
	// them all.
	shownRows = 1000
	// compareBuilds is how many of the newest builds the compare form
	// offers.
	compareBuilds = 100
	// metricsHere is where the header marks a metrics page.
	metricsHere = "/metrics"
)

type metricsData struct {
	Stores []api.StoreView
}

// metricRow is one item of a metric as the store remembers it.
type metricRow struct {
	Item, Value, Unit string
	// From names the build that set the value, and Build is its page:
	// empty for a value put by hand.
	From, Build string
	At          time.Time
	Spark       string // its last values, oldest first, as the script reads them
}

// compareRow is a row of a compare as the page shows it.
type compareRow struct {
	Item, From, To, Delta string
	// Change is the size of Delta, for the script to sort by; Sign is
	// fell, rose or none.
	Change float64
	Sign   string
}

type metricsStoreData struct {
	Store  *api.StoreView
	Metric string
	Rows   []metricRow
	Total  int
	Builds []store.MetricBuild
	// From and To are what the compare form was sent with; Compared says
	// it was, and Compare holds its rows, of CompareTotal.
	From, To     string
	Compared     bool
	Compare      []compareRow
	CompareTotal int
	CompareError string
}

func (s *Site) metricsIndex(w http.ResponseWriter, r *http.Request, u *store.User) {
	stores, err := s.api.Service().MetricStores(r.Context(), u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, "metrics", http.StatusOK, page{Title: "Metrics", Here: metricsHere, Data: metricsData{Stores: stores}})
}

func (s *Site) metricsStore(w http.ResponseWriter, r *http.Request, u *store.User) {
	data, err := s.metricsStoreData(r, u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, "metrics_store", http.StatusOK, page{Title: data.Store.Store + " metrics", Here: metricsHere, Data: data})
}

func (s *Site) metricsStoreData(r *http.Request, u *store.User) (*metricsStoreData, error) {
	ctx, svc, name := r.Context(), s.api.Service(), r.PathValue("store")
	v, err := svc.MetricStore(ctx, u, name)
	if err != nil {
		return nil, err
	}
	q := r.URL.Query()
	d := &metricsStoreData{Store: v, Metric: pickMetric(v.Metrics, q.Get("metric")), From: q.Get("from"), To: q.Get("to")}
	if d.Metric == "" {
		return d, nil
	}
	remembered, err := svc.RememberedValues(ctx, u, name, d.Metric)
	if err != nil {
		return nil, err
	}
	recent, err := svc.RecentValues(ctx, u, name, d.Metric, sparkValues)
	if err != nil {
		return nil, err
	}
	d.Total = len(remembered)
	d.Rows = metricRows(remembered[:min(len(remembered), shownRows)], recent)
	if d.Builds, err = svc.MetricBuilds(ctx, u, name, compareBuilds); err != nil {
		return nil, err
	}
	return d, s.compare(r, u, d)
}

// compare fills in the compare the form asked for, if it did. What it got
// wrong, or a build it may not see, is said on the page above the form.
func (s *Site) compare(r *http.Request, u *store.User, d *metricsStoreData) error {
	if d.From == "" {
		return nil
	}
	d.Compared = true
	rows, err := s.api.Service().Compare(r.Context(), u, d.Store.Store, d.Metric, d.From, d.To)
	switch {
	case errors.Is(err, api.ErrBadInput):
		d.CompareError = plainly(err) + "."
		return nil
	case errors.Is(err, api.ErrNotFound):
		d.CompareError = "There is no such build, or none you may see."
		return nil
	case err != nil:
		return err
	}
	d.CompareTotal = len(rows)
	for _, row := range rows[:min(len(rows), shownRows)] {
		d.Compare = append(d.Compare, compareRowOf(row))
	}
	return nil
}

// pickMetric is the metric the page shows: the one asked for when the
// store remembers it, else the first that is not one of kipp's own, else
// the first.
func pickMetric(ms []store.MetricCount, asked string) string {
	if slices.ContainsFunc(ms, func(m store.MetricCount) bool { return m.Name == asked }) {
		return asked
	}
	for _, m := range ms {
		if !strings.HasPrefix(m.Name, config.MetricPrefix) {
			return m.Name
		}
	}
	if len(ms) > 0 {
		return ms[0].Name
	}
	return ""
}

func metricRows(rs []store.Remembered, recent map[string][]float64) []metricRow {
	out := make([]metricRow, len(rs))
	for i, v := range rs {
		row := metricRow{Item: v.Item, Value: number(v.Value), Unit: v.Unit, From: "by hand", At: v.At}
		if v.Number > 0 {
			row.From = v.Job + " #" + strconv.Itoa(v.Number)
			row.Build = "/jobs/" + url.PathEscape(v.Job) + "/" + strconv.Itoa(v.Number)
		}
		spark := make([]string, len(recent[v.Item]))
		for j, x := range recent[v.Item] {
			spark[j] = number(x)
		}
		row.Spark = strings.Join(spark, " ")
		out[i] = row
	}
	return out
}

func compareRowOf(c api.CompareRow) compareRow {
	side := func(v *float64) string {
		if v == nil {
			return ""
		}
		return number(*v)
	}
	row := compareRow{Item: c.Item, From: side(c.From), To: side(c.To), Delta: side(c.Delta), Sign: "none"}
	if c.Delta != nil {
		row.Change = *c.Delta
		switch {
		case *c.Delta < 0:
			row.Sign = "fell"
		case *c.Delta > 0:
			row.Sign, row.Delta = "rose", "+"+row.Delta
		}
	}
	return row
}

// number is a value as the page and the API's JSON write it: as few
// digits as say it exactly.
func number(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// metricsPath is the page of a store, at a metric when one is given.
func metricsPath(store, metric string) string {
	p := "/metrics/" + url.PathEscape(store)
	if metric != "" {
		p += "?metric=" + url.QueryEscape(metric)
	}
	return p
}
