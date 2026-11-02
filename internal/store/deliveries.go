package store

import (
	"context"
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
