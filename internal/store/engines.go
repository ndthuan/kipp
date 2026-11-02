package store

import (
	"context"
	"encoding/json"
	"time"
)

// Engine is a place kipp starts containers in, registered by an admin.
// The URL may name a user but never a password, and the TLS material and
// the ssh key are paths of files on the server.
type Engine struct {
	Name, Kind, URL        string
	Labels                 []string
	Max                    int
	Enabled                bool
	TLSCA, TLSCert, TLSKey string
	SSHKey                 string
	CreatedAt, UpdatedAt   time.Time
}

const engineCols = `name, kind, url, labels, max, enabled, tls_ca, tls_cert, tls_key, ssh_key, created_at, updated_at`

// PutEngine adds the engine, or replaces the one of that name and keeps
// when it was added.
func (s *Store) PutEngine(ctx context.Context, e Engine) error {
	labels, err := labelsJSON(e.Labels)
	if err != nil {
		return err
	}
	now := ts(time.Now())
	_, err = s.db.ExecContext(ctx, `INSERT INTO engines (`+engineCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET kind = excluded.kind, url = excluded.url, labels = excluded.labels, max = excluded.max,
		enabled = excluded.enabled, tls_ca = excluded.tls_ca, tls_cert = excluded.tls_cert, tls_key = excluded.tls_key,
		ssh_key = excluded.ssh_key, updated_at = excluded.updated_at`,
		e.Name, e.Kind, e.URL, labels, e.Max, e.Enabled, e.TLSCA, e.TLSCert, e.TLSKey, e.SSHKey, now, now)
	return err
}

// Engine by name.
func (s *Store) Engine(ctx context.Context, name string) (*Engine, error) {
	e, err := scanEngine(s.db.QueryRowContext(ctx, `SELECT `+engineCols+` FROM engines WHERE name = ?`, name))
	return e, notFound(err)
}

// Engines by name.
func (s *Store) Engines(ctx context.Context) ([]*Engine, error) {
	return collect(ctx, s.db, scanEngine, `SELECT `+engineCols+` FROM engines ORDER BY name`)
}

// DeleteEngine removes one.
func (s *Store) DeleteEngine(ctx context.Context, name string) error {
	return requireChange(s.db.ExecContext(ctx, `DELETE FROM engines WHERE name = ?`, name))
}

func scanEngine(row scanner) (*Engine, error) {
	var e Engine
	var labels, created, updated string
	if err := row.Scan(&e.Name, &e.Kind, &e.URL, &labels, &e.Max, &e.Enabled, &e.TLSCA, &e.TLSCert, &e.TLSKey, &e.SSHKey, &created, &updated); err != nil {
		return nil, err
	}
	var err error
	if e.Labels, err = parseLabels(labels); err != nil {
		return nil, err
	}
	if e.CreatedAt, err = parseSetTS(created); err != nil {
		return nil, err
	}
	e.UpdatedAt, err = parseSetTS(updated)
	return &e, err
}

// labelsJSON is how a list of labels is kept: a JSON array, as a stage's
// needs are.
func labelsJSON(labels []string) (string, error) {
	b, err := json.Marshal(append([]string{}, labels...))
	return string(b), err
}

// parseLabels reads a kept list of labels; never nil.
func parseLabels(s string) ([]string, error) {
	out := []string{}
	return out, json.Unmarshal([]byte(s), &out)
}
