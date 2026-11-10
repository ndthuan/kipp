package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/ndthuan/kipp/internal/storage"
	"github.com/ndthuan/kipp/internal/store"
)

// errNoStorage is the answer of a server that was given no storage.
var errNoStorage = fmt.Errorf("%w: this server keeps no storage report", ErrConflict)

// storageErr turns what the storage refuses into the API's own kinds.
func storageErr(err error) error {
	var room *storage.RoomError
	switch {
	case errors.Is(err, storage.ErrCompactBusy), errors.As(err, &room), errors.Is(err, storage.ErrHasFile):
		return fmt.Errorf("%w: %w", ErrConflict, err)
	case errors.Is(err, storage.ErrNothingLeft):
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	return err
}

// Storage is what the data directory holds and what to change, for an
// admin; a token may read it.
func (s *Service) Storage(ctx context.Context, u *store.User) (*storage.Report, error) {
	if !u.Admin {
		return nil, ErrForbidden
	}
	if s.deps.Storage == nil {
		return nil, errNoStorage
	}
	return s.deps.Storage.Report(ctx)
}

// CompactState is the compact that runs or ran last, for an admin.
func (s *Service) CompactState(u *store.User) (storage.CompactState, error) {
	if !u.Admin {
		return storage.CompactState{}, ErrForbidden
	}
	if s.deps.Storage == nil {
		return storage.CompactState{}, errNoStorage
	}
	return s.deps.Storage.CompactState(), nil
}

// StartCompact begins a compact of the database. Like quiet it is an
// admin's, token or browser alike; it writes its own row of the audit.
func (s *Service) StartCompact(ctx context.Context, u *store.User) (storage.CompactState, error) {
	if !u.Admin {
		return storage.CompactState{}, ErrForbidden
	}
	if s.deps.Storage == nil {
		return storage.CompactState{}, errNoStorage
	}
	c, err := s.deps.Storage.StartCompact(ctx, u.Name)
	return c, storageErr(err)
}

// RemoveJobData is "remove now" of a job whose file is gone: its ended
// builds, logs and artifacts. It is an admin's, token or browser alike, as
// the retention it brings forward is nobody's credential; the page asks to
// confirm it.
func (s *Service) RemoveJobData(ctx context.Context, u *store.User, name string) (int, error) {
	if !u.Admin {
		return 0, ErrForbidden
	}
	if s.deps.Storage == nil {
		return 0, errNoStorage
	}
	n, err := s.deps.Storage.RemoveJob(ctx, u.Name, name)
	return n, storageErr(err)
}

// SelfCheckFailed is the last self-check when it failed, for the banner
// every admin sees; nil when it passed or never ran.
func (s *Service) SelfCheckFailed() *storage.Check {
	if s.deps.Storage == nil {
		return nil
	}
	if c := s.deps.Storage.LastCheck(); c != nil && !c.OK {
		return c
	}
	return nil
}

type storageJSON struct {
	At              stamp                `json:"at"`
	Disk            storageDiskJSON      `json:"disk"`
	Parts           []storagePartJSON    `json:"parts"`
	Database        storageDBJSON        `json:"database"`
	Jobs            []storageJobJSON     `json:"jobs"`
	Recommendations []recommendationJSON `json:"recommendations"`
	SelfCheck       *selfCheckJSON       `json:"self_check"`
	Heal            *healJSON            `json:"heal"`
	Compact         compactJSON          `json:"compact"`
}

type storageDiskJSON struct {
	Free  *int64 `json:"free_bytes"`
	Total *int64 `json:"total_bytes"`
	Floor *int64 `json:"floor_bytes"`
}

type storagePartJSON struct {
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
	Made  bool   `json:"made_by_kipp"`
}

type storageDBJSON struct {
	File         int64          `json:"file_bytes"`
	WAL          int64          `json:"wal_bytes"`
	PageSize     int64          `json:"page_size"`
	Pages        int64          `json:"pages"`
	Free         int64          `json:"free_bytes"`
	Used         int64          `json:"used_bytes"`
	AutoVacuum   string         `json:"auto_vacuum"`
	Exact        bool           `json:"exact"`
	MeasuredAt   *stamp         `json:"tables_measured_at"`
	Measuring    bool           `json:"measuring"`
	MeasureError string         `json:"measure_error"`
	Objects      []dbObjectJSON `json:"tables"`
}

type dbObjectJSON struct {
	Name  string `json:"name"`
	Table string `json:"table"`
	Index bool   `json:"index"`
	Bytes int64  `json:"bytes"`
}

type storageJobJSON struct {
	Name      string `json:"name"`
	Builds    int    `json:"builds"`
	Kept      int    `json:"kept"`
	KeepBuild int    `json:"keep_builds"`
	KeepDays  int    `json:"keep_days"`
	Tests     int64  `json:"test_rows"`
	Metrics   int64  `json:"metric_rows"`
	DB        int64  `json:"db_bytes"`
	Logs      int64  `json:"log_bytes"`
	Artifacts int64  `json:"artifact_bytes"`
	Gone      string `json:"gone"`
	Parent    string `json:"parent"`
	Until     *stamp `json:"until"`
}

type recommendationJSON struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
	Reason string `json:"reason"`
	Saving int64  `json:"saving_bytes"`
	Change string `json:"change"`
	Until  *stamp `json:"until"`
}

type selfCheckJSON struct {
	At       stamp    `json:"at"`
	OK       bool     `json:"ok"`
	Problems []string `json:"problems"`
	Error    string   `json:"error"`
}

type healJSON struct {
	At         stamp          `json:"at"`
	Floor      int64          `json:"floor_bytes"`
	FreeBefore int64          `json:"free_before_bytes"`
	FreeAfter  int64          `json:"free_after_bytes"`
	Freed      int64          `json:"freed_bytes"`
	FloorMet   bool           `json:"floor_met"`
	Steps      []healStepJSON `json:"steps"`
}

type healStepJSON struct {
	Kind  string `json:"kind"`
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
	Error string `json:"error"`
}

type compactJSON struct {
	State       string `json:"state"`
	By          string `json:"by"`
	StartedAt   *stamp `json:"started_at"`
	EndedAt     *stamp `json:"ended_at"`
	Running     int    `json:"running"`
	Queued      int    `json:"queued"`
	Before      int64  `json:"before_bytes"`
	After       int64  `json:"after_bytes"`
	Incremental bool   `json:"incremental"`
	Error       string `json:"error"`
	HoldsQuiet  bool   `json:"holds_quiet"`
}

func toSelfCheckJSON(c *storage.Check) *selfCheckJSON {
	if c == nil {
		return nil
	}
	return &selfCheckJSON{At: stampOf(c.At), OK: c.OK, Problems: append([]string{}, c.Problems...), Error: c.Error}
}

func toCompactJSON(c storage.CompactState) compactJSON {
	return compactJSON{State: c.State, By: c.By, StartedAt: stampSince(c.StartedAt), EndedAt: stampSince(c.EndedAt),
		Running: c.Running, Queued: c.Queued, Before: c.Before, After: c.After, Incremental: c.Incremental, Error: c.Error, HoldsQuiet: c.HoldsQuiet}
}

func toStorageJSON(r *storage.Report) storageJSON {
	out := storageJSON{At: stampOf(r.At), Parts: []storagePartJSON{}, Jobs: []storageJobJSON{}, Recommendations: []recommendationJSON{},
		SelfCheck: toSelfCheckJSON(r.Check), Compact: toCompactJSON(r.Compact)}
	if r.Disk.Known {
		out.Disk = storageDiskJSON{Free: new(r.Disk.Free), Total: new(r.Disk.Total), Floor: new(r.Disk.Floor)}
	}
	for _, p := range r.Parts {
		out.Parts = append(out.Parts, storagePartJSON(p))
	}
	d := r.Database
	out.Database = storageDBJSON{File: d.File, WAL: d.WAL, PageSize: d.PageSize, Pages: d.Pages, Free: d.FreeBytes, Used: d.UsedBytes,
		AutoVacuum: d.AutoVacuum, Exact: d.Exact, MeasuredAt: stampSince(d.MeasuredAt), Measuring: d.Measuring, MeasureError: d.MeasureError,
		Objects: []dbObjectJSON{}}
	for _, o := range d.Objects {
		out.Database.Objects = append(out.Database.Objects, dbObjectJSON(o))
	}
	for _, j := range r.Jobs {
		out.Jobs = append(out.Jobs, storageJobJSON{Name: j.Name, Builds: j.Builds, Kept: j.Kept, KeepBuild: j.Keep.Builds, KeepDays: j.Keep.Days,
			Tests: j.Tests, Metrics: j.Metrics, DB: j.DBBytes, Logs: j.Logs, Artifacts: j.Artifacts, Gone: j.Gone, Parent: j.Parent, Until: stampSince(j.Until)})
	}
	for _, rec := range r.Recommendations {
		out.Recommendations = append(out.Recommendations, recommendationJSON{Kind: rec.Kind, Target: rec.Target, Reason: rec.Reason,
			Saving: rec.Saving, Change: rec.Change, Until: stampSince(rec.Until)})
	}
	if h := r.Heal; h != nil {
		out.Heal = &healJSON{At: stampOf(h.At), Floor: h.Floor, FreeBefore: h.FreeBefore, FreeAfter: h.FreeAfter, Freed: h.Freed(), FloorMet: h.FloorMet, Steps: []healStepJSON{}}
		for _, s := range h.Steps {
			out.Heal.Steps = append(out.Heal.Steps, healStepJSON(s))
		}
	}
	return out
}

func (a *API) storageReport(w http.ResponseWriter, r *http.Request, u *store.User) {
	rep, err := a.svc.Storage(r.Context(), u)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toStorageJSON(rep))
}

func (a *API) compactState(w http.ResponseWriter, r *http.Request, u *store.User) {
	c, err := a.svc.CompactState(u)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toCompactJSON(c))
}

func (a *API) compactStart(w http.ResponseWriter, r *http.Request, u *store.User) {
	c, err := a.svc.StartCompact(r.Context(), u)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, toCompactJSON(c))
}

func (a *API) removeJobData(w http.ResponseWriter, r *http.Request, u *store.User) {
	name := r.PathValue("name")
	n, err := a.svc.RemoveJobData(r.Context(), u, name)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Job     string `json:"job"`
		Removed int    `json:"removed"`
	}{name, n})
}
