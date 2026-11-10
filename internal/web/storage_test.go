package web

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/storage"
	"github.com/ndthuan/kipp/internal/store"
)

// withStorage gives the site the storage of its data directory; what it
// would run apart runs in place.
func withStorage(d *api.Deps) {
	disp := d.Dispatch
	state := func(ctx context.Context) (storage.Counts, error) {
		q, err := disp.QuietState(ctx)
		return storage.Counts{Quiet: q.On, Running: q.Running, Queued: q.Queued}, err
	}
	data := filepath.Dir(d.Store.Path())
	d.Storage = storage.New(storage.Options{Store: d.Store, Data: data, Snapshot: disp.Snapshot,
		SetQuiet: func(ctx context.Context, on bool) (storage.Counts, error) {
			if _, err := disp.SetQuiet(ctx, on); err != nil {
				return storage.Counts{}, err
			}
			return state(ctx)
		},
		Counts:   state,
		Space:    func(string) (uint64, uint64, bool) { return 100 << 30, 1 << 40, true },
		SameDisk: func(_, _ string) bool { return true },
		TempDir:  data,
		Go:       func(f func()) { f() },
	})
}

func gone(t *testing.T, e *env, job string) {
	t.Helper()
	b := &store.Build{Job: job, Status: store.Success, Trigger: "cli"}
	must(t, e.st.CreateBuild(context.Background(), b))
}

func TestTheStoragePageIsForAnAdminOnlyAndInTheAdminMenu(t *testing.T) {
	e := site(t, withStorage)
	code, _ := get(t, e.login(t, "bob"), e.srv.URL+"/admin/storage")
	eq(t, "a user", code, http.StatusForbidden)
	_, page := get(t, e.login(t, "admin"), e.srv.URL+"/jobs/smoke")
	contains(t, page, `<a href="/admin/storage">Storage</a>`)
}

func TestTheStoragePageShowsTheDataDirectoryTheTablesTheJobsAndWhatToChange(t *testing.T) {
	e := site(t, withStorage)
	gone(t, e, "old-job")
	must(t, os.WriteFile(filepath.Join(e.dir, "db.sqlite.before-reset"), []byte("a copy"), 0o600))
	admin := e.login(t, "admin")
	code, page := get(t, admin, e.srv.URL+"/admin/storage")
	eq(t, "page", code, http.StatusOK)
	selfContained(t, page)
	contains(t, page, "<h1>Storage</h1>", "Data directory", "Keys and small files", "<code>db.sqlite.before-reset</code>",
		"kipp did not make this file", "<code>old-job</code>", "job file gone", `href="/admin/storage/jobs/old-job/remove"`,
		"index of <code>builds</code>", "page by page", "Compact database", "No self-check has run yet.")
}

func TestTheCompactButtonCompactsAndThePageSaysHowItWent(t *testing.T) {
	e := site(t, withStorage)
	admin := e.login(t, "admin")
	res, _ := e.form(t, admin, "/admin/storage/compact", url.Values{})
	eq(t, "start", res.StatusCode, http.StatusSeeOther)
	_, page := get(t, admin, e.srv.URL+"/admin/storage?done=compact-started")
	contains(t, page, "The compact has started.", "Compacted by admin", "Incremental auto_vacuum is on", "Quiet is off.")
	lacks(t, page, `hx-trigger="every 3s"`)
}

func TestRemoveNowAsksFirstThenRemovesAGoneJobsData(t *testing.T) {
	e := site(t, withStorage)
	gone(t, e, "old-job")
	admin := e.login(t, "admin")
	code, page := get(t, admin, e.srv.URL+"/admin/storage/jobs/old-job/remove")
	eq(t, "confirm", code, http.StatusOK)
	contains(t, page, "Remove the data of old-job?", "go now and for good", "Remove now")
	code, _ = get(t, admin, e.srv.URL+"/admin/storage/jobs/smoke/remove")
	eq(t, "a job with a file", code, http.StatusNotFound)
	res, _ := e.form(t, admin, "/admin/storage/jobs/old-job/remove", url.Values{})
	eq(t, "remove", res.StatusCode, http.StatusSeeOther)
	if left, _ := e.st.Builds(context.Background(), "old-job", 10); len(left) != 0 {
		t.Fatalf("%d builds left", len(left))
	}
	res, _ = e.form(t, admin, "/admin/storage/jobs/old-job/remove", url.Values{})
	eq(t, "again", res.StatusCode, http.StatusNotFound)
}

func TestAFailedSelfCheckIsABannerOnEveryAdminPageAndOnNoOneElses(t *testing.T) {
	e := site(t, func(d *api.Deps) {
		data := filepath.Dir(d.Store.Path())
		must(t, os.WriteFile(filepath.Join(data, storage.CheckFile), []byte(`{"at":"2026-11-10T03:31:00Z","ok":false,"problems":["*** in database main ***\nPage 7: btreeInitPage() returns error code 11"],"error":""}`), 0o600))
		withStorage(d)
	})
	admin := e.login(t, "admin")
	for _, path := range []string{"/", "/jobs/smoke", "/admin/storage"} {
		_, page := get(t, admin, e.srv.URL+path)
		contains(t, page, "The database's self-check failed at", "btreeInitPage", "kipp does not restore a backup by itself", "#restoring-the-database-by-hand")
	}
	_, page := get(t, e.login(t, "bob"), e.srv.URL+"/jobs/smoke")
	lacks(t, page, "self-check failed")
}
