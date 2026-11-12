package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/runner"
	"github.com/ndthuan/kipp/internal/store"
)

// restart is what a safe restart does to h: the stop of the old process,
// then a new dispatcher on the same data, which queues the stopped builds
// again before it runs anything. It returns the new harness and the
// builds Rerun queued.
func (h *harness) restart(t *testing.T) (*harness, []*store.Build) {
	t.Helper()
	ctx := context.Background()
	h.d.StopForRestart(ctx)
	h.stop()
	d := New(h.st, runner.Local{}, h.d.opts)
	d.SetSnapshot(h.d.Snapshot())
	if err := d.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	again := d.Rerun(ctx)
	runCtx, cancel := context.WithCancel(ctx)
	ended := make(chan struct{})
	go func() { d.Run(runCtx); close(ended) }()
	stop := func() { cancel(); <-ended }
	t.Cleanup(stop) // before the first harness's, which closes the store
	return &harness{d: d, st: h.st, dir: h.dir, stop: stop}, again
}

func rerunJob(name, run string) string {
	return "name: " + name + "\nrerun_on_restart: true\nparams:\n  WHO: {default: nobody}\nstages:\n  s: {run: '" + run + "'}\n"
}

func TestAQuietWaitIsIdleWhileOnlyABuildToRunAgainRuns(t *testing.T) {
	block, _ := gated(t)
	h := newHarness(t, map[string]string{"flow": rerunJob("flow", block)})
	ctx := context.Background()
	b := h.trigger(t, "flow")
	h.untilRunning(t, b.ID)
	st, err := h.d.QuietState(ctx)
	if err != nil || st.Running != 1 || st.Rerun != 0 {
		t.Fatalf("before quiet %+v %v: every running build is running", st, err)
	}
	st, err = h.d.SetQuiet(ctx, true)
	if err != nil || st.Running != 0 || st.Queued != 0 || st.Rerun != 1 {
		t.Fatalf("quiet %+v %v, want 0 running, 0 queued, 1 to run again", st, err)
	}
}

func TestARestartStopsABuildToRunAgainAndTheNextStartQueuesItWithItsParamsAndCause(t *testing.T) {
	block, open := gated(t)
	h := newHarness(t, map[string]string{"flow": rerunJob("flow", block)})
	ctx := context.Background()
	first, err := h.d.Trigger(ctx, TriggerRequest{Job: "flow", Kind: "webhook", Detail: "abc123", Branch: "main", By: "github", Params: map[string]string{"WHO": "bob"}})
	if err != nil {
		t.Fatal(err)
	}
	// The stage itself, not only the build: a build is running from its
	// prepare on, and one stopped before its stage starts has no stage log.
	h.untilStage(t, first.ID, "s", store.StageRunning)
	if _, err := h.d.SetQuiet(ctx, true); err != nil {
		t.Fatal(err)
	}
	h2, again := h.restart(t)

	old, err := h.st.Build(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if old.Status != store.Cancelled {
		t.Fatalf("the stopped build ended %s, want cancelled", old.Status)
	}
	if log := h.log(t, old, "s"); !strings.Contains(log, "kipp: stopped for a restart, runs again after it") {
		t.Fatalf("the stage's log does not say why it ends:\n%s", log)
	}
	if len(again) != 1 {
		t.Fatalf("queued again: %+v", again)
	}
	n := again[0]
	if n.Job != "flow" || n.Trigger != "webhook" || n.TriggerDetail != "abc123" || n.TriggerBranch != "main" || n.StartedBy != "github" || n.Params["WHO"] != "bob" || n.RerunOf != old.Number {
		t.Fatalf("the build queued again %+v does not repeat #%d", n, old.Number)
	}
	if old.RerunAs != n.Number {
		t.Fatalf("the stopped build links to #%d, want #%d", old.RerunAs, n.Number)
	}
	open()
	if b := h2.wait(t, n.ID); b.Status != store.Success {
		t.Fatalf("the build run again ended %s", b.Status)
	}
	if pending, _ := h.st.PendingReruns(ctx); len(pending) != 0 {
		t.Fatalf("still waiting to run again: %+v", pending)
	}
	all := activity(t, h.st, "flow")
	for _, want := range []string{fmt.Sprintf("#%d stopped for a restart, runs again after it", old.Number), fmt.Sprintf("#%d runs #%d again, stopped for the restart", n.Number, old.Number)} {
		if !strings.Contains(all, want) {
			t.Errorf("the activity does not say %q:\n%s", want, all)
		}
	}
}

func TestAnAfterBuildRunAgainAfterARestartKeepsItsUpstream(t *testing.T) {
	block, open := gated(t)
	h := newHarness(t, map[string]string{"down": rerunJob("down", block+"; echo $KIPP_UPSTREAM_BUILD $KIPP_UPSTREAM_STATUS $KIPP_UPSTREAM_COMMIT")})
	ctx := context.Background()
	first, err := h.d.Trigger(ctx, TriggerRequest{Job: "down", Kind: "after", Detail: "acme.site.main#4", UpstreamStatus: store.Unstable, UpstreamCommit: "abc123", By: "kipp"})
	if err != nil {
		t.Fatal(err)
	}
	h.untilRunning(t, first.ID)
	if _, err := h.d.SetQuiet(ctx, true); err != nil {
		t.Fatal(err)
	}
	h2, again := h.restart(t)
	if len(again) != 1 {
		t.Fatalf("queued again: %+v", again)
	}
	open()
	b := h2.wait(t, again[0].ID)
	if got := h2.log(t, b, "s"); b.UpstreamCommit != "abc123" || !strings.HasSuffix(got, "acme.site.main#4 unstable abc123\n") {
		t.Fatalf("the build run again has upstream commit %q and says %q", b.UpstreamCommit, got)
	}
}

// activity is the job's newest activity lines, one a line.
func activity(t *testing.T, st *store.Store, job string) string {
	t.Helper()
	acts, err := st.Activity(context.Background(), job, 10)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, a := range acts {
		lines = append(lines, a.Line)
	}
	return strings.Join(lines, "\n")
}

func TestQuietTurnedOffWithoutARestartStopsNothing(t *testing.T) {
	block, open := gated(t)
	h := newHarness(t, map[string]string{"flow": rerunJob("flow", block), "deploy": "name: deploy\nstages:\n  s: {run: '" + block + "'}\n"})
	ctx := context.Background()
	flow, deploy := h.trigger(t, "flow"), h.trigger(t, "deploy")
	h.untilRunning(t, flow.ID)
	h.untilRunning(t, deploy.ID)
	if _, err := h.d.SetQuiet(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.ChooseRerun(deploy.ID); err != nil {
		t.Fatal(err)
	}
	st, err := h.d.SetQuiet(ctx, false)
	if err != nil || st.Running != 2 || st.Rerun != 0 {
		t.Fatalf("quiet off %+v %v: both run, and none is to run again", st, err)
	}
	// What a stop does without quiet: nothing is stopped to run again,
	// and the admin's choice went with the quiet.
	if stopped := h.d.StopForRestart(ctx); len(stopped) != 0 {
		t.Fatalf("stopped without quiet: %+v", stopped)
	}
	if _, err := h.d.SetQuiet(ctx, true); err != nil {
		t.Fatal(err)
	}
	if st, _ := h.d.QuietState(ctx); st.Running != 1 || st.Rerun != 1 {
		t.Fatalf("a new quiet %+v kept the choice of the last one", st)
	}
	if _, err := h.d.SetQuiet(ctx, false); err != nil {
		t.Fatal(err)
	}
	open()
	for _, id := range []int64{flow.ID, deploy.ID} {
		if b := h.wait(t, id); b.Status != store.Success {
			t.Fatalf("#%d ended %s", b.Number, b.Status)
		}
	}
}

func TestABuildOfAJobWithoutTheKeyIsWaitedForAndNeverStopped(t *testing.T) {
	block, open := gated(t)
	h := newHarness(t, map[string]string{"deploy": "name: deploy\nstages:\n  s: {run: '" + block + "'}\n", "flow": rerunJob("flow", block)})
	ctx := context.Background()
	deploy, flow := h.trigger(t, "deploy"), h.trigger(t, "flow")
	h.untilRunning(t, deploy.ID)
	h.untilRunning(t, flow.ID)
	st, err := h.d.SetQuiet(ctx, true)
	if err != nil || st.Running != 1 || st.Rerun != 1 {
		t.Fatalf("quiet %+v %v: the deploy is still waited for", st, err)
	}
	stopped := h.d.StopForRestart(ctx)
	if len(stopped) != 1 || stopped[0].ID != flow.ID {
		t.Fatalf("stopped %+v, want the flow alone", stopped)
	}
	if b, _ := h.st.Build(ctx, deploy.ID); b.Status != store.Running {
		t.Fatalf("the deploy is %s: a build without the key is never stopped", b.Status)
	}
	open()
	if b := h.wait(t, deploy.ID); b.Status != store.Success {
		t.Fatalf("the deploy ended %s", b.Status)
	}
}

func TestChooseRerunStopsTheChosenBuildAloneAndOnlyWhileQuiet(t *testing.T) {
	block, open := gated(t)
	h := newHarness(t, map[string]string{"a": "name: a\nstages:\n  s: {run: '" + block + "'}\n", "b": "name: b\nstages:\n  s: {run: '" + block + "'}\n"})
	ctx := context.Background()
	a, b := h.trigger(t, "a"), h.trigger(t, "b")
	h.untilRunning(t, a.ID)
	h.untilRunning(t, b.ID)
	if _, err := h.d.ChooseRerun(a.ID); !errors.Is(err, ErrNotQuiet) {
		t.Fatalf("chosen without quiet: %v", err)
	}
	if _, err := h.d.SetQuiet(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.ChooseRerun(999); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("a build that does not run: %v", err)
	}
	ref, err := h.d.ChooseRerun(a.ID)
	if err != nil || ref.Job != "a" || ref.Number != a.Number {
		t.Fatalf("%+v %v", ref, err)
	}
	h2, again := h.restart(t)
	if len(again) != 1 || again[0].Job != "a" || again[0].RerunOf != a.Number {
		t.Fatalf("queued again %+v, want a alone", again)
	}
	if got, _ := h.st.Build(ctx, b.ID); got.Status != store.Lost {
		t.Fatalf("b, not chosen, is %s: the restart loses it as before", got.Status)
	}
	open()
	if got := h2.wait(t, again[0].ID); got.Status != store.Success {
		t.Fatalf("a run again ended %s", got.Status)
	}
}

func TestTheBuildsRunAgainAreQueuedInTheOrderTheyWereFirstQueued(t *testing.T) {
	block, open := gated(t)
	h := newHarness(t, map[string]string{"x": rerunJob("x", block), "y": rerunJob("y", block), "z": rerunJob("z", block)})
	ctx := context.Background()
	var first []*store.Build
	for _, job := range []string{"y", "z", "x"} {
		b := h.trigger(t, job)
		h.untilRunning(t, b.ID)
		first = append(first, b)
	}
	if _, err := h.d.SetQuiet(ctx, true); err != nil {
		t.Fatal(err)
	}
	h2, again := h.restart(t)
	if len(again) != 3 {
		t.Fatalf("queued again %+v", again)
	}
	for i, b := range again {
		if b.Job != first[i].Job || b.ID <= first[len(first)-1].ID || (i > 0 && b.ID <= again[i-1].ID) {
			t.Fatalf("queued again in the order %s, %s, %s; want y, z, x", again[0].Job, again[1].Job, again[2].Job)
		}
	}
	open()
	for _, b := range again {
		h2.wait(t, b.ID)
	}
}

func TestABuildToRunAgainWhoseJobIsGoneIsForgottenAndSaidOnItsActivity(t *testing.T) {
	block, _ := gated(t)
	h := newHarness(t, map[string]string{"flow": rerunJob("flow", block)})
	ctx := context.Background()
	b := h.trigger(t, "flow")
	h.untilRunning(t, b.ID)
	if _, err := h.d.SetQuiet(ctx, true); err != nil {
		t.Fatal(err)
	}
	h.d.StopForRestart(ctx)
	h.stop()
	d := New(h.st, runner.Local{}, h.d.opts)
	d.SetSnapshot(&config.Snapshot{Jobs: map[string]*config.Job{}})
	if again := d.Rerun(ctx); len(again) != 0 {
		t.Fatalf("queued again for a job that is gone: %+v", again)
	}
	if pending, _ := h.st.PendingReruns(ctx); len(pending) != 0 {
		t.Fatalf("still waiting: %+v", pending)
	}
	acts, _ := h.st.Activity(ctx, "flow", 5)
	if len(acts) == 0 || !strings.Contains(acts[0].Line, "was not run again") {
		t.Fatalf("the activity reads %+v", acts)
	}
}

// The deadlock a safe restart met: a job polls for the build of a pull
// request, whose webhook arrives while quiet and so is held until quiet
// ends; quiet waits for the job, which waits for what quiet holds.
func TestASafeRestartCompletesWhileAJobWaitsForABuildQuietHoldsBack(t *testing.T) {
	built := filepath.Join(t.TempDir(), "pr-built")
	poll := fmt.Sprintf("while [ ! -e %s ]; do sleep 0.02; done", built)
	files := map[string]string{
		"pr": "name: pr\nstages:\n  s: {run: 'touch " + built + "'}\n",
	}
	ctx := context.Background()

	// Without the key, quiet never gets idle, and after ten minutes it
	// names the build that holds it.
	files["flow"] = "name: flow\nstages:\n  s: {run: '" + poll + "'}\n"
	h := newHarness(t, files)
	flow := h.trigger(t, "flow")
	h.untilRunning(t, flow.ID)
	st, err := h.d.SetQuiet(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.Trigger(ctx, TriggerRequest{Job: "pr", Kind: "webhook", By: "github"}); !errors.Is(err, ErrQuiet) {
		t.Fatalf("the pull request's build: %v, want held back by quiet", err)
	}
	h.d.now = func() time.Time { return st.Since.Add(HoldingAfter) }
	st, err = h.d.QuietState(ctx)
	if err != nil || st.Running != 1 || len(st.Holding) != 1 || st.Holding[0].ID != flow.ID {
		t.Fatalf("without the key %+v %v: the flow holds quiet, and is named", st, err)
	}
	if _, err := h.d.SetQuiet(ctx, false); err != nil {
		t.Fatal(err)
	}
	if b, _ := h.d.Trigger(ctx, TriggerRequest{Job: "pr", Kind: "webhook", By: "github"}); b == nil || h.wait(t, b.ID).Status != store.Success || h.wait(t, flow.ID).Status != store.Success {
		t.Fatal("the flow did not end once quiet was off")
	}
	if err := os.Remove(built); err != nil {
		t.Fatal(err)
	}

	// With it, quiet is idle at once, the restart stops the flow, and
	// after the start the flow runs again and sees the build.
	files["flow"] = rerunJob("flow", poll)
	h = newHarness(t, files)
	flow = h.trigger(t, "flow")
	h.untilRunning(t, flow.ID)
	if st, err := h.d.SetQuiet(ctx, true); err != nil || st.Running+st.Queued != 0 {
		t.Fatalf("with the key %+v %v: quiet is idle", st, err)
	}
	if _, err := h.d.Trigger(ctx, TriggerRequest{Job: "pr", Kind: "webhook", By: "github"}); !errors.Is(err, ErrQuiet) {
		t.Fatalf("the pull request's build: %v", err)
	}
	h2, again := h.restart(t)
	if len(again) != 1 {
		t.Fatalf("queued again %+v", again)
	}
	// The held webhook comes after the builds run again.
	pr, err := h2.d.Trigger(ctx, TriggerRequest{Job: "pr", Kind: "webhook", By: "github"})
	if err != nil {
		t.Fatal(err)
	}
	if h2.wait(t, pr.ID).Status != store.Success || h2.wait(t, again[0].ID).Status != store.Success {
		t.Fatal("the flow run again did not end with the pull request's build")
	}
}

func TestTheHintNamesTheBuildsHoldingQuietFromTenMinutesOnAndNotBefore(t *testing.T) {
	block, open := gated(t)
	defer open()
	h := newHarness(t, map[string]string{"flow": "name: flow\nstages:\n  s: {run: '" + block + "'}\n"})
	ctx := context.Background()
	b := h.trigger(t, "flow")
	h.untilRunning(t, b.ID)
	st, err := h.d.SetQuiet(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	since := st.Since
	h.d.now = func() time.Time { return since.Add(HoldingAfter - time.Nanosecond) }
	if st, _ := h.d.QuietState(ctx); len(st.Holding) != 0 {
		t.Fatalf("a hint before ten minutes: %+v", st.Holding)
	}
	h.d.now = func() time.Time { return since.Add(HoldingAfter) }
	st, _ = h.d.QuietState(ctx)
	if len(st.Holding) != 1 || st.Holding[0] != (BuildRef{ID: b.ID, Job: "flow", Number: b.Number}) {
		t.Fatalf("at ten minutes the hint is %+v", st.Holding)
	}
	if HoldingAfter != 10*time.Minute {
		t.Fatalf("the hint waits %s, want 10m", HoldingAfter)
	}
}

func TestTheHintStaysSilentWhileABuildIsQueuedOrStartedDuringQuiet(t *testing.T) {
	block, open := gated(t)
	defer open()
	h := newHarness(t, map[string]string{"flow": "name: flow\nstages:\n  s: {run: '" + block + "'}\n"})
	ctx := context.Background()
	first := h.trigger(t, "flow")
	h.untilRunning(t, first.ID)
	second := h.trigger(t, "flow") // waits for the first
	st, err := h.d.SetQuiet(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	since := st.Since
	h.d.now = func() time.Time { return since.Add(time.Hour) }
	if st, _ := h.d.QuietState(ctx); len(st.Holding) != 0 || st.Queued != 1 {
		t.Fatalf("a hint while a build is queued: %+v", st)
	}
	if err := h.d.Cancel(ctx, first.ID, "test"); err != nil {
		t.Fatal(err)
	}
	h.untilRunning(t, second.ID) // it started after quiet began
	if st, _ := h.d.QuietState(ctx); len(st.Holding) != 0 || st.Running != 1 {
		t.Fatalf("a hint for a build that started during quiet: %+v", st)
	}
}

func TestTheQuietCountsSayWhatRunsAgainAndTheHintNamesTheBuilds(t *testing.T) {
	q := QuietState{Running: 2, Queued: 0}
	if got := q.Counts(); got != "2 running, 0 queued" {
		t.Errorf("counts %q", got)
	}
	q.Rerun = 1
	if got := q.Counts(); got != "2 running, 0 queued, 1 will run again after the restart" {
		t.Errorf("counts %q", got)
	}
	if got := q.Hint(); got != "" {
		t.Errorf("a hint with nothing holding: %q", got)
	}
	q.Holding = []BuildRef{{Job: "flow", Number: 3}}
	if got := q.Hint(); got != "Only flow #3 still runs; it may be waiting on something quiet holds back" {
		t.Errorf("hint %q", got)
	}
	q.Holding = append(q.Holding, BuildRef{Job: "pr", Number: 4}, BuildRef{Job: "x", Number: 1})
	if got := q.Hint(); got != "Only flow #3, pr #4 and x #1 still run; they may be waiting on something quiet holds back" {
		t.Errorf("hint %q", got)
	}
}
