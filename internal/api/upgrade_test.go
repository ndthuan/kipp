package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/upgrade/backup"
	"github.com/ndthuan/kipp/internal/upgrade/engine"
	et "github.com/ndthuan/kipp/internal/upgrade/engine/enginetest"
	"github.com/ndthuan/kipp/internal/upgrade/state"
)

// withUpgrades gives the test's server an engine on a fake GitHub release
// and a fake restart.
func (e *env) withUpgrades(t *testing.T, tweak func(*engine.Options)) (*et.Server, string) {
	t.Helper()
	f := et.NewServer()
	eng, data := et.Engine(t, et.NewRelease(t, et.GoodBinary(et.Newer)), f, tweak)
	e.api.deps.Upgrade = eng
	return f, data
}

func (e *env) untilUpgrade(t *testing.T, want string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := e.object(t, 200, e.admin, "GET", "/api/v1/admin/upgrade", "")
		if got["state"] == want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("the upgrade is %v, not %s", got["state"], want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestUpgradeIsAnAdminsAndAServerWithoutAnEngineSaysItDoesNotUpgrade(t *testing.T) {
	e := newEnv(t)
	got := e.object(t, 200, e.admin, "GET", "/api/v1/admin/upgrade", "")
	eq(t, "state", got["state"], "idle")
	eq(t, "unavailable", got["unavailable"], "this server does not upgrade itself")
	e.asProblem(t, 403, "forbidden", e.viewer, "GET", "/api/v1/admin/upgrade", "")
	for _, c := range [][2]string{{"POST", "/api/v1/admin/upgrade"}, {"DELETE", "/api/v1/admin/upgrade"}, {"POST", "/api/v1/admin/upgrade/rollback"}} {
		e.asProblem(t, 403, "forbidden", e.viewer, c[0], c[1], "")
		e.asProblem(t, 409, "conflict", e.admin, c[0], c[1], "")
	}
}

func TestAnAdminStartsAnUpgradeAndSeesItsStatusThroughToTheSwitch(t *testing.T) {
	e := newEnv(t)
	f, data := e.withUpgrades(t, nil)
	idle := e.object(t, 200, e.admin, "GET", "/api/v1/admin/upgrade", "")
	eq(t, "state", idle["state"], "idle")
	eq(t, "latest", idle["latest"], et.Newer)
	eq(t, "unavailable", idle["unavailable"], "")
	eq(t, "last failure", idle["last_failure"], nil)

	started := e.object(t, 202, e.admin, "POST", "/api/v1/admin/upgrade", `{"version":"`+et.Newer+`","wait_timeout":"5m"}`)
	if started["state"] == "idle" || started["version"] != et.Newer || started["from"] != et.Running {
		t.Fatalf("%v", started)
	}
	switching := e.untilUpgrade(t, "switching")
	if switching["backup"] == "" || switching["updated_at"] == nil {
		t.Fatalf("%v", switching)
	}
	eq(t, "restarts asked for", f.Switched.Load(), int32(1))
	p, err := state.ReadPending(data)
	if err != nil || p == nil || p.Kind != state.KindUpgrade {
		t.Fatalf("%+v, %v", p, err)
	}
	if !strings.HasPrefix(e.lastAudit(t), "admin upgrade "+et.Newer+": safe restart") {
		t.Fatalf("audit %q", e.lastAudit(t))
	}
	// One at a time, and nothing to cancel any more.
	d, _ := e.asProblem(t, 409, "conflict", e.admin, "POST", "/api/v1/admin/upgrade", "")
	contains(t, d.Detail, "already running")
	d, _ = e.asProblem(t, 409, "conflict", e.admin, "DELETE", "/api/v1/admin/upgrade", "")
	contains(t, d.Detail, "rollback")
}

func TestAnAdminCancelsAnUpgradeThatWaitsForABuild(t *testing.T) {
	e := newEnv(t)
	f, _ := e.withUpgrades(t, nil)
	f.SetBusy(1)
	e.object(t, 202, e.admin, "POST", "/api/v1/admin/upgrade", `{"safe_restart":true}`)
	waiting := e.untilUpgrade(t, "waiting")
	eq(t, "running", waiting["running"], any(float64(1)))
	cancelled := e.object(t, 200, e.admin, "DELETE", "/api/v1/admin/upgrade", "")
	eq(t, "state", cancelled["state"], "cancelled")
	eq(t, "audit", e.lastAudit(t), "admin upgrade.cancel "+et.Newer+": ")
	eq(t, "restarts", f.Switched.Load(), int32(0))
}

func TestAnUpgradeWithoutASafeRestartAsksToConfirmTheBuildsLost(t *testing.T) {
	e := newEnv(t)
	f, _ := e.withUpgrades(t, nil)
	f.SetBusy(2)
	d, _ := e.asProblem(t, 400, "bad_input", e.admin, "POST", "/api/v1/admin/upgrade", `{"safe_restart":false}`)
	for _, want := range []string{"2 running builds", "lost", "confirm_lost"} {
		contains(t, d.Detail, want)
	}
	e.asProblem(t, 400, "bad_input", e.admin, "POST", "/api/v1/admin/upgrade", `{"version":"v9.9.9"}`)
	e.asProblem(t, 400, "bad_input", e.admin, "POST", "/api/v1/admin/upgrade", `{"wait_timeout":"soon"}`)
	e.asProblem(t, 400, "bad_input", e.admin, "POST", "/api/v1/admin/upgrade", `{"surprise":1}`)
	e.asProblem(t, 400, "bad_input", e.admin, "POST", "/api/v1/admin/upgrade", `{} {}`)
	e.object(t, 202, e.admin, "POST", "/api/v1/admin/upgrade", `{"safe_restart":false,"confirm_lost":true}`)
	e.untilUpgrade(t, "switching")
	eq(t, "quiet", e.object(t, 200, e.admin, "GET", "/api/v1/admin/quiet", "")["quiet"], false)
}

func TestAnUpgradeOfADevelopmentBuildSaysSo(t *testing.T) {
	e := newEnv(t)
	e.withUpgrades(t, func(o *engine.Options) { o.InstallType = func() string { return "dev" } })
	got := e.object(t, 200, e.admin, "GET", "/api/v1/admin/upgrade", "")
	eq(t, "unavailable", got["unavailable"], "not available in a development build")
	d, _ := e.asProblem(t, 409, "conflict", e.admin, "POST", "/api/v1/admin/upgrade", "")
	contains(t, d.Detail, "not available in a development build")
}

func TestARollbackIsStartedByAnAdminAndTakesNoVersion(t *testing.T) {
	e := newEnv(t)
	f, data := e.withUpgrades(t, func(o *engine.Options) { o.Version = "v1.1.0" })
	e.asProblem(t, 409, "conflict", e.admin, "POST", "/api/v1/admin/upgrade/rollback", "")
	name, err := backup.Take(context.Background(), data, et.Running, time.Now())
	must(t, err)
	must(t, backup.MarkSucceeded(data, name, time.Now()))
	must(t, state.WritePrevious(data, et.Running))
	eq(t, "rollback_to", e.object(t, 200, e.admin, "GET", "/api/v1/admin/upgrade", "")["rollback_to"], et.Running)
	e.asProblem(t, 400, "bad_input", e.admin, "POST", "/api/v1/admin/upgrade/rollback", `{"version":"v1.0.0"}`)
	e.object(t, 202, e.admin, "POST", "/api/v1/admin/upgrade/rollback", "")
	got := e.untilUpgrade(t, "switching")
	eq(t, "version", got["version"], et.Running)
	eq(t, "backup", got["backup"], any(name))
	eq(t, "restarts", f.Switched.Load(), int32(1))
	eq(t, "audit", e.lastAudit(t), "admin upgrade.rollback "+et.Running+": safe restart")
}

func TestAFailedUpgradeShowsItsReasonAndTheLaunchersFailureIsReported(t *testing.T) {
	e := newEnv(t)
	_, data := e.withUpgrades(t, func(o *engine.Options) {
		o.Free = func(string) (uint64, bool) { return 1, true }
	})
	e.object(t, 202, e.admin, "POST", "/api/v1/admin/upgrade", "")
	failed := e.untilUpgrade(t, "failed")
	contains(t, failed["error"].(string), "not enough free space")
	must(t, state.WriteFailure(data, state.Failure{Version: et.Newer, From: et.Running, Reason: "it did not answer its health check", LogTail: "boom", At: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}))
	lf := e.object(t, 200, e.admin, "GET", "/api/v1/admin/upgrade", "")["last_failure"].(map[string]any)
	eq(t, "reason", lf["reason"], "it did not answer its health check")
	eq(t, "at", lf["at"], "2026-01-02T03:04:05.000Z")
}
