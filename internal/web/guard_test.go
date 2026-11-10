package web

import (
	"context"
	"html"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

// add loads the job files beside the site's jobs.
func (e *env) add(t *testing.T, files map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		must(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
	}
	more, err := config.Load(dir)
	must(t, err)
	snap := e.d.Snapshot()
	jobs := maps.Clone(snap.Jobs)
	maps.Copy(jobs, more.Jobs)
	e.d.SetSnapshot(&config.Snapshot{Jobs: jobs, Names: slices.Sorted(maps.Keys(jobs))})
}

func TestTheGuardLineIsInTheLogAndOnTheBuildPage(t *testing.T) {
	e := site(t)
	e.add(t, map[string]string{"site-smoke.yml": `name: site-smoke
metrics: {store: site}
stages:
  measure:
    metrics:
      file: m.json
      guard:
        - {metric: coverage, item: total, no_lower: true, tolerance: 0.01, on_fall: failed}
        - {metric: coverage, item: lines, no_lower: true}
    run: |
      echo '{"metrics": [{"name": "coverage", "unit": "%", "values": {"total": 86.90, "lines": 12}}]}' > m.json
`})
	must(t, e.st.Remember(context.Background(), "site", store.Origin{BuildID: 99, Job: "site-deploy", Number: 123}, time.Now(),
		[]store.Metric{{Name: "coverage", Unit: "%", Values: map[string]float64{"total": 87.19}}}))
	c := e.login(t, "admin")
	id := e.build(t, c, "site-smoke", nil)
	b := e.wait(t, id)
	eq(t, "status", b.Status, store.Failed)
	line := "kipp: metrics guard: coverage total 87.19 -> 86.90 (-0.29), remembered from site-deploy #123, tolerance 0.01: failed"
	held := "kipp: metrics guard: coverage lines no baseline -> 12.00, tolerance 0: held"
	data, err := os.ReadFile(e.d.LogPath(b.Job, b.Number, "measure"))
	must(t, err)
	if got := string(data); !strings.HasSuffix(got, line+"\n"+held+"\n") {
		t.Fatalf("the log: %q", got)
	}
	_, page := get(t, c, e.srv.URL+e.at(t, id))
	contains(t, page, `<details class="guards" id="guards" open>`, `<summary><h2>Guards</h2></summary>`,
		`<td class="lead" data-label="Stage">measure</td>
<td class="lead" data-label="Result"><span class="status failed">failed</span></td>
<td data-label="Guard"><code>`+html.EscapeString(line)+`</code></td>`,
		`<td class="lead" data-label="Result"><span class="status success">held</span></td>
<td data-label="Guard"><code>`+html.EscapeString(held)+`</code></td>`)
	// A build with no guard has no such section.
	other := e.build(t, c, "smoke", nil)
	e.wait(t, other)
	_, page = get(t, c, e.srv.URL+e.at(t, other))
	lacks(t, page, `id="guards"`)
}
