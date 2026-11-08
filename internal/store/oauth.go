package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ErrOAuthReuse is a refresh token presented again after it was used: the
// grant it belonged to is gone.
var ErrOAuthReuse = errors.New("a used refresh token was presented again")

// OAuthClient is an app that may ask for access: one that registered
// itself, or one named by the URL of its metadata document (CIMD).
type OAuthClient struct {
	ID           string
	Name         string
	CIMD         bool
	RedirectURIs []string
	CreatedAt    time.Time
	UsedAt       time.Time
}

// OAuthGrant is what a user let an app do: its token hashes hang off it,
// and deleting it ends them all.
type OAuthGrant struct {
	ID         int64
	UserID     int64
	ClientID   string
	ClientName string
	// Resource is the audience of its tokens.
	Resource   string
	Scopes     []string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	// ExpiresAt is when the consent ends, whatever refreshes it saw.
	ExpiresAt time.Time
}

// OAuthIssue is the pair of tokens a grant is given: the hashes the store
// keeps and how long each lasts, cut short by the grant's own end.
type OAuthIssue struct {
	AccessHash, RefreshHash string
	AccessTTL, RefreshTTL   time.Duration
}

// SaveOAuthClient stores a client, or renews the name and redirect URIs
// of the one with its id, and says whether it was new.
func (s *Store) SaveOAuthClient(ctx context.Context, c *OAuthClient, now time.Time) (created bool, err error) {
	uris := strings.Join(c.RedirectURIs, "\n")
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE oauth_clients SET name = ?, redirect_uris = ?, used_at = ? WHERE id = ?`, c.Name, uris, ts(now), c.ID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n > 0 {
			return err
		}
		created = true
		_, err = tx.ExecContext(ctx, `INSERT INTO oauth_clients (id, name, cimd, redirect_uris, created_at, used_at) VALUES (?, ?, ?, ?, ?, ?)`,
			c.ID, c.Name, c.CIMD, uris, ts(now), ts(now))
		return err
	})
	return created, err
}

// OAuthClient is the client with the id, or ErrNotFound.
func (s *Store) OAuthClient(ctx context.Context, id string) (*OAuthClient, error) {
	var c OAuthClient
	var uris, created, used string
	if err := s.db.QueryRowContext(ctx, `SELECT id, name, cimd, redirect_uris, created_at, used_at FROM oauth_clients WHERE id = ?`, id).
		Scan(&c.ID, &c.Name, &c.CIMD, &uris, &created, &used); err != nil {
		return nil, notFound(err)
	}
	c.RedirectURIs = strings.Split(uris, "\n")
	var err error
	if c.CreatedAt, err = parseSetTS(created); err != nil {
		return nil, err
	}
	c.UsedAt, err = parseSetTS(used)
	return &c, err
}

// TouchOAuthClient records a use of the client, which keeps it from being
// swept.
func (s *Store) TouchOAuthClient(ctx context.Context, id string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE oauth_clients SET used_at = ? WHERE id = ?`, ts(now), id)
	return err
}

// CountOAuthClients is how many clients are stored.
func (s *Store) CountOAuthClients(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM oauth_clients`).Scan(&n)
	return n, err
}

const grantColumns = `g.id, g.user_id, g.client_id, g.client_name, g.resource, g.scopes, g.created_at, g.last_used_at, g.expires_at`

// scanGrant reads grantColumns, then whatever else the row has into more.
func scanGrant(row scanner, more ...any) (*OAuthGrant, error) {
	var g OAuthGrant
	var scopes, created, expires string
	var used sql.NullString
	dest := append([]any{&g.ID, &g.UserID, &g.ClientID, &g.ClientName, &g.Resource, &scopes, &created, &used, &expires}, more...)
	if err := row.Scan(dest...); err != nil {
		return nil, notFound(err)
	}
	g.Scopes = strings.Fields(scopes)
	var err error
	if g.CreatedAt, err = parseSetTS(created); err != nil {
		return nil, err
	}
	if g.LastUsedAt, err = parseTS(used); err != nil {
		return nil, err
	}
	g.ExpiresAt, err = parseSetTS(expires)
	return &g, err
}

func earlier(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func insertIssue(ctx context.Context, tx *sql.Tx, grantID int64, grantEnds time.Time, is OAuthIssue, now time.Time) error {
	for _, t := range []struct {
		hash, kind string
		ttl        time.Duration
	}{{is.AccessHash, "access", is.AccessTTL}, {is.RefreshHash, "refresh", is.RefreshTTL}} {
		if _, err := tx.ExecContext(ctx, `INSERT INTO oauth_tokens (hash, grant_id, kind, expires_at) VALUES (?, ?, ?, ?)`,
			t.hash, grantID, t.kind, ts(earlier(now.Add(t.ttl), grantEnds))); err != nil {
			return err
		}
	}
	return nil
}

// CreateOAuthGrant stores a grant, with its first tokens, and sets its ID.
// A user who is gone meanwhile is ErrNotFound.
func (s *Store) CreateOAuthGrant(ctx context.Context, g *OAuthGrant, is OAuthIssue, now time.Time) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO oauth_grants (user_id, client_id, client_name, resource, scopes, created_at, expires_at)
			SELECT id, ?, ?, ?, ?, ?, ? FROM users WHERE id = ?`,
			g.ClientID, g.ClientName, g.Resource, strings.Join(g.Scopes, " "), ts(now), ts(g.ExpiresAt), g.UserID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return errors.Join(err, ErrNotFound)
		}
		if g.ID, err = res.LastInsertId(); err != nil {
			return err
		}
		g.CreatedAt = now
		return insertIssue(ctx, tx, g.ID, g.ExpiresAt, is, now)
	})
}

// OAuthGrants are a user's live grants, oldest first.
func (s *Store) OAuthGrants(ctx context.Context, userID int64, now time.Time) ([]*OAuthGrant, error) {
	return collect(ctx, s.db, func(r scanner) (*OAuthGrant, error) { return scanGrant(r) },
		`SELECT `+grantColumns+` FROM oauth_grants g WHERE g.user_id = ? AND g.expires_at > ? ORDER BY g.id`, userID, ts(now))
}

// OAuthGrant is the grant with the id, which must be the user's.
func (s *Store) OAuthGrant(ctx context.Context, userID, id int64) (*OAuthGrant, error) {
	return scanGrant(s.db.QueryRowContext(ctx, `SELECT `+grantColumns+` FROM oauth_grants g WHERE g.id = ? AND g.user_id = ?`, id, userID))
}

// DeleteOAuthGrant ends a grant and its tokens; one already gone is
// ErrNotFound.
func (s *Store) DeleteOAuthGrant(ctx context.Context, id int64) error {
	return requireChange(s.db.ExecContext(ctx, `DELETE FROM oauth_grants WHERE id = ?`, id))
}

// DeleteUserOAuthGrants ends every grant of a user and says how many there
// were.
func (s *Store) DeleteUserOAuthGrants(ctx context.Context, userID int64) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM oauth_grants WHERE user_id = ?`, userID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// OAuthAccess finds the live grant an access token belongs to, with its
// user, and records the use to the minute. Anything else, a refresh token
// or an expired one included, is ErrNotFound.
func (s *Store) OAuthAccess(ctx context.Context, hash string, now time.Time) (*OAuthGrant, *User, error) {
	var u User
	var login sql.NullString
	g, err := scanGrant(s.db.QueryRowContext(ctx, `SELECT `+grantColumns+`, u.id, u.name, u.hash, u.admin, u.last_login_at, u.device_epoch
		FROM oauth_tokens t JOIN oauth_grants g ON g.id = t.grant_id JOIN users u ON u.id = g.user_id
		WHERE t.hash = ? AND t.kind = 'access' AND t.expires_at > ? AND g.expires_at > ?`, hash, ts(now), ts(now)),
		&u.ID, &u.Name, &u.Hash, &u.Admin, &login, &u.DeviceEpoch)
	if err != nil {
		return nil, nil, err
	}
	if u.LastLoginAt, err = parseTS(login); err != nil {
		return nil, nil, err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE oauth_grants SET last_used_at = ? WHERE id = ? AND (last_used_at IS NULL OR last_used_at < ?)`,
		ts(now), g.ID, ts(now.Add(-lastUseEvery))); err != nil {
		return nil, nil, err
	}
	return g, &u, nil
}

// RotateOAuthRefresh trades the refresh token hash, which client clientID
// holds, for the tokens in is. Each refresh token works once: one used
// before is ErrOAuthReuse, and the grant is deleted, returned for the
// record. A token that is unknown, expired, another client's or of an
// expired grant is ErrNotFound.
func (s *Store) RotateOAuthRefresh(ctx context.Context, hash, clientID string, is OAuthIssue, now time.Time) (*OAuthGrant, error) {
	var g *OAuthGrant
	reused := false
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		// The update comes first: it takes the write lock, so two uses of
		// one token cannot both find it unused.
		var id int64
		err := tx.QueryRowContext(ctx, `UPDATE oauth_tokens SET used_at = ? WHERE hash = ? AND kind = 'refresh' AND used_at IS NULL AND expires_at > ?
			AND grant_id IN (SELECT id FROM oauth_grants WHERE client_id = ? AND expires_at > ?) RETURNING grant_id`,
			ts(now), hash, ts(now), clientID, ts(now)).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			g, err = scanGrant(tx.QueryRowContext(ctx, `SELECT `+grantColumns+` FROM oauth_tokens t JOIN oauth_grants g ON g.id = t.grant_id
				WHERE t.hash = ? AND t.kind = 'refresh' AND t.used_at IS NOT NULL AND g.client_id = ?`, hash, clientID))
			if err != nil {
				return err
			}
			reused = true
			_, err = tx.ExecContext(ctx, `DELETE FROM oauth_grants WHERE id = ?`, g.ID)
			return err
		}
		if err != nil {
			return err
		}
		if g, err = scanGrant(tx.QueryRowContext(ctx, `SELECT `+grantColumns+` FROM oauth_grants g WHERE g.id = ?`, id)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE oauth_grants SET last_used_at = ? WHERE id = ?`, ts(now), id); err != nil {
			return err
		}
		return insertIssue(ctx, tx, id, g.ExpiresAt, is, now)
	})
	switch {
	case err != nil:
		return nil, err
	case reused:
		return g, ErrOAuthReuse
	}
	return g, nil
}

// OAuthGrantOfToken is the grant a token of either kind belongs to, for
// the revocation of the token.
func (s *Store) OAuthGrantOfToken(ctx context.Context, hash string) (*OAuthGrant, error) {
	return scanGrant(s.db.QueryRowContext(ctx, `SELECT `+grantColumns+` FROM oauth_tokens t JOIN oauth_grants g ON g.id = t.grant_id WHERE t.hash = ?`, hash))
}

// DeleteExpiredOAuth is housekeeping for the daily sweep: grants past
// their end, tokens past theirs, and the clients nothing used since
// unusedBefore that no grant names.
func (s *Store) DeleteExpiredOAuth(ctx context.Context, now, unusedBefore time.Time) error {
	for _, q := range []struct {
		sql string
		at  time.Time
	}{
		{`DELETE FROM oauth_grants WHERE expires_at <= ?`, now},
		{`DELETE FROM oauth_tokens WHERE expires_at <= ?`, now},
		{`DELETE FROM oauth_clients WHERE used_at < ? AND id NOT IN (SELECT client_id FROM oauth_grants)`, unusedBefore},
	} {
		if _, err := s.db.ExecContext(ctx, q.sql, ts(q.at)); err != nil {
			return err
		}
	}
	return nil
}
