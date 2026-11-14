package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/jobstore"
	"github.com/ndthuan/kipp/internal/store"
)

// Tools that read and change the job files are for an admin whose
// credential may do all an admin may (Ceiling == AllGrants). A narrower
// ceiling, such as a read-only OAuth grant, must not edit jobs, and no
// scope exists for it, so those callers are not shown the tools at all.

// kind is which of the server's tool sets a caller is served.
type kind int

const (
	plain    kind = iota // the tools every caller has
	jobsRead             // plus the tools that read job definitions
	jobsEdit             // plus the tools that change them
)

// editsJobs is whether c may use the job tools.
func editsJobs(c *Caller) bool { return c.User.Admin && c.Ceiling == AllGrants }

// kindFor is the tool set served to the request's caller; edits off keeps
// the tools that change jobs out of the list.
func (s *Server) kindFor(info *auth.TokenInfo) kind {
	var c *Caller
	if info != nil {
		c, _ = info.Extra[callerKey].(*Caller)
	}
	switch {
	case c == nil || !editsJobs(c):
		return plain
	case s.cfg.Service.JobEditMode() == jobstore.Off:
		return jobsRead
	}
	return jobsEdit
}

// toldError is an error already in the words a model should read.
type toldError struct{ msg string }

func (e *toldError) Error() string { return e.msg }

func edits() *sdk.ToolAnnotations {
	return &sdk.ToolAnnotations{DestructiveHint: new(true), IdempotentHint: false, OpenWorldHint: new(false)}
}

func creates() *sdk.ToolAnnotations {
	return &sdk.ToolAnnotations{DestructiveHint: new(false), IdempotentHint: false, OpenWorldHint: new(false)}
}

func (s *Server) addJobTools(srv *sdk.Server, k kind) {
	if k == plain {
		return
	}
	add(s, srv, "get_job_yaml", "A job's YAML as written, with its etag, where it comes from and whether an edit waits in conflict. Admins only.", reads(), s.jobYAML)
	add(s, srv, "list_job_versions", "A job's versions, newest first, who made each and through what. Pass next_before as before for older ones. Also works for a deleted job. Admins only.", reads(), s.jobVersions)
	add(s, srv, "get_job_version", "One version of a job with its YAML (none for a deletion). Admins only.", reads(), s.jobVersion)
	if k != jobsEdit {
		return
	}
	add(s, srv, "create_job", "Create a job from its YAML; its name is the name: in the file. Admins only.", creates(), s.createJob)
	add(s, srv, "replace_job", "Change a job to new YAML. etag is the one get_job_yaml gave; a stale etag is refused with the current YAML and etag, so read it and try again. Admins only.", edits(), s.replaceJob)
	add(s, srv, "delete_job", "Delete a job; etag as for replace_job. Its versions are kept and restore_job can bring it back. Admins only.", edits(), s.deleteJob)
	add(s, srv, "restore_job", "Make an earlier version the job's newest. etag is the job's current one, and is not needed when the job was deleted. Settling a conflict between a file and an edit is for the web page and the command line. Admins only.", edits(), s.restoreJob)
}

// jobCaller is the caller when it may use the job tools, else the
// refusal for a tool that should not have been reachable.
func jobCaller(c *Caller) error {
	if !editsJobs(c) {
		return api.ErrForbidden
	}
	return nil
}

// jobFailure answers an error of the job store in words for a model.
func (s *Server) jobFailure(ctx context.Context, c *Caller, name string, err error) error {
	var refused *config.Errors
	var write *jobstore.WriteError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &refused):
		lines := make([]string, len(refused.Items))
		for i, e := range refused.Items {
			lines[i] = "- " + e.Error()
		}
		return &toldError{"refused: the YAML is not a valid job set:\n" + strings.Join(lines, "\n")}
	case errors.As(err, &write):
		s.log.Error("mcp: a job file could not be written", slog.String("path", write.Path), "err", write.Err)
		return &toldError{"the job file could not be written; see the server's log"}
	case errors.Is(err, jobstore.ErrStale):
		msg := "refused: " + err.Error() + "."
		if d, derr := s.cfg.Service.JobDefinition(ctx, c.User, name); derr == nil {
			msg += fmt.Sprintf(" The job now has etag %s and reads:\n%s", etagOf(d.Hash), d.Text)
		}
		return &toldError{msg}
	case errors.Is(err, jobstore.ErrPreconditionRequired):
		return &toldError{"bad input: " + err.Error() + " (get_job_yaml gives it)"}
	case errors.Is(err, jobstore.ErrNoJob), errors.Is(err, jobstore.ErrNoConflict):
		return api.ErrNotFound
	case errors.Is(err, jobstore.ErrEditsOff), errors.Is(err, jobstore.ErrFilesChanged),
		errors.Is(err, jobstore.ErrExists), errors.Is(err, jobstore.ErrDeletion):
		return &toldError{"refused: " + err.Error()}
	}
	return err
}

func etagOf(hash string) string { return `"` + hash + `"` }

type jobIn struct {
	Job string `json:"job" jsonschema:"the job's name"`
}

type definitionOut struct {
	Job      string `json:"job"`
	YAML     string `json:"yaml"`
	ETag     string `json:"etag" jsonschema:"pass it back as etag to change or delete the job"`
	Source   string `json:"source" jsonschema:"file, store or store over file"`
	Conflict bool   `json:"conflict" jsonschema:"an edit waits while the file serves; settle it on the web or the command line"`
}

func definitionOf(d jobstore.Definition) definitionOut {
	return definitionOut{Job: d.Job, YAML: string(d.Text), ETag: etagOf(d.Hash), Source: string(d.Origin), Conflict: d.Conflict}
}

func (s *Server) jobYAML(ctx context.Context, c *Caller, in jobIn) (definitionOut, error) {
	if err := jobCaller(c); err != nil {
		return definitionOut{}, err
	}
	d, err := s.cfg.Service.JobDefinition(ctx, c.User, in.Job)
	if err != nil {
		return definitionOut{}, s.jobFailure(ctx, c, in.Job, err)
	}
	return definitionOf(d), nil
}

type createJobIn struct {
	YAML string `json:"yaml" jsonschema:"the whole job file; the job is named by its name: line"`
}

func (s *Server) createJob(ctx context.Context, c *Caller, in createJobIn) (definitionOut, error) {
	if err := jobCaller(c); err != nil {
		return definitionOut{}, err
	}
	name, err := config.NameOf([]byte(in.YAML))
	if err != nil {
		return definitionOut{}, badInput("%v", err)
	}
	d, err := s.cfg.Service.CreateJob(ctx, c.User, nil, name, []byte(in.YAML), store.HowMCP)
	if err != nil {
		return definitionOut{}, s.jobFailure(ctx, c, name, err)
	}
	return definitionOf(d), nil
}

type replaceJobIn struct {
	Job  string `json:"job" jsonschema:"the job's name"`
	YAML string `json:"yaml" jsonschema:"the whole new job file"`
	ETag string `json:"etag" jsonschema:"the etag get_job_yaml gave"`
}

func (s *Server) replaceJob(ctx context.Context, c *Caller, in replaceJobIn) (definitionOut, error) {
	if err := jobCaller(c); err != nil {
		return definitionOut{}, err
	}
	d, err := s.cfg.Service.ReplaceJob(ctx, c.User, nil, in.Job, []byte(in.YAML), in.ETag, store.HowMCP)
	if err != nil {
		return definitionOut{}, s.jobFailure(ctx, c, in.Job, err)
	}
	return definitionOf(d), nil
}

type deleteJobIn struct {
	Job  string `json:"job" jsonschema:"the job's name"`
	ETag string `json:"etag" jsonschema:"the etag get_job_yaml gave"`
}

type deletedOut struct {
	Job     string `json:"job"`
	Deleted bool   `json:"deleted"`
}

func (s *Server) deleteJob(ctx context.Context, c *Caller, in deleteJobIn) (deletedOut, error) {
	if err := jobCaller(c); err != nil {
		return deletedOut{}, err
	}
	if err := s.cfg.Service.DeleteJob(ctx, c.User, nil, in.Job, in.ETag, store.HowMCP); err != nil {
		return deletedOut{}, s.jobFailure(ctx, c, in.Job, err)
	}
	return deletedOut{Job: in.Job, Deleted: true}, nil
}

type restoreJobIn struct {
	Job     string `json:"job" jsonschema:"the job's name"`
	Version int64  `json:"version" jsonschema:"a version's id from list_job_versions"`
	ETag    string `json:"etag,omitempty" jsonschema:"the job's current etag; not needed for a job that was deleted"`
}

func (s *Server) restoreJob(ctx context.Context, c *Caller, in restoreJobIn) (definitionOut, error) {
	if err := jobCaller(c); err != nil {
		return definitionOut{}, err
	}
	if in.Version < 1 {
		return definitionOut{}, badInput("version is a number, 1 or more")
	}
	d, err := s.cfg.Service.RestoreJob(ctx, c.User, nil, in.Job, in.Version, in.ETag)
	if err != nil {
		return definitionOut{}, s.jobFailure(ctx, c, in.Job, err)
	}
	return definitionOf(d), nil
}

type versionOut struct {
	ID      int64  `json:"id"`
	Job     string `json:"job"`
	Deleted bool   `json:"deleted"`
	Hash    string `json:"hash,omitempty"`
	Author  string `json:"author"`
	How     string `json:"how" jsonschema:"api, web or mcp"`
	At      string `json:"at"`
	YAML    string `json:"yaml,omitempty"`
}

func versionOf(v *store.JobVersion, withText bool) versionOut {
	out := versionOut{ID: v.ID, Job: v.Job, Deleted: v.Deleted, Hash: v.Hash, Author: v.Author, How: v.How, At: stamp(v.At)}
	if withText && !v.Deleted {
		out.YAML = v.Text
	}
	return out
}

type jobVersionsIn struct {
	Job    string `json:"job" jsonschema:"the job's name"`
	Limit  int    `json:"limit,omitempty" jsonschema:"how many versions, default 50"`
	Before int64  `json:"before,omitempty" jsonschema:"a next_before of an earlier answer, for older versions"`
}

type jobVersionsOut struct {
	Versions   []versionOut `json:"versions"`
	NextBefore int64        `json:"next_before,omitempty"`
}

func (s *Server) jobVersions(ctx context.Context, c *Caller, in jobVersionsIn) (jobVersionsOut, error) {
	out := jobVersionsOut{Versions: []versionOut{}}
	if err := jobCaller(c); err != nil {
		return out, err
	}
	page, more, err := s.cfg.Service.JobVersions(ctx, c.User, in.Job, in.Before, in.Limit)
	if err != nil {
		return out, s.jobFailure(ctx, c, in.Job, err)
	}
	for _, v := range page {
		out.Versions = append(out.Versions, versionOf(v, false))
	}
	if more {
		out.NextBefore = page[len(page)-1].ID
	}
	return out, nil
}

type jobVersionIn struct {
	Job     string `json:"job" jsonschema:"the job's name"`
	Version int64  `json:"version" jsonschema:"a version's id from list_job_versions"`
}

func (s *Server) jobVersion(ctx context.Context, c *Caller, in jobVersionIn) (versionOut, error) {
	if err := jobCaller(c); err != nil {
		return versionOut{}, err
	}
	v, err := s.cfg.Service.JobVersion(ctx, c.User, in.Job, in.Version)
	if err != nil {
		return versionOut{}, s.jobFailure(ctx, c, in.Job, err)
	}
	return versionOf(v, true), nil
}
