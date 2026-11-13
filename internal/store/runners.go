package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Runner is a standing runner: registered by an admin with its labels and a
// token, run by whoever runs its machine. Only the token's hash is kept.
type Runner struct {
	Name       string
	Labels     []string
	Executors  int
	Enabled    bool
	TokenHash  string
	CreatedAt  time.Time
	LastSeenAt *time.Time
	// What the runner said of itself when it last connected.
	OS, Arch, Version string
}

const runnerCols = `name, labels, executors, enabled, token_hash, created_at, last_seen_at, os, arch, version`

// PutRunner adds the runner, or replaces the one of that name and keeps
// when it was added and what was seen of it.
func (s *Store) PutRunner(ctx context.Context, a Runner) error {
	labels, err := labelsJSON(a.Labels)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO runners (name, labels, executors, enabled, token_hash, created_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET labels = excluded.labels, executors = excluded.executors, enabled = excluded.enabled,
		token_hash = excluded.token_hash`,
		a.Name, labels, a.Executors, a.Enabled, a.TokenHash, ts(time.Now()))
	return err
}

// Runner by name.
func (s *Store) Runner(ctx context.Context, name string) (*Runner, error) {
	a, err := scanRunner(s.db.QueryRowContext(ctx, `SELECT `+runnerCols+` FROM runners WHERE name = ?`, name))
	return a, notFound(err)
}

// RunnerByTokenHash is the runner whose token hashes to hash.
func (s *Store) RunnerByTokenHash(ctx context.Context, hash string) (*Runner, error) {
	a, err := scanRunner(s.db.QueryRowContext(ctx, `SELECT `+runnerCols+` FROM runners WHERE token_hash = ?`, hash))
	return a, notFound(err)
}

// Runners by name.
func (s *Store) Runners(ctx context.Context) ([]*Runner, error) {
	return collect(ctx, s.db, scanRunner, `SELECT `+runnerCols+` FROM runners ORDER BY name`)
}

// DeleteRunner removes one.
func (s *Store) DeleteRunner(ctx context.Context, name string) error {
	return requireChange(s.db.ExecContext(ctx, `DELETE FROM runners WHERE name = ?`, name))
}

// SetRunnerSeen records that the runner connected at at, and what it said
// of itself.
func (s *Store) SetRunnerSeen(ctx context.Context, name string, at time.Time, os, arch, version string) error {
	return requireChange(s.db.ExecContext(ctx, `UPDATE runners SET last_seen_at = ?, os = ?, arch = ?, version = ? WHERE name = ?`,
		ts(at), os, arch, version, name))
}

func scanRunner(row scanner) (*Runner, error) {
	var a Runner
	var labels, created string
	var seen sql.NullString
	if err := row.Scan(&a.Name, &labels, &a.Executors, &a.Enabled, &a.TokenHash, &created, &seen, &a.OS, &a.Arch, &a.Version); err != nil {
		return nil, err
	}
	var err error
	if a.Labels, err = parseLabels(labels); err != nil {
		return nil, err
	}
	if a.CreatedAt, err = parseSetTS(created); err != nil {
		return nil, err
	}
	a.LastSeenAt, err = parseTS(seen)
	return &a, err
}

// BuiltInExecutors is the built-in runner's limit as an admin last set it;
// ok is false while none has, and the server's --executors applies. It is
// a table of its own, not a row of runners, so no runner's name is taken
// and no token is asked of it.
func (s *Store) BuiltInExecutors(ctx context.Context) (n int, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT executors FROM builtin_runner WHERE id = 1`).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return n, err == nil, err
}

// SetBuiltInExecutors keeps the built-in runner's limit.
func (s *Store) SetBuiltInExecutors(ctx context.Context, n int) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO builtin_runner (id, executors) VALUES (1, ?)
		ON CONFLICT(id) DO UPDATE SET executors = excluded.executors`, n)
	return err
}
