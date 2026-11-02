package web

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/store"
)

func (s *Site) cacheRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/cache", s.withUser(s.cachePage))
	mux.HandleFunc("GET /admin/cache/entries/{id}/delete", s.withUser(s.confirmDeleteCache))
	mux.HandleFunc("POST /admin/cache/entries/{id}/delete", s.withUser(s.deleteCache))
	mux.HandleFunc("GET /admin/cache/scopes/{scope}/delete", s.withUser(s.confirmClearScope))
	mux.HandleFunc("POST /admin/cache/scopes/{scope}/delete", s.withUser(s.clearScope))
}

// cacheScope is the entries of one scope and what they come to.
type cacheScope struct {
	Name    string
	Entries []store.CacheEntry
	Size    int64
}

type cacheData struct {
	Scopes []cacheScope
	Total  int64
	Notice string
}

func (s *Site) cachePage(w http.ResponseWriter, r *http.Request, u *store.User) {
	es, err := s.api.Service().Caches(r.Context(), u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d := cacheData{Notice: noticeOf(r)}
	at := map[string]int{}
	for _, e := range es {
		i, ok := at[e.Scope]
		if !ok {
			i = len(d.Scopes)
			at[e.Scope] = i
			d.Scopes = append(d.Scopes, cacheScope{Name: e.Scope})
		}
		d.Scopes[i].Entries = append(d.Scopes[i].Entries, e)
		d.Scopes[i].Size += e.Size
		d.Total += e.Size
	}
	s.render(w, r, "cache", http.StatusOK, page{Title: "Cache", Here: "/admin/cache", Data: d})
}

func cacheID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, api.ErrNotFound
	}
	return id, nil
}

func (s *Site) confirmDeleteCache(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, err := cacheID(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	es, err := s.api.Service().Caches(r.Context(), u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for _, e := range es {
		if e.ID == id {
			s.confirm(w, r, "/admin/cache", confirmData{
				Heading: "Remove the cache entry " + e.Key + "?",
				Lines:   []string{"It is " + byteSize(e.Size) + " in scope " + e.Scope + ".", "The next build that asks for this key finds none, and builds what it holds again."},
				Button:  "Remove the entry", Back: "/admin/cache",
			})
			return
		}
	}
	s.fail(w, r, api.ErrNotFound)
}

func (s *Site) deleteCache(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, err := cacheID(r)
	if err == nil {
		err = s.api.Service().RemoveCacheEntry(r.Context(), u, api.SessionFrom(r.Context()), id)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	done(w, r, "/admin/cache", "cache-gone")
}

func (s *Site) confirmClearScope(w http.ResponseWriter, r *http.Request, u *store.User) {
	scope := r.PathValue("scope")
	es, err := s.api.Service().Caches(r.Context(), u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	n, size := 0, int64(0)
	for _, e := range es {
		if e.Scope == scope {
			n++
			size += e.Size
		}
	}
	if n == 0 {
		s.fail(w, r, api.ErrNotFound)
		return
	}
	s.confirm(w, r, "/admin/cache", confirmData{
		Heading: "Remove every cache entry of the scope " + scope + "?",
		Lines:   []string{strconv.Itoa(n) + " entries, " + byteSize(size) + ".", "The next builds that ask for their keys find none, and build what they hold again."},
		Button:  "Remove them", Back: "/admin/cache",
	})
}

func (s *Site) clearScope(w http.ResponseWriter, r *http.Request, u *store.User) {
	if _, err := s.api.Service().RemoveCacheScope(r.Context(), u, api.SessionFrom(r.Context()), r.PathValue("scope")); err != nil {
		s.fail(w, r, err)
		return
	}
	done(w, r, "/admin/cache", "cache-cleared")
}

// scopePath is the address of a scope's pages under /admin/cache/scopes.
func scopePath(scope string) string { return "/admin/cache/scopes/" + url.PathEscape(scope) }
