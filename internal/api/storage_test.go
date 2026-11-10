package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ndthuan/kipp/internal/storage"
	"github.com/ndthuan/kipp/internal/store"
)

// withStorage gives the test's server the storage of its data directory,
// with a disk of free bytes; what it would run apart runs in place.
func (e *env) withStorage(t *testing.T, free uint64) *storage.Service {
	t.Helper()
	d := e.api.deps.Dispatch
	counts := func(ctx context.Context, on *bool) (storage.Counts, error) {
		var err error
		if on != nil {
			_, err = d.SetQuiet(ctx, *on)
		}
		if err != nil {
			return storage.Counts{}, err
		}
		q, err := d.QuietState(ctx)
		return storage.Counts{Quiet: q.On, Running: q.Running, Queued: q.Queued}, err
	}
	svc := storage.New(storage.Options{Store: e.st, Data: e.dir, Snapshot: d.Snapshot,
		SetQuiet: func(ctx context.Context, on bool) (storage.Counts, error) { return counts(ctx, &on) },
		Counts:   func(ctx context.Context) (storage.Counts, error) { return counts(ctx, nil) },
		Space:    func(string) (uint64, uint64, bool) { return free, 1 << 40, true },
		SameDisk: func(_, _ string) bool { return true },
		TempDir:  e.dir,
		Go:       func(f func()) { f() },
	})
	e.api.deps.Storage = svc
	return svc
}

// goneJob gives the store a job no file names, with a build and its log.
func (e *env) goneJob(t *testing.T, name string) {
	t.Helper()
	b := &store.Build{Job: name, Status: store.Success, Trigger: "cli"}
	must(t, e.st.CreateBuild(context.Background(), b))
	dir := filepath.Join(e.dir, "logs", name, "1")
	must(t, os.MkdirAll(dir, 0o700))
	must(t, os.WriteFile(filepath.Join(dir, "s.log"), []byte("log"), 0o600))
}

func TestStorageIsAnAdminsAndAServerWithoutItSaysSo(t *testing.T) {
	e := newEnv(t)
	e.asProblem(t, 409, "conflict", e.admin, "GET", "/api/v1/admin/storage", "")
	e.withStorage(t, 100<<30)
	for _, c := range [][2]string{{"GET", "/api/v1/admin/storage"}, {"GET", "/api/v1/admin/storage/compact"}, {"POST", "/api/v1/admin/storage/compact"}, {"DELETE", "/api/v1/admin/storage/jobs/gone"}} {
		e.asProblem(t, 403, "forbidden", e.viewer, c[0], c[1], "")
	}
}

func TestTheStorageReportNamesTheDatabaseTheJobsAndAStrayFile(t *testing.T) {
	e := newEnv(t)
	e.withStorage(t, 100<<30)
	e.finished(t, e.trigger(t, "smoke", ""))
	e.goneJob(t, "gone")
	must(t, os.WriteFile(filepath.Join(e.dir, "db.sqlite.before-reset"), []byte("copy"), 0o600))
	got := e.object(t, 200, e.admin, "GET", "/api/v1/admin/storage", "")
	db := got["database"].(map[string]any)
	if db["exact"] != true || db["tables_measured_at"] == nil || len(db["tables"].([]any)) == 0 || db["auto_vacuum"] != "none" {
		t.Fatalf("database %v", db)
	}
	jobs := map[string]map[string]any{}
	for _, j := range got["jobs"].([]any) {
		jobs[j.(map[string]any)["name"].(string)] = j.(map[string]any)
	}
	eq(t, "smoke gone", jobs["smoke"]["gone"], "")
	eq(t, "gone gone", jobs["gone"]["gone"], "removed")
	if jobs["gone"]["until"] == nil {
		t.Fatal("no until for a removed job")
	}
	recs := map[string]map[string]any{}
	for _, r := range got["recommendations"].([]any) {
		m := r.(map[string]any)
		recs[m["kind"].(string)+" "+m["target"].(string)] = m
	}
	if recs["stray db.sqlite.before-reset"] == nil || recs["remove gone"] == nil {
		t.Fatalf("recommendations %v", got["recommendations"])
	}
	eq(t, "compact", got["compact"].(map[string]any)["state"], "idle")
}

func TestAnAdminCompactsTheDatabaseAndTheServerIsNotLeftQuiet(t *testing.T) {
	e := newEnv(t)
	e.withStorage(t, 100<<30)
	idle := e.object(t, 200, e.admin, "GET", "/api/v1/admin/storage/compact", "")
	eq(t, "state", idle["state"], "idle")
	done := e.object(t, 202, e.admin, "POST", "/api/v1/admin/storage/compact", "")
	eq(t, "state", done["state"], "done")
	eq(t, "by", done["by"], "admin")
	eq(t, "incremental", done["incremental"], true)
	if done["ended_at"] == nil {
		t.Fatalf("%v", done)
	}
	if !strings.HasPrefix(e.lastAudit(t), "admin db.compact db.sqlite: ") {
		t.Fatal(e.lastAudit(t))
	}
	eq(t, "quiet", e.object(t, 200, e.admin, "GET", "/api/v1/admin/quiet", "")["quiet"], false)
	e.finished(t, e.trigger(t, "smoke", ""))
}

func TestACompactTheDiskHasNoRoomForIsAConflictWithTheNumbers(t *testing.T) {
	e := newEnv(t)
	e.withStorage(t, 1)
	d, _ := e.asProblem(t, 409, "conflict", e.admin, "POST", "/api/v1/admin/storage/compact", "")
	contains(t, d.Detail, "free and a compact needs")
	eq(t, "quiet", e.object(t, 200, e.admin, "GET", "/api/v1/admin/quiet", "")["quiet"], false)
}

func TestRemoveNowTakesAGoneJobsDataAndRefusesAJobWithAFile(t *testing.T) {
	e := newEnv(t)
	e.withStorage(t, 100<<30)
	e.goneJob(t, "gone")
	e.asProblem(t, 409, "conflict", e.admin, "DELETE", "/api/v1/admin/storage/jobs/smoke", "")
	got := e.object(t, 200, e.admin, "DELETE", "/api/v1/admin/storage/jobs/gone", "")
	eq[any](t, "removed", got["removed"], float64(1))
	eq(t, "audit", e.lastAudit(t), "admin storage.remove-job gone: 1 builds removed by hand, its job file gone")
	e.asProblem(t, 404, "not_found", e.admin, "DELETE", "/api/v1/admin/storage/jobs/gone", "")
}

func TestTheServerSaysTheLastSelfCheck(t *testing.T) {
	e := newEnv(t)
	eq(t, "before any", e.object(t, 200, e.admin, "GET", "/api/v1/admin/server", "")["self_check"], nil)
	svc := e.withStorage(t, 100<<30)
	svc.Check(context.Background())
	c := e.object(t, 200, e.admin, "GET", "/api/v1/admin/server", "")["self_check"].(map[string]any)
	eq(t, "ok", c["ok"], true)
	if e.api.svc.SelfCheckFailed() != nil {
		t.Fatal("a passed check is said to have failed")
	}
}
