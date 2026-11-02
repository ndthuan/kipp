package api

import (
	"context"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"

	"github.com/ndthuan/kipp/internal/artifacts"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/problem"
	"github.com/ndthuan/kipp/internal/store"
)

// ArtifactStatuses are the build statuses latest may be asked for: those
// of a build that has ended.
var ArtifactStatuses = []store.Status{store.Success, store.Unstable, store.Failed, store.Cancelled, store.Timeout, store.Lost}

// StageFiles are the files one stage of a build kept.
type StageFiles struct {
	Stage string
	Files []store.Artifact
}

// Artifacts are what the build's stages kept, stage by stage.
func (s *Service) Artifacts(ctx context.Context, u *store.User, id int64) (*store.Build, []StageFiles, error) {
	b, err := s.build(ctx, u, id, "view")
	if err != nil {
		return nil, nil, err
	}
	rows, err := s.deps.Store.Artifacts(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	return b, byStage(rows), nil
}

func byStage(rows []store.Artifact) []StageFiles {
	var out []StageFiles
	for _, r := range rows {
		if n := len(out); n == 0 || out[n-1].Stage != r.Stage {
			out = append(out, StageFiles{Stage: r.Stage})
		}
		out[len(out)-1].Files = append(out[len(out)-1].Files, r)
	}
	return out
}

// StageArtifacts is one stage's files of a build, with the directory
// they are kept in; a stage that kept nothing is ErrNotFound.
func (s *Service) StageArtifacts(ctx context.Context, u *store.User, id int64, stage string) (*store.Build, string, []store.Artifact, error) {
	b, err := s.build(ctx, u, id, "view")
	if err != nil {
		return nil, "", nil, err
	}
	if !config.ValidName(stage) {
		return nil, "", nil, ErrNotFound
	}
	rows, err := s.deps.Store.StageArtifacts(ctx, id, stage)
	if err == nil && len(rows) == 0 {
		err = ErrNotFound
	}
	return b, s.deps.Dispatch.StageArtifactsDir(b.Job, b.Number, stage), rows, err
}

// LatestArtifacts is the newest build of the job that ended with one of
// statuses and whose stage kept something (path, when it is given).
func (s *Service) LatestArtifacts(ctx context.Context, u *store.User, job, stage, file string, statuses []store.Status) (int64, error) {
	if _, err := s.history(ctx, u, job); err != nil {
		return 0, err
	}
	id, err := s.deps.Store.LatestArtifacts(ctx, job, stage, file, statuses)
	return id, missing(err)
}

type artifactJSON struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	URL    string `json:"url"`
}

type stageArtifactsJSON struct {
	Stage string         `json:"stage"`
	Zip   string         `json:"zip"`
	Files []artifactJSON `json:"files"`
}

// ArtifactURL is where one kept file is downloaded, each part of its path
// escaped on its own.
func ArtifactURL(id int64, stage, file string) string {
	parts := strings.Split(file, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return fmt.Sprintf("%s/builds/%d/stages/%s/artifacts/%s", PrefixV1, id, stage, strings.Join(parts, "/"))
}

// ArtifactsZipURL is where a stage's files are downloaded as one zip.
func ArtifactsZipURL(id int64, stage string) string {
	return fmt.Sprintf("%s/builds/%d/stages/%s/artifacts.zip", PrefixV1, id, stage)
}

// stageArtifactsOf is one stage's kept files as the API lists them, each
// with the URL it is downloaded from.
func stageArtifactsOf(id int64, stage string, files []store.Artifact) stageArtifactsJSON {
	row := stageArtifactsJSON{Stage: stage, Zip: ArtifactsZipURL(id, stage), Files: make([]artifactJSON, len(files))}
	for i, f := range files {
		row.Files[i] = artifactJSON{Path: f.Path, Size: f.Size, SHA256: f.SHA256, URL: ArtifactURL(id, stage, f.Path)}
	}
	return row
}

func (a *API) artifactList(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, err := BuildID(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	_, stages, err := a.svc.Artifacts(r.Context(), u, id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := make([]stageArtifactsJSON, 0, len(stages))
	for _, st := range stages {
		out = append(out, stageArtifactsOf(id, st.Stage, st.Files))
	}
	writeList(w, out, "")
}

// stageArtifactList answers one stage's kept files.
func (a *API) stageArtifactList(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, err := BuildID(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	stage := r.PathValue("stage")
	_, _, rows, err := a.svc.StageArtifacts(r.Context(), u, id, stage)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, stageArtifactsOf(id, stage, rows))
}

// attachment marks an answer as a file to save, whatever it holds: an
// HTML file shown inline would run with the console's origin, so it is
// never given a type a browser renders, never sniffed into one, and
// sandboxed should a browser open it all the same.
func attachment(w http.ResponseWriter, name string) {
	sealed(w)
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Security-Policy", "sandbox")
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": name})
	if disposition == "" {
		disposition = "attachment"
	}
	h.Set("Content-Disposition", disposition)
}

func (a *API) artifactFile(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, err := BuildID(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	file := r.PathValue("path")
	_, dir, rows, err := a.svc.StageArtifacts(r.Context(), u, id, r.PathValue("stage"))
	if err == nil && !slices.ContainsFunc(rows, func(row store.Artifact) bool { return row.Path == file }) {
		err = ErrNotFound
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	f, err := artifacts.Open(dir, file)
	if err != nil {
		a.fail(w, r, ErrNotFound)
		return
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		a.fail(w, r, err)
		return
	}
	attachment(w, path.Base(file))
	// Its 416 is a problem, as the rest of the failures are.
	http.ServeContent(problem.Catch(w), r, "", info.ModTime(), f)
}

// artifactZip answers one stage's files as one zip, made as it is sent.
func (a *API) artifactZip(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, err := BuildID(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	stage := r.PathValue("stage")
	b, dir, rows, err := a.svc.StageArtifacts(r.Context(), u, id, stage)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	files := make([]artifacts.File, len(rows))
	for i, row := range rows {
		files[i] = artifacts.File{Path: row.Path, Size: row.Size, SHA256: row.SHA256}
	}
	attachment(w, fmt.Sprintf("%s-%d-%s.zip", b.Job, b.Number, stage))
	if err := artifacts.Zip(w, dir, files); err != nil {
		// The answer has begun: the zip ends short, and the log says why.
		a.deps.Logger.Warn("sending a stage's artifacts as a zip", "build", id, "stage", stage, "err", err)
	}
}

// latestArtifacts redirects to the newest build of the job that ended as
// ?status= says (success when it says nothing) and whose ?stage= kept
// something: to its listing, or with ?path= to that file of it.
func (a *API) latestArtifacts(w http.ResponseWriter, r *http.Request, u *store.User) {
	q := r.URL.Query()
	stage, file := q.Get("stage"), q.Get("path")
	statuses, err := artifactStatuses(q["status"])
	switch {
	case err != nil:
	case !config.ValidName(stage):
		err = fmt.Errorf("%w: stage: the name of one of the job's stages", ErrBadInput)
	case file != "" && !artifacts.ValidPath(file):
		err = fmt.Errorf("%w: path: a file's path inside the stage's directory", ErrBadInput)
	}
	var id int64
	if err == nil {
		id, err = a.svc.LatestArtifacts(r.Context(), u, r.PathValue("name"), stage, file, statuses)
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	to := fmt.Sprintf("%s/builds/%d/stages/%s/artifacts", PrefixV1, id, stage)
	if file != "" {
		to = ArtifactURL(id, stage, file)
	}
	sealed(w)
	http.Redirect(w, r, to, http.StatusFound)
}

// artifactStatuses reads ?status=, given once or more, each one status or
// several comma-separated.
func artifactStatuses(given []string) ([]store.Status, error) {
	var out []store.Status
	for _, v := range given {
		for item := range strings.SplitSeq(v, ",") {
			st := store.Status(strings.TrimSpace(item))
			if !slices.Contains(ArtifactStatuses, st) {
				return nil, fmt.Errorf("%w: status %q: one or more of %v", ErrBadInput, item, ArtifactStatuses)
			}
			out = append(out, st)
		}
	}
	if len(out) == 0 {
		out = []store.Status{store.Success}
	}
	return out, nil
}
