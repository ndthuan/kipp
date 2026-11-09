package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ndthuan/kipp/internal/runnerhub"
	"github.com/ndthuan/kipp/internal/store"
)

// artifactsOf is what the build's stages kept, as stage/path: content.
func (h *harness) artifactsOf(t *testing.T, b *store.Build) map[string]string {
	t.Helper()
	rows, err := h.st.Artifacts(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, r := range rows {
		data, err := os.ReadFile(filepath.Join(h.d.StageArtifactsDir(b.Job, b.Number, r.Stage), filepath.FromSlash(r.Path)))
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(data)) != r.Size || len(r.SHA256) != 64 {
			t.Fatalf("%+v holds %d bytes", r, len(data))
		}
		out[r.Stage+"/"+r.Path] = string(data)
	}
	return out
}

func TestAStageKeepsTheFilesItsGlobsMatch(t *testing.T) {
	h := newHarness(t, map[string]string{"site-smoke": `name: site-smoke
stages:
  report:
    workdir: src
    artifacts: {paths: ["dist/*.txt", coverage, missing.txt]}
    run: |
      mkdir -p dist coverage/html
      echo one > dist/a.txt; echo two > dist/b.log; echo three > coverage/html/index.html
`})
	b := h.wait(t, h.trigger(t, "site-smoke").ID)
	if b.Status != store.Success {
		t.Fatalf("%s: %s", b.Status, h.log(t, b, "report"))
	}
	got := h.artifactsOf(t, b)
	want := map[string]string{"report/dist/a.txt": "one\n", "report/coverage/html/index.html": "three\n"}
	if len(got) != len(want) {
		t.Fatalf("%v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s: %q, want %q (%v)", k, got[k], v, got)
		}
	}
}

func TestAFailedStageKeepsItsArtifactsToo(t *testing.T) {
	h := newHarness(t, map[string]string{"a": "name: a\nstages:\n  s: {artifacts: {paths: [report.html]}, run: 'echo \"<p>no</p>\" > report.html; exit 3'}\n"})
	b := h.wait(t, h.trigger(t, "a").ID)
	if b.Status != store.Failed {
		t.Fatalf("%s", b.Status)
	}
	if got := h.artifactsOf(t, b); got["s/report.html"] != "<p>no</p>\n" {
		t.Fatalf("%v", got)
	}
}

func TestOverMaxSizeFailsTheStageAndKeepsNone(t *testing.T) {
	h := newHarness(t, map[string]string{"a": "name: a\nstages:\n  s: {artifacts: {paths: ['*.bin'], max_size: 10}, run: 'printf 123456 > a.bin; printf 12345 > b.bin'}\n"})
	b := h.wait(t, h.trigger(t, "a").ID)
	log := h.log(t, b, "s")
	if b.Status != store.Failed || !strings.Contains(log, "kipp: artifacts: 11 bytes over the stage's max_size of 10\n") {
		t.Fatalf("%s: %q", b.Status, log)
	}
	if got := h.artifactsOf(t, b); len(got) != 0 {
		t.Fatalf("kept %v", got)
	}
	if _, err := os.Stat(h.d.StageArtifactsDir("a", 1, "s")); !os.IsNotExist(err) {
		t.Fatalf("the stage's artifacts directory is there: %v", err)
	}
}

func TestALinkOutOfTheStageFailsItAndKeepsNone(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("not the stage's"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, map[string]string{"a": "name: a\nstages:\n  s: {artifacts: {paths: ['out/*']}, run: 'mkdir out; echo ok > out/a.txt; ln -s " + outside + " out/b.txt'}\n"})
	b := h.wait(t, h.trigger(t, "a").ID)
	if log := h.log(t, b, "s"); b.Status != store.Failed || !strings.Contains(log, "kipp: artifacts: out/b.txt leads outside the stage's directory") {
		t.Fatalf("%s: %q", b.Status, log)
	}
	if got := h.artifactsOf(t, b); len(got) != 0 {
		t.Fatalf("kept %v", got)
	}
}

func TestAStageOnAnRunnerSendsItsArtifactsBack(t *testing.T) {
	ag := newRunners(t, runnerhub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
	ag.runBox(t.TempDir())
	h := newHarness(t, map[string]string{"a": `name: a
stages:
  s:
    runs_on: linux
    workdir: src
    artifacts: {paths: ["out/*"]}
    run: mkdir out; echo far > out/x.html; exit 4
  big:
    runs_on: linux
    artifacts: {paths: ["*.bin"], max_size: 3}
    run: printf 1234 > a.bin
`}, ag.opts)
	b := h.wait(t, h.trigger(t, "a").ID)
	got := h.artifactsOf(t, b)
	if b.Status != store.Failed || got["s/out/x.html"] != "far\n" || len(got) != 1 {
		t.Fatalf("%s %v: %s", b.Status, got, h.log(t, b, "s"))
	}
	if log := h.log(t, b, "big"); h.stages(t, b.ID)["big"].Status != store.StageFailed || !strings.Contains(log, "kipp: artifacts: 4 bytes over the stage's max_size of 3") {
		t.Fatalf("%q", log)
	}
}

// inputsJob builds in one stage and reads what it kept in the next,
// which runs where place says (a runs_on line, or nothing for the server).
func inputsJob(place string) string {
	return `name: site-deploy
stages:
  build:
    workdir: src
    artifacts: {paths: [dist]}
    run: mkdir -p dist/css; echo app > dist/app.js; echo css > dist/css/site.css
  empty: {artifacts: {paths: [nothing]}, run: 'true'}
  plain: {run: echo not kept > plain.txt}
  deploy:
    needs: [build, empty, plain]
    artifacts_from: [build, empty]
` + place + `
    run: |
      cd "$KIPP_WORKSPACE/.kipp/artifacts"
      echo "app=$(cat build/dist/app.js) css=$(cat build/dist/css/site.css)"
      echo "dirs: $(ls -A | tr '\n' ' ')"
      echo "empty holds: [$(ls -A empty)]"
`
}

func TestAStageOnTheServerFindsItsInputsUnderKippArtifacts(t *testing.T) {
	h := newHarness(t, map[string]string{"site-deploy": inputsJob("")})
	b := h.wait(t, h.trigger(t, "site-deploy").ID)
	log := h.log(t, b, "deploy")
	if b.Status != store.Success || !strings.Contains(log, "app=app css=css\ndirs: build empty \nempty holds: []\n") {
		t.Fatalf("%s: %q", b.Status, log)
	}
}

// The same job file finds the same files at the same path on a runner.
func TestAStageOnAnRunnerFindsTheSameInputsAtTheSamePath(t *testing.T) {
	ag := newRunners(t, runnerhub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
	ag.runBox(t.TempDir())
	h := newHarness(t, map[string]string{"site-deploy": inputsJob("    runs_on: linux")}, ag.opts)
	b := h.wait(t, h.trigger(t, "site-deploy").ID)
	log := h.log(t, b, "deploy")
	if b.Status != store.Success || !strings.Contains(log, "app=app css=css\ndirs: build empty \nempty holds: []\n") {
		t.Fatalf("%s: %q", b.Status, log)
	}
}

func TestAStageWithoutArtifactsFromReceivesNothing(t *testing.T) {
	h := newHarness(t, map[string]string{"a": `name: a
stages:
  build: {artifacts: {paths: [out.txt]}, run: echo kept > out.txt}
  next: {needs: [build], run: 'ls -A "$KIPP_WORKSPACE/.kipp/artifacts" 2>&1 || echo nothing given'}
`})
	b := h.wait(t, h.trigger(t, "a").ID)
	if log := h.log(t, b, "next"); b.Status != store.Success || !strings.Contains(log, "nothing given") {
		t.Fatalf("%s: %q", b.Status, log)
	}
}

func TestAnInputStageThatKeptNothingGivesAnEmptyDirectory(t *testing.T) {
	h := newHarness(t, map[string]string{"a": `name: a
stages:
  build: {artifacts: {paths: ["*.missing"]}, run: 'true'}
  next: {needs: [build], artifacts_from: [build], run: 'test -d "$KIPP_WORKSPACE/.kipp/artifacts/build" && echo "[$(ls -A "$KIPP_WORKSPACE/.kipp/artifacts/build")]"'}
`})
	b := h.wait(t, h.trigger(t, "a").ID)
	if log := h.log(t, b, "next"); b.Status != store.Success || log != "[]\n" {
		t.Fatalf("%s: %q", b.Status, log)
	}
}
