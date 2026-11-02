package web

import (
	"strconv"
	"strings"
	"testing"

	"github.com/ndthuan/kipp/internal/store"
)

// The build page lists what each stage kept, each file a download, and a
// zip of each stage's.
func TestTheBuildPageListsTheArtifactsAsDownloads(t *testing.T) {
	e := site(t)
	e.add(t, map[string]string{"site-smoke.yml": `name: site-smoke
stages:
  report: {artifacts: {paths: [out]}, run: 'mkdir -p out/css; echo "<p>x</p>" > out/x.html; echo b > "out/css/a b.css"'}
  plain: {run: 'true'}
`})
	c := e.login(t, "admin")
	id := e.build(t, c, "site-smoke", nil)
	b := e.wait(t, id)
	eq(t, "status", b.Status, store.Success)
	_, page := get(t, c, e.srv.URL+e.at(t, id))
	i := strings.Index(page, `<details class="artifacts" id="artifacts" open>`)
	if i < 0 {
		t.Fatalf("no Artifacts section: %s", page)
	}
	section, _, _ := strings.Cut(page[i:], "</details>")
	contains(t, section, `<summary><h2>Artifacts</h2></summary>`,
		`report <a class="link sub" href="/api/v1/builds/`+strconv.FormatInt(id, 10)+`/stages/report/artifacts.zip" download>zip</a>`,
		`href="/api/v1/builds/`+strconv.FormatInt(id, 10)+`/stages/report/artifacts/out/css/a%20b.css" download><code>out/css/a b.css</code>`,
		`href="/api/v1/builds/`+strconv.FormatInt(id, 10)+`/stages/report/artifacts/out/x.html" download><code>out/x.html</code>`,
		`<td class="num">2 B</td>`, `<td class="num">9 B</td>`)
	if strings.Contains(section, "plain") {
		t.Fatal("a stage that keeps nothing is listed")
	}
}
