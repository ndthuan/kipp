package store

import (
	"context"
	"strings"
	"time"
)

// DefaultScope is the scope of a secret nobody narrowed.
const DefaultScope = "global"

// scopesOf reads the column; an empty one is the default.
func scopesOf(col string) []string {
	if col == "" {
		return []string{DefaultScope}
	}
	return strings.Split(col, "\n")
}

// PutSecret stores or replaces a sealed one-line value.
func (s *Store) PutSecret(ctx context.Context, name string, sealed []byte) error {
	return s.PutSecretKind(ctx, name, "text", "", sealed, nil)
}

// PutSecretKind stores or replaces a sealed value of the given kind. user is
// a userpass secret's user, kept in the clear; the sealed value is its
// password, or a file's bytes. scopes replace the secret's; nil keeps those
// of a secret set before, and is global for a new one.
func (s *Store) PutSecretKind(ctx context.Context, name, kind, user string, sealed []byte, scopes []string) error {
	col := DefaultScope
	if scopes != nil {
		col = strings.Join(scopes, "\n")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO secrets (name, value, kind, username, scopes, updated_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET value = excluded.value, kind = excluded.kind,
		username = excluded.username, updated_at = excluded.updated_at,
		scopes = CASE WHEN ? THEN excluded.scopes ELSE secrets.scopes END`,
		name, sealed, kind, user, col, ts(time.Now()), scopes != nil)
	return err
}

// SetSecretScopes replaces the scopes of the secret name and leaves its
// value as it is; ErrNotFound when there is none.
func (s *Store) SetSecretScopes(ctx context.Context, name string, scopes []string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE secrets SET scopes = ? WHERE name = ?`, strings.Join(scopes, "\n"), name)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// Secret returns the sealed value.
func (s *Store) Secret(ctx context.Context, name string) ([]byte, error) {
	rec, err := s.SecretRecord(ctx, name)
	return rec.Sealed, err
}

// SecretRecord is a stored secret as it is kept: its kind, its user (for
// a userpass) and its sealed value.
type SecretRecord struct {
	Kind, User string
	Sealed     []byte
	// Scopes are the jobs that may list the secret, as config reads them.
	Scopes []string
}

// SecretRecord returns the secret called name.
func (s *Store) SecretRecord(ctx context.Context, name string) (SecretRecord, error) {
	var rec SecretRecord
	var scopes string
	err := s.db.QueryRowContext(ctx, `SELECT kind, username, value, scopes FROM secrets WHERE name = ?`, name).Scan(&rec.Kind, &rec.User, &rec.Sealed, &scopes)
	rec.Scopes = scopesOf(scopes)
	return rec, notFound(err)
}

// SecretNames lists names, sorted; never values.
func (s *Store) SecretNames(ctx context.Context) ([]string, error) {
	return collect(ctx, s.db, scanOne[string], `SELECT name FROM secrets ORDER BY name`)
}

// SecretInfo is what may be shown of a secret: never its value.
type SecretInfo struct {
	Name      string
	Kind      string
	User      string
	Scopes    []string
	UpdatedAt time.Time
}

// SecretInfos lists every secret's name and when it was last set, by name.
func (s *Store) SecretInfos(ctx context.Context) ([]SecretInfo, error) {
	return collect(ctx, s.db, func(row scanner) (SecretInfo, error) {
		var info SecretInfo
		var updated, scopes string
		if err := row.Scan(&info.Name, &info.Kind, &info.User, &scopes, &updated); err != nil {
			return info, err
		}
		info.Scopes = scopesOf(scopes)
		var err error
		info.UpdatedAt, err = parseSetTS(updated)
		return info, err
	}, `SELECT name, kind, username, scopes, updated_at FROM secrets ORDER BY name`)
}

// DeleteSecret removes one.
func (s *Store) DeleteSecret(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM secrets WHERE name = ?`, name)
	return err
}
