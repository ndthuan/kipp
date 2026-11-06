package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/logtime"
	"github.com/ndthuan/kipp/internal/runner"
	"github.com/ndthuan/kipp/internal/store"
)

type harness struct {
	d    *Dispatcher
	st   *store.Store
	dir  string
	stop func() // ends Run and returns once every build has ended
}

func newHarness(t *testing.T, files map[string]string, opts ...func(*Options)) *harness {
	t.Helper()
	dir := t.TempDir()
	snap, err := config.Load(writeJobs(t, dir, files))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	o := Options{DataDir: dir, Executors: 4, Secrets: func(_ context.Context, names []string) (map[string]Secret, error) {
		m := map[string]Secret{}
		for _, n := range names {
			m[n] = Secret{Kind: config.SecretText, Value: "value-of-" + n, Scopes: []string{config.ScopeGlobal}}
		}
		return m, nil
	}}
	for _, f := range opts {
		f(&o)
	}
	d := New(st, runner.Local{}, o)
	d.SetSnapshot(snap)
	ctx, cancel := context.WithCancel(context.Background())
	if err := d.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	ended := make(chan struct{})
	go func() { d.Run(ctx); close(ended) }()
	stop := func() { cancel(); <-ended }
	t.Cleanup(func() { stop(); _ = st.Close() })
	return &harness{d: d, st: st, dir: dir, stop: stop}
}

func writeJobs(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	jobs := filepath.Join(dir, "jobs")
	if err := os.MkdirAll(jobs, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(jobs, name+".yml"), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return jobs
}

func (h *harness) trigger(t *testing.T, job string) *store.Build {
	t.Helper()
	b, err := h.d.Trigger(context.Background(), TriggerRequest{Job: job, Kind: "cli", By: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (h *harness) wait(t *testing.T, id int64) *store.Build {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, err := h.d.Wait(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (h *harness) log(t *testing.T, b *store.Build, stage string) string {
	t.Helper()
	data, err := os.ReadFile(h.d.LogPath(b.Job, b.Number, stage))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (h *harness) stages(t *testing.T, id int64) map[string]*store.Stage {
	t.Helper()
	st, err := h.st.Stages(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*store.Stage{}
	for _, s := range st {
		byName[s.Name] = s
	}
	return byName
}

// eventually polls until ok, and fails the test after ten seconds.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting until %s", what)
}

func (h *harness) untilRunning(t *testing.T, id int64) {
	t.Helper()
	eventually(t, "the build is running", func() bool {
		b, err := h.st.Build(context.Background(), id)
		return err == nil && b.Status == store.Running
	})
}

// gated is a command that blocks until the returned file exists.
func gated(t *testing.T) (command string, open func()) {
	t.Helper()
	gate := filepath.Join(t.TempDir(), "gate")
	open = func() {
		if err := os.WriteFile(gate, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return fmt.Sprintf("while [ ! -e %s ]; do sleep 0.02; done", gate), open
}

const dagJob = `
name: dag
stages:
  a: {run: "echo A > $KIPP_WORKSPACE/a; echo stage-a"}
  b: {run: "echo B > $KIPP_WORKSPACE/b; sleep 0.2"}
  c: {needs: [a, b], run: "cat $KIPP_WORKSPACE/a $KIPP_WORKSPACE/b"}
`

func TestStagesRunByTheDAGAndShareTheWorkspace(t *testing.T) {
	h := newHarness(t, map[string]string{"dag": dagJob})
	b := h.wait(t, h.trigger(t, "dag").ID)
	if st := h.stages(t, b.ID); !slices.Equal(st["c"].Needs, []string{"a", "b"}) || len(st["a"].Needs) != 0 {
		t.Fatalf("recorded needs: c %v, a %v", st["c"].Needs, st["a"].Needs)
	}
	if b.Status != store.Success {
		t.Fatalf("%+v", b)
	}
	if got := h.log(t, b, "c"); got != "A\nB\n" {
		t.Fatalf("c log %q", got)
	}
	st, _ := h.st.Stages(context.Background(), b.ID)
	if len(st) != 3 || st[2].Status != store.StageSuccess || st[0].StartedAt == nil {
		t.Fatalf("%+v", st)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "workspace", "dag", "1")); !os.IsNotExist(err) {
		t.Fatal("workspace not removed")
	}
}

func TestAFailureSkipsDependantsOtherBranchesFinish(t *testing.T) {
	h := newHarness(t, map[string]string{"f": `
name: f
stages:
  a: {run: "exit 1"}
  b: {run: "echo fine"}
  c: {needs: [a], run: "echo never"}
`})
	b := h.wait(t, h.trigger(t, "f").ID)
	st := h.stages(t, b.ID)
	if b.Status != store.Failed || st["a"].Status != store.StageFailed || *st["a"].ExitCode != 1 || st["b"].Status != store.StageSuccess || st["c"].Status != store.StageSkipped {
		t.Fatalf("%v %+v", b.Status, st)
	}
}

func TestQueueRunsOneAtATimeAndParallelOverlaps(t *testing.T) {
	h := newHarness(t, map[string]string{
		"q": "name: q\nstages:\n  s: {run: sleep 0.3}\n",
		"p": "name: p\nconcurrency: parallel\nstages:\n  s: {run: sleep 0.3}\n",
	})
	q1, q2 := h.trigger(t, "q"), h.trigger(t, "q")
	p1, p2 := h.trigger(t, "p"), h.trigger(t, "p")
	q1, q2 = h.wait(t, q1.ID), h.wait(t, q2.ID)
	p1, p2 = h.wait(t, p1.ID), h.wait(t, p2.ID)
	if q2.StartedAt.Before(*q1.FinishedAt) {
		t.Fatalf("q2 started %v before q1 finished %v", q2.StartedAt, q1.FinishedAt)
	}
	if !p2.StartedAt.Before(*p1.FinishedAt) {
		t.Fatalf("parallel did not overlap: %v %v", p2.StartedAt, p1.FinishedAt)
	}
}

func TestDropDiscardsATriggerOnARunningBuild(t *testing.T) {
	block, open := gated(t)
	h := newHarness(t, map[string]string{"d": "name: d\nconcurrency: drop\nstages:\n  s: {run: '" + block + "'}\n"})
	first := h.trigger(t, "d")
	h.untilRunning(t, first.ID)
	_, err := h.d.Trigger(context.Background(), TriggerRequest{Job: "d", Kind: "api"})
	if !errors.Is(err, ErrDropped) {
		t.Fatalf("err %v", err)
	}
	act, _ := h.st.Activity(context.Background(), "d", 5)
	if len(act) != 1 || !strings.Contains(act[0].Line, "dropped") {
		t.Fatalf("%+v", act)
	}
	if all, _ := h.st.Builds(context.Background(), "d", 5); len(all) != 1 {
		t.Fatalf("%d builds, want 1", len(all))
	}
	open()
	h.wait(t, first.ID)
}

func TestSupersedeCancelsTheQueuedBuildsOfTheJobAndSparesTheRunningOne(t *testing.T) {
	block, open := gated(t)
	h := newHarness(t, map[string]string{
		"s":     "name: s\nconcurrency: supersede\nstages:\n  s: {run: '" + block + "'}\n",
		"other": "name: other\nstages:\n  s: {run: '" + block + "'}\n",
	})
	ctx := context.Background()
	running := h.trigger(t, "s")
	h.untilRunning(t, running.ID)
	otherRunning := h.trigger(t, "other")
	h.untilRunning(t, otherRunning.ID)
	otherQueued := h.trigger(t, "other")
	first, second, third := h.trigger(t, "s"), h.trigger(t, "s"), h.trigger(t, "s")
	for _, c := range []struct {
		b    *store.Build
		want store.Status
	}{{first, store.Cancelled}, {second, store.Cancelled}, {third, store.Queued}, {running, store.Running}, {otherQueued, store.Queued}} {
		got, err := h.st.Build(ctx, c.b.ID)
		if err != nil || got.Status != c.want {
			t.Fatalf("#%d of %s is %v (%v), want %v", c.b.Number, c.b.Job, got.Status, err, c.want)
		}
	}
	act, _ := h.st.Activity(ctx, "s", 5)
	if len(act) != 2 || !strings.Contains(act[0].Line, fmt.Sprintf("#%d superseded by #%d", second.Number, third.Number)) {
		t.Fatalf("%+v", act)
	}
	open()
	if b := h.wait(t, third.ID); b.Status != store.Success {
		t.Fatalf("#%d %v", b.Number, b.Status)
	}
}

func TestACronFireOnAQueuedBuildIsDroppedNotStacked(t *testing.T) {
	block, open := gated(t)
	h := newHarness(t, map[string]string{"c": "name: c\ntriggers: {cron: '* * * * *'}\nstages:\n  s: {run: '" + block + "'}\n"})
	ctx := context.Background()
	first := h.trigger(t, "c")
	h.untilRunning(t, first.ID)
	now := time.Now()
	h.d.Tick(ctx, now) // queues one behind the running build
	h.d.Tick(ctx, now.Add(time.Minute))
	h.d.Tick(ctx, now.Add(2*time.Minute))
	queued, _ := h.st.BuildsWithStatus(ctx, store.Queued)
	if len(queued) != 1 || queued[0].Trigger != "cron" {
		t.Fatalf("queued %+v, want one cron build", queued)
	}
	act, _ := h.st.Activity(ctx, "c", 5)
	if len(act) != 2 || !strings.Contains(act[0].Line, "dropped a cron trigger") {
		t.Fatalf("%+v", act)
	}
	// A trigger that is not a cron fire still queues behind it.
	manual := h.trigger(t, "c")
	open()
	for _, id := range []int64{first.ID, queued[0].ID, manual.ID} {
		if b := h.wait(t, id); b.Status != store.Success {
			t.Fatalf("#%d %v", b.Number, b.Status)
		}
	}
}

func TestATickFiresOnlyTheMinuteTheCronNames(t *testing.T) {
	h := newHarness(t, map[string]string{"c": "name: c\ntriggers: {cron: '30 4 * * *'}\nstages:\n  s: {run: 'true'}\n"})
	ctx := context.Background()
	day := time.Date(2026, 10, 11, 0, 0, 0, 0, time.Local)
	h.d.Tick(ctx, day.Add(4*time.Hour+29*time.Minute))
	h.d.Tick(ctx, day.Add(4*time.Hour+31*time.Minute))
	if all, _ := h.st.Builds(ctx, "c", 5); len(all) != 0 {
		t.Fatalf("fired off the minute: %+v", all)
	}
	h.d.Tick(ctx, day.Add(4*time.Hour+30*time.Minute+20*time.Second))
	all, _ := h.st.Builds(ctx, "c", 5)
	if len(all) != 1 || all[0].Trigger != "cron" {
		t.Fatalf("%+v", all)
	}
	h.wait(t, all[0].ID)
}

func TestATickReadsTheCronInTheJobsZoneElseTheServersElseTheHosts(t *testing.T) {
	server := time.FixedZone("server", 7*3600)
	h := newHarness(t, map[string]string{
		"own":  "name: own\ntimezone: Asia/Tokyo\ntriggers: {cron: '30 4 * * *'}\nstages:\n  s: {run: 'true'}\n",
		"kipp": "name: kipp\ntriggers: {cron: '30 4 * * *'}\nstages:\n  s: {run: 'true'}\n",
	}, func(o *Options) { o.TimeZone = server })
	ctx := context.Background()
	fired := func(job string) int {
		all, _ := h.st.Builds(ctx, job, 5)
		for _, b := range all {
			h.wait(t, b.ID)
		}
		return len(all)
	}
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	h.d.Tick(ctx, time.Date(2026, 10, 11, 4, 30, 0, 0, tokyo))
	if fired("own") != 1 || fired("kipp") != 0 {
		t.Fatalf("at 04:30 in Tokyo: own %d, kipp %d", fired("own"), fired("kipp"))
	}
	h.d.Tick(ctx, time.Date(2026, 10, 11, 4, 30, 0, 0, server))
	if fired("own") != 1 || fired("kipp") != 1 {
		t.Fatalf("at 04:30 in the server's zone: own %d, kipp %d", fired("own"), fired("kipp"))
	}
	if z := h.d.Zone(h.d.Snapshot().Jobs["kipp"]); z != server {
		t.Fatalf("zone %v", z)
	}

	host := newHarness(t, map[string]string{"c": "name: c\nstages:\n  s: {run: 'true'}\n"})
	if z := host.d.Zone(host.d.Snapshot().Jobs["c"]); z != time.Local {
		t.Fatalf("with no zone given anywhere: %v", z)
	}
}

func TestANamedLockSerialisesBuildsInEveryMode(t *testing.T) {
	h := newHarness(t, map[string]string{
		"x": "name: x\nlocks: [checkout]\nstages:\n  s: {run: sleep 0.3}\n",
		"y": "name: y\nconcurrency: drop\nlocks: [checkout]\nstages:\n  s: {run: sleep 0.1}\n",
		"z": "name: z\nconcurrency: parallel\nlocks: [checkout, other]\nstages:\n  s: {run: sleep 0.1}\n",
	})
	ids := []int64{h.trigger(t, "x").ID}
	h.untilRunning(t, ids[0])
	ids = append(ids, h.trigger(t, "y").ID, h.trigger(t, "z").ID, h.trigger(t, "z").ID)
	var builds []*store.Build
	for _, id := range ids {
		b := h.wait(t, id)
		if b.Status != store.Success {
			t.Fatalf("%s#%d %v", b.Job, b.Number, b.Status)
		}
		builds = append(builds, b)
	}
	if n := mostAtOnce(builds); n != 1 {
		t.Fatalf("%d builds held the lock at once", n)
	}
}

// mostAtOnce is the largest number of builds whose running spans overlap.
func mostAtOnce(builds []*store.Build) int {
	type edge struct {
		at    time.Time
		delta int
	}
	var edges []edge
	for _, b := range builds {
		edges = append(edges, edge{*b.StartedAt, 1}, edge{*b.FinishedAt, -1})
	}
	slices.SortFunc(edges, func(a, b edge) int {
		if c := a.at.Compare(b.at); c != 0 {
			return c
		}
		return a.delta - b.delta
	})
	most, now := 0, 0
	for _, e := range edges {
		now += e.delta
		most = max(most, now)
	}
	return most
}

// --executors bounds the stages the server runs at once, so the spans measured
// are the stages': a build's own span also holds its preparing and finishing,
// and a build whose stage ended may be counted beside two that run.
func TestExecutorsBoundHowManyStagesRunAtOnce(t *testing.T) {
	h := newHarness(t, map[string]string{
		"e": "name: e\nconcurrency: parallel\nstages:\n  s: {run: sleep 0.3}\n",
	}, func(o *Options) { o.Executors = 2 })
	var ids []int64
	for range 5 {
		ids = append(ids, h.trigger(t, "e").ID)
	}
	var spans []*store.Build
	for _, id := range ids {
		h.wait(t, id)
		for _, s := range h.stages(t, id) {
			spans = append(spans, &store.Build{StartedAt: s.StartedAt, FinishedAt: s.FinishedAt})
		}
	}
	if n := mostAtOnce(spans); n != 2 {
		t.Fatalf("%d stages ran at once, want 2", n)
	}
}

func TestExecutorsBoundTheStagesOfOneBuild(t *testing.T) {
	h := newHarness(t, map[string]string{"w": `
name: w
stages:
  a: {run: sleep 0.3}
  b: {run: sleep 0.3}
  c: {run: sleep 0.3}
  d: {run: sleep 0.3}
`}, func(o *Options) { o.Executors = 2 })
	b := h.wait(t, h.trigger(t, "w").ID)
	if b.Status != store.Success {
		t.Fatalf("%v", b.Status)
	}
	var spans []*store.Build
	for _, s := range h.stages(t, b.ID) {
		spans = append(spans, &store.Build{StartedAt: s.StartedAt, FinishedAt: s.FinishedAt})
	}
	if n := mostAtOnce(spans); n != 2 {
		t.Fatalf("%d stages ran at once, want 2", n)
	}
}

// groupJob starts a background child, records both pids, and hangs.
func groupJob(name, extra, pids string) string {
	return fmt.Sprintf(`
name: %s
%s
stages:
  s: {run: 'sleep 60 & echo $! > %s.tmp; echo $$ >> %s.tmp; mv %s.tmp %s; wait'}
  later: {needs: [s], run: echo never}
`, name, extra, pids, pids, pids, pids)
}

func readPids(t *testing.T, path string) []int {
	t.Helper()
	var pids []int
	eventually(t, "the stage wrote its pids", func() bool {
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		for f := range strings.FieldsSeq(string(data)) {
			pid, _ := strconv.Atoi(f)
			pids = append(pids, pid)
		}
		return true
	})
	if len(pids) != 2 {
		t.Fatalf("pids %v", pids)
	}
	return pids
}

func assertGone(t *testing.T, pids []int) {
	t.Helper()
	eventually(t, "the stage's whole process group is gone", func() bool {
		return !slices.ContainsFunc(pids, func(pid int) bool { return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) })
	})
}

func (h *harness) assertStoppedAt(t *testing.T, b *store.Build, want store.Status) {
	t.Helper()
	st := h.stages(t, b.ID)
	if b.Status != want || st["s"].Status != store.StageCancelled || st["later"].Status != store.StageSkipped || st["later"].StartedAt != nil {
		t.Fatalf("%v s=%+v later=%+v", b.Status, st["s"], st["later"])
	}
	if _, err := os.Stat(h.d.LogPath(b.Job, b.Number, "later")); !os.IsNotExist(err) {
		t.Fatal("the later stage ran")
	}
}

func (h *harness) assertLogEnds(t *testing.T, b *store.Build, stage, want string) {
	t.Helper()
	if got := h.log(t, b, stage); !strings.HasSuffix(got, want) {
		t.Fatalf("the log of %s ends %q, want %q at its end", stage, got, want)
	}
}

func TestCancelKillsTheStagesProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pids")
	h := newHarness(t, map[string]string{"slow": groupJob("slow", "", pidFile)})
	slow := h.trigger(t, "slow")
	pids := readPids(t, pidFile)
	if err := h.d.Cancel(context.Background(), slow.ID, "ada"); err != nil {
		t.Fatal(err)
	}
	b := h.wait(t, slow.ID)
	h.assertStoppedAt(t, b, store.Cancelled)
	assertGone(t, pids)
	h.assertLogEnds(t, b, "s", "kipp: cancelled by ada\n")
	if err := h.d.Cancel(context.Background(), slow.ID, "ada"); !errors.Is(err, ErrTooLate) || !strings.Contains(err.Error(), "it is cancelled") {
		t.Fatalf("cancelling a finished build: %v", err)
	}
	if err := h.d.Cancel(context.Background(), 99, "ada"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cancelling no build: %v", err)
	}
}

func TestAStageWaitingForAnExecutorNeverStartsOnceCancelled(t *testing.T) {
	block, _ := gated(t)
	started := filepath.Join(t.TempDir(), "started")
	run := fmt.Sprintf("echo $KIPP_STAGE >> %s; %s", started, block)
	h := newHarness(t, map[string]string{
		"w": "name: w\nstages:\n  a: {run: '" + run + "'}\n  b: {run: '" + run + "'}\n",
	}, func(o *Options) { o.Executors = 1 })
	b := h.trigger(t, "w")
	eventually(t, "one stage started", func() bool {
		data, _ := os.ReadFile(started)
		return len(data) > 0
	})
	if err := h.d.Cancel(context.Background(), b.ID, "ada"); err != nil {
		t.Fatal(err)
	}
	if got := h.wait(t, b.ID); got.Status != store.Cancelled {
		t.Fatalf("%v", got.Status)
	}
	data, _ := os.ReadFile(started)
	ran := strings.TrimSpace(string(data))
	waited := map[string]string{"a": "b", "b": "a"}[ran]
	st := h.stages(t, b.ID)
	if waited == "" || st[ran].Status != store.StageCancelled || st[waited].Status != store.StageCancelled || st[waited].StartedAt != nil {
		t.Fatalf("ran %q: %+v %+v", ran, st["a"], st["b"])
	}
	if _, err := os.Stat(h.d.LogPath("w", b.Number, waited)); !os.IsNotExist(err) {
		t.Fatal("the waiting stage was started after the cancel")
	}
}

func TestTimeoutKillsTheStagesProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pids")
	h := newHarness(t, map[string]string{"to": groupJob("to", "timeout: 1s", pidFile)})
	b := h.wait(t, h.trigger(t, "to").ID)
	h.assertStoppedAt(t, b, store.Timeout)
	assertGone(t, readPids(t, pidFile))
	h.assertLogEnds(t, b, "s", "kipp: the job's timeout of 1s passed\n")
}

func TestCancelAQueuedBuild(t *testing.T) {
	block, open := gated(t)
	h := newHarness(t, map[string]string{"q": "name: q\nstages:\n  s: {run: '" + block + "'}\n"})
	first := h.trigger(t, "q")
	second := h.trigger(t, "q")
	h.untilRunning(t, first.ID)
	if h.d.Busy(second.ID) || !h.d.Busy(first.ID) {
		t.Fatal("busy is the running build, not the queued one")
	}
	if err := h.d.Cancel(context.Background(), second.ID, "ada"); err != nil {
		t.Fatal(err)
	}
	if b := h.wait(t, second.ID); b.Status != store.Cancelled || b.StartedAt != nil {
		t.Fatalf("%+v", b)
	}
	if err := h.d.Cancel(context.Background(), second.ID, "ada"); !errors.Is(err, ErrTooLate) {
		t.Fatalf("cancelling it twice: %v", err)
	}
	open()
	if b := h.wait(t, first.ID); b.Status != store.Success {
		t.Fatalf("%v", b.Status)
	}
	if b, _ := h.st.Build(context.Background(), second.ID); b.Status != store.Cancelled {
		t.Fatalf("the cancelled build came back as %v", b.Status)
	}
}

func TestRecoverMarksRunningLostAndRerunsQueued(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	running := &store.Build{Job: "r", Status: store.Running, Trigger: "cli"}
	queued := &store.Build{Job: "r", Status: store.Queued, Trigger: "cli"}
	for _, b := range []*store.Build{running, queued} {
		if err := st.CreateBuild(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	_ = st.Close()
	left := filepath.Join(dir, "workspace", "r", "1")
	if err := os.MkdirAll(left, 0o750); err != nil {
		t.Fatal(err)
	}
	// A second process starts over the same data.
	st2 := reopen(t, dir)
	snap, err := config.Load(writeJobs(t, dir, map[string]string{"r": "name: r\nstages:\n  s: {run: echo back}\n"}))
	if err != nil {
		t.Fatal(err)
	}
	d := New(st2, runner.Local{}, Options{DataDir: dir, Executors: 2})
	d.SetSnapshot(snap)
	if err := d.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "workspace")); !os.IsNotExist(err) {
		t.Fatal("the lost build's workspace was left on disk")
	}
	runCtx, stop := context.WithCancel(ctx)
	ended := make(chan struct{})
	go func() { d.Run(runCtx); close(ended) }()
	defer func() { stop(); <-ended }()
	wctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	b, err := d.Wait(wctx, queued.ID)
	if err != nil || b.Status != store.Success {
		t.Fatal(err, b)
	}
	lost, err := d.Wait(wctx, running.ID)
	if err != nil || lost.Status != store.Lost || lost.StartedAt != nil {
		t.Fatalf("%v %+v", err, lost)
	}
	if _, err := os.Stat(d.LogPath("r", running.Number, "s")); !os.IsNotExist(err) {
		t.Fatal("the lost build was rerun")
	}
}

func TestShutdownEndsARunningBuildAsLostAndLeavesTheQueue(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pids")
	h := newHarness(t, map[string]string{"g": groupJob("g", "on_failure: {run: echo hook}", pidFile)})
	first, second := h.trigger(t, "g"), h.trigger(t, "g")
	pids := readPids(t, pidFile)
	h.stop()
	assertGone(t, pids)
	ctx := context.Background()
	if b, _ := h.st.Build(ctx, first.ID); b.Status != store.Lost {
		t.Fatalf("%v", b.Status)
	}
	h.assertLogEnds(t, first, "s", "kipp: the server stopped while this ran\n")
	if b, _ := h.st.Build(ctx, second.ID); b.Status != store.Queued {
		t.Fatalf("%v", b.Status)
	}
	if _, err := os.Stat(h.d.LogPath("g", first.Number, "hooks")); !os.IsNotExist(err) {
		t.Fatal("a hook ran for a build the shutdown ended")
	}
}

func TestAStageSeesParamsEnvThePassedServerVariablesAndTheKippVariables(t *testing.T) {
	t.Setenv("KIPP_TEST_SERVER_ENV", "from-server")
	h := newHarness(t, map[string]string{"s": `
name: s
params:
  P: {default: pv}
env: {E: ev}
stages:
  build: {run: 'echo p=$P e=$E job=$KIPP_JOB build=$KIPP_BUILD stage=$KIPP_STAGE trigger=$KIPP_TRIGGER url=$KIPP_URL server=$KIPP_TEST_SERVER_ENV; test "$(pwd -P)" = "$(cd $KIPP_WORKSPACE && pwd -P)" && echo $KIPP_WORKSPACE'}
`}, func(o *Options) { o.BaseURL, o.PassEnv = "https://kipp.test", []string{"PATH", "KIPP_TEST_SERVER_ENV"} })
	b := h.wait(t, h.trigger(t, "s").ID)
	want := "p=pv e=ev job=s build=1 stage=build trigger=cli url=https://kipp.test server=from-server\n" +
		filepath.Join(h.dir, "workspace", "s", "1") + "\n"
	if got := h.log(t, b, "build"); got != want {
		t.Fatalf("log %q\nwant %q", got, want)
	}
}

func assertMasked(t *testing.T, name, got, want string) {
	t.Helper()
	if strings.Contains(got, "value-of-TOKEN") || !strings.Contains(got, want) {
		t.Fatalf("%s log %q, want %q and no secret", name, got, want)
	}
}

func TestSecretsReachTheStageAndNoLog(t *testing.T) {
	h := newHarness(t, map[string]string{"s": `
name: s
secrets: [TOKEN]
stages:
  s:
    run: 'echo token=$TOKEN; mkdir r; echo broken > r/$TOKEN.xml; test "$TOKEN" = value-of-TOKEN'
    junit: r/*.xml
on_success: {run: 'printf "hook token=$TOKEN held=value-of-"'}
`})
	b := h.wait(t, h.trigger(t, "s").ID)
	if b.Status != store.Success {
		t.Fatalf("the stage did not see the secret: %v", b.Status)
	}
	stage := h.log(t, b, "s")
	assertMasked(t, "stage", stage, "token=***\n")
	assertMasked(t, "stage", stage, "kipp: junit: r/***.xml: ")
	// The log ends on what could have been the start of the secret: it is
	// written when the hook ends, not held back for ever.
	if got := h.log(t, b, "hooks"); got != "hook token=*** held=value-of-" {
		t.Fatalf("hooks log %q", got)
	}
}

func TestSecretsStayOutOfTheCloneAndPrepareLogs(t *testing.T) {
	h := newHarness(t, map[string]string{
		"src": "name: src\nsecrets: [TOKEN]\nsource: {repo: o/r, ref: main, path: kipp.yml}\non_failure: {run: 'echo failed status=$KIPP_STATUS'}\n",
	}, func(o *Options) {
		o.Clone = func(_ context.Context, _, _, _ string, out io.Writer) error {
			_, err := io.WriteString(out, "fetching with value-of-TOKEN\n")
			return errors.Join(err, errors.New("denied for value-of-TOKEN"))
		}
	})
	b := h.wait(t, h.trigger(t, "src").ID)
	if b.Status != store.Failed {
		t.Fatalf("%v", b.Status)
	}
	assertMasked(t, "clone", h.log(t, b, "clone"), "fetching with ***\n")
	assertMasked(t, "prepare", h.log(t, b, "prepare"), "denied for ***\n")
	assertMasked(t, "hooks", h.log(t, b, "hooks"), "failed status=failed\n")
}

func TestTheCredentialsInASourceURLStayOutOfEveryLog(t *testing.T) {
	const repo = "https://ci-bot:pw-of-the-bot@git.example.com/o/r.git"
	server := &lockedLog{}
	h := newHarness(t, map[string]string{
		"src": "name: src\nsource: {repo: '" + repo + "', ref: main, path: kipp.yml}\n",
	}, func(o *Options) {
		o.Logger = slog.New(slog.NewTextHandler(server, nil))
		o.Clone = func(_ context.Context, _, repo, _ string, out io.Writer) error {
			_, err := io.WriteString(out, "fatal: could not read from "+repo+"\n")
			return errors.Join(err, errors.New("git fetch: "+repo))
		}
	})
	b := h.wait(t, h.trigger(t, "src").ID)
	act, err := h.st.Activity(context.Background(), "src", 50)
	if err != nil {
		t.Fatal(err)
	}
	logs := h.log(t, b, "clone") + h.log(t, b, "prepare") + fmt.Sprint(act) + server.String()
	if strings.Contains(logs, "pw-of-the-bot") || strings.Contains(logs, "ci-bot") {
		t.Fatalf("the URL's credentials reached a log: %s", logs)
	}
	if !strings.Contains(h.log(t, b, "prepare"), "https://git.example.com/o/r.git") {
		t.Fatalf("the prepare log does not name the repo: %q", h.log(t, b, "prepare"))
	}
}

func TestARepoRefusedForItsDashIsNamedWithoutItsCredentials(t *testing.T) {
	err := gitClone(context.Background(), t.TempDir(), "-https://u:pw-of-it@host/o", "main", io.Discard)
	if err == nil || strings.Contains(err.Error(), "pw-of-it") || !strings.Contains(err.Error(), "-https://host/o") {
		t.Fatalf("%v", err)
	}
}

func TestAStageIsGivenOnlyTheServerVariablesPassEnvNames(t *testing.T) {
	t.Setenv("KIPP_TEST_SERVER_ONLY", "leaked")
	t.Setenv("LC_KIPPTEST", "kept")
	job := map[string]string{"e": "name: e\nenv: {FROM_JOB: job}\nstages:\n  s: {run: 'echo only=$KIPP_TEST_SERVER_ONLY lc=$LC_KIPPTEST job=$FROM_JOB path=${PATH:+set}'}\non_success: {run: 'echo hook only=$KIPP_TEST_SERVER_ONLY'}\n"}
	for _, c := range []struct {
		pass        []string
		stage, hook string
	}{
		{nil, "only= lc=kept job=job path=set\n", "hook only=\n"},
		{[]string{"*"}, "only=leaked lc=kept job=job path=set\n", "hook only=leaked\n"},
		{[]string{"KIPP_TEST_*", "PATH"}, "only=leaked lc= job=job path=set\n", "hook only=leaked\n"},
	} {
		h := newHarness(t, job, func(o *Options) { o.PassEnv = c.pass })
		b := h.wait(t, h.trigger(t, "e").ID)
		if got := h.log(t, b, "s"); got != c.stage {
			t.Errorf("%v: stage %q, want %q", c.pass, got, c.stage)
		}
		if got := h.log(t, b, "hooks"); got != c.hook {
			t.Errorf("%v: hook %q, want %q", c.pass, got, c.hook)
		}
	}
}

func TestHooksRunAndAfterTriggersFire(t *testing.T) {
	h := newHarness(t, map[string]string{
		"up":   "name: up\nstages:\n  s: {run: echo up > made}\non_success: {run: 'echo hook status=$KIPP_STATUS; cat made'}\n",
		"down": "name: down\ntriggers: {after: [up]}\nstages:\n  s: {run: 'echo from=$KIPP_UPSTREAM_BUILD'}\n",
		"bad":  "name: bad\nstages:\n  s: {run: exit 2}\non_failure: {run: 'echo failed status=$KIPP_STATUS'}\non_success: {run: echo wrong hook}\n",
	})
	up := h.wait(t, h.trigger(t, "up").ID)
	if got := h.log(t, up, "hooks"); got != "hook status=success\nup\n" {
		t.Fatalf("hooks %q", got)
	}
	var downs []*store.Build
	eventually(t, "the downstream job was triggered", func() bool {
		downs, _ = h.st.Builds(context.Background(), "down", 5)
		return len(downs) == 1
	})
	down := h.wait(t, downs[0].ID)
	if down.Trigger != "after" || !strings.Contains(h.log(t, down, "s"), "from=up#1") {
		t.Fatalf("%+v", down)
	}
	bad := h.wait(t, h.trigger(t, "bad").ID)
	if got := h.log(t, bad, "hooks"); got != "failed status=failed\n" {
		t.Fatalf("hooks %q", got)
	}
	if downs, _ = h.st.Builds(context.Background(), "down", 5); len(downs) != 1 {
		t.Fatalf("a failed build triggered downstream: %+v", downs)
	}
}

func TestTheStatusIsFinalBeforeTheHookAndWaitReturnsAfterIt(t *testing.T) {
	block, open := gated(t)
	h := newHarness(t, map[string]string{"k": "name: k\nstages:\n  s: {run: 'true'}\non_success: {run: '" + block + "; echo hook done'}\n"})
	ctx := context.Background()
	b := h.trigger(t, "k")
	eventually(t, "the stages are over", func() bool {
		got, err := h.st.Build(ctx, b.ID)
		return err == nil && got.Status == store.Success
	})
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := h.d.Wait(short, b.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait returned while the hook ran: %v", err)
	}
	if !h.d.Busy(b.ID) {
		t.Fatal("a build whose hook runs is not busy")
	}
	if err := h.d.Cancel(ctx, b.ID, "ada"); !errors.Is(err, ErrTooLate) || !strings.Contains(err.Error(), "already finishing") {
		t.Fatalf("a cancel after the status is final must say so: %v", err)
	}
	open()
	if got := h.wait(t, b.ID); got.Status != store.Success || h.log(t, got, "hooks") != "hook done\n" {
		t.Fatalf("%v", got.Status)
	}
	if h.d.Busy(b.ID) {
		t.Fatal("a build Wait has returned is still busy")
	}
}

func TestShutdownDoesNotWaitForAHook(t *testing.T) {
	h := newHarness(t, map[string]string{"k": "name: k\nstages:\n  s: {run: 'true'}\non_success: {run: 'echo hook up; sleep 60'}\n"})
	b := h.trigger(t, "k")
	eventually(t, "the hook is running", func() bool {
		data, _ := os.ReadFile(h.d.LogPath("k", b.Number, "hooks"))
		return len(data) > 0
	})
	began := time.Now()
	h.stop()
	if took := time.Since(began); took > 5*time.Second {
		t.Fatalf("shutdown waited %v for the hook", took)
	}
	if got, _ := h.st.Build(context.Background(), b.ID); got.Status != store.Success {
		t.Fatalf("%v", got.Status)
	}
}

func TestAStagesJunitReportsAreStoredWithItsBuild(t *testing.T) {
	h := newHarness(t, map[string]string{"j": `
name: j
stages:
  t:
    run: 'mkdir -p r; printf "<testsuite name=\"g\"><testcase name=\"a\"/><testcase name=\"b\"><failure message=\"no\"/></testcase></testsuite>" > r/x.xml'
    junit: r/*.xml
`})
	b := h.wait(t, h.trigger(t, "j").ID)
	tests, _ := h.st.Tests(context.Background(), b.ID)
	if b.Status != store.Success || len(tests) != 2 || tests[1].Status != "failed" || tests[1].Message != "no" || tests[1].Stage != "t" {
		t.Fatalf("%v %+v", b.Status, tests)
	}
}

func TestASourceJobClonesAndReadsItsStages(t *testing.T) {
	cloned := ""
	h := newHarness(t, map[string]string{"src": "name: src\nsource: {repo: o/r, ref: main, path: ci/kipp.yml}\n"}, func(o *Options) {
		o.Clone = func(_ context.Context, dir, repo, ref string, _ io.Writer) error {
			cloned += repo + "@" + ref + " "
			if err := os.MkdirAll(filepath.Join(dir, "ci"), 0o750); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dir, "ci", "kipp.yml"), []byte("stages:\n  build: {run: 'echo built commit=$KIPP_COMMIT.'}\n  check: {needs: [build], run: 'true'}\n"), 0o600)
		}
	})
	b := h.wait(t, h.trigger(t, "src").ID)
	if b.Status != store.Success || h.log(t, b, "build") != "built commit=.\n" {
		t.Fatalf("%v %q", b.Status, h.log(t, b, "build"))
	}
	if st := h.stages(t, b.ID); !slices.Equal(st["check"].Needs, []string{"build"}) {
		t.Fatalf("the needs the repo's file gave were not recorded: %v", st["check"].Needs)
	}
	// A webhook build checks out the commit that was pushed.
	hook, err := h.d.Trigger(context.Background(), TriggerRequest{Job: "src", Kind: "webhook", Detail: "abc123"})
	if err != nil {
		t.Fatal(err)
	}
	if got := h.log(t, h.wait(t, hook.ID), "build"); got != "built commit=abc123.\n" {
		t.Fatalf("webhook build log %q", got)
	}
	if cloned != "o/r@main o/r@abc123 " {
		t.Fatalf("cloned %q", cloned)
	}
}

func TestAWebhookBuildGetsTheBranchItWasStartedForAndOthersGetItEmpty(t *testing.T) {
	h := newHarness(t, map[string]string{"j": "name: j\nconcurrency: parallel\nstages:\n  s: {run: 'echo \"[${KIPP_BRANCH-unset}] [${KIPP_COMMIT-unset}]\"'}\n"})
	hook, err := h.d.Trigger(context.Background(), TriggerRequest{Job: "j", Kind: "webhook", Detail: "abc123", Branch: "epic/APP-3-big"})
	if err != nil {
		t.Fatal(err)
	}
	if got := h.log(t, h.wait(t, hook.ID), "s"); got != "[epic/APP-3-big] [abc123]\n" {
		t.Fatalf("webhook build log %q", got)
	}
	if got := h.log(t, h.wait(t, h.trigger(t, "j").ID), "s"); got != "[] []\n" {
		t.Fatalf("api build log %q", got)
	}
}

func TestGitCloneChecksOutABranchOrACommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on this machine")
	}
	origin := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", origin, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	for _, v := range []string{"one", "two"} {
		if err := os.WriteFile(filepath.Join(origin, "f"), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
		git("add", "f")
		git("commit", "-q", "-m", v)
	}
	first := git("rev-parse", "HEAD~1")
	for ref, want := range map[string]string{"main": "two", first: "one"} {
		dir := t.TempDir()
		var out strings.Builder
		if err := gitClone(context.Background(), dir, origin, ref, &out); err != nil {
			t.Fatalf("%s: %v\n%s", ref, err, out.String())
		}
		got, _ := os.ReadFile(filepath.Join(dir, "f"))
		if string(got) != want {
			t.Fatalf("%s: f is %q, want %q", ref, got, want)
		}
	}
	if err := gitClone(context.Background(), t.TempDir(), origin, "--upload-pack=x", io.Discard); err == nil {
		t.Fatal("a ref that reads as an option was accepted")
	}
}

func TestATriggerForNoJobOrWithBadParamsIsRefusedByName(t *testing.T) {
	h := newHarness(t, map[string]string{"p": "name: p\nparams:\n  T: {type: choice, options: [a, b]}\nstages:\n  s: {run: 'true'}\n"})
	ctx := context.Background()
	if _, err := h.d.Trigger(ctx, TriggerRequest{Job: "zzz", Kind: "api"}); !errors.Is(err, ErrUnknownJob) {
		t.Fatal(err)
	}
	if _, err := h.d.Trigger(ctx, TriggerRequest{Job: "p", Kind: "api", Params: map[string]string{"T": "c"}}); !errors.Is(err, ErrBadParams) || !strings.Contains(err.Error(), "param T") {
		t.Fatalf("a bad choice: %v", err)
	}
	if _, err := h.d.Trigger(ctx, TriggerRequest{Job: "p", Kind: "api", Params: map[string]string{"NOPE": "1"}}); !errors.Is(err, ErrBadParams) {
		t.Fatalf("an unknown param: %v", err)
	}
	if _, err := h.d.Wait(ctx, 99); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("waiting on no build: %v", err)
	}
}

func TestAQueuedBuildWhoseJobIsGoneFails(t *testing.T) {
	block, open := gated(t)
	h := newHarness(t, map[string]string{"q": "name: q\nstages:\n  s: {run: '" + block + "'}\n"})
	first, second := h.trigger(t, "q"), h.trigger(t, "q")
	h.untilRunning(t, first.ID)
	h.d.SetSnapshot(&config.Snapshot{Jobs: map[string]*config.Job{}})
	if b := h.wait(t, second.ID); b.Status != store.Failed {
		t.Fatalf("%v", b.Status)
	}
	open()
	if b := h.wait(t, first.ID); b.Status != store.Success {
		t.Fatalf("a running build keeps the job it started with: %v", b.Status)
	}
}

func TestOnFailureRunsWhenTheSecretsCannotBeRead(t *testing.T) {
	h := newHarness(t, map[string]string{
		"s": "name: s\nsecrets: [TOKEN]\nstages:\n  s: {run: echo never}\non_failure: {run: 'echo hook status=$KIPP_STATUS'}\n",
	}, func(o *Options) {
		o.Secrets = func(context.Context, []string) (map[string]Secret, error) { return nil, errors.New("the key is gone") }
	})
	b := h.wait(t, h.trigger(t, "s").ID)
	if b.Status != store.Failed || h.log(t, b, "prepare") != "secrets: the key is gone\n" {
		t.Fatalf("%v", b.Status)
	}
	if got := h.log(t, b, "hooks"); got != "hook status=failed\n" {
		t.Fatalf("hooks %q", got)
	}
}

func TestRetryStopsAtSuccessOrAfterItsAttempts(t *testing.T) {
	calls := 0
	err := retry(4, time.Millisecond, func() error {
		if calls++; calls < 3 {
			return errors.New("busy")
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("err %v after %d calls", err, calls)
	}
	calls = 0
	gone := errors.New("still busy")
	if err := retry(4, time.Millisecond, func() error { calls++; return gone }); !errors.Is(err, gone) || calls != 4 {
		t.Fatalf("err %v after %d calls", err, calls)
	}
}

func TestSecretsStayOutOfTheStoredTests(t *testing.T) {
	h := newHarness(t, map[string]string{"j": `
name: j
secrets: [TOKEN]
stages:
  t:
    run: 'printf "<testsuite name=\"suite $TOKEN\"><testcase name=\"case $TOKEN\"><failure message=\"got $TOKEN back\"/></testcase></testsuite>" > x.xml'
    junit: x.xml
`})
	b := h.wait(t, h.trigger(t, "j").ID)
	tests, _ := h.st.Tests(context.Background(), b.ID)
	if len(tests) != 1 || tests[0].Suite != "suite ***" || tests[0].Name != "case ***" || tests[0].Message != "got *** back" {
		t.Fatalf("%+v", tests)
	}
}

func TestAFreedStageSlotStartsAQueuedBuildAtOnce(t *testing.T) {
	rescan = time.Hour // only a kick wakes the loop
	t.Cleanup(func() { rescan = time.Second })
	first, openFirst := gated(t)
	last, openLast := gated(t)
	h := newHarness(t, map[string]string{
		"wide":  "name: wide\nstages:\n  a: {run: '" + first + "'}\n  b: {run: '" + first + "'}\n  c: {needs: [a, b], run: '" + last + "'}\n",
		"other": "name: other\nstages:\n  s: {run: 'true'}\n",
	}, func(o *Options) { o.Executors = 2 })
	wide := h.trigger(t, "wide")
	eventually(t, "both stages hold an executor", func() bool {
		st := h.stages(t, wide.ID)
		return st["a"] != nil && st["a"].Status == store.StageRunning && st["b"].Status == store.StageRunning
	})
	other := h.trigger(t, "other")
	openFirst() // a and b end; c runs on the build's own slot, the other is free
	if b := h.wait(t, other.ID); b.Status != store.Success {
		t.Fatalf("%v", b.Status)
	}
	if b, _ := h.st.Build(context.Background(), wide.ID); b.Status != store.Running {
		t.Fatalf("the other build only ran once the wide one ended: %v", b.Status)
	}
	openLast()
	h.wait(t, wide.ID)
}

func TestACronFireTheStoreRefusesIsAWarningNotADrop(t *testing.T) {
	var logged strings.Builder
	h := newHarness(t, map[string]string{"c": "name: c\ntriggers: {cron: '* * * * *'}\nstages:\n  s: {run: 'true'}\n"}, func(o *Options) {
		o.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	h.stop()
	_ = h.st.Close()
	h.d.Tick(context.Background(), time.Now())
	if got := logged.String(); !strings.Contains(got, "level=WARN") || !strings.Contains(got, "cron trigger failed") || strings.Contains(got, "not queued") {
		t.Fatalf("logged %q", got)
	}
}

func TestAParamWinsOverEnvAndEnvOverTheServer(t *testing.T) {
	for _, name := range []string{"A", "B", "C"} {
		t.Setenv(name, "server")
	}
	h := newHarness(t, map[string]string{"e": `
name: e
env: {B: env, C: env}
params:
  C: {default: param}
stages:
  s: {run: 'echo a=$A b=$B c=$C'}
`}, func(o *Options) { o.PassEnv = []string{"*"} })
	b := h.wait(t, h.trigger(t, "e").ID)
	if got := h.log(t, b, "s"); got != "a=server b=env c=param\n" {
		t.Fatalf("log %q", got)
	}
}

func TestRetentionRunsOncePerFinishedBuild(t *testing.T) {
	var calls []string
	h := newHarness(t, map[string]string{
		"ok":  "name: ok\nstages:\n  s: {run: 'true'}\n",
		"bad": "name: bad\nstages:\n  s: {run: 'false'}\n",
	}, func(o *Options) {
		o.Retain = func(_ context.Context, _ *store.Store, job *config.Job, logsDir string, _ time.Time) error {
			calls = append(calls, job.Name+" "+logsDir)
			return nil
		}
	})
	h.wait(t, h.trigger(t, "ok").ID)
	h.wait(t, h.trigger(t, "bad").ID)
	logs := filepath.Join(h.dir, "logs")
	if want := []string{"ok " + logs, "bad " + logs}; !slices.Equal(calls, want) {
		t.Fatalf("retention calls %q, want %q", calls, want)
	}
}

// lockedBuffer is a log several goroutines write and the test reads.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// stuckWorkspace leaves a workspace under dir that cannot be removed.
func stuckWorkspace(t *testing.T, dir string) {
	t.Helper()
	if os.Getuid() == 0 {
		t.Skip("root removes what this test locks")
	}
	left := filepath.Join(dir, "workspace", "r", "1")
	if err := os.MkdirAll(left, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(left, "f"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(left, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(left, 0o700) })
}

func TestRecoverWarnsOfAWorkspaceItCannotRemoveAndReturnsOnlyTheStoresError(t *testing.T) {
	dir := t.TempDir()
	stuckWorkspace(t, dir)
	st, err := store.Open(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	var logged strings.Builder
	d := New(st, runner.Local{}, Options{DataDir: dir, Logger: slog.New(slog.NewTextHandler(&logged, nil))})
	if err := d.Recover(context.Background()); err != nil {
		t.Fatalf("a folder that would not go kept the server down: %v", err)
	}
	if got := logged.String(); !strings.Contains(got, "level=WARN") || !strings.Contains(got, filepath.Join("workspace", "r", "1")) {
		t.Fatalf("logged %q", got)
	}
	_ = st.Close()
	err = d.Recover(context.Background())
	var leftover *fs.PathError
	if err == nil || errors.As(err, &leftover) {
		t.Fatalf("with the store closed too: %v", err)
	}
}

func TestAFinalStatusTheStoreRefusesIsWrittenAgain(t *testing.T) {
	block, open := gated(t)
	logged := &lockedBuffer{}
	statusBackoff = time.Millisecond
	t.Cleanup(func() { statusBackoff = 200 * time.Millisecond })
	h := newHarness(t, map[string]string{"f": "name: f\nstages:\n  s: {run: '" + block + "'}\n"}, func(o *Options) {
		o.Logger = slog.New(slog.NewTextHandler(logged, nil))
	})
	b := h.trigger(t, "f")
	h.untilRunning(t, b.ID)
	_ = h.st.Close()
	open()
	eventually(t, "the dispatcher gives the row up", func() bool { return strings.Contains(logged.String(), "its row is stale") })
	if got := strings.Count(logged.String(), "writing the build's final status"); got != statusAttempts {
		t.Fatalf("the final status was written %d times, want %d", got, statusAttempts)
	}
}

func TestATextParamReachesTheStageWithEveryLineItHad(t *testing.T) {
	seen := filepath.Join(t.TempDir(), "note")
	h := newHarness(t, map[string]string{
		"t": "name: t\nparams:\n  NOTE: {type: text}\n  WHO: {default: world}\nstages:\n  s: {run: 'printf %s \"$NOTE\" > " + seen + "'}\n",
	})
	ctx := context.Background()
	const note = "first line\n  second, indented\n\nlast, with a tab\tand no newline at its end"
	b, err := h.d.Trigger(ctx, TriggerRequest{Job: "t", Kind: "api", Params: map[string]string{"NOTE": note}})
	if err != nil {
		t.Fatal(err)
	}
	if got := h.wait(t, b.ID); got.Status != store.Success || got.Params["NOTE"] != note {
		t.Fatalf("status %s, stored param %q", got.Status, got.Params["NOTE"])
	}
	got, err := os.ReadFile(seen)
	if err != nil || string(got) != note {
		t.Fatalf("what the stage saw: %q %v", got, err)
	}
	if _, err := h.d.Trigger(ctx, TriggerRequest{Job: "t", Kind: "api", Params: map[string]string{"WHO": "two\nlines"}}); !errors.Is(err, ErrBadParams) {
		t.Fatalf("a string param of two lines: %v", err)
	}
}

// The masking writer works on bytes as they come; a value of several
// lines beside a secret changes neither.
func TestASecretIsMaskedBesideATextParamThatArrivesWhole(t *testing.T) {
	h := newHarness(t, map[string]string{
		"t": "name: t\nparams:\n  NOTE: {type: text}\nsecrets: [TOKEN]\nstages:\n  s: {run: 'printf \"%s\" \"$NOTE\"; echo; echo token=$TOKEN; printf \"%s\" \"$NOTE\"'}\n",
	})
	const note = "line one\n  value-of line two\n\nlast line, no newline"
	b, err := h.d.Trigger(context.Background(), TriggerRequest{Job: "t", Kind: "api", Params: map[string]string{"NOTE": note}})
	if err != nil {
		t.Fatal(err)
	}
	if got := h.wait(t, b.ID); got.Status != store.Success {
		t.Fatalf("status %s", got.Status)
	}
	if got, want := h.log(t, b, "s"), note+"\ntoken=***\n"+note; got != want {
		t.Fatalf("log %q\nwant %q", got, want)
	}
}

func TestTheLineThatSaysWhyAStageStoppedStartsALineAndIsMaskedLikeAnyOther(t *testing.T) {
	started := filepath.Join(t.TempDir(), "started")
	h := newHarness(t, map[string]string{"p": "name: p\nsecrets: [TOKEN]\nstages:\n  s: {run: 'printf half-a-line; touch " + started + "; sleep 60'}\n"})
	b := h.trigger(t, "p")
	eventually(t, "the stage printed", func() bool {
		_, err := os.Stat(started)
		return err == nil
	})
	// Whoever cancels is named in the log, and a name is text like any other.
	if err := h.d.Cancel(context.Background(), b.ID, "value-of-TOKEN"); err != nil {
		t.Fatal(err)
	}
	h.wait(t, b.ID)
	if got, want := h.log(t, b, "s"), "half-a-line\nkipp: cancelled by ***\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// lostStage records a build and a stage of it as a server that then died
// would have left them: running, with the group of the real process p.
func lostStage(t *testing.T, st *store.Store, job string, g runner.Group) *store.Build {
	t.Helper()
	ctx := context.Background()
	b := &store.Build{Job: job, Status: store.Running, Trigger: "cli"}
	for _, err := range []error{
		st.CreateBuild(ctx, b),
		st.PutStages(ctx, b.ID, []string{"s"}, nil),
		st.SetStage(ctx, b.ID, "s", store.StageRunning, nil, time.Now()),
		st.SetStageGroup(ctx, b.ID, "s", g.Pgid, g.Born),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	return b
}

func TestRecoverKillsWhatALostStageLeftRunningAndNothingItCannotProveIsTheStages(t *testing.T) {
	dir := t.TempDir()
	st := reopen(t, dir)
	// Two stages a dead server started: nobody is left to wait on them.
	orphan, err := runner.Local{}.Start(context.Background(), runner.Spec{Dir: dir, Command: "sleep 300 & wait", Output: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer orphan.Kill(100 * time.Millisecond)
	stranger, err := runner.Local{}.Start(context.Background(), runner.Spec{Dir: dir, Command: "sleep 300", Output: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer stranger.Kill(100 * time.Millisecond)
	if orphan.Group().Born == "" {
		t.Skip("this system does not say when a process was born")
	}
	ours := lostStage(t, st, "ours", orphan.Group())
	// The id is in use, by a process that is not the one the stage recorded.
	theirs := lostStage(t, st, "theirs", runner.Group{Pgid: stranger.Group().Pgid, Born: orphan.Group().Born})
	before := "half a line"
	if err := os.MkdirAll(filepath.Join(dir, "logs", "ours", "1"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "logs", "ours", "1", "s.log"), []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}

	var logged strings.Builder
	d := New(st, runner.Local{}, Options{DataDir: dir, Logger: slog.New(slog.NewTextHandler(&logged, nil))})
	if err := d.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Strictly, with no waiting: Recover returns once the group is gone, so
	// that the job's next build cannot start beside it.
	if err := syscall.Kill(-orphan.Group().Pgid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("what the lost stage left is still running: %v", err)
	}
	if err := syscall.Kill(stranger.Group().Pgid, 0); err != nil {
		t.Fatalf("a process that was not the stage's was killed: %v", err)
	}
	if got := logged.String(); !strings.Contains(got, "level=WARN") || !strings.Contains(got, "pgid="+strconv.Itoa(stranger.Group().Pgid)) || strings.Contains(got, "pgid="+strconv.Itoa(orphan.Group().Pgid)) {
		t.Fatalf("logged %q", got)
	}
	read := func(b *store.Build) string {
		data, err := os.ReadFile(d.LogPath(b.Job, b.Number, "s"))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	if got, want := read(ours), before+"\nkipp: the server stopped while this ran; what it had left running was killed when the server started again\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got, want := read(theirs), "kipp: the server stopped while this ran\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestADisabledJobIgnoresItsTriggersAndRefusesOneMadeByHand(t *testing.T) {
	logged := &lockedBuffer{}
	h := newHarness(t, map[string]string{
		"up":  "name: up\nstages:\n  s: {run: 'true'}\n",
		"off": "name: off\ndisabled: true\ntriggers: {cron: '* * * * *', after: [up]}\nparams:\n  P: {type: bool}\nstages:\n  s: {run: 'true'}\n",
	}, func(o *Options) { o.Logger = slog.New(slog.NewTextHandler(logged, nil)) })
	ctx := context.Background()
	// By hand it is refused as disabled, whatever else is wrong with the request.
	for _, params := range []map[string]string{nil, {"P": "neither"}} {
		if _, err := h.d.Trigger(ctx, TriggerRequest{Job: "off", Kind: "api", Params: params}); !errors.Is(err, ErrDisabled) {
			t.Fatalf("params %v: %v", params, err)
		}
	}
	h.d.Tick(ctx, time.Now())
	if b := h.wait(t, h.trigger(t, "up").ID); b.Status != store.Success {
		t.Fatalf("%v", b.Status)
	}
	if builds, _ := h.st.Builds(ctx, "off", 5); len(builds) != 0 {
		t.Fatalf("a disabled job was built: %+v", builds)
	}
	if act, _ := h.st.Activity(ctx, "off", 5); len(act) != 0 {
		t.Fatalf("an ignored trigger left a line: %+v", act)
	}
	if strings.Contains(logged.String(), "off") {
		t.Fatalf("an ignored trigger was logged: %s", logged)
	}
}

func TestABuildQueuedWhenItsJobIsDisabledFailsAndSaysWhy(t *testing.T) {
	block, open := gated(t)
	const job = "name: q\nstages:\n  s: {run: '%s'}\n"
	h := newHarness(t, map[string]string{"q": fmt.Sprintf(job, block)})
	first, second := h.trigger(t, "q"), h.trigger(t, "q")
	h.untilRunning(t, first.ID)
	snap, err := config.Load(writeJobs(t, h.dir, map[string]string{"q": "disabled: true\n" + fmt.Sprintf(job, block)}))
	if err != nil {
		t.Fatal(err)
	}
	h.d.SetSnapshot(snap)
	if b := h.wait(t, second.ID); b.Status != store.Failed || b.StartedAt != nil {
		t.Fatalf("%+v", b)
	}
	act, _ := h.st.Activity(context.Background(), "q", 5)
	if len(act) != 1 || act[0].Line != "#2 failed before it started: the job is disabled" {
		t.Fatalf("%+v", act)
	}
	open()
	if b := h.wait(t, first.ID); b.Status != store.Success {
		t.Fatalf("a running build keeps the job it started with: %v", b.Status)
	}
}

// A database restored from a backup gives out numbers it gave before,
// while the logs of the builds that had them are still on disk.
func TestABuildStartsWithNoneOfTheLogsAnEarlierBuildOfItsNumberLeft(t *testing.T) {
	h := newHarness(t, map[string]string{"n": "name: n\nstages:\n  s: {run: echo new}\n"})
	old := filepath.Join(h.d.LogsDir(), "n", "1")
	if err := os.MkdirAll(old, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"s.log", "gone-since.log", "hooks.log"} {
		if err := os.WriteFile(filepath.Join(old, name), []byte("old\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	b := h.wait(t, h.trigger(t, "n").ID)
	if b.Number != 1 || b.Status != store.Success {
		t.Fatalf("%+v", b)
	}
	if got := h.log(t, b, "s"); got != "new\n" {
		t.Fatalf("the stage's log: %q", got)
	}
	left, err := os.ReadDir(old)
	if err != nil || len(left) != 2 || left[0].Name() != "s.log" || left[1].Name() != "s.log"+logtime.Suffix {
		t.Fatalf("the build's log directory holds %v, %v", left, err)
	}
}

// Every line of a log has the time it began beside it, and the log is
// what the stage printed, untouched.
func TestEveryLineOfALogHasItsTimeBesideIt(t *testing.T) {
	h := newHarness(t, map[string]string{"n": "name: n\nstages:\n  s: {run: 'echo one; echo two; printf three'}\n"})
	before := time.Now().Add(-time.Second)
	b := h.wait(t, h.trigger(t, "n").ID)
	if got := h.log(t, b, "s"); got != "one\ntwo\nthree" {
		t.Fatalf("the stage's log: %q", got)
	}
	path := h.d.LogPath(b.Job, b.Number, "s") + logtime.Suffix
	r := logtime.NewReader(func() (*os.File, error) { return OpenLog(path, os.O_RDONLY) })
	defer r.Close()
	for _, end := range []int64{4, 8, 13} {
		got, ok := r.At(end)
		if !ok || got.Before(before.Truncate(time.Millisecond)) || got.After(time.Now()) {
			t.Fatalf("the line ending at %d began at %v, %v", end, got, ok)
		}
	}
}

func TestTheStagesOfASourceJobRunInItsWorkdirInsideTheClone(t *testing.T) {
	fail := false
	h := newHarness(t, map[string]string{"src": "name: src\nsource: {repo: o/r, path: ci/kipp.yml}\nworkdir: ci\non_failure: {run: 'echo hook in $PWD'}\n"}, func(o *Options) {
		o.Clone = func(_ context.Context, dir, _, _ string, _ io.Writer) error {
			if fail {
				return errors.New("no such repo")
			}
			if err := os.MkdirAll(filepath.Join(dir, "ci"), 0o750); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dir, "ci", "kipp.yml"), []byte("stages:\n  where: {run: 'echo in $PWD; ls'}\n"), 0o600)
		}
	})
	b := h.wait(t, h.trigger(t, "src").ID)
	inClone := filepath.Join("workspace", "src", "1", "ci")
	if got := h.log(t, b, "where"); b.Status != store.Success || !strings.Contains(got, inClone+"\nkipp.yml\n") {
		t.Fatalf("%v %q, want it in %s", b.Status, got, inClone)
	}
	// A clone that fails leaves no such directory: the hook runs in the workspace.
	fail = true
	b = h.wait(t, h.trigger(t, "src").ID)
	if got := h.log(t, b, "hooks"); b.Status != store.Failed || !strings.Contains(got, "hook in ") || !strings.HasSuffix(got, filepath.Join("workspace", "src", "2")+"\n") {
		t.Fatalf("%v %q", b.Status, got)
	}
}

func TestACancelledCloneTakesTheHelpersGitStartedWithIt(t *testing.T) {
	bin, pidFile := t.TempDir(), filepath.Join(t.TempDir(), "pid")
	// A git that, like the real one, leaves the fetching to a child.
	script := "#!/bin/sh\nsleep 300 &\necho $! > " + pidFile + ".tmp\nmv " + pidFile + ".tmp " + pidFile + "\nwait\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- gitClone(ctx, t.TempDir(), "o/r", "main", io.Discard) }()
	helper := 0
	eventually(t, "git started its helper", func() bool {
		data, _ := os.ReadFile(pidFile)
		helper, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		return helper > 0
	})
	defer func() { _ = syscall.Kill(helper, syscall.SIGKILL) }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled clone reported success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a cancelled clone did not return")
	}
	assertGone(t, []int{helper})
}

func TestRecoverWarnsOfALostStageWhoseProcessGroupWasNeverRecorded(t *testing.T) {
	dir := t.TempDir()
	st := reopen(t, dir)
	b := lostStage(t, st, "unrecorded", runner.Group{})
	var logged strings.Builder
	d := New(st, runner.Local{}, Options{DataDir: dir, Logger: slog.New(slog.NewTextHandler(&logged, nil))})
	if err := d.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := logged.String()
	for _, want := range []string{"level=WARN", "no process group was recorded", "job=unrecorded", "number=" + strconv.Itoa(b.Number), "stage=s"} {
		if !strings.Contains(got, want) {
			t.Fatalf("logged %q, want %q in it", got, want)
		}
	}
}

func TestABuildWhoseHookRunsHoldsNoExecutorAndStillHoldsItsLock(t *testing.T) {
	block, open := gated(t)
	h := newHarness(t, map[string]string{
		"k":     "name: k\nlocks: [shared]\nstages:\n  s: {run: 'true'}\non_success: {run: '" + block + "; ls $KIPP_WORKSPACE > /dev/null && echo workspace still here'}\n",
		"other": "name: other\nstages:\n  s: {run: 'true'}\n",
		"same":  "name: same\nlocks: [shared]\nstages:\n  s: {run: 'true'}\n",
	}, func(o *Options) { o.Executors = 1 })
	ctx := context.Background()
	hooked := h.trigger(t, "k")
	eventually(t, "the hook is running", func() bool {
		got, err := h.st.Build(ctx, hooked.ID)
		return err == nil && got.Status == store.Success && h.d.Busy(hooked.ID)
	})
	// The one executor is free again: a build of another job runs to its end.
	if got := h.wait(t, h.trigger(t, "other").ID); got.Status != store.Success {
		t.Fatalf("a build beside a running hook ended %v", got.Status)
	}
	// What the hooked build holds besides is its until the hook is done.
	waiting := h.trigger(t, "same")
	time.Sleep(200 * time.Millisecond)
	if got, _ := h.st.Build(ctx, waiting.ID); got.Status != store.Queued {
		t.Fatalf("a build wanting the lock of a build whose hook runs is %v, want queued", got.Status)
	}
	open()
	if got := h.wait(t, hooked.ID); h.log(t, got, "hooks") != "workspace still here\n" {
		t.Fatalf("the hook's log: %q", h.log(t, got, "hooks"))
	}
	if got := h.wait(t, waiting.ID); got.Status != store.Success {
		t.Fatalf("the build that waited for the lock ended %v", got.Status)
	}
}

// reopen opens the store a server left in dir, as the next server would.
func reopen(t *testing.T, dir string) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	return st
}

// lockedLog is a server's log that several goroutines write.
type lockedLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
