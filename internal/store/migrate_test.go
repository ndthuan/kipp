package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"
)

// migrationSums are the SHA-256 of every migration as it was released.
// Data directories made at each of them exist, so an entry is never
// edited: a change to the schema is a new migration, and its sum is
// appended here.
var migrationSums = []string{
	"cd2482859ccc037c683ad245bcac8c380cb66420818a24705369b4dd9476ffe2",
}

func TestMigrationsAreAppendOnly(t *testing.T) {
	if len(migrations) < len(migrationSums) {
		t.Fatalf("%d migrations, but %d were released: one was removed", len(migrations), len(migrationSums))
	}
	for i, m := range migrations {
		sum := sha256.Sum256([]byte(m))
		got := hex.EncodeToString(sum[:])
		if i >= len(migrationSums) {
			t.Errorf("migration %d is new: append its sum %s to migrationSums", i+1, got)
			continue
		}
		if got != migrationSums[i] {
			t.Errorf("migration %d was edited (sum %s): a released migration stays as it is; append a new one", i+1, got)
		}
	}
}

// withMigrations runs fn with the list of migrations set to ms.
func withMigrations(t *testing.T, ms []string, fn func()) {
	t.Helper()
	all := migrations
	migrations = ms
	defer func() { migrations = all }()
	fn()
}

// openWith makes a database of the migrations ms at path.
func openWith(t *testing.T, path string, ms []string) {
	t.Helper()
	withMigrations(t, ms, func() {
		s, err := Open(path)
		must(t, err)
		must(t, s.Close())
	})
}

// later is the list of migrations with one more at the end, as the next
// release's list will be, for the tests of an upgrade.
func later() []string {
	return slices.Concat(migrations, []string{`CREATE TABLE later (id INTEGER PRIMARY KEY);`})
}

func schemaVersion(t *testing.T, s *Store) int {
	t.Helper()
	var v int
	must(t, s.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema`).Scan(&v))
	return v
}

// Every schema a release ever left on disk is brought up to the newest,
// and what it held is still there and still consistent.
func TestADatabaseOfEverySchemaUpgradesToTheNewest(t *testing.T) {
	upgradesEverySchema(t)
	withMigrations(t, later(), func() { upgradesEverySchema(t) })
}

func upgradesEverySchema(t *testing.T) {
	t.Helper()
	for n := 1; n < len(migrations); n++ {
		path := filepath.Join(t.TempDir(), "db.sqlite")
		withMigrations(t, migrations[:n], func() {
			s, err := Open(path)
			must(t, err)
			_, err = s.db.ExecContext(ctx, `INSERT INTO builds (job, number, status, trigger, queued_at) VALUES ('a', 1, 'success', 'cli', ?)`, ts(time.Now()))
			must(t, err)
			_, err = s.db.ExecContext(ctx, `INSERT INTO stages (build_id, seq, name, status) VALUES (1, 0, 'one', 'success')`)
			must(t, err)
			must(t, s.Close())
		})

		s, err := Open(path)
		must(t, err)
		if v := schemaVersion(t, s); v != len(migrations) {
			t.Fatalf("from schema %d: at %d, want %d", n, v, len(migrations))
		}
		if up := s.Upgrade(); up.From != n || up.To != len(migrations) {
			t.Fatalf("from schema %d: upgrade reads %+v", n, up)
		}
		var ok string
		must(t, s.db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&ok))
		if ok != "ok" {
			t.Fatalf("from schema %d: integrity_check says %s", n, ok)
		}
		stages, err := s.Stages(ctx, 1)
		must(t, err)
		if len(stages) != 1 {
			t.Fatalf("from schema %d: the build's stages read %+v", n, stages)
		}
		must(t, s.Close())
	}
}

func TestANewDatabaseIsMadeAtTheNewestSchemaWithNoCopy(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "db.sqlite"))
	must(t, err)
	t.Cleanup(func() { must(t, s.Close()) })
	if up := s.Upgrade(); up.From != 0 || up.To != len(migrations) || up.Backup != "" {
		t.Fatalf("a new database's upgrade reads %+v", up)
	}
	if v := schemaVersion(t, s); v != len(migrations) {
		t.Fatalf("a new database is at schema %d, want %d", v, len(migrations))
	}
	entries, err := os.ReadDir(dir)
	must(t, err)
	for _, e := range entries {
		if e.Name() != "db.sqlite" && e.Name() != "db.sqlite-wal" && e.Name() != "db.sqlite-shm" {
			t.Fatalf("a new database left %s beside it", e.Name())
		}
	}
}

func TestADatabaseAtTheNewestSchemaIsLeftAsItIs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")
	openWith(t, path, migrations)
	s, err := Open(path)
	must(t, err)
	t.Cleanup(func() { must(t, s.Close()) })
	if up := s.Upgrade(); up.From != up.To || up.Backup != "" {
		t.Fatalf("an up-to-date database's upgrade reads %+v", up)
	}
}

// The copy taken before an upgrade is the database as it was, readable by
// its owner alone, and the copy of an earlier upgrade goes: one is kept.
func TestAnUpgradeKeepsACopyOfTheDatabaseAsItWas(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.sqlite")
	openWith(t, path, migrations)
	have := len(migrations)
	stale := filepath.Join(dir, "db.schema-0.sqlite")
	must(t, os.WriteFile(stale, []byte("an older copy"), 0o600))
	other := filepath.Join(dir, "db.schema-notes.txt")
	must(t, os.WriteFile(other, []byte("not a copy"), 0o600))

	var up Upgrade
	withMigrations(t, later(), func() {
		s, err := Open(path)
		must(t, err)
		up = s.Upgrade()
		must(t, s.Close())
	})
	backup := filepath.Join(dir, "db.schema-"+strconv.Itoa(have)+".sqlite")
	if up.Backup != backup || up.From != have || up.To != have+1 {
		t.Fatalf("upgrade reads %+v, want one from %d to %d with a copy at %q", up, have, have+1, backup)
	}
	info, err := os.Stat(backup)
	must(t, err)
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("the copy has mode %v, want 600", perm)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the copy of an earlier upgrade is still there: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("a file that only looks like a copy was touched: %v", err)
	}

	old, err := OpenCurrent(backup)
	must(t, err)
	must(t, old.Close())
}

func TestADatabaseOfANewerKippIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")
	newer := later()
	openWith(t, path, newer)
	for _, open := range []func(string) (*Store, error){Open, OpenCurrent} {
		s, err := open(path)
		var se *SchemaError
		if !errors.As(err, &se) || se.Have != len(newer) || se.Want != len(migrations) {
			if s != nil {
				_ = s.Close()
			}
			t.Fatalf("a database of a newer schema opens: %v", err)
		}
	}
}

// Commands other than the server do not upgrade: they would change the
// schema under a server of the older kipp still running on it.
func TestOpenCurrentRefusesADatabaseThatNeedsAnUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")
	openWith(t, path, migrations)
	have, next := len(migrations), later()
	withMigrations(t, next, func() {
		_, err := OpenCurrent(path)
		var se *SchemaError
		if !errors.As(err, &se) || se.Have != have || se.Want != len(next) {
			t.Fatalf("OpenCurrent on an older schema: %v", err)
		}
		s, err := Open(path)
		must(t, err)
		if v := schemaVersion(t, s); v != len(next) {
			t.Fatalf("Open left the schema at %d", v)
		}
		must(t, s.Close())
		s, err = OpenCurrent(path)
		must(t, err)
		must(t, s.Close())
	})
}

// A migration that rebuilds a table drops the old one; with foreign keys
// on, that would delete every row that points into it.
func TestAMigrationThatRebuildsATableKeepsTheRowsThatPointIntoIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")
	s, err := Open(path)
	must(t, err)
	for _, q := range []string{
		`INSERT INTO users (id, name, hash, created_at) VALUES (1, 'ann', 'h', 't')`,
		`INSERT INTO tokens (user_id, hash, label, created_at) VALUES (1, 'th', 'laptop', 't')`,
		`INSERT INTO permissions (user_id, job, perm) VALUES (1, 'a', 'view')`,
	} {
		_, err = s.db.ExecContext(ctx, q)
		must(t, err)
	}
	must(t, s.Close())

	rebuild := `
CREATE TABLE users_new (
  id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, hash TEXT NOT NULL,
  admin INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, note TEXT NOT NULL DEFAULT '');
INSERT INTO users_new (id, name, hash, admin, created_at) SELECT id, name, hash, admin, created_at FROM users;
DROP TABLE users;
ALTER TABLE users_new RENAME TO users;
`
	withMigrations(t, slices.Concat(migrations, []string{rebuild}), func() {
		s, err := Open(path)
		must(t, err)
		defer func() { must(t, s.Close()) }()
		var n int
		must(t, s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM tokens) + (SELECT COUNT(*) FROM permissions)`).Scan(&n))
		if n != 2 {
			t.Fatalf("after the rebuild %d of the 2 rows that point at the user are left", n)
		}
		var fk int
		must(t, s.db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk))
		if fk != 1 {
			t.Fatal("foreign keys are still off after the upgrade")
		}
	})
}

// The pending migrations are one transaction: one that fails leaves the
// database at the schema it had, which the kipp before still opens.
func TestAFailedUpgradeLeavesTheDatabaseAsItWas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")
	openWith(t, path, migrations)
	cases := map[string]string{
		"an error":         `ALTER TABLE builds ADD COLUMN x TEXT; THIS IS NOT SQL;`,
		"a dangling key":   `ALTER TABLE builds ADD COLUMN x TEXT; INSERT INTO stages (build_id, seq, name, status) VALUES (99, 0, 'orphan', 'success');`,
		"a dangling build": `CREATE TABLE extra (id INTEGER PRIMARY KEY); INSERT INTO tests (build_id, stage, suite, name, status) VALUES (42, 's', 's', 'n', 'passed');`,
	}
	for name, bad := range cases {
		next := slices.Concat(migrations, []string{`CREATE TABLE fine (id INTEGER);`, bad})
		withMigrations(t, next, func() {
			if s, err := Open(path); err == nil {
				_ = s.Close()
				t.Fatalf("%s: the upgrade went through", name)
			}
		})
		s, err := OpenCurrent(path)
		if err != nil {
			t.Fatalf("%s: the database is not at the schema it had: %v", name, err)
		}
		var n int
		must(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name IN ('fine', 'extra')`).Scan(&n))
		if n != 0 {
			t.Fatalf("%s: part of the failed upgrade stayed", name)
		}
		must(t, s.Close())
	}
}
