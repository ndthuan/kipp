package cli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/store"
)

func cachedData(t *testing.T) string {
	t.Helper()
	data := newData(t)
	st := openDB(t, data)
	defer func() { _ = st.Close() }()
	for _, k := range []struct{ scope, key string }{{"site", "a"}, {"site", "b"}, {"api", "c"}} {
		e := &store.CacheEntry{Scope: k.scope, Namespace: "default", Key: k.key, Size: 7, SavedAt: time.Now()}
		if _, err := st.PutCacheEntry(context.Background(), e); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(data, "cache", k.scope, "default")
		must(t, os.MkdirAll(dir, 0o750))
		must(t, os.WriteFile(filepath.Join(dir, k.key+".tar.zst"), []byte("x"), 0o600))
	}
	return data
}

func TestCacheLsListsAndRmRemovesEntriesWithTheirAuditRows(t *testing.T) {
	data := cachedData(t)
	out := ok(t, "", "cache", "ls", "--data", data)
	contains(t, out, "site\tdefault\ta\t7 bytes", "site\tdefault\tb", "api\tdefault\tc", "never restored")
	st := openDB(t, data)
	es, err := st.CacheEntries(context.Background())
	must(t, err)
	_ = st.Close()
	id := es[0].ID
	contains(t, ok(t, "", "cache", "rm", itoa(id), "--data", data), "removed 1 entry")
	if _, err := os.Stat(filepath.Join(data, "cache", es[0].Scope, "default", es[0].Key+".tar.zst")); !os.IsNotExist(err) {
		t.Fatalf("the file is still there: %v", err)
	}
	contains(t, ok(t, "", "cache", "rm", "--scope", "site", "--data", data), "of scope site")
	out = ok(t, "", "cache", "ls", "--data", data)
	if strings.Count(out, "\n") > 1 {
		t.Fatalf("%s", out)
	}
	st = openDB(t, data)
	defer func() { _ = st.Close() }()
	audit, err := st.AuditBefore(context.Background(), 0, 10)
	must(t, err)
	n := 0
	for _, a := range audit {
		if a.Action == "cache.remove" && a.Actor == "cli" {
			n++
		}
	}
	if n != 3-len(strings.Split(strings.TrimSpace(out), "\n")) {
		t.Fatalf("%d audit rows, ls says %q", n, out)
	}
}

func TestCacheRmRefusesWhatItCannotRemove(t *testing.T) {
	data := cachedData(t)
	for args, want := range map[string]string{
		"cache rm abc --data D":          "is not an entry id",
		"cache rm 9999 --data D":         "no cache entry 9999",
		"cache rm --scope nope --data D": "no entry in scope nope",
		"cache rm --data D":              "usage: kipp cache",
		"cache ls extra --data D":        "usage: kipp cache",
	} {
		code, _, errOut := kipp("", strings.Fields(strings.ReplaceAll(args, " D", " "+data))...)
		if code == 0 {
			t.Fatalf("%s: exit 0", args)
		}
		contains(t, errOut, want)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
