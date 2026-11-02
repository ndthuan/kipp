package cachesrv

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/ndthuan/kipp/internal/cache"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/problem"
	"github.com/ndthuan/kipp/internal/route"
	"github.com/ndthuan/kipp/internal/store"
)

// Handler serves /api/v1/cache/<scope>/... with a build's token alone, never a
// session: what it reaches is the scopes the token names, and nothing else
// of the server.
//
//	GET /api/v1/cache/<scope>/lookup?key=K&restore=P&restore=P2 -> cache.Found, or 404
//	GET /api/v1/cache/<scope>/entry/<id>                       -> the archive
//	PUT /api/v1/cache/<scope>/<key>                            -> 201 kept, 200 already there
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	route.Mount(mux, s.Routes())
	mux.Handle(cache.Prefix, problem.Fallback(mux, cache.Prefix))
	return mux
}

// Routes are the calls the cache answers, which a token alone opens.
func (s *Service) Routes() []route.Route {
	return []route.Route{
		{Method: http.MethodGet, Path: cache.Prefix + "{scope}/lookup", Handler: http.HandlerFunc(s.lookup)},
		{Method: http.MethodGet, Path: cache.Prefix + "{scope}/entry/{id}", Handler: http.HandlerFunc(s.entry)},
		{Method: http.MethodPut, Path: cache.Prefix + "{scope}/{key}", Handler: http.HandlerFunc(s.put)},
	}
}

// authorize is the grant of the request's token for the scope in its path,
// or an answer already written: 401 for no token or one that has ended, 403
// for a scope the token does not name.
func (s *Service) authorize(w http.ResponseWriter, r *http.Request) (Grant, string, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	g, valid := s.grantOf(token)
	if !ok || !valid {
		w.Header().Set("WWW-Authenticate", "Bearer")
		problem.Write(w, problem.Unauthorized, "the cache token is missing, or its build has ended")
		return Grant{}, "", false
	}
	scope := r.PathValue("scope")
	if !config.ValidName(scope) || !slices.Contains(g.Scopes, scope) {
		problem.Write(w, problem.Forbidden, "the token does not reach scope "+strconv.Quote(scope))
		return Grant{}, "", false
	}
	return g, scope, true
}

func (s *Service) lookup(w http.ResponseWriter, r *http.Request) {
	g, scope, ok := s.authorize(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	key, prefixes := q.Get("key"), q["restore"]
	if !cache.ValidKey(key) || slices.ContainsFunc(prefixes, func(p string) bool { return !cache.ValidKey(p) }) {
		problem.Write(w, problem.BadInput, "key and restore are 1 to 512 letters, digits and . _ + = -")
		return
	}
	f, err := s.find(r.Context(), g, scope, key, prefixes)
	if errors.Is(err, store.ErrNotFound) {
		problem.Write(w, problem.NotFound, "no such cache entry")
		return
	}
	if err != nil {
		s.fail(w, "looking up a cache entry", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(f)
}

func (s *Service) entry(w http.ResponseWriter, r *http.Request) {
	g, scope, ok := s.authorize(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		problem.Write(w, problem.NotFound, "no such cache entry")
		return
	}
	f, size, err := s.open(r.Context(), g, scope, id)
	if errors.Is(err, store.ErrNotFound) {
		problem.Write(w, problem.NotFound, "no such cache entry")
		return
	}
	if err != nil {
		s.fail(w, "opening a cache entry", err)
		return
	}
	defer func() { _ = f.Close() }()
	w.Header().Set("Content-Type", "application/zstd")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	_, _ = io.Copy(w, f)
}

func (s *Service) put(w http.ResponseWriter, r *http.Request) {
	g, scope, ok := s.authorize(w, r)
	if !ok {
		return
	}
	key := r.PathValue("key")
	if !cache.ValidKey(key) {
		problem.Write(w, problem.BadInput, "the key is 1 to 512 letters, digits and . _ + = -")
		return
	}
	kept, err := s.Save(r.Context(), g, scope, key, r.Body)
	switch {
	case errors.Is(err, ErrTooLarge):
		problem.Write(w, problem.TooLarge, "the entry is over the most an entry may be, "+strconv.FormatInt(s.o.EntryMax, 10)+" bytes")
	case errors.Is(err, ErrBadEntry):
		problem.Write(w, problem.BadInput, err.Error())
	case err != nil:
		s.fail(w, "saving a cache entry", err)
	case kept:
		s.o.Log.Info("cache entry saved", "scope", scope, "namespace", g.Namespace, "key", key, "build", g.Build)
		w.WriteHeader(http.StatusCreated)
	default:
		w.WriteHeader(http.StatusOK)
	}
}

// fail answers a fault of the server's own; the reason stays in its log.
func (s *Service) fail(w http.ResponseWriter, what string, err error) {
	s.o.Log.Error(what, "err", err)
	problem.Write(w, problem.Internal, "the server could not do that; see its log")
}
