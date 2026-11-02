package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/store"
)

func putCache(t *testing.T, e *env, scope, key string, size int64) int64 {
	t.Helper()
	ent := &store.CacheEntry{Scope: scope, Namespace: "default", Key: key, Size: size, SavedAt: time.Now()}
	if _, err := e.st.PutCacheEntry(context.Background(), ent); err != nil {
		t.Fatal(err)
	}
	return ent.ID
}

func TestTheCachePageIsForAnAdminOnly(t *testing.T) {
	e := site(t)
	code, _ := get(t, e.login(t, "bob"), e.srv.URL+"/admin/cache")
	eq(t, "a user", code, http.StatusForbidden)
	_, page := get(t, e.login(t, "admin"), e.srv.URL+"/jobs/smoke")
	contains(t, page, `<a href="/admin/cache">Cache</a>`)
}

func TestTheCachePageListsEntriesByScope(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	code, page := get(t, admin, e.srv.URL+"/admin/cache")
	eq(t, "empty", code, http.StatusOK)
	selfContained(t, page)
	contains(t, page, "No cache is kept yet.")
	putCache(t, e, "site", "npm-1", 2048)
	putCache(t, e, "site", "npm-2", 1024)
	putCache(t, e, "api", "go-1", 10)
	_, page = get(t, admin, e.srv.URL+"/admin/cache")
	contains(t, page, "npm-1", "npm-2", "go-1", "2 entries, 3.0 KiB", "1 entry, 10 B", "/admin/cache/scopes/site/delete")
}

func TestTheCachePageRemovesAnEntryAndAScopeAsAnAdminAfterSudo(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	one := putCache(t, e, "site", "npm-1", 100)
	putCache(t, e, "site", "npm-2", 100)
	putCache(t, e, "api", "go-1", 100)

	code, page := get(t, admin, e.srv.URL+"/admin/cache/entries/"+strconv.FormatInt(one, 10)+"/delete")
	eq(t, "confirm", code, http.StatusOK)
	contains(t, page, "Remove the cache entry npm-1?")
	path := "/admin/cache/entries/" + strconv.FormatInt(one, 10) + "/delete"
	res, _ := e.form(t, admin, path, url.Values{})
	eq(t, "without sudo", res.StatusCode, http.StatusForbidden)
	e.sudo(t, admin)
	res, _ = e.form(t, admin, path, url.Values{})
	eq(t, "remove an entry", res.StatusCode, http.StatusSeeOther)
	if _, err := e.st.CacheEntry(context.Background(), one); err == nil {
		t.Fatal("the entry is still there")
	}
	res, _ = e.form(t, admin, path, url.Values{})
	eq(t, "again", res.StatusCode, http.StatusNotFound)

	res, _ = e.form(t, admin, "/admin/cache/scopes/site/delete", url.Values{})
	eq(t, "clear a scope", res.StatusCode, http.StatusSeeOther)
	left, err := e.st.CacheEntries(context.Background())
	must(t, err)
	if len(left) != 1 || left[0].Scope != "api" {
		t.Fatalf("%+v", left)
	}
	res, _ = e.form(t, admin, "/admin/cache/scopes/site/delete", url.Values{})
	eq(t, "an empty scope", res.StatusCode, http.StatusNotFound)

	audit, err := e.st.AuditBefore(context.Background(), 0, 10)
	must(t, err)
	n := 0
	for _, a := range audit {
		if a.Action == "cache.remove" && a.Actor == "admin" {
			n++
		}
	}
	eq(t, "audit rows by the admin", n, 2)
}
