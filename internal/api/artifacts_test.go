package api

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/store"
)

// kept gives the job a build that ended with status, whose stage kept
// the files given, and is the build's id.
func (e *env) kept(t *testing.T, job string, status store.Status, stage string, files map[string]string) int64 {
	t.Helper()
	ctx := context.Background()
	b := &store.Build{Job: job, Status: status, Trigger: "cli", QueuedAt: time.Now()}
	must(t, e.st.CreateBuild(ctx, b))
	var rows []store.Artifact
	dir := e.api.deps.Dispatch.StageArtifactsDir(job, b.Number, stage)
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		must(t, os.MkdirAll(filepath.Dir(p), 0o750))
		must(t, os.WriteFile(p, []byte(body), 0o600))
		rows = append(rows, store.Artifact{Stage: stage, Path: name, Size: int64(len(body)), SHA256: strings.Repeat("a", 64)})
	}
	must(t, e.st.PutArtifacts(ctx, b.ID, stage, rows))
	return b.ID
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

// An HTML report opened as a page of the console would run with its
// origin: every download is an attachment, never sniffed into a type a
// browser renders, and sandboxed.
func TestADownloadIsAnAttachmentWithNosniffAndSandbox(t *testing.T) {
	e := newEnv(t)
	id := e.kept(t, "smoke", store.Failed, "s", map[string]string{"report/x.html": "<script>alert(1)</script>", "a b.txt": "spaced"})
	base := "/api/v1/builds/" + itoa64(id) + "/stages/s/artifacts"
	body, h := e.call(t, 200, e.viewer, "GET", base+"/report/x.html", "")
	eq(t, "body", body, "<script>alert(1)</script>")
	eq(t, "type", h.Get("Content-Type"), "application/octet-stream")
	eq(t, "disposition", h.Get("Content-Disposition"), "attachment; filename=x.html")
	eq(t, "nosniff", h.Get("X-Content-Type-Options"), "nosniff")
	eq(t, "csp", h.Get("Content-Security-Policy"), "sandbox")
	body, h = e.call(t, 200, e.viewer, "GET", base+"/a%20b.txt", "")
	eq(t, "spaced", body, "spaced")
	eq(t, "spaced disposition", h.Get("Content-Disposition"), `attachment; filename="a b.txt"`)
	_, h = e.call(t, 200, e.viewer, "GET", base+".zip", "")
	eq(t, "zip type", h.Get("Content-Type"), "application/octet-stream")
	eq(t, "zip csp", h.Get("Content-Security-Policy"), "sandbox")
	// Only what the stage kept is served: nothing beside it, nothing above.
	for _, p := range []string{"s/artifacts/report/y.html", "s/artifacts/../../../db", "s/artifacts/report/..%2f..%2f..%2fdb", "t/artifacts/x.html", "s/artifacts/"} {
		if code, _, _ := e.raw(t, e.viewer, "GET", "/api/v1/builds/"+itoa64(id)+"/stages/"+p, ""); code != 404 && code != 301 {
			t.Fatalf("%s: %d", p, code)
		}
	}
	list, _ := e.page(t, e.viewer, "/api/v1/builds/"+itoa64(id)+"/artifacts")
	if len(list) != 1 || list[0]["stage"] != "s" || list[0]["zip"] != "/api/v1/builds/"+itoa64(id)+"/stages/s/artifacts.zip" {
		t.Fatalf("%v", list)
	}
	files := list[0]["files"].([]any)
	first := files[0].(map[string]any)
	if len(files) != 2 || first["path"] != "a b.txt" || first["size"] != float64(6) || first["url"] != "/api/v1/builds/"+itoa64(id)+"/stages/s/artifacts/a%20b.txt" {
		t.Fatalf("%v", files)
	}
}

func TestTheZipHoldsTheStagesFiles(t *testing.T) {
	e := newEnv(t)
	id := e.kept(t, "smoke", store.Success, "s", map[string]string{"dist/app.js": "js", "dist/css/app.css": "css"})
	e.kept(t, "smoke", store.Success, "other", map[string]string{"no.txt": "not this stage's"})
	body, h := e.call(t, 200, e.viewer, "GET", "/api/v1/builds/"+itoa64(id)+"/stages/s/artifacts.zip", "")
	eq(t, "disposition", h.Get("Content-Disposition"), "attachment; filename=smoke-1-s.zip")
	zr, err := zip.NewReader(bytes.NewReader([]byte(body)), int64(len(body)))
	must(t, err)
	got := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		must(t, err)
		data, _ := io.ReadAll(rc)
		_ = rc.Close()
		got[f.Name] = string(data)
	}
	if len(got) != 2 || got["dist/app.js"] != "js" || got["dist/css/app.css"] != "css" {
		t.Fatalf("%v", got)
	}
	e.call(t, 404, e.viewer, "GET", "/api/v1/builds/"+itoa64(id)+"/stages/nothing/artifacts.zip", "")
}

func TestLatestRedirectsToTheNewestSuccessfulBuild(t *testing.T) {
	e := newEnv(t)
	first := e.kept(t, "smoke", store.Success, "s", map[string]string{"r.txt": "1"})
	second := e.kept(t, "smoke", store.Success, "s", map[string]string{"other.txt": "2"})
	failed := e.kept(t, "smoke", store.Failed, "s", map[string]string{"r.txt": "3"})
	e.kept(t, "smoke", store.Success, "t", map[string]string{"r.txt": "4"})
	noFollow := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	where := func(query string) (int, string) {
		t.Helper()
		res, err := noFollow.Do(e.request(context.Background(), t, e.viewer, "GET", "/api/v1/jobs/smoke/artifacts/latest?"+query, nil))
		must(t, err)
		_ = res.Body.Close()
		return res.StatusCode, res.Header.Get("Location")
	}
	for query, want := range map[string]string{
		"stage=s":                           "/api/v1/builds/" + itoa64(second) + "/stages/s/artifacts",
		"stage=s&path=r.txt":                "/api/v1/builds/" + itoa64(first) + "/stages/s/artifacts/r.txt",
		"stage=s&status=failed":             "/api/v1/builds/" + itoa64(failed) + "/stages/s/artifacts",
		"stage=s&status=success,failed":     "/api/v1/builds/" + itoa64(failed) + "/stages/s/artifacts",
		"stage=s&status=success&path=r.txt": "/api/v1/builds/" + itoa64(first) + "/stages/s/artifacts/r.txt",
	} {
		code, loc := where(query)
		if code != http.StatusFound || loc != want {
			t.Fatalf("%s: %d %q, want %q", query, code, loc, want)
		}
	}
	for query, want := range map[string]int{"stage=none": 404, "stage=s&status=lost": 404, "stage=s&status=running": 400, "": 400, "stage=s&path=../db": 400} {
		if code, _ := where(query); code != want {
			t.Fatalf("%s: %d, want %d", query, code, want)
		}
	}
	text, _ := e.call(t, 200, e.viewer, "GET", "/api/v1/jobs/smoke/artifacts/latest?stage=s&path=r.txt", "")
	eq(t, "followed", text, "1")
}

func TestArtifactsNeedViewOnTheJob(t *testing.T) {
	e := newEnv(t)
	id := e.kept(t, "gc", store.Success, "s", map[string]string{"r.txt": "hidden"})
	for _, p := range []string{"/api/v1/builds/" + itoa64(id) + "/artifacts", "/api/v1/builds/" + itoa64(id) + "/stages/s/artifacts/r.txt", "/api/v1/builds/" + itoa64(id) + "/stages/s/artifacts.zip", "/api/v1/jobs/gc/artifacts/latest?stage=s"} {
		e.call(t, 404, e.viewer, "GET", p, "")
		e.call(t, 401, "", "GET", p, "")
		e.call(t, 200, e.admin, "GET", p, "")
	}
}

// Under /api/v1 the files of a stage are below it, and the paths the build
// had them under are gone.
func TestV1ServesAStagesArtifactsBelowTheStage(t *testing.T) {
	e := newEnv(t)
	id := itoa64(e.kept(t, "smoke", store.Success, "s", map[string]string{"dist/app.js": "js", "a b.txt": "spaced"}))
	base := "/api/v1/builds/" + id + "/stages/s/artifacts"

	// The stage alone: one object, not a list.
	body := e.object(t, 200, e.viewer, "GET", base, "")
	eq[any](t, "stage", body["stage"], "s")
	eq[any](t, "zip", body["zip"], base+".zip")
	files := body["files"].([]any)
	if len(files) != 2 || files[1].(map[string]any)["url"] != base+"/dist/app.js" {
		t.Fatalf("%v", files)
	}

	// A file, a zip, and a file's part.
	text, h := e.call(t, 200, e.viewer, "GET", base+"/dist/app.js", "")
	eq(t, "file", text, "js")
	eq(t, "file disposition", h.Get("Content-Disposition"), "attachment; filename=app.js")
	text, _ = e.call(t, 206, e.viewer, "GET", base+"/dist/app.js", "", "Range", "bytes=1-1")
	eq(t, "part", text, "s")
	zipped, h := e.call(t, 200, e.viewer, "GET", base+".zip", "")
	eq(t, "zip disposition", h.Get("Content-Disposition"), "attachment; filename=smoke-1-s.zip")
	zr, err := zip.NewReader(bytes.NewReader([]byte(zipped)), int64(len(zipped)))
	must(t, err)
	eq(t, "zipped files", len(zr.File), 2)

	// What the stage did not keep, and a stage that kept nothing.
	for _, p := range []string{base + "/dist/none.js", "/api/v1/builds/" + id + "/stages/other/artifacts", "/api/v1/builds/" + id + "/stages/other/artifacts.zip", base + "/"} {
		if code, _, _ := e.raw(t, e.viewer, "GET", p, ""); code != 404 && code != 301 {
			t.Fatalf("%s: %d", p, code)
		}
	}

	// The files are not reached below the build.
	for _, p := range []string{"/api/v1/builds/" + id + "/artifacts/s.zip", "/api/v1/builds/" + id + "/artifacts/s/dist/app.js"} {
		e.asProblem(t, 404, "not_found", e.viewer, "GET", p, "")
	}
}

func TestV1ArtifactsOfAHiddenJobNeedView(t *testing.T) {
	e := newEnv(t)
	id := itoa64(e.kept(t, "gc", store.Success, "s", map[string]string{"r.txt": "hidden"}))
	for _, p := range []string{"/api/v1/builds/" + id + "/stages/s/artifacts", "/api/v1/builds/" + id + "/stages/s/artifacts/r.txt", "/api/v1/builds/" + id + "/stages/s/artifacts.zip"} {
		e.call(t, 404, e.viewer, "GET", p, "")
		e.call(t, 401, "", "GET", p, "")
		e.call(t, 200, e.admin, "GET", p, "")
	}
}

func TestV1LatestRedirectsToTheStage(t *testing.T) {
	e := newEnv(t)
	id := itoa64(e.kept(t, "smoke", store.Success, "s", map[string]string{"r.txt": "1"}))
	noFollow := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for query, want := range map[string]string{
		"stage=s":            "/api/v1/builds/" + id + "/stages/s/artifacts",
		"stage=s&path=r.txt": "/api/v1/builds/" + id + "/stages/s/artifacts/r.txt",
	} {
		res, err := noFollow.Do(e.request(context.Background(), t, e.viewer, "GET", "/api/v1/jobs/smoke/artifacts/latest?"+query, nil))
		must(t, err)
		_ = res.Body.Close()
		eq(t, query+": status", res.StatusCode, http.StatusFound)
		eq(t, query+": location", res.Header.Get("Location"), want)
	}
	text, _ := e.call(t, 200, e.viewer, "GET", "/api/v1/jobs/smoke/artifacts/latest?stage=s&path=r.txt", "")
	eq(t, "followed", text, "1")
}
