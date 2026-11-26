package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/storage"
	"github.com/ndthuan/kipp/internal/store"
)

const restoreDocs = "https://github.com/ndthuan/kipp/blob/main/docs/operations.md#restoring-the-database-by-hand"

func (s *Site) storageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/housekeeping", s.withUser(s.housekeepingPage))
	mux.HandleFunc("GET /admin/housekeeping/sections/{name}", s.withUser(s.housekeepingSection))
	mux.HandleFunc("POST /admin/housekeeping/compact", s.withUser(s.compact))
	mux.HandleFunc("GET /admin/housekeeping/jobs/{name}/remove", s.withUser(s.confirmRemoveJob))
	mux.HandleFunc("POST /admin/housekeeping/jobs/{name}/remove", s.withUser(s.removeJob))
	mux.HandleFunc("GET /admin/storage", func(w http.ResponseWriter, r *http.Request) {
		movedTo(w, r, "/admin/housekeeping")
	})
	mux.HandleFunc("GET /admin/storage/jobs/{name}/remove", func(w http.ResponseWriter, r *http.Request) {
		movedTo(w, r, removePath(r.PathValue("name")))
	})
}

func movedTo(w http.ResponseWriter, r *http.Request, to string) {
	if r.URL.RawQuery != "" {
		to += "?" + r.URL.RawQuery
	}
	redirect(w, r, to, http.StatusMovedPermanently)
}

type housekeepingSection struct {
	Name    string
	Heading string
	Panel   bool
}

var housekeepingSections = []housekeepingSection{
	{Name: "facts"}, {Name: "compact", Heading: "Compact", Panel: true}, {Name: "recommendations", Heading: "Recommendations"},
	{Name: "parts", Heading: "Data directory"}, {Name: "database", Heading: "Database"}, {Name: "jobs", Heading: "Jobs"},
	{Name: "self-check", Heading: "Self-check", Panel: true}, {Name: "heal"},
}

var housekeepingTabs = []string{"recommendations", "parts", "database", "jobs"}

type housekeepingData struct {
	Sections []housekeepingSection
	Notice   string
}

// Section is the block of that name, for the template to place where the
// layout wants it.
func (d housekeepingData) Section(name string) housekeepingSection {
	for _, s := range d.Sections {
		if s.Name == name {
			return s
		}
	}
	return housekeepingSection{Name: name}
}

// Tabs are the sections that sit in the tab panels.
func (d housekeepingData) Tabs() []housekeepingSection {
	tabs := make([]housekeepingSection, len(housekeepingTabs))
	for i, name := range housekeepingTabs {
		tabs[i] = d.Section(name)
	}
	return tabs
}

type sectionData struct {
	R       *storage.Report
	Parts   []storage.Part
	Jobs    []storage.Job
	Err     string
	Disk    string
	Active  bool
	DocsURL string
	CSRF    string
}

var partLabels = map[string]string{
	storage.PartDatabase: "Database", storage.PartLogs: "Logs", storage.PartArtifacts: "Artifacts", storage.PartCache: "Cache",
	storage.PartWorkspaces: "Workspaces", storage.PartStageTemp: "Stage temp", storage.PartUpgradeBackups: "Upgrade backups",
	storage.PartSchemaCopies: "Database before a schema upgrade", storage.PartUpgradeBinaries: "Upgrade binaries and dry runs",
	storage.PartSmallFiles: "Keys and small files", storage.PartOther: "Not kipp's",
}

func (s *Site) housekeepingPage(w http.ResponseWriter, r *http.Request, u *store.User) {
	s.renderHousekeeping(w, r, u, http.StatusOK, "")
}

func (s *Site) renderHousekeeping(w http.ResponseWriter, r *http.Request, u *store.User, code int, msg string) {
	if err := s.api.Service().CheckStorage(u); err != nil {
		s.fail(w, r, err)
		return
	}
	d := housekeepingData{Sections: housekeepingSections, Notice: noticeOf(r)}
	s.render(w, r, "housekeeping", code, page{Title: "Housekeeping", Here: "/admin/housekeeping", Data: d, Error: msg})
}

var sectionReaders = map[string]func(*Site, context.Context, *store.User, *sectionData) error{
	"facts": (*Site).readOverview, "compact": (*Site).readOverview, "database": (*Site).readOverview,
	"self-check": (*Site).readOverview, "heal": (*Site).readOverview,
	"recommendations": func(s *Site, ctx context.Context, u *store.User, d *sectionData) (err error) {
		d.R, err = s.api.Service().Storage(ctx, u)
		return err
	},
	"parts": func(s *Site, _ context.Context, u *store.User, d *sectionData) (err error) {
		d.Parts, err = s.api.Service().StorageParts(u)
		return err
	},
	"jobs": func(s *Site, ctx context.Context, u *store.User, d *sectionData) (err error) {
		d.Jobs, err = s.api.Service().StorageJobs(ctx, u)
		return err
	},
}

func (s *Site) readOverview(ctx context.Context, u *store.User, d *sectionData) (err error) {
	d.R, err = s.api.Service().StorageOverview(ctx, u)
	return err
}

func diskLine(d storage.Disk) string {
	if !d.Known {
		return "the free space of this disk cannot be read here"
	}
	return config.Readable(d.Free) + " free of " + config.Readable(d.Total) +
		"; under " + config.Readable(d.Floor) + " kipp cleans what is safe to clean"
}

func (s *Site) housekeepingSection(w http.ResponseWriter, r *http.Request, u *store.User) {
	name := r.PathValue("name")
	read, ok := sectionReaders[name]
	if !ok {
		s.fail(w, r, api.ErrNotFound)
		return
	}
	d := &sectionData{DocsURL: restoreDocs}
	if sess := api.SessionFrom(r.Context()); sess != nil {
		d.CSRF = sess.CSRF
	}
	if err := read(s, r.Context(), u, d); err != nil {
		if errors.Is(err, api.ErrForbidden) {
			s.fail(w, r, err)
			return
		}
		slog.Error("web: housekeeping section", "section", name, "err", err)
		d.Err = "This could not be read: " + err.Error()
	} else if d.R != nil {
		d.Disk = diskLine(d.R.Disk)
		d.Active = d.R.Compact.State == storage.CompactWaiting || d.R.Compact.State == storage.CompactCompacting
	}
	s.fragment(w, "housekeeping_sections", name, d)
}

func (s *Site) compact(w http.ResponseWriter, r *http.Request, u *store.User) {
	_, err := s.api.Service().StartCompact(r.Context(), u)
	if errors.Is(err, api.ErrConflict) {
		s.renderHousekeeping(w, r, u, http.StatusConflict, "The compact did not start: "+err.Error())
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	done(w, r, "/admin/housekeeping", "compact-started")
}

func (s *Site) confirmRemoveJob(w http.ResponseWriter, r *http.Request, u *store.User) {
	name := r.PathValue("name")
	jobs, err := s.api.Service().StorageJobs(r.Context(), u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for _, j := range jobs {
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
		s.confirm(w, r, "/admin/housekeeping", confirmData{Heading: "Remove the data of " + name + "?", Lines: lines, Button: "Remove now", Back: "/admin/housekeeping"})
		return
	}
	s.fail(w, r, api.ErrNotFound)
}

func (s *Site) removeJob(w http.ResponseWriter, r *http.Request, u *store.User) {
	if _, err := s.api.Service().RemoveJobData(r.Context(), u, r.PathValue("name")); err != nil {
		s.fail(w, r, err)
		return
	}
	done(w, r, "/admin/housekeeping", "storage-removed")
}

func removePath(job string) string {
	return "/admin/housekeeping/jobs/" + url.PathEscape(job) + "/remove"
}

func partLabel(kind string) string { return partLabels[kind] }

func compactTook(c storage.CompactState) string {
	if c.StartedAt.IsZero() || c.EndedAt.IsZero() {
		return ""
	}
	return c.EndedAt.Sub(c.StartedAt).Round(1e9).String()
}

func rowsText(n int64) string { return strconv.FormatInt(n, 10) }
