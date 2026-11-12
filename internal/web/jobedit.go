package web

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/jobstore"
	"github.com/ndthuan/kipp/internal/store"
)

// The routes of the two forms that send a job's whole text, which the
// password step brings back filled rather than through an address: a job
// file is too long for one.
const (
	createJobRoute = "POST /admin/jobs"
	saveJobRoute   = "POST /jobs/{name}/edit"
)

// historyPage is how many versions the History tab shows at once.
const historyPage = 20

// newJobTemplate is what the editor of a new job starts with: the least a
// job needs, a name and one stage.
const newJobTemplate = "name: my-job\ndescription: What this job does.\nstages:\n  build:\n    run: echo hello\n"

func (s *Site) jobEditRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/jobs/new", s.withUser(s.newJob))
	mux.HandleFunc(createJobRoute, s.withUser(s.createJob))
	mux.HandleFunc("GET /admin/jobs/deleted", s.withUser(s.deletedJobs))
	mux.HandleFunc("GET /jobs/{name}/edit", s.withUser(s.editJob))
	mux.HandleFunc(saveJobRoute, s.withUser(s.saveJob))
	mux.HandleFunc("GET /jobs/{name}/delete", s.withUser(s.confirmDeleteJob))
	mux.HandleFunc("POST /jobs/{name}/delete", s.withUser(s.deleteJob))
	mux.HandleFunc("GET /jobs/{name}/history", s.withUser(s.jobHistory))
	mux.HandleFunc("POST /jobs/{name}/history/{id}/restore", s.withUser(s.restoreJob))
	mux.HandleFunc("POST /jobs/{name}/resolve", s.withUser(s.resolveJob))
}

// jobPath is a job's page.
func jobPath(name string) string { return "/jobs/" + url.PathEscape(name) }

func jobPage(r *http.Request) string { return jobPath(r.PathValue("name")) }

func editPage(r *http.Request) string { return jobPage(r) + "/edit" }

func historyOf(r *http.Request) string { return jobPage(r) + "/history" }

// jobRefusal is the status and the words a refusal of the job store is
// answered with, 0 for an error that is not one.
func jobRefusal(err error) (int, string) {
	var refused *config.Errors
	var write *jobstore.WriteError
	switch {
	case errors.As(err, &refused):
		return http.StatusUnprocessableEntity, "The job was refused: " + err.Error()
	case errors.As(err, &write):
		return http.StatusInternalServerError, "The job file could not be written, and nothing changed: " + err.Error()
	case errors.Is(err, jobstore.ErrEditsOff):
		return http.StatusConflict, "Job edits are off on this server (kipp serve --job-edits off)."
	case errors.Is(err, jobstore.ErrStale), errors.Is(err, jobstore.ErrPreconditionRequired):
		return http.StatusPreconditionFailed, "The job changed since this page was opened. Open it again to see the change, then make yours."
	case errors.Is(err, jobstore.ErrFilesChanged):
		return http.StatusConflict, "The job file changed on disk since kipp read it. Reload the job files first."
	case errors.Is(err, jobstore.ErrExists):
		return http.StatusConflict, "A job of that name exists already."
	case errors.Is(err, jobstore.ErrDeletion):
		return http.StatusConflict, "That version is a deletion. Restore the version before it."
	case errors.Is(err, jobstore.ErrNoJob), errors.Is(err, jobstore.ErrNoConflict):
		return http.StatusNotFound, "There is nothing here, or nothing you may see."
	}
	return 0, ""
}

// jobError is one error of a refused job, with the line of the text it is
// about when it says one.
type jobError struct {
	Line int
	Text string
}

var lineRe = regexp.MustCompile(`\bline (\d+):\s*`)

// jobErrors are the errors of a refused change, a line each: a YAML error
// may hold several, each about a line of its own.
func jobErrors(err error) []jobError {
	items := []error{err}
	if refused, ok := errors.AsType[*config.Errors](err); ok {
		items = refused.Items
	}
	var out []jobError
	for _, item := range items {
		for part := range strings.SplitSeq(item.Error(), "\n") {
			// The heading yaml puts over its list says nothing its lines do not.
			if part = strings.TrimSpace(part); part != "" && !strings.HasSuffix(part, "unmarshal errors:") {
				out = append(out, lineOf(part))
			}
		}
	}
	return out
}

func lineOf(msg string) jobError {
	m := lineRe.FindStringSubmatchIndex(msg)
	if m == nil {
		return jobError{Text: msg}
	}
	n, _ := strconv.Atoi(msg[m[2]:m[3]])
	text := msg[:m[0]] + msg[m[1]:]
	// The file's name and the parser's are the same on every line here.
	if head, rest, ok := strings.Cut(text, ".yml: "); ok && config.ValidName(head) {
		text = rest
	}
	return jobError{Line: n, Text: strings.TrimPrefix(text, "yaml: ")}
}

// tabsView is the tabs of a job's own pages, for an admin: Edit is there
// unless edits are off.
type tabsView struct {
	Job, Current string
	Edit         bool
}

// editorData is the editor of a job's text: Job is "" for a new job, and
// Version the ETag it was opened at, which a save carries.
type editorData struct {
	Job, YAML, Version string
	Errors             []jobError
	// Stale: the job changed since the editor was opened; Newer is its
	// text now, and Sides it beside the text that was sent.
	Stale  bool
	Newer  string
	Sides  sidesView
	Notice string
	Tabs   tabsView
}

func (s *Site) editor(w http.ResponseWriter, r *http.Request, code int, d editorData) {
	title, here := "New job", "/jobs"
	if d.Job != "" {
		title = "Edit " + d.Job
		d.Tabs = tabsView{Job: d.Job, Current: "edit", Edit: true}
	}
	s.render(w, r, "jobedit", code, page{Title: title, Here: here, Job: d.Job, Data: d})
}

// postedText is the job text a form sent, with a browser's line ends made the
// file's.
func postedText(r *http.Request, field string) string {
	return strings.ReplaceAll(r.PostFormValue(field), "\r\n", "\n")
}

// editsOn refuses a page that changes jobs on a server whose edits are
// off, before it shows a form nobody could send.
func (s *Site) editsOn(u *store.User) error {
	if !u.Admin {
		return api.ErrForbidden
	}
	if s.api.Service().JobEditMode() == jobstore.Off {
		return jobstore.ErrEditsOff
	}
	return nil
}

func (s *Site) newJob(w http.ResponseWriter, r *http.Request, u *store.User) {
	if err := s.editsOn(u); err != nil {
		s.fail(w, r, err)
		return
	}
	s.editor(w, r, http.StatusOK, editorData{YAML: newJobTemplate})
}

func (s *Site) createJob(w http.ResponseWriter, r *http.Request, u *store.User) {
	text := postedText(r, "yaml")
	name, err := config.NameOf([]byte(text))
	if err != nil {
		s.editor(w, r, http.StatusUnprocessableEntity, editorData{YAML: text, Errors: jobErrors(err)})
		return
	}
	if _, err := s.api.Service().CreateJob(r.Context(), u, api.SessionFrom(r.Context()), name, []byte(text), store.HowWeb); err != nil {
		s.editorRefused(w, r, editorData{YAML: text}, err)
		return
	}
	done(w, r, jobPath(name), "job-created")
}

func (s *Site) editJob(w http.ResponseWriter, r *http.Request, u *store.User) {
	name := r.PathValue("name")
	err := s.editsOn(u)
	var d jobstore.Definition
	if err == nil {
		d, err = s.api.Service().JobDefinition(r.Context(), u, name)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.editor(w, r, http.StatusOK, editorData{Job: name, YAML: string(d.Text), Version: d.Hash})
}

func (s *Site) saveJob(w http.ResponseWriter, r *http.Request, u *store.User) {
	name, text := r.PathValue("name"), postedText(r, "yaml")
	_, err := s.api.Service().ReplaceJob(r.Context(), u, api.SessionFrom(r.Context()), name, []byte(text), r.PostFormValue("version"), store.HowWeb)
	if err != nil {
		s.editorRefused(w, r, editorData{Job: name, YAML: text, Version: r.PostFormValue("version")}, err)
		return
	}
	done(w, r, jobPath(name), "job-saved")
}

// editorRefused answers a save or a create that was refused with the
// editor, the text as it was sent: every error of a refused set, or the
// job as it is now beside the text when it changed meanwhile. Any other
// refusal is a page of its own.
func (s *Site) editorRefused(w http.ResponseWriter, r *http.Request, d editorData, err error) {
	_, refused := errors.AsType[*config.Errors](err)
	stale := errors.Is(err, jobstore.ErrStale) || errors.Is(err, jobstore.ErrPreconditionRequired)
	switch {
	case stale && d.Job != "":
		now, rerr := s.api.Service().JobDefinition(r.Context(), api.UserFrom(r.Context()), d.Job)
		if rerr != nil {
			s.fail(w, r, rerr)
			return
		}
		d.Stale, d.Newer, d.Version = true, string(now.Text), now.Hash
		d.Sides = sidesView{Left: "The job now", Right: "Your text", Rows: sides(d.Newer, d.YAML)}
		s.editor(w, r, http.StatusPreconditionFailed, d)
	case refused:
		d.Errors = jobErrors(err)
		s.editor(w, r, http.StatusUnprocessableEntity, d)
	case errors.Is(err, jobstore.ErrExists):
		_, msg := jobRefusal(err)
		d.Errors = []jobError{{Text: msg}}
		s.editor(w, r, http.StatusConflict, d)
	default:
		s.fail(w, r, err)
	}
}

// editorAfterSudo brings the editor back filled once the password step is
// passed, for the admin to send it again, seeing it.
func (s *Site) editorAfterSudo(w http.ResponseWriter, r *http.Request, d sudoData) {
	e := editorData{Notice: "Your password is confirmed. Nothing was changed yet: send the job again."}
	for _, c := range d.Carry {
		switch strings.TrimPrefix(c.Name, carryPrefix) {
		case "yaml":
			e.YAML = strings.ReplaceAll(c.Value, "\r\n", "\n")
		case "version":
			e.Version = c.Value
		}
	}
	if d.Action == saveJobRoute {
		if !config.ValidName(d.About) {
			s.fail(w, r, api.ErrNotFound)
			return
		}
		e.Job = d.About
	}
	s.editor(w, r, http.StatusOK, e)
}

func (s *Site) confirmDeleteJob(w http.ResponseWriter, r *http.Request, u *store.User) {
	name := r.PathValue("name")
	err := s.editsOn(u)
	var d jobstore.Definition
	if err == nil {
		d, err = s.api.Service().JobDefinition(r.Context(), u, name)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	lines := []string{"Nothing starts it any more. Its builds and logs stay until retention removes them, and its versions stay: Deleted jobs, on the Jobs page, can restore it."}
	if d.Origin != jobstore.FromStore {
		lines = append(lines, "It has a job file. In store mode the file stays and the deletion is kept over it; in files mode the file is removed.")
	}
	s.confirm(w, r, "/jobs", confirmData{
		Heading: "Delete the job " + name + "?", Lines: lines,
		Button: "Delete " + name, Back: jobPath(name),
		Hidden: []hiddenField{{"version", d.Hash}},
	})
}

func (s *Site) deleteJob(w http.ResponseWriter, r *http.Request, u *store.User) {
	name := r.PathValue("name")
	if err := s.api.Service().DeleteJob(r.Context(), u, api.SessionFrom(r.Context()), name, r.PostFormValue("version"), store.HowWeb); err != nil {
		s.fail(w, r, err)
		return
	}
	done(w, r, "/admin/jobs/deleted", "job-deleted")
}

// versionRow is a version on the History tab, with its diff against the
// version before it; Restorable says Restore would change what serves.
type versionRow struct {
	V          *store.JobVersion
	Diff       diffView
	First      bool
	Restorable bool
}

type historyData struct {
	Job  string
	Tabs tabsView
	// Serves says the job serves now, at Version, the ETag a restore
	// carries; Edits that edits are on.
	Serves   bool
	Version  string
	Edits    bool
	Versions []versionRow
	// Older is the version to read on before, 0 at the first; Paged: the
	// page is not the newest.
	Older  int64
	Paged  bool
	Notice string
}

func (s *Site) jobHistory(w http.ResponseWriter, r *http.Request, u *store.User) {
	name, svc := r.PathValue("name"), s.api.Service()
	before, err := beforeVersion(r)
	var found []*store.JobVersion
	var more bool
	if err == nil {
		// One more than is shown, as the base of the oldest one's diff.
		found, more, err = svc.JobVersions(r.Context(), u, name, before, historyPage+1)
	}
	if err == nil && len(found) == 0 {
		err = api.ErrNotFound
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	edits := svc.JobEditMode() != jobstore.Off
	d := historyData{Job: name, Edits: edits, Paged: before > 0, Notice: noticeOf(r), Tabs: tabsView{Job: name, Current: "history", Edit: edits}}
	if now, err := svc.JobDefinition(r.Context(), u, name); err == nil {
		d.Serves, d.Version = true, now.Hash
	}
	shown := found[:min(len(found), historyPage)]
	if len(found) > historyPage || more {
		d.Older = shown[len(shown)-1].ID
	}
	for i, v := range shown {
		row := versionRow{V: v, First: i+1 == len(found) && !more, Restorable: edits && !v.Deleted && (!d.Serves || v.Hash != d.Version)}
		prev := ""
		if i+1 < len(found) {
			prev = found[i+1].Text
		}
		row.Diff = diffOf(prev, v.Text)
		d.Versions = append(d.Versions, row)
	}
	s.render(w, r, "jobhistory", http.StatusOK, page{Title: "History of " + name, Here: "/jobs", Job: name, Data: d})
}

// beforeVersion is the version the History tab was asked to start before.
func beforeVersion(r *http.Request) (int64, error) {
	v := r.URL.Query().Get("before")
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 1 {
		return 0, api.ErrNotFound
	}
	return n, nil
}

func (s *Site) restoreJob(w http.ResponseWriter, r *http.Request, u *store.User) {
	name := r.PathValue("name")
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.fail(w, r, api.ErrNotFound)
		return
	}
	if _, err := s.api.Service().RestoreJob(r.Context(), u, api.SessionFrom(r.Context()), name, id, r.PostFormValue("version")); err != nil {
		s.fail(w, r, err)
		return
	}
	done(w, r, jobPath(name), "job-restored")
}

func (s *Site) resolveJob(w http.ResponseWriter, r *http.Request, u *store.User) {
	name, keep := r.PathValue("name"), r.PostFormValue("keep")
	if err := s.api.Service().ResolveJob(r.Context(), u, api.SessionFrom(r.Context()), name, keep); err != nil {
		s.fail(w, r, err)
		return
	}
	done(w, r, jobPath(name), "kept-"+keep)
}

type deletedData struct {
	Jobs   []*store.JobVersion
	Notice string
}

func (s *Site) deletedJobs(w http.ResponseWriter, r *http.Request, u *store.User) {
	gone, err := s.api.Service().DeletedJobs(r.Context(), u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, "jobsdeleted", http.StatusOK, page{Title: "Deleted jobs", Here: "/jobs", Data: deletedData{Jobs: gone, Notice: noticeOf(r)}})
}

// jobEdits fills what the job page shows an admin about changing a job of
// the server's own: its tabs, whether edits are off, and the edit that
// waits in conflict.
func (s *Site) jobEdits(r *http.Request, u *store.User, d *jobData) error {
	d.Notice = noticeOf(r)
	if !u.Admin || d.View.Source == "" {
		return nil
	}
	svc := s.api.Service()
	d.EditsOff = svc.JobEditMode() == jobstore.Off
	d.Tabs = &tabsView{Job: d.View.Name, Current: "builds", Edit: !d.EditsOff}
	if !d.View.EditWaits {
		return nil
	}
	cs, err := svc.JobConflicts(r.Context(), u)
	if err != nil {
		return err
	}
	d.Conflict = conflictOf(cs, d.View.Name)
	return nil
}

// conflictView is an edit that waits while its file serves, as the job
// page shows it to an admin: the file beside the edit.
type conflictView struct {
	Reason                 string
	FileGone, EditDeletion bool
	Sides                  sidesView
}

func conflictOf(cs []jobstore.Conflict, name string) *conflictView {
	for _, c := range cs {
		if c.Job != name {
			continue
		}
		v := &conflictView{Reason: c.Reason, FileGone: c.File == nil, EditDeletion: c.Edit.Deleted}
		edit := ""
		if !c.Edit.Deleted {
			edit = c.Edit.Text
		}
		v.Sides = sidesView{Left: "The job file", Right: "Your edit", Rows: sides(string(c.File), edit)}
		return v
	}
	return nil
}
