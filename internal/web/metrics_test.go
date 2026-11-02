package web

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/store"
)

// measured is an ended build of the job that reported values of the
// metric into the store from the stage measure.
func measured(t *testing.T, e *env, job, st, metric string, values map[string]float64) *store.Build {
	t.Helper()
	ctx := context.Background()
	// Made as ended, so that the dispatcher never runs it.
	b := &store.Build{Job: job, Status: store.Success, Trigger: "cli"}
	must(t, e.st.CreateBuild(ctx, b))
	must(t, e.st.PutMetricHistory(ctx, b.ID, st, "measure", []store.Metric{{Name: metric, Unit: "s", Values: values}}))
	return b
}

func TestTheMetricsPageListsRememberedValuesOfAStoreTheUserMayView(t *testing.T) {
	e := site(t)
	ctx := context.Background()
	var last *store.Build
	for i := range 60 {
		last = measured(t, e, "smoke", "smoke", "seconds", map[string]float64{"lint": float64(i), "<b>unit</b>": 2})
	}
	must(t, e.st.Remember(ctx, "smoke", store.Origin{BuildID: last.ID, Job: "smoke", Number: last.Number}, time.Now(),
		[]store.Metric{{Name: "seconds", Unit: "s", Values: map[string]float64{"lint": 59, "<b>unit</b>": 2, "zero": 0.25}}}))
	must(t, e.st.Remember(ctx, "smoke", store.Origin{}, time.Now(), []store.Metric{{Name: "kipp.stage_seconds", Unit: "s", Values: map[string]float64{"lint": 1}}}))
	c := e.login(t, "alice")

	code, index := get(t, c, e.srv.URL+"/metrics")
	eq(t, "the index", code, http.StatusOK)
	contains(t, index, `<a class="link" href="/metrics/smoke">smoke</a>`, `href="/metrics/smoke?metric=seconds">seconds</a> <span class="sub">3</span>`,
		`<a href="/metrics" aria-current="page">Metrics</a>`)
	lacks(t, index, "orphaned")

	code, page := get(t, c, e.srv.URL+"/metrics/smoke")
	eq(t, "the store", code, http.StatusOK)
	selfContained(t, page)
	contains(t, page,
		// The metric that is not kipp's own is the one shown first.
		`<a class="pick" href="/metrics/smoke?metric=seconds" aria-current="page">seconds`,
		`<a class="pick" href="/metrics/smoke?metric=kipp.stage_seconds">kipp.stage_seconds`,
		`<table class="metric" data-sortable>`, `<th class="num" data-sort="number">Value</th>`,
		`<td class="item">&lt;b&gt;unit&lt;/b&gt;</td>`,
		`<td class="num" data-value="0.25">0.25</td>`,
		`<a class="link" href="/jobs/smoke/60">smoke #60</a>`,
		`/static/metrics.js?v=`,
		`<option value="remembered" selected>what the store remembers</option>`,
		`<option value="`+strconv.FormatInt(last.ID, 10)+`">smoke #60, success, `,
	)
	// The line has the last 50 values of lint, oldest first.
	var want []string
	for i := 10; i < 60; i++ {
		want = append(want, strconv.Itoa(i))
	}
	contains(t, page, `<td class="spark-cell" data-values="`+strings.Join(want, " ")+`"></td>`, `<td class="spark-cell" data-values=""></td>`)
	lacks(t, page, "<b>unit")

	_, kipps := get(t, c, e.srv.URL+"/metrics/smoke?metric=kipp.stage_seconds")
	contains(t, kipps, `aria-current="page">kipp.stage_seconds`, `<span class="sub">by hand</span>`)

	first := measured(t, e, "smoke", "smoke", "seconds", map[string]float64{"lint": 50, "new": 1})
	_, compared := get(t, c, e.srv.URL+"/metrics/smoke?metric=seconds&from="+strconv.FormatInt(first.ID, 10)+"&to=remembered")
	contains(t, compared, "4 items, the largest change first",
		`<td class="item">lint</td>`+"\n"+`<td class="num" data-value="50">50</td>`+"\n"+`<td class="num" data-value="59">59</td>`+"\n"+`<td class="num" data-value="9">&#43;9</td>`, // html/template writes + as an entity
		`<span class="sub">not in To</span>`, `<span class="sub">not in From</span>`,
		`<option value="`+strconv.FormatInt(first.ID, 10)+`" selected>`)
	if strings.Index(compared, `<td class="item">lint</td>`) > strings.Index(compared, `<td class="item">new</td>`) {
		t.Fatal("the change comes before the item on one side only")
	}
	_, bad := get(t, c, e.srv.URL+"/metrics/smoke?metric=seconds&from=x&to=remembered")
	contains(t, bad, `<p class="notice notice-error" role="alert">from is a build id or remembered.</p>`)
}

func TestTheMetricsPageIs404ForAStoreTheUserMayNotView(t *testing.T) {
	e := site(t)
	ctx := context.Background()
	must(t, e.st.Remember(ctx, "slow", store.Origin{}, time.Now(), []store.Metric{{Name: "seconds", Values: map[string]float64{"a": 1}}}))
	must(t, e.st.Remember(ctx, "old", store.Origin{}, time.Now(), []store.Metric{{Name: "seconds", Values: map[string]float64{"a": 1}}}))
	alice := e.login(t, "alice") // views smoke alone
	for _, path := range []string{"/metrics/slow", "/metrics/old", "/metrics/nothing", "/metrics/slow?metric=seconds&from=1&to=remembered"} {
		code, page := get(t, alice, e.srv.URL+path)
		eq(t, path, code, http.StatusNotFound)
		contains(t, page, "There is nothing here, or nothing you may see.")
	}
	_, index := get(t, alice, e.srv.URL+"/metrics")
	lacks(t, index, "/metrics/slow", "/metrics/old")
	contains(t, index, "No store you may see remembers anything yet.")

	code, _ := get(t, e.login(t, "bob"), e.srv.URL+"/metrics/slow")
	eq(t, "bob views slow", code, http.StatusOK)
	admin := e.login(t, "admin")
	_, index = get(t, admin, e.srv.URL+"/metrics")
	contains(t, index, `<a class="link" href="/metrics/old">old</a> <span class="tag">orphaned</span>`)
	code, page := get(t, admin, e.srv.URL+"/metrics/old")
	eq(t, "the admin views old", code, http.StatusOK)
	contains(t, page, "No loaded job names this store")
}

func TestAJobPageLinksToItsStore(t *testing.T) {
	e := site(t)
	_, page := get(t, e.login(t, "alice"), e.srv.URL+"/jobs/smoke")
	contains(t, page, `Its builds report metrics into the store <a class="link" href="/metrics/smoke">smoke</a>.`)
}
