// Package cachesrv is the cache's side that runs on the server: the entries
// kept as files under <data>/cache with a row each, the tokens a build's
// stages reach them with, and the endpoints a stage's kipp cache command
// talks to. The side that runs in a stage is package cache.
package cachesrv

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ndthuan/kipp/internal/cache"
	"github.com/ndthuan/kipp/internal/store"
)

// IdleDays is how long an entry may go unrestored before the daily sweep
// removes it.
const IdleDays = 7

// Errors a save ends with, which the endpoint turns into a status.
var (
	ErrTooLarge = errors.New("the entry is over the most an entry may be")
	ErrBadEntry = errors.New("the body is not a zstd archive")
)

// Options are what the service needs.
type Options struct {
	// Dir holds the entries, as <scope>/<namespace>/<key>.tar.zst.
	Dir   string
	Store *store.Store
	// EntryMax is the most one entry may be, in bytes; zero is 10 GiB. It
	// never exceeds Max.
	EntryMax int64
	// Max is the most every entry comes to, in bytes; past it the least
	// recently restored go. Zero is no limit.
	Max int64
	Now func() time.Time
	Log *slog.Logger
}

// Grant is what a build's token reaches: the scopes its stages named, and
// the namespace the build writes (it reads that one and default).
type Grant struct {
	Build     int64
	Scopes    []string
	Namespace cache.Namespace
}

// namespaces are the ones a grant reads, its own first.
func (g Grant) namespaces() []cache.Namespace {
	if g.Namespace == cache.Default {
		return []cache.Namespace{cache.Default}
	}
	return []cache.Namespace{g.Namespace, cache.Default}
}

// Service keeps the entries and the tokens.
type Service struct {
	o Options

	mu      sync.Mutex
	tokens  map[string]Grant   // by the SHA-256 of the token
	byBuild map[int64][]string // a build's token hashes
	// trimMu is held while entries are removed to fit the limits: two
	// would count the same entries twice.
	trimMu sync.Mutex
}

// New is a service over opts.
func New(o Options) *Service {
	if o.EntryMax <= 0 || o.EntryMax > cache.EntryMax {
		o.EntryMax = cache.EntryMax
	}
	if o.Max > 0 && o.EntryMax > o.Max {
		o.EntryMax = o.Max
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return &Service{o: o, tokens: map[string]Grant{}, byBuild: map[int64][]string{}}
}

func hashOf(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Mint is a new token for the grant. It is held in memory only: it dies
// with Revoke of its build, and with the server, whose builds die with it.
func (s *Service) Mint(g Grant) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the system's random source failing is not a state to run on
	}
	token := cache.TokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	h := hashOf(token)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[h] = g
	s.byBuild[g.Build] = append(s.byBuild[g.Build], h)
	return token
}

// Revoke ends every token of the build.
func (s *Service) Revoke(build int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range s.byBuild[build] {
		delete(s.tokens, h)
	}
	delete(s.byBuild, build)
}

func (s *Service) grantOf(token string) (Grant, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.tokens[hashOf(token)]
	return g, ok
}

func (s *Service) file(scope string, ns cache.Namespace, key string) string {
	return filepath.Join(s.o.Dir, scope, string(ns), key+".tar.zst")
}

// find is the entry a lookup gives, or store.ErrNotFound: the exact key in
// the build's namespaces, else the newest of each prefix in order. An entry
// whose file is gone is no entry, and its row goes.
func (s *Service) find(ctx context.Context, g Grant, scope, key string, prefixes []string) (*cache.Found, error) {
	nss := g.namespaces()
	f, err := s.firstLive(ctx, nss, true, func(ns cache.Namespace) (store.CacheEntry, error) {
		return s.o.Store.FindCacheExact(ctx, scope, string(ns), key)
	})
	if !errors.Is(err, store.ErrNotFound) {
		return f, err
	}
	for _, p := range prefixes {
		f, err := s.firstLive(ctx, nss, false, func(ns cache.Namespace) (store.CacheEntry, error) {
			return s.o.Store.FindCachePrefix(ctx, scope, string(ns), p)
		})
		if !errors.Is(err, store.ErrNotFound) {
			return f, err
		}
	}
	return nil, store.ErrNotFound
}

// firstLive is what get finds in the first of the namespaces that has an
// entry whose file is there.
func (s *Service) firstLive(ctx context.Context, nss []cache.Namespace, exact bool, get func(ns cache.Namespace) (store.CacheEntry, error)) (*cache.Found, error) {
	for _, ns := range nss {
		for range 5 { // a stale row is removed and the next best looked for
			e, err := get(ns)
			if errors.Is(err, store.ErrNotFound) {
				break
			}
			if err != nil {
				return nil, err
			}
			live, err := s.live(ctx, e)
			if err != nil {
				return nil, err
			}
			if live {
				return &cache.Found{ID: e.ID, Key: e.Key, Exact: exact, Size: e.Size}, nil
			}
		}
	}
	return nil, store.ErrNotFound
}

// live reports whether the file of the entry is there; if not, the entry's
// row goes.
func (s *Service) live(ctx context.Context, e store.CacheEntry) (bool, error) {
	if _, err := os.Stat(s.file(e.Scope, cache.Namespace(e.Namespace), e.Key)); err == nil {
		return true, nil
	}
	s.o.Log.Warn("a cache entry's file is gone; its row goes", "scope", e.Scope, "key", e.Key)
	return false, s.o.Store.DeleteCacheEntry(ctx, e.ID)
}

// open is the file of an entry the grant may read, with its size, after
// recording that it was restored.
func (s *Service) open(ctx context.Context, g Grant, scope string, id int64) (*os.File, int64, error) {
	e, err := s.o.Store.CacheEntry(ctx, id)
	if err != nil {
		return nil, 0, err
	}
	if e.Scope != scope || !hasNamespace(g, e.Namespace) {
		return nil, 0, store.ErrNotFound
	}
	f, err := os.Open(s.file(e.Scope, cache.Namespace(e.Namespace), e.Key))
	if errors.Is(err, fs.ErrNotExist) {
		_ = s.o.Store.DeleteCacheEntry(ctx, e.ID)
		return nil, 0, store.ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	if err := s.o.Store.TouchCacheEntry(ctx, e.ID, s.o.Now()); err != nil {
		s.o.Log.Warn("recording that a cache entry was restored", "key", e.Key, "err", err)
	}
	return f, fi.Size(), nil
}

func hasNamespace(g Grant, ns string) bool {
	for _, n := range g.namespaces() {
		if string(n) == ns {
			return true
		}
	}
	return false
}

// Save keeps r as the entry of key in the build's namespace. It reports
// false, keeping nothing, when the key is already there: a key is never
// overwritten, and of two saves of one key at once the first to be linked
// wins. The entry is a whole file or none: it is written aside and linked
// into place.
func (s *Service) Save(ctx context.Context, g Grant, scope, key string, r io.Reader) (bool, error) {
	dir := filepath.Join(s.o.Dir, scope, string(g.Namespace))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return false, err
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	n, err := io.Copy(tmp, io.LimitReader(r, s.o.EntryMax+1))
	switch {
	case err != nil:
		return false, err
	case n > s.o.EntryMax:
		return false, ErrTooLarge
	case !zstdMagic(tmp):
		return false, ErrBadEntry
	}
	if err := tmp.Sync(); err != nil {
		return false, err
	}
	final := s.file(scope, g.Namespace, key)
	err = os.Link(tmp.Name(), final)
	if errors.Is(err, fs.ErrExist) {
		return false, s.adopt(ctx, g, scope, key, final)
	}
	if err != nil {
		return false, err
	}
	e := store.CacheEntry{Scope: scope, Namespace: string(g.Namespace), Key: key, Size: n, SavedAt: s.o.Now(), SavedByBuild: g.Build}
	if _, err := s.o.Store.PutCacheEntry(ctx, &e); err != nil {
		_ = os.Remove(final)
		return false, err
	}
	s.afterSave(ctx)
	return true, nil
}

// adopt gives a file that is there without a row (a crash between the link
// and the row) the row it lacks, so that the key is found.
func (s *Service) adopt(ctx context.Context, g Grant, scope, key, file string) error {
	_, err := s.o.Store.FindCacheExact(ctx, scope, string(g.Namespace), key)
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	fi, err := os.Stat(file)
	if err != nil {
		return err
	}
	e := store.CacheEntry{Scope: scope, Namespace: string(g.Namespace), Key: key, Size: fi.Size(), SavedAt: s.o.Now(), SavedByBuild: g.Build}
	_, err = s.o.Store.PutCacheEntry(ctx, &e)
	return err
}

// zstdMagic reports whether f starts as a zstd frame does: a body that does
// not is not an entry, and would be a miss at every restore.
func zstdMagic(f *os.File) bool {
	var m [4]byte
	_, err := f.ReadAt(m[:], 0)
	return err == nil && m == [4]byte{0x28, 0xb5, 0x2f, 0xfd}
}

func (s *Service) afterSave(ctx context.Context) {
	if s.o.Max <= 0 {
		return
	}
	if _, err := s.Trim(ctx, s.o.Max, 0); err != nil {
		s.o.Log.Error("removing cache entries over --cache-max", "err", err)
	}
}

// Sweep is the daily retention: the entries not restored for IdleDays go,
// and then the least recently restored while all are over the limit.
func (s *Service) Sweep(ctx context.Context) (int, error) {
	return s.Trim(ctx, s.o.Max, IdleDays*24*time.Hour)
}

// Trim removes the entries idle for longer than idle (zero: none), then the
// least recently restored until the rest come to max bytes (zero: no limit),
// each a row of the audit, and is how many it removed. An entry never
// restored counts from when it was saved.
func (s *Service) Trim(ctx context.Context, max int64, idle time.Duration) (int, error) {
	s.trimMu.Lock()
	defer s.trimMu.Unlock()
	entries, err := s.o.Store.CacheEntries(ctx)
	if err != nil {
		return 0, err
	}
	now := s.o.Now()
	var total int64
	for _, e := range entries {
		total += e.Size
	}
	n := 0
	for _, e := range entries {
		used := e.LastRestoredAt
		if used.IsZero() {
			used = e.SavedAt
		}
		var why string
		switch {
		case idle > 0 && now.Sub(used) > idle:
			why = fmt.Sprintf("not restored for more than %d days", int(idle.Hours()/24))
		case max > 0 && total > max:
			why = fmt.Sprintf("the least recently restored while the cache was over --cache-max of %d", max)
		default:
			continue
		}
		if err := s.remove(ctx, e, "kipp", why, now); err != nil {
			return n, err
		}
		total -= e.Size
		n++
	}
	return n, nil
}

// Remove removes one entry by hand, as actor: its file, its row and a row
// of the audit. An entry a build is restoring at that moment still has the
// file open and finishes the restore.
func (s *Service) Remove(ctx context.Context, id int64, actor string) error {
	s.trimMu.Lock()
	defer s.trimMu.Unlock()
	e, err := s.o.Store.CacheEntry(ctx, id)
	if err != nil {
		return err
	}
	return s.remove(ctx, e, actor, "removed by hand", s.o.Now())
}

// RemoveScope removes every entry of scope by hand, as actor, and is how
// many it removed.
func (s *Service) RemoveScope(ctx context.Context, scope, actor string) (int, error) {
	s.trimMu.Lock()
	defer s.trimMu.Unlock()
	entries, err := s.o.Store.CacheEntries(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if e.Scope != scope {
			continue
		}
		if err := s.remove(ctx, e, actor, "removed by hand with the rest of its scope", s.o.Now()); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func (s *Service) remove(ctx context.Context, e store.CacheEntry, actor, why string, now time.Time) error {
	if err := os.Remove(s.file(e.Scope, cache.Namespace(e.Namespace), e.Key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := s.o.Store.DeleteCacheEntry(ctx, e.ID); err != nil {
		return err
	}
	return s.o.Store.AppendAudit(ctx, store.Audit{At: now, Actor: actor, Action: "cache.remove",
		Target: e.Scope + "/" + e.Namespace + "/" + e.Key, Detail: fmt.Sprintf("%d bytes, %s", e.Size, why)})
}
