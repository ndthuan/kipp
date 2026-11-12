package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/jobstore"
	"github.com/ndthuan/kipp/internal/problem"
	"github.com/ndthuan/kipp/internal/store"
)

// defaultVersions is a page of a job's versions when the request says no
// other.
const defaultVersions = 50

// errNoJobStore is a server that was given no job store, which only a test
// makes.
var errNoJobStore = errors.New("this server keeps no job store")

// readsJobs is nil when u may read the YAML of jobs, their versions and the
// conflicts: an admin, as a job's env may hold what a viewer never saw.
func readsJobs(u *store.User) error {
	if !u.Admin {
		return ErrForbidden
	}
	return nil
}

// editsJobs is nil when u may change jobs: an admin's token, or an admin's
// browser session in sudo mode, as for a secret. A build's token is never
// an admin.
func editsJobs(u *store.User, sess *store.Session) error {
	if !u.Admin {
		return ErrForbidden
	}
	if sess == nil {
		return nil
	}
	return credentials(u, sess)
}

// jobStore is the job store for a caller whom allowed lets in; the
// permission is checked first, so that a refusal reads the same on every
// server.
func (s *Service) jobStore(allowed error) (*jobstore.Jobs, error) {
	if allowed != nil {
		return nil, allowed
	}
	if s.deps.Jobs == nil {
		return nil, errNoJobStore
	}
	return s.deps.Jobs, nil
}

// ownJob refuses a change of a child or a repo's node, which its repo
// declares, by the job's name in the set served now.
func (s *Service) ownJob(name string) error {
	j := s.deps.Dispatch.Snapshot().Jobs[name]
	switch {
	case j == nil:
		return nil
	case j.Parent != "":
		path := ".kipp.yml"
		if j.Multibranch != nil && j.Multibranch.Path != "" {
			path = j.Multibranch.Path
		}
		return fmt.Errorf("%w: %s is built from the %s of its repo, which multibranch job %s reads; change it there", ErrConflict, name, path, j.Parent)
	case j.Folder != "":
		return fmt.Errorf("%w: %s is a repo of owner job %s, which makes it; change the owner job", ErrConflict, name, j.Folder)
	}
	return nil
}

// JobDefinition is the job name of the server's own as it serves, for an
// admin.
func (s *Service) JobDefinition(_ context.Context, u *store.User, name string) (jobstore.Definition, error) {
	js, err := s.jobStore(readsJobs(u))
	if err == nil {
		err = s.ownJob(name)
	}
	if err != nil {
		return jobstore.Definition{}, err
	}
	return js.Definition(name)
}

// CreateJob adds the job name with text, through how (api, web or mcp).
func (s *Service) CreateJob(ctx context.Context, u *store.User, sess *store.Session, name string, text []byte, how string) (jobstore.Definition, error) {
	return s.changeJob(ctx, u, sess, name, "job.create", func(js *jobstore.Jobs) (*store.JobVersion, error) {
		return js.Create(ctx, name, text, jobstore.Who{Author: u.Name, How: how})
	})
}

// ReplaceJob changes the job name to text, when ifMatch is its ETag.
func (s *Service) ReplaceJob(ctx context.Context, u *store.User, sess *store.Session, name string, text []byte, ifMatch, how string) (jobstore.Definition, error) {
	return s.changeJob(ctx, u, sess, name, "job.update", func(js *jobstore.Jobs) (*store.JobVersion, error) {
		return js.Replace(ctx, name, text, ifMatch, jobstore.Who{Author: u.Name, How: how})
	})
}

// DeleteJob deletes the job name, when ifMatch is its ETag.
func (s *Service) DeleteJob(ctx context.Context, u *store.User, sess *store.Session, name, ifMatch, how string) error {
	_, err := s.changeJob(ctx, u, sess, name, "job.delete", func(js *jobstore.Jobs) (*store.JobVersion, error) {
		return js.Delete(ctx, name, ifMatch, jobstore.Who{Author: u.Name, How: how})
	})
	return err
}

// RestoreJob makes the text of the job's version id its newest version.
func (s *Service) RestoreJob(ctx context.Context, u *store.User, sess *store.Session, name string, id int64, ifMatch string) (jobstore.Definition, error) {
	return s.changeJob(ctx, u, sess, name, "job.revert", func(js *jobstore.Jobs) (*store.JobVersion, error) {
		v, err := js.Restore(ctx, name, id, ifMatch, u.Name)
		return v, missing(err)
	})
}

// changeJob runs change for an admin allowed to make it, records it in the
// audit, and is the job as it then serves (none once deleted).
func (s *Service) changeJob(ctx context.Context, u *store.User, sess *store.Session, name, action string,
	change func(*jobstore.Jobs) (*store.JobVersion, error),
) (jobstore.Definition, error) {
	js, err := s.jobStore(editsJobs(u, sess))
	if err == nil {
		err = s.ownJob(name)
	}
	if err != nil {
		return jobstore.Definition{}, err
	}
	v, err := change(js)
	if err != nil {
		return jobstore.Definition{}, err
	}
	s.record(ctx, u.Name, action, name, "version "+strconv.FormatInt(v.ID, 10))
	if v.Deleted {
		return jobstore.Definition{Job: name}, nil
	}
	return js.Definition(name)
}

// JobVersions is a page of the job's versions, newest first: with before,
// the ones older than that version.
func (s *Service) JobVersions(ctx context.Context, u *store.User, name string, before int64, limit int) (page []*store.JobVersion, more bool, err error) {
	js, err := s.jobStore(readsJobs(u))
	if err != nil {
		return nil, false, err
	}
	n := rows(limit, defaultVersions)
	found, err := js.Versions(ctx, name, before, n+1)
	if err != nil {
		return nil, false, err
	}
	page, more = trim(found, n)
	return page, more, nil
}

// JobVersion is one version of the job.
func (s *Service) JobVersion(ctx context.Context, u *store.User, name string, id int64) (*store.JobVersion, error) {
	js, err := s.jobStore(readsJobs(u))
	if err != nil {
		return nil, err
	}
	v, err := js.Version(ctx, name, id)
	return v, missing(err)
}

// DeletedJobs are the jobs deleted and not made again, each with its
// deletion, by name.
func (s *Service) DeletedJobs(ctx context.Context, u *store.User) ([]*store.JobVersion, error) {
	js, err := s.jobStore(readsJobs(u))
	if err != nil {
		return nil, err
	}
	return js.Deleted(ctx)
}

// JobEditMode is serve --job-edits, off on a server with no job store.
func (s *Service) JobEditMode() jobstore.Mode {
	if s.deps.Jobs == nil {
		return jobstore.Off
	}
	return s.deps.Jobs.Mode()
}

// JobConflicts are the edits that wait while their files serve.
func (s *Service) JobConflicts(_ context.Context, u *store.User) ([]jobstore.Conflict, error) {
	js, err := s.jobStore(readsJobs(u))
	if err != nil {
		return nil, err
	}
	return js.Conflicts(), nil
}

// ResolveJob settles the edit of name that waits, keeping the file or the
// edit.
func (s *Service) ResolveJob(ctx context.Context, u *store.User, sess *store.Session, name, keep string) error {
	js, err := s.jobStore(editsJobs(u, sess))
	if err == nil && keep != jobstore.KeepFile && keep != jobstore.KeepEdit {
		err = fmt.Errorf("%w: keep is %s or %s", ErrBadInput, jobstore.KeepFile, jobstore.KeepEdit)
	}
	if err == nil {
		err = js.Resolve(ctx, name, keep, u.Name)
	}
	if err != nil {
		return err
	}
	s.record(ctx, u.Name, "job.resolve", name, "kept the "+keep)
	return nil
}

// failJob answers an error of the job store, and hands any other to fail.
func (a *API) failJob(w http.ResponseWriter, r *http.Request, err error) {
	var refused *config.Errors
	var write *jobstore.WriteError
	switch {
	case errors.As(err, &refused):
		items := make([]string, len(refused.Items))
		for i, e := range refused.Items {
			items[i] = e.Error()
		}
		problem.WriteErrors(w, problem.JobRefused, strings.Join(items, "\n"), items)
	case errors.As(err, &write):
		a.deps.Logger.Error("api: a job file could not be written", "path", write.Path, "err", write.Err)
		problem.Write(w, problem.JobWriteFailed, err.Error())
	case errors.Is(err, jobstore.ErrEditsOff):
		problem.Write(w, problem.EditsOff, err.Error())
	case errors.Is(err, jobstore.ErrFilesChanged):
		problem.Write(w, problem.FilesChanged, err.Error())
	case errors.Is(err, jobstore.ErrPreconditionRequired):
		problem.Write(w, problem.PreconditionNeeded, err.Error())
	case errors.Is(err, jobstore.ErrStale):
		problem.Write(w, problem.PreconditionFailed, err.Error())
	case errors.Is(err, jobstore.ErrExists), errors.Is(err, jobstore.ErrDeletion):
		problem.Write(w, problem.Conflict, err.Error())
	case errors.Is(err, jobstore.ErrNoJob), errors.Is(err, jobstore.ErrNoConflict):
		problem.Write(w, problem.NotFound, err.Error())
	case errors.Is(err, errNoJobStore):
		problem.Write(w, problem.EditsOff, err.Error())
	default:
		a.fail(w, r, err)
	}
}

// definitionJSON is a job as it serves, its YAML as written.
type definitionJSON struct {
	Name     string `json:"name"`
	YAML     string `json:"yaml"`
	ETag     string `json:"etag"`
	Source   string `json:"source"`
	Conflict bool   `json:"conflict"`
}

func etagOf(hash string) string { return `"` + hash + `"` }

// writeDefinition answers d with its ETag in the header too.
func writeDefinition(w http.ResponseWriter, code int, d jobstore.Definition) {
	w.Header().Set("ETag", etagOf(d.Hash))
	writeJSON(w, code, definitionJSON{Name: d.Job, YAML: string(d.Text), ETag: etagOf(d.Hash), Source: string(d.Origin), Conflict: d.Conflict})
}

type jobVersionJSON struct {
	ID      int64   `json:"id"`
	Job     string  `json:"job"`
	Deleted bool    `json:"deleted"`
	Hash    *string `json:"hash"`
	Author  string  `json:"author"`
	How     string  `json:"how"`
	At      stamp   `json:"at"`
}

type jobVersionDetailJSON struct {
	jobVersionJSON
	YAML *string `json:"yaml"`
}

func toJobVersionJSON(v *store.JobVersion) jobVersionJSON {
	return jobVersionJSON{ID: v.ID, Job: v.Job, Deleted: v.Deleted, Hash: orNil(v.Hash), Author: v.Author, How: v.How, At: stampOf(v.At)}
}

type conflictJSON struct {
	Job          string  `json:"job"`
	Reason       string  `json:"reason"`
	File         *string `json:"file"`
	FileHash     *string `json:"file_hash"`
	Edit         *string `json:"edit"`
	EditVersion  int64   `json:"edit_version"`
	EditDeletion bool    `json:"edit_deletion"`
	Base         *string `json:"base"`
}

func toConflictJSON(c jobstore.Conflict) conflictJSON {
	out := conflictJSON{Job: c.Job, Reason: c.Reason, FileHash: orNil(c.FileHash), EditVersion: c.Edit.ID, EditDeletion: c.Edit.Deleted, Base: orNil(c.Base)}
	if c.File != nil {
		out.File = new(string(c.File))
	}
	if !c.Edit.Deleted {
		out.Edit = new(c.Edit.Text)
	}
	return out
}

// jobBody reads a job's YAML and, on a create, its name.
type jobBody struct {
	Name string `json:"name"`
	YAML string `json:"yaml"`
}

func readJobBody(r *http.Request, named bool) (jobBody, error) {
	var b jobBody
	empty, err := optionalBody(r, &b)
	switch {
	case err != nil:
		return b, err
	case empty || b.YAML == "":
		return b, fmt.Errorf("%w: the body is {\"yaml\": \"<the job file>\"}", ErrBadInput)
	case named && b.Name == "":
		return b, fmt.Errorf("%w: the body is {\"name\": \"<job>\", \"yaml\": \"<the job file>\"}", ErrBadInput)
	case !named && b.Name != "":
		return b, fmt.Errorf("%w: the job's name is in the path", ErrBadInput)
	}
	return b, nil
}

func (a *API) jobDefinition(w http.ResponseWriter, r *http.Request, u *store.User) {
	d, err := a.svc.JobDefinition(r.Context(), u, r.PathValue("name"))
	if err != nil {
		a.failJob(w, r, err)
		return
	}
	writeDefinition(w, http.StatusOK, d)
}

func (a *API) createJob(w http.ResponseWriter, r *http.Request, u *store.User) {
	b, err := readJobBody(r, true)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	d, err := a.svc.CreateJob(r.Context(), u, SessionFrom(r.Context()), b.Name, []byte(b.YAML), store.HowAPI)
	if err != nil {
		a.failJob(w, r, err)
		return
	}
	w.Header().Set("Location", PrefixV1+"/jobs/"+d.Job+"/definition")
	writeDefinition(w, http.StatusCreated, d)
}

func (a *API) replaceJob(w http.ResponseWriter, r *http.Request, u *store.User) {
	b, err := readJobBody(r, false)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	d, err := a.svc.ReplaceJob(r.Context(), u, SessionFrom(r.Context()), r.PathValue("name"), []byte(b.YAML), r.Header.Get("If-Match"), store.HowAPI)
	if err != nil {
		a.failJob(w, r, err)
		return
	}
	writeDefinition(w, http.StatusOK, d)
}

func (a *API) deleteJob(w http.ResponseWriter, r *http.Request, u *store.User) {
	if err := a.svc.DeleteJob(r.Context(), u, SessionFrom(r.Context()), r.PathValue("name"), r.Header.Get("If-Match"), store.HowAPI); err != nil {
		a.failJob(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// versionID is the {id} of a version's path.
func versionID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return 0, fmt.Errorf("%w: a version is a number, 1 or more", ErrBadInput)
	}
	return id, nil
}

func (a *API) jobVersions(w http.ResponseWriter, r *http.Request, u *store.User) {
	limit, at, err := pageArgs(r, "versions", 1)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	vs, more, err := a.svc.JobVersions(r.Context(), u, r.PathValue("name"), firstOf(at), limit)
	if err != nil {
		a.failJob(w, r, err)
		return
	}
	out := make([]jobVersionJSON, 0, len(vs))
	for _, v := range vs {
		out = append(out, toJobVersionJSON(v))
	}
	next := ""
	if more {
		next = cursorFor("versions", vs[len(vs)-1].ID)
	}
	writeList(w, out, next)
}

func (a *API) jobVersion(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, err := versionID(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	v, err := a.svc.JobVersion(r.Context(), u, r.PathValue("name"), id)
	if err != nil {
		a.failJob(w, r, err)
		return
	}
	out := jobVersionDetailJSON{jobVersionJSON: toJobVersionJSON(v)}
	if !v.Deleted {
		out.YAML = new(v.Text)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) restoreJob(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, err := versionID(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	d, err := a.svc.RestoreJob(r.Context(), u, SessionFrom(r.Context()), r.PathValue("name"), id, r.Header.Get("If-Match"))
	if err != nil {
		a.failJob(w, r, err)
		return
	}
	writeDefinition(w, http.StatusOK, d)
}

func (a *API) jobConflicts(w http.ResponseWriter, r *http.Request, u *store.User) {
	cs, err := a.svc.JobConflicts(r.Context(), u)
	if err != nil {
		a.failJob(w, r, err)
		return
	}
	out := make([]conflictJSON, 0, len(cs))
	for _, c := range cs {
		out = append(out, toConflictJSON(c))
	}
	writeList(w, out, "")
}

func (a *API) resolveJob(w http.ResponseWriter, r *http.Request, u *store.User) {
	var body struct {
		Keep string `json:"keep"`
	}
	if _, err := optionalBody(r, &body); err != nil {
		a.fail(w, r, err)
		return
	}
	if err := a.svc.ResolveJob(r.Context(), u, SessionFrom(r.Context()), r.PathValue("name"), body.Keep); err != nil {
		a.failJob(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
