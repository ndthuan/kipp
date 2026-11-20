package web

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/storage"
	"github.com/ndthuan/kipp/internal/store"
)

// withStorage gives the site the storage of its data directory; what it
// would run apart runs in place.
func withStorage(d *api.Deps) { storageAt(filepath.Dir(d.Store.Path()), true)(d) }

// storageAt is withStorage over another data directory; run says whether
// what the storage starts apart runs, and if not a compact stays at its
// start.
func storageAt(data string, run bool) func(*api.Deps) {
	return func(d *api.Deps) { storageOver(d, data, run) }
}

func storageOver(d *api.Deps, data string, run bool) {
	disp := d.Dispatch
	state := func(ctx context.Context) (storage.Counts, error) {
		q, err := disp.QuietState(ctx)
		return storage.Counts{Quiet: q.On, Running: q.Running, Queued: q.Queued}, err
	}
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
		Go: func(f func()) {
			if run {
				f()
			}
		},
	})
}

func gone(t *testing.T, e *env, job string) {
	t.Helper()
	b := &store.Build{Job: job, Status: store.Success, Trigger: "cli"}
	must(t, e.st.CreateBuild(context.Background(), b))
}

func TestTheHousekeepingPageIsForAnAdminOnlyAndInTheAdminMenu(t *testing.T) {
	e := site(t, withStorage)
	code, _ := get(t, e.login(t, "bob"), e.srv.URL+"/admin/housekeeping")
	eq(t, "a user", code, http.StatusForbidden)
	_, page := get(t, e.login(t, "admin"), e.srv.URL+"/jobs/smoke")
	contains(t, page, `<a href="/admin/housekeeping">Housekeeping</a>`)
	lacks(t, page, "/admin/storage", ">Storage<")
	_, page = get(t, e.login(t, "admin"), e.srv.URL+"/admin/housekeeping")
	contains(t, page, `<summary aria-current="true"`, `<a href="/admin/housekeeping" aria-current="page">Housekeeping</a>`)
}

func TestTheHousekeepingPageLoadsEverySectionByItselfAndReadsNothingToDoIt(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "file")
	must(t, os.WriteFile(notADir, []byte("x"), 0o600))
	e := site(t, storageAt(notADir, true))
	code, page := get(t, e.login(t, "admin"), e.srv.URL+"/admin/housekeeping")
	eq(t, "page", code, http.StatusOK)
	selfContained(t, page)
	contains(t, page, "<h1>Housekeeping</h1>")
	for _, name := range []string{"facts", "compact", "recommendations", "parts", "database", "jobs", "self-check", "heal"} {
		contains(t, page, `id="`+name+`" `, `hx-get="/admin/housekeeping/sections/`+name+`" hx-trigger="load" hx-swap="outerHTML"`)
	}
	eq(t, "busy", strings.Count(page, `aria-busy="true"`), 8)
	eq(t, "loading", strings.Count(page, "Loading..."), 8)
	last := -1
	for _, name := range []string{"facts", "compact", "recommendations", "parts", "database", "jobs", "self-check", "heal"} {
		at := strings.Index(page, `id="`+name+`" `)
		if at <= last {
			t.Fatalf("section %s is out of order", name)
		}
		last = at
	}
}

func TestEverySectionOfHousekeepingAnswersOnItsOwnForAnAdminOnly(t *testing.T) {
	e := site(t, withStorage)
	gone(t, e, "old-job")
	must(t, os.WriteFile(filepath.Join(e.dir, "db.sqlite.before-reset"), []byte("a copy"), 0o600))
	admin := e.login(t, "admin")
	want := map[string][]string{
		"facts":           {`<dl id="facts"`, "free of", "free pages"},
		"compact":         {`id="compact"`, "Compact database", `action="/admin/housekeeping/compact"`},
		"recommendations": {`id="recommendations"`, `class="scroll tall"`, "<code>old-job</code>", `href="/admin/housekeeping/jobs/old-job/remove"`},
		"parts":           {`id="parts"`, "Keys and small files", "<code>db.sqlite.before-reset</code>", "kipp did not make this file"},
		"database":        {`id="database"`, "index of <code>builds</code>", "page by page"},
		"jobs":            {`id="jobs"`, "<code>old-job</code>", "job file gone"},
		"self-check":      {`id="self-check"`, "No self-check has run yet."},
	}
	for name, subs := range want {
		code, frag := get(t, admin, e.srv.URL+"/admin/housekeeping/sections/"+name)
		eq(t, name, code, http.StatusOK)
		contains(t, frag, subs...)
		lacks(t, frag, "<html", "<h1>")
		code, _ = get(t, e.login(t, "bob"), e.srv.URL+"/admin/housekeeping/sections/"+name)
		eq(t, name+" as a user", code, http.StatusForbidden)
	}
	code, frag := get(t, admin, e.srv.URL+"/admin/housekeeping/sections/heal")
	eq(t, "heal", code, http.StatusOK)
	eq(t, "no heal, nothing", strings.TrimSpace(frag), "")
	code, _ = get(t, admin, e.srv.URL+"/admin/housekeeping/sections/nothing")
	eq(t, "unknown", code, http.StatusNotFound)
}

func TestASectionThatCannotBeReadSaysSoAloneAndTheOthersAnswer(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "file")
	must(t, os.WriteFile(notADir, []byte("x"), 0o600))
	e := site(t, storageAt(notADir, true))
	admin := e.login(t, "admin")
	for _, name := range []string{"parts", "recommendations"} {
		code, frag := get(t, admin, e.srv.URL+"/admin/housekeeping/sections/"+name)
		eq(t, name, code, http.StatusOK)
		contains(t, frag, `id="`+name+`"`, `class="notice notice-error"`, "This could not be read")
	}
	for _, name := range []string{"facts", "compact", "database", "jobs", "self-check"} {
		code, frag := get(t, admin, e.srv.URL+"/admin/housekeeping/sections/"+name)
		eq(t, name, code, http.StatusOK)
		lacks(t, frag, "notice-error", "This could not be read")
	}
}

func TestTheOldStorageAddressesMoveToHousekeeping(t *testing.T) {
	e := site(t, withStorage)
	admin := e.login(t, "admin")
	for from, to := range map[string]string{
		"/admin/storage":                      "/admin/housekeeping",
		"/admin/storage?done=compact-started": "/admin/housekeeping?done=compact-started",
		"/admin/storage/jobs/x/remove":        "/admin/housekeeping/jobs/x/remove",
		"/admin/storage/jobs/a%20b/remove":    "/admin/housekeeping/jobs/a%20b/remove",
	} {
		req, err := http.NewRequest(http.MethodGet, e.srv.URL+from, nil)
		must(t, err)
		res, _ := do(t, admin, req)
		eq(t, from, res.StatusCode, http.StatusMovedPermanently)
		eq(t, from+" to", res.Header.Get("Location"), to)
	}
}

func TestAFailedSelfCheckBannerLinksHousekeeping(t *testing.T) {
	e := site(t, func(d *api.Deps) {
		must(t, os.WriteFile(filepath.Join(filepath.Dir(d.Store.Path()), storage.CheckFile), []byte(`{"at":"2026-11-10T03:31:00Z","ok":false,"problems":["x"],"error":""}`), 0o600))
		withStorage(d)
	})
	_, page := get(t, e.login(t, "admin"), e.srv.URL+"/jobs/smoke")
	contains(t, page, `href="/admin/housekeeping#self-check"`)
}

func TestTheCompactSectionPollsItselfWhileACompactRuns(t *testing.T) {
	e := site(t, storageAt(t.TempDir(), false))
	admin := e.login(t, "admin")
	res, _ := e.form(t, admin, "/admin/housekeeping/compact", url.Values{})
	eq(t, "start", res.StatusCode, http.StatusSeeOther)
	_, frag := get(t, admin, e.srv.URL+"/admin/housekeeping/sections/compact")
	contains(t, frag, `hx-get="/admin/housekeeping/sections/compact"`, `hx-trigger="every 3s"`, "The compact holds quiet")
	lacks(t, frag, "<form")
	res, page := e.form(t, admin, "/admin/housekeeping/compact", url.Values{})
	eq(t, "second", res.StatusCode, http.StatusConflict)
	contains(t, page, "The compact did not start:", "<h1>Housekeeping</h1>")
}

func TestTheCompactButtonCompactsAndThePageSaysHowItWent(t *testing.T) {
	e := site(t, withStorage)
	admin := e.login(t, "admin")
	res, _ := e.form(t, admin, "/admin/housekeeping/compact", url.Values{})
	eq(t, "start", res.StatusCode, http.StatusSeeOther)
	_, page := get(t, admin, e.srv.URL+"/admin/housekeeping?done=compact-started")
	contains(t, page, "The compact has started.")
	_, frag := get(t, admin, e.srv.URL+"/admin/housekeeping/sections/compact")
	contains(t, frag, "Compacted by admin", "Incremental auto_vacuum is on", "Quiet is off.")
	lacks(t, frag, `hx-trigger="every 3s"`)
}

func TestRemoveNowAsksFirstThenRemovesAGoneJobsData(t *testing.T) {
	e := site(t, withStorage)
	gone(t, e, "old-job")
	admin := e.login(t, "admin")
	code, page := get(t, admin, e.srv.URL+"/admin/housekeeping/jobs/old-job/remove")
	eq(t, "confirm", code, http.StatusOK)
	contains(t, page, "Remove the data of old-job?", "go now and for good", "Remove now")
	code, _ = get(t, admin, e.srv.URL+"/admin/housekeeping/jobs/smoke/remove")
	eq(t, "a job with a file", code, http.StatusNotFound)
	res, _ := e.form(t, admin, "/admin/housekeeping/jobs/old-job/remove", url.Values{})
	eq(t, "remove", res.StatusCode, http.StatusSeeOther)
	if left, _ := e.st.Builds(context.Background(), "old-job", 10); len(left) != 0 {
		t.Fatalf("%d builds left", len(left))
	}
	res, _ = e.form(t, admin, "/admin/housekeeping/jobs/old-job/remove", url.Values{})
	eq(t, "again", res.StatusCode, http.StatusNotFound)
}

func TestAFailedSelfCheckIsABannerOnEveryAdminPageAndOnNoOneElses(t *testing.T) {
	e := site(t, func(d *api.Deps) {
		data := filepath.Dir(d.Store.Path())
		must(t, os.WriteFile(filepath.Join(data, storage.CheckFile), []byte(`{"at":"2026-11-10T03:31:00Z","ok":false,"problems":["*** in database main ***\nPage 7: btreeInitPage() returns error code 11"],"error":""}`), 0o600))
		withStorage(d)
	})
	admin := e.login(t, "admin")
	for _, path := range []string{"/", "/jobs/smoke", "/admin/housekeeping"} {
		_, page := get(t, admin, e.srv.URL+path)
		contains(t, page, "The database's self-check failed at", "btreeInitPage", "kipp does not restore a backup by itself", "#restoring-the-database-by-hand")
	}
	_, page := get(t, e.login(t, "bob"), e.srv.URL+"/jobs/smoke")
	lacks(t, page, "self-check failed")
}
