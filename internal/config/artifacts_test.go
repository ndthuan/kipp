package config

import (
	"strings"
	"testing"
)

func TestASizeIsReadWithItsUnitAndWrittenBack(t *testing.T) {
	for in, want := range map[string]ByteSize{"500": 500, "64KiB": 64 << 10, "1.5GB": 1500 * 1000 * 1000, "10 GiB": 10 << 30, "2mb": 2e6, "1B": 1} {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Fatalf("%q: %d %v, want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0", "-1", "1XB", "GB", "1e3", "0.1"} {
		if _, err := ParseSize(in); err == nil {
			t.Fatalf("%q was taken", in)
		}
	}
	for size, want := range map[ByteSize]string{GiB: "1GiB", 50 * GB: "50GB", 1500: "1500", 3 * MiB: "3MiB"} {
		if got := size.String(); got != want {
			t.Fatalf("%d: %q, want %q", size, got, want)
		}
	}
}

func TestArtifactsTakeGlobsAndAMaxSizeUpTo10GiB(t *testing.T) {
	job, err := ParseJob("a.yml", []byte("name: a\nstages:\n  s: {artifacts: {paths: [\"dist/*\", report.html]}, run: 'true'}\n  t: {artifacts: {paths: [x], max_size: 10GiB}, run: 'true'}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := job.Stages[0].Artifacts.Max(); got != int64(GiB) {
		t.Fatalf("default max %d", got)
	}
	if got := job.Stages[1].Artifacts.Max(); got != 10<<30 {
		t.Fatalf("max %d", got)
	}
	for yml, why := range map[string]string{
		"{artifacts: {paths: []}, run: 'true'}":                   "one or more globs",
		"{artifacts: {paths: [x], max_size: 11GiB}, run: 'true'}": "over the most a stage keeps, 10GiB",
		"{artifacts: {paths: [x], max_size: 0}, run: 'true'}":     "a size such as",
		"{artifacts: {paths: [\"[\"]}, run: 'true'}":              "not a pattern",
		"{artifacts: {paths: [x], keep: 1}, run: 'true'}":         "field keep not found",
	} {
		_, err := ParseJob("a.yml", []byte("name: a\nstages:\n  s: "+yml+"\n"))
		if err == nil || !strings.Contains(err.Error(), why) {
			t.Fatalf("%s: %v, want %q", yml, err, why)
		}
	}
}

func TestArtifactsFromNamesOnlyStagesItNeeds(t *testing.T) {
	job, err := ParseJob("a.yml", []byte(`name: a
stages:
  build: {artifacts: {paths: [dist]}, run: 'true'}
  test: {needs: [build], run: 'true'}
  deploy: {needs: [test], artifacts_from: [build, test], run: 'true'}
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := job.Stages[2].ArtifactsFrom; len(got) != 2 || got[0] != "build" {
		t.Fatalf("%v", got)
	}
	for yml, why := range map[string]string{
		"  side: {run: 'true'}\n  s: {needs: [build], artifacts_from: [side], run: 'true'}":               `"side" is not a stage it needs`,
		"  s: {needs: [build], artifacts_from: [s], run: 'true'}":                                         `"s" is not a stage it needs`,
		"  s: {needs: [build], artifacts_from: [later], run: 'true'}\n  later: {needs: [s], run: 'true'}": `"later" is not a stage it needs`,
		"  s: {needs: [build], artifacts_from: [none], run: 'true'}":                                      `"none" is not a stage it needs`,
		"  s: {needs: [build], artifacts_from: [build, build], run: 'true'}":                              "names build twice",
	} {
		_, err := ParseJob("a.yml", []byte("name: a\nstages:\n  build: {run: 'true'}\n"+yml+"\n"))
		if err == nil || !strings.Contains(err.Error(), why) {
			t.Fatalf("%s: %v, want %q", yml, err, why)
		}
	}
	if _, err := stagesOf([]byte("stages:\n  a: {run: 'true'}\n  b: {artifacts_from: [a], run: 'true'}\n")); err == nil {
		t.Fatal("a source file's stage was given another it does not need")
	}
}
