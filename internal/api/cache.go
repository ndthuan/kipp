package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/ndthuan/kipp/internal/store"
)

// Caches are the keyed caches the server keeps, the least recently restored
// first, for an admin; a token may read them.
func (s *Service) Caches(ctx context.Context, u *store.User) ([]store.CacheEntry, error) {
	if !u.Admin {
		return nil, ErrForbidden
	}
	return s.deps.Store.CacheEntries(ctx)
}

// CachesPage is up to limit of the caches Caches lists (100 unless limit says
// otherwise, 500 at most) after the position (at, id) of the entry a page
// ended on, which Caches's order puts them in: a zero at starts at the first.
// More says there are entries beyond the page.
func (s *Service) CachesPage(ctx context.Context, u *store.User, at time.Time, id int64, limit int) (page []store.CacheEntry, more bool, err error) {
	if !u.Admin {
		return nil, false, ErrForbidden
	}
	n := rows(limit, defaultCache)
	found, err := s.deps.Store.CacheEntriesAfter(ctx, at, id, n+1)
	if err != nil {
		return nil, false, err
	}
	page, more = trim(found, n)
	return page, more, nil
}

// RemoveCacheEntry removes one cache entry by hand.
func (s *Service) RemoveCacheEntry(ctx context.Context, u *store.User, sess *store.Session, id int64) error {
	if err := administers(u, sess); err != nil {
		return err
	}
	if s.deps.Cache == nil {
		return fmt.Errorf("%w: this server keeps no cache", ErrNotFound)
	}
	if err := s.deps.Cache.Remove(ctx, id, u.Name); err != nil {
		return missing(err)
	}
	return nil
}

// RemoveCacheScope removes every entry of a scope by hand and is how many
// it removed. A scope with none is not found.
func (s *Service) RemoveCacheScope(ctx context.Context, u *store.User, sess *store.Session, scope string) (int, error) {
	if err := administers(u, sess); err != nil {
		return 0, err
	}
	if s.deps.Cache == nil {
		return 0, fmt.Errorf("%w: this server keeps no cache", ErrNotFound)
	}
	n, err := s.deps.Cache.RemoveScope(ctx, scope, u.Name)
	if err == nil && n == 0 {
		err = errors.Join(ErrNotFound, fmt.Errorf("no entry in scope %s", scope))
	}
	return n, err
}

// cacheEntries are the page of entries a request asks for.
func (a *API) cacheEntries(r *http.Request, u *store.User) (es []store.CacheEntry, more bool, err error) {
	limit, at, err := pageArgs(r, "cache", 2)
	if err != nil {
		return nil, false, err
	}
	var from time.Time
	var id int64
	if len(at) == 2 {
		from, id = time.Unix(0, at[0]), at[1]
	}
	return a.svc.CachesPage(r.Context(), u, from, id, limit)
}

func (a *API) cacheList(w http.ResponseWriter, r *http.Request, u *store.User) {
	es, more, err := a.cacheEntries(r, u)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	next := ""
	if more {
		last := es[len(es)-1]
		next = cursorFor("cache", last.OrderedAt().UnixNano(), last.ID)
	}
	ids := make([]int64, 0, len(es))
	for _, e := range es {
		ids = append(ids, e.SavedByBuild)
	}
	builds, err := a.svc.BuildsByID(r.Context(), ids)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := make([]cacheV1, 0, len(es))
	for _, e := range es {
		out = append(out, toCacheV1(e, builds))
	}
	writeList(w, out, next)
}
