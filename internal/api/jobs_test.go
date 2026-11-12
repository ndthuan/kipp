package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ndthuan/kipp/internal/jobstore"
	"github.com/ndthuan/kipp/internal/store"
)

const freshJob = "name: fresh\n# a comment kept as written\nstages:\n  s: {run: echo fresh}\n"

func bodyOf(t *testing.T, fields map[string]string) string {
	t.Helper()
	data, err := json.Marshal(fields)
	must(t, err)
	return string(data)
}

// smoke is the YAML of the job smoke and its ETag, read as an admin.
func (e *env) smoke(t *testing.T) (yaml, etag string) {
	t.Helper()
	text, h := e.call(t, 200, e.admin, "GET", "/api/v1/jobs/smoke/definition", "")
	var d struct{ YAML, ETag string }
	must(t, json.Unmarshal([]byte(text), &d))
	eq(t, "ETag header", h.Get("ETag"), d.ETag)
	return d.YAML, d.ETag
}

func TestAJobIsCreatedChangedRestoredAndDeletedThroughTheAPI(t *testing.T) {
	e := newEnv(t)
	created := e.object(t, 201, e.admin, "POST", "/api/v1/jobs", bodyOf(t, map[string]string{"name": "fresh", "yaml": freshJob}))
	eq(t, "source", created["source"].(string), "store")
	eq(t, "yaml as written", created["yaml"].(string), freshJob)
	first := created["etag"].(string)
	eq(t, "job source", e.object(t, 200, e.admin, "GET", "/api/v1/jobs/fresh", "")["source"].(string), "store")
	contains(t, e.lastAudit(t), "job.create fresh")

	changed := strings.Replace(freshJob, "echo fresh", "echo changed", 1)
	put := bodyOf(t, map[string]string{"yaml": changed})
	e.asProblem(t, 428, "precondition_required", e.admin, "PUT", "/api/v1/jobs/fresh/definition", put)
	e.asProblem(t, 412, "precondition_failed", e.admin, "PUT", "/api/v1/jobs/fresh/definition", put, "If-Match", `"stale"`)
	second := e.object(t, 200, e.admin, "PUT", "/api/v1/jobs/fresh/definition", put, "If-Match", first)["etag"].(string)
	if second == first {
		t.Fatal("a changed job kept its ETag")
	}
	// A comment changes the text and not the job, nor its ETag.
	_, h := e.call(t, 200, e.admin, "PUT", "/api/v1/jobs/fresh/definition", bodyOf(t, map[string]string{"yaml": changed + "# one more\n"}), "If-Match", second)
	eq(t, "ETag after a comment", h.Get("ETag"), second)

	versions := e.list(t, e.admin, "/api/v1/jobs/fresh/versions")
	eq(t, "versions", len(versions), 3)
	eq(t, "newest first by", versions[0]["author"].(string), "admin")
	oldest := int64(versions[2]["id"].(float64))
	v := e.object(t, 200, e.admin, "GET", fmt.Sprintf("/api/v1/jobs/fresh/versions/%d", oldest), "")
	eq(t, "the first text", v["yaml"].(string), freshJob)
	eq(t, "how", v["how"].(string), "api")
	e.asProblem(t, 404, "not_found", e.admin, "GET", "/api/v1/jobs/fresh/versions/999999", "")

	restored := e.object(t, 200, e.admin, "POST", fmt.Sprintf("/api/v1/jobs/fresh/versions/%d/restore", oldest), "", "If-Match", second)
	eq(t, "restored ETag", restored["etag"].(string), first)
	contains(t, e.lastAudit(t), "job.revert fresh")

	e.asProblem(t, 428, "precondition_required", e.admin, "DELETE", "/api/v1/jobs/fresh", "")
	e.call(t, 204, e.admin, "DELETE", "/api/v1/jobs/fresh", "", "If-Match", first)
	e.asProblem(t, 404, "not_found", e.admin, "GET", "/api/v1/jobs/fresh/definition", "")
	deletion := e.list(t, e.admin, "/api/v1/jobs/fresh/versions")[0]
	eq(t, "deleted", deletion["deleted"].(bool), true)
	eq(t, "a deletion's hash", deletion["hash"], nil)
	e.asProblem(t, 409, "conflict", e.admin, "POST", fmt.Sprintf("/api/v1/jobs/fresh/versions/%d/restore", int64(deletion["id"].(float64))), "")
	// A deleted job comes back without If-Match.
	e.object(t, 200, e.admin, "POST", fmt.Sprintf("/api/v1/jobs/fresh/versions/%d/restore", oldest), "")
	eq(t, "versions kept", len(e.list(t, e.admin, "/api/v1/jobs/fresh/versions")), 6)
}

func TestAChangeThatBreaksTheSetIsRefusedWithEveryErrorAndChangesNothing(t *testing.T) {
	e := newEnv(t)
	yaml, etag := e.smoke(t)
	bad := "name: smoke\nconcurrency: sometimes\nstages:\n  s: {run: echo, needs: [nowhere]}\n"
	code, text, _ := e.raw(t, e.admin, "PUT", "/api/v1/jobs/smoke/definition", bodyOf(t, map[string]string{"yaml": bad}), "If-Match", etag)
	eq(t, "status", code, 422)
	var d struct {
		Code, Detail string
		Errors       []string
	}
	must(t, json.Unmarshal([]byte(text), &d))
	eq(t, "code", d.Code, "job_refused")
	if len(d.Errors) == 0 || d.Detail != strings.Join(d.Errors, "\n") {
		t.Fatalf("the errors are listed and joined in detail: %q, %q", d.Errors, d.Detail)
	}
	contains(t, d.Errors[0], "concurrency")
	now, still := e.smoke(t)
	eq(t, "yaml", now, yaml)
	eq(t, "etag", still, etag)
	e.asProblem(t, 400, "bad_input", e.admin, "PUT", "/api/v1/jobs/smoke/definition", "{}", "If-Match", etag)
	e.asProblem(t, 400, "bad_input", e.admin, "PUT", "/api/v1/jobs/smoke/definition", "{", "If-Match", etag)
	e.asProblem(t, 400, "bad_input", e.admin, "POST", "/api/v1/jobs", bodyOf(t, map[string]string{"yaml": freshJob}))
	e.asProblem(t, 400, "bad_input", e.admin, "GET", "/api/v1/jobs/smoke/versions/first", "")
	e.asProblem(t, 400, "bad_input", e.admin, "POST", "/api/v1/jobs/smoke/versions/0/restore", "")
	e.asProblem(t, 400, "bad_input", e.admin, "POST", "/api/v1/admin/job-conflicts/smoke/resolve", "{")
}

func TestACreateOfANameTheSetHasOrAChildUsesIsAConflict(t *testing.T) {
	e := newEnv(t)
	e.asProblem(t, 409, "conflict", e.admin, "POST", "/api/v1/jobs", bodyOf(t, map[string]string{"name": "smoke", "yaml": "name: smoke\nstages:\n  s: {run: echo}\n"}))
	e.withSite(t, store.Child{Parent: "site", Key: "main", Kind: "branch", Branch: "main", State: "active"})
	e.asProblem(t, 409, "conflict", e.admin, "POST", "/api/v1/jobs", bodyOf(t, map[string]string{"name": "site.main", "yaml": "name: site.main\nstages:\n  s: {run: echo}\n"}))
	e.asProblem(t, 409, "conflict", e.admin, "GET", "/api/v1/jobs/site.main/definition", "")
}

func TestOnlyAnAdminReadsOrChangesAJobsYAML(t *testing.T) {
	e := newEnv(t)
	_, etag := e.smoke(t)
	for _, tok := range []string{e.viewer, e.aide} {
		e.asProblem(t, 403, "forbidden", tok, "GET", "/api/v1/jobs/smoke/definition", "")
		e.asProblem(t, 403, "forbidden", tok, "GET", "/api/v1/jobs/smoke/versions", "")
		e.asProblem(t, 403, "forbidden", tok, "PUT", "/api/v1/jobs/smoke/definition", bodyOf(t, map[string]string{"yaml": jobFiles["smoke.yml"]}), "If-Match", etag)
		e.asProblem(t, 403, "forbidden", tok, "DELETE", "/api/v1/jobs/smoke", "", "If-Match", etag)
	}
	// An admin's browser session changes a job in sudo mode alone.
	ctx := context.Background()
	p := e.person(t, "keeper", true)
	_, err := e.api.svc.CreateJob(ctx, p.u, p.sess, "fresh", []byte(freshJob), store.HowWeb)
	is(t, "without sudo", err, ErrSudoRequired)
	e.sudo(t, p)
	_, err = e.api.svc.CreateJob(ctx, p.u, p.sess, "fresh", []byte(freshJob), store.HowWeb)
	must(t, err)
	v := e.list(t, e.admin, "/api/v1/jobs/fresh/versions")[0]
	eq(t, "author", v["author"].(string), "keeper")
	eq(t, "how", v["how"].(string), "web")
}

func TestABuildTokenIsRefusedOnEveryRouteThatChangesAJob(t *testing.T) {
	e, bt := buildTokens(t)
	all := map[string]store.Perm{}
	for _, j := range []string{"smoke", "gc", "slow", "once", "cr"} {
		all[j] = store.Perm{View: true, Build: true, Cancel: true}
	}
	tok := bt.Mint(900, "other", 1, "", all)
	_, etag := e.smoke(t)
	named := bodyOf(t, map[string]string{"name": "fresh", "yaml": freshJob})
	text := bodyOf(t, map[string]string{"yaml": jobFiles["smoke.yml"]})
	for _, r := range [][3]string{
		{"POST", "/api/v1/jobs", named},
		{"PUT", "/api/v1/jobs/smoke/definition", text},
		{"DELETE", "/api/v1/jobs/smoke", ""},
		{"POST", "/api/v1/jobs/smoke/versions/1/restore", ""},
		{"POST", "/api/v1/admin/job-conflicts/smoke/resolve", `{"keep":"file"}`},
		{"GET", "/api/v1/jobs/smoke/definition", ""},
		{"GET", "/api/v1/jobs/smoke/versions", ""},
	} {
		e.asProblem(t, 403, "forbidden", tok, r[0], r[1], r[2], "If-Match", etag)
	}
}

func TestAnEditThatMeetsAChangedFileWaitsUntilItIsResolved(t *testing.T) {
	e := newEnv(t)
	_, etag := e.smoke(t)
	edit := strings.Replace(jobFiles["smoke.yml"], "group: backend", "group: edited", 1)
	e.call(t, 200, e.admin, "PUT", "/api/v1/jobs/smoke/definition", bodyOf(t, map[string]string{"yaml": edit}), "If-Match", etag)
	eq(t, "source", e.object(t, 200, e.admin, "GET", "/api/v1/jobs/smoke", "")["source"].(string), "store over file")

	onDisk := strings.Replace(jobFiles["smoke.yml"], "group: backend", "group: disk", 1)
	e.writeJob(t, "smoke", onDisk)
	detail := e.object(t, 200, e.admin, "GET", "/api/v1/jobs/smoke", "")
	eq(t, "the file serves", detail["group"].(string), "disk")
	eq(t, "edit waits", detail["edit_waits"].(bool), true)
	eq(t, "server count", e.object(t, 200, e.admin, "GET", "/api/v1/admin/server", "")["job_conflicts"].(float64), 1)
	conflicts := e.list(t, e.admin, "/api/v1/admin/job-conflicts")
	eq(t, "conflicts", len(conflicts), 1)
	eq(t, "edit", conflicts[0]["edit"].(string), edit)
	eq(t, "file", conflicts[0]["file"].(string), onDisk)

	e.asProblem(t, 400, "bad_input", e.admin, "POST", "/api/v1/admin/job-conflicts/smoke/resolve", `{"keep":"both"}`)
	e.call(t, 204, e.admin, "POST", "/api/v1/admin/job-conflicts/smoke/resolve", `{"keep":"edit"}`)
	contains(t, e.lastAudit(t), "job.resolve smoke: kept the edit")
	eq(t, "the edit serves", e.object(t, 200, e.admin, "GET", "/api/v1/jobs/smoke", "")["group"].(string), "edited")
	eq(t, "none waits", len(e.list(t, e.admin, "/api/v1/admin/job-conflicts")), 0)
	e.asProblem(t, 404, "not_found", e.admin, "POST", "/api/v1/admin/job-conflicts/smoke/resolve", `{"keep":"file"}`)

	// Once more, keeping the file.
	e.writeJob(t, "smoke", jobFiles["smoke.yml"])
	e.call(t, 204, e.admin, "POST", "/api/v1/admin/job-conflicts/smoke/resolve", `{"keep":"file"}`)
	eq(t, "the file serves", e.object(t, 200, e.admin, "GET", "/api/v1/jobs/smoke", "")["source"].(string), "file")
}

func TestWithEditsOffAJobIsReadAndNeverChanged(t *testing.T) {
	e := newEnv(t, func(a *API) {
		js := jobstore.New(jobstore.Options{Mode: jobstore.Off, Dir: a.deps.ServerInfo().DataDir + "/jobs", Store: a.deps.Store})
		_, err := js.Load(context.Background())
		must(t, err)
		a.deps.Jobs = js
	})
	_, etag := e.smoke(t)
	e.asProblem(t, 409, "edits_off", e.admin, "PUT", "/api/v1/jobs/smoke/definition", bodyOf(t, map[string]string{"yaml": jobFiles["smoke.yml"]}), "If-Match", etag)
	e.asProblem(t, 409, "edits_off", e.admin, "POST", "/api/v1/jobs", bodyOf(t, map[string]string{"name": "fresh", "yaml": freshJob}))
	e.list(t, e.admin, "/api/v1/jobs/smoke/versions")
	eq(t, "mode", e.object(t, 200, e.admin, "GET", "/api/v1/admin/server", "")["job_edits"].(string), "off")
}

func TestDeletedJobsAreForAnAdminAndTheModeIsOffWithoutAJobStore(t *testing.T) {
	e := newEnv(t)
	e.object(t, 201, e.admin, "POST", "/api/v1/jobs", bodyOf(t, map[string]string{"name": "fresh", "yaml": freshJob}))
	_, h := e.call(t, 200, e.admin, "GET", "/api/v1/jobs/fresh/definition", "")
	e.call(t, 204, e.admin, "DELETE", "/api/v1/jobs/fresh", "", "If-Match", h.Get("ETag"))
	svc := e.api.Service()
	gone, err := svc.DeletedJobs(context.Background(), e.user(t, "admin"))
	must(t, err)
	if len(gone) != 1 || gone[0].Job != "fresh" {
		t.Fatalf("deleted %+v", gone)
	}
	if _, err := svc.DeletedJobs(context.Background(), e.user(t, "viewer")); err == nil {
		t.Fatal("a viewer read the deleted jobs")
	}
	eq(t, "mode", svc.JobEditMode(), jobstore.Store)
	eq(t, "no job store", (&Service{deps: &Deps{}}).JobEditMode(), jobstore.Off)
}
