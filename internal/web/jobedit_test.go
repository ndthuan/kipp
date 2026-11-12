package web

import (
	"context"
	"errors"
	"html"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/jobstore"
	"github.com/ndthuan/kipp/internal/store"
)

// jobsEnv is the site with a job store over a jobs directory of its own,
// which holds the same files as the site's.
type jobsEnv struct {
	*env
	jobs *jobstore.Jobs
	dir  string
}

func jobsSite(t *testing.T, mode jobstore.Mode) *jobsEnv {
	t.Helper()
	dir := t.TempDir()
	for name, body := range jobFiles {
		must(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
	}
	je := &jobsEnv{dir: dir}
	je.env = site(t, func(d *api.Deps) {
		dispatcher := d.Dispatch
		je.jobs = jobstore.New(jobstore.Options{Mode: mode, Dir: dir, Store: d.Store, Log: slog.New(slog.DiscardHandler),
			Publish: func(_ context.Context, snap *config.Snapshot) { dispatcher.SetSnapshot(snap) }})
		_, err := je.jobs.Load(context.Background())
		must(t, err)
		d.Jobs = je.jobs
	})
	return je
}

// sudoAdmin is the admin logged in, past the password step.
func (je *jobsEnv) sudoAdmin(t *testing.T) *http.Client {
	t.Helper()
	c := je.login(t, "admin")
	je.sudo(t, c)
	return c
}

func (je *jobsEnv) definition(t *testing.T, name string) jobstore.Definition {
	t.Helper()
	d, err := je.jobs.Definition(name)
	must(t, err)
	return d
}

var versionFieldRe = regexp.MustCompile(`name="version" value="([^"]*)"`)

// versionOf is the version the form on page carries.
func versionOf(t *testing.T, page string) string {
	t.Helper()
	m := versionFieldRe.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no version field in:\n%s", page)
	}
	return html.UnescapeString(m[1])
}

// textareaOf is the text of the editor on page, as a browser reads it.
func textareaOf(t *testing.T, page string) string {
	t.Helper()
	_, rest, ok := strings.Cut(page, `<textarea class="entry entry-code"`)
	if !ok {
		t.Fatalf("no editor in:\n%s", page)
	}
	_, rest, _ = strings.Cut(rest, ">\n")
	text, _, _ := strings.Cut(rest, "</textarea>")
	return html.UnescapeString(text)
}

const smokeChanged = "name: smoke\ngroup: backend\nstages:\n  lint: {run: echo changed-lint}\n"

func TestOnlyAnAdminSeesTheJobControlsAndTheYAML(t *testing.T) {
	je := jobsSite(t, jobstore.Store)
	bob := je.login(t, "bob")
	code, page := get(t, bob, je.srv.URL+"/jobs/smoke")
	eq(t, "bob's job page", code, http.StatusOK)
	contains(t, page, `<span class="tag origin" title="Where this job comes from">file</span>`)
	lacks(t, page, "/jobs/smoke/edit", "/jobs/smoke/history", "echo lint")
	for _, path := range []string{"/jobs/smoke/edit", "/jobs/smoke/history", "/jobs/smoke/delete", "/admin/jobs/new", "/admin/jobs/deleted"} {
		code, page := get(t, bob, je.srv.URL+path)
		eq(t, "bob at "+path, code, http.StatusForbidden)
		lacks(t, page, "echo lint")
	}
	_, page = get(t, bob, je.srv.URL+"/jobs")
	lacks(t, page, "New job", "Deleted jobs")
	res, _ := je.form(t, bob, "/jobs/smoke/edit", url.Values{"yaml": {smokeChanged}, "version": {"*"}})
	eq(t, "bob saves", res.StatusCode, http.StatusForbidden)

	admin := je.login(t, "admin")
	_, page = get(t, admin, je.srv.URL+"/jobs/smoke")
	contains(t, page, `href="/jobs/smoke/edit"`, `href="/jobs/smoke/history"`, `aria-current="page">Builds`)
	_, page = get(t, admin, je.srv.URL+"/jobs")
	contains(t, page, `href="/admin/jobs/new"`, `href="/admin/jobs/deleted"`)
	code, page = get(t, admin, je.srv.URL+"/jobs/smoke/edit")
	eq(t, "the editor", code, http.StatusOK)
	eq(t, "the editor's text", textareaOf(t, page), jobFiles["smoke.yml"])
	eq(t, "the editor's version", versionOf(t, page), je.definition(t, "smoke").Hash)
}

func TestASaveNeedsThePasswordAndTheTextComesBackFromThePasswordStep(t *testing.T) {
	je := jobsSite(t, jobstore.Store)
	admin := je.login(t, "admin")
	_, page := get(t, admin, je.srv.URL+"/jobs/smoke/edit")
	version := versionOf(t, page)
	res, page := je.form(t, admin, "/jobs/smoke/edit", url.Values{"yaml": {smokeChanged}, "version": {version}})
	eq(t, "without sudo", res.StatusCode, http.StatusForbidden)
	contains(t, page, "You were about to change a job", "<strong>smoke</strong>")
	eq(t, "nothing changed", string(je.definition(t, "smoke").Text), jobFiles["smoke.yml"])

	fields := hiddenOf(page)
	fields.Set("password", "pw")
	res, page = postBody(t, admin, je.srv.URL+"/sudo", fields)
	eq(t, "the password step", res.StatusCode, http.StatusOK)
	contains(t, page, "Your password is confirmed")
	eq(t, "the text back", textareaOf(t, page), smokeChanged)
	eq(t, "the version back", versionOf(t, page), version)

	res, _ = je.form(t, admin, "/jobs/smoke/edit", url.Values{"yaml": {strings.ReplaceAll(smokeChanged, "\n", "\r\n")}, "version": {version}})
	eq(t, "saved", res.StatusCode, http.StatusSeeOther)
	eq(t, "back to", res.Header.Get("Location"), "/jobs/smoke?done=job-saved")
	d := je.definition(t, "smoke")
	eq(t, "the text, with a file's line ends", string(d.Text), smokeChanged)
	eq(t, "where it comes from", d.Origin, jobstore.StoreOverFile)
	vs, err := je.st.JobVersions(context.Background(), "smoke", 0, 1)
	must(t, err)
	eq(t, "how", vs[0].How, store.HowWeb)
	contains(t, je.audit(t), "admin job.update smoke")
	_, page = get(t, admin, je.srv.URL+"/jobs/smoke?done=job-saved")
	contains(t, page, "The job is saved.", `title="Where this job comes from">store over file</span>`)
}

func TestAStaleSaveKeepsTheTextAndShowsTheJobAsItIsNow(t *testing.T) {
	je := jobsSite(t, jobstore.Store)
	admin := je.sudoAdmin(t)
	_, page := get(t, admin, je.srv.URL+"/jobs/smoke/edit")
	opened := versionOf(t, page)
	meanwhile := strings.Replace(smokeChanged, "changed-lint", "meanwhile-lint", 1)
	_, err := je.jobs.Replace(context.Background(), "smoke", []byte(meanwhile), opened, jobstore.Who{Author: "bob", How: store.HowAPI})
	must(t, err)

	mine := strings.Replace(smokeChanged, "changed-lint", "my-lint", 1)
	res, page := je.form(t, admin, "/jobs/smoke/edit", url.Values{"yaml": {mine}, "version": {opened}})
	eq(t, "stale", res.StatusCode, http.StatusPreconditionFailed)
	contains(t, page, "The job changed since you opened it", "The job now", "Your text", "meanwhile-lint")
	eq(t, "my text kept", textareaOf(t, page), mine)
	now := je.definition(t, "smoke")
	eq(t, "the form carries the job as it is now", versionOf(t, page), now.Hash)
	eq(t, "nothing saved", string(now.Text), meanwhile)

	res, _ = je.form(t, admin, "/jobs/smoke/edit", url.Values{"yaml": {mine}, "version": {versionOf(t, page)}})
	eq(t, "saved once seen", res.StatusCode, http.StatusSeeOther)
}

func TestARefusedSaveListsEveryErrorByLineAndKeepsTheText(t *testing.T) {
	je := jobsSite(t, jobstore.Store)
	admin := je.sudoAdmin(t)
	version := je.definition(t, "smoke").Hash
	bad := "name: smoke\nstages:\n  lint: {run: echo lint}\nnot_a_key: 1\nalso_not: 2\n"
	res, page := je.form(t, admin, "/jobs/smoke/edit", url.Values{"yaml": {bad}, "version": {version}})
	eq(t, "refused", res.StatusCode, http.StatusUnprocessableEntity)
	contains(t, page, "The job was refused, and nothing changed", "<strong>Line 4:</strong>", "<strong>Line 5:</strong>", "not_a_key")
	eq(t, "the text kept", textareaOf(t, page), bad)

	// A job the set refuses as a whole: a stage that needs one not there.
	broken := "name: smoke\nstages:\n  lint: {needs: [nowhere], run: echo lint}\n"
	res, page = je.form(t, admin, "/jobs/smoke/edit", url.Values{"yaml": {broken}, "version": {version}})
	eq(t, "refused by the set", res.StatusCode, http.StatusUnprocessableEntity)
	contains(t, page, "nowhere")
	eq(t, "nothing changed", string(je.definition(t, "smoke").Text), jobFiles["smoke.yml"])
}

func TestCreateTakesTheNameFromTheTextAndADeletedJobIsRestoredFromItsHistory(t *testing.T) {
	je := jobsSite(t, jobstore.Store)
	admin := je.sudoAdmin(t)
	code, page := get(t, admin, je.srv.URL+"/admin/jobs/new")
	eq(t, "new job page", code, http.StatusOK)
	eq(t, "the template", textareaOf(t, page), newJobTemplate)

	res, page := je.form(t, admin, "/admin/jobs", url.Values{"yaml": {"stages:\n  s: {run: 'true'}\n"}})
	eq(t, "no name", res.StatusCode, http.StatusUnprocessableEntity)
	contains(t, page, "the job has no name")
	res, page = je.form(t, admin, "/admin/jobs", url.Values{"yaml": {jobFiles["slow.yml"]}})
	eq(t, "a name taken", res.StatusCode, http.StatusConflict)
	contains(t, page, "A job of that name exists already.")

	fresh := "name: fresh\n# kept as written\nstages:\n  s: {run: echo fresh}\n"
	res, _ = je.form(t, admin, "/admin/jobs", url.Values{"yaml": {fresh}})
	eq(t, "created", res.StatusCode, http.StatusSeeOther)
	eq(t, "to its page", res.Header.Get("Location"), "/jobs/fresh?done=job-created")
	_, page = get(t, admin, je.srv.URL+"/jobs/fresh?done=job-created")
	contains(t, page, "The job is created.", `title="Where this job comes from">store</span>`)

	code, page = get(t, admin, je.srv.URL+"/jobs/fresh/delete")
	eq(t, "the confirmation", code, http.StatusOK)
	contains(t, page, "Delete the job fresh?", "Delete fresh")
	res, _ = je.form(t, admin, "/jobs/fresh/delete", url.Values{"version": {"stale"}})
	eq(t, "a stale delete", res.StatusCode, http.StatusPreconditionFailed)
	res, _ = je.form(t, admin, "/jobs/fresh/delete", url.Values{"version": {versionOf(t, page)}})
	eq(t, "deleted", res.StatusCode, http.StatusSeeOther)
	eq(t, "to the deleted jobs", res.Header.Get("Location"), "/admin/jobs/deleted?done=job-deleted")
	if _, err := je.jobs.Definition("fresh"); !errors.Is(err, jobstore.ErrNoJob) {
		t.Fatalf("fresh still serves: %v", err)
	}

	_, page = get(t, admin, je.srv.URL+"/admin/jobs/deleted?done=job-deleted")
	contains(t, page, "The job is deleted.", `href="/jobs/fresh/history"`)
	_, page = get(t, admin, je.srv.URL+"/jobs/fresh/history")
	contains(t, page, "This job serves no more.", "deleted</span>", `action="/jobs/fresh/history/`)
	m := regexp.MustCompile(`action="/jobs/fresh/history/(\d+)/restore"`).FindStringSubmatch(page)
	res, _ = je.form(t, admin, "/jobs/fresh/history/"+m[1]+"/restore", url.Values{"version": {""}})
	eq(t, "restored", res.StatusCode, http.StatusSeeOther)
	eq(t, "to its page", res.Header.Get("Location"), "/jobs/fresh?done=job-restored")
	eq(t, "the text back", string(je.definition(t, "fresh").Text), fresh)
	contains(t, je.audit(t), "admin job.create fresh", "admin job.delete fresh", "admin job.revert fresh")
	_, page = get(t, admin, je.srv.URL+"/admin/jobs/deleted")
	contains(t, page, "No job is deleted.")
}

func TestHistoryShowsEachVersionWithItsDiffAndARestoreWhereOneChangesSomething(t *testing.T) {
	je := jobsSite(t, jobstore.Store)
	admin := je.sudoAdmin(t)
	res, _ := je.form(t, admin, "/jobs/smoke/edit", url.Values{"yaml": {smokeChanged}, "version": {je.definition(t, "smoke").Hash}})
	eq(t, "saved", res.StatusCode, http.StatusSeeOther)
	code, page := get(t, admin, je.srv.URL+"/jobs/smoke/history")
	eq(t, "history", code, http.StatusOK)
	contains(t, page, `aria-current="page">History`, "by admin through web", `<span class="dl dl-add">`, `<span class="dl dl-del">`, "changed-lint", "The first version")
	restores := regexp.MustCompile(`action="/jobs/smoke/history/(\d+)/restore"`).FindAllStringSubmatch(page, -1)
	eq(t, "a restore for the older version alone", len(restores), 1)
	eq(t, "it carries the version that serves", versionOf(t, page), je.definition(t, "smoke").Hash)
	res, _ = je.form(t, admin, "/jobs/smoke/history/"+restores[0][1]+"/restore", url.Values{"version": {je.definition(t, "smoke").Hash}})
	eq(t, "restored", res.StatusCode, http.StatusSeeOther)
	eq(t, "the file's text back", string(je.definition(t, "smoke").Text), jobFiles["smoke.yml"])

	code, _ = get(t, admin, je.srv.URL+"/jobs/nothing-here/history")
	eq(t, "a job with no versions", code, http.StatusNotFound)
	code, _ = get(t, admin, je.srv.URL+"/jobs/smoke/history?before=x")
	eq(t, "a bad before", code, http.StatusNotFound)
}

func TestHistoryPagesThroughManyVersions(t *testing.T) {
	je := jobsSite(t, jobstore.Store)
	for i := range historyPage + 2 {
		text := strings.Replace(smokeChanged, "changed-lint", "lint-"+strconv.Itoa(i), 1)
		_, err := je.jobs.Replace(context.Background(), "smoke", []byte(text), je.definition(t, "smoke").Hash, jobstore.Who{Author: "admin", How: store.HowAPI})
		must(t, err)
	}
	admin := je.login(t, "admin")
	_, page := get(t, admin, je.srv.URL+"/jobs/smoke/history")
	eq(t, "a page", strings.Count(page, `<li class="version">`), historyPage)
	m := regexp.MustCompile(`history\?before=(\d+)`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no link to older versions")
	}
	_, page = get(t, admin, je.srv.URL+"/jobs/smoke/history?before="+m[1])
	contains(t, page, "Newest versions", "The first version")
	eq(t, "the rest", strings.Count(page, `<li class="version">`), 3)
}

func TestAConflictShowsTheFileBesideTheEditAndIsSettledEitherWay(t *testing.T) {
	je := jobsSite(t, jobstore.Store)
	admin := je.sudoAdmin(t)
	conflict := func(fileRun string) {
		t.Helper()
		text := strings.Replace(jobFiles["smoke.yml"], "echo lint", fileRun, 1)
		must(t, os.WriteFile(filepath.Join(je.dir, "smoke.yml"), []byte(text), 0o600))
		r, err := je.jobs.Load(context.Background())
		must(t, err)
		eq(t, "conflicts", r.Conflicts, 1)
	}
	res, _ := je.form(t, admin, "/jobs/smoke/edit", url.Values{"yaml": {smokeChanged}, "version": {je.definition(t, "smoke").Hash}})
	eq(t, "saved", res.StatusCode, http.StatusSeeOther)
	conflict("echo file-lint")

	_, page := get(t, admin, je.srv.URL+"/jobs/smoke")
	contains(t, page, "An edit of this job waits", "The job file changed under the edit", "The job file", "Your edit", "file-lint", "changed-lint", "Use the file", "Keep my edit")
	_, page = get(t, je.login(t, "bob"), je.srv.URL+"/jobs/smoke")
	lacks(t, page, "An edit of this job waits", "changed-lint")
	_, page = get(t, admin, je.srv.URL+"/admin/server")
	contains(t, page, "1 job edit waits in conflict", `href="/jobs/smoke#conflict"`, "<code>store</code>")

	res, _ = je.form(t, admin, "/jobs/smoke/resolve", url.Values{"keep": {"edit"}})
	eq(t, "kept the edit", res.StatusCode, http.StatusSeeOther)
	eq(t, "back to", res.Header.Get("Location"), "/jobs/smoke?done=kept-edit")
	eq(t, "the edit serves", string(je.definition(t, "smoke").Text), smokeChanged)

	conflict("echo other-lint")
	res, _ = je.form(t, admin, "/jobs/smoke/resolve", url.Values{"keep": {"file"}})
	eq(t, "kept the file", res.StatusCode, http.StatusSeeOther)
	d := je.definition(t, "smoke")
	contains(t, string(d.Text), "other-lint")
	eq(t, "from the file", d.Origin, jobstore.FromFile)
	contains(t, je.audit(t), "job.resolve smoke kept the edit", "job.resolve smoke kept the file")
	res, _ = je.form(t, admin, "/jobs/smoke/resolve", url.Values{"keep": {"file"}})
	eq(t, "nothing waits", res.StatusCode, http.StatusNotFound)
}

func TestEveryJobChangeNeedsThePasswordStep(t *testing.T) {
	je := jobsSite(t, jobstore.Store)
	admin := je.login(t, "admin")
	for path, what := range map[string]string{
		"/admin/jobs":                   "create a job",
		"/jobs/smoke/delete":            "delete a job",
		"/jobs/smoke/history/1/restore": "restore a version of a job",
		"/jobs/smoke/resolve":           "settle a job&#39;s conflict",
		"/jobs/smoke/edit":              "change a job",
	} {
		res, page := je.form(t, admin, path, url.Values{"yaml": {smokeChanged}, "keep": {"file"}})
		eq(t, path, res.StatusCode, http.StatusForbidden)
		contains(t, page, "You were about to "+what)
	}
	eq(t, "nothing changed", string(je.definition(t, "smoke").Text), jobFiles["smoke.yml"])
}

func TestOffModeHasNoControlsAndTheJobPageSaysEditsAreOff(t *testing.T) {
	je := jobsSite(t, jobstore.Off)
	admin := je.sudoAdmin(t)
	_, page := get(t, admin, je.srv.URL+"/jobs/smoke")
	contains(t, page, "Job edits are off on this server", `href="/jobs/smoke/history"`)
	lacks(t, page, `href="/jobs/smoke/edit"`)
	_, page = get(t, admin, je.srv.URL+"/jobs")
	lacks(t, page, "/admin/jobs/new")
	for _, path := range []string{"/jobs/smoke/edit", "/admin/jobs/new", "/jobs/smoke/delete"} {
		code, page := get(t, admin, je.srv.URL+path)
		eq(t, path, code, http.StatusConflict)
		contains(t, page, "Job edits are off")
	}
	res, _ := je.form(t, admin, "/admin/jobs", url.Values{"yaml": {"name: fresh\nstages:\n  s: {run: 'true'}\n"}})
	eq(t, "a create", res.StatusCode, http.StatusConflict)
	_, page = get(t, admin, je.srv.URL+"/admin/server")
	contains(t, page, "<dt>Job edits</dt><dd><code>off</code>")
}

func TestJobErrorsReadTheLineEachErrorIsAbout(t *testing.T) {
	errs := jobErrors(&config.Errors{Items: []error{
		errors.New("smoke.yml: yaml: unmarshal errors:\n  line 4: field not_a_key not found in type config.file"),
		errors.New("smoke.yml: stage lint needs nowhere, which is not a stage"),
	}})
	eq(t, "errors", len(errs), 2)
	eq(t, "line", errs[0].Line, 4)
	eq(t, "text", errs[0].Text, "field not_a_key not found in type config.file")
	eq(t, "no line", errs[1].Line, 0)
	eq(t, "a plain error", jobErrors(errors.New("yaml: line 2: did not find expected key"))[0], jobError{Line: 2, Text: "did not find expected key"})
}

func TestJobRefusalsAreAnsweredInWords(t *testing.T) {
	for err, want := range map[error]int{
		jobstore.ErrEditsOff:     http.StatusConflict,
		jobstore.ErrStale:        http.StatusPreconditionFailed,
		jobstore.ErrFilesChanged: http.StatusConflict,
		jobstore.ErrExists:       http.StatusConflict,
		jobstore.ErrDeletion:     http.StatusConflict,
		jobstore.ErrNoJob:        http.StatusNotFound,
		&jobstore.WriteError{Path: "x.yml", Err: errors.New("disk full")}: http.StatusInternalServerError,
		&config.Errors{Items: []error{errors.New("bad")}}:                 http.StatusUnprocessableEntity,
		errors.New("something else"):                                      0,
	} {
		code, msg := jobRefusal(err)
		eq(t, err.Error(), code, want)
		if want != 0 && msg == "" {
			t.Errorf("%v has no words", err)
		}
	}
}
