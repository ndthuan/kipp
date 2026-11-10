package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// migrations are applied in order, once each, and a database's schema
// version is how many of them it has had. Data directories made at each
// released version exist, so a change to the schema is a new entry at the
// end and a released entry is never edited or removed (migrate_test.go
// holds the sum of every released entry). An entry runs inside the one
// transaction of the upgrade with foreign keys off, so one that rebuilds a
// table (create the new one, copy, drop the old, rename) keeps the rows
// that point into it; every key is checked before the upgrade commits.
//
// Why the first entry is shaped as it is:
//   - sessions are kept by the SHA-256 of their id, so a copy of the file
//     is no way into the server;
//   - a user's device_epoch is in the MAC of their device cookies, and a
//     new password starts a new one;
//   - an empty stages.ran_on is the server itself;
//   - metric_values, metric_guards and the cache name the build they came
//     from by job and number, or by id with no key, because a remembered
//     value or a cache entry outlives the build's pruning;
//   - children and owner_repos rows are kept while a build of the child
//     is, so that its builds are still its parent's to grant after the
//     branch, pull request or repo is gone;
//   - owner_repos.hook_fp is a fingerprint of the webhook as kipp last
//     wrote it, since GitHub never hands a secret back.
var migrations = []string{`
CREATE TABLE users (
  id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, hash TEXT NOT NULL,
  admin INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, last_login_at TEXT,
  device_epoch INTEGER NOT NULL DEFAULT 0);
CREATE TABLE permissions (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  job TEXT NOT NULL, perm TEXT NOT NULL CHECK (perm IN ('view','build','cancel')),
  PRIMARY KEY (user_id, job, perm));
CREATE TABLE tokens (
  id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  hash TEXT NOT NULL UNIQUE, label TEXT NOT NULL, created_at TEXT NOT NULL, last_used_at TEXT,
  expires_at TEXT);
CREATE TABLE sessions (
  hash TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  csrf TEXT NOT NULL, expires_at TEXT NOT NULL, sudo_until TEXT);
CREATE INDEX sessions_user ON sessions(user_id);
CREATE TABLE secrets (
  name TEXT PRIMARY KEY, value BLOB NOT NULL, updated_at TEXT NOT NULL,
  kind TEXT NOT NULL DEFAULT 'text', username TEXT NOT NULL DEFAULT '',
  scopes TEXT NOT NULL DEFAULT 'global');
CREATE TABLE builds (
  id INTEGER PRIMARY KEY, job TEXT NOT NULL, number INTEGER NOT NULL, status TEXT NOT NULL,
  trigger TEXT NOT NULL, trigger_detail TEXT NOT NULL DEFAULT '', params TEXT NOT NULL DEFAULT '{}',
  started_by TEXT NOT NULL DEFAULT '', queued_at TEXT NOT NULL, started_at TEXT, finished_at TEXT,
  config_hash TEXT NOT NULL DEFAULT '', upstream_status TEXT NOT NULL DEFAULT '',
  trigger_branch TEXT NOT NULL DEFAULT '', trigger_default_branch TEXT NOT NULL DEFAULT '',
  parent TEXT NOT NULL DEFAULT '', built_commit TEXT NOT NULL DEFAULT '', pr INTEGER NOT NULL DEFAULT 0,
  base_branch TEXT NOT NULL DEFAULT '', base_commit TEXT NOT NULL DEFAULT '',
  pushed_commit TEXT NOT NULL DEFAULT '', kept_by TEXT NOT NULL DEFAULT '', kept_at TEXT,
  trigger_tag TEXT NOT NULL DEFAULT '',
  UNIQUE (job, number));
CREATE INDEX builds_job_id ON builds(job, id);
CREATE INDEX builds_status ON builds(status);
CREATE TABLE stages (
  build_id INTEGER NOT NULL REFERENCES builds(id) ON DELETE CASCADE, seq INTEGER NOT NULL,
  name TEXT NOT NULL, needs TEXT NOT NULL DEFAULT '[]', status TEXT NOT NULL, exit_code INTEGER,
  started_at TEXT, finished_at TEXT, pgid INTEGER NOT NULL DEFAULT 0, born TEXT NOT NULL DEFAULT '',
  ran_on TEXT NOT NULL DEFAULT '', tries INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY (build_id, name));
CREATE TABLE tests (
  id INTEGER PRIMARY KEY, build_id INTEGER NOT NULL REFERENCES builds(id) ON DELETE CASCADE,
  stage TEXT NOT NULL, suite TEXT NOT NULL, name TEXT NOT NULL, status TEXT NOT NULL,
  duration_ms INTEGER NOT NULL DEFAULT 0, message TEXT NOT NULL DEFAULT '');
CREATE INDEX tests_build ON tests(build_id);
CREATE TABLE job_activity (id INTEGER PRIMARY KEY, job TEXT NOT NULL, at TEXT NOT NULL, line TEXT NOT NULL);
CREATE INDEX job_activity_job ON job_activity(job, id);
CREATE TABLE job_counters (job TEXT PRIMARY KEY, last_number INTEGER NOT NULL);
CREATE TABLE audit (
  id INTEGER PRIMARY KEY, at TEXT NOT NULL, actor TEXT NOT NULL, action TEXT NOT NULL,
  target TEXT NOT NULL DEFAULT '', detail TEXT NOT NULL DEFAULT '');
CREATE TABLE webhook_deliveries (id TEXT PRIMARY KEY, job TEXT NOT NULL, at TEXT NOT NULL);
CREATE INDEX webhook_deliveries_at ON webhook_deliveries(at);
CREATE TABLE engines (
  name TEXT PRIMARY KEY, kind TEXT NOT NULL, url TEXT NOT NULL, labels TEXT NOT NULL DEFAULT '[]',
  max INTEGER NOT NULL DEFAULT 1, enabled INTEGER NOT NULL DEFAULT 1,
  tls_ca TEXT NOT NULL DEFAULT '', tls_cert TEXT NOT NULL DEFAULT '', tls_key TEXT NOT NULL DEFAULT '',
  ssh_key TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE agents (
  name TEXT PRIMARY KEY, labels TEXT NOT NULL DEFAULT '[]', executors INTEGER NOT NULL DEFAULT 1,
  enabled INTEGER NOT NULL DEFAULT 1, token_hash TEXT NOT NULL UNIQUE, created_at TEXT NOT NULL,
  last_seen_at TEXT, os TEXT NOT NULL DEFAULT '', arch TEXT NOT NULL DEFAULT '', version TEXT NOT NULL DEFAULT '');
CREATE TABLE metric_history (
  build_id INTEGER NOT NULL REFERENCES builds(id) ON DELETE CASCADE, store TEXT NOT NULL,
  stage TEXT NOT NULL, metric TEXT NOT NULL, item TEXT NOT NULL, value REAL NOT NULL,
  unit TEXT NOT NULL DEFAULT '');
CREATE INDEX metric_history_build ON metric_history(build_id);
CREATE INDEX metric_history_item ON metric_history(store, metric, item, build_id);
CREATE TABLE metric_values (
  store TEXT NOT NULL, metric TEXT NOT NULL, item TEXT NOT NULL, value REAL NOT NULL,
  unit TEXT NOT NULL DEFAULT '', build_id INTEGER NOT NULL DEFAULT 0, job TEXT NOT NULL DEFAULT '',
  number INTEGER NOT NULL DEFAULT 0, at TEXT NOT NULL,
  PRIMARY KEY (store, metric, item));
CREATE TABLE metric_guards (
  build_id INTEGER NOT NULL REFERENCES builds(id) ON DELETE CASCADE, stage TEXT NOT NULL,
  metric TEXT NOT NULL, item TEXT NOT NULL, remembered REAL, reported REAL,
  tolerance REAL NOT NULL, verdict TEXT NOT NULL, outcome TEXT NOT NULL,
  from_job TEXT NOT NULL DEFAULT '', from_number INTEGER NOT NULL DEFAULT 0);
CREATE INDEX metric_guards_build ON metric_guards(build_id);
CREATE TABLE children (
  parent TEXT NOT NULL, key TEXT NOT NULL, kind TEXT NOT NULL, branch TEXT NOT NULL DEFAULT '',
  pr INTEGER NOT NULL DEFAULT 0, title TEXT NOT NULL DEFAULT '', head_sha TEXT NOT NULL DEFAULT '',
  base_branch TEXT NOT NULL DEFAULT '', from_fork INTEGER NOT NULL DEFAULT 0, state TEXT NOT NULL,
  gone_at TEXT, last_seen_at TEXT NOT NULL, author TEXT NOT NULL DEFAULT '', url TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (parent, key));
CREATE TABLE multibranch (
  parent TEXT PRIMARY KEY, default_branch TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL);
CREATE TABLE artifacts (
  build_id INTEGER NOT NULL REFERENCES builds(id) ON DELETE CASCADE, stage TEXT NOT NULL,
  path TEXT NOT NULL, size INTEGER NOT NULL, sha256 TEXT NOT NULL,
  PRIMARY KEY (build_id, stage, path));
CREATE TABLE cache_entries (
  id INTEGER PRIMARY KEY, scope TEXT NOT NULL, namespace TEXT NOT NULL, key TEXT NOT NULL,
  size INTEGER NOT NULL, saved_at TEXT NOT NULL, saved_by_build INTEGER NOT NULL DEFAULT 0,
  last_restored_at TEXT,
  UNIQUE (scope, namespace, key));
CREATE TABLE owner_repos (
  owner TEXT NOT NULL, key TEXT NOT NULL, repo TEXT NOT NULL, state TEXT NOT NULL,
  gone_at TEXT, last_seen_at TEXT NOT NULL,
  hook TEXT NOT NULL DEFAULT '', hook_note TEXT NOT NULL DEFAULT '', hook_fp TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (owner, key));
`, `
CREATE TABLE oauth_clients (
  id TEXT PRIMARY KEY, name TEXT NOT NULL, cimd INTEGER NOT NULL DEFAULT 0,
  redirect_uris TEXT NOT NULL, created_at TEXT NOT NULL, used_at TEXT NOT NULL);
CREATE TABLE oauth_grants (
  id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  client_id TEXT NOT NULL, client_name TEXT NOT NULL, resource TEXT NOT NULL, scopes TEXT NOT NULL,
  created_at TEXT NOT NULL, last_used_at TEXT, expires_at TEXT NOT NULL);
CREATE INDEX oauth_grants_user ON oauth_grants(user_id);
CREATE TABLE oauth_tokens (
  hash TEXT PRIMARY KEY, grant_id INTEGER NOT NULL REFERENCES oauth_grants(id) ON DELETE CASCADE,
  kind TEXT NOT NULL CHECK (kind IN ('access','refresh')), expires_at TEXT NOT NULL, used_at TEXT);
CREATE INDEX oauth_tokens_grant ON oauth_tokens(grant_id);
`, `
CREATE INDEX builds_finished_at ON builds(finished_at);
`, `
CREATE TABLE pending_deliveries (
  seq INTEGER PRIMARY KEY AUTOINCREMENT, route TEXT NOT NULL, name TEXT NOT NULL,
  event TEXT NOT NULL, delivery TEXT NOT NULL, body BLOB NOT NULL, received_at TEXT NOT NULL);
`, `
ALTER TABLE agents RENAME TO runners;
`, `
ALTER TABLE builds ADD COLUMN built_tree TEXT NOT NULL DEFAULT '';
ALTER TABLE builds ADD COLUMN fingerprint TEXT NOT NULL DEFAULT '';
ALTER TABLE builds ADD COLUMN fingerprint_sum TEXT NOT NULL DEFAULT '';
ALTER TABLE stages ADD COLUMN runner_class TEXT NOT NULL DEFAULT '';
ALTER TABLE stages ADD COLUMN tree TEXT NOT NULL DEFAULT '';
ALTER TABLE stages ADD COLUMN reused_job TEXT NOT NULL DEFAULT '';
ALTER TABLE stages ADD COLUMN reused_number INTEGER NOT NULL DEFAULT 0;
ALTER TABLE stages ADD COLUMN reused_stage TEXT NOT NULL DEFAULT '';
CREATE INDEX builds_built_tree ON builds(built_tree) WHERE built_tree != '';
CREATE INDEX stages_tree ON stages(tree) WHERE tree != '';
`}

// Upgrade is what Open did to the schema of the database it opened.
type Upgrade struct {
	From, To int // schema versions; equal when there was nothing to do
	// Backup is the copy of the database taken before the upgrade, empty
	// for a new database and for one that was already up to date.
	Backup string
}

// SchemaError is a database at a schema this kipp does not run on.
type SchemaError struct {
	Path       string
	Have, Want int
}

func (e *SchemaError) Error() string {
	if e.Have > e.Want {
		return fmt.Sprintf("%s is at schema %d and this kipp knows up to %d: a newer kipp upgraded it; run that one, or restore the copy its upgrade kept beside it (%s)",
			e.Path, e.Have, e.Want, backupName(e.Path, "<N>"))
	}
	return fmt.Sprintf("%s is at schema %d and this kipp needs %d: start this kipp's serve on it, which upgrades it", e.Path, e.Have, e.Want)
}

// Open opens or creates the file and brings its schema up to the newest,
// after copying the database as it was beside it (Upgrade says where).
// Only the process that holds the data directory calls it: a schema
// changed under a running server of an older kipp is one that server does
// not know. A database of a newer schema is refused.
func Open(path string) (*Store, error) {
	s, err := openFile(path)
	if err != nil {
		return nil, err
	}
	if err := s.upgrade(context.Background(), path); err != nil {
		return nil, errors.Join(err, s.db.Close())
	}
	return s, nil
}

// OpenCurrent opens a database that is at the newest schema, and refuses
// one at any other.
func OpenCurrent(path string) (*Store, error) {
	s, err := openFile(path)
	if err != nil {
		return nil, err
	}
	have, err := s.schemaVersion(context.Background())
	if err == nil && have != len(migrations) {
		err = &SchemaError{Path: path, Have: have, Want: len(migrations)}
	}
	if err != nil {
		return nil, errors.Join(err, s.db.Close())
	}
	s.upgraded = Upgrade{From: have, To: have}
	return s, nil
}

// Upgrade says what Open did to the schema.
func (s *Store) Upgrade() Upgrade { return s.upgraded }

func openFile(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return &Store{db: db}, nil
}

// schemaVersion is the database's schema, 0 for a file that has none yet.
func (s *Store) schemaVersion(ctx context.Context) (int, error) {
	var tables int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema'`).Scan(&tables); err != nil || tables == 0 {
		return 0, err
	}
	var version int
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema`).Scan(&version)
	return version, err
}

func (s *Store) upgrade(ctx context.Context, path string) error {
	have, err := s.schemaVersion(ctx)
	if err != nil {
		return err
	}
	want := len(migrations)
	s.upgraded = Upgrade{From: have, To: have}
	switch {
	case have > want:
		return &SchemaError{Path: path, Have: have, Want: want}
	case have == want:
		return nil
	case have > 0:
		if s.upgraded.Backup, err = s.backup(ctx, path, have); err != nil {
			return fmt.Errorf("copying the database before upgrading it from schema %d: %w", have, err)
		}
	}
	if err := s.migrate(ctx, have); err != nil {
		return fmt.Errorf("upgrading %s from schema %d to %d: %w", path, have, want, err)
	}
	s.upgraded.To = want
	if s.upgraded.Backup != "" {
		return removeOtherBackups(path, have)
	}
	return nil
}

// migrate applies the migrations after from, all in one transaction on
// one connection with foreign keys off: a failure leaves the database at
// the schema it had, which the kipp before still runs on.
func (s *Store) migrate(ctx context.Context, from int) (err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	// The pragma does nothing inside a transaction, so it is set around one.
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}
	defer func() {
		_, ferr := conn.ExecContext(context.WithoutCancel(ctx), `PRAGMA foreign_keys = ON`)
		err = errors.Join(err, ferr)
	}()
	return inTx(ctx, conn, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema (version INTEGER NOT NULL)`); err != nil {
			return err
		}
		for i := from; i < len(migrations); i++ {
			if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
				return fmt.Errorf("migration %d: %w", i+1, err)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO schema (version) VALUES (?)`, i+1); err != nil {
				return err
			}
		}
		return checkForeignKeys(ctx, tx)
	})
}

// checkForeignKeys fails when a row points at one that is not there.
func checkForeignKeys(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		var table, parent string
		var rowid sql.NullInt64
		var fk int
		if err := rows.Scan(&table, &rowid, &parent, &fk); err != nil {
			return err
		}
		return fmt.Errorf("a row of %s points at a row of %s that is not there", table, parent)
	}
	return rows.Err()
}

// backupName is where the copy of the database at a schema is kept:
// db.sqlite at schema 2 is copied to db.schema-2.sqlite beside it.
func backupName(path, version string) string {
	ext := filepath.Ext(path)
	return strings.TrimSuffix(path, ext) + ".schema-" + version + ext
}

// backup copies the database, with what its write-ahead log holds, to a
// file made for its owner alone: it holds password hashes and sealed
// secrets. A copy at the same schema, left by an upgrade that failed, is
// replaced.
func (s *Store) backup(ctx context.Context, path string, version int) (string, error) {
	dst := backupName(path, strconv.Itoa(version))
	root, err := os.OpenRoot(filepath.Dir(dst))
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	name := filepath.Base(dst)
	if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	// VACUUM INTO writes into an empty file that is already there.
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		return "", errors.Join(err, root.Remove(name))
	}
	return dst, nil
}

// removeOtherBackups removes the copies of earlier upgrades: the one just
// taken is the database as the kipp before ran it, which is what going
// back needs, and each copy is as large as the database.
func removeOtherBackups(path string, keep int) error {
	ext := filepath.Ext(path)
	prefix := filepath.Base(strings.TrimSuffix(path, ext)) + ".schema-"
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		v, ok := strings.CutPrefix(e.Name(), prefix)
		if !ok || !e.Type().IsRegular() {
			continue
		}
		v, ok = strings.CutSuffix(v, ext)
		if n, err := strconv.Atoi(v); !ok || err != nil || n == keep {
			continue
		}
		errs = append(errs, root.Remove(e.Name()))
	}
	return errors.Join(errs...)
}
