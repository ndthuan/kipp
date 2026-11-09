package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

// ended writes builds of a job oldest first from a shape read newest first
// (s success, f failed), the newest ending an hour ago.
func (e *env) ended(t *testing.T, job, shape string) {
	t.Helper()
	ctx := context.Background()
	for i := len(shape) - 1; i >= 0; i-- {
		at := time.Now().Add(-time.Duration(i+1) * time.Hour)
		b := &store.Build{Job: job, Status: store.Running, Trigger: "cli", QueuedAt: at}
		must(t, e.st.CreateBuild(ctx, b))
		st := store.Success
		if shape[i] == 'f' {
			st = store.Failed
		}
		must(t, e.st.SetBuildStatus(ctx, b.ID, st, at))
	}
}

func rowsOf(t *testing.T, o map[string]any, key string) []map[string]any {
	t.Helper()
	list, ok := o[key].([]any)
	if !ok {
		t.Fatalf("%s is not a list: %v", key, o[key])
	}
	out := make([]map[string]any, len(list))
	for i, r := range list {
		out[i] = r.(map[string]any)
	}
	return out
}

func jobsOf(rows []map[string]any, key string) string {
	var out []string
	for _, r := range rows {
		if b, ok := r[key].(map[string]any); ok {
			out = append(out, fmt.Sprint(b["job"]))
		} else {
			out = append(out, fmt.Sprint(r[key]))
		}
	}
	return strings.Join(out, ",")
}

func TestTheOverviewAnswersEverySectionAsTheSpecDescribesItForATokenUser(t *testing.T) {
	e := newEnv(t)
	e.ended(t, "smoke", "fsss")
	e.ended(t, "slow", "ssssfssssfsssssfsssf")
	e.ended(t, "once", "ffffssffssf")
	o := e.object(t, http.StatusOK, e.admin, "GET", "/api/v1/overview", "")
	eqKeys(t, "keys", o, "charts,failing,flaky,groups,more_failures,recent_failures,unstable,upcoming")

	failing := rowsOf(t, o, "failing")
	// The job red for the shortest time first.
	eq(t, "failing", jobsOf(failing, "job"), "smoke,once")
	eqKeys(t, "a failing row", failing[0], "build,flaky,job,recent,since")
	eq(t, "smoke does not flap", failing[0]["flaky"], false)
	eq(t, "once does", failing[1]["flaky"], true)
	eq(t, "the red build", failing[0]["build"].(map[string]any)["status"], "failed")

	flaky := rowsOf(t, o, "flaky")
	eq(t, "flaky", jobsOf(flaky, "job"), "slow")
	eqKeys(t, "a flaky row", flaky[0], "failed,failed_percent,finished,flips,job,recent")
	eq(t, "failed", fmt.Sprint(flaky[0]["failed"], "/", flaky[0]["finished"], " ", flaky[0]["failed_percent"], "% ", flaky[0]["flips"]), "4/20 20% 7")

	recent := rowsOf(t, o, "recent_failures")
	if len(recent) == 0 || len(recent) > 10 {
		t.Fatalf("%d recent failures", len(recent))
	}
	eqKeys(t, "a recent failure", recent[0], "build,green_again")

	upcoming := rowsOf(t, o, "upcoming")
	eq(t, "the one cron", jobsOf(upcoming, "job"), "gc")
	eqKeys(t, "an upcoming build", upcoming[0], "at,cron,job")
	eq(t, "its cron", upcoming[0]["cron"], "*/10 * * * *")

	groups := rowsOf(t, o, "groups")
	eq(t, "groups", jobsOf(groups, "name"), "backend,")
	eqKeys(t, "a group", groups[0], "disabled,green,name,never_built,red,running")
	eq(t, "backend has smoke, red", groups[0]["red"], any(float64(1)))
}

func TestTheOverviewIsAnsweredToASessionUserAsToATokenUser(t *testing.T) {
	e := newEnv(t)
	e.ended(t, "smoke", "fs")
	sess := &store.Session{ID: "sess-ov", UserID: e.user(t, "admin").ID, CSRF: "csrf-ov", ExpiresAt: time.Now().Add(time.Hour)}
	must(t, e.st.CreateSession(context.Background(), sess))
	o := e.object(t, http.StatusOK, "", "GET", "/api/v1/overview", "", "Cookie", SessionCookieFor(sess).String())
	eq(t, "failing", jobsOf(rowsOf(t, o, "failing"), "job"), "smoke")
	e.call(t, http.StatusUnauthorized, "", "GET", "/api/v1/overview", "")
}

func TestTheOverviewKeepsToTheJobsTheCallerMayViewAndASectionOfNoneIsEmpty(t *testing.T) {
	e := newEnv(t)
	e.ended(t, "smoke", "ffff")
	e.ended(t, "slow", "ffff")
	o := e.object(t, http.StatusOK, e.viewer, "GET", "/api/v1/overview", "") // may view smoke
	eq(t, "failing", jobsOf(rowsOf(t, o, "failing"), "job"), "smoke")
	eq(t, "recent", jobsOf(rowsOf(t, o, "recent_failures"), "build"), "smoke,smoke,smoke,smoke")
	eq(t, "upcoming: gc is not the viewer's", len(rowsOf(t, o, "upcoming")), 0)
	eq(t, "groups", jobsOf(rowsOf(t, o, "groups"), "name"), "backend")

	none := token(t, e.st, "nobody", false, nil)
	o = e.object(t, http.StatusOK, none, "GET", "/api/v1/overview", "")
	for _, k := range []string{"failing", "flaky", "recent_failures", "upcoming", "groups"} {
		eq(t, k, len(rowsOf(t, o, k)), 0)
	}
	eq(t, "more", o["more_failures"], any(float64(0)))
}

func TestAJobRemovedFromTheFilesIsInNoSectionAndADisabledOneOnlyInItsGroupCount(t *testing.T) {
	e := newEnv(t)
	e.ended(t, "ghost", "ffff") // no file
	off, err := config.ParseJob("off.yml", []byte("name: off\ngroup: backend\ndisabled: true\ntriggers: {cron: '* * * * *'}\nstages:\n  s: {run: 'true'}\n"))
	must(t, err)
	e.api.deps.Dispatch.SetSnapshot(e.api.deps.Dispatch.Snapshot().WithJobs(off))
	e.ended(t, "off", "ffff")
	o := e.object(t, http.StatusOK, e.admin, "GET", "/api/v1/overview", "")
	eq(t, "failing", len(rowsOf(t, o, "failing")), 0)
	eq(t, "recent", len(rowsOf(t, o, "recent_failures")), 0)
	eq(t, "upcoming", jobsOf(rowsOf(t, o, "upcoming"), "job"), "gc")
	groups := rowsOf(t, o, "groups")
	eq(t, "groups", jobsOf(groups, "name"), "backend,")
	eq(t, "disabled", groups[0]["disabled"], any(float64(1)))
	eq(t, "backend's smoke, never built", groups[0]["never_built"], any(float64(1)))
}

func TestAMultibranchJobAndAnOwnerJobAreRepresentedByTheirChildrenAlone(t *testing.T) {
	e := newEnv(t)
	tok := e.withOwner(t)
	ctx := context.Background()
	b := &store.Build{Job: "acme.site.main", Parent: "acme.site", Status: store.Running, Trigger: "webhook", TriggerBranch: "main"}
	must(t, e.st.CreateBuild(ctx, b))
	must(t, e.st.SetBuildStatus(ctx, b.ID, store.Failed, time.Now()))
	o := e.object(t, http.StatusOK, tok, "GET", "/api/v1/overview", "")
	eq(t, "failing: the child, not the repo's node nor the owner job", jobsOf(rowsOf(t, o, "failing"), "job"), "acme.site.main")
	groups := rowsOf(t, o, "groups")
	eq(t, "one group, web", jobsOf(groups, "name"), "web")
	eq(t, "one red job in it", fmt.Sprint(groups[0]["red"], groups[0]["green"], groups[0]["never_built"]), "1 0 0")
}

func TestFlakyFlipsOfZeroLeavesTheFlakySectionEmpty(t *testing.T) {
	e := newEnv(t, func(a *API) { a.deps.FlakyFlips = 0 })
	e.ended(t, "slow", "ssssfssssfsssssfsssf")
	e.ended(t, "once", "ffffssffssf")
	o := e.object(t, http.StatusOK, e.admin, "GET", "/api/v1/overview", "")
	eq(t, "flaky", len(rowsOf(t, o, "flaky")), 0)
	failing := rowsOf(t, o, "failing")
	eq(t, "once is failing, untagged", fmt.Sprint(jobsOf(failing, "job"), failing[0]["flaky"]), "oncefalse")
}

// endedAt writes one build of a job with the status, ended ago ago.
func (e *env) endedAt(t *testing.T, job string, st store.Status, ago time.Duration) {
	t.Helper()
	b := &store.Build{Job: job, Status: store.Running, Trigger: "cli", QueuedAt: time.Now().Add(-ago - 2*time.Minute)}
	must(t, e.st.CreateBuild(context.Background(), b))
	must(t, e.st.SetBuildStatus(context.Background(), b.ID, store.Running, time.Now().Add(-ago-time.Minute)))
	must(t, e.st.SetBuildStatus(context.Background(), b.ID, st, time.Now().Add(-ago)))
}

func TestTheOverviewAnswersTheChartsOfTheDayAndTheWeekAndRefusesAnyOtherWindow(t *testing.T) {
	e := newEnv(t)
	e.endedAt(t, "smoke", store.Success, time.Hour)
	e.endedAt(t, "smoke", store.Failed, 2*time.Hour)
	e.endedAt(t, "smoke", store.Unstable, 3*time.Hour)
	e.endedAt(t, "smoke", store.Failed, 3*24*time.Hour)
	e.endedAt(t, "slow", store.Cancelled, time.Hour)

	day := e.object(t, http.StatusOK, e.admin, "GET", "/api/v1/overview", "")
	week := e.object(t, http.StatusOK, e.admin, "GET", "/api/v1/overview?window=7d", "")
	explicit := e.object(t, http.StatusOK, e.admin, "GET", "/api/v1/overview?window=24h", "")
	dc, wc := day["charts"].(map[string]any), week["charts"].(map[string]any)
	eqKeys(t, "charts", dc, "bucket_seconds,buckets,builds_limit,end,groups,start,window")
	eq(t, "the default window", dc["window"], "24h")
	eq(t, "explicit 24h is the default", explicit["charts"].(map[string]any)["window"], "24h")
	eq(t, "the week", wc["window"], "7d")
	eq(t, "day columns", len(rowsOf(t, dc, "buckets")), 25)
	eq(t, "week columns", len(rowsOf(t, wc, "buckets")), 29)
	eq(t, "bucket", dc["bucket_seconds"], any(float64(3600)))
	eqKeys(t, "a bucket", rowsOf(t, dc, "buckets")[0], "cancelled,failed,max_running,start,started,success,unstable,wait_max_seconds,wait_median_seconds")

	sum := func(c map[string]any, key string) int {
		n := 0
		for _, b := range rowsOf(t, c, "buckets") {
			n += int(b[key].(float64))
		}
		return n
	}
	eq(t, "day: one success", sum(dc, "success"), 1)
	eq(t, "day: failed", sum(dc, "failed"), 1)
	eq(t, "day: unstable", sum(dc, "unstable"), 1)
	eq(t, "day: cancelled", sum(dc, "cancelled"), 1)
	eq(t, "week: the failure of three days ago too", sum(wc, "failed"), 2)
	eq(t, "the failed list follows the window", len(rowsOf(t, day, "recent_failures")), 1)
	eq(t, "the failed list of the week", len(rowsOf(t, week, "recent_failures")), 2)
	eq(t, "the limit of builds", dc["builds_limit"], any(float64(e.api.deps.Dispatch.Builds())))

	groups := rowsOf(t, dc, "groups")
	eqKeys(t, "a group's rate", groups[0], "before,change,name,now")
	eqKeys(t, "a tally", groups[0]["now"].(map[string]any), "green,percent,red")
	eq(t, "backend: 2 of 3 builds green, unstable being green", fmt.Sprint(groups[0]["name"], groups[0]["now"].(map[string]any)["percent"]), "backend67")
	if groups[0]["change"] != nil {
		t.Fatalf("no change with no build before: %v", groups[0]["change"])
	}

	for _, bad := range []string{"1h", "30d", "week", "24H"} {
		body, _ := e.call(t, http.StatusBadRequest, e.admin, "GET", "/api/v1/overview?window="+bad, "")
		contains(t, body, "window is 24h or 7d")
	}
}

func TestTheUnstableSectionHoldsTheJobsWhoseNewestBuildIsUnstableAndSaysSince(t *testing.T) {
	e := newEnv(t)
	e.endedAt(t, "smoke", store.Success, 5*time.Hour)
	e.endedAt(t, "smoke", store.Unstable, 4*time.Hour)
	e.endedAt(t, "smoke", store.Unstable, time.Hour)
	e.endedAt(t, "slow", store.Unstable, 3*time.Hour)
	e.endedAt(t, "slow", store.Failed, time.Hour)
	o := e.object(t, http.StatusOK, e.admin, "GET", "/api/v1/overview", "")
	unstable := rowsOf(t, o, "unstable")
	eq(t, "unstable", jobsOf(unstable, "job"), "smoke")
	eqKeys(t, "an unstable row", unstable[0], "build,flaky,job,recent,since")
	eq(t, "the build", unstable[0]["build"].(map[string]any)["status"], "unstable")
	eq(t, "a job unstable and then failed is failing only", jobsOf(rowsOf(t, o, "failing"), "job"), "slow")
}
