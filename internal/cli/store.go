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

// The files of a data directory, beside logs/ and workspace/.
const (
	dbFile  = "db.sqlite"
	keyFile = "secret.key" // seals the secrets in the database
	// deviceKeyFile signs the cookies that mark a browser as one that has
	// logged in; without it every browser is a stranger until its next login.
	deviceKeyFile = "device.key"
	lockFile      = "lock"
)

// action is what a user, token or secret command does to the database.
// These commands open it themselves, on the server's machine: the first
// admin and a lost password cannot come through a login.
type action func(ctx context.Context, st *store.Store) error

// openStore opens the database of a data directory. Only serve passes
// create: for every other command a directory with no database is an
// error, not a second, empty kipp behind a mistyped --data. Only serve,
// which holds the directory, brings the schema up to date; every other
// command runs beside a server that may be of the kipp before, so it
// refuses a database at any schema but this kipp's. The file
// holds password and token hashes, so it is made for its owner alone, and
// one that group or others can read is refused as the key file is; SQLite
// gives the files it keeps beside it the same mode.
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

// onStore runs do, the command's action, on the data directory's database.
// A nil do is a command line that names no action.
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

// record appends the audit row of a change the command has made, with the
// actor cli, as the server records its own. A row that cannot be written
// fails the command, saying the change itself was made.
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
