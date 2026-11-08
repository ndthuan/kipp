package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// User is one login.
type User struct {
	ID    int64
	Name  string
	Hash  string
	Admin bool
	// LastLoginAt is the user's last login through the browser; nil when
	// there was none.
	LastLoginAt *time.Time
	// DeviceEpoch is signed into the user's device cookies; a new password
	// starts a new one, and the cookies of the one before count no more.
	DeviceEpoch int64
}

// Perm is what a user may do on one job.
type Perm struct {
	View, Build, Cancel bool
}

// Names are the perm column's values for what p allows.
func (p Perm) Names() []string {
	var out []string
	for i, on := range []bool{p.View, p.Build, p.Cancel} {
		if on {
			out = append(out, [...]string{"view", "build", "cancel"}[i])
		}
	}
	return out
}

// Token is an API token's row; the token itself is never stored.
type Token struct {
	ID         int64
	UserID     int64
	Label      string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	// ExpiresAt is when the token stops working; nil for never.
	ExpiresAt *time.Time
}

// CreateUser inserts a user; a duplicate name is an error.
func (s *Store) CreateUser(ctx context.Context, name, hash string, admin bool) (*User, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO users (name, hash, admin, created_at) VALUES (?, ?, ?, ?)`, name, hash, admin, ts(time.Now()))
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &User{ID: id, Name: name, Hash: hash, Admin: admin}, nil
}

// userColumns are what scanUser reads, in its order.
const userColumns = `id, name, hash, admin, last_login_at, device_epoch`

func scanUser(row scanner) (*User, error) {
	var u User
	var login sql.NullString
	if err := row.Scan(&u.ID, &u.Name, &u.Hash, &u.Admin, &login, &u.DeviceEpoch); err != nil {
		return nil, notFound(err)
	}
	var err error
	if u.LastLoginAt, err = parseTS(login); err != nil {
		return nil, err
	}
	return &u, nil
}

// UserByName looks a user up.
func (s *Store) UserByName(ctx context.Context, name string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE name = ?`, name))
}

// UserByID looks a user up.
func (s *Store) UserByID(ctx context.Context, id int64) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id))
}

// Users lists every user by name.
func (s *Store) Users(ctx context.Context) ([]*User, error) {
	return collect(ctx, s.db, scanUser, `SELECT `+userColumns+` FROM users ORDER BY name`)
}

// setPassword stores the hash of a new password and starts a new device
// epoch, so the devices that logged in with the old one count no more. An
// unknown user is ErrNotFound.
func setPassword(ctx context.Context, tx *sql.Tx, id int64, hash string) error {
	return requireChange(tx.ExecContext(ctx, `UPDATE users SET hash = ?, device_epoch = device_epoch + 1 WHERE id = ?`, hash, id))
}

// ResetPassword gives a user a new password and ends every session and
// app grant of theirs, in one transaction: somebody who had the old one
// may hold one.
func (s *Store) ResetPassword(ctx context.Context, id int64, hash string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := setPassword(ctx, tx, id, hash); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, id); err != nil {
			return err
		}
		return endOAuthGrants(ctx, tx, id)
	})
}

// endOAuthGrants ends the apps a user let in: a new password is what a
// user does when somebody else may hold access, and an app's token is
// access too.
func endOAuthGrants(ctx context.Context, tx *sql.Tx, id int64) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM oauth_grants WHERE user_id = ?`, id)
	return err
}

// ChangePassword gives a user the new password they chose in the live
// session sessID, ends their other sessions and puts next in sessID's
// place, sudo kept, and ends their app grants, in one transaction: a
// session that ended meanwhile, or is not the user's, is ErrNotFound and
// changes nothing, so the password never changes without the session that
// asked for it being renewed.
func (s *Store) ChangePassword(ctx context.Context, id int64, hash, sessID string, next *Session) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := setPassword(ctx, tx, id, hash); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND hash != ?`, id, sessionKey(sessID)); err != nil {
			return err
		}
		if err := endOAuthGrants(ctx, tx, id); err != nil {
			return err
		}
		return replaceSession(ctx, tx, sessID, id, next)
	})
}

// SetUserHash changes a password's hash and nothing else: the same
// password hashed again, with today's parameters.
func (s *Store) SetUserHash(ctx context.Context, id int64, hash string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET hash = ? WHERE id = ?`, hash, id)
	return err
}

// RecordLogin keeps the time of a user's login.
func (s *Store) RecordLogin(ctx context.Context, id int64, at time.Time) error {
	return requireChange(s.db.ExecContext(ctx, `UPDATE users SET last_login_at = ? WHERE id = ?`, ts(at), id))
}

// leavesAnAdmin is the condition, on a users row, that changing or
// removing it leaves an admin: it is not one, or another is. It is part of
// the statement that makes the change, so two changes at once cannot each
// count the other's admin and both go through.
const leavesAnAdmin = `(admin = 0 OR EXISTS (SELECT 1 FROM users other WHERE other.admin = 1 AND other.id != users.id))`

// SetAdmin turns a user's admin on or off. Turning off the last admin is
// ErrLastAdmin, and an unknown user ErrNotFound.
func (s *Store) SetAdmin(ctx context.Context, id int64, admin bool) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		guard := ``
		if !admin {
			guard = ` AND ` + leavesAnAdmin
		}
		res, err := tx.ExecContext(ctx, `UPDATE users SET admin = ? WHERE id = ?`+guard, admin, id)
		return whyUnchanged(ctx, tx, id, res, err)
	})
}

// DeleteUser removes a user and, by cascade, their permissions, tokens,
// sessions and app grants. Deleting the last admin is ErrLastAdmin, and an
// unknown user ErrNotFound.
func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = ? AND `+leavesAnAdmin, id)
		return whyUnchanged(ctx, tx, id, res, err)
	})
}

// whyUnchanged is the error of a guarded change to a user: none when it
// changed a row, ErrNotFound when the user is not there, and ErrLastAdmin
// when the guard held it back.
func whyUnchanged(ctx context.Context, tx *sql.Tx, id int64, res sql.Result, err error) error {
	if err := requireChange(res, err); !errors.Is(err, ErrNotFound) {
		return err
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE id = ?`, id).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return ErrLastAdmin
}

// Grant replaces the user's permission set on one job; with no perms it
// takes them all away.
func (s *Store) Grant(ctx context.Context, userID int64, job string, perms []string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM permissions WHERE user_id = ? AND job = ?`, userID, job); err != nil {
			return err
		}
		for _, p := range perms {
			if _, err := tx.ExecContext(ctx, `INSERT INTO permissions (user_id, job, perm) VALUES (?, ?, ?)`, userID, job, p); err != nil {
				return err
			}
		}
		return nil
	})
}

// ReplaceGrants sets everything a user may do, job by job, in one
// transaction: a job not in perms, or one with no perm on, is taken away.
// An unknown user is ErrNotFound.
func (s *Store) ReplaceGrants(ctx context.Context, userID int64, perms map[string]Perm) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE id = ?`, userID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM permissions WHERE user_id = ?`, userID); err != nil {
			return err
		}
		for job, p := range perms {
			for _, name := range p.Names() {
				if _, err := tx.ExecContext(ctx, `INSERT INTO permissions (user_id, job, perm) VALUES (?, ?, ?)`, userID, job, name); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// Permissions of a user, by job.
func (s *Store) Permissions(ctx context.Context, userID int64) (map[string]Perm, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT job, perm FROM permissions WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]Perm{}
	for rows.Next() {
		var job, perm string
		if err := rows.Scan(&job, &perm); err != nil {
			return nil, err
		}
		p := out[job]
		switch perm {
		case "view":
			p.View = true
		case "build":
			p.Build = true
		case "cancel":
			p.Cancel = true
		}
		out[job] = p
	}
	return out, rows.Err()
}

// CreateToken stores a token's hash under a user, for good.
func (s *Store) CreateToken(ctx context.Context, userID int64, label, hash string) (*Token, error) {
	return s.CreateTokenUntil(ctx, userID, label, hash, nil)
}

// CreateTokenUntil stores a token's hash under a user, working until
// expires; nil is for good.
func (s *Store) CreateTokenUntil(ctx context.Context, userID int64, label, hash string, expires *time.Time) (*Token, error) {
	now := time.Now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO tokens (user_id, hash, label, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		userID, hash, label, ts(now), nullTS(expires))
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &Token{ID: id, UserID: userID, Label: label, CreatedAt: now, ExpiresAt: expires}, nil
}

// lastUseEvery is how fine a token's last use is kept: a use is written
// when the one on record is older than this.
const lastUseEvery = time.Minute

// UserByTokenHash finds the token's user and records the use, to the
// minute: every request with a token would otherwise be a write. An
// expired token is ErrNotFound, as one that was never made is.
func (s *Store) UserByTokenHash(ctx context.Context, hash string) (*User, error) {
	now := time.Now()
	u, err := scanUser(s.db.QueryRowContext(ctx, `SELECT u.id, u.name, u.hash, u.admin, u.last_login_at, u.device_epoch FROM tokens t JOIN users u ON u.id = t.user_id
		WHERE t.hash = ? AND (t.expires_at IS NULL OR t.expires_at > ?)`, hash, ts(now)))
	if err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tokens SET last_used_at = ? WHERE hash = ? AND (last_used_at IS NULL OR last_used_at < ?)`,
		ts(now), hash, ts(now.Add(-lastUseEvery))); err != nil {
		return nil, err
	}
	return u, nil
}

// Tokens of a user, oldest first.
func (s *Store) Tokens(ctx context.Context, userID int64) ([]*Token, error) {
	return collect(ctx, s.db, scanToken, `SELECT id, user_id, label, created_at, last_used_at, expires_at FROM tokens WHERE user_id = ? ORDER BY id`, userID)
}

// Token by id, with the user it belongs to.
func (s *Store) Token(ctx context.Context, id int64) (*Token, error) {
	t, err := scanToken(s.db.QueryRowContext(ctx, `SELECT id, user_id, label, created_at, last_used_at, expires_at FROM tokens WHERE id = ?`, id))
	return t, notFound(err)
}

// CountTokens is how many tokens a user has, expired ones too: they are
// listed until revoked.
func (s *Store) CountTokens(ctx context.Context, userID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tokens WHERE user_id = ?`, userID).Scan(&n)
	return n, err
}

func scanToken(row scanner) (*Token, error) {
	var t Token
	var created string
	var used, expires sql.NullString
	if err := row.Scan(&t.ID, &t.UserID, &t.Label, &created, &used, &expires); err != nil {
		return nil, err
	}
	var err error
	if t.CreatedAt, err = parseSetTS(created); err != nil {
		return nil, err
	}
	if t.LastUsedAt, err = parseTS(used); err != nil {
		return nil, err
	}
	if t.ExpiresAt, err = parseTS(expires); err != nil {
		return nil, err
	}
	return &t, nil
}

// DeleteToken revokes a token.
func (s *Store) DeleteToken(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tokens WHERE id = ?`, id)
	return err
}

// DeleteUserTokens revokes every token of a user and says how many there
// were.
func (s *Store) DeleteUserTokens(ctx context.Context, userID int64) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM tokens WHERE user_id = ?`, userID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}
