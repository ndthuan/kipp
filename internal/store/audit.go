package store

import (
	"context"
	"math"
	"time"
)

// Audit is one change to accounts, credentials, secrets or the loaded
// jobs: who made it, what it was, and on what.
type Audit struct {
	ID     int64
	At     time.Time
	Actor  string // a user's name, or "cli"
	Action string
	Target string
	Detail string
}

// AppendAudit records a change; the program never edits or removes a row
// but by PruneAudit.
func (s *Store) AppendAudit(ctx context.Context, a Audit) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit (at, actor, action, target, detail) VALUES (?, ?, ?, ?, ?)`,
		ts(a.At), a.Actor, a.Action, a.Target, a.Detail)
	return err
}

// AuditBefore is up to limit rows older than the row beforeID, newest
// first; a beforeID of 0 starts at the newest.
func (s *Store) AuditBefore(ctx context.Context, beforeID int64, limit int) ([]*Audit, error) {
	if beforeID <= 0 {
		beforeID = math.MaxInt64
	}
	return collect(ctx, s.db, scanAudit, `SELECT id, at, actor, action, target, detail FROM audit WHERE id < ? ORDER BY id DESC LIMIT ?`, beforeID, limit)
}

func scanAudit(row scanner) (*Audit, error) {
	var a Audit
	var at string
	if err := row.Scan(&a.ID, &at, &a.Actor, &a.Action, &a.Target, &a.Detail); err != nil {
		return nil, err
	}
	var err error
	a.At, err = parseSetTS(at)
	return &a, err
}

// PruneAudit keeps the newest keep rows and deletes the rest.
func (s *Store) PruneAudit(ctx context.Context, keep int) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM audit WHERE id NOT IN (SELECT id FROM audit ORDER BY id DESC LIMIT ?)`, keep)
	return err
}
