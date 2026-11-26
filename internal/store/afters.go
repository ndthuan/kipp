package store

import "context"

// PendingAfter is an after trigger refused while the server was quiet: Job
// is to run after Upstream, a build that has ended.
type PendingAfter struct {
	Upstream *Build
	Job      string
}

// KeepAfter records that job is to be triggered after the build id once
// the server is no longer quiet. Recording it twice changes nothing.
func (s *Store) KeepAfter(ctx context.Context, id int64, job string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO pending_afters (build_id, job) VALUES (?, ?) ON CONFLICT DO NOTHING`, id, job)
	return err
}

// PendingAfters are the kept after triggers, in the order they were kept.
func (s *Store) PendingAfters(ctx context.Context) ([]PendingAfter, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT build_id, job FROM pending_afters ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	type kept struct {
		id  int64
		job string
	}
	var all []kept
	for rows.Next() {
		var k kept
		if err := rows.Scan(&k.id, &k.job); err != nil {
			_ = rows.Close()
			return nil, err
		}
		all = append(all, k)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]PendingAfter, 0, len(all))
	for _, k := range all {
		b, err := s.Build(ctx, k.id)
		if err != nil {
			return nil, err
		}
		out = append(out, PendingAfter{Upstream: b, Job: k.job})
	}
	return out, nil
}

// DropAfter forgets a kept after trigger.
func (s *Store) DropAfter(ctx context.Context, id int64, job string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM pending_afters WHERE build_id = ? AND job = ?`, id, job)
	return err
}
