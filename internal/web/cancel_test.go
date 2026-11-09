package web

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/store"
)

func TestARefreshedPartKeepsThePagesTitle(t *testing.T) {
	e := site(t)
	_, page := get(t, e.login(t, "admin"), e.srv.URL+"/queue")
	contains(t, page, `"ignoreTitle":true`)
}

func TestCancelAsksFirstAndLeadsBackToALocalNext(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	id := e.build(t, admin, "slow", nil)
	path := e.at(t, id)
	eventually(t, "the build runs", func() bool {
		b, err := e.st.Build(t.Context(), id)
		return err == nil && b.Status == store.Running
	})

	code, page := get(t, admin, e.srv.URL+path)
	eq(t, "build page", code, http.StatusOK)
	contains(t, page, `href="`+path+`/cancel"`)
	lacks(t, page, `action="`+path+`/cancel"`)

	code, page = get(t, admin, e.srv.URL+path+"/cancel?next=%2Fjobs")
	eq(t, "the question", code, http.StatusOK)
	contains(t, page, "Cancel slow #1?", "has run for", `action="`+path+`/cancel"`, `name="next" value="/jobs"`,
		`class="action action-danger">Cancel the build`, `href="/jobs">Keep running`)

	_, page = get(t, admin, e.srv.URL+path+"/cancel?next=https%3A%2F%2Fevil.example%2F")
	contains(t, page, `name="next" value="`+path+`"`, `href="`+path+`">Keep running`)
	lacks(t, page, "evil.example")

	// The queue links to the question, with the page it is on.
	req, err := http.NewRequest(http.MethodGet, e.srv.URL+"/queue", nil)
	must(t, err)
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Current-URL", e.srv.URL+"/jobs/slow?x=1")
	_, page = do(t, admin, req)
	contains(t, page, `href="`+path+`/cancel?next=%2fjobs%2fslow%3fx%3d1"`, ">Cancel...</a>")
	lacks(t, page, `action="`+path+`/cancel"`)

	res := post(t, admin, e.srv.URL+path+"/cancel", url.Values{"csrf": {e.token(t, admin)}, "next": {"https://evil.example/"}})
	eq(t, "an off-site next", res.StatusCode, http.StatusSeeOther)
	eq(t, "falls back to the build", res.Header.Get("Location"), path)
	eq(t, "status", e.wait(t, id).Status, store.Cancelled)

	id = e.build(t, admin, "slow", nil)
	path = e.at(t, id)
	res = post(t, admin, e.srv.URL+path+"/cancel", url.Values{"csrf": {e.token(t, admin)}, "next": {"/jobs/slow"}})
	eq(t, "a local next", res.StatusCode, http.StatusSeeOther)
	eq(t, "back where it came from", res.Header.Get("Location"), "/jobs/slow")
	eq(t, "status", e.wait(t, id).Status, store.Cancelled)

	code, _ = get(t, admin, e.srv.URL+path+"/cancel")
	eq(t, "a build that has ended needs no question", code, http.StatusSeeOther)
}

func TestCancelNeedsTheCSRFTokenAndTheRight(t *testing.T) {
	e := site(t)
	admin := e.login(t, "admin")
	id := e.build(t, admin, "slow", nil)
	path := e.at(t, id)
	res := post(t, admin, e.srv.URL+path+"/cancel", url.Values{})
	eq(t, "no token", res.StatusCode, http.StatusForbidden)
	b, err := e.st.Build(t.Context(), id)
	must(t, err)
	if b.Status.Terminal() {
		t.Fatalf("a post without the token cancelled the build: %s", b.Status)
	}
	bob := e.login(t, "bob")
	code, page := get(t, bob, e.srv.URL+path+"/cancel")
	eq(t, "bob is asked nothing", code, http.StatusForbidden)
	contains(t, page, "You are not allowed to do that.")
	res = post(t, admin, e.srv.URL+path+"/cancel", url.Values{"csrf": {e.token(t, admin)}})
	eq(t, "cancel", res.StatusCode, http.StatusSeeOther)
	e.wait(t, id)
}

func TestAKilledStageShowsNoExitMinusOne(t *testing.T) {
	minus, one := -1, 1
	for _, c := range []struct {
		name   string
		status store.Status
		stage  store.StageStatus
		code   *int
		want   string
	}{
		{"cancelled", store.Cancelled, store.StageCancelled, &minus, ""},
		{"timeout", store.Timeout, store.StageFailed, &minus, ""},
		{"lost", store.Lost, store.StageFailed, &minus, ""},
		{"failed with its own code", store.Failed, store.StageFailed, &one, "1"},
		{"no code", store.Running, store.StageRunning, nil, ""},
	} {
		st := &store.Stage{Status: c.stage, ExitCode: c.code}
		b := &store.Build{Status: c.status, StartedAt: new(time.Now())}
		eq(t, c.name+" in the table", exitText(st, b), c.want)
		_, exit := headline(&api.BuildView{Build: b, Stages: []*store.Stage{st}})
		eq(t, c.name+" in the headline", exit, c.want)
	}
}
