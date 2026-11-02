package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// CacheEntry is one saved cache: the row of the file
// <data>/cache/<scope>/<namespace>/<key>.tar.zst.
type CacheEntry struct {
	ID               int64
	Scope, Namespace string
	Key              string
	Size             int64
	SavedAt          time.Time
	SavedByBuild     int64
	LastRestoredAt   time.Time // the zero time until it is first restored
}

const cacheColumns = `id, scope, namespace, key, size, saved_at, saved_by_build, last_restored_at`

// PutCacheEntry records a saved entry and sets its ID. It reports false,
// changing nothing, when the scope and namespace already hold the key: a
// key is never overwritten.
func (s *Store) PutCacheEntry(ctx context.Context, e *CacheEntry) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO cache_entries (scope, namespace, key, size, saved_at, saved_by_build)
		VALUES (?, ?, ?, ?, ?, ?)`, e.Scope, e.Namespace, e.Key, e.Size, ts(e.SavedAt), e.SavedByBuild)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	e.ID, err = res.LastInsertId()
	return err == nil, err
}

// CacheEntry is the entry with the id, or ErrNotFound.
func (s *Store) CacheEntry(ctx context.Context, id int64) (CacheEntry, error) {
	e, err := scanCacheEntry(s.db.QueryRowContext(ctx, `SELECT `+cacheColumns+` FROM cache_entries WHERE id = ?`, id))
	return e, notFound(err)
}

// FindCacheExact is the entry of the key in the scope and namespace, or
// ErrNotFound.
func (s *Store) FindCacheExact(ctx context.Context, scope, namespace, key string) (CacheEntry, error) {
	e, err := scanCacheEntry(s.db.QueryRowContext(ctx, `SELECT `+cacheColumns+` FROM cache_entries
		WHERE scope = ? AND namespace = ? AND key = ?`, scope, namespace, key))
	return e, notFound(err)
}

// FindCachePrefix is the newest entry of the scope and namespace whose key
// starts with prefix, or ErrNotFound.
func (s *Store) FindCachePrefix(ctx context.Context, scope, namespace, prefix string) (CacheEntry, error) {
	// ESCAPE, so that a key's % or _ matches only itself; a key holds
	// neither, but a prefix is whatever a stage typed.
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix)
	e, err := scanCacheEntry(s.db.QueryRowContext(ctx, `SELECT `+cacheColumns+` FROM cache_entries
		WHERE scope = ? AND namespace = ? AND key LIKE ? ESCAPE '\'
		ORDER BY saved_at DESC, id DESC LIMIT 1`, scope, namespace, esc+"%"))
	return e, notFound(err)
}

// TouchCacheEntry records that the entry was restored at at.
func (s *Store) TouchCacheEntry(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE cache_entries SET last_restored_at = ? WHERE id = ?`, ts(at), id)
	return err
}

// CacheEntries are every entry, the least recently restored first (one
// never restored counts from when it was saved).
func (s *Store) CacheEntries(ctx context.Context) ([]CacheEntry, error) {
	return s.CacheEntriesAfter(ctx, time.Time{}, 0, -1)
}

// OrderedAt is the time CacheEntries order an entry by: when it was last
// restored, or when it was saved if it never was.
func (e CacheEntry) OrderedAt() time.Time {
	if e.LastRestoredAt.IsZero() {
		return e.SavedAt
	}
	return e.LastRestoredAt
}

// CacheEntriesAfter is up to limit entries that CacheEntries lists after the
// position (at, id) of the one a page ended on; a zero at and an id of 0
// start at the first, and a limit below 0 is all of them.
func (s *Store) CacheEntriesAfter(ctx context.Context, at time.Time, id int64, limit int) ([]CacheEntry, error) {
	from := ""
	if !at.IsZero() {
		from = ts(at)
	}
	return collect(ctx, s.db, scanCacheEntry, `SELECT `+cacheColumns+` FROM cache_entries
		WHERE COALESCE(last_restored_at, saved_at) > ? OR (COALESCE(last_restored_at, saved_at) = ? AND id > ?)
		ORDER BY COALESCE(last_restored_at, saved_at), id LIMIT ?`, from, from, id, limit)
}

// DeleteCacheEntry removes the row; the file is the caller's.
func (s *Store) DeleteCacheEntry(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM cache_entries WHERE id = ?`, id)
	return err
}

func scanCacheEntry(row scanner) (CacheEntry, error) {
	var (
		e        CacheEntry
		saved    string
		restored sql.NullString
	)
	if err := row.Scan(&e.ID, &e.Scope, &e.Namespace, &e.Key, &e.Size, &saved, &e.SavedByBuild, &restored); err != nil {
		return e, err
	}
	var err error
	if e.SavedAt, err = time.Parse(tsLayout, saved); err != nil {
		return e, err
	}
	if restored.Valid {
		e.LastRestoredAt, err = time.Parse(tsLayout, restored.String)
	}
	return e, err
}
