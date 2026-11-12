package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

var ctx = context.Background()

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// fakeQuiet is the dispatcher's quiet mode as a compact sees it: busy
// counts down the polls before nothing runs.
type fakeQuiet struct {
	mu    sync.Mutex
	quiet bool
	busy  int
	log   []string
	fail  error
}

func (q *fakeQuiet) set(_ context.Context, on bool) (Counts, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.quiet = on
	q.log = append(q.log, fmt.Sprintf("quiet %v", on))
	return Counts{Quiet: q.quiet, Running: q.busy}, nil
}

func (q *fakeQuiet) counts(context.Context) (Counts, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.fail != nil {
		return Counts{}, q.fail
	}
	c := Counts{Quiet: q.quiet, Running: q.busy}
	if q.busy > 0 {
		q.busy--
	}
	return c, nil
}

type harness struct {
	svc   *Service
	st    *store.Store
	data  string
	quiet *fakeQuiet
	// free and total are what Space answers for every directory.
	mu          sync.Mutex
	free, total uint64
	jobs        map[string]*config.Job
}

func (h *harness) space(string) (uint64, uint64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.free, h.total, true
}

func (h *harness) setFree(n int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if n < 0 {
		n = 0
	}
	h.free = uint64(n)
}

func newHarness(t *testing.T, jobs ...string) *harness {
	t.Helper()
	data := t.TempDir()
	st, err := store.Open(filepath.Join(data, "db.sqlite"))
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	h := &harness{st: st, data: data, quiet: &fakeQuiet{}, free: 100 << 30, total: 1 << 40, jobs: map[string]*config.Job{}}
	for _, name := range jobs {
		h.jobs[name] = &config.Job{Name: name, Keep: config.Keep{Builds: config.DefaultKeepBuilds}}
	}
	h.svc = New(Options{Store: st, Data: data,
		Snapshot: func() *config.Snapshot {
			return &config.Snapshot{Jobs: h.jobs, Names: slices.Sorted(func(yield func(string) bool) {
				for n := range h.jobs {
					if !yield(n) {
						return
					}
				}
			})}
		},
		SetQuiet: h.quiet.set, Counts: h.quiet.counts, Space: h.space,
		SameDisk: func(_, _ string) bool { return true }, TempDir: data,
		Poll: time.Millisecond, Go: func(f func()) { f() },
	})
	return h
}

// seed gives the job n ended builds, each with tests test rows and a log.
func (h *harness) seed(t *testing.T, job string, n, tests int) []int64 {
	t.Helper()
	var ids []int64
	for range n {
		b := &store.Build{Job: job, Status: store.Success, Trigger: "cli"}
		must(t, h.st.CreateBuild(ctx, b))
		ts := make([]store.Test, tests)
		for i := range ts {
			ts[i] = store.Test{Stage: "s", Suite: "s", Name: fmt.Sprint(i), Status: "passed", Message: strings.Repeat("x", 300)}
		}
		must(t, h.st.PutTests(ctx, b.ID, ts))
		must(t, h.st.PutMetricHistory(ctx, b.ID, "site", "s", []store.Metric{{Name: config.CaseSeconds, Values: map[string]float64{"s / 0": 1}}}))
		dir := filepath.Join(h.data, "logs", job, fmt.Sprint(b.Number))
		must(t, os.MkdirAll(dir, 0o700))
		must(t, os.WriteFile(filepath.Join(dir, "s.log"), []byte(strings.Repeat("l", 1000)), 0o600))
		ids = append(ids, b.ID)
	}
	return ids
}

func (h *harness) lastAudit(t *testing.T) string {
	t.Helper()
	rows, err := h.st.AuditBefore(ctx, 0, 1)
	must(t, err)
	if len(rows) == 0 {
		return ""
	}
	return rows[0].Actor + " " + rows[0].Action + " " + rows[0].Target + ": " + rows[0].Detail
}

func TestTheReportOfASeededDatabaseNamesTablesJobsGoneJobsFreePagesAndStrayFiles(t *testing.T) {
	h := newHarness(t, "big", "small")
	h.seed(t, "big", 30, 40)
	h.seed(t, "small", 2, 1)
	h.seed(t, "removed", 3, 1)
	h.seed(t, "multi.pr-43", 2, 1)
	must(t, h.st.PutChild(ctx, store.Child{Parent: "multi", Key: "pr-43", Kind: "pr", PR: 43, State: "gone", LastSeenAt: time.Now()}))
	junk := h.seed(t, "junk", 20, 40)
	for _, id := range junk {
		must(t, h.st.DeleteBuild(ctx, id))
	}
	must(t, os.WriteFile(filepath.Join(h.data, "db.sqlite.before-reset"), []byte("copy"), 0o600))
	must(t, os.MkdirAll(filepath.Join(h.data, "cache"), 0o700))
	must(t, os.WriteFile(filepath.Join(h.data, "cache", "x.tar.zst"), make([]byte, 500), 0o600))

	r, err := h.svc.Report(ctx)
	must(t, err)
	if r.Database.MeasuredAt.IsZero() || !r.Database.Exact || r.Database.Objects[0].Name != "tests" {
		t.Fatalf("tables %+v", r.Database)
	}
	if r.Database.FreeBytes <= 0 || r.Database.File <= 0 || r.Database.AutoVacuum != "none" {
		t.Fatalf("database %+v", r.Database)
	}
	if !r.Disk.Known || r.Disk.Floor != FloorBytes {
		t.Fatalf("disk %+v", r.Disk)
	}
	checkJobs(t, r.Jobs)
	checkParts(t, r.Parts)
	checkRecommendations(t, r.Recommendations)
}

func checkJobs(t *testing.T, jobs []Job) {
	t.Helper()
	byName := map[string]Job{}
	for _, j := range jobs {
		byName[j.Name] = j
	}
	if jobs[0].Name != "big" || byName["big"].Tests != 1200 || byName["big"].DBBytes <= byName["small"].DBBytes || byName["big"].Logs != 30000 {
		t.Fatalf("jobs %+v", jobs)
	}
	if g := byName["removed"]; g.Gone != GoneRemoved || g.Until.Sub(g.Newest) != 90*24*time.Hour || g.Keep.Days != config.RemovedKeepDays {
		t.Fatalf("removed %+v", g)
	}
	if g := byName["multi.pr-43"]; g.Gone != GoneClosed || g.Parent != "multi" {
		t.Fatalf("closed %+v", g)
	}
	if byName["small"].Gone != "" {
		t.Fatal("a job with a file is said to be gone")
	}
}

func checkParts(t *testing.T, parts []Part) {
	t.Helper()
	kinds := map[string]Part{}
	for _, p := range parts {
		kinds[p.Kind+" "+p.Name] = p
	}
	if p := kinds["other db.sqlite.before-reset"]; p.Made || p.Bytes != 4 {
		t.Fatalf("parts %+v", parts)
	}
	if kinds[PartCache+" cache/"].Bytes != 500 || kinds[PartDatabase+" db.sqlite, -wal, -shm"].Bytes == 0 || !kinds[PartLogs+" logs/"].Made {
		t.Fatalf("parts %+v", parts)
	}
}

func checkRecommendations(t *testing.T, got []Recommendation) {
	t.Helper()
	recs := map[string]Recommendation{}
	for _, rec := range got {
		recs[rec.Kind+" "+rec.Target] = rec
	}
	for _, want := range []string{"keep big", "remove removed", "remove multi.pr-43", "compact ", "stray db.sqlite.before-reset"} {
		if _, ok := recs[want]; !ok {
			t.Errorf("no recommendation %q in %+v", want, got)
		}
	}
	if _, ok := recs["keep small"]; ok {
		t.Error("a small job was told to keep fewer builds")
	}
	if !strings.Contains(recs["stray db.sqlite.before-reset"].Reason, "kipp did not make this file") {
		t.Errorf("%+v", recs["stray db.sqlite.before-reset"])
	}
	if k := recs["keep big"]; !strings.HasPrefix(k.Change, "keep: {builds: ") || k.Saving <= 0 {
		t.Errorf("%+v", k)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Saving > got[i-1].Saving {
			t.Fatalf("not the largest saving first: %+v", got)
		}
	}
}

// report is a database of 1000 bytes in use and the jobs.
func report(jobs ...Job) *Report {
	return &Report{Database: Database{PageSize: 1, Pages: 1000, UsedBytes: 1000}, Jobs: jobs}
}

func kinds(recs []Recommendation) []string {
	var out []string
	for _, r := range recs {
		out = append(out, r.Kind+" "+r.Target)
	}
	return out
}

func TestAKeepIsRecommendedOverTheJobShareAndNotAtIt(t *testing.T) {
	at := Job{Name: "a", Builds: 100, DBBytes: 100}
	if got := Recommend(report(at)); len(got) != 0 {
		t.Fatalf("at exactly %v: %v", JobShare, kinds(got))
	}
	over := Job{Name: "a", Builds: 100, DBBytes: 101, Keep: config.Keep{Builds: 100, Days: 7}}
	got := Recommend(report(over))
	if len(got) != 1 || got[0].Kind != RecommendKeep {
		t.Fatalf("%v", kinds(got))
	}
	n := FewerBuilds(101, 100, 1000)
	if got[0].Change != fmt.Sprintf("keep: {builds: %d, days: 7}", n) {
		t.Fatalf("%q", got[0].Change)
	}
	// With n builds the job is under the share of the smaller database.
	rows := float64(101) / 100 * float64(n)
	if share := rows / (1000 - 101 + rows); share >= JobShare {
		t.Fatalf("%d builds leave a share of %v", n, share)
	}
	if more := float64(101) / 100 * float64(n+1); more/(1000-101+more) < JobShare {
		t.Fatalf("%d is not the most builds under the share", n)
	}
}

func TestAChildIsToldWhichJobFileHoldsItsKeep(t *testing.T) {
	got := Recommend(report(Job{Name: "m.main", Parent: "m", Builds: 10, DBBytes: 900}))
	if len(got) != 1 || !strings.Contains(got[0].Reason, "the job file of m") || got[0].Change != "keep: {builds: 1}" {
		t.Fatalf("%+v", got)
	}
}

func TestACompactIsRecommendedOverTheFreeShareAndNotAtIt(t *testing.T) {
	r := report()
	r.Database.FreeBytes = 100
	if got := Recommend(r); len(got) != 0 {
		t.Fatalf("%v", kinds(got))
	}
	r.Database.FreeBytes = 101
	if got := Recommend(r); len(got) != 1 || got[0].Kind != RecommendCompact || got[0].Saving != 101 {
		t.Fatalf("%v", got)
	}
}

func TestAGoneJobsDataIsOfferedForRemovalUnlessOnlyKeptBuildsAreLeft(t *testing.T) {
	until := time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC)
	got := Recommend(report(
		Job{Name: "gone", Gone: GoneRemoved, Builds: 3, Logs: 10, Until: until},
		Job{Name: "kept", Gone: GoneRemoved, Builds: 1, Kept: 1},
		Job{Name: "live", Builds: 3, Logs: 10},
	))
	if len(got) != 1 || got[0].Target != "gone" || got[0].Change != "remove now" || !got[0].Until.Equal(until) || !strings.Contains(got[0].Reason, "2027-01-02") {
		t.Fatalf("%+v", got)
	}
}

func TestAFileKippDidNotMakeIsNamedAndOneItMadeIsNot(t *testing.T) {
	r := report()
	r.Parts = []Part{{Kind: PartLogs, Name: "logs/", Bytes: 9999, Made: true}, {Kind: PartOther, Name: "copy.sqlite", Bytes: 5}}
	if got := Recommend(r); len(got) != 1 || got[0].Kind != RecommendStray || got[0].Target != "copy.sqlite" {
		t.Fatalf("%+v", got)
	}
}

func TestACompactQuietsWaitsForTheBuildsShrinksTheFileAndEndsQuiet(t *testing.T) {
	h := newHarness(t, "a")
	ids := h.seed(t, "a", 30, 60)
	for _, id := range ids[:25] {
		must(t, h.st.DeleteBuild(ctx, id))
	}
	h.quiet.busy = 3
	c, err := h.svc.StartCompact(ctx, "admin")
	must(t, err)
	if c.State != CompactDone || c.After >= c.Before || !c.Incremental || c.HoldsQuiet || c.Error != "" {
		t.Fatalf("%+v", c)
	}
	if strings.Join(h.quiet.log, ", ") != "quiet true, quiet false" || h.quiet.busy != 0 {
		t.Fatalf("quiet %v, busy %d", h.quiet.log, h.quiet.busy)
	}
	if a := h.lastAudit(t); !strings.HasPrefix(a, "admin db.compact db.sqlite: ") || !strings.Contains(a, "incremental auto_vacuum on") {
		t.Fatalf("audit %q", a)
	}
	f, _ := h.st.DBFile(ctx)
	if f.AutoVacuum != store.AutoVacuumIncremental || f.FreePages != 0 {
		t.Fatalf("%+v", f)
	}
}

func TestACompactLeavesQuietOnWhenItWasOnBefore(t *testing.T) {
	h := newHarness(t)
	h.quiet.quiet = true
	_, err := h.svc.StartCompact(ctx, "admin")
	must(t, err)
	if !h.quiet.quiet {
		t.Fatal("a quiet an admin had turned on was turned off")
	}
}

func TestACompactIsRefusedWithTheNumbersWhenTheDiskHasNoRoomForTwoCopies(t *testing.T) {
	h := newHarness(t, "a")
	h.seed(t, "a", 5, 20)
	f, _ := h.st.DBFile(ctx)
	h.setFree(2*f.UsedBytes() - 1)
	_, err := h.svc.StartCompact(ctx, "admin")
	var room *RoomError
	if !errors.As(err, &room) || room.Need != 2*f.UsedBytes() || room.Copies != 2 {
		t.Fatalf("%v", err)
	}
	if !strings.Contains(err.Error(), "free and a compact needs") {
		t.Fatalf("%v", err)
	}
	if len(h.quiet.log) != 0 || h.svc.CompactState().State != CompactIdle {
		t.Fatalf("a refused compact changed something: %v %+v", h.quiet.log, h.svc.CompactState())
	}
	if !strings.Contains(h.lastAudit(t), "refused: ") {
		t.Fatal(h.lastAudit(t))
	}
	// On two disks, each needs one copy.
	h.svc.o.SameDisk = func(_, _ string) bool { return false }
	h.setFree(f.UsedBytes())
	if _, err := h.svc.StartCompact(ctx, "admin"); err != nil {
		t.Fatal(err)
	}
}

func TestACompactThatFailsLeavesQuietOffAndSaysWhy(t *testing.T) {
	h := newHarness(t)
	h.quiet.busy = 2
	h.quiet.fail = nil
	// The room is there when it starts and gone once the builds have ended.
	calls := 0
	h.svc.o.Space = func(string) (uint64, uint64, bool) {
		calls++
		if calls > 1 {
			return 0, 1 << 40, true
		}
		return 1 << 40, 1 << 40, true
	}
	c, err := h.svc.StartCompact(ctx, "admin")
	must(t, err)
	if c.State != CompactFailed || !strings.Contains(c.Error, "free and a compact needs") || c.HoldsQuiet {
		t.Fatalf("%+v", c)
	}
	if h.quiet.quiet {
		t.Fatal("a failed compact left the server quiet")
	}
	if !strings.Contains(h.lastAudit(t), "db.compact db.sqlite: failed: ") {
		t.Fatal(h.lastAudit(t))
	}
}

func TestASecondCompactWhileOneRunsIsRefused(t *testing.T) {
	h := newHarness(t)
	h.svc.compact.State = CompactWaiting
	if _, err := h.svc.StartCompact(ctx, "admin"); !errors.Is(err, ErrCompactBusy) {
		t.Fatal(err)
	}
}

func TestTheSelfCheckIsRecordedAndReadBackAfterARestart(t *testing.T) {
	h := newHarness(t)
	c := h.svc.Check(ctx)
	if !c.OK || c.At.IsZero() || c.Problems != nil {
		t.Fatalf("%+v", c)
	}
	again := New(Options{Store: h.st, Data: h.data})
	if got := again.LastCheck(); got == nil || !got.OK || !got.At.Equal(c.At) {
		t.Fatalf("%+v", got)
	}
}

func TestASelfCheckOfADamagedFileFails(t *testing.T) {
	h := newHarness(t, "a")
	h.seed(t, "a", 20, 50)
	path := h.st.Path()
	must(t, h.st.Close())
	root, err := os.OpenRoot(h.data)
	must(t, err)
	defer func() { _ = root.Close() }()
	f, err := root.OpenFile(filepath.Base(path), os.O_RDWR, 0)
	must(t, err)
	fi, _ := f.Stat()
	// Damage the pages in the middle of the file, not its header.
	_, err = f.WriteAt([]byte(strings.Repeat("\xff", 8192)), fi.Size()/2/4096*4096)
	must(t, errors.Join(err, f.Close()))
	st, err := store.OpenCurrent(path)
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	svc := New(Options{Store: st, Data: h.data})
	c := svc.Check(ctx)
	if c.OK || (len(c.Problems) == 0 && c.Error == "") {
		t.Fatalf("%+v", c)
	}
	if got := readCheck(h.data); got == nil || got.OK {
		t.Fatalf("the failure was not kept: %+v", got)
	}
}

func TestUnderTheFloorTheCleanRemovesOnlyTheThreeKindsInOrder(t *testing.T) {
	h := newHarness(t, "known")
	h.setFree(1 << 20)
	started := time.Now().Add(-time.Hour)
	running := &store.Build{Job: "busy", Status: store.Running, Trigger: "cli", QueuedAt: started}
	must(t, h.st.CreateBuild(ctx, running))
	must(t, h.st.SetBuildStatus(ctx, running.ID, store.Running, started))
	mk := func(rel string, mod time.Time) {
		p := filepath.Join(h.data, rel)
		must(t, os.MkdirAll(filepath.Dir(p), 0o700))
		must(t, os.WriteFile(p, make([]byte, 100), 0o600))
		must(t, os.Chtimes(filepath.Dir(p), mod, mod))
	}
	mk("stage-tmp/t-old/f", started.Add(-time.Hour))
	mk("stage-tmp/t-new/f", time.Now())
	mk("workspace/gone/1/f", time.Now())
	mk("workspace/known/1/f", time.Now())
	mk("workspace/busy/1/f", time.Now())
	mk("logs/gone/1/s.log", time.Now())
	mk("artifacts/gone/1/s/a", time.Now())
	var order []string
	h.svc.o.TrimCache = func(context.Context) (int, error) {
		order = append(order, "cache")
		return 1, nil
	}
	heal, err := h.svc.Heal(ctx)
	must(t, err)
	var steps []string
	for _, s := range heal.Steps {
		steps = append(steps, s.Kind+" "+s.Path)
	}
	if strings.Join(steps, ", ") != "stage_temp stage-tmp/t-old, workspace workspace/gone, cache cache" || heal.FloorMet || len(order) != 1 {
		t.Fatalf("%v", steps)
	}
	for _, kept := range []string{"stage-tmp/t-new", "workspace/known/1/f", "workspace/busy/1/f", "logs/gone/1/s.log", "artifacts/gone/1/s/a", "db.sqlite"} {
		if _, err := os.Stat(filepath.Join(h.data, kept)); err != nil {
			t.Errorf("%s went: %v", kept, err)
		}
	}
	if heal.Freed() != 200 || h.svc.LastHeal() != heal {
		t.Fatalf("freed %d", heal.Freed())
	}
}

func TestTheCleanStopsOnceTheFloorIsMetAndDoesNothingAboveIt(t *testing.T) {
	h := newHarness(t)
	if heal, err := h.svc.Heal(ctx); heal != nil || err != nil {
		t.Fatalf("above the floor: %+v %v", heal, err)
	}
	h.setFree(1 << 20)
	must(t, os.MkdirAll(filepath.Join(h.data, "stage-tmp", "t-old"), 0o700))
	must(t, os.MkdirAll(filepath.Join(h.data, "workspace", "gone"), 0o700))
	h.svc.o.Space = func(string) (uint64, uint64, bool) {
		if _, err := os.Stat(filepath.Join(h.data, "stage-tmp", "t-old")); err != nil {
			return 100 << 30, 1 << 40, true
		}
		return 1 << 20, 1 << 40, true
	}
	heal, err := h.svc.Heal(ctx)
	must(t, err)
	if len(heal.Steps) != 1 || !heal.FloorMet {
		t.Fatalf("%+v", heal)
	}
	if _, err := os.Stat(filepath.Join(h.data, "workspace", "gone")); err != nil {
		t.Fatal("a workspace went after the floor was met")
	}
}

func TestTheFloorIsTheSmallerOfFiveGiBAndFivePercent(t *testing.T) {
	if floor(1<<40) != FloorBytes || floor(20<<30) != 1<<30 {
		t.Fatal(floor(1<<40), floor(20<<30))
	}
}

func TestRemoveNowTakesAGoneJobsDataAndRefusesAJobWithAFile(t *testing.T) {
	h := newHarness(t, "live")
	h.seed(t, "live", 2, 1)
	h.seed(t, "gone", 3, 1)
	if _, err := h.svc.RemoveJob(ctx, "admin", "live"); !errors.Is(err, ErrHasFile) {
		t.Fatal(err)
	}
	n, err := h.svc.RemoveJob(ctx, "admin", "gone")
	if err != nil || n != 3 {
		t.Fatal(n, err)
	}
	if a := h.lastAudit(t); a != "admin storage.remove-job gone: 3 builds removed by hand, its job file gone" {
		t.Fatal(a)
	}
	if _, err := h.svc.RemoveJob(ctx, "admin", "gone"); !errors.Is(err, ErrNothingLeft) {
		t.Fatal(err)
	}
}

func TestStopCancelsACompactStillWaitingForBuildsAndReturnsOnceItHasEnded(t *testing.T) {
	h := newHarness(t)
	h.svc.o.Go = func(f func()) { go f() }
	h.svc.o.Poll = time.Hour // only the stop can end the wait
	h.quiet.busy = 1 << 30
	changes := h.svc.Changes()
	if _, err := h.svc.StartCompact(ctx, "admin"); err != nil {
		t.Fatal(err)
	}
	for !h.svc.CompactState().HoldsQuiet {
		<-changes
		changes = h.svc.Changes()
	}
	h.svc.Stop()
	c := h.svc.CompactState()
	if c.State != CompactFailed || !strings.Contains(c.Error, "the server stopped") || c.HoldsQuiet {
		t.Fatalf("%+v", c)
	}
	if h.quiet.quiet {
		t.Fatal("a cancelled compact left the server quiet")
	}
	if a := h.lastAudit(t); !strings.Contains(a, "db.compact db.sqlite: failed: ") || !strings.Contains(a, "the server stopped") {
		t.Fatalf("audit %q", a)
	}
}

func TestAStoppedServiceStartsNoMoreWork(t *testing.T) {
	h := newHarness(t)
	ran := 0
	h.svc.o.Go = func(f func()) { ran++; f() }
	h.svc.Stop()
	h.svc.MeasureSoon()
	if _, err := h.svc.StartCompact(ctx, "admin"); !errors.Is(err, ErrStopped) {
		t.Fatal(err)
	}
	if ran != 0 || h.svc.CompactState().State != CompactIdle || len(h.quiet.log) != 0 {
		t.Fatalf("ran %d, %+v, quiet %v", ran, h.svc.CompactState(), h.quiet.log)
	}
}
