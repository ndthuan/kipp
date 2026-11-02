package dispatch

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

// One job is the CI of every branch and the deploy of the default one: on
// a ticket branch deploy and what needs it are skipped and the build is a
// success; on the default branch, and in a build not started by a push,
// they run.
func TestAStageRunsOnlyWhereItsConditionHolds(t *testing.T) {
	h := newHarness(t, map[string]string{"j": "name: j\nconcurrency: parallel\nstages:\n" +
		"  ci: {run: 'echo \"ci $KIPP_BRANCH of $KIPP_DEFAULT_BRANCH\"'}\n" +
		"  deploy: {needs: [ci], when: {default_branch: true}, run: echo deploy}\n" +
		"  smoke: {needs: [deploy], run: echo smoke}\n" +
		"  preview: {needs: [ci], when: {default_branch: false, triggers: [webhook]}, run: echo preview}\n"})
	push := func(branch, def string) *store.Build {
		t.Helper()
		b, err := h.d.Trigger(context.Background(), TriggerRequest{Job: "j", Kind: "webhook", Detail: "abc123", Branch: branch, DefaultBranch: def})
		if err != nil {
			t.Fatal(err)
		}
		return h.wait(t, b.ID)
	}
	statuses := func(b *store.Build) map[string]store.StageStatus {
		out := map[string]store.StageStatus{}
		for n, s := range h.stages(t, b.ID) {
			out[n] = s.Status
		}
		return out
	}
	want := func(name string, b *store.Build, build store.Status, st map[string]store.StageStatus) {
		t.Helper()
		got := statuses(b)
		if b.Status != build {
			t.Errorf("%s: build %s, want %s", name, b.Status, build)
		}
		for n, s := range st {
			if got[n] != s {
				t.Errorf("%s: stage %s is %s, want %s", name, n, got[n], s)
			}
		}
	}
	ok, skip := store.StageSuccess, store.StageSkipped
	ticket := push("APP-12-fix", "main")
	want("ticket", ticket, store.Success, map[string]store.StageStatus{"ci": ok, "deploy": skip, "smoke": skip, "preview": ok})
	if got := h.log(t, ticket, "ci"); got != "ci APP-12-fix of main\n" {
		t.Errorf("ci log %q", got)
	}
	want("main", push("main", "main"), store.Success, map[string]store.StageStatus{"ci": ok, "deploy": ok, "smoke": ok, "preview": skip})
	want("a push whose default branch was not kept", push("main", ""), store.Success, map[string]store.StageStatus{"ci": ok, "deploy": skip, "smoke": skip})
	want("by hand", h.wait(t, h.trigger(t, "j").ID), store.Success, map[string]store.StageStatus{"ci": ok, "deploy": ok, "smoke": ok, "preview": skip})
}

func (h *harness) statuses(t *testing.T, id int64) map[string]store.StageStatus {
	t.Helper()
	out := map[string]store.StageStatus{}
	for n, s := range h.stages(t, id) {
		out[n] = s.Status
	}
	return out
}

// A param's default counts as the value it was given, and a bool param's
// form value is true or false.
func TestAStageRunsOnlyForTheParamValuesItNames(t *testing.T) {
	h := newHarness(t, map[string]string{"j": "name: j\nconcurrency: parallel\n" +
		"params:\n  TIER: {type: choice, options: [quick, full], default: quick}\n  DRY_RUN: {type: bool, default: true}\n" +
		"stages:\n  ci: {run: echo ci}\n" +
		"  full: {needs: [ci], when: {params: {TIER: [full]}}, run: echo full}\n" +
		"  quick: {needs: [ci], when: {params: {TIER: [quick]}}, run: echo quick}\n" +
		"  deploy: {needs: [ci], when: {params: {DRY_RUN: [false]}}, run: echo deploy}\n"})
	run := func(params map[string]string) map[string]store.StageStatus {
		t.Helper()
		b, err := h.d.Trigger(context.Background(), TriggerRequest{Job: "j", Kind: "web", By: "test", Params: params})
		if err != nil {
			t.Fatal(err)
		}
		if b = h.wait(t, b.ID); b.Status != store.Success {
			t.Fatalf("%v: build %s", params, b.Status)
		}
		return h.statuses(t, b.ID)
	}
	ok, skip := store.StageSuccess, store.StageSkipped
	for _, c := range []struct {
		params              map[string]string
		full, quick, deploy store.StageStatus
	}{
		{nil, skip, ok, skip},
		{map[string]string{"TIER": "full"}, ok, skip, skip},
		{map[string]string{"DRY_RUN": "false"}, skip, ok, ok},
	} {
		if got := run(c.params); got["ci"] != ok || got["full"] != c.full || got["quick"] != c.quick || got["deploy"] != c.deploy {
			t.Errorf("%v: %v", c.params, got)
		}
	}
}

// The cleanup runs after the failure and goes green; the build is still
// the failed build it was, and on_failure runs, not on_success.
func TestAGreenCleanupStageLeavesAFailedBuildFailed(t *testing.T) {
	h := newHarness(t, map[string]string{"j": "name: j\nstages:\n" +
		"  unit: {run: exit 1}\n" +
		"  deploy: {needs: [unit], run: echo deploy}\n" +
		"  report: {needs: [unit], when: {needs_status: failure}, run: echo report}\n" +
		"  cleanup: {needs: [deploy, report], when: {needs_status: always}, run: echo cleanup}\n" +
		"on_failure: {run: 'echo failure status=$KIPP_STATUS'}\n" +
		"on_success: {run: echo wrong hook}\n"})
	b := h.wait(t, h.trigger(t, "j").ID)
	want := map[string]store.StageStatus{"unit": store.StageFailed, "deploy": store.StageSkipped, "report": store.StageSuccess, "cleanup": store.StageSuccess}
	if got := h.statuses(t, b.ID); b.Status != store.Failed || !maps.Equal(got, want) {
		t.Fatalf("build %s, stages %v", b.Status, got)
	}
	if got := h.log(t, b, "cleanup"); got != "cleanup\n" {
		t.Fatalf("cleanup log %q", got)
	}
	if got := h.log(t, b, config.HooksLog); got != "failure status=failed\n" {
		t.Fatalf("hooks %q", got)
	}
}

// A cancel and the job's timeout each stop the build: a stage that would
// run however its needs ended is not started after them.
func TestNothingStartsAfterACancelEvenForAlways(t *testing.T) {
	for _, extra := range []string{"", "timeout: 1s"} {
		pidFile := filepath.Join(t.TempDir(), "pids")
		job := strings.Replace(groupJob("g", extra, pidFile), "later: {needs: [s],", "later: {needs: [s], when: {needs_status: always},", 1)
		h := newHarness(t, map[string]string{"g": job})
		b := h.trigger(t, "g")
		readPids(t, pidFile)
		want := store.Timeout
		if extra == "" {
			want = store.Cancelled
			if err := h.d.Cancel(context.Background(), b.ID, "ada"); err != nil {
				t.Fatal(err)
			}
		}
		b = h.wait(t, b.ID)
		st := h.stages(t, b.ID)
		if b.Status != want || st["later"].Status != store.StageCancelled || st["later"].StartedAt != nil {
			t.Fatalf("%q: build %s, later %+v", extra, b.Status, st["later"])
		}
		if _, err := os.Stat(h.d.LogPath(b.Job, b.Number, "later")); !os.IsNotExist(err) {
			t.Fatalf("%q: the always stage ran", extra)
		}
	}
}
