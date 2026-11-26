package api

import (
	"fmt"
	"testing"

	"github.com/ndthuan/kipp/internal/config"
)

func (e *env) withLockJobs(t *testing.T) {
	t.Helper()
	var jobs []*config.Job
	for name, body := range map[string]string{
		"holder": "name: holder\nlocks: [x]\nstages:\n  s: {run: 'echo started; sleep 30'}\n",
		"waiter": "name: waiter\nstages:\n  build: {run: 'true'}\n  deploy: {needs: [build], run: 'true', locks: [x]}\n",
	} {
		j, err := config.ParseJob(name+".yml", []byte(body))
		must(t, err)
		jobs = append(jobs, j)
	}
	e.api.deps.Dispatch.SetSnapshot(e.api.deps.Dispatch.Snapshot().WithJobs(jobs...))
}

func (e *env) runningOf(t *testing.T, tok, id string) map[string]any {
	t.Helper()
	q := e.object(t, 200, tok, "GET", "/api/v1/queue", "")
	for _, r := range q["running"].([]any) {
		if m := r.(map[string]any); jsonInt(m["id"]) == id {
			return m
		}
	}
	return nil
}

func TestAStageThatWaitsForALockSaysWhichAndWhoHoldsItInV1(t *testing.T) {
	e := newEnv(t)
	e.withLockJobs(t)
	holder := e.started(t, "holder")
	waiter := e.trigger(t, "waiter", "")
	var run map[string]any
	eventually(t, func() bool {
		run = e.runningOf(t, e.admin, waiter)
		return run != nil && len(run["waiting_stage_locks"].(map[string]any)) == 1
	})
	eq(t, "the line", fmt.Sprint(run["waiting_stages"]), "map[deploy:waiting for lock x]")
	lock := run["waiting_stage_locks"].(map[string]any)["deploy"].(map[string]any)
	eqKeys(t, "a stage's lock", lock, "blocker,lock")
	eq(t, "lock", lock["lock"], any("x"))
	blocker := lock["blocker"].(map[string]any)
	eqKeys(t, "blocker", blocker, "id,job,number,status")
	eq(t, "blocker", jsonInt(blocker["id"]), holder)
	eq(t, "blocker job", blocker["job"], any("holder"))

	tok := token(t, e.st, "dev", false, map[string][]string{"waiter": {"view"}})
	run = e.runningOf(t, tok, waiter)
	eq(t, "a viewer's line", fmt.Sprint(run["waiting_stages"]), "map[deploy:waiting for lock x]")
	lock = run["waiting_stage_locks"].(map[string]any)["deploy"].(map[string]any)
	eq(t, "a viewer's lock", lock["lock"], any("x"))
	eq(t, "a viewer's blocker", lock["blocker"], nil)

	e.object(t, 202, e.admin, "POST", "/api/v1/builds/"+holder+"/cancel", "")
	eq(t, "the waiter, once the lock is free", e.finished(t, waiter)["status"], any("success"))
}

func TestAJobsDetailListsItsStageLocksAndTheGrantInV1(t *testing.T) {
	e := newEnv(t)
	e.withLockJobs(t)
	src, err := config.ParseJob("src.yml", []byte("name: src\nstage_locks: [production]\nsource: {repo: o/r, path: kipp.yml}\n"))
	must(t, err)
	e.api.deps.Dispatch.SetSnapshot(e.api.deps.Dispatch.Snapshot().WithJobs(src))
	waiter := e.object(t, 200, e.admin, "GET", "/api/v1/jobs/waiter", "")
	eq(t, "the job file's locked stages", fmt.Sprint(waiter["locked_stages"]), "map[deploy:[x]]")
	eq(t, "no grant", fmt.Sprint(waiter["stage_locks"]), "[]")
	detail := e.object(t, 200, e.admin, "GET", "/api/v1/jobs/src", "")
	eq(t, "the grant", fmt.Sprint(detail["stage_locks"]), "[production]")
	eq(t, "no stage of its own", fmt.Sprint(detail["locked_stages"]), "map[]")
}
