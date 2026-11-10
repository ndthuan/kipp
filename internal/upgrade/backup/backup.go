// Package backup keeps a copy of the database and its key before an
// upgrade, checks that the data directory has room for one, puts the copy
// back when the new version does not come up, and clears the old ones.
// Everything stays inside the data directory.
package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite" // the driver, no cgo

	"github.com/ndthuan/kipp/internal/upgrade/state"
)

// The files of a data directory that a backup holds.
const (
	DBFile  = "db.sqlite"
	KeyFile = "secret.key"
)

// Retention is how long a backup of a successful upgrade is kept.
const Retention = 7 * 24 * time.Hour

const (
	namePrefix = "backup-"
	timeLayout = "20060102T150405Z"
	// markerFile in a backup says that the upgrade it was taken for
	// succeeded, and when.
	markerFile = "succeeded"
)

var (
	nameRE    = regexp.MustCompile(`^backup-[A-Za-z0-9._-]+-(\d{8}T\d{6}Z)$`)
	versionRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// Dir is where the backups of a data directory live.
func Dir(data string) string { return state.UpgradeDir(data) }

// Name is what a backup of a server running version from, taken at now, is
// called. A version with a character a directory name should not have
// reads as "unknown".
func Name(from string, now time.Time) string {
	if !versionRE.MatchString(from) {
		from = "unknown"
	}
	return namePrefix + from + "-" + now.UTC().Format(timeLayout)
}

// Take copies the database and the key of a data directory into a new
// backup and returns its name. The database goes through SQLite itself, so
// the copy is whole and consistent while the server writes to it; a plain
// copy of the file would miss what its write-ahead log holds.
func Take(ctx context.Context, data, from string, now time.Time) (name string, err error) {
	name = Name(from, now)
	dir := filepath.Join(Dir(data), name)
	if err = os.MkdirAll(Dir(data), 0o700); err != nil {
		return "", err
	}
	if err = os.Mkdir(dir, 0o700); err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
			name = ""
		}
	}()
	if err = CopyDatabase(ctx, data, dir); err != nil {
		return "", fmt.Errorf("backing up the database: %w", err)
	}
	if err = copyFile(data, dir, KeyFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("backing up the key: %w", err)
	}
	return name, syncDir(dir)
}

// CopyDatabase writes a consistent copy of the live database of srcDir into
// dstDir with VACUUM INTO, which wants an empty file that is already there,
// and then checks the copy.
func CopyDatabase(ctx context.Context, srcDir, dstDir string) error {
	src, dst := filepath.Join(srcDir, DBFile), filepath.Join(dstDir, DBFile)
	if _, err := os.Stat(src); err != nil {
		return err
	}
	f, err := openIn(dstDir, DBFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := vacuumInto(ctx, src, dst); err != nil {
		return err
	}
	if err := CheckDatabase(ctx, dst); err != nil {
		return err
	}
	return syncFile(dstDir, DBFile)
}

func openDB(path string) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=busy_timeout(5000)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func vacuumInto(ctx context.Context, src, dst string) error {
	db, err := openDB(src)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `VACUUM INTO ?`, dst)
	return errors.Join(err, db.Close())
}

// CheckDatabase opens a copy and has SQLite check it.
func CheckDatabase(ctx context.Context, path string) error {
	db, err := openDB(path)
	if err != nil {
		return err
	}
	var verdict string
	err = db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&verdict)
	if err == nil && verdict != "ok" {
		err = fmt.Errorf("the copy fails its integrity check: %s", verdict)
	}
	return errors.Join(err, db.Close())
}

// SpaceError says the disk of the data directory is too small for an
// upgrade.
type SpaceError struct {
	Dir        string
	Need, Free uint64
}

func (e *SpaceError) Error() string {
	return fmt.Sprintf("not enough free space for the upgrade in %s: it needs %s and %s is free, %s short",
		e.Dir, mib(e.Need), mib(e.Free), mib(e.Need-e.Free))
}

func mib(n uint64) string {
	return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
}

func size(fi os.FileInfo) uint64 {
	if n := fi.Size(); n > 0 {
		return uint64(n)
	}
	return 0
}

// Need is what a backup of the data directory takes: the database with its
// write-ahead log, and the key.
func Need(data string) uint64 {
	var n uint64
	for _, name := range []string{DBFile, DBFile + "-wal", KeyFile} {
		if fi, err := os.Stat(filepath.Join(data, name)); err == nil {
			n += size(fi)
		}
	}
	return n
}

// Preflight checks that the filesystem of the data directory has room for
// the backup and for extra bytes more (the release archive and the
// unpacked binary, sized by the caller), and says how much is missing when
// it has not. free tells the free bytes of a directory's disk, false when
// it cannot be told, and then the upgrade goes on.
func Preflight(data string, extra uint64, free func(string) (uint64, bool)) error {
	have, known := free(data)
	if !known {
		return nil
	}
	need := Need(data) + extra
	if have < need {
		return &SpaceError{Dir: data, Need: need, Free: have}
	}
	return nil
}

// valid reports whether name is one Name could have made, so that it is
// safe to join into a path.
func valid(name string) bool { return nameRE.MatchString(name) }

// Restore puts the database and the key of a backup back over the live
// ones, each by a write to a temporary name, a sync and a rename, and
// removes the write-ahead log and shared-memory files of the database it
// replaces, which belong to it and would corrupt the restored one. The
// server must be stopped: nothing here keeps a running one from writing.
func Restore(data, name string) error {
	if !valid(name) {
		return fmt.Errorf("%q is not a backup name", name)
	}
	dir := filepath.Join(Dir(data), name)
	if _, err := os.Stat(filepath.Join(dir, DBFile)); err != nil {
		return fmt.Errorf("backup %s: %w", name, err)
	}
	if _, err := os.Stat(filepath.Join(dir, KeyFile)); err == nil {
		if err := replaceFile(dir, data, KeyFile); err != nil {
			return fmt.Errorf("restoring the key: %w", err)
		}
	}
	// The stale log goes before the new file arrives: a crash between the
	// two leaves the old database without its log, never the new one with it.
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(filepath.Join(data, DBFile+suffix)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := replaceFile(dir, data, DBFile); err != nil {
		return fmt.Errorf("restoring the database: %w", err)
	}
	return nil
}

// replaceFile copies a file of srcDir beside the same-named one of dstDir,
// syncs it and renames it over that one, the mode 0600.
func replaceFile(srcDir, dstDir, name string) error {
	tmp, err := os.CreateTemp(dstDir, "."+name+".*")
	if err != nil {
		return err
	}
	if err := fillAndRename(tmp, srcDir, name, filepath.Join(dstDir, name)); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return syncDir(dstDir)
}

func fillAndRename(tmp *os.File, srcDir, name, dst string) error {
	in, err := openIn(srcDir, name, os.O_RDONLY)
	if err != nil {
		_ = tmp.Close()
		return err
	}
	defer func() { _ = in.Close() }()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// openIn opens a file of a directory through the directory as a root, so
// a link in there cannot lead to a file outside it. A file it creates is
// for its owner alone.
func openIn(dir, name string, flag int) (*os.File, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return root.OpenFile(name, flag, 0o600)
}

// copyFile copies a file of srcDir to a new one of dstDir, the mode 0600.
func copyFile(srcDir, dstDir, name string) error {
	in, err := openIn(srcDir, name, os.O_RDONLY)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := openIn(dstDir, name, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func syncFile(dir, name string) error {
	f, err := openIn(dir, name, os.O_RDWR)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func syncDir(dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	d, err := root.Open(".")
	if err != nil {
		return errors.Join(err, root.Close())
	}
	return errors.Join(d.Sync(), d.Close(), root.Close())
}

// MarkSucceeded records that the upgrade the backup was taken for came up
// and stayed, which starts its retention.
func MarkSucceeded(data, name string, at time.Time) error {
	if !valid(name) {
		return fmt.Errorf("%q is not a backup name", name)
	}
	return os.WriteFile(filepath.Join(Dir(data), name, markerFile), []byte(at.UTC().Format(time.RFC3339)+"\n"), 0o600)
}

// entry is a backup found in the upgrade directory.
type entry struct {
	name    string
	taken   time.Time
	success time.Time // zero while the upgrade has not succeeded
}

func list(data string) ([]entry, error) {
	des, err := os.ReadDir(Dir(data))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []entry
	for _, de := range des {
		m := nameRE.FindStringSubmatch(de.Name())
		if m == nil || !de.IsDir() {
			continue
		}
		taken, err := time.Parse(timeLayout, m[1])
		if err != nil {
			continue
		}
		e := entry{name: de.Name(), taken: taken}
		if b, err := os.ReadFile(filepath.Join(Dir(data), de.Name(), markerFile)); err == nil {
			e.success, _ = time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
		}
		out = append(out, e)
	}
	return out, nil
}

// Clean removes the backups that are no longer needed and returns their
// names: one of a successful upgrade a week after it, and one of an upgrade
// that failed once a later upgrade has succeeded. The backup a pending
// record names is never removed, because the next start still needs it.
func Clean(data string, now time.Time) ([]string, error) {
	all, err := list(data)
	if err != nil {
		return nil, err
	}
	keep := ""
	if p, err := state.ReadPending(data); err != nil {
		return nil, err
	} else if p != nil {
		keep = p.Backup
	}
	var newestOK time.Time
	for _, e := range all {
		if !e.success.IsZero() && e.taken.After(newestOK) {
			newestOK = e.taken
		}
	}
	var gone []string
	for _, e := range all {
		if e.name == keep || !expired(e, newestOK, now) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(Dir(data), e.name)); err != nil {
			return gone, err
		}
		gone = append(gone, e.name)
	}
	return gone, nil
}

func expired(e entry, newestOK, now time.Time) bool {
	if e.success.IsZero() {
		return e.taken.Before(newestOK)
	}
	return now.Sub(e.success) >= Retention
}

// LastSucceeded is the newest backup of an upgrade that came up, which is
// the one a rollback puts back; false when there is none.
func LastSucceeded(data string) (string, bool) {
	all, err := list(data)
	if err != nil {
		return "", false
	}
	best := -1
	for i, e := range all {
		if !e.success.IsZero() && (best < 0 || e.taken.After(all[best].taken)) {
			best = i
		}
	}
	if best < 0 {
		return "", false
	}
	return all[best].name, true
}
