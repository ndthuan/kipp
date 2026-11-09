package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/cachesrv"
	"github.com/ndthuan/kipp/internal/store"
)

// page is one answer of a v1 list: the rows, and the cursor of the next
// page, none when it is the last.
func (e *env) page(t *testing.T, tok, path string) ([]map[string]any, string) {
	t.Helper()
	text, h := e.call(t, 200, tok, "GET", path, "")
	eq(t, path+" content type", h.Get("Content-Type"), "application/json")
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		t.Fatalf("%s is not an object: %q", path, text)
	}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"items", "next_cursor"}) {
		t.Fatalf("%s: a list has an items and a next_cursor and nothing else: %q", path, text)
	}
	var items []map[string]any
	if string(raw["items"]) == "null" || json.Unmarshal(raw["items"], &items) != nil {
		t.Fatalf("%s: items is an array, never null: %q", path, text)
	}
	var next *string
	must(t, json.Unmarshal(raw["next_cursor"], &next))
	if next == nil {
		return items, ""
	}
	return items, *next
}

// walk reads a list a page at a time from its first, limit rows each, and
// is every row it saw and how many pages that took. between, when given,
// runs after each page but the last.
func (e *env) walk(t *testing.T, tok, path string, limit int, between func()) (rows []map[string]any, pages int) {
	t.Helper()
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	next := ""
	for {
		url := path + sep + "limit=" + strconv.Itoa(limit)
		if next != "" {
			url += "&cursor=" + next
		}
		items, cursor := e.page(t, tok, url)
		rows, pages = append(rows, items...), pages+1
		if cursor == "" {
			return rows, pages
		}
		if between != nil {
			between()
		}
		next = cursor
	}
}

func numbersOf(rows []map[string]any, field string) []int {
	var out []int
	for _, r := range rows {
		out = append(out, int(r[field].(float64)))
	}
	return out
}

func TestTheBuildsOfAJobArePagedWithoutAGapOrARepeatWhileNewBuildsComeIn(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for range 120 {
		must(t, e.st.CreateBuild(ctx, &store.Build{Job: "gc", Status: store.Success, Trigger: "cli"}))
	}
	rows, pages := e.walk(t, e.admin, "/api/v1/jobs/gc/builds", 50, func() {
		must(t, e.st.CreateBuild(ctx, &store.Build{Job: "gc", Status: store.Success, Trigger: "cli"}))
	})
	eq(t, "pages of 50 over 120 builds", pages, 3)
	got := numbersOf(rows, "number")
	want := make([]int, 0, 120)
	for n := 120; n >= 1; n-- {
		want = append(want, n)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the walk saw %v, want 120 down to 1 with no builds added during it", got)
	}
	first, next := e.page(t, e.admin, "/api/v1/jobs/gc/builds")
	eq(t, "50 builds by default", len(first), 50)
	if next == "" {
		t.Fatal("a job with more than a page of builds has a next cursor")
	}
	last, none := e.page(t, e.admin, "/api/v1/jobs/gc/builds?limit=500")
	eq(t, "all of them under the most", len(last), 122)
	eq(t, "no cursor on the last page", none, "")
	empty, _ := e.page(t, e.admin, "/api/v1/jobs/smoke/builds")
	eq(t, "no builds is an empty array", len(empty), 0)
}

func TestAV1QueryThatDoesNotParseIsRefusedNotReadAsADefault(t *testing.T) {
	e := newEnv(t)
	must(t, e.st.CreateBuild(context.Background(), &store.Build{Job: "gc", Status: store.Success, Trigger: "cli"}))
	for _, q := range []string{"limit=abc", "limit=0", "limit=-1", "limit=1.5", "cursor=!!", "cursor=Zm9v", "cursor=" + cursorFor("audit", 3)} {
		e.asProblem(t, 400, "bad_input", e.admin, "GET", "/api/v1/jobs/gc/builds?"+q, "")
	}
	// A cursor that is no other list's, and one that is not a number.
	e.asProblem(t, 400, "bad_input", e.admin, "GET", "/api/v1/jobs/gc/builds?cursor="+cursorFor("builds", 1, 2), "")
	e.asProblem(t, 400, "bad_input", e.admin, "GET", "/api/v1/admin/audit?cursor="+cursorFor("builds", 3), "")
	// A build number a 32-bit int cannot hold, and one below the first, are
	// refused before they are converted.
	for _, n := range []int64{math.MaxInt32 + 1, math.MaxInt64, 0, -1, math.MinInt64} {
		e.asProblem(t, 400, "bad_input", e.admin, "GET", "/api/v1/jobs/gc/builds?cursor="+cursorFor("builds", n), "")
	}
	e.call(t, 200, e.admin, "GET", "/api/v1/jobs/gc/builds?cursor="+cursorFor("builds", math.MaxInt32), "")
	// before is not a v1 parameter, and unknown ones are ignored.
	e.call(t, 200, e.admin, "GET", "/api/v1/jobs/gc/builds?before=abc&color=blue", "")
}

func TestTrueAndFalseAreTheOnlyBooleans(t *testing.T) {
	e := newEnv(t)
	id := e.trigger(t, "smoke", "")
	for _, v := range []string{"1", "yes", "True", "0"} {
		e.asProblem(t, 400, "bad_input", e.admin, "GET", "/api/v1/builds/"+id+"?wait="+v, "")
	}
	eq(t, "wait=true", e.object(t, 200, e.admin, "GET", "/api/v1/builds/"+id+"?wait=true", "")["id"], any(mustFloat(id)))
	e.call(t, 200, e.admin, "GET", "/api/v1/builds/"+id+"?wait=false", "")
	e.call(t, 200, e.admin, "GET", "/api/v1/builds/"+id+"?wait=true", "")

	log := "/api/v1/builds/" + id + "/stages/s/log"
	for _, q := range []string{"download=1", "timestamps=1", "download=true&timestamps=2", "offset=abc", "offset=-1", "offset=1.5"} {
		e.asProblem(t, 400, "bad_input", e.admin, "GET", log+"?"+q, "")
	}
	e.call(t, 200, e.admin, "GET", log+"?download=true&timestamps=true", "")
	e.call(t, 200, e.admin, "GET", log+"?offset=0", "")
}

func mustFloat(s string) float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		panic(err)
	}
	return f
}

func TestAJobsActivityIsPagedNewestFirstAndItsDetailEmbedsTheFirstPage(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for i := range 60 {
		must(t, e.st.AddActivity(ctx, "smoke", fmt.Sprintf("line %02d", i), time.Now()))
	}
	rows, pages := e.walk(t, e.admin, "/api/v1/jobs/smoke/activity", 25, nil)
	eq(t, "pages of 25 over 60 lines", pages, 3)
	eq(t, "lines", len(rows), 60)
	eq(t, "newest first", rows[0]["line"], any("line 59"))
	eq(t, "oldest last", rows[59]["line"], any("line 00"))
	first, next := e.page(t, e.admin, "/api/v1/jobs/smoke/activity")
	eq(t, "50 lines by default", len(first), 50)
	if next == "" {
		t.Fatal("more than a page of lines has a next cursor")
	}

	// The detail holds its own first page, in the same shape, whose cursor
	// continues in the list.
	detail := e.object(t, 200, e.admin, "GET", "/api/v1/jobs/smoke", "")
	act := detail["activity"].(map[string]any)
	items := act["items"].([]any)
	eq(t, "lines in the detail", len(items), 20)
	cursor, ok := act["next_cursor"].(string)
	if !ok {
		t.Fatalf("the detail's activity has a next_cursor when there are more lines: %v", act)
	}
	more, _ := e.page(t, e.admin, "/api/v1/jobs/smoke/activity?cursor="+cursor+"&limit=1")
	eq(t, "the line after the detail's twentieth", more[0]["line"], any("line 39"))

	builds := detail["builds"].(map[string]any)
	if _, ok := builds["items"].([]any); !ok || builds["next_cursor"] != nil {
		t.Fatalf("the detail's builds are a list with no next page here: %v", builds)
	}
}

func TestADetailEmbedsTheFirstPageOfBuildsWithTheCursorOfTheNext(t *testing.T) {
	e := newEnv(t)
	for range 55 {
		must(t, e.st.CreateBuild(context.Background(), &store.Build{Job: "gc", Status: store.Success, Trigger: "cli"}))
	}
	detail := e.object(t, 200, e.admin, "GET", "/api/v1/jobs/gc", "")
	builds := detail["builds"].(map[string]any)
	eq(t, "builds in the detail", len(builds["items"].([]any)), 50)
	rest, none := e.page(t, e.admin, "/api/v1/jobs/gc/builds?cursor="+builds["next_cursor"].(string))
	eq(t, "the rest", len(rest), 5)
	eq(t, "and the end", none, "")
	eq(t, "which begins at the build after the detail's last", rest[0]["number"], any(5.0))
}

func TestTheAuditAndACachesEntriesAndABuildsResultsArePagedByCursor(t *testing.T) {
	e := newEnv(t, func(a *API) {
		a.deps.Cache = cachesrv.New(cachesrv.Options{Dir: t.TempDir(), Store: a.deps.Store})
	})
	ctx := context.Background()
	for i := range 7 {
		must(t, e.st.AppendAudit(ctx, store.Audit{At: time.Now(), Actor: "cli", Action: "reload", Target: "t" + itoa(int64(i))}))
		_, err := e.st.PutCacheEntry(ctx, &store.CacheEntry{Scope: "site", Namespace: "default", Key: "k" + itoa(int64(i)), Size: 5, SavedAt: time.Now()})
		must(t, err)
	}
	b := &store.Build{Job: "smoke", Status: store.Success, Trigger: "cli"}
	must(t, e.st.CreateBuild(ctx, b))
	var cases []store.Test
	for i := range 7 {
		cases = append(cases, store.Test{Stage: "s", Suite: "x", Name: "t" + itoa(int64(i)), Status: "passed"})
	}
	must(t, e.st.PutTests(ctx, b.ID, cases))

	audit, pages := e.walk(t, e.admin, "/api/v1/admin/audit", 3, nil)
	eq(t, "audit pages", pages, 3)
	eq(t, "audit rows", len(audit), 7)
	eq(t, "audit newest first", audit[0]["target"], any("t6"))

	entries, pages := e.walk(t, e.admin, "/api/v1/admin/cache", 3, nil)
	eq(t, "cache pages", pages, 3)
	var keys []string
	for _, c := range entries {
		keys = append(keys, c["key"].(string))
	}
	if !slices.Equal(keys, []string{"k0", "k1", "k2", "k3", "k4", "k5", "k6"}) {
		t.Fatalf("every entry once, in the order they are listed: %v", keys)
	}

	results, pages := e.walk(t, e.admin, "/api/v1/builds/"+itoa(b.ID)+"/results", 3, nil)
	eq(t, "result pages", pages, 3)
	eq(t, "results", len(results), 7)
	eq(t, "in the order stored", results[0]["name"], any("t0"))
	eq(t, "to the last", results[6]["name"], any("t6"))
}

func TestAMetricsHistoryIsPagedByCursorEvenWhereABuildReportedTwice(t *testing.T) {
	e := newEnv(t)
	metricJobs(t, e)
	for i := range 6 {
		b := measuredBuild(t, e, "site-smoke", "seconds", map[string]float64{"a": float64(i)})
		must(t, e.st.PutMetricHistory(context.Background(), b.ID, "site", "second", []store.Metric{{Name: "seconds", Unit: "s", Values: map[string]float64{"a": float64(i) + 0.5}}}))
	}
	path := "/api/v1/metrics/site/history?metric=seconds&item=a"
	rows, pages := e.walk(t, e.admin, path, 5, nil)
	eq(t, "pages of 5 over 12 rows", pages, 3)
	var values []float64
	for _, r := range rows {
		values = append(values, r["value"].(float64))
	}
	want := []float64{5.5, 5, 4.5, 4, 3.5, 3, 2.5, 2, 1.5, 1, 0.5, 0}
	if !slices.Equal(values, want) {
		t.Fatalf("every row once, the newest build first: %v", values)
	}
	first, _ := e.page(t, e.admin, path)
	eq(t, "100 by default", len(first), 12)
	e.asProblem(t, 400, "bad_input", e.admin, "GET", path+"&cursor="+cursorFor("history", 1), "")
}

func TestEveryOtherV1ListIsAnObjectWithItemsAndNoNextPage(t *testing.T) {
	e := newEnv(t)
	metricJobs(t, e)
	ctx := context.Background()
	must(t, e.st.PutRunner(ctx, store.Runner{Name: "box", Labels: []string{"linux"}, Executors: 2, Enabled: true, TokenHash: "h1"}))
	b := measuredBuild(t, e, "site-smoke", "seconds", map[string]float64{"a": 1, "b": 2})
	remember(t, e, "site", b, "seconds", map[string]float64{"a": 3})
	id := e.kept(t, "smoke", store.Success, "s", map[string]string{"a.txt": "x"})
	tok := e.withSite(t, store.Child{Parent: "site", Key: "main", Kind: "branch", Branch: "main", State: "live", LastSeenAt: time.Now()})
	for _, c := range []struct {
		tok, path string
		min       int
	}{
		{e.admin, "/api/v1/jobs", 3},
		{e.admin, "/api/v1/admin/users", 3},
		{e.admin, "/api/v1/admin/runners", 1},
		{e.admin, "/api/v1/admin/engines", 0},
		{e.admin, "/api/v1/metrics", 1},
		{e.admin, "/api/v1/metrics/site/remembered?metric=seconds", 1},
		{e.admin, "/api/v1/metrics/site/compare?metric=seconds&from=" + itoa(b.ID) + "&to=remembered", 2},
		{e.admin, "/api/v1/builds/" + itoa(id) + "/artifacts", 1},
		{tok, "/api/v1/jobs/site/children", 1},
	} {
		items, next := e.page(t, c.tok, c.path)
		if len(items) < c.min {
			t.Errorf("%s: %d items, want %d or more", c.path, len(items), c.min)
		}
		eq(t, c.path+" next cursor", next, "")
		// A list that is not paged ignores what pages the others.
		more, _ := e.page(t, c.tok, c.path+sepOf(c.path)+"limit=1&cursor="+cursorFor("builds", 1))
		eq(t, c.path+" with a limit", len(more), len(items))
	}
}

func sepOf(path string) string {
	if strings.Contains(path, "?") {
		return "&"
	}
	return "?"
}
