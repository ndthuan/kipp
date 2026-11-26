package store

import (
	"context"
	"database/sql"
)

// KeepRerun records that the build was stopped for a restart and is to be
// queued again after the next start. Recording it twice changes nothing.
func (s *Store) KeepRerun(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO pending_reruns (build_id) VALUES (?) ON CONFLICT(build_id) DO NOTHING`, id)
	return err
}

// PendingReruns are the builds stopped for a restart and not queued again
// yet, in the order they were queued first.
func (s *Store) PendingReruns(ctx context.Context) ([]*Build, error) {
	return s.queryBuilds(ctx, `WHERE id IN (SELECT build_id FROM pending_reruns) ORDER BY id`)
}

// LinkRerun records that number, a build of the same job, runs the stopped
// build id again, and that id waits no more.
func (s *Store) LinkRerun(ctx context.Context, id int64, number int) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := requireChange(tx.ExecContext(ctx, `UPDATE builds SET rerun_as = ? WHERE id = ?`, number, id)); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM pending_reruns WHERE build_id = ?`, id)
		return err
	})
}

// DropRerun forgets a stopped build that could not be queued again.
func (s *Store) DropRerun(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM pending_reruns WHERE build_id = ?`, id)
	return err
}
