package store

import (
	"context"
	"database/sql"
	"time"
)

// Agent is a standing agent: registered by an admin with its labels and a
// token, run by whoever runs its machine. Only the token's hash is kept.
type Agent struct {
	Name       string
	Labels     []string
	Executors  int
	Enabled    bool
	TokenHash  string
	CreatedAt  time.Time
	LastSeenAt *time.Time
	// What the agent said of itself when it last connected.
	OS, Arch, Version string
}

const agentCols = `name, labels, executors, enabled, token_hash, created_at, last_seen_at, os, arch, version`

// PutAgent adds the agent, or replaces the one of that name and keeps
// when it was added and what was seen of it.
func (s *Store) PutAgent(ctx context.Context, a Agent) error {
	labels, err := labelsJSON(a.Labels)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO agents (name, labels, executors, enabled, token_hash, created_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET labels = excluded.labels, executors = excluded.executors, enabled = excluded.enabled,
		token_hash = excluded.token_hash`,
		a.Name, labels, a.Executors, a.Enabled, a.TokenHash, ts(time.Now()))
	return err
}

// Agent by name.
func (s *Store) Agent(ctx context.Context, name string) (*Agent, error) {
	a, err := scanAgent(s.db.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE name = ?`, name))
	return a, notFound(err)
}

// AgentByTokenHash is the agent whose token hashes to hash.
func (s *Store) AgentByTokenHash(ctx context.Context, hash string) (*Agent, error) {
	a, err := scanAgent(s.db.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE token_hash = ?`, hash))
	return a, notFound(err)
}

// Agents by name.
func (s *Store) Agents(ctx context.Context) ([]*Agent, error) {
	return collect(ctx, s.db, scanAgent, `SELECT `+agentCols+` FROM agents ORDER BY name`)
}

// DeleteAgent removes one.
func (s *Store) DeleteAgent(ctx context.Context, name string) error {
	return requireChange(s.db.ExecContext(ctx, `DELETE FROM agents WHERE name = ?`, name))
}

// SetAgentSeen records that the agent connected at at, and what it said
// of itself.
func (s *Store) SetAgentSeen(ctx context.Context, name string, at time.Time, os, arch, version string) error {
	return requireChange(s.db.ExecContext(ctx, `UPDATE agents SET last_seen_at = ?, os = ?, arch = ?, version = ? WHERE name = ?`,
		ts(at), os, arch, version, name))
}

func scanAgent(row scanner) (*Agent, error) {
	var a Agent
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
