package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/dispatch"
	"github.com/ndthuan/kipp/internal/index"
	"github.com/ndthuan/kipp/internal/store"
)

// withScanner gives the API a scanner over a multibranch job, site, that
// names a token, against a GitHub that has nothing; it returns the tokens
// of a user with the grants given on site.
func (e *env) withScanner(t *testing.T, perms ...string) string {
	t.Helper()
	gh := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(gh.Close)
	parent, err := config.ParseJob("site.yml", []byte("name: site\nmultibranch: {repo: acme/site, token: GH_TOKEN, scan: 0}\n"))
	must(t, err)
	snap := e.api.deps.Dispatch.Snapshot().WithJobs(parent)
	ix := index.New(e.st, e.api.deps.Dispatch.SetSnapshot)
	must(t, ix.SetBase(context.Background(), snap))
	sc := index.NewScanner(context.Background(), ix, index.ScannerOptions{
		API:     gh.URL,
		Token:   func(context.Context, string) (string, error) { return "scan-token", nil },
		Trigger: func(context.Context, dispatch.TriggerRequest) (*store.Build, error) { return &store.Build{}, nil },
	})
	t.Cleanup(sc.Wait)
	e.api.deps.Scanner = sc
	return token(t, e.st, "site-"+perms[len(perms)-1], false, map[string][]string{"site": perms})
}

func TestScanNowNeedsTheBuildGrant(t *testing.T) {
	e := newEnv(t)
	viewer := e.withScanner(t, "view")
	e.call(t, http.StatusForbidden, viewer, "POST", "/api/v1/jobs/site/scan", "")
	e.call(t, http.StatusNotFound, e.aide, "POST", "/api/v1/jobs/site/scan", "")
	builder := token(t, e.st, "site-builder", false, map[string][]string{"site": {"view", "build"}})
	e.call(t, http.StatusAccepted, builder, "POST", "/api/v1/jobs/site/scan", "")
	// The scan reads a GitHub with nothing: it ends, and says how.
	e.api.deps.Scanner.Wait()
	d := e.object(t, http.StatusOK, builder, "GET", "/api/v1/jobs/site", "")
	scan, _ := d["scan"].(map[string]any)
	if scan == nil || scan["token"] != true || scan["error"] == "" {
		t.Fatalf("%v", d)
	}
	if _, ok := scan["at"]; !ok {
		t.Fatalf("the scan's end is not said: %v", scan)
	}
}

func TestScanNowOfAJobThatIsNotMultibranchIsRefused(t *testing.T) {
	e := newEnv(t)
	e.withScanner(t, "view")
	// smoke is a job of its own, not a repo: nothing to scan.
	e.call(t, http.StatusNotFound, e.aide, "POST", "/api/v1/jobs/smoke/scan", "")
}
