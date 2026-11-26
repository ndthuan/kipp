package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/metrics"
	"github.com/ndthuan/kipp/internal/store"
)

const baselineDir = ".kipp"

func baselineName(stage string) string {
	return filepath.Join(baselineDir, "metrics-"+stage+".json")
}

type reports struct {
	mu        sync.Mutex
	order     []string
	byName    map[string]*store.Metric
	baselines map[string][]byte
}

func (r *reports) given(stage string, data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.baselines == nil {
		r.baselines = map[string][]byte{}
	}
	r.baselines[stage] = data
}

func (r *reports) baseline(stage string) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.baselines[stage]
}

func (r *reports) add(ms []store.Metric) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byName == nil {
		r.byName = map[string]*store.Metric{}
	}
	for _, m := range ms {
		got := r.byName[m.Name]
		if got == nil {
			got = &store.Metric{Name: m.Name, Values: map[string]float64{}}
			r.byName[m.Name] = got
			r.order = append(r.order, m.Name)
		}
		if m.Unit != "" {
			got.Unit = m.Unit
		}
		got.Complete = got.Complete || m.Complete
		maps.Copy(got.Values, m.Values)
	}
}

func (r *reports) all() []store.Metric {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]store.Metric, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, *r.byName[name])
	}
	return out
}

func (d *Dispatcher) baseline(ctx context.Context, e *buildEnv, s *config.Stage, out io.Writer) []byte {
	if s.Metrics == nil {
		return nil
	}
	stored, err := d.st.RememberedValues(context.WithoutCancel(ctx), e.store, "")
	var data []byte
	if err == nil {
		remembered := make([]metrics.Remembered, len(stored))
		for i, r := range stored {
			remembered[i] = metrics.Remembered{Metric: r.Metric, Item: r.Item, Unit: r.Unit, Value: r.Value,
				From: metrics.From{Job: r.Job, Number: r.Number, At: r.At}}
		}
		data, err = metrics.Baseline(e.store, remembered, s.Metrics.Guard)
	}
	if err != nil {
		_, _ = fmt.Fprintf(out, "kipp: metrics: what store %s remembers could not be read, and KIPP_METRICS is not set: %v\n", e.store, err)
		return nil
	}
	e.reports.given(s.Name, data)
	return data
}

func writeBaseline(workspace, stage string, data []byte) (string, error) {
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	if err := root.MkdirAll(baselineDir, 0o750); err != nil {
		return "", err
	}
	name := baselineName(stage)
	if err := root.WriteFile(name, data, 0o600); err != nil {
		return "", err
	}
	return "KIPP_METRICS=" + filepath.Join(workspace, name), nil
}

func (d *Dispatcher) report(b *store.Build, e *buildEnv, s *config.Stage, dir string) []metrics.Metric {
	if s.Metrics == nil || s.Metrics.File == "" {
		return nil
	}
	reported, errs := metrics.ParseGlob(dir, s.Metrics.File)
	for _, err := range errs {
		d.note(b, s.Name, e, fmt.Sprintf("kipp: metrics: %v", err))
	}
	return reported
}

func measured(s *config.Stage, took time.Duration, reported []metrics.Metric, secrets []string) []store.Metric {
	ms := []store.Metric{{Name: config.StageSeconds, Unit: "s", Values: map[string]float64{s.Name: took.Seconds()}}}
	for _, m := range reported {
		ms = append(ms, maskedMetric(store.Metric(m), secrets))
	}
	return ms
}

func maskedMetric(m store.Metric, secrets []string) store.Metric {
	out := store.Metric{Name: m.Name, Unit: masked(m.Unit, secrets), Complete: m.Complete, Values: make(map[string]float64, len(m.Values))}
	for _, item := range slices.Sorted(maps.Keys(m.Values)) {
		out.Values[masked(item, secrets)] = m.Values[item]
	}
	return out
}

func (d *Dispatcher) keep(ctx context.Context, b *store.Build, e *buildEnv, stage string, ms []store.Metric) {
	ms = slices.DeleteFunc(ms, func(m store.Metric) bool { return len(m.Values) == 0 && !m.Complete })
	if len(ms) == 0 {
		return
	}
	if err := d.st.PutMetricHistory(context.WithoutCancel(ctx), b.ID, e.store, stage, ms); err != nil {
		d.log.Error("keeping a stage's metrics", "build", b.ID, "stage", stage, "err", err)
	}
	e.reports.add(ms)
}

func (d *Dispatcher) remember(ctx context.Context, b *store.Build, job *config.Job, e *buildEnv, status store.Status) {
	push := pushOf(b)
	var keep []store.Metric
	for _, m := range e.reports.all() {
		if job.Remembers(m.Name, push, string(status)) {
			keep = append(keep, m)
		}
	}
	if len(keep) == 0 {
		return
	}
	from := store.Origin{BuildID: b.ID, Job: b.Job, Number: b.Number}
	if err := d.st.Remember(ctx, e.store, from, time.Now(), keep); err != nil {
		d.log.Error("remembering a build's metrics", "job", b.Job, "number", b.Number, "store", e.store, "err", err)
	}
}

func (d *Dispatcher) guard(ctx context.Context, b *store.Build, e *buildEnv, s *config.Stage, reported []metrics.Metric, status store.StageStatus) store.StageStatus {
	if s.Metrics == nil || len(s.Metrics.Guard) == 0 || (status != store.StageSuccess && status != store.StageUnstable) {
		return status
	}
	results := metrics.GuardAll(d.given(b, e, s), reported)
	rows := make([]store.GuardRow, len(results))
	for i, r := range results {
		d.note(b, s.Name, e, r.Line())
		rows[i] = guardRow(s.Name, r, e.secrets)
	}
	if err := d.st.PutGuardResults(context.WithoutCancel(ctx), b.ID, rows); err != nil {
		d.log.Error("keeping a stage's guard results", "build", b.ID, "stage", s.Name, "err", err)
	}
	switch metrics.Worst(results) {
	case metrics.OutcomeFailed:
		return store.StageFailed
	case metrics.OutcomeUnstable:
		return store.StageUnstable
	}
	return status
}

func (d *Dispatcher) given(b *store.Build, e *buildEnv, s *config.Stage) metrics.File {
	f := metrics.File{Store: e.store, Guards: metrics.FileGuards(s.Metrics.Guard)}
	if data := e.reports.baseline(s.Name); data != nil {
		var got metrics.File
		if err := json.Unmarshal(data, &got); err != nil {
			d.log.Error("reading back a stage's KIPP_METRICS", "build", b.ID, "stage", s.Name, "err", err)
			return f
		}
		f = got
	}
	return f
}

func guardRow(stage string, r metrics.GuardResult, secrets []string) store.GuardRow {
	row := store.GuardRow{Stage: stage, Metric: r.Metric, Item: masked(r.Item, secrets), Remembered: r.Before, Reported: r.After,
		Tolerance: r.Tolerance, Verdict: r.Verdict, Outcome: r.Outcome}
	if r.From != nil {
		row.FromJob, row.FromNumber = r.From.Job, r.From.Number
	}
	return row
}

func executable() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	return p
}
