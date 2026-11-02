package store

import (
	"context"
	"database/sql"
	"math"
	"time"
)

// Test is one JUnit case of a build.
type Test struct {
	ID                         int64  // set when read; a page begins after one
	Stage, Suite, Name, Status string // passed, failed, skipped
	DurationMs                 int
	Message                    string
}

// TestRow is a case's outcome in one build, for the history view.
type TestRow struct {
	BuildNumber         int
	Suite, Name, Status string
}

// Activity is one line of a job's log of events that are not builds.
type Activity struct {
	ID   int64 // a page of older lines begins before one
	At   time.Time
	Line string
}

// PutTests stores a build's cases.
func (s *Store) PutTests(ctx context.Context, buildID int64, tests []Test) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, t := range tests {
			if _, err := tx.ExecContext(ctx, `INSERT INTO tests (build_id, stage, suite, name, status, duration_ms, message) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				buildID, t.Stage, t.Suite, t.Name, t.Status, t.DurationMs, t.Message); err != nil {
				return err
			}
		}
		return nil
	})
}

// Tests of a build, in the order stored.
func (s *Store) Tests(ctx context.Context, buildID int64) ([]Test, error) {
	return s.TestsAfter(ctx, buildID, 0, -1)
}

// TestsAfter is up to limit of a build's cases stored after the case with
// id after (0 starts at the first), in the order stored; a limit below 0 is
// all of them.
func (s *Store) TestsAfter(ctx context.Context, buildID, after int64, limit int) ([]Test, error) {
	return collect(ctx, s.db, func(row scanner) (t Test, err error) {
		return t, row.Scan(&t.ID, &t.Stage, &t.Suite, &t.Name, &t.Status, &t.DurationMs, &t.Message)
	}, `SELECT id, stage, suite, name, status, duration_ms, message FROM tests WHERE build_id = ? AND id > ? ORDER BY id LIMIT ?`,
		buildID, after, limit)
}

// TestHistory is every case of the job's last n builds, newest build first.
func (s *Store) TestHistory(ctx context.Context, job string, builds int) ([]TestRow, error) {
	return collect(ctx, s.db, func(row scanner) (r TestRow, err error) {
		return r, row.Scan(&r.BuildNumber, &r.Suite, &r.Name, &r.Status)
	}, `SELECT b.number, t.suite, t.name, t.status FROM tests t JOIN builds b ON b.id = t.build_id
		WHERE b.id IN (SELECT id FROM builds WHERE job = ? ORDER BY id DESC LIMIT ?) ORDER BY b.id DESC, t.id`, job, builds)
}

// AddActivity appends a line to a job's activity.
func (s *Store) AddActivity(ctx context.Context, job, line string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO job_activity (job, at, line) VALUES (?, ?, ?)`, job, ts(at), line)
	return err
}

// Activity of a job, newest first.
func (s *Store) Activity(ctx context.Context, job string, limit int) ([]Activity, error) {
	return s.ActivityBefore(ctx, job, 0, limit)
}

// ActivityBefore is up to limit lines of a job's activity older than the
// line with id before, newest first; a before of 0 starts at the newest.
func (s *Store) ActivityBefore(ctx context.Context, job string, before int64, limit int) ([]Activity, error) {
	if before <= 0 {
		before = math.MaxInt64
	}
	return collect(ctx, s.db, func(row scanner) (a Activity, err error) {
		var at string
		if err := row.Scan(&a.ID, &at, &a.Line); err != nil {
			return a, err
		}
		a.At, err = parseSetTS(at)
		return a, err
	}, `SELECT id, at, line FROM job_activity WHERE job = ? AND id < ? ORDER BY id DESC LIMIT ?`, job, before, limit)
}
