package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/ndthuan/kipp/internal/store"
)

const (
	dbFile        = "db.sqlite"
	keyFile       = "secret.key"
	deviceKeyFile = "device.key"
)

type action func(ctx context.Context, st *store.Store) error

func openStore(dir string, create bool) (*store.Store, error) {
	path := filepath.Join(dir, dbFile)
	flags := os.O_RDWR
	if create {
		flags |= os.O_CREATE
	}
	f, err := openInDir(dir, dbFile, flags)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s holds no kipp database: start kipp serve --data %s first, which makes it", dir, dir)
	}
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("%s: mode %v lets group or others read the database; chmod 600 it", path, perm)
	}
	if create {
		return store.Open(path)
	}
	return store.OpenCurrent(path)
}

func onStore(cmd, data string, usage func(), stderr io.Writer, do action) int {
	if do == nil {
		usage()
		return 2
	}
	if data == "" {
		return fail(stderr, cmd, refused("--data is required"))
	}
	st, err := openStore(data, false)
	if err != nil {
		return fail(stderr, cmd, err)
	}
	defer func() { _ = st.Close() }()
	if err := do(context.Background(), st); err != nil {
		return fail(stderr, cmd, err)
	}
	return 0
}

func record(ctx context.Context, st *store.Store, action, target, detail string) error {
	err := st.AppendAudit(ctx, store.Audit{At: time.Now(), Actor: "cli", Action: action, Target: target, Detail: detail})
	if err != nil {
		return fmt.Errorf("the change was made, but its audit row was not written: %w", err)
	}
	return nil
}

func findUser(ctx context.Context, st *store.Store, name string) (*store.User, error) {
	u, err := st.UserByName(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("no user called %q", name)
	}
	return u, err
}
