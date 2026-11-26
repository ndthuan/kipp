package cli

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/ndthuan/kipp/internal/upgrade/state"
)

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

func openInDir(dir, name string, flag int) (*os.File, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return root.OpenFile(name, flag, 0o600)
}

func lockData(dir string) (*os.File, error) {
	f, err := state.Lock(dir)
	if errors.Is(err, state.ErrLocked) {
		return nil, fmt.Errorf("another kipp serve is running on the data directory %s", dir)
	}
	if err != nil {
		return nil, fmt.Errorf("locking the data directory %s: %w", dir, err)
	}
	return f, nil
}

func containerOwner(key []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("kipp.server"))
	return hex.EncodeToString(mac.Sum(nil)[:16])
}
