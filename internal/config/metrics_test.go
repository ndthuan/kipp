package config

import (
	"slices"
	"strings"
	"testing"
)

// parseMetricsJob reads a job named site-smoke whose body is the YAML
// given, with one stage s unless the body brings its own.
func parseMetricsJob(t *testing.T, body string) (*Job, error) {
	t.Helper()
	src := "name: site-smoke\n" + body
	if !strings.Contains(body, "stages:") {
		src += "stages:\n  s: {run: make}\n"
	}
	return ParseJob("site-smoke.yml", []byte(src))
}

func refusedWith(t *testing.T, body, want string) {
	t.Helper()
	_, err := parseMetricsJob(t, body)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("%q: got %v, want an error with %q", body, err, want)
	}
}

func TestAJobWithoutMetricsRemembersAllOnTheDefaultBranchWhenItSucceeds(t *testing.T) {
	j, err := parseMetricsJob(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if j.StoreName() != "site-smoke" {
		t.Fatalf("store %q", j.StoreName())
	}
	main := Push{Trigger: "webhook", Branch: "main", DefaultBranch: "main"}
	branch := Push{Trigger: "webhook", Branch: "fix-1", DefaultBranch: "main"}
	for _, c := range []struct {
		push   Push
		status string
		want   bool
	}{
		{main, "success", true},
		{main, "unstable", true},
		{main, "failed", false},
		{main, "cancelled", false},
		{branch, "success", false},
		{Push{Trigger: "cron"}, "success", true},
	} {
		for _, metric := range []string{"seconds", "kipp.stage_seconds"} {
			if got := j.Remembers(metric, c.push, c.status); got != c.want {
				t.Errorf("%s on %+v %s: %v, want %v", metric, c.push, c.status, got, c.want)
			}
		}
	}
	if rules := j.RememberRules(); len(rules) != 1 || rules[0].Metrics != nil {
		t.Fatalf("the default rules %+v", rules)
	}
}

func TestAStoreNameIsAValidName(t *testing.T) {
	j, err := parseMetricsJob(t, "metrics: {store: acme.site}\n")
	if err != nil || j.StoreName() != "acme.site" {
		t.Fatalf("%v %+v", err, j)
	}
	refusedWith(t, "metrics: {store: Acme Site}\n", `metrics.store "Acme Site"`)
	refusedWith(t, "metrics: {store: ../x}\n", "metrics.store")
}

func TestRememberStatusIsSuccessUnstableOrFailedOnly(t *testing.T) {
	body := "metrics:\n  remember:\n    - {when: {status: [success]}}\n    - {metrics: [seconds], when: {status: [failed, cancelled]}}\n"
	refusedWith(t, body, "line 5")
	refusedWith(t, body, `"cancelled"`)
	refusedWith(t, "metrics:\n  remember:\n    - {when: {status: [lost]}}\n", `"lost"`)
	if _, err := parseMetricsJob(t, "metrics:\n  remember:\n    - {when: {status: [success, unstable, failed]}}\n"); err != nil {
		t.Fatal(err)
	}
}

func TestStatusIsRefusedInAStageWhen(t *testing.T) {
	_, err := stagesOf([]byte("stages:\n  s: {run: x, when: {default_branch: true, status: [success]}}\n"))
	if err == nil || !strings.Contains(err.Error(), "stage s: when.status") {
		t.Fatalf("got %v", err)
	}
}

func TestAGuardMustSayNoLowerTrue(t *testing.T) {
	stage := func(guard string) string {
		return "stages:\n  s:\n    run: make\n    metrics:\n      file: m.json\n      guard:\n        - " + guard + "\n"
	}
	refusedWith(t, stage("{metric: coverage, item: total}"), "no_lower: true")
	refusedWith(t, stage("{metric: coverage, item: total, no_lower: false}"), "no_lower: true")
	refusedWith(t, stage("{metric: coverage, item: total, no_lower: true, other: 1}"), "other")
	j, err := parseMetricsJob(t, stage("{metric: coverage, item: total, no_lower: true, tolerance: 0.01}"))
	if err != nil {
		t.Fatal(err)
	}
	g := j.Stages[0].Metrics.Guard[0]
	if g.Metric != "coverage" || g.Item != "total" || !g.NoLower || g.Tolerance != 0.01 || g.OnFall != "failed" || !g.IsRequired() {
		t.Fatalf("%+v", g)
	}
	refusedWith(t, stage("{metric: coverage, item: total, no_lower: true, tolerance: -1}"), "tolerance")
	refusedWith(t, stage("{metric: coverage, no_lower: true}"), "item")
	refusedWith(t, stage("{metric: Coverage, item: total, no_lower: true}"), "metric")
	refusedWith(t, stage("{metric: coverage, item: '[', no_lower: true}"), "path.Match")
}

func TestOnFallIsFailedOrUnstable(t *testing.T) {
	stage := func(onFall string) string {
		return "stages:\n  s:\n    run: make\n    metrics:\n      file: m.json\n      guard: [{metric: coverage, item: total, no_lower: true, on_fall: " + onFall + ", required: false}]\n"
	}
	j, err := parseMetricsJob(t, stage("unstable"))
	if err != nil {
		t.Fatal(err)
	}
	if g := j.Stages[0].Metrics.Guard[0]; g.OnFall != "unstable" || g.IsRequired() {
		t.Fatalf("%+v", g)
	}
	refusedWith(t, stage("cancelled"), `on_fall "cancelled"`)
}

func TestAGuardNeedsAMetricsFile(t *testing.T) {
	refusedWith(t, "stages:\n  s:\n    run: make\n    metrics:\n      guard: [{metric: coverage, item: total, no_lower: true}]\n",
		"stage s: metrics.guard reads the stage's report, and metrics names no file")
}

func TestAStageMayDeclareMetricsWithoutAFileToReadOnly(t *testing.T) {
	j, err := parseMetricsJob(t, "stages:\n  s: {run: make, metrics: {}}\n  t: {run: make, metrics: {file: \"out/metrics-*.json\"}}\n  u: {run: make}\n")
	if err != nil {
		t.Fatal(err)
	}
	s, u := j.Stages[0], j.Stages[2]
	if s.Metrics == nil || s.Metrics.File != "" || j.Stages[1].Metrics.File != "out/metrics-*.json" || u.Metrics != nil {
		t.Fatalf("%+v %+v %+v", s.Metrics, j.Stages[1].Metrics, u.Metrics)
	}
	refusedWith(t, "stages:\n  s: {run: make, metrics: {file: ../m.json}}\n", "leaves the stage's directory")
	refusedWith(t, "stages:\n  s: {run: make, metrics: {file: \"[\"}}\n", "path.Match")
}

func TestARememberRuleWithStatusAloneIsValid(t *testing.T) {
	j, err := parseMetricsJob(t, "metrics:\n  remember:\n    - {metrics: [seconds], when: {status: [success, unstable, failed]}}\n")
	if err != nil {
		t.Fatal(err)
	}
	branch := Push{Trigger: "webhook", Branch: "fix-1", DefaultBranch: "main"}
	if !j.Remembers("seconds", branch, "failed") {
		t.Fatal("seconds from a failed branch build is not remembered")
	}
	if j.Remembers("seconds", branch, "timeout") {
		t.Fatal("a timeout remembers")
	}
	if j.Remembers("coverage", Push{Trigger: "cron"}, "success") {
		t.Fatal("a metric no rule names is remembered")
	}
	refusedWith(t, "metrics:\n  remember:\n    - {metrics: [seconds]}\n", "line 4: metrics.remember: a rule needs when")
	refusedWith(t, "metrics:\n  remember:\n    - {metrics: [seconds], when: {}}\n", "a rule needs when")
	refusedWith(t, "metrics:\n  remember:\n    - {metrics: [Seconds], when: {status: [success]}}\n", `"Seconds"`)
	refusedWith(t, "metrics:\n  remember:\n    - {metrics: [seconds], when: {status: [success], branch: x}}\n", "branch")
}

func TestTheFirstRememberRuleThatNamesAMetricDecidesIt(t *testing.T) {
	j, err := parseMetricsJob(t, "metrics:\n  store: acme\n  remember:\n"+
		"    - {metrics: [coverage], when: {default_branch: true, status: [success]}}\n"+
		"    - {when: {status: [success, unstable, failed]}}\n")
	if err != nil {
		t.Fatal(err)
	}
	branch := Push{Trigger: "webhook", Branch: "fix-1", DefaultBranch: "main"}
	main := Push{Trigger: "webhook", Branch: "main", DefaultBranch: "main"}
	if j.Remembers("coverage", branch, "success") {
		t.Fatal("the second rule decided coverage")
	}
	if !j.Remembers("coverage", main, "success") || j.Remembers("coverage", main, "unstable") {
		t.Fatal("the first rule does not decide coverage")
	}
	if !j.Remembers("seconds", branch, "failed") {
		t.Fatal("the rule for every metric does not cover seconds")
	}
	if r := j.RememberRules()[0]; !r.Covers("coverage") || r.Covers("seconds") || !j.RememberRules()[1].Covers("anything") {
		t.Fatalf("%+v", j.RememberRules())
	}
}

func TestTheSampleJobsHoldAStoreRulesAndAReport(t *testing.T) {
	snap, err := Load("../../testdata/jobs")
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range snap.Jobs {
		if j.Metrics != nil && len(j.Metrics.Remember) > 0 && slices.ContainsFunc(j.Stages, func(s *Stage) bool { return s.Metrics != nil && s.Metrics.File != "" }) {
			return
		}
	}
	t.Fatal("no job in testdata/jobs keeps metrics, so kipp check does not cover the keys")
}
