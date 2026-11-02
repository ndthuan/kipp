package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"maps"
	"slices"
	"time"
)

// Metric is one metric of a report: a named number per item.
type Metric struct {
	Name, Unit string
	// Complete says the report holds every item of the metric: the store
	// forgets the items it does not name. Otherwise the report merges.
	Complete bool
	Values   map[string]float64
}

// Origin is the build a remembered value came from: kept by its job and
// number, which outlive the build's row. A value put by hand has no build.
type Origin struct {
	BuildID int64
	Job     string
	Number  int
}

// Remembered is a store's value of one item of a metric.
type Remembered struct {
	Store, Metric, Item, Unit string
	Value                     float64
	// BuildID, Job and Number are the build that set the value.
	BuildID int64
	Job     string
	Number  int
	// Status is how that build ended, empty once retention has removed it.
	Status Status
	At     time.Time
}

// HistoryRow is one value a build reported.
type HistoryRow struct {
	BuildID                    int64
	Job                        string
	Number                     int
	Status                     Status
	Store, Stage, Metric, Item string
	Unit                       string
	Value                      float64
	// Seq orders the rows of one build, which a page can end between.
	Seq int64
	// At is when the build ended, or for one that has not, when it
	// started or was queued: history keeps no time of its own.
	At time.Time
}

// MetricCount is how many items a store remembers of a metric.
type MetricCount struct {
	Name  string
	Items int
}

// MetricStore is a store and what it remembers, by metric.
type MetricStore struct {
	Store   string
	Metrics []MetricCount
}

// PutMetricHistory keeps what a stage of the build reported into the
// store, whether or not the store remembers it.
func (s *Store) PutMetricHistory(ctx context.Context, buildID int64, store, stage string, ms []Metric) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		ins, err := tx.PrepareContext(ctx, `INSERT INTO metric_history (build_id, store, stage, metric, item, value, unit) VALUES (?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer func() { _ = ins.Close() }()
		for _, m := range ms {
			for _, item := range slices.Sorted(maps.Keys(m.Values)) {
				if _, err := ins.ExecContext(ctx, buildID, store, stage, m.Name, item, m.Values[item], m.Unit); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// Remember updates the store with the metrics, in one transaction: a
// complete metric replaces every item the store had of it, any other
// merges, item by item.
func (s *Store) Remember(ctx context.Context, store string, from Origin, at time.Time, ms []Metric) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		ins, err := tx.PrepareContext(ctx, `INSERT INTO metric_values (store, metric, item, value, unit, build_id, job, number, at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (store, metric, item) DO UPDATE SET value = excluded.value, unit = excluded.unit,
			build_id = excluded.build_id, job = excluded.job, number = excluded.number, at = excluded.at`)
		if err != nil {
			return err
		}
		defer func() { _ = ins.Close() }()
		for _, m := range ms {
			if err := rememberMetric(ctx, tx, ins, store, from, at, m); err != nil {
				return err
			}
		}
		return nil
	})
}

// rememberMetric is Remember's work for one metric, with ins its upsert.
func rememberMetric(ctx context.Context, tx *sql.Tx, ins *sql.Stmt, store string, from Origin, at time.Time, m Metric) error {
	if m.Complete {
		if _, err := tx.ExecContext(ctx, `DELETE FROM metric_values WHERE store = ? AND metric = ?`, store, m.Name); err != nil {
			return err
		}
	}
	for _, item := range slices.Sorted(maps.Keys(m.Values)) {
		if _, err := ins.ExecContext(ctx, store, m.Name, item, m.Values[item], m.Unit, from.BuildID, from.Job, from.Number, ts(at)); err != nil {
			return err
		}
	}
	return nil
}

// RememberedValues are what the store remembers of the metric, or of
// every metric when it is empty, by metric and item.
func (s *Store) RememberedValues(ctx context.Context, store, metric string) ([]Remembered, error) {
	return collect(ctx, s.db, func(row scanner) (r Remembered, err error) {
		var at string
		if err := row.Scan(&r.Store, &r.Metric, &r.Item, &r.Value, &r.Unit, &r.BuildID, &r.Job, &r.Number, &r.Status, &at); err != nil {
			return r, err
		}
		r.At, err = parseSetTS(at)
		return r, err
	}, `SELECT v.store, v.metric, v.item, v.value, v.unit, v.build_id, v.job, v.number, COALESCE(b.status, ''), v.at FROM metric_values v
		LEFT JOIN builds b ON b.job = v.job AND b.number = v.number
		WHERE v.store = ? AND (? = '' OR v.metric = ?) ORDER BY v.metric, v.item`, store, metric, metric)
}

const historyCols = `h.build_id, b.job, b.number, b.status, h.store, h.stage, h.metric, h.item, h.unit, h.value,
	COALESCE(b.finished_at, b.started_at, b.queued_at), h.rowid`

func scanHistory(row scanner) (r HistoryRow, err error) {
	var at string
	if err := row.Scan(&r.BuildID, &r.Job, &r.Number, &r.Status, &r.Store, &r.Stage, &r.Metric, &r.Item, &r.Unit, &r.Value, &at, &r.Seq); err != nil {
		return r, err
	}
	r.At, err = parseSetTS(at)
	return r, err
}

// MetricHistory is what the builds reported of one item into the store,
// newest build first, from the row before the one at seq of the build with
// id beforeBuild (a beforeBuild of 0 is from the newest; a seq of 0 is from
// the build before). A build that reported an item in more than one stage
// is not cut in two by a page that ends on the position of its last row.
func (s *Store) MetricHistory(ctx context.Context, store, metric, item string, beforeBuild, beforeSeq int64, limit int) ([]HistoryRow, error) {
	return collect(ctx, s.db, scanHistory, `SELECT `+historyCols+` FROM metric_history h JOIN builds b ON b.id = h.build_id
		WHERE h.store = ? AND h.metric = ? AND h.item = ?
		AND (? = 0 OR h.build_id < ? OR (h.build_id = ? AND h.rowid < ?))
		ORDER BY h.build_id DESC, h.rowid DESC LIMIT ?`, store, metric, item, beforeBuild, beforeBuild, beforeBuild, beforeSeq, limit)
}

// BuildMetrics is everything the build reported, in the order it was kept.
func (s *Store) BuildMetrics(ctx context.Context, buildID int64) ([]HistoryRow, error) {
	return collect(ctx, s.db, scanHistory, `SELECT `+historyCols+` FROM metric_history h JOIN builds b ON b.id = h.build_id
		WHERE h.build_id = ? ORDER BY h.rowid`, buildID)
}

// BuildMetric is what the build reported of one metric into the store, in
// the order it was kept: of an item reported twice, the later row is the
// one that counts, as when the stages' reports merge.
func (s *Store) BuildMetric(ctx context.Context, buildID int64, store, metric string) ([]HistoryRow, error) {
	return collect(ctx, s.db, scanHistory, `SELECT `+historyCols+` FROM metric_history h JOIN builds b ON b.id = h.build_id
		WHERE h.build_id = ? AND h.store = ? AND h.metric = ? ORDER BY h.rowid`, buildID, store, metric)
}

// RecentValues are the last n values each item of the metric was reported
// with into the store, by item, oldest first: what a sparkline draws.
func (s *Store) RecentValues(ctx context.Context, store, metric string, n int) (map[string][]float64, error) {
	type row struct {
		item  string
		value float64
	}
	rows, err := collect(ctx, s.db, func(sc scanner) (r row, err error) {
		return r, sc.Scan(&r.item, &r.value)
	}, `SELECT item, value FROM (
		SELECT item, value, build_id, rowid AS seq,
		ROW_NUMBER() OVER (PARTITION BY item ORDER BY build_id DESC, rowid DESC) AS age
		FROM metric_history WHERE store = ? AND metric = ?)
		WHERE age <= ? ORDER BY item, build_id, seq`, store, metric, n)
	out := map[string][]float64{}
	for _, r := range rows {
		out[r.item] = append(out[r.item], r.value)
	}
	return out, err
}

// MetricBuild is a build that reported into a store.
type MetricBuild struct {
	ID     int64
	Job    string
	Number int
	Status Status
	At     time.Time
}

// MetricJobs are the jobs whose builds reported into the store, by name.
func (s *Store) MetricJobs(ctx context.Context, store string) ([]string, error) {
	return collect(ctx, s.db, func(row scanner) (job string, err error) {
		return job, row.Scan(&job)
	}, `SELECT DISTINCT b.job FROM builds b WHERE EXISTS
		(SELECT 1 FROM metric_history h WHERE h.build_id = b.id AND h.store = ?) ORDER BY b.job`, store)
}

// MetricBuilds are the builds that reported into the store, of the jobs
// named or of every job when jobs is nil, newest first, limit at most.
func (s *Store) MetricBuilds(ctx context.Context, store string, jobs []string, limit int) ([]MetricBuild, error) {
	if jobs != nil && len(jobs) == 0 {
		return nil, nil
	}
	named, err := json.Marshal(jobs)
	if err != nil {
		return nil, err
	}
	return collect(ctx, s.db, func(row scanner) (b MetricBuild, err error) {
		var at string
		if err := row.Scan(&b.ID, &b.Job, &b.Number, &b.Status, &at); err != nil {
			return b, err
		}
		b.At, err = parseSetTS(at)
		return b, err
	}, `SELECT b.id, b.job, b.number, b.status, COALESCE(b.finished_at, b.started_at, b.queued_at) FROM builds b
		WHERE (? = 'null' OR b.job IN (SELECT value FROM json_each(?)))
		AND EXISTS (SELECT 1 FROM metric_history h WHERE h.build_id = b.id AND h.store = ?)
		ORDER BY b.id DESC LIMIT ?`, string(named), string(named), store, limit)
}

// MetricStoreKnown reports whether the store remembers anything or any
// build reported into it.
func (s *Store) MetricStoreKnown(ctx context.Context, store string) (bool, error) {
	var known bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM metric_values WHERE store = ?)
		OR EXISTS (SELECT 1 FROM metric_history WHERE store = ?)`, store, store).Scan(&known)
	return known, err
}

// Forget removes what the store remembers of an item, of every item of a
// metric when item is empty, or of everything when metric is empty too,
// and says how many items went.
func (s *Store) Forget(ctx context.Context, store, metric, item string) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM metric_values WHERE store = ? AND (? = '' OR metric = ?) AND (? = '' OR item = ?)`,
		store, metric, metric, item, item)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// MetricStores are the stores that remember anything, by name, each with
// its metrics and their item counts.
func (s *Store) MetricStores(ctx context.Context) ([]MetricStore, error) {
	type row struct {
		store string
		MetricCount
	}
	rows, err := collect(ctx, s.db, func(sc scanner) (r row, err error) {
		return r, sc.Scan(&r.store, &r.Name, &r.Items)
	}, `SELECT store, metric, COUNT(*) FROM metric_values GROUP BY store, metric ORDER BY store, metric`)
	if err != nil {
		return nil, err
	}
	var out []MetricStore
	for _, r := range rows {
		if len(out) == 0 || out[len(out)-1].Store != r.store {
			out = append(out, MetricStore{Store: r.store})
		}
		last := &out[len(out)-1]
		last.Metrics = append(last.Metrics, r.MetricCount)
	}
	return out, nil
}

// GuardRow is what a guard of a stage found of one item.
type GuardRow struct {
	Stage, Metric, Item string
	// Remembered and Reported are the two values, nil where there is none.
	Remembered, Reported *float64
	Tolerance            float64
	// Verdict is held, no baseline, fell, missing or not reported;
	// Outcome how it ended the stage: held, failed or unstable.
	Verdict, Outcome string
	// FromJob and FromNumber are the build that set Remembered. FromBuildID
	// and FromStatus are what the builds table says of it now: 0 and empty
	// once retention has removed the build, which a guard's result outlives.
	FromJob     string
	FromNumber  int
	FromBuildID int64
	FromStatus  Status
}

// PutGuardResults keeps what the guards of a stage of the build found.
func (s *Store) PutGuardResults(ctx context.Context, buildID int64, rows []GuardRow) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, r := range rows {
			if _, err := tx.ExecContext(ctx, `INSERT INTO metric_guards (build_id, stage, metric, item, remembered, reported, tolerance, verdict, outcome, from_job, from_number)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				buildID, r.Stage, r.Metric, r.Item, r.Remembered, r.Reported, r.Tolerance, r.Verdict, r.Outcome, r.FromJob, r.FromNumber); err != nil {
				return err
			}
		}
		return nil
	})
}

// GuardResults are what the guards of the build found, in the order they
// were kept.
func (s *Store) GuardResults(ctx context.Context, buildID int64) ([]GuardRow, error) {
	return collect(ctx, s.db, func(row scanner) (r GuardRow, err error) {
		return r, row.Scan(&r.Stage, &r.Metric, &r.Item, &r.Remembered, &r.Reported, &r.Tolerance, &r.Verdict, &r.Outcome, &r.FromJob, &r.FromNumber, &r.FromBuildID, &r.FromStatus)
	}, `SELECT g.stage, g.metric, g.item, g.remembered, g.reported, g.tolerance, g.verdict, g.outcome, g.from_job, g.from_number,
		COALESCE(b.id, 0), COALESCE(b.status, '')
		FROM metric_guards g LEFT JOIN builds b ON b.job = g.from_job AND b.number = g.from_number
		WHERE g.build_id = ? ORDER BY g.rowid`, buildID)
}
