package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/dispatch"
	"github.com/ndthuan/kipp/internal/index"
	"github.com/ndthuan/kipp/internal/store"
)

// scanSite is a site whose scanner reads a GitHub with nothing in it.
func scanSite(t *testing.T, file string) (*env, *index.Scanner) {
	t.Helper()
	gh := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(gh.Close)
	var ix *index.Index
	var scanner *index.Scanner
	e := site(t, func(d *api.Deps) {
		ix = index.New(d.Store, d.Dispatch.SetSnapshot)
		sc := index.NewScanner(context.Background(), ix, index.ScannerOptions{
			API:     gh.URL,
			Token:   func(context.Context, string) (string, error) { return "scan-token", nil },
			Trigger: func(context.Context, dispatch.TriggerRequest) (*store.Build, error) { return &store.Build{}, nil },
		})
		t.Cleanup(sc.Wait)
		d.Scanner, scanner = sc, sc
	})
	e.add(t, map[string]string{"site.yml": file})
	must(t, ix.SetBase(context.Background(), e.d.Snapshot()))
	return e, scanner
}

func TestAParentWithNoTokenSaysNoScanNoStatuses(t *testing.T) {
	e, _ := scanSite(t, "name: site\nmultibranch: {repo: acme/site}\n")
	_, page := get(t, e.login(t, "admin"), e.srv.URL+"/jobs/site")
	contains(t, page, "No scan, no statuses")
	lacks(t, page, "Scan now")
}

func TestScanNowOnAParentsPageStartsAScanAndComesBack(t *testing.T) {
	e, sc := scanSite(t, "name: site\nmultibranch: {repo: acme/site, token: GH_TOKEN, scan: 0}\n")
	c := e.login(t, "admin")
	_, page := get(t, c, e.srv.URL+"/jobs/site")
	contains(t, page, "Scan now", "Only on request", "Not scanned yet")
	res := post(t, c, e.srv.URL+"/jobs/site/scan", url.Values{"csrf": {csrfOf(t, page)}})
	eq(t, "the scan", res.StatusCode, http.StatusSeeOther)
	eq(t, "back to", res.Header.Get("Location"), "/jobs/site")
	sc.Wait()
	_, page = get(t, c, e.srv.URL+"/jobs/site")
	contains(t, page, "Last scan", "The last scan failed", "404")
	lacks(t, page, "scan-token")
}
