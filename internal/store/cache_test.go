package store

import (
	"errors"
	"testing"
	"time"
)

var cacheT0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func putEntry(t *testing.T, s *Store, key string, at time.Time) CacheEntry {
	t.Helper()
	e := CacheEntry{Scope: "acme", Namespace: "default", Key: key, Size: 10, SavedAt: at}
	ok, err := s.PutCacheEntry(ctx, &e)
	must(t, err)
	if !ok {
		t.Fatalf("acme/default/%s was not new", key)
	}
	return e
}

func TestACacheKeyIsSavedOnceInANamespace(t *testing.T) {
	s := open(t)
	putEntry(t, s, "k1", cacheT0)
	again := CacheEntry{Scope: "acme", Namespace: "default", Key: "k1", Size: 99, SavedAt: cacheT0.Add(time.Hour)}
	ok, err := s.PutCacheEntry(ctx, &again)
	must(t, err)
	if ok {
		t.Fatal("a key already saved was saved again")
	}
	got, err := s.FindCacheExact(ctx, "acme", "default", "k1")
	must(t, err)
	if got.Size != 10 {
		t.Fatalf("size %d, the first save's 10 must stay", got.Size)
	}
	if _, err := s.FindCacheExact(ctx, "acme", "j-other", "k1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another namespace found the key: %v", err)
	}
	if _, err := s.FindCacheExact(ctx, "other", "default", "k1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another scope found the key: %v", err)
	}
}

func TestAPrefixFindsTheNewestEntryThatStartsWithIt(t *testing.T) {
	s := open(t)
	putEntry(t, s, "mod-a", cacheT0)
	newest := putEntry(t, s, "mod-b", cacheT0.Add(time.Hour))
	putEntry(t, s, "other-c", cacheT0.Add(2*time.Hour))
	got, err := s.FindCachePrefix(ctx, "acme", "default", "mod-")
	must(t, err)
	if got.ID != newest.ID {
		t.Fatalf("found %q, want mod-b", got.Key)
	}
	if _, err := s.FindCachePrefix(ctx, "acme", "default", "none-"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a prefix nothing starts with: %v", err)
	}
}

func TestAPrefixWithLikeCharactersMatchesThemLiterally(t *testing.T) {
	s := open(t)
	putEntry(t, s, "abc", cacheT0)
	for _, p := range []string{"%", "_", "a_c"} {
		if _, err := s.FindCachePrefix(ctx, "acme", "default", p); !errors.Is(err, ErrNotFound) {
			t.Fatalf("prefix %q matched: %v", p, err)
		}
	}
}

func TestRestoringAnEntryRecordsWhenAndOrdersTheLeastRecentFirst(t *testing.T) {
	s := open(t)
	a := putEntry(t, s, "a", cacheT0)
	b := putEntry(t, s, "b", cacheT0.Add(time.Hour))
	must(t, s.TouchCacheEntry(ctx, a.ID, cacheT0.Add(5*time.Hour)))
	all, err := s.CacheEntries(ctx)
	must(t, err)
	if len(all) != 2 || all[0].ID != b.ID || all[1].ID != a.ID {
		t.Fatalf("order %+v, want b (saved, never restored) before a (restored later)", all)
	}
	if !all[1].LastRestoredAt.Equal(cacheT0.Add(5 * time.Hour)) {
		t.Fatalf("last restored %v", all[1].LastRestoredAt)
	}
}

func TestADeletedEntryIsGone(t *testing.T) {
	s := open(t)
	e := putEntry(t, s, "a", cacheT0)
	must(t, s.DeleteCacheEntry(ctx, e.ID))
	if _, err := s.CacheEntry(ctx, e.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a deleted entry was read: %v", err)
	}
}
