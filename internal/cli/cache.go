package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ndthuan/kipp/internal/cachesrv"
	"github.com/ndthuan/kipp/internal/store"
)

const cacheAdminUsage = `cache ls --data <dir>
       kipp cache rm <id>... --data <dir>
       kipp cache rm --scope <scope> --data <dir>

ls lists the keyed caches the server keeps, the least recently restored first. rm removes the
entries with those ids (ls shows them), or every entry of a scope; each is a row of the audit.`

func cmdCacheAdmin(args []string, stdout, stderr io.Writer) int {
	fs := newFlags(cacheAdminUsage, stderr)
	data := fs.String("data", "", "the server's data `directory` (required)")
	scope := fs.String("scope", "", "remove every entry of this `scope`")
	positional, err := parseArgs(fs, args[1:])
	if err != nil {
		return exitFor(err)
	}
	var do action
	switch {
	case args[0] == "ls" && len(positional) == 0 && *scope == "":
		do = func(ctx context.Context, st *store.Store) error { return cacheLs(ctx, st, stdout) }
	case args[0] == "rm" && *scope != "" && len(positional) == 0:
		do = func(ctx context.Context, st *store.Store) error { return cacheRmScope(ctx, st, *data, *scope, stdout) }
	case args[0] == "rm" && *scope == "" && len(positional) > 0:
		do = func(ctx context.Context, st *store.Store) error { return cacheRm(ctx, st, *data, positional, stdout) }
	}
	return onStore("cache", *data, fs.Usage, stderr, do)
}

func cacheLs(ctx context.Context, st *store.Store, stdout io.Writer) error {
	es, err := st.CacheEntries(ctx)
	if err != nil {
		return err
	}
	for _, e := range es {
		restored := "never restored"
		if !e.LastRestoredAt.IsZero() {
			restored = "restored " + e.LastRestoredAt.UTC().Format("2006-01-02 15:04")
		}
		say(stdout, "%d\t%s\t%s\t%s\t%d bytes\tsaved %s\t%s", e.ID, e.Scope, e.Namespace, e.Key, e.Size, e.SavedAt.UTC().Format("2006-01-02 15:04"), restored)
	}
	return nil
}

// cacheService serves the data directory's caches for the removal of
// entries; the audit rows it writes are the cli's.
func cacheService(st *store.Store, data string) *cachesrv.Service {
	return cachesrv.New(cachesrv.Options{Dir: filepath.Join(data, "cache"), Store: st})
}

func cacheRm(ctx context.Context, st *store.Store, data string, ids []string, stdout io.Writer) error {
	parsed := make([]int64, len(ids))
	for i, s := range ids {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil || id <= 0 {
			return refused(fmt.Sprintf("%q is not an entry id; kipp cache ls shows them", s))
		}
		if _, err := st.CacheEntry(ctx, id); err != nil {
			return fmt.Errorf("no cache entry %d", id)
		}
		parsed[i] = id
	}
	srv := cacheService(st, data)
	for _, id := range parsed {
		if err := srv.Remove(ctx, id, "cli"); err != nil {
			return err
		}
	}
	say(stdout, "removed %d %s", len(parsed), plural(len(parsed), "entry", "entries"))
	return nil
}

func cacheRmScope(ctx context.Context, st *store.Store, data, scope string, stdout io.Writer) error {
	n, err := cacheService(st, data).RemoveScope(ctx, scope, "cli")
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("no entry in scope %s", scope)
	}
	say(stdout, "removed %d %s of scope %s", n, plural(n, "entry", "entries"), strings.TrimSpace(scope))
	return nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
