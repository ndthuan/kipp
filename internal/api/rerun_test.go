package api

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/ndthuan/kipp/internal/store"
)

func TestRunningBuildsAreChosenToRunAgainByAnAdminAloneAuditedAndARestartLosesNothing(t *testing.T) {
	var restarts atomic.Int32
	e := newEnv(t, func(a *API) { a.deps.Restart = func() { restarts.Add(1) } })
	slow := e.started(t, "slow")
	once := e.started(t, "once")
	number := func(id string) int {
		return int(e.object(t, 200, e.admin, "GET", "/api/v1/builds/"+id, "")["number"].(float64))
	}
	body := func(job string, n int, restart bool) string {
		return fmt.Sprintf(`{"builds":[{"job":%q,"number":%d}],"restart":%t}`, job, n, restart)
	}
	slowN, onceN := number(slow), number(once)

	e.call(t, 409, e.admin, "POST", "/api/v1/admin/quiet/rerun", body("slow", slowN, false)) // not quiet
	e.object(t, 200, e.admin, "POST", "/api/v1/admin/quiet", "")
	e.call(t, 403, e.viewer, "POST", "/api/v1/admin/quiet/rerun", body("slow", slowN, true))
	e.call(t, 400, e.admin, "POST", "/api/v1/admin/quiet/rerun", `{"builds":[]}`)
	e.call(t, 404, e.admin, "POST", "/api/v1/admin/quiet/rerun", body("slow", 999, false))

	// once still runs, so a restart now would lose it: none happens.
	text, _ := e.call(t, 409, e.admin, "POST", "/api/v1/admin/quiet/rerun", body("slow", slowN, true))
	contains(t, text, "not restarted: 1 running, 0 queued, 1 will run again after the restart")
	eq(t, "restarts", restarts.Load(), int32(0))
	q := e.object(t, 200, e.admin, "GET", "/api/v1/admin/quiet", "")
	eq(t, "running", q["running"], any(float64(1)))
	eq(t, "rerun", q["rerun"], any(float64(1)))

	q = e.object(t, 200, e.admin, "POST", "/api/v1/admin/quiet/rerun", body("once", onceN, true))
	eq(t, "running", q["running"], any(float64(0)))
	eq(t, "rerun", q["rerun"], any(float64(2)))
	eq(t, "idle", q["idle"], true)
	eq(t, "restarts", restarts.Load(), int32(1))
	eq(t, "audit", e.lastAudit(t), "admin restart : 0 running, 0 queued, 2 will run again after the restart")
	rows := e.audit(t)
	choice := rows[len(rows)-2]
	eq(t, "the choice's audit", choice.Action+" "+choice.Target+": "+choice.Detail, fmt.Sprintf("quiet.rerun once #%d: runs again after the restart", onceN))

	// Quiet off forgets the choice: both count as running again.
	q = e.object(t, 200, e.admin, "DELETE", "/api/v1/admin/quiet", "")
	eq(t, "running", q["running"], any(float64(2)))
	eq(t, "rerun", q["rerun"], any(float64(0)))
}

func TestARestartIsRefusedWhenTheServerCannotRestartItself(t *testing.T) {
	e := newEnv(t)
	slow := e.started(t, "slow")
	n := int(e.object(t, 200, e.admin, "GET", "/api/v1/builds/"+slow, "")["number"].(float64))
	e.object(t, 200, e.admin, "POST", "/api/v1/admin/quiet", "")
	text, _ := e.call(t, 409, e.admin, "POST", "/api/v1/admin/quiet/rerun", fmt.Sprintf(`{"builds":[{"job":"slow","number":%d}],"restart":true}`, n))
	contains(t, text, "cannot restart itself")
}

func TestABuildStoppedForARestartAndTheOneThatRunsItAgainLinkEachOther(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	old := &store.Build{Job: "smoke", Status: store.Cancelled, Trigger: "api"}
	must(t, e.st.CreateBuild(ctx, old))
	must(t, e.st.KeepRerun(ctx, old.ID))
	again := &store.Build{Job: "smoke", Status: store.Cancelled, Trigger: "api", RerunOf: old.Number}
	must(t, e.st.CreateBuild(ctx, again))
	must(t, e.st.LinkRerun(ctx, old.ID, again.Number))
	o := e.object(t, 200, e.admin, "GET", fmt.Sprintf("/api/v1/builds/%d", old.ID), "")
	n := e.object(t, 200, e.admin, "GET", fmt.Sprintf("/api/v1/builds/%d", again.ID), "")
	eq(t, "rerun_as", o["rerun_as"], any(float64(again.Number)))
	eq(t, "rerun_of", n["rerun_of"], any(float64(old.Number)))
	eq(t, "the stopped build's rerun_of", o["rerun_of"], nil)
}
