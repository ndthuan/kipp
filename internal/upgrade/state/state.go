// Package state holds the files an upgrade leaves in the data directory
// and how they are read and written: the versions under <data>/bin/ that
// the launcher chooses between, and the progress under <data>/upgrade/.
// Every write is an atomic rename, so a reader sees the old file or the
// new one and never half of either.
package state

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ndthuan/kipp/internal/update"
)

// Bundled stands, in previous and in a pending record, for the executable
// the server was started as: the image's, Homebrew's or the archive's.
const Bundled = "bundled"

// The kinds of a pending record.
const (
	KindUpgrade  = "upgrade"
	KindRollback = "rollback"
)

// The states of an upgrade in status.json.
const (
	StatePreflight   = "preflight"
	StateDownloading = "downloading"
	StateVerifying   = "verifying"
	StateWaiting     = "waiting"
	StateBackup      = "backup"
	StateSwitching   = "switching"
	StateDone        = "done"
	StateFailed      = "failed"
	StateCancelled   = "cancelled"
)

const (
	binDir     = "bin"
	upgradeDir = "upgrade"

	currentFile     = "current"
	previousFile    = "previous"
	pendingFile     = "pending"
	rejectedFile    = "rejected"
	statusFile      = "status.json"
	lastFailureFile = "last-failure.json"
	migratingFile   = "migrating"
	refusedFile     = "pending.refused"

	binaryPrefix = "kipp-"
)

// BinDir is where the data directory keeps the binaries and the choice
// between them.
func BinDir(data string) string { return filepath.Join(data, binDir) }

// UpgradeDir is where an upgrade in progress keeps its state.
func UpgradeDir(data string) string { return filepath.Join(data, upgradeDir) }

// BinaryPath is where the binary of release version v lives.
func BinaryPath(data, v string) string { return filepath.Join(BinDir(data), binaryPrefix+v) }

// MigratingPath is the file the server holds while its store migrates.
func MigratingPath(data string) string { return filepath.Join(UpgradeDir(data), migratingFile) }

// BinaryVersion is the version a file of <data>/bin/ is the binary of, and
// false for any other name.
func BinaryVersion(name string) (string, bool) {
	v, ok := strings.CutPrefix(name, binaryPrefix)
	return v, ok && update.Release(v)
}

// Pending is an upgrade, or a return to an earlier version, that the next
// start is to carry out.
type Pending struct {
	Kind    string `json:"kind"`
	Version string `json:"version"`
	Backup  string `json:"backup,omitempty"`
	From    string `json:"from,omitempty"`
}

// Status is the progress of an upgrade, as the engine and the launcher
// write it.
type Status struct {
	State     string    `json:"state"`
	Version   string    `json:"version,omitempty"`
	From      string    `json:"from,omitempty"`
	Backup    string    `json:"backup,omitempty"`
	Running   int       `json:"running"`
	Queued    int       `json:"queued"`
	Rerun     int       `json:"rerun,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Failure is why the launcher went back to an earlier version.
type Failure struct {
	Version string `json:"version"`
	From    string `json:"from,omitempty"`
	Reason  string `json:"reason"`
	// LogTail is the end of what the failed version printed.
	LogTail string    `json:"log_tail,omitempty"`
	At      time.Time `json:"at"`
}

// validVersion reports whether v may be named in current, previous or
// a record: a release version, and for the last two also "bundled".
func validVersion(v string, allowBundled bool) bool {
	return update.Release(v) || (allowBundled && v == Bundled)
}

// ReadCurrent is the version run by default; "" when there is none. A
// file naming anything but a release version counts as none, because a
// name read from a file is joined into a path.
func ReadCurrent(data string) (string, error) {
	return readVersion(filepath.Join(BinDir(data), currentFile), false)
}

// WriteCurrent makes v the version run by default.
func WriteCurrent(data, v string) error {
	return writeVersion(filepath.Join(BinDir(data), currentFile), v, false)
}

// ClearCurrent makes the executable itself the version run by default;
// none is not an error.
func ClearCurrent(data string) error {
	return removeIfThere(filepath.Join(BinDir(data), currentFile))
}

// ReadPrevious is the version before the current one, or Bundled; "" when
// there is none.
func ReadPrevious(data string) (string, error) {
	return readVersion(filepath.Join(BinDir(data), previousFile), true)
}

// WritePrevious records the version before the current one.
func WritePrevious(data, v string) error {
	return writeVersion(filepath.Join(BinDir(data), previousFile), v, true)
}

// ClearPrevious forgets the version before the current one; none is not an
// error.
func ClearPrevious(data string) error {
	return removeIfThere(filepath.Join(BinDir(data), previousFile))
}

func readVersion(path string, allowBundled bool) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(b))
	if !validVersion(v, allowBundled) {
		return "", nil
	}
	return v, nil
}

func writeVersion(path, v string, allowBundled bool) error {
	if !validVersion(v, allowBundled) {
		return fmt.Errorf("%q is not a version a state file may name", v)
	}
	return writeAtomic(path, []byte(v+"\n"))
}

// ReadPending is the record waiting for the next start; nil when there is
// none or it names nothing usable.
func ReadPending(data string) (*Pending, error) {
	var p Pending
	found, err := readJSON(filepath.Join(BinDir(data), pendingFile), &p)
	if err != nil || !found || !p.valid() {
		return nil, err
	}
	return &p, nil
}

func (p *Pending) valid() bool {
	if p.Kind != KindUpgrade && p.Kind != KindRollback {
		return false
	}
	return validVersion(p.Version, p.Kind == KindRollback) &&
		(p.From == "" || validVersion(p.From, true))
}

// WritePending records what the next start is to carry out.
func WritePending(data string, p Pending) error {
	if !p.valid() {
		return fmt.Errorf("a pending record of kind %q for %q is not valid", p.Kind, p.Version)
	}
	return writeJSON(filepath.Join(BinDir(data), pendingFile), p)
}

// ClearPending removes the pending record; none is not an error.
func ClearPending(data string) error {
	return removeIfThere(filepath.Join(BinDir(data), pendingFile))
}

// RefusePending sets the pending record aside as pending.refused, for an
// admin to look at, so that the next start does not meet it again.
func RefusePending(data string) error {
	return os.Rename(filepath.Join(BinDir(data), pendingFile), filepath.Join(BinDir(data), refusedFile))
}

// MarkMigrating writes the file that says the store is being migrated and
// returns the function that removes it. Failing to write it is reported to
// the caller, which may carry on: the file only lengthens a deadline.
func MarkMigrating(data string) (done func(), err error) {
	path := MigratingPath(data)
	if err := writeAtomic(path, []byte("1\n")); err != nil {
		return func() {}, err
	}
	return func() { _ = os.Remove(path) }, nil
}

// ReadRejected is the versions the launcher must never pick, one per line
// in the file. A line that is not a release version is skipped.
func ReadRejected(data string) ([]string, error) {
	f, err := os.Open(filepath.Join(BinDir(data), rejectedFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v := strings.TrimSpace(sc.Text()); update.Release(v) && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out, sc.Err()
}

// Reject adds v to the versions the launcher never picks.
func Reject(data, v string) error {
	if !update.Release(v) {
		return fmt.Errorf("%q is not a release version", v)
	}
	have, err := ReadRejected(data)
	if err != nil {
		return err
	}
	if slices.Contains(have, v) {
		return nil
	}
	var buf bytes.Buffer
	for _, h := range append(have, v) {
		buf.WriteString(h + "\n")
	}
	return writeAtomic(filepath.Join(BinDir(data), rejectedFile), buf.Bytes())
}

// Unreject takes v off the versions the launcher never picks; none is not
// an error.
func Unreject(data, v string) error {
	have, err := ReadRejected(data)
	if err != nil || !slices.Contains(have, v) {
		return err
	}
	var buf bytes.Buffer
	for _, h := range have {
		if h != v {
			buf.WriteString(h + "\n")
		}
	}
	return writeAtomic(filepath.Join(BinDir(data), rejectedFile), buf.Bytes())
}

// ReadStatus is the progress of the last upgrade; nil when there is none.
func ReadStatus(data string) (*Status, error) {
	var s Status
	found, err := readJSON(filepath.Join(UpgradeDir(data), statusFile), &s)
	if err != nil || !found {
		return nil, err
	}
	return &s, nil
}

// WriteStatus replaces the progress of the upgrade.
func WriteStatus(data string, s Status) error {
	return writeJSON(filepath.Join(UpgradeDir(data), statusFile), s)
}

// ReadFailure is why the last upgrade was undone; nil when none was.
func ReadFailure(data string) (*Failure, error) {
	var f Failure
	found, err := readJSON(filepath.Join(UpgradeDir(data), lastFailureFile), &f)
	if err != nil || !found {
		return nil, err
	}
	return &f, nil
}

// WriteFailure records why an upgrade was undone.
func WriteFailure(data string, f Failure) error {
	return writeJSON(filepath.Join(UpgradeDir(data), lastFailureFile), f)
}

func readJSON(path string, into any) (bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, into); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	return true, nil
}

func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeAtomic(path, append(b, '\n'))
}

func removeIfThere(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// writeAtomic writes a file beside its place and renames it there, making
// the directory when it is not there yet.
func writeAtomic(path string, b []byte) (err error) {
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
		}
	}()
	if _, err = f.Write(b); err != nil {
		return err
	}
	if err = f.Chmod(0o600); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
