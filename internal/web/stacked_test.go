package web

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/ndthuan/kipp/internal/store"
)

var (
	stackedTable = regexp.MustCompile(`(?s)<table class="[^"]*\bstacked\b[^"]*">(.*?)</table>`)
	headCell     = regexp.MustCompile(`(?s)<th[^>]*>(.*?)</th>`)
	bodyCell     = regexp.MustCompile(`<td( [^>]*)?>`)
	cellLabel    = regexp.MustCompile(`data-label="([^"]*)"`)
	markup       = regexp.MustCompile(`<[^>]+>`)
)

// A table that stacks into blocks on a phone shows each cell under its
// column's name, which only the cell's own data-label can give it once the
// header row is hidden: so every cell of such a table carries the label of
// its column, in order, and the first line of a row is named by `lead`.
func TestEveryCellOfAStackedTableNamesItsColumn(t *testing.T) {
	e := site(t)
	e.lockJobs(t)
	c := e.login(t, "admin")
	e.finished(t, "smoke", "fsss")
	e.finished(t, "slow", "ssssfssssfsssssfsssf")
	e.finished(t, "quoted", "ssssssfffff")
	e.build(t, c, "hold", nil)
	e.queued(t, 1, 0)
	e.build(t, c, "wait", nil)
	e.queued(t, 1, 1)
	id := e.build(t, c, "nightly", nil)
	eq(t, "status", e.wait(t, id).Status, store.Failed)
	eventually(t, "the tests are stored", func() bool {
		tests, err := e.st.Tests(context.Background(), id)
		return err == nil && len(tests) == 3
	})

	// How many stacked tables each page has at least: the overview's
	// failing, flaky and now; a job's builds; a build's
	// stages are one only with several, so its results and tests stand in.
	pages := map[string]int{"/": 3, "/queue": 1, "/jobs": 1, "/jobs/smoke": 1, "/jobs/nightly": 2, e.at(t, id): 1}
	for path, least := range pages {
		code, page := get(t, c, e.srv.URL+path)
		eq(t, path, code, http.StatusOK)
		if n := len(stackedTable.FindAllString(page, -1)); n < least {
			t.Errorf("%s has %d stacked tables, want %d", path, n, least)
		}
		stackedLabels(t, path, page)
	}
}

// stackedLabels checks every stacked table of page: its th names, in order,
// are the labels of the td in each row.
func stackedLabels(t *testing.T, what, page string) {
	t.Helper()
	for _, m := range stackedTable.FindAllStringSubmatch(page, -1) {
		head, body, _ := strings.Cut(m[1], "</thead>")
		var names []string
		for _, h := range headCell.FindAllStringSubmatch(head, -1) {
			names = append(names, strings.TrimSpace(markup.ReplaceAllString(h[1], "")))
		}
		cells := bodyCell.FindAllStringSubmatch(body, -1)
		if len(names) == 0 || len(cells)%len(names) != 0 {
			t.Fatalf("%s: %d cells under %v", what, len(cells), names)
		}
		for i, cell := range cells {
			label := cellLabel.FindStringSubmatch(cell[1])
			if label == nil || label[1] != names[i%len(names)] {
				t.Errorf("%s: cell %d of %v is %q, want the label %q", what, i, names, cell[0], names[i%len(names)])
			}
		}
	}
}
