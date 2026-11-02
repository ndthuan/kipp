// Package store is every query kipp makes, over one SQLite file in WAL mode.
// One writer connection (SetMaxOpenConns(1)) keeps SQLITE_BUSY away at the
// scale kipp runs at.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // the driver, no cgo
)

// ErrNotFound is returned where a row was asked for by key and is not there.
var ErrNotFound = errors.New("not found")

// ErrLastAdmin refuses a change that would leave the server with no admin:
// with nobody to see every job, the server's own machine would be the one
// way back in.
var ErrLastAdmin = errors.New("the last admin")

// Store is the open database.
type Store struct {
	db       *sql.DB
	upgraded Upgrade
}

// Close closes the file.
func (s *Store) Close() error { return s.db.Close() }

// inTx runs fn in one transaction, committed when fn returns nil and
// rolled back when it fails or panics. A rollback that fails comes back
// with fn's error: the connection may still hold the transaction open.
func (s *Store) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	return inTx(ctx, s.db, fn)
}

// beginner is what starts a transaction: the database, or one connection
// of it.
type beginner interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

func inTx(ctx context.Context, db beginner, fn func(*sql.Tx) error) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	done := false
	defer func() {
		if !done {
			err = errors.Join(err, tx.Rollback())
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	done = true
	return tx.Commit()
}

// tsLayout is fixed width, so stored times compare correctly as TEXT.
const tsLayout = "2006-01-02T15:04:05.000000000Z"

func ts(t time.Time) string { return t.UTC().Format(tsLayout) }

// nullTS is a time column that may be NULL: nil is stored as NULL.
func nullTS(t *time.Time) sql.NullString {
	if t == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: ts(*t), Valid: true}
}

func parseTS(s sql.NullString) (*time.Time, error) {
	if !s.Valid {
		return nil, nil
	}
	t, err := time.Parse(tsLayout, s.String)
	if err != nil {
		return nil, fmt.Errorf("stored time %q: %w", s.String, err)
	}
	return &t, nil
}

// parseSetTS parses a time column that is never NULL.
func parseSetTS(s string) (time.Time, error) {
	t, err := parseTS(sql.NullString{String: s, Valid: true})
	if err != nil {
		return time.Time{}, err
	}
	return *t, nil
}

// scanner is a row to scan: *sql.Row and *sql.Rows are both one.
type scanner interface{ Scan(...any) error }

// querier is what runs a query: the database, or a transaction on it.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// collect runs a query and scans each of its rows with scan.
func collect[T any](ctx context.Context, db querier, scan func(scanner) (T, error), query string, args ...any) ([]T, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// scanOne is the scan of a query that selects a single column.
func scanOne[T any](row scanner) (T, error) {
	var v T
	err := row.Scan(&v)
	return v, err
}

// requireChange turns an update that matched no row into ErrNotFound.
func requireChange(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
