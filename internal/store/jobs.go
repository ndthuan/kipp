package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// The ways a job's version came to be, as job_versions.how holds them.
const (
	HowAPI     = "api"
	HowWeb     = "web"
	HowMCP     = "mcp"
	HowImport  = "import"
	HowSeed    = "seed"
	HowFiles   = "files"
	HowRevert  = "revert"
	HowResolve = "resolve"
)

// AuthorKipp is the author of a version a load found on disk.
const AuthorKipp = "kipp"

// JobVersion is one text a job had: made through kipp, or found on disk.
// A deletion has no text and no hash.
type JobVersion struct {
	ID      int64
	Job     string
	Text    string
	Deleted bool
	// Hash is the job's decoded hash (config.DecodedHash).
	Hash   string
	Author string
	How    string
	At     time.Time
}

// JobEdit is a job kept in the store: its version, and the decoded hash of
// the job file it was made over, empty when there was none.
type JobEdit struct {
	Version *JobVersion
	Base    string
}

// JobChanges are written in one transaction: Versions are appended (their
// IDs set), then Edits are put, Retire is removed from the edits, and Files
// says what each job file's decoded hash now is ("" for a file gone).
type JobChanges struct {
	Versions []*JobVersion
	// Edits are by job name; an edit's Version is one of Versions, or one
	// already stored.
	Edits  map[string]JobEdit
	Retire []string
	Files  map[string]string
	// Mark is set in the same transaction, when not empty.
	Mark string
}

// MarkSeed is the mark of a data directory whose first start in store mode
// decided whether to create the example jobs: it never decides again.
const MarkSeed = "seed"

// ApplyJobChanges writes c.
func (s *Store) ApplyJobChanges(ctx context.Context, c JobChanges) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, step := range []func(context.Context, *sql.Tx, JobChanges) error{addVersions, putEdits, retireEdits, putFiles, putMark} {
			if err := step(ctx, tx, c); err != nil {
				return err
			}
		}
		return nil
	})
}

func addVersions(ctx context.Context, tx *sql.Tx, c JobChanges) error {
	for _, v := range c.Versions {
		res, err := tx.ExecContext(ctx, `INSERT INTO job_versions (job, text, deleted, hash, author, how, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			v.Job, v.Text, v.Deleted, v.Hash, v.Author, v.How, ts(v.At))
		if err != nil {
			return err
		}
		if v.ID, err = res.LastInsertId(); err != nil {
			return err
		}
	}
	return nil
}

func putEdits(ctx context.Context, tx *sql.Tx, c JobChanges) error {
	for job, e := range c.Edits {
		base := sql.NullString{String: e.Base, Valid: e.Base != ""}
		if _, err := tx.ExecContext(ctx, `INSERT INTO job_edits (job, version_id, base) VALUES (?, ?, ?)
			ON CONFLICT (job) DO UPDATE SET version_id = excluded.version_id, base = excluded.base`, job, e.Version.ID, base); err != nil {
			return err
		}
	}
	return nil
}

func retireEdits(ctx context.Context, tx *sql.Tx, c JobChanges) error {
	for _, job := range c.Retire {
		if _, err := tx.ExecContext(ctx, `DELETE FROM job_edits WHERE job = ?`, job); err != nil {
			return err
		}
	}
	return nil
}

func putFiles(ctx context.Context, tx *sql.Tx, c JobChanges) error {
	for job, hash := range c.Files {
		var err error
		if hash == "" {
			_, err = tx.ExecContext(ctx, `DELETE FROM job_files WHERE job = ?`, job)
		} else {
			_, err = tx.ExecContext(ctx, `INSERT INTO job_files (job, hash) VALUES (?, ?) ON CONFLICT (job) DO UPDATE SET hash = excluded.hash`, job, hash)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func putMark(ctx context.Context, tx *sql.Tx, c JobChanges) error {
	if c.Mark == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO marks (name, at) VALUES (?, ?) ON CONFLICT (name) DO NOTHING`, c.Mark, ts(time.Now()))
	return err
}

// SeedDue reports whether the example jobs may still be created: the seed
// was never decided, no build was ever made, and no job was ever kept in
// the store (a version through kipp, not one a load found on disk).
func (s *Store) SeedDue(ctx context.Context) (bool, error) {
	var due bool
	err := s.db.QueryRowContext(ctx, `SELECT NOT EXISTS (SELECT 1 FROM marks WHERE name = ?)
		AND NOT EXISTS (SELECT 1 FROM builds)
		AND NOT EXISTS (SELECT 1 FROM job_versions WHERE how != ?)`, MarkSeed, HowFiles).Scan(&due)
	return due, err
}

const versionColumns = `v.id, v.job, v.text, v.deleted, v.hash, v.author, v.how, v.created_at`

func scanJobVersion(row scanner, extra ...any) (*JobVersion, error) {
	var v JobVersion
	var at string
	if err := row.Scan(append([]any{&v.ID, &v.Job, &v.Text, &v.Deleted, &v.Hash, &v.Author, &v.How, &at}, extra...)...); err != nil {
		return nil, err
	}
	var err error
	v.At, err = parseSetTS(at)
	return &v, err
}

// JobEdits are the jobs kept in the store, by name.
func (s *Store) JobEdits(ctx context.Context) (map[string]JobEdit, error) {
	type row struct {
		v    *JobVersion
		base sql.NullString
	}
	rows, err := collect(ctx, s.db, func(r scanner) (row, error) {
		var base sql.NullString
		v, err := scanJobVersion(r, &base)
		return row{v, base}, err
	}, `SELECT `+versionColumns+`, e.base FROM job_edits e JOIN job_versions v ON v.id = e.version_id`)
	if err != nil {
		return nil, err
	}
	out := make(map[string]JobEdit, len(rows))
	for _, r := range rows {
		out[r.v.Job] = JobEdit{Version: r.v, Base: r.base.String}
	}
	return out, nil
}

// JobFiles is the decoded hash of each job file as the last load saw it.
func (s *Store) JobFiles(ctx context.Context) (map[string]string, error) {
	type pair struct{ job, hash string }
	rows, err := collect(ctx, s.db, func(r scanner) (pair, error) {
		var p pair
		return p, r.Scan(&p.job, &p.hash)
	}, `SELECT job, hash FROM job_files`)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, p := range rows {
		out[p.job] = p.hash
	}
	return out, nil
}

// LastJobVersions is each job's newest version, by name.
func (s *Store) LastJobVersions(ctx context.Context) (map[string]*JobVersion, error) {
	rows, err := collect(ctx, s.db, func(r scanner) (*JobVersion, error) { return scanJobVersion(r) },
		`SELECT `+versionColumns+` FROM job_versions v WHERE v.id IN (SELECT MAX(id) FROM job_versions GROUP BY job)`)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*JobVersion, len(rows))
	for _, v := range rows {
		out[v.Job] = v
	}
	return out, nil
}

// JobVersions are up to limit of the job's versions older than beforeID,
// newest first; a beforeID of 0 starts at the newest.
func (s *Store) JobVersions(ctx context.Context, job string, beforeID int64, limit int) ([]*JobVersion, error) {
	if beforeID <= 0 {
		beforeID = 1<<63 - 1
	}
	return collect(ctx, s.db, func(r scanner) (*JobVersion, error) { return scanJobVersion(r) },
		`SELECT `+versionColumns+` FROM job_versions v WHERE v.job = ? AND v.id < ? ORDER BY v.id DESC LIMIT ?`, job, beforeID, limit)
}

// JobVersion is the version id of job, or ErrNotFound.
func (s *Store) JobVersion(ctx context.Context, job string, id int64) (*JobVersion, error) {
	v, err := scanJobVersion(s.db.QueryRowContext(ctx, `SELECT `+versionColumns+` FROM job_versions v WHERE v.job = ? AND v.id = ?`, job, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return v, err
}
