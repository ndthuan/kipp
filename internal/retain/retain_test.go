package retain

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

func openStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, filepath.Join(dir, "logs")
}

// seed gives the job n ended builds with a log each, queued a day apart,
// the newest a day ago.
func seed(t *testing.T, st *store.Store, logs, job string, n int, now time.Time) {
	t.Helper()
	seedEvery(t, st, logs, job, n, now, 24*time.Hour)
}

// seedEvery is seed with the builds queued step apart, the newest one step ago.
func seedEvery(t *testing.T, st *store.Store, logs, job string, n int, now time.Time, step time.Duration) {
	t.Helper()
	for i := range n {
		b := &store.Build{Job: job, Status: store.Success, Trigger: "cli", QueuedAt: now.Add(time.Duration(i-n) * step)}
		if err := st.CreateBuild(context.Background(), b); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(logs, job, strconv.Itoa(b.Number))
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "s.log"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func parse(t *testing.T, yml string) *config.Job {
	t.Helper()
	job, err := config.ParseJob("a.yml", []byte(yml))
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestJobKeepsTheNewestBuildsAndRemovesLogs(t *testing.T) {
	st, logs := openStore(t)
	now := time.Now()
	seed(t, st, logs, "a", 5, now)
	job := parse(t, "name: a\nkeep: {builds: 2}\nstages:\n  s: {run: true}\n")
	if err := Job(context.Background(), st, job, logs, now); err != nil {
		t.Fatal(err)
	}
	left, _ := st.Builds(context.Background(), "a", 10)
	if len(left) != 2 || left[0].Number != 5 {
		t.Fatalf("%+v", left)
	}
	if exists(filepath.Join(logs, "a", "1")) {
		t.Fatal("log folder of build 1 still there")
	}
	if !exists(filepath.Join(logs, "a", "5", "s.log")) {
		t.Fatal("log of build 5 gone")
	}
}

func TestJobLeavesOtherJobsAndRunningBuilds(t *testing.T) {
	st, logs := openStore(t)
	ctx := context.Background()
	now := time.Now()
	seed(t, st, logs, "a", 3, now)
	seed(t, st, logs, "b", 3, now)
	run := &store.Build{Job: "a", Status: store.Running, Trigger: "cli", QueuedAt: now.AddDate(-1, 0, 0)}
	if err := st.CreateBuild(ctx, run); err != nil {
		t.Fatal(err)
	}
	job := parse(t, "name: a\nkeep: {builds: 1}\nstages:\n  s: {run: true}\n")
	if err := Job(ctx, st, job, logs, now); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Builds(ctx, "b", 10); len(got) != 3 {
		t.Fatalf("job b lost builds: %d", len(got))
	}
	if _, err := st.Build(ctx, run.ID); err != nil {
		t.Fatalf("running build deleted: %v", err)
	}
}

func TestKeepDaysRemovesBuildsQueuedBeforeTheCutoff(t *testing.T) {
	st, logs := openStore(t)
	now := time.Now()
	seed(t, st, logs, "a", 5, now) // queued 5,4,3,2,1 days ago; the 3-day-old one sits on the cutoff and stays
	job := parse(t, "name: a\nkeep: {days: 3}\nstages:\n  s: {run: true}\n")
	if err := Job(context.Background(), st, job, logs, now); err != nil {
		t.Fatal(err)
	}
	left, _ := st.Builds(context.Background(), "a", 10)
	if len(left) != 3 {
		t.Fatalf("%d left", len(left))
	}
}

func TestJobWithNoLogsDirStillDeletesRows(t *testing.T) {
	st, logs := openStore(t)
	now := time.Now()
	seed(t, st, logs, "a", 3, now)
	if err := os.RemoveAll(logs); err != nil {
		t.Fatal(err)
	}
	job := parse(t, "name: a\nkeep: {builds: 1}\nstages:\n  s: {run: true}\n")
	if err := Job(context.Background(), st, job, logs, now); err != nil {
		t.Fatal(err)
	}
	if left, _ := st.Builds(context.Background(), "a", 10); len(left) != 1 {
		t.Fatalf("%d left", len(left))
	}
}

func TestLogRemovalNeverLeavesLogsDir(t *testing.T) {
	st, logs := openStore(t)
	ctx := context.Background()
	now := time.Now()
	outside := filepath.Join(filepath.Dir(logs), "victim")
	if err := os.MkdirAll(filepath.Join(outside, "1"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(logs, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../victim", "..", ".", "", "x/y"} {
		b := &store.Build{Job: name, Status: store.Success, Trigger: "cli", QueuedAt: now}
		if err := st.CreateBuild(ctx, b); err != nil {
			t.Fatal(err)
		}
		if _, err := apply(ctx, st, name, config.Keep{Builds: 0, Days: 1}, logs, now.AddDate(0, 0, 5)); err == nil {
			t.Fatalf("job name %q accepted", name)
		}
		if _, err := st.Build(ctx, b.ID); err != nil {
			t.Fatalf("build of %q deleted", name)
		}
	}
	if !exists(filepath.Join(outside, "1")) || !exists(logs) {
		t.Fatal("something outside a job folder was removed")
	}
}

func TestALinkedJobFolderIsNotFollowedAndItsBuildsAreKept(t *testing.T) {
	st, logs := openStore(t)
	now := time.Now()
	seed(t, st, logs, "a", 3, now)
	outside := filepath.Join(filepath.Dir(logs), "victim")
	if err := os.MkdirAll(filepath.Join(outside, "1"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(logs, "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(logs, "a")); err != nil {
		t.Skip(err)
	}
	job := parse(t, "name: a\nkeep: {builds: 1}\nstages:\n  s: {run: true}\n")
	err := Job(context.Background(), st, job, logs, now)
	if !exists(filepath.Join(outside, "1")) {
		t.Fatal("removal followed a symlink out of logsDir")
	}
	// A build whose logs could not be removed keeps its row, so that the
	// next pass tries again.
	if err == nil {
		t.Fatal("no error for logs that could not be removed")
	}
	if left, _ := st.Builds(context.Background(), "a", 10); len(left) != 3 {
		t.Fatalf("%d builds left, want all 3", len(left))
	}
}

func TestSweepCoversJobsWithoutAFileAtTheDefault(t *testing.T) {
	st, logs := openStore(t)
	ctx := context.Background()
	now := time.Now()
	seed(t, st, logs, "gone", 3, now) // no job file; 3 is under the default 100
	u, err := st.CreateUser(ctx, "u", "h", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, &store.Session{ID: "old", UserID: u.ID, CSRF: "c", ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	snap := &config.Snapshot{Jobs: map[string]*config.Job{}}
	// The session has an hour left; the sweep is told it is a day later.
	n, err := Sweep(ctx, st, snap, logs, now.Add(24*time.Hour))
	if err != nil || n != 0 {
		t.Fatal(err, n)
	}
	if left, _ := st.Builds(ctx, "gone", 10); len(left) != 3 {
		t.Fatalf("%d", len(left))
	}
	if _, err := st.Session(ctx, "old"); err == nil {
		t.Fatal("expired session survived the sweep")
	}
}

func TestSweepAppliesRulesAndCounts(t *testing.T) {
	st, logs := openStore(t)
	ctx := context.Background()
	now := time.Now()
	seed(t, st, logs, "a", 4, now)
	// All within the days a removed job's builds are kept: the count alone decides.
	seedEvery(t, st, logs, "orphan", 105, now, time.Minute)
	job := parse(t, "name: a\nkeep: {builds: 1}\nstages:\n  s: {run: true}\n")
	snap := &config.Snapshot{Jobs: map[string]*config.Job{"a": job}, Names: []string{"a"}}
	n, err := Sweep(ctx, st, snap, logs, now)
	if err != nil || n != 3+5 {
		t.Fatal(err, n)
	}
	if left, _ := st.Builds(ctx, "orphan", 200); len(left) != 100 {
		t.Fatalf("%d", len(left))
	}
}

func TestSweepWithoutALogsDirectoryIsNotAnError(t *testing.T) {
	st, logs := openStore(t)
	snap := &config.Snapshot{Jobs: map[string]*config.Job{}}
	if _, err := Sweep(context.Background(), st, snap, logs, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestSweepGoesOnPastABuildItCannotRemove(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root removes what this test locks")
	}
	st, logs := openStore(t)
	ctx := context.Background()
	now := time.Now()
	seed(t, st, logs, "a", 3, now)
	seed(t, st, logs, "b", 3, now)
	// All within the days a removed job's builds are kept: the count alone decides.
	seedEvery(t, st, logs, "orphan", 101, now, time.Minute)
	stuck := filepath.Join(logs, "a", "1")
	if err := os.Chmod(stuck, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stuck, 0o700) })
	u, err := st.CreateUser(ctx, "u", "h", false)
	if err != nil {
		t.Fatal(err)
	}
	// Not expired yet, so that the store still shows it until the sweep,
	// which is told it is a day later, has removed it.
	if err := st.CreateSession(ctx, &store.Session{ID: "old", UserID: u.ID, CSRF: "c", ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	keepOne := func(name string) *config.Job {
		job, err := config.ParseJob(name+".yml", []byte("name: "+name+"\nkeep: {builds: 1}\nstages:\n  s: {run: true}\n"))
		if err != nil {
			t.Fatal(err)
		}
		return job
	}
	snap := &config.Snapshot{Jobs: map[string]*config.Job{"a": keepOne("a"), "b": keepOne("b")}, Names: []string{"a", "b"}}
	n, err := Sweep(ctx, st, snap, logs, now.Add(24*time.Hour))
	if err == nil || !strings.Contains(err.Error(), filepath.Join("a", "1")) {
		t.Fatalf("the build that would not go is not in the error: %v", err)
	}
	if n != 1+2+1 {
		t.Fatalf("removed %d builds, want a's second, two of b and one orphan", n)
	}
	left := func(job string) []int {
		var numbers []int
		builds, _ := st.Builds(ctx, job, 200)
		for _, b := range builds {
			numbers = append(numbers, b.Number)
		}
		return numbers
	}
	if got := left("a"); !slices.Equal(got, []int{3, 1}) {
		t.Fatalf("a keeps %v, want its newest and the one that would not go", got)
	}
	if got := left("b"); !slices.Equal(got, []int{3}) {
		t.Fatalf("b keeps %v", got)
	}
	if got := left("orphan"); len(got) != 100 {
		t.Fatalf("the orphan keeps %d", len(got))
	}
	if _, err := st.Session(ctx, "old"); err == nil {
		t.Fatal("the expired session survived")
	}
}

// An unstable build has ended: it counts among the builds kept and goes,
// with its logs, when it is beyond them.
func TestAnUnstableBuildIsRemovedLikeAnyOtherThatEnded(t *testing.T) {
	st, logs := openStore(t)
	ctx := context.Background()
	now := time.Now()
	seed(t, st, logs, "a", 3, now)
	all, err := st.Builds(ctx, "a", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range all {
		if err := st.SetBuildStatus(ctx, b.ID, store.Unstable, now); err != nil {
			t.Fatal(err)
		}
	}
	job := parse(t, "name: a\nkeep: {builds: 1}\nstages:\n  s: {run: true}\n")
	if err := Job(ctx, st, job, logs, now); err != nil {
		t.Fatal(err)
	}
	left, _ := st.Builds(ctx, "a", 10)
	if len(left) != 1 || left[0].Number != 3 || left[0].Status != store.Unstable {
		t.Fatalf("%+v", left)
	}
	if exists(filepath.Join(logs, "a", "1")) || exists(filepath.Join(logs, "a", "2")) || !exists(filepath.Join(logs, "a", "3", "s.log")) {
		t.Fatal("the logs kept are not those of the one build kept")
	}
}

func TestARemovedJobsBuildsAllGoOnceTheyAreOlderThanItsDaysAndNotBefore(t *testing.T) {
	st, logs := openStore(t)
	ctx := context.Background()
	now := time.Now()
	seed(t, st, logs, "gone", 3, now) // queued three, two and one day ago
	seed(t, st, logs, "live", 3, now)
	live := &config.Job{Name: "live", Keep: config.Keep{Builds: config.DefaultKeepBuilds}}
	snap := &config.Snapshot{Jobs: map[string]*config.Job{"live": live}, Names: []string{"live"}}
	left := func(job string) int {
		t.Helper()
		builds, err := st.Builds(ctx, job, 10)
		if err != nil {
			t.Fatal(err)
		}
		return len(builds)
	}
	// Half a day before the newest reaches the limit, the older two are past it.
	n, err := Sweep(ctx, st, snap, logs, now.AddDate(0, 0, config.RemovedKeepDays-2).Add(12*time.Hour))
	if err != nil || n != 2 || left("gone") != 1 {
		t.Fatalf("err %v, removed %d, left %d: want the two builds past the limit gone and the newest kept", err, n, left("gone"))
	}
	n, err = Sweep(ctx, st, snap, logs, now.AddDate(0, 0, config.RemovedKeepDays+2))
	if err != nil || n != 1 || left("gone") != 0 {
		t.Fatalf("err %v, removed %d, left %d: want nothing left of a removed job", err, n, left("gone"))
	}
	if exists(filepath.Join(logs, "gone", "3")) {
		t.Fatal("the newest build's logs outlived it")
	}
	// A job that still has its file and no days in its rule keeps its newest however old.
	if left("live") != 3 {
		t.Fatalf("a live job lost builds to the removed jobs' limit: %d left", left("live"))
	}
}

func TestSweepFindsARemovedJobThatHasBuildsAndNoLogs(t *testing.T) {
	st, logs := openStore(t)
	ctx := context.Background()
	now := time.Now()
	b := &store.Build{Job: "quiet", Status: store.Failed, Trigger: "cli", QueuedAt: now}
	if err := st.CreateBuild(ctx, b); err != nil {
		t.Fatal(err)
	}
	snap := &config.Snapshot{Jobs: map[string]*config.Job{}}
	n, err := Sweep(ctx, st, snap, logs, now.AddDate(0, 0, config.RemovedKeepDays+1))
	if err != nil || n != 1 {
		t.Fatalf("err %v, removed %d: want the build of a removed job that never wrote a log gone", err, n)
	}
}

func TestSweepKeepsTheNewestAuditRowsAndNoOlder(t *testing.T) {
	st, logs := openStore(t)
	ctx := context.Background()
	now := time.Now()
	for i := range AuditRows + 2 {
		if err := st.AppendAudit(ctx, store.Audit{At: now, Actor: "cli", Action: "secret.set", Target: "s" + strconv.Itoa(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Sweep(ctx, st, &config.Snapshot{}, logs, now); err != nil {
		t.Fatal(err)
	}
	rows, err := st.AuditBefore(ctx, 0, AuditRows+10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != AuditRows || rows[0].Target != "s"+strconv.Itoa(AuditRows+1) || rows[len(rows)-1].Target != "s2" {
		t.Fatalf("kept %d rows, newest %q, oldest %q", len(rows), rows[0].Target, rows[len(rows)-1].Target)
	}
}

func TestSweepForgetsWebhookDeliveriesOlderThanTheirDays(t *testing.T) {
	st, logs := openStore(t)
	ctx := context.Background()
	now := time.Now()
	old := now.AddDate(0, 0, -DeliveryDays).Add(-time.Minute)
	recent := now.AddDate(0, 0, -DeliveryDays).Add(time.Minute)
	for id, at := range map[string]time.Time{"old": old, "recent": recent} {
		if err := st.RecordDelivery(ctx, id, "a", at); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Sweep(ctx, st, &config.Snapshot{}, logs, now); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordDelivery(ctx, "old", "a", now); err != nil {
		t.Fatalf("a delivery older than %d days is still remembered: %v", DeliveryDays, err)
	}
	if err := st.RecordDelivery(ctx, "recent", "a", now); err == nil {
		t.Fatalf("a delivery younger than %d days was forgotten", DeliveryDays)
	}
}
