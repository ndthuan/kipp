package web

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/storage"
	"github.com/ndthuan/kipp/internal/store"
)

// restoreDocs is where restoring the database by hand is written down, which
// the banner of a failed self-check links to.
const restoreDocs = "https://github.com/ndthuan/kipp/blob/main/docs/operations.md#restoring-the-database-by-hand"

func (s *Site) storageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/storage", s.withUser(s.storagePage))
	mux.HandleFunc("POST /admin/storage/compact", s.withUser(s.compact))
	mux.HandleFunc("GET /admin/storage/jobs/{name}/remove", s.withUser(s.confirmRemoveJob))
	mux.HandleFunc("POST /admin/storage/jobs/{name}/remove", s.withUser(s.removeJob))
}

type storageData struct {
	R      *storage.Report
	Notice string
	// Disk is the disk's line, said once here rather than in the template.
	Disk string
	// Active says a compact runs: the section asks again every few seconds.
	Active bool
	// DocsURL is restoring by hand.
	DocsURL string
}

// partLabels say what each kind of the data directory holds.
var partLabels = map[string]string{
	storage.PartDatabase: "Database", storage.PartLogs: "Logs", storage.PartArtifacts: "Artifacts", storage.PartCache: "Cache",
	storage.PartWorkspaces: "Workspaces", storage.PartStageTemp: "Stage temp", storage.PartUpgradeBackups: "Upgrade backups",
	storage.PartSchemaCopies: "Database before a schema upgrade", storage.PartUpgradeBinaries: "Upgrade binaries and dry runs",
	storage.PartSmallFiles: "Keys and small files", storage.PartOther: "Not kipp's",
}

func (s *Site) storagePage(w http.ResponseWriter, r *http.Request, u *store.User) {
	s.renderStorage(w, r, u, http.StatusOK, "")
}

func (s *Site) renderStorage(w http.ResponseWriter, r *http.Request, u *store.User, code int, msg string) {
	rep, err := s.api.Service().Storage(r.Context(), u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d := storageData{R: rep, Notice: noticeOf(r), DocsURL: restoreDocs,
		Active: rep.Compact.State == storage.CompactWaiting || rep.Compact.State == storage.CompactCompacting}
	if rep.Disk.Known {
		d.Disk = config.Readable(rep.Disk.Free) + " free of " + config.Readable(rep.Disk.Total) +
			"; under " + config.Readable(rep.Disk.Floor) + " kipp cleans what is safe to clean"
	} else {
		d.Disk = "the free space of this disk cannot be read here"
	}
	s.render(w, r, "storage", code, page{Title: "Storage", Here: "/admin/storage", Data: d, Error: msg})
}

func (s *Site) compact(w http.ResponseWriter, r *http.Request, u *store.User) {
	_, err := s.api.Service().StartCompact(r.Context(), u)
	if errors.Is(err, api.ErrConflict) {
		s.renderStorage(w, r, u, http.StatusConflict, "The compact did not start: "+err.Error())
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	done(w, r, "/admin/storage", "compact-started")
}

func (s *Site) confirmRemoveJob(w http.ResponseWriter, r *http.Request, u *store.User) {
	name := r.PathValue("name")
	rep, err := s.api.Service().Storage(r.Context(), u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for _, j := range rep.Jobs {
		if j.Name != name || j.Gone == "" {
			continue
		}
		lines := []string{name + " has no job file any more. Its " + plural(j.Builds-j.Kept, "build") + " that have ended, with their logs and artifacts, about " + config.Readable(j.Bytes()) + ", go now and for good."}
		if !j.Until.IsZero() {
			lines = append(lines, "Left alone, the retention removes them by "+j.Until.UTC().Format("2006-01-02")+".")
		}
		if j.Kept > 0 {
			lines = append(lines, plural(j.Kept, "build")+" kept by hand stay.")
		}
		s.confirm(w, r, "/admin/storage", confirmData{Heading: "Remove the data of " + name + "?", Lines: lines, Button: "Remove now", Back: "/admin/storage"})
		return
	}
	s.fail(w, r, api.ErrNotFound)
}

func (s *Site) removeJob(w http.ResponseWriter, r *http.Request, u *store.User) {
	if _, err := s.api.Service().RemoveJobData(r.Context(), u, r.PathValue("name")); err != nil {
		s.fail(w, r, err)
		return
	}
	done(w, r, "/admin/storage", "storage-removed")
}

// removePath is the address of the confirmation of "remove now" of a job.
func removePath(job string) string { return "/admin/storage/jobs/" + url.PathEscape(job) + "/remove" }

func partLabel(kind string) string { return partLabels[kind] }

// compactTook is how long a compact ran, rounded to the second.
func compactTook(c storage.CompactState) string {
	if c.StartedAt.IsZero() || c.EndedAt.IsZero() {
		return ""
	}
	return c.EndedAt.Sub(c.StartedAt).Round(1e9).String()
}

func rowsText(n int64) string { return strconv.FormatInt(n, 10) }
