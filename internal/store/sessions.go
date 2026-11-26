package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"time"
)

// Session is a browser login. ID is the cookie's value: callers hold it,
// the database holds only its hash.
type Session struct {
	ID        string
	UserID    int64
	CSRF      string
	ExpiresAt time.Time
	SudoUntil *time.Time
}

func sessionKey(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

// CreateSession stores a browser login.
func (s *Store) CreateSession(ctx context.Context, sess *Session) error {
	return insertSession(ctx, s.db, sess)
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func insertSession(ctx context.Context, db execer, sess *Session) error {
	_, err := db.ExecContext(ctx, `INSERT INTO sessions (hash, user_id, csrf, expires_at, sudo_until) VALUES (?, ?, ?, ?, ?)`,
		sessionKey(sess.ID), sess.UserID, sess.CSRF, ts(sess.ExpiresAt), nullTS(sess.SudoUntil))
	return err
}

// Session by id; an expired one is ErrNotFound.
func (s *Store) Session(ctx context.Context, id string) (*Session, error) {
	return liveSession(ctx, s.db, id, time.Now())
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func liveSession(ctx context.Context, db rowQuerier, id string, now time.Time) (*Session, error) {
	sess := Session{ID: id}
	var exp string
	var sudo sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT user_id, csrf, expires_at, sudo_until FROM sessions WHERE hash = ? AND expires_at > ?`,
		sessionKey(id), ts(now)).Scan(&sess.UserID, &sess.CSRF, &exp, &sudo); err != nil {
		return nil, notFound(err)
	}
	var err error
	if sess.ExpiresAt, err = parseSetTS(exp); err != nil {
		return nil, err
	}
	if sess.SudoUntil, err = parseTS(sudo); err != nil {
		return nil, err
	}
	return &sess, nil
}

// SetSessionSudo gives a live session leave to change credentials and
// secrets until the time given; a session that is gone is ErrNotFound.
func (s *Store) SetSessionSudo(ctx context.Context, id string, until time.Time) error {
	return requireChange(s.db.ExecContext(ctx, `UPDATE sessions SET sudo_until = ? WHERE hash = ? AND expires_at > ?`,
		ts(until), sessionKey(id), ts(time.Now())))
}

func replaceSession(ctx context.Context, tx *sql.Tx, oldID string, userID int64, next *Session) error {
	var sudo sql.NullString
	if err := tx.QueryRowContext(ctx, `DELETE FROM sessions WHERE hash = ? AND user_id = ? AND expires_at > ? RETURNING sudo_until`,
		sessionKey(oldID), userID, ts(time.Now())).Scan(&sudo); err != nil {
		return notFound(err)
	}
	until, err := parseTS(sudo)
	if err != nil {
		return err
	}
	next.UserID, next.SudoUntil = userID, until
	return insertSession(ctx, tx, next)
}

// DeleteSession logs a browser out.
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE hash = ?`, sessionKey(id))
	return err
}

// DeleteUserSessions logs a user out everywhere but the session exceptID;
// an empty exceptID logs them out everywhere.
func (s *Store) DeleteUserSessions(ctx context.Context, userID int64, exceptID string) error {
	if exceptID == "" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND hash != ?`, userID, sessionKey(exceptID))
	return err
}

// CountSessions is how many live sessions a user has.
func (s *Store) CountSessions(ctx context.Context, userID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE user_id = ? AND expires_at > ?`, userID, ts(time.Now())).Scan(&n)
	return n, err
}

// DeleteExpiredSessions is housekeeping for the daily sweep.
func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, ts(now))
	return err
}
