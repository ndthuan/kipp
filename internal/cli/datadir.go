package cli

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"syscall"
)

// makeDataDir makes the data directory for its owner alone: it holds the
// logs of every build beside the database and the key. One that is
// already there keeps its mode, which is the operator's to choose, and a
// mode that lets others in is said aloud.
func makeDataDir(dir string, log *slog.Logger) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		log.Warn("the data directory lets group or others read it; chmod 700 it", "dir", dir, "mode", perm.String())
	}
	return nil
}

// openInDir opens a file of the data directory through the directory as a
// root, so a link in there cannot lead kipp to a file outside it. A file
// it creates is for its owner alone.
func openInDir(dir, name string, flag int) (*os.File, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return root.OpenFile(name, flag, 0o600)
}

// lockData takes the data directory for this process, or says that
// another has it. The lock is an flock on an empty file in it: the system
// drops it when the process dies, however it dies, so there is never a
// stale one to clear by hand.
func lockData(dir string) (*os.File, error) {
	f, err := openInDir(dir, lockFile, os.O_RDWR|os.O_CREATE)
	if err != nil {
		return nil, err
	}
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return f, nil
	}
	_ = f.Close()
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return nil, fmt.Errorf("another kipp serve is running on the data directory %s", dir)
	}
	return nil, fmt.Errorf("locking the data directory %s: %w", dir, err)
}
