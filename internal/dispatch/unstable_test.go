package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/runner"
	"github.com/ndthuan/kipp/internal/store"
)

// bothHooks tells which hook ran and with which KIPP_STATUS.
const bothHooks = "on_success: {run: 'echo on_success status=$KIPP_STATUS'}\non_failure: {run: 'echo on_failure status=$KIPP_STATUS'}\n"

// downOf is a job that runs after up.
func downOf(up string) string {
	return "name: down\ntriggers: {after: [" + up + "]}\nstages:\n  s: {run: 'echo from=$KIPP_UPSTREAM_BUILD'}\n"
}

// downs is the builds of the job called down, newest first.
func (h *harness) downs(t *testing.T) []*store.Build {
	t.Helper()
	bs, err := h.st.Builds(context.Background(), "down", 10)
	if err != nil {
		t.Fatal(err)
	}
	return bs
}

// untilStage waits until the stage of the build is in the status.
func (h *harness) untilStage(t *testing.T, id int64, stage string, want store.StageStatus) {
	t.Helper()
	eventually(t, "stage "+stage+" is "+string(want), func() bool {
		st, err := h.st.Stages(context.Background(), id)
		if err != nil {
			return false
		}
		return slices.ContainsFunc(st, func(s *store.Stage) bool { return s.Name == stage && s.Status == want })
	})
}

func TestAnUnstableExitEndsTheStageAndTheBuildUnstableAndWhatNeedsItRuns(t *testing.T) {
	h := newHarness(t, map[string]string{"u": `
name: u
stages:
  a: {run: "echo checked; exit 4", unstable_exit: [3, 4]}
  b: {needs: [a], run: "echo after-a"}
  c: {needs: [b], run: "echo after-b"}
`})
	b := h.wait(t, h.trigger(t, "u").ID)
	st := h.stages(t, b.ID)
	if b.Status != store.Unstable || b.FinishedAt == nil {
		t.Fatalf("the build ended %s", b.Status)
	}
	if a := st["a"]; a.Status != store.StageUnstable || a.ExitCode == nil || *a.ExitCode != 4 || a.FinishedAt == nil {
		t.Fatalf("a: %+v", a)
	}
	if st["b"].Status != store.StageSuccess || st["c"].Status != store.StageSuccess {
		t.Fatalf("what needs the unstable stage: b %s, c %s", st["b"].Status, st["c"].Status)
	}
	if got := h.log(t, b, "a"); got != "checked\n" {
		t.Fatalf("the log of a stage that exited by itself gained a line: %q", got)
	}
	if got := h.log(t, b, "c"); got != "after-b\n" {
		t.Fatalf("c log %q", got)
	}
}

func TestOnlyACodeTheStageItselfNamesIsUnstable(t *testing.T) {
	h := newHarness(t, map[string]string{"m": `
name: m
stages:
  named: {run: "exit 4", unstable_exit: [4]}
  other-code: {run: "exit 5", unstable_exit: [4]}
  no-list: {run: "exit 4"}
  ok: {run: "true", unstable_exit: [4]}
`})
	b := h.wait(t, h.trigger(t, "m").ID)
	st := h.stages(t, b.ID)
	want := map[string]store.StageStatus{"named": store.StageUnstable, "other-code": store.StageFailed, "no-list": store.StageFailed, "ok": store.StageSuccess}
	for name, status := range want {
		if st[name].Status != status {
			t.Errorf("%s ended %s, want %s", name, st[name].Status, status)
		}
	}
}

func TestAFailedStageWinsOverAnUnstableOne(t *testing.T) {
	h := newHarness(t, map[string]string{
		"f": `
name: f
stages:
  shaky: {run: "exit 4", unstable_exit: [4]}
  after-shaky: {needs: [shaky], run: "echo ran"}
  broken: {run: "exit 1"}
  after-both: {needs: [shaky, broken], run: "echo never"}
` + bothHooks,
		"down": downOf("f"),
	})
	b := h.wait(t, h.trigger(t, "f").ID)
	st := h.stages(t, b.ID)
	if b.Status != store.Failed {
		t.Fatalf("a build with a failed and an unstable stage ended %s", b.Status)
	}
	if st["shaky"].Status != store.StageUnstable || st["after-shaky"].Status != store.StageSuccess || st["broken"].Status != store.StageFailed || st["after-both"].Status != store.StageSkipped {
		t.Fatalf("shaky %s, after-shaky %s, broken %s, after-both %s", st["shaky"].Status, st["after-shaky"].Status, st["broken"].Status, st["after-both"].Status)
	}
	if got := h.log(t, b, "hooks"); got != "on_failure status=failed\n" {
		t.Fatalf("hooks %q", got)
	}
	if downs := h.downs(t); len(downs) != 0 {
		t.Fatalf("a failed build triggered downstream: %+v", downs)
	}
}

// stoppedAfterUnstable is a job whose first stage ends unstable and whose
// second, which needs it, hangs until the build is stopped.
func stoppedAfterUnstable(name, extra string) string {
	return "name: " + name + "\n" + extra + `
stages:
  shaky: {run: "exit 4", unstable_exit: [4]}
  hangs: {needs: [shaky], run: "sleep 60"}
` + bothHooks
}

func (h *harness) assertStoppedAfterUnstable(t *testing.T, b *store.Build, want store.Status) {
	t.Helper()
	st := h.stages(t, b.ID)
	if b.Status != want {
		t.Fatalf("the build ended %s, want %s", b.Status, want)
	}
	if st["shaky"].Status != store.StageUnstable || st["hangs"].Status != store.StageCancelled {
		t.Fatalf("shaky %s, hangs %s", st["shaky"].Status, st["hangs"].Status)
	}
	if downs := h.downs(t); len(downs) != 0 {
		t.Fatalf("a %s build triggered downstream: %+v", want, downs)
	}
}

func TestACancelWinsOverAnUnstableStage(t *testing.T) {
	h := newHarness(t, map[string]string{"x": stoppedAfterUnstable("x", ""), "down": downOf("x")})
	b := h.trigger(t, "x")
	h.untilStage(t, b.ID, "hangs", store.StageRunning)
	if err := h.d.Cancel(context.Background(), b.ID, "ada"); err != nil {
		t.Fatal(err)
	}
	got := h.wait(t, b.ID)
	h.assertStoppedAfterUnstable(t, got, store.Cancelled)
	if hooks := h.log(t, got, "hooks"); hooks != "on_failure status=cancelled\n" {
		t.Fatalf("hooks %q", hooks)
	}
}

func TestATimeoutWinsOverAnUnstableStage(t *testing.T) {
	h := newHarness(t, map[string]string{"x": stoppedAfterUnstable("x", "timeout: 1s"), "down": downOf("x")})
	got := h.wait(t, h.trigger(t, "x").ID)
	h.assertStoppedAfterUnstable(t, got, store.Timeout)
	if hooks := h.log(t, got, "hooks"); hooks != "on_failure status=timeout\n" {
		t.Fatalf("hooks %q", hooks)
	}
}

func TestAShutdownWinsOverAnUnstableStage(t *testing.T) {
	h := newHarness(t, map[string]string{"x": stoppedAfterUnstable("x", ""), "down": downOf("x")})
	b := h.trigger(t, "x")
	h.untilStage(t, b.ID, "hangs", store.StageRunning)
	h.stop()
	got, err := h.st.Build(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	h.assertStoppedAfterUnstable(t, got, store.Lost)
	if _, err := os.Stat(h.d.LogPath("x", b.Number, "hooks")); !os.IsNotExist(err) {
		t.Fatal("a hook ran for a build the shutdown ended")
	}
}

func TestAnUnstableBuildRunsOnSuccessWithItsStatusAndFiresWhatRunsAfterIt(t *testing.T) {
	h := newHarness(t, map[string]string{
		"up":   "name: up\nstages:\n  s: {run: exit 4, unstable_exit: [4]}\n" + bothHooks,
		"down": downOf("up"),
	})
	up := h.wait(t, h.trigger(t, "up").ID)
	if up.Status != store.Unstable {
		t.Fatalf("up ended %s", up.Status)
	}
	if got := h.log(t, up, "hooks"); got != "on_success status=unstable\n" {
		t.Fatalf("hooks %q", got)
	}
	var downs []*store.Build
	eventually(t, "the downstream job was triggered", func() bool {
		downs = h.downs(t)
		return len(downs) == 1
	})
	down := h.wait(t, downs[0].ID)
	if down.Trigger != "after" || down.TriggerDetail != "up#1" || !strings.Contains(h.log(t, down, "s"), "from=up#1") {
		t.Fatalf("%+v", down)
	}
}

func TestAnUnstableBuildWithOnlyOnFailureRunsNoHook(t *testing.T) {
	h := newHarness(t, map[string]string{
		"u": "name: u\nstages:\n  s: {run: exit 4, unstable_exit: [4]}\non_failure: {run: echo wrong hook}\n",
	})
	b := h.wait(t, h.trigger(t, "u").ID)
	if b.Status != store.Unstable {
		t.Fatalf("ended %s", b.Status)
	}
	if _, err := os.Stat(h.d.LogPath("u", b.Number, "hooks")); !os.IsNotExist(err) {
		t.Fatalf("on_failure ran for an unstable build: %q", h.log(t, b, "hooks"))
	}
}

func TestTheJunitReportsOfAnUnstableStageAreStored(t *testing.T) {
	h := newHarness(t, map[string]string{"j": `
name: j
stages:
  t:
    run: 'mkdir -p r; printf "<testsuite name=\"g\"><testcase name=\"a\"/><testcase name=\"b\"><failure message=\"no\"/></testcase></testsuite>" > r/x.xml; exit 4'
    junit: r/*.xml
    unstable_exit: [4]
`})
	b := h.wait(t, h.trigger(t, "j").ID)
	tests, err := h.st.Tests(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != store.Unstable || len(tests) != 2 || tests[1].Status != "failed" || tests[1].Message != "no" || tests[1].Stage != "t" {
		t.Fatalf("%v %+v", b.Status, tests)
	}
}

// An unstable build is over: it holds neither its job's turn nor a lock,
// and retention is run for it like for any other.
func TestAnUnstableBuildFreesItsJobItsLockAndIsRetained(t *testing.T) {
	var retained []string
	h := newHarness(t, map[string]string{
		"d":     "name: d\nconcurrency: drop\nlocks: [shared]\nstages:\n  s: {run: exit 4, unstable_exit: [4]}\n",
		"other": "name: other\nlocks: [shared]\nstages:\n  s: {run: 'true'}\n",
	}, func(o *Options) {
		o.Retain = func(_ context.Context, _ *store.Store, job *config.Job, _ string, _ time.Time) error {
			retained = append(retained, job.Name)
			return nil
		}
	})
	first := h.wait(t, h.trigger(t, "d").ID)
	if first.Status != store.Unstable {
		t.Fatalf("ended %s", first.Status)
	}
	if h.d.Busy(first.ID) {
		t.Fatal("an unstable build Wait has returned is still busy")
	}
	second, err := h.d.Trigger(context.Background(), TriggerRequest{Job: "d", Kind: "api"})
	if err != nil {
		t.Fatalf("a drop job refused a trigger after its unstable build: %v", err)
	}
	if got := h.wait(t, second.ID); got.Status != store.Unstable {
		t.Fatalf("the second build ended %s", got.Status)
	}
	if got := h.wait(t, h.trigger(t, "other").ID); got.Status != store.Success {
		t.Fatalf("a job sharing the lock ended %s", got.Status)
	}
	if want := []string{"d", "d", "other"}; !slices.Equal(retained, want) {
		t.Fatalf("retention ran for %q, want %q", retained, want)
	}
	if err := h.d.Cancel(context.Background(), first.ID, "ada"); err == nil || !strings.Contains(err.Error(), "it is unstable") {
		t.Fatalf("cancelling an unstable build: %v", err)
	}
}

// A cancel, a timeout or a shutdown that lands after the last stage ended
// and before the status is settled wins over unstable, as it does over success.
func TestWhatStoppedTheBuildBeforeItSettledWinsOverUnstable(t *testing.T) {
	d := New(nil, runner.Local{}, Options{})
	for want, cause := range map[store.Status]error{
		store.Cancelled: cancelledBy("ada"),
		store.Timeout:   timedOut(time.Second),
		store.Lost:      nil,
	} {
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(cause)
		for _, ran := range []store.Status{store.Unstable, store.Success} {
			r := &run{}
			if got := d.settle(ctx, r, ran); got != want || !r.settled {
				t.Errorf("stages ended %s, then %v: settled as %s, want %s", ran, cause, got, want)
			}
		}
	}
	r := &run{}
	if got := d.settle(context.Background(), r, store.Unstable); got != store.Unstable || !r.settled {
		t.Errorf("nothing stopped the build: settled as %s", got)
	}
}

// trapped is a job whose stage, once killed, exits with a code it lists as
// unstable. It writes to ready once the trap is set.
func trapped(extra, ready string) string {
	return "name: x\n" + extra + `
stages:
  s: {run: "trap 'exit 4' TERM; echo set > ` + ready + `; while :; do sleep 0.05; done", unstable_exit: [4]}
` + bothHooks
}

// The code a killed command exits with says nothing: kipp ended it.
func TestAStageKippStoppedIsCancelledWhateverCodeItExitsWith(t *testing.T) {
	stop := map[store.Status]func(h *harness, id int64){
		store.Cancelled: func(h *harness, id int64) {
			if err := h.d.Cancel(context.Background(), id, "ada"); err != nil {
				t.Fatal(err)
			}
		},
		store.Timeout: func(*harness, int64) {},
	}
	for want, do := range stop {
		ready := filepath.Join(t.TempDir(), "ready")
		extra := ""
		if want == store.Timeout {
			extra = "timeout: 1s"
		}
		h := newHarness(t, map[string]string{"x": trapped(extra, ready), "down": downOf("x")})
		b := h.trigger(t, "x")
		eventually(t, "the stage has set its trap", func() bool { return exists(ready) })
		do(h, b.ID)
		got := h.wait(t, b.ID)
		s := h.stages(t, b.ID)["s"]
		if got.Status != want || s.Status != store.StageCancelled {
			t.Fatalf("stopped by %s: the build ended %s, the stage %s (exit %v)", want, got.Status, s.Status, s.ExitCode)
		}
		if s.ExitCode == nil || *s.ExitCode != 4 {
			t.Fatalf("the test's stage did not exit 4 on the kill: %v", s.ExitCode)
		}
		if hooks := h.log(t, got, "hooks"); hooks != "on_failure status="+string(want)+"\n" {
			t.Fatalf("stopped by %s: hooks %q", want, hooks)
		}
		if downs := h.downs(t); len(downs) != 0 {
			t.Fatalf("a %s build triggered downstream", want)
		}
	}
}

func TestAStageThatFailsAfterAnUnstableOneFailsTheBuild(t *testing.T) {
	h := newHarness(t, map[string]string{
		"f": `
name: f
stages:
  shaky: {run: "exit 4", unstable_exit: [4]}
  breaks: {needs: [shaky], run: "exit 1"}
  later: {needs: [breaks], run: "echo never"}
` + bothHooks,
		"down": downOf("f"),
	})
	b := h.wait(t, h.trigger(t, "f").ID)
	st := h.stages(t, b.ID)
	if b.Status != store.Failed || st["shaky"].Status != store.StageUnstable || st["breaks"].Status != store.StageFailed || st["later"].Status != store.StageSkipped {
		t.Fatalf("build %s: shaky %s, breaks %s, later %s", b.Status, st["shaky"].Status, st["breaks"].Status, st["later"].Status)
	}
	if got := h.log(t, b, "hooks"); got != "on_failure status=failed\n" {
		t.Fatalf("hooks %q", got)
	}
	if downs := h.downs(t); len(downs) != 0 {
		t.Fatalf("a failed build triggered downstream: %+v", downs)
	}
}

func TestACancelBesideABranchThatEndedUnstableLeavesThatStageUnstable(t *testing.T) {
	h := newHarness(t, map[string]string{"x": `
name: x
stages:
  shaky: {run: "exit 4", unstable_exit: [4]}
  slow: {run: "sleep 60"}
  join: {needs: [shaky, slow], run: "echo never"}
`})
	b := h.trigger(t, "x")
	h.untilStage(t, b.ID, "shaky", store.StageUnstable)
	h.untilStage(t, b.ID, "slow", store.StageRunning)
	if err := h.d.Cancel(context.Background(), b.ID, "ada"); err != nil {
		t.Fatal(err)
	}
	got := h.wait(t, b.ID)
	st := h.stages(t, b.ID)
	if got.Status != store.Cancelled || st["shaky"].Status != store.StageUnstable || st["slow"].Status != store.StageCancelled || st["join"].Status != store.StageSkipped {
		t.Fatalf("build %s: shaky %s, slow %s, join %s", got.Status, st["shaky"].Status, st["slow"].Status, st["join"].Status)
	}
}

// A job that runs after another is told how that one ended, so that it can
// refuse an unstable upstream; its hook is told too.
func TestABuildStartedAfterAnotherIsToldHowThatOneEnded(t *testing.T) {
	down := "name: down\ntriggers: {after: [clean, shaky]}\nconcurrency: parallel\nstages:\n  s: {run: 'echo stage $KIPP_UPSTREAM_BUILD $KIPP_UPSTREAM_STATUS'}\n" +
		"on_success: {run: 'echo hook $KIPP_UPSTREAM_BUILD $KIPP_UPSTREAM_STATUS'}\n"
	h := newHarness(t, map[string]string{
		"clean": "name: clean\nstages:\n  s: {run: 'true'}\n",
		"shaky": "name: shaky\nstages:\n  s: {run: exit 4, unstable_exit: [4]}\n",
		"down":  down,
		"alone": "name: alone\nstages:\n  s: {run: 'echo \"[${KIPP_UPSTREAM_STATUS-unset}]\"'}\n",
	})
	for up, status := range map[string]string{"clean": "success", "shaky": "unstable"} {
		h.wait(t, h.trigger(t, up).ID)
		var found *store.Build
		eventually(t, "the build after "+up+" was triggered", func() bool {
			for _, b := range h.downs(t) {
				if b.TriggerDetail == up+"#1" {
					found = b
				}
			}
			return found != nil
		})
		b := h.wait(t, found.ID)
		if got, want := h.log(t, b, "s"), "stage "+up+"#1 "+status+"\n"; got != want {
			t.Errorf("stage of the build after %s: %q, want %q", up, got, want)
		}
		if got, want := h.log(t, b, "hooks"), "hook "+up+"#1 "+status+"\n"; got != want {
			t.Errorf("hook of the build after %s: %q, want %q", up, got, want)
		}
	}
	alone := h.wait(t, h.trigger(t, "alone").ID)
	if got := h.log(t, alone, "s"); got != "[unset]\n" {
		t.Errorf("a build nothing ran before has an upstream status: %q", got)
	}
}

// How the upstream build ended is recorded with the build it triggers, so
// the build is told also when retention has removed the upstream by the
// time it starts.
func TestABuildStartedAfterAnotherIsToldHowItEndedAlsoOnceThatBuildIsGone(t *testing.T) {
	held, open := gated(t)
	h := newHarness(t, map[string]string{
		"holder": "name: holder\nlocks: [l]\nstages:\n  s: {run: '" + held + "'}\n",
		"shaky":  "name: shaky\nstages:\n  s: {run: exit 4, unstable_exit: [4]}\n",
		"down":   "name: down\nlocks: [l]\ntriggers: {after: [shaky]}\nstages:\n  s: {run: 'echo \"[${KIPP_UPSTREAM_STATUS-unset}]\"'}\n",
	})
	holder := h.trigger(t, "holder")
	h.untilRunning(t, holder.ID)
	up := h.wait(t, h.trigger(t, "shaky").ID)
	var down *store.Build
	eventually(t, "the build after shaky is queued", func() bool {
		if bs := h.downs(t); len(bs) == 1 {
			down = bs[0]
		}
		return down != nil
	})
	if err := h.st.DeleteBuild(context.Background(), up.ID); err != nil {
		t.Fatal(err)
	}
	open()
	h.wait(t, holder.ID)
	if got := h.log(t, h.wait(t, down.ID), "s"); got != "[unstable]\n" {
		t.Errorf("the stage of a build whose upstream is gone: %q, want [unstable]", got)
	}
}
