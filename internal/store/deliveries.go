package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ErrSeen is a webhook delivery whose id was recorded before: a replay.
var ErrSeen = errors.New("delivery seen before")

// RecordDelivery records a webhook delivery's id; one already recorded is
// ErrSeen and changes nothing.
func (s *Store) RecordDelivery(ctx context.Context, id, job string, at time.Time) error {
	err := requireChange(s.db.ExecContext(ctx, `INSERT INTO webhook_deliveries (id, job, at) VALUES (?, ?, ?) ON CONFLICT(id) DO NOTHING`, id, job, ts(at)))
	if errors.Is(err, ErrNotFound) {
		return ErrSeen
	}
	return err
}

// PruneDeliveries forgets the deliveries recorded before olderThan.
func (s *Store) PruneDeliveries(ctx context.Context, olderThan time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM webhook_deliveries WHERE at < ?`, ts(olderThan))
	return err
}

// PendingDelivery is a webhook delivery that arrived while the server was
// quiet: verified and recorded, waiting to be handled as if it had just
// arrived. Route is which route it came in by and Name the job or App it
// was addressed to.
type PendingDelivery struct {
	Seq         int64
	Route, Name string
	Event, ID   string
	Body        []byte
	ReceivedAt  time.Time
}

// KeepDelivery records the ids of a verified delivery and keeps it as
// pending, in one step, so that it is never recorded without being kept. An
// id recorded before is ErrSeen and nothing is kept.
func (s *Store) KeepDelivery(ctx context.Context, p PendingDelivery, ids []string, job string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			res, err := tx.ExecContext(ctx, `INSERT INTO webhook_deliveries (id, job, at) VALUES (?, ?, ?) ON CONFLICT(id) DO NOTHING`, id, job, ts(p.ReceivedAt))
			if err := requireChange(res, err); err != nil {
				if errors.Is(err, ErrNotFound) {
					return ErrSeen
				}
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO pending_deliveries (route, name, event, delivery, body, received_at) VALUES (?, ?, ?, ?, ?, ?)`,
			p.Route, p.Name, p.Event, p.ID, p.Body, ts(p.ReceivedAt))
		return err
	})
}

// PendingDeliveries are the kept deliveries, the oldest first.
func (s *Store) PendingDeliveries(ctx context.Context) ([]PendingDelivery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, route, name, event, delivery, body, received_at FROM pending_deliveries ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []PendingDelivery
	for rows.Next() {
		var p PendingDelivery
		var at string
		if err := rows.Scan(&p.Seq, &p.Route, &p.Name, &p.Event, &p.ID, &p.Body, &at); err != nil {
			return nil, err
		}
		if p.ReceivedAt, err = parseSetTS(at); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePendingDelivery forgets a kept delivery once it was handled.
func (s *Store) DeletePendingDelivery(ctx context.Context, seq int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM pending_deliveries WHERE seq = ?`, seq)
	return err
}
