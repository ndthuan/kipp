package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/update"
	"github.com/ndthuan/kipp/internal/upgrade/backup"
	"github.com/ndthuan/kipp/internal/upgrade/engine"
	et "github.com/ndthuan/kipp/internal/upgrade/engine/enginetest"
	"github.com/ndthuan/kipp/internal/upgrade/state"
)

// upgradeEnv is a server running a release, with an engine that knows a
// newer one and a data directory the tests write the engine's status into.
type upgradeEnv struct {
	*env
	data string
	fake *et.Server
}

func upgradeSite(t *testing.T) *upgradeEnv {
	t.Helper()
	rel := et.NewRelease(t, et.GoodBinary(et.Newer))
	fake := et.NewServer()
	eng, data := et.Engine(t, rel, fake, nil)
	e := site(t, func(d *api.Deps) {
		d.Version, d.Upgrade = et.Running, eng
		d.ServerInfo = func() api.ServerInfo {
			i := serverInfoNow()
			i.Version, i.Origin, i.UpdateCheck = et.Running, "Homebrew", true
			i.Latest, i.NotesURL, i.Upgradable = et.Newer, "https://github.com/ndthuan/kipp/releases/tag/"+et.Newer, true
			return i
		}
	})
	return &upgradeEnv{env: e, data: data, fake: fake}
}

func (u *upgradeEnv) status(t *testing.T, st state.Status) {
	t.Helper()
	st.UpdatedAt = time.Now()
	must(t, state.WriteStatus(u.data, st))
}

func (u *upgradeEnv) server(t *testing.T) string {
	t.Helper()
	code, page := get(t, u.login(t, "admin"), u.srv.URL+"/admin/server")
	eq(t, "server page", code, http.StatusOK)
	return page
}

func TestTheUpgradeBannerIsAnAdminsAndOnlyWhileAReleaseIsOnOffer(t *testing.T) {
	u := upgradeSite(t)
	for _, path := range []string{"/", "/account", "/admin/server"} {
		_, page := get(t, u.login(t, "admin"), u.srv.URL+path)
		contains(t, page, "kipp "+et.Newer+" is available.", `href="/admin/server#upgrade"`, "See and upgrade", "Remind me later", "upgrade.js")
	}
	_, page := get(t, u.login(t, "bob"), u.srv.URL+"/")
	lacks(t, page, "is available", "Remind me later", "upgrade.js", "upgrade-watch")
	_, page = get(t, u.anon(t), u.srv.URL+"/login")
	lacks(t, page, "is available", "upgrade-watch")
}

func TestTheUpgradeBannerIsAbsentWhenNothingIsUpgradable(t *testing.T) {
	rel := et.NewRelease(t, et.GoodBinary(et.Newer))
	eng, _ := et.Engine(t, rel, et.NewServer(), func(o *engine.Options) { o.Checker, o.Version = rel.Checker("v9.0.0"), "v9.0.0" })
	// The server already runs a release newer than the one on offer.
	e := site(t, func(d *api.Deps) { d.Version, d.Upgrade = "v9.0.0", eng })
	_, page := get(t, e.login(t, "admin"), e.srv.URL+"/")
	lacks(t, page, "is available", "Remind me later")

	// A development build has nothing to upgrade and no card.
	e = site(t, func(d *api.Deps) { d.Upgrade = eng })
	_, page = get(t, e.login(t, "admin"), e.srv.URL+"/admin/server")
	lacks(t, page, "is available", `id="upgrade"`, "upgrade-watch")
}

func TestRemindMeLaterPutsTheBannerOffForThatReleaseOnly(t *testing.T) {
	u := upgradeSite(t)
	admin := u.login(t, "admin")
	base, err := url.Parse(u.srv.URL)
	must(t, err)
	set := func(value string) {
		admin.Jar.SetCookies(base, []*http.Cookie{{Name: snoozeCookie, Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}})
	}
	set(et.Newer)
	_, page := get(t, admin, u.srv.URL+"/")
	lacks(t, page, "is available", "Remind me later")
	_, page = get(t, admin, u.srv.URL+"/admin/server")
	contains(t, page, `id="upgrade"`, "Upgrade to "+et.Newer)

	set("v1.1.0")
	_, page = get(t, admin, u.srv.URL+"/")
	contains(t, page, "kipp "+et.Newer+" is available.")
}

func TestTheUpgradeCardTicksSafeRestartByDefaultAndNamesWhatItUpgrades(t *testing.T) {
	u := upgradeSite(t)
	page := u.server(t)
	contains(t, page, `<section id="upgrade"`, `action="/admin/server/upgrade"`,
		`<input type="checkbox" name="safe" checked>`, "Wait for builds to end, then restart (safe restart)",
		"Upgrade to "+et.Newer, `name="version" value="`+et.Newer+`"`, et.Running+", installed from Homebrew",
		`href="https://github.com/ndthuan/kipp/releases/tag/`+et.Newer+`"`)
	code, _ := get(t, u.login(t, "bob"), u.srv.URL+"/admin/server")
	eq(t, "a non-admin has no Server page", code, http.StatusForbidden)
}

func TestUpgradingWithTheBoxTickedStartsAWaitingRunForAnAdminOnly(t *testing.T) {
	u := upgradeSite(t)
	u.fake.SetBusy(1)
	admin := u.login(t, "admin")
	res, _ := u.form(t, admin, "/admin/server/upgrade", url.Values{"safe": {"on"}, "version": {et.Newer}})
	eq(t, "start", res.StatusCode, http.StatusSeeOther)
	eq(t, "led to", res.Header.Get("Location"), "/admin/server?done=upgrade-started")
	eventually(t, "the run waits for the build", func() bool {
		return strings.Contains(u.server(t), "Waiting: 1 running, 0 queued")
	})
	contains(t, u.audit(t), "admin upgrade")

	res, _ = u.form(t, u.login(t, "bob"), "/admin/server/upgrade", url.Values{"safe": {"on"}})
	eq(t, "a non-admin", res.StatusCode, http.StatusForbidden)

	res, page := u.form(t, admin, "/admin/server/upgrade", url.Values{"safe": {"on"}})
	eq(t, "a second start", res.StatusCode, http.StatusConflict)
	contains(t, page, "already running")
}

func TestUntickingTheBoxLeadsToAPageOfItsOwnThatAsksAgain(t *testing.T) {
	u := upgradeSite(t)
	admin := u.login(t, "admin")
	res, page := u.form(t, admin, "/admin/server/upgrade", url.Values{"version": {et.Newer}})
	eq(t, "the confirmation", res.StatusCode, http.StatusOK)
	contains(t, page, "without waiting for the builds?", "No build runs now", `action="/admin/server/upgrade"`,
		`name="confirm_lost" value="1"`, `name="version" value="`+et.Newer+`"`, "Upgrade and restart now")
	lacks(t, page, `name="safe"`)
	lacks(t, u.server(t), "kipp is upgrading")

	// With a build running the page counts it.
	u.build(t, admin, "slow", nil)
	eventually(t, "the build runs", func() bool {
		_, page = u.form(t, admin, "/admin/server/upgrade", url.Values{"version": {et.Newer}})
		return strings.Contains(page, "1 running build will end as lost")
	})

	// Asking again starts the run.
	res, _ = u.form(t, admin, "/admin/server/upgrade", url.Values{"version": {et.Newer}, "confirm_lost": {"1"}})
	eq(t, "start", res.StatusCode, http.StatusSeeOther)
	eventually(t, "the run restarts the server", func() bool { return u.fake.Switched.Load() == 1 })
	contains(t, u.audit(t), "no safe restart")
}

func TestTheCardShowsEachStateOfARunFromItsStatus(t *testing.T) {
	for _, c := range []struct {
		name   string
		st     state.Status
		wants  []string
		lacks  []string
		banner string
	}{
		{"downloading", state.Status{State: state.StateDownloading},
			[]string{"Checked that it can run", "Downloading and verifying the release", "Wait for builds to end", "Cancel upgrade"}, []string{"Upgrade to "}, "downloading"},
		{"waiting", state.Status{State: state.StateWaiting},
			[]string{"Downloaded and verified the release", "Waiting: 0 running, 0 queued", "Back up the data, just before the restart", `action="/admin/server/upgrade/cancel"`, "Cancel upgrade"}, []string{"Upgrade to ", "Restarted"}, "waiting for builds"},
		{"backing up", state.Status{State: state.StateBackup},
			[]string{"Builds ended", "Backing up the data"}, []string{"Cancel upgrade"}, "backing up"},
		{"restarting", state.Status{State: state.StateSwitching},
			[]string{"Backed up the data", "Restarting, this page reconnects by itself"}, []string{"Cancel upgrade", "Upgrade to "}, "restarting"},
	} {
		t.Run(c.name, func(t *testing.T) {
			u := upgradeSite(t)
			c.st.Version, c.st.From = et.Newer, et.Running
			u.status(t, c.st)
			page := u.server(t)
			contains(t, page, `data-state="`+c.st.State+`"`, "Upgrading from "+et.Running+" to "+et.Newer, "kipp is upgrading to "+et.Newer+": "+c.banner)
			contains(t, page, c.wants...)
			lacks(t, page, c.lacks...)
			lacks(t, page, "Remind me later")
			_, other := get(t, u.login(t, "admin"), u.srv.URL+"/")
			contains(t, other, "kipp is upgrading to "+et.Newer)
		})
	}
}

func TestTheCardSaysHowTheLastRunEnded(t *testing.T) {
	u := upgradeSite(t)
	u.status(t, state.Status{State: state.StateDone, Version: et.Newer, From: et.Running})
	page := u.server(t)
	contains(t, page, "kipp is now "+et.Newer, "kept for 7 days", "Upgrade to "+et.Newer)
	lacks(t, page, "Roll back")

	// A run the launcher undid says why, and links its log.
	u.status(t, state.Status{State: state.StateFailed, Version: et.Newer, From: et.Running})
	must(t, state.WriteFailure(u.data, state.Failure{Version: et.Newer, From: et.Running, Reason: "it did not answer its health check", LogTail: "boom\n", At: time.Now()}))
	page = u.server(t)
	contains(t, page, et.Newer+" did not start; back on "+et.Running, "it did not answer its health check", `href="/admin/server/upgrade/log"`)
	code, log := get(t, u.login(t, "admin"), u.srv.URL+"/admin/server/upgrade/log")
	eq(t, "log", code, http.StatusOK)
	eq(t, "log body", log, "boom\n")
	code, _ = get(t, u.login(t, "bob"), u.srv.URL+"/admin/server/upgrade/log")
	eq(t, "log for a non-admin", code, http.StatusForbidden)

	// An old result is not repeated for ever.
	must(t, state.WriteStatus(u.data, state.Status{State: state.StateDone, Version: et.Newer, From: et.Running, UpdatedAt: time.Now().Add(-8 * 24 * time.Hour)}))
	lacks(t, u.server(t), "kipp is now")
}

func TestARunThatFailedBeforeTheSwitchSaysSoWithoutALog(t *testing.T) {
	u := upgradeSite(t)
	u.status(t, state.Status{State: state.StateFailed, Version: et.Newer, From: et.Running})
	page := u.server(t)
	contains(t, page, "The upgrade to "+et.Newer+" did not finish, and nothing was changed.")
	lacks(t, page, "did not start; back on", "The end of its log")
	code, _ := get(t, u.login(t, "admin"), u.srv.URL+"/admin/server/upgrade/log")
	eq(t, "no log", code, http.StatusNotFound)
}

func TestTheCancelButtonCallsTheCancelRoute(t *testing.T) {
	u := upgradeSite(t)
	u.fake.SetBusy(1)
	admin := u.login(t, "admin")
	_, _ = u.form(t, admin, "/admin/server/upgrade", url.Values{"safe": {"on"}})
	eventually(t, "the run waits", func() bool { return strings.Contains(u.server(t), "Waiting: 1 running") })

	res, _ := u.form(t, u.login(t, "bob"), "/admin/server/upgrade/cancel", nil)
	eq(t, "a non-admin", res.StatusCode, http.StatusForbidden)
	res, _ = u.form(t, admin, "/admin/server/upgrade/cancel", nil)
	eq(t, "cancel", res.StatusCode, http.StatusSeeOther)
	eq(t, "led to", res.Header.Get("Location"), "/admin/server?done=upgrade-cancelled")
	calls := u.fake.QuietCalls()
	eq(t, "quiet is off again", calls[len(calls)-1], false)
	page := u.server(t)
	lacks(t, page, "Cancel upgrade", "Waiting:")
	contains(t, page, "Upgrade to "+et.Newer)
	res, _ = u.form(t, admin, "/admin/server/upgrade/cancel", nil)
	eq(t, "nothing to cancel", res.StatusCode, http.StatusConflict)
}

func TestARollbackIsConfirmedOnAPageThatWarnsOfTheDataLost(t *testing.T) {
	u := upgradeSite(t)
	admin := u.login(t, "admin")
	code, page := get(t, admin, u.srv.URL+"/admin/server/upgrade/rollback")
	eq(t, "nothing to go back to", code, http.StatusConflict)
	contains(t, page, "no earlier version")

	name, err := backup.Take(context.Background(), u.data, et.Running, time.Now())
	must(t, err)
	must(t, backup.MarkSucceeded(u.data, name, time.Now()))
	must(t, state.WritePrevious(u.data, "v0.9.0"))
	u.status(t, state.Status{State: state.StateDone, Version: et.Newer, From: et.Running, Backup: name})
	page = u.server(t)
	contains(t, page, `href="/admin/server/upgrade/rollback"`, "Roll back to v0.9.0", "everything written since the upgrade is lost")

	code, page = get(t, admin, u.srv.URL+"/admin/server/upgrade/rollback")
	eq(t, "confirmation", code, http.StatusOK)
	contains(t, page, "Roll back to v0.9.0?", "is lost", `action="/admin/server/upgrade/rollback"`)
	res, _ := u.form(t, admin, "/admin/server/upgrade/rollback", nil)
	eq(t, "rollback", res.StatusCode, http.StatusSeeOther)
	eventually(t, "the server restarts", func() bool { return u.fake.Switched.Load() == 1 })
	res, _ = u.form(t, u.login(t, "bob"), "/admin/server/upgrade/rollback", nil)
	eq(t, "a non-admin", res.StatusCode, http.StatusForbidden)
}

func TestTheCardShowsTheManualLinesWhereTheEngineCannotUpgrade(t *testing.T) {
	// A release with no signature file: kipp does not upgrade to it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"` + et.Newer + `","html_url":"x","assets":[]}`))
	}))
	t.Cleanup(srv.Close)
	host, _ := url.Parse(srv.URL)
	chk := update.New(update.Options{Current: et.Running, Base: srv.URL, DownloadBase: srv.URL, Hosts: []string{host.Hostname()}, Insecure: true, Timeout: 5 * time.Second})
	must(t, chk.Check(context.Background()))
	for _, c := range []struct{ origin, line string }{
		{"Homebrew", "brew upgrade kipp"},
		{"Docker image", "docker pull ghcr.io/ndthuan/kipp:" + et.Newer},
		{"release archive or build", "Download the archive of " + et.Newer},
	} {
		eng, _ := et.Engine(t, et.NewRelease(t, et.GoodBinary(et.Newer)), et.NewServer(), func(o *engine.Options) { o.Checker = chk })
		e := site(t, func(d *api.Deps) {
			d.Version, d.Upgrade = et.Running, eng
			d.ServerInfo = func() api.ServerInfo {
				i := serverInfoNow()
				i.Version, i.Origin, i.UpdateCheck = et.Running, c.origin, true
				return i
			}
		})
		_, page := get(t, e.login(t, "admin"), e.srv.URL+"/admin/server")
		contains(t, page, `id="upgrade"`, "cannot install it from here", "has no signature file", c.line)
		lacks(t, page, "Upgrade to ", `name="safe"`)
		_, other := get(t, e.login(t, "admin"), e.srv.URL+"/")
		lacks(t, other, "is available")
	}
}
