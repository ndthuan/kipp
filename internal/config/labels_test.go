package config

import (
	"slices"
	"strings"
	"testing"
)

func TestRunsOnIsOneLabelOrAListOfThem(t *testing.T) {
	for file, want := range map[string]Labels{
		"name: a\nstages:\n  s: {run: true, runs_on: linux}\n":             {"linux"},
		"name: a\nstages:\n  s: {run: true, runs_on: [linux, arm64]}\n":    {"linux", "arm64"},
		"name: a\nstages:\n  s: {run: true, runs_on: [linux, v1.2_x-y]}\n": {"linux", "v1.2_x-y"},
	} {
		j, err := ParseJob("a.yml", []byte(file))
		if err != nil {
			t.Fatalf("%q: %v", file, err)
		}
		if s := j.Stages[0]; !slices.Equal(s.RunsOn, want) || !s.Remote() {
			t.Fatalf("%q: runs_on %v, remote %v", file, s.RunsOn, s.Remote())
		}
	}
}

func TestALabelIsAPlainLowerCaseName(t *testing.T) {
	for file, want := range map[string]string{
		"name: a\nstages:\n  s: {run: true, runs_on: Linux}\n":          `label "Linux": lower-case letters, digits, dot, dash and underscore, starting with a letter or digit`,
		"name: a\nstages:\n  s: {run: true, runs_on: [linux, -x]}\n":    `label "-x"`,
		"name: a\nstages:\n  s: {run: true, runs_on: []}\n":             "runs_on is a label or a list of one or more labels",
		"name: a\nstages:\n  s: {run: true, runs_on: [[linux]]}\n":      "runs_on is a label or a list of one or more labels",
		"name: a\nstages:\n  s: {run: true, runs_on: [linux, linux]}\n": "runs_on names linux twice",
	} {
		_, err := ParseJob("a.yml", []byte(file))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%q: %v, want %q", file, err, want)
		}
	}
}

func TestAStageWithNoRunsOnRunsOnTheServer(t *testing.T) {
	j, err := ParseJob("a.yml", []byte("name: a\nstages:\n  s: {run: true}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if s := j.Stages[0]; s.Remote() || s.RunsOn != nil || s.Image != "" {
		t.Fatalf("%+v", s)
	}
}

// An image is run by an engine, and the file names no engine: it names
// labels, and an engine is chosen by them.
func TestAnImageNeedsRunsOn(t *testing.T) {
	_, err := ParseJob("a.yml", []byte("name: a\nstages:\n  quality-linux: {run: true, image: golang:1.27}\n"))
	want := "stage quality-linux: image needs runs_on: an image runs on an engine, and an engine is chosen by label"
	if err == nil || err.Error() != want {
		t.Fatalf("%v, want %q", err, want)
	}
	j, err := ParseJob("a.yml", []byte("name: a\nstages:\n  s: {run: true, runs_on: linux, image: golang:1.27}\n"))
	if err != nil || j.Stages[0].Image != "golang:1.27" {
		t.Fatalf("%v %+v", err, j)
	}
}

func TestAnImageIsOneWordOfText(t *testing.T) {
	for _, image := range []string{`"golang 1.27"`, `"golang:1.27\n"`} {
		_, err := ParseJob("a.yml", []byte("name: a\nstages:\n  s: {run: true, runs_on: linux, image: "+image+"}\n"))
		if err == nil || !strings.Contains(err.Error(), "stage s: image") {
			t.Fatalf("%s: %v", image, err)
		}
	}
}

func TestAStagesWorkdirIsAPathInsideItsWorkspace(t *testing.T) {
	for _, dir := range []string{"../x", "/abs", "a/../../b"} {
		_, err := ParseJob("a.yml", []byte("name: a\nstages:\n  x: {run: true, runs_on: linux, workdir: '"+dir+"'}\n"))
		want := "stage x: workdir is a path inside the stage's workspace, like src or services/api"
		if err == nil || err.Error() != want {
			t.Fatalf("%s: %v", dir, err)
		}
	}
	j, err := ParseJob("a.yml", []byte("name: a\nstages:\n  x: {run: true, runs_on: linux, workdir: src/api}\n"))
	if err != nil || j.Stages[0].Workdir != "src/api" {
		t.Fatalf("%v %+v", err, j)
	}
}

func TestASourceFilesStagesMayRunOnAnAgent(t *testing.T) {
	stages, err := stagesOf([]byte("stages:\n  s: {run: true, runs_on: [linux], image: alpine:3.22}\n"))
	if err != nil || !stages[0].Remote() || stages[0].Image != "alpine:3.22" {
		t.Fatalf("%v %+v", err, stages)
	}
}

func TestTheExamplesHoldAStageInAContainer(t *testing.T) {
	snap, err := Load("../../examples/jobs")
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range snap.Jobs {
		for _, s := range j.Stages {
			if s.Image != "" && s.RunsOn.Has("linux") {
				return
			}
		}
	}
	t.Fatal("no stage with runs_on and image in examples/jobs, so kipp check does not cover the keys")
}

func TestLabelsPrintCommaJoined(t *testing.T) {
	if s := (Labels{"linux", "arm64"}).String(); s != "linux,arm64" {
		t.Fatal(s)
	}
}

func TestLabelsOnACommandLineAreCommaSeparated(t *testing.T) {
	got, err := ParseLabels(" linux, arm64 ,docker")
	if err != nil || got.String() != "linux,arm64,docker" {
		t.Fatal(got, err)
	}
	for in, want := range map[string]string{"": "one or more labels", ",": "one or more labels", "linux,Mac": `label "Mac"`, "a,a": "label a twice"} {
		if _, err := ParseLabels(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v", in, err)
		}
	}
}
