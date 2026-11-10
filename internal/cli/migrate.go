package cli

import (
	"errors"
	"io"
	"io/fs"
	"os"
)

// cmdMigrate brings the database of a data directory no server holds up to
// this kipp's schema. An upgrade's dry run has the new release migrate a
// copy with it, to find a migration that fails before the real one does.
func cmdMigrate(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	flags := newFlags("migrate --data <dir>", stderr)
	data := flags.String("data", "", "the data directory whose database to migrate; no server may run on it")
	positional, err := parseArgs(flags, args)
	if err != nil {
		return exitFor(err)
	}
	if len(positional) != 0 {
		flags.Usage()
		return 2
	}
	if *data == "" {
		return fail(stderr, "migrate", refused("--data is required"))
	}
	if err := migrate(*data, stdout); err != nil {
		return fail(stderr, "migrate", err)
	}
	return 0
}

func migrate(data string, stdout io.Writer) error {
	lock, err := lockData(data)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if err := hasDatabase(data); err != nil {
		return err
	}
	st, err := openStore(data, true)
	if err != nil {
		return err
	}
	up := st.Upgrade()
	if err := st.Close(); err != nil {
		return err
	}
	if up.From == up.To {
		say(stdout, "ok: the database is at schema %d already", up.To)
		return nil
	}
	say(stdout, "ok: the database went from schema %d to %d", up.From, up.To)
	return nil
}

// hasDatabase refuses a data directory with no database, which openStore
// with create would make: a mistyped --data is not a new kipp.
func hasDatabase(data string) error {
	f, err := openInDir(data, dbFile, os.O_RDONLY)
	if errors.Is(err, fs.ErrNotExist) {
		return refused(data + " holds no kipp database")
	}
	if err != nil {
		return err
	}
	return f.Close()
}
