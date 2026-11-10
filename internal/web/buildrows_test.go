package web

import (
	"context"
	"strings"
	"testing"

	"github.com/ndthuan/kipp/internal/store"
)

const sha40 = "0e36bd4c5d898f6ae5867341c261903edbaf9ff1"

// pushed writes a finished build of the job with the trigger the arguments
// say, and returns the job's page.
func (e *env) pushed(t *testing.T, b store.Build) string {
	t.Helper()
	const job = "smoke"
	b.Job, b.Status = job, store.Success
	must(t, e.st.CreateBuild(context.Background(), &b))
	_, page := get(t, e.login(t, "admin"), e.srv.URL+"/jobs/"+job)
	_, body, _ := strings.Cut(page, "<h2>Builds</h2>")
	return body
}

func TestAWebhookBuildsCommitIsSevenCharactersWithTheFullValueInTitle(t *testing.T) {
	e := site(t)
	page := e.pushed(t, store.Build{Trigger: "webhook", TriggerDetail: sha40, TriggerBranch: "main"})
	contains(t, page, `<code title="`+sha40+`">0e36bd4</code>`, "main")
	lacks(t, page, `<span class="sub">`+sha40)
}

func TestADetailThatIsNotACommitPrintsAsItDid(t *testing.T) {
	e := site(t)
	page := e.pushed(t, store.Build{Trigger: "upstream", TriggerDetail: "deploy #12"})
	contains(t, page, "deploy #12")
	lacks(t, page, "<code")
}

func TestABuildsTagIsShownInTheList(t *testing.T) {
	e := site(t)
	page := e.pushed(t, store.Build{Trigger: "webhook", TriggerDetail: sha40, TriggerTag: "v1.2.0"})
	contains(t, page, "v1.2.0")
}

func TestTheBuildPageHeaderShortensTheCommitToo(t *testing.T) {
	e := site(t)
	e.pushed(t, store.Build{Trigger: "webhook", TriggerDetail: sha40})
	_, page := get(t, e.login(t, "admin"), e.srv.URL+"/jobs/smoke/1")
	contains(t, page, `<code title="`+sha40+`">0e36bd4</code>`)
}

func TestAFailedRowInTheBuildListNamesAndLinksTheFailedStage(t *testing.T) {
	e := site(t)
	e.ended(t, "smoke", store.Failed,
		stage{"build", store.StageSuccess, nil},
		stage{"test", store.StageFailed, new(2)})
	e.ended(t, "smoke", store.Timeout, stage{"wait", store.StageCancelled, nil})
	e.ended(t, "smoke", store.Failed)
	_, page := get(t, e.login(t, "admin"), e.srv.URL+"/jobs/smoke")
	contains(t, page,
		`failed in <a class="link" href="/jobs/smoke/1#log-test">test</a>`,
		`timed out in <a class="link" href="/jobs/smoke/2#log-wait">wait</a>`,
		`failed in <a class="link" href="/jobs/smoke/3#log-prepare">prepare</a>`)
	lacks(t, page, "#log-build")
}

func TestAnUnstableRowHasNoReason(t *testing.T) {
	e := site(t)
	e.ended(t, "smoke", store.Unstable, stage{"test", store.StageSuccess, nil})
	_, page := get(t, e.login(t, "admin"), e.srv.URL+"/jobs/smoke")
	lacks(t, page, `class="sub why"`)
}

func TestTheWhyLineIsADirectChildOfItsCellSoTheStackedRowCanBreakItOut(t *testing.T) {
	e := site(t)
	e.ended(t, "smoke", store.Failed, stage{"test", store.StageFailed, new(2)})
	_, page := get(t, e.login(t, "admin"), e.srv.URL+"/jobs/smoke")
	contains(t, page, `</span><div class="sub why">failed in `)
	css, err := staticFS.ReadFile("static/kipp.css")
	must(t, err)
	if !strings.Contains(string(css), ".stacked td:has(> .why) { display: contents; }") ||
		!strings.Contains(string(css), ".stacked .why { flex: 0 0 100%; order: 1; }") {
		t.Fatal("the stacked layout does not give the why line a line of its own")
	}
}

func TestTheHeadingCrumbsShareOneBaselineSoAWrappedTitleKeepsTheGroupBesideItsFirstLine(t *testing.T) {
	e := site(t)
	e.ended(t, "smoke", store.Success)
	_, page := get(t, e.login(t, "admin"), e.srv.URL+"/jobs/smoke")
	contains(t, page, `<h1 class="crumbs"><a class="link-plain"`, `<span aria-hidden="true">/</span><span>smoke</span>`)
	css, err := staticFS.ReadFile("static/kipp.css")
	must(t, err)
	if !strings.Contains(string(css), ".crumbs { align-items: baseline;") {
		t.Fatal("the crumbs row is not aligned on a baseline")
	}
}
