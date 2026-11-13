package web

import (
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/dispatch"
	"github.com/ndthuan/kipp/internal/store"
)

func TestTheServerPageNamesTheBuildHoldingQuietAndRunsItAgainAfterARestartOnceConfirmed(t *testing.T) {
	var restarts atomic.Int32
	e := site(t, func(d *api.Deps) { d.Restart = func() { restarts.Add(1) } })
	admin := e.login(t, "admin")
	id := e.build(t, admin, "slow", nil)
	eventually(t, "the build runs", func() bool {
		b, err := e.st.Build(t.Context(), id)
		return err == nil && b.Status == store.Running
	})
	res, _ := e.form(t, admin, "/admin/server/quiet", nil)
	eq(t, "quiet on", res.StatusCode, http.StatusSeeOther)
	_, page := get(t, admin, e.srv.URL+"/admin/server")
	contains(t, page, "1 running, 0 waiting")
	lacks(t, page, "still runs", `action="/admin/server/quiet/rerun"`)

	e.ahead.Store(int64(dispatch.HoldingAfter))
	_, page = get(t, admin, e.srv.URL+"/admin/server")
	contains(t, page, "Only slow #1 still runs; it may be waiting on something quiet holds back.",
		`action="/admin/server/quiet/rerun"`, `name="build" value="slow#1"`,
		"Run slow #1 again after the restart, and restart now", "<code>rerun_on_restart: true</code>")

	// It asks first, and nothing happens until it is confirmed.
	res, page = e.form(t, admin, "/admin/server/quiet/rerun", url.Values{"build": {"slow#1"}})
	eq(t, "the question", res.StatusCode, http.StatusOK)
	contains(t, page, "Run slow #1 again after the restart, and restart now?", "Restart now", `name="confirmed" value="1"`)
	eq(t, "restarts before the answer", restarts.Load(), int32(0))
	if st, _ := e.d.QuietState(t.Context()); st.Rerun != 0 {
		t.Fatalf("chosen before the answer: %+v", st)
	}

	res, _ = e.form(t, admin, "/admin/server/quiet/rerun", url.Values{"build": {"slow#1"}, "confirmed": {"1"}})
	eq(t, "the answer", res.StatusCode, http.StatusSeeOther)
	eq(t, "led to", res.Header.Get("Location"), "/admin/server?done=restarting")
	eq(t, "restarts", restarts.Load(), int32(1))
	contains(t, e.audit(t), "quiet.rerun", "slow #1", "restart")
	if st, _ := e.d.QuietState(t.Context()); st.Rerun != 1 || st.Running != 0 {
		t.Fatalf("after the answer %+v", st)
	}

	res, _ = e.form(t, admin, "/admin/server/quiet/rerun", url.Values{"build": {"slow"}})
	eq(t, "a build without its number", res.StatusCode, http.StatusBadRequest)
}

func TestTheBuildPagesOfAStoppedBuildAndOfTheOneThatRunsItAgainLinkEachOther(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	ctx := t.Context()
	old := &store.Build{Job: "smoke", Status: store.Cancelled, Trigger: "api"}
	must(t, e.st.CreateBuild(ctx, old))
	again := &store.Build{Job: "smoke", Status: store.Cancelled, Trigger: "api", RerunOf: old.Number}
	must(t, e.st.CreateBuild(ctx, again))
	must(t, e.st.LinkRerun(ctx, old.ID, again.Number))
	_, page := get(t, admin, e.srv.URL+buildPath(old))
	contains(t, page, `stopped for a restart, run again as <a class="link" href="/jobs/smoke/2">#2</a>`)
	_, page = get(t, admin, e.srv.URL+buildPath(again))
	contains(t, page, `runs <a class="link" href="/jobs/smoke/1">#1</a> again, stopped for a restart`)
}
