package store

import "context"

// KeepCron records that job's cron fired while the server was quiet, so
// that it is triggered once when quiet ends. Recording it twice changes
// nothing.
func (s *Store) KeepCron(ctx context.Context, job string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO pending_crons (job) VALUES (?) ON CONFLICT DO NOTHING`, job)
	return err
}

// PendingCrons are the jobs whose cron fire is kept, in the order they were
// first kept.
func (s *Store) PendingCrons(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT job FROM pending_crons ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var job string
		if err := rows.Scan(&job); err != nil {
			return nil, err
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

// DropCron forgets a kept cron fire.
func (s *Store) DropCron(ctx context.Context, job string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM pending_crons WHERE job = ?`, job)
	return err
}
