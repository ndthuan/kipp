package store

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
)

// AutoVacuum is SQLite's auto_vacuum of the file.
type AutoVacuum int

// The values of PRAGMA auto_vacuum.
const (
	AutoVacuumNone        AutoVacuum = 0
	AutoVacuumFull        AutoVacuum = 1
	AutoVacuumIncremental AutoVacuum = 2
)

func (a AutoVacuum) String() string {
	switch a {
	case AutoVacuumFull:
		return "full"
	case AutoVacuumIncremental:
		return "incremental"
	}
	return "none"
}

// DBFile is the database file as SQLite counts it.
type DBFile struct {
	PageSize, Pages, FreePages int64
	AutoVacuum                 AutoVacuum
}

// Bytes is the file's pages, free ones included.
func (f DBFile) Bytes() int64 { return f.PageSize * f.Pages }

// FreeBytes is the pages that hold nothing, which a compact gives back.
func (f DBFile) FreeBytes() int64 { return f.PageSize * f.FreePages }

// UsedBytes is the pages that hold something.
func (f DBFile) UsedBytes() int64 { return f.Bytes() - f.FreeBytes() }

// DBFile reads the pages of the database. The pragmas read the header only.
func (s *Store) DBFile(ctx context.Context) (DBFile, error) {
	var f DBFile
	var av int
	for _, p := range []struct {
		pragma string
		into   any
	}{{"page_size", &f.PageSize}, {"page_count", &f.Pages}, {"freelist_count", &f.FreePages}, {"auto_vacuum", &av}} {
		if err := s.db.QueryRowContext(ctx, `PRAGMA `+p.pragma).Scan(p.into); err != nil {
			return DBFile{}, fmt.Errorf("PRAGMA %s: %w", p.pragma, err)
		}
	}
	f.AutoVacuum = AutoVacuum(av)
	return f, nil
}

// reader is the read-only connection, opened the first time it is asked
// for. WAL lets it read while the writer writes.
func (s *Store) reader() (*sql.DB, error) {
	s.roMu.Lock()
	defer s.roMu.Unlock()
	if s.ro != nil {
		return s.ro, nil
	}
	db, err := sql.Open("sqlite", "file:"+s.path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s.ro = db
	return db, nil
}

// DBObject is a table or an index and the room it takes in the file.
type DBObject struct {
	// Name is the table's or the index's; Table is the table an index
	// belongs to, and the table's own name for a table.
	Name, Table string
	Index       bool
	Bytes       int64
}

// DBObjects are the tables and indexes, the largest first. Exact says they
// were measured page by page (SQLite's dbstat); otherwise each table's
// bytes are an estimate from its share of the rows, and indexes are counted
// in their table's.
type DBObjects struct {
	Objects []DBObject
	Exact   bool
}

// MeasureObjects measures every table and index. On a large file it reads
// the whole file, so it runs on the read-only connection.
func (s *Store) MeasureObjects(ctx context.Context) (DBObjects, error) {
	ro, err := s.reader()
	if err != nil {
		return DBObjects{}, err
	}
	objs, err := collect(ctx, ro, func(r scanner) (DBObject, error) {
		var o DBObject
		var kind string
		err := r.Scan(&o.Name, &o.Table, &kind, &o.Bytes)
		o.Index = kind == "index"
		return o, err
	}, `SELECT d.name, COALESCE(m.tbl_name, d.name), COALESCE(m.type, 'table'), SUM(d.pgsize)
		FROM dbstat d LEFT JOIN sqlite_master m ON m.name = d.name
		GROUP BY d.name ORDER BY SUM(d.pgsize) DESC, d.name`)
	if err == nil {
		return DBObjects{Objects: objs, Exact: true}, nil
	}
	if !strings.Contains(err.Error(), "dbstat") {
		return DBObjects{}, err
	}
	f, ferr := s.DBFile(ctx)
	if ferr != nil {
		return DBObjects{}, ferr
	}
	return estimateObjects(ctx, ro, f.UsedBytes())
}

// estimateObjects shares used bytes out among the tables by their rows,
// for an SQLite built without dbstat.
func estimateObjects(ctx context.Context, q *sql.DB, used int64) (DBObjects, error) {
	names, err := collect(ctx, q, scanOne[string], `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return DBObjects{}, err
	}
	rows := make([]int64, len(names))
	var total int64
	for i, name := range names {
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM "`+strings.ReplaceAll(name, `"`, `""`)+`"`).Scan(&rows[i]); err != nil {
			return DBObjects{}, err
		}
		total += rows[i]
	}
	out := DBObjects{Objects: make([]DBObject, 0, len(names))}
	for i, name := range names {
		var b int64
		if total > 0 {
			b = int64(float64(used) * float64(rows[i]) / float64(total))
		}
		out.Objects = append(out.Objects, DBObject{Name: name, Table: name, Bytes: b})
	}
	sortObjects(out.Objects)
	return out, nil
}

func sortObjects(objs []DBObject) {
	slices.SortFunc(objs, func(a, b DBObject) int {
		return cmp.Or(cmp.Compare(b.Bytes, a.Bytes), cmp.Compare(a.Name, b.Name))
	})
}

// JobRows is what one job holds in the database.
type JobRows struct {
	Job string
	// Builds is its builds, Kept those kept by hand among them.
	Builds, Kept int
	// Tests and Metrics are its rows of test results and metric history.
	Tests, Metrics int64
	// Newest is when its newest build was queued.
	Newest time.Time
}

// JobRows counts, per job that has a build, what it holds. It runs on the
// read-only connection: on a large file the counts take a moment.
func (s *Store) JobRows(ctx context.Context) ([]JobRows, error) {
	ro, err := s.reader()
	if err != nil {
		return nil, err
	}
	out, err := collect(ctx, ro, func(r scanner) (JobRows, error) {
		var j JobRows
		var newest string
		err := r.Scan(&j.Job, &j.Builds, &j.Kept, &newest)
		if err == nil {
			j.Newest, err = parseSetTS(newest)
		}
		return j, err
	}, `SELECT job, COUNT(*), COUNT(kept_at), MAX(queued_at) FROM builds GROUP BY job ORDER BY job`)
	if err != nil {
		return nil, err
	}
	tests, err := countByJob(ctx, ro, `SELECT b.job, COUNT(*) FROM tests t JOIN builds b ON b.id = t.build_id GROUP BY b.job`)
	if err != nil {
		return nil, err
	}
	metrics, err := countByJob(ctx, ro, `SELECT b.job, COUNT(*) FROM metric_history t JOIN builds b ON b.id = t.build_id GROUP BY b.job`)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Tests, out[i].Metrics = tests[out[i].Job], metrics[out[i].Job]
	}
	return out, nil
}

// countByJob runs a query of (job, count) rows into a map.
func countByJob(ctx context.Context, q querier, query string) (map[string]int64, error) {
	type count struct {
		job string
		n   int64
	}
	rows, err := collect(ctx, q, func(r scanner) (count, error) {
		var c count
		err := r.Scan(&c.job, &c.n)
		return c, err
	}, query)
	out := make(map[string]int64, len(rows))
	for _, c := range rows {
		out[c.job] = c.n
	}
	return out, err
}

// EndedBuilds are the job's builds that have ended and are not kept by
// hand, the oldest first: what removing a job's data removes.
func (s *Store) EndedBuilds(ctx context.Context, job string) ([]int64, error) {
	return collect(ctx, s.db, scanOne[int64], `SELECT id FROM builds WHERE job = ? AND status NOT IN ('queued','running') AND kept_at IS NULL ORDER BY id`, job)
}

// CapItemHistory removes the metric's history beyond the newest keep
// builds of each item, per job, and is how many rows it removed. It works a
// job at a time, so the writer is let go between jobs.
func (s *Store) CapItemHistory(ctx context.Context, metric string, keep int) (int64, error) {
	jobs, err := collect(ctx, s.db, scanOne[string], `SELECT DISTINCT b.job FROM metric_history h JOIN builds b ON b.id = h.build_id WHERE h.metric = ? ORDER BY b.job`, metric)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, job := range jobs {
		res, err := s.db.ExecContext(ctx, `DELETE FROM metric_history WHERE rowid IN (
			SELECT rid FROM (
				SELECT h.rowid AS rid, DENSE_RANK() OVER (PARTITION BY h.store, h.item ORDER BY h.build_id DESC) AS rank
				FROM metric_history h JOIN builds b ON b.id = h.build_id
				WHERE h.metric = ? AND b.job = ?)
			WHERE rank > ?)`, metric, job, keep)
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// IncrementalVacuum gives the file's free pages back to the disk, and
// reports whether it could: only a file whose auto_vacuum is incremental
// can, which a compact turns on.
func (s *Store) IncrementalVacuum(ctx context.Context) (bool, error) {
	var av int
	if err := s.db.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&av); err != nil {
		return false, err
	}
	if AutoVacuum(av) != AutoVacuumIncremental {
		return false, nil
	}
	_, err := s.db.ExecContext(ctx, `PRAGMA incremental_vacuum`)
	return err == nil, err
}

// QuickCheck runs PRAGMA quick_check on the read-only connection and
// returns what it found wrong, nothing for a sound file.
func (s *Store) QuickCheck(ctx context.Context) ([]string, error) {
	ro, err := s.reader()
	if err != nil {
		return nil, err
	}
	lines, err := collect(ctx, ro, scanOne[string], `PRAGMA quick_check`)
	if err != nil {
		return nil, err
	}
	if len(lines) == 1 && lines[0] == "ok" {
		return nil, nil
	}
	return lines, nil
}

// Compacted is what a compact did.
type Compacted struct {
	// Before and After are the bytes of the file and its write-ahead log.
	Before, After int64
	// Incremental says this compact turned incremental auto_vacuum on.
	Incremental bool
}

// Compact rewrites the file without its free pages (VACUUM), turning
// incremental auto_vacuum on the first time, then lets SQLite refresh its
// statistics and folds the write-ahead log back into the file. It runs on a
// connection of its own, so the server's connection still reads; a write
// meanwhile waits up to the busy timeout and fails, which is why the server
// is quiet first. It needs room on disk for a copy of the database in the
// temporary directory and another in the write-ahead log.
func (s *Store) Compact(ctx context.Context) (Compacted, error) {
	var out Compacted
	out.Before = s.FileBytes()
	db, err := sql.Open("sqlite", writerDSN(s.path))
	if err != nil {
		return out, err
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	var av int
	if err := db.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&av); err != nil {
		return out, err
	}
	if AutoVacuum(av) != AutoVacuumIncremental {
		// Set before VACUUM, which is what applies it to a file that has tables.
		if _, err := db.ExecContext(ctx, `PRAGMA auto_vacuum = INCREMENTAL`); err != nil {
			return out, err
		}
		out.Incremental = true
	}
	for _, q := range []string{`VACUUM`, `PRAGMA optimize`, `PRAGMA wal_checkpoint(TRUNCATE)`} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return out, fmt.Errorf("%s: %w", q, err)
		}
	}
	if err := db.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&av); err != nil {
		return out, err
	}
	if AutoVacuum(av) != AutoVacuumIncremental {
		out.Incremental = false
	}
	out.After = s.FileBytes()
	return out, nil
}

// FileBytes is the size of the database file and its write-ahead log on
// disk, what the file system sees.
func (s *Store) FileBytes() int64 {
	var n int64
	for _, p := range []string{s.path, s.path + "-wal"} {
		if fi, err := os.Stat(p); err == nil {
			n += fi.Size()
		}
	}
	return n
}

// Path is the database file.
func (s *Store) Path() string { return s.path }
