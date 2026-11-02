package config

import "testing"

func TestTestsFailedIsUnstableOrFailedAndNeedsAJunitGlob(t *testing.T) {
	stage := func(extra string) string {
		return "stages:\n  s:\n    run: make\n" + extra
	}
	for _, v := range []string{"unstable", "failed"} {
		j, err := parseMetricsJob(t, stage("    junit: r/*.xml\n    tests_failed: "+v+"\n"))
		if err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		if got := j.Stages[0].TestsFailed; got != v {
			t.Fatalf("%s: read as %q", v, got)
		}
	}
	j, err := parseMetricsJob(t, stage("    junit: r/*.xml\n"))
	if err != nil || j.Stages[0].TestsFailed != "" {
		t.Fatalf("absent changed: %v %q", err, j.Stages[0].TestsFailed)
	}
	refusedWith(t, stage("    junit: r/*.xml\n    tests_failed: cancelled\n"), `stage s: tests_failed "cancelled": unstable or failed`)
	refusedWith(t, stage("    tests_failed: unstable\n"), "stage s: tests_failed reads the stage's junit report, and junit names no file")
}
