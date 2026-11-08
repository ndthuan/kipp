package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/upgrade/state"
)

var t0 = time.Date(2026, 11, 3, 10, 15, 0, 0, time.UTC)

// seed makes a data directory with a WAL-mode database of n rows and a key.
func seed(t *testing.T, n int) (string, *sql.DB) {
	t.Helper()
	data := t.TempDir()
	db, err := openDB(filepath.Join(data, DBFile))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, q := range []string{`PRAGMA journal_mode=WAL`, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for i := range n {
		if _, err := db.Exec(`INSERT INTO t (v) VALUES (?)`, fmt.Sprint("row", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(data, KeyFile), []byte("key-bytes-0123"), 0o600); err != nil {
		t.Fatal(err)
	}
	return data, db
}

func readIn(t *testing.T, dir, name string) string {
	t.Helper()
	f, err := openIn(dir, name, os.O_RDONLY)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func count(t *testing.T, path string) int {
	t.Helper()
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var verdict string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&verdict); err != nil || verdict != "ok" {
		t.Fatalf("integrity_check of %s = %q, %v", path, verdict, err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestBackupTakenWhileTheDatabaseIsWrittenIsConsistentAndHoldsTheSameKey(t *testing.T) {
	data, db := seed(t, 50)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = db.Exec(`INSERT INTO t (v) VALUES ('more')`)
			}
		}
	})
	name, err := Take(context.Background(), data, "v1.0.0", t0)
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if name != "backup-v1.0.0-20261103T101500Z" {
		t.Fatalf("name = %q", name)
	}
	dir := filepath.Join(Dir(data), name)
	if n := count(t, filepath.Join(dir, DBFile)); n < 50 {
		t.Fatalf("the backup holds %d rows, want at least the 50 written before it", n)
	}
	got := readIn(t, dir, KeyFile)
	if got != "key-bytes-0123" {
		t.Fatalf("key in backup = %q", got)
	}
}

func TestBackupDirectoryIs0700AndItsFiles0600(t *testing.T) {
	data, _ := seed(t, 1)
	name, err := Take(context.Background(), data, "v1.0.0", t0)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(Dir(data), name)
	for path, want := range map[string]os.FileMode{
		dir:                         0o700,
		filepath.Join(dir, DBFile):  0o600,
		filepath.Join(dir, KeyFile): 0o600,
	} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", filepath.Base(path), got, want)
		}
	}
}

func TestBackupWithoutAKeyStillHoldsTheDatabase(t *testing.T) {
	data, _ := seed(t, 2)
	if err := os.Remove(filepath.Join(data, KeyFile)); err != nil {
		t.Fatal(err)
	}
	name, err := Take(context.Background(), data, "v1.0.0", t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(Dir(data), name, KeyFile)); err == nil {
		t.Fatal("a key appeared out of nowhere")
	}
}

func TestFailedBackupLeavesNoDirectoryBehind(t *testing.T) {
	data := t.TempDir()
	if _, err := Take(context.Background(), data, "v1.0.0", t0); err == nil {
		t.Fatal("a backup of a data directory with no database succeeded")
	}
	des, _ := os.ReadDir(Dir(data))
	if len(des) != 0 {
		t.Fatalf("left behind: %v", des)
	}
}

func TestRestoreReturnsTheExactDatabaseAndKeyAndRemovesStaleWalAndShm(t *testing.T) {
	data, db := seed(t, 10)
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	name, err := Take(context.Background(), data, "v1.0.0", t0)
	if err != nil {
		t.Fatal(err)
	}
	want := readIn(t, filepath.Join(Dir(data), name), DBFile)
	for i := range 5 {
		_, _ = db.Exec(`INSERT INTO t (v) VALUES (?)`, fmt.Sprint("later", i))
	}
	if _, err := os.Stat(filepath.Join(data, DBFile+"-wal")); err != nil {
		t.Fatalf("the test needs a live write-ahead log: %v", err)
	}
	_ = db.Close() // the server is stopped
	// Whatever the stopped server left must go, even a log of its own.
	for _, s := range []string{"-wal", "-shm"} {
		if err := os.WriteFile(filepath.Join(data, DBFile+s), []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(data, KeyFile), []byte("a newer key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Restore(data, name); err != nil {
		t.Fatal(err)
	}
	if got := readIn(t, data, DBFile); got != want {
		t.Fatal("the database is not the backup's bytes")
	}
	if k := readIn(t, data, KeyFile); k != "key-bytes-0123" {
		t.Fatalf("key = %q", k)
	}
	if n := count(t, filepath.Join(data, DBFile)); n != 10 {
		t.Fatalf("restored database has %d rows, want 10", n)
	}
	checkRestoreLeftNoStrays(t, data)
}

func checkRestoreLeftNoStrays(t *testing.T, data string) {
	t.Helper()
	for _, s := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(filepath.Join(data, DBFile+s)); err == nil {
			t.Errorf("%s survived the restore", s)
		}
	}
	if fi, _ := os.Stat(filepath.Join(data, KeyFile)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("key mode = %o", fi.Mode().Perm())
	}
	if leftovers, _ := filepath.Glob(filepath.Join(data, ".*")); len(leftovers) != 0 {
		t.Fatalf("temporary files left: %v", leftovers)
	}
}

func TestRestoreRefusesANameThatIsNotABackupAndAMissingBackup(t *testing.T) {
	data, _ := seed(t, 1)
	for _, n := range []string{"../x", "backup-v1.0.0-20261103T101500Z/../..", "", "backup-v1-bad"} {
		if err := Restore(data, n); err == nil {
			t.Errorf("Restore(%q) succeeded", n)
		}
	}
	if err := Restore(data, Name("v1.0.0", t0)); err == nil {
		t.Error("restoring a backup that is not there succeeded")
	}
	if n := count(t, filepath.Join(data, DBFile)); n != 1 {
		t.Fatalf("a refused restore changed the database: %d rows", n)
	}
}

func TestPreflightRefusesWithTheShortfall(t *testing.T) {
	data, _ := seed(t, 200)
	need := Need(data)
	extra := uint64(30 << 20)
	err := Preflight(data, extra, func(string) (uint64, bool) { return 10 << 20, true })
	var se *SpaceError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want a SpaceError", err)
	}
	if se.Need != need+extra || se.Free != 10<<20 {
		t.Fatalf("need %d free %d", se.Need, se.Free)
	}
	if !strings.Contains(err.Error(), "short") || !strings.Contains(err.Error(), "MiB") {
		t.Fatalf("message does not say how much is missing: %v", err)
	}
	if err := Preflight(data, extra, func(string) (uint64, bool) { return need + extra, true }); err != nil {
		t.Fatalf("exactly enough was refused: %v", err)
	}
	if err := Preflight(data, extra, func(string) (uint64, bool) { return 0, false }); err != nil {
		t.Fatalf("unknown free space was refused: %v", err)
	}
}

func TestNameOfAVersionWithOddCharactersIsUnknown(t *testing.T) {
	if got := Name("../v1", t0); got != "backup-unknown-20261103T101500Z" {
		t.Fatalf("got %q", got)
	}
}

// fake makes a backup directory of the given age without a database.
func fake(t *testing.T, data string, taken time.Time, succeededAt time.Time) string {
	t.Helper()
	name := Name("v1.0.0", taken)
	if err := os.MkdirAll(filepath.Join(Dir(data), name), 0o700); err != nil {
		t.Fatal(err)
	}
	if !succeededAt.IsZero() {
		if err := MarkSucceeded(data, name, succeededAt); err != nil {
			t.Fatal(err)
		}
	}
	return name
}

func exists(data, name string) bool {
	_, err := os.Stat(filepath.Join(Dir(data), name))
	return err == nil
}

func TestCleanRemovesOnlyExpiredSuccessfulBackups(t *testing.T) {
	data := t.TempDir()
	now := t0.Add(30 * 24 * time.Hour)
	old := fake(t, data, now.Add(-10*24*time.Hour), now.Add(-9*24*time.Hour))
	fresh := fake(t, data, now.Add(-3*24*time.Hour), now.Add(-2*24*time.Hour))
	edge := fake(t, data, now.Add(-9*24*time.Hour), now.Add(-Retention+time.Minute))
	gone, err := Clean(data, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(gone) != 1 || gone[0] != old {
		t.Fatalf("removed %v, want only %s", gone, old)
	}
	if exists(data, old) || !exists(data, fresh) || !exists(data, edge) {
		t.Fatal("wrong backups remain")
	}
}

func TestCleanKeepsAFailedBackupUntilALaterUpgradeSucceeds(t *testing.T) {
	data := t.TempDir()
	now := t0.Add(2 * 24 * time.Hour)
	failed := fake(t, data, now.Add(-36*time.Hour), time.Time{})
	if _, err := Clean(data, now); err != nil || !exists(data, failed) {
		t.Fatalf("a failed backup with nothing after it was removed (err %v)", err)
	}
	ok := fake(t, data, now.Add(-time.Hour), now)
	if _, err := Clean(data, now); err != nil {
		t.Fatal(err)
	}
	if exists(data, failed) || !exists(data, ok) {
		t.Fatal("the failed backup should go once a later one succeeded, and the new one stay")
	}
}

func TestCleanNeverRemovesTheBackupAPendingRecordNames(t *testing.T) {
	data := t.TempDir()
	now := t0.Add(30 * 24 * time.Hour)
	held := fake(t, data, now.Add(-20*24*time.Hour), now.Add(-19*24*time.Hour))
	if err := state.WritePending(data, state.Pending{Kind: state.KindUpgrade, Version: "v1.0.1", Backup: held, From: "v1.0.0"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Clean(data, now); err != nil || !exists(data, held) {
		t.Fatalf("the pending backup was removed (err %v)", err)
	}
}

func TestCleanIgnoresOtherFilesAndAMissingDirectory(t *testing.T) {
	data := t.TempDir()
	if gone, err := Clean(data, t0); err != nil || len(gone) != 0 {
		t.Fatalf("empty data dir: %v %v", gone, err)
	}
	if err := state.WriteStatus(data, state.Status{State: state.StateDone}); err != nil {
		t.Fatal(err)
	}
	if _, err := Clean(data, t0.Add(1000*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if s, _ := state.ReadStatus(data); s == nil {
		t.Fatal("status.json was removed")
	}
}

func TestMarkSucceededWritesTheTimeAndRefusesANameThatIsNotABackup(t *testing.T) {
	data := t.TempDir()
	name := fake(t, data, t0, time.Time{})
	if err := MarkSucceeded(data, name, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := readIn(t, filepath.Join(Dir(data), name), markerFile); !strings.HasPrefix(got, "2026-11-03T11:15:00Z") {
		t.Fatalf("marker %q", got)
	}
	if err := MarkSucceeded(data, "../x", t0); err == nil {
		t.Fatal("a name outside the upgrade directory was marked")
	}
	if err := MarkSucceeded(data, Name("v9.9.9", t0), t0); err == nil {
		t.Fatal("a backup that is not there was marked")
	}
}

func TestBackupOfADataDirectoryWithoutADatabaseFailsAndLeavesNothing(t *testing.T) {
	data := t.TempDir()
	if _, err := Take(context.Background(), data, "v1.0.0", t0); err == nil {
		t.Fatal("a backup of nothing succeeded")
	}
	if des, _ := os.ReadDir(Dir(data)); len(des) != 0 {
		t.Fatalf("left %v", des)
	}
}

func TestRestoreDoesNotFollowALinkOutOfTheBackupAndKeepsTheLiveFiles(t *testing.T) {
	data, db := seed(t, 3)
	name, err := Take(context.Background(), data, "v1.0.0", t0)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	outside := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(outside, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(Dir(data), name, KeyFile)
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, key); err != nil {
		t.Fatal(err)
	}
	liveKey := readIn(t, data, KeyFile)
	if err := Restore(data, name); err == nil {
		t.Fatal("restored through a link that leaves the backup")
	}
	if got := readIn(t, data, KeyFile); got != liveKey {
		t.Fatal("the live key changed")
	}
}

func TestRestoreIntoADirectoryItCannotWriteFailsAndSaysWhichFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	data, db := seed(t, 2)
	name, err := Take(context.Background(), data, "v1.0.0", t0)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if err := os.Chmod(data, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(data, 0o700) }()
	err = Restore(data, name)
	if err == nil || !strings.Contains(err.Error(), "restoring the") {
		t.Fatalf("%v", err)
	}
}
