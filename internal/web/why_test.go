package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/store"
)

// stage is how one stage of a finished build ended; code is nil for a stage
// that has no exit code.
type stage struct {
	name   string
	status store.StageStatus
	code   *int
}

// ended writes a build of the job that ended an hour ago with the status and
// the stages, and returns its page's address.
func (e *env) ended(t *testing.T, job string, status store.Status, stages ...stage) string {
	t.Helper()
	ctx := context.Background()
	at := time.Now().Add(-time.Hour)
	b := &store.Build{Job: job, Status: store.Running, Trigger: "cli", QueuedAt: at.Add(-2 * time.Minute)}
	must(t, e.st.CreateBuild(ctx, b))
	must(t, e.st.SetBuildStatus(ctx, b.ID, store.Running, at.Add(-time.Minute)))
	var names []string
	for _, s := range stages {
		names = append(names, s.name)
	}
	if len(names) > 0 {
		must(t, e.st.PutStages(ctx, b.ID, names, nil))
	}
	for _, s := range stages {
		must(t, e.st.SetStage(ctx, b.ID, s.name, store.StageRunning, nil, at.Add(-time.Minute)))
		must(t, e.st.SetStage(ctx, b.ID, s.name, s.status, s.code, at))
	}
	must(t, e.st.SetBuildStatus(ctx, b.ID, status, at))
	return e.at(t, b.ID)
}

func TestTheOverviewPutsWhatIsRedFirstAndTheGraphsLast(t *testing.T) {
	e := site(t)
	e.ended(t, "smoke", store.Failed, stage{"test", store.StageFailed, new(2)})
	page := e.overviewPage(t, "admin")
	var at []int
	for _, mark := range []string{"<h2>Failing now</h2>", "Unstable now</h2>", "<h2>Flaky</h2>", "<h2>Failed in the last", `<section id="queue"`, "<h2>Coming up</h2>", `<section class="charts`} {
		i := strings.Index(page, mark)
		if i < 0 {
			t.Fatalf("%q is not on the page", mark)
		}
		at = append(at, i)
	}
	for i := 1; i < len(at); i++ {
		if at[i] < at[i-1] {
			t.Fatalf("section %d comes before section %d: %v", i, i-1, at)
		}
	}
}

func TestAFailingRowNamesTheFailedStagesAndLinksToTheirLogs(t *testing.T) {
	e := site(t)
	e.ended(t, "smoke", store.Failed,
		stage{"build", store.StageSuccess, nil},
		stage{"lint", store.StageFailed, new(1)},
		stage{"test", store.StageFailed, new(2)},
		stage{"deploy", store.StageSkipped, nil})
	e.ended(t, "slow", store.Timeout, stage{"wait", store.StageCancelled, nil})
	e.ended(t, "nightly", store.Failed)
	page := e.overviewPage(t, "admin")
	contains(t, page,
		`failed in <a class="link" href="/jobs/smoke/1#log-lint">lint</a>, <a class="link" href="/jobs/smoke/1#log-test">test</a>`,
		`timed out in <a class="link" href="/jobs/slow/1#log-wait">wait</a>`,
		// A build that failed before any stage ran failed in its prepare step.
		`failed in <a class="link" href="/jobs/nightly/1#log-prepare">prepare</a>`)
	lacks(t, page, "#log-build", "#log-deploy")
}

func TestTheNoticeOnARedBuildSaysWhichStageAndHowItEnded(t *testing.T) {
	e := site(t)
	c := e.login(t, "admin")
	for name, tc := range map[string]struct {
		status store.Status
		stages []stage
		want   []string
	}{
		"failed with an exit code": {store.Failed, []stage{{"build", store.StageSuccess, nil}, {"test", store.StageFailed, new(2)}},
			[]string{`class="notice notice-error"`, "Stage test failed, exit 2", `href="#log-test">Go to the log</a>`, `data-tail-of="test"`}},
		"failed with no exit code and another failed": {store.Failed, []stage{{"a", store.StageFailed, nil}, {"b", store.StageFailed, new(1)}},
			[]string{"Stage a failed and 1 more", `data-tail-of="a"`}},
		"failed before any stage ran": {store.Failed, nil,
			[]string{`class="notice notice-error"`, "Failed before any stage ran"}},
		"timed out": {store.Timeout, []stage{{"build", store.StageSuccess, nil}, {"slow", store.StageCancelled, nil}},
			[]string{`class="notice notice-warn"`, "Timed out after 1m 0s in stage slow", `href="#log-slow"`, `data-tail-of="slow"`}},
		"lost": {store.Lost, []stage{{"slow", store.StageCancelled, nil}},
			[]string{`class="notice notice-warn"`, "Lost during stage slow: the runner stopped answering", `data-tail-of="slow"`}},
		"unstable":  {store.Unstable, []stage{{"lint", store.StageUnstable, nil}}, nil},
		"cancelled": {store.Cancelled, []stage{{"lint", store.StageCancelled, nil}}, nil},
		"success":   {store.Success, []stage{{"lint", store.StageSuccess, nil}}, nil},
	} {
		code, page := get(t, c, e.srv.URL+e.ended(t, "smoke", tc.status, tc.stages...))
		eq(t, name, code, http.StatusOK)
		if tc.want == nil {
			lacks(t, page, `class="notice `, "data-tail-of")
			continue
		}
		contains(t, page, tc.want...)
	}
}

func TestTheStageTableLinksEachStageToItsLogAndTheLogHeadingCarriesTheStatus(t *testing.T) {
	e := site(t)
	_, page := get(t, e.login(t, "admin"), e.srv.URL+e.ended(t, "smoke", store.Failed,
		stage{"build", store.StageSuccess, nil}, stage{"test", store.StageFailed, new(2)}))
	contains(t, page,
		`<td><a class="link" href="#log-build">build</a></td>`, `<td><a class="link" href="#log-test">test</a></td>`,
		`<summary>test<span class="status failed" data-chip>failed</span></summary>`,
		`<summary>build<span class="status success" data-chip>success</span></summary>`,
		// The refresh of a live build brings the states, so the headings follow.
		`data-states="build=success test=failed "`)
}
