package config

import (
	"strings"
	"testing"
)

func yes() *bool { v := true; return &v }
func no() *bool  { v := false; return &v }

func TestAConditionHoldsOnlyWhenEveryKeyGivenHolds(t *testing.T) {
	main := Push{Trigger: "webhook", Branch: "main", DefaultBranch: "main"}
	ticket := Push{Trigger: "webhook", Branch: "APP-12-fix", DefaultBranch: "main"}
	cron := Push{Trigger: "cron"}
	tag := Push{Trigger: "webhook", Tag: "v1.2.0", DefaultBranch: "main"}
	cases := []struct {
		name string
		when *When
		push Push
		want bool
	}{
		{"no condition", nil, ticket, true},
		{"default branch on main", &When{DefaultBranch: yes()}, main, true},
		{"default branch on a ticket", &When{DefaultBranch: yes()}, ticket, false},
		{"not default on a ticket", &When{DefaultBranch: no()}, ticket, true},
		{"not default on main", &When{DefaultBranch: no()}, main, false},
		{"a build not pushed runs on the default branch", &When{DefaultBranch: yes()}, cron, true},
		{"a push whose default branch was not kept is not on it", &When{DefaultBranch: yes()}, Push{Trigger: "webhook", Branch: "main"}, false},
		{"a pattern", &When{Branches: []string{"APP-*"}}, ticket, true},
		{"a pattern stops at a slash", &When{Branches: []string{"APP-*"}}, Push{Trigger: "webhook", Branch: "epic/APP-1-x"}, false},
		{"branches never hold without a branch", &When{Branches: []string{"*"}}, cron, false},
		{"a pull request into main", &When{TargetBranches: []string{"main"}}, Push{Trigger: "api", Child: true, PR: 4, Branch: "APP-1-x", DefaultBranch: "main", Base: "main"}, true},
		{"a pull request into an epic", &When{TargetBranches: []string{"main"}}, Push{Trigger: "webhook", Child: true, PR: 5, Branch: "APP-2-x", DefaultBranch: "main", Base: "epic/APP-1-y"}, false},
		{"a target pattern", &When{TargetBranches: []string{"epic/*"}}, Push{Trigger: "webhook", Child: true, PR: 5, Branch: "APP-2-x", DefaultBranch: "main", Base: "epic/APP-1-y"}, true},
		{"a branch has no target", &When{TargetBranches: []string{"*"}}, ticket, false},
		{"a trigger", &When{Triggers: []string{"cron"}}, cron, true},
		{"another trigger", &When{Triggers: []string{"cron"}}, main, false},
		{"both keys must hold", &When{DefaultBranch: yes(), Triggers: []string{"webhook"}}, cron, false},
		{"a tag pattern", &When{Tags: []string{"v*"}}, tag, true},
		{"another tag", &When{Tags: []string{"v*"}}, Push{Trigger: "webhook", Tag: "nightly"}, false},
		{"tags never hold on a branch", &When{Tags: []string{"*"}}, main, false},
		{"tags never hold without a push", &When{Tags: []string{"*"}}, cron, false},
		{"branches never hold on a tag", &When{Branches: []string{"*"}}, tag, false},
		{"a tag is not on the default branch", &When{DefaultBranch: yes()}, tag, false},
	}
	for _, c := range cases {
		if got := c.when.Holds(c.push); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

func TestABadConditionIsRefusedWithTheStage(t *testing.T) {
	for body, want := range map[string]string{
		"s: {run: x, when: {}}":                                          "when needs branches, tags, target_branches, default_branch, triggers, params or needs_status",
		"s: {run: x, when: {tags: [\"[\"]}}":                             "when.tags",
		"s: {run: x, when: {tags: [v*], triggers: [cron]}}":              "leaves webhook out",
		"s: {run: x, when: {tags: [v*], branches: [main]}}":              "a push is to a branch or of a tag",
		"s: {run: x, when: {tags: [v*], triggers: [webhook]}}":           "",
		"s: {run: x, when: {branches: [\"[\"]}}":                         "not a pattern",
		"s: {run: x, when: {target_branches: [\"[\"]}}":                  "when.target_branches",
		"s: {run: x, when: {triggers: [push]}}":                          `"push" is not one of`,
		"s: {run: x, when: {branches: [main], triggers: [cron]}}":        "leaves webhook out",
		"s: {run: x, when: {default_branch: true, other: 1}}":            "other",
		"s: {run: x, when: {default_branch: true, triggers: [webhook]}}": "",
	} {
		_, err := stagesOf([]byte("stages:\n  " + body + "\n"))
		switch {
		case want == "" && err != nil:
			t.Errorf("%s: refused: %v", body, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%s: got %v, want %q", body, err, want)
		}
	}
}

const paramsJob = "params:\n  TIER: {type: choice, options: [quick, full], default: quick}\n  DRY_RUN: {type: bool, default: true}\n  TARGET: {default: staging}\n"

func TestParamsHoldOnlyForTheValuesListed(t *testing.T) {
	j, err := parseMetricsJob(t, paramsJob+"stages:\n  s: {run: x, when: {params: {TIER: [full], TARGET: [staging, prod]}}}\n")
	if err != nil {
		t.Fatal(err)
	}
	w := j.Stages[0].When
	for _, c := range []struct {
		params map[string]string
		want   bool
	}{
		{map[string]string{"TIER": "full", "TARGET": "prod"}, true},
		{map[string]string{"TIER": "quick", "TARGET": "prod"}, false},
		{map[string]string{"TIER": "full", "TARGET": "dev"}, false},
		{map[string]string{"TIER": "full"}, false}, // a param the build does not have holds no value
	} {
		if got := w.Holds(Push{Trigger: "web", Params: c.params}); got != c.want {
			t.Errorf("%v: got %v", c.params, got)
		}
	}
	j, err = parseMetricsJob(t, paramsJob+"metrics:\n  remember:\n    - {when: {params: {TIER: [full]}, status: [success]}}\n")
	if err != nil {
		t.Fatal(err)
	}
	full, quick := Push{Trigger: "web", Params: map[string]string{"TIER": "full"}}, Push{Trigger: "web", Params: map[string]string{"TIER": "quick"}}
	if !j.Remembers("seconds", full, "success") || j.Remembers("seconds", quick, "success") {
		t.Error("a remember rule does not read params as a stage's condition does")
	}
}

// YAML reads true as a bool and "true" as a string; both are the one
// value a bool param has, as are the other spellings YAML reads as true.
func TestABoolParamIsComparedAsTrueOrFalse(t *testing.T) {
	for _, list := range []string{"[true]", `["true"]`, "[True]", "[TRUE]"} {
		j, err := parseMetricsJob(t, paramsJob+"stages:\n  s: {run: x, when: {params: {DRY_RUN: "+list+"}}}\n")
		if err != nil {
			t.Fatalf("%s: %v", list, err)
		}
		w := j.Stages[0].When
		if !w.Holds(Push{Params: map[string]string{"DRY_RUN": "true"}}) || w.Holds(Push{Params: map[string]string{"DRY_RUN": "false"}}) {
			t.Errorf("%s: true and false are not told apart", list)
		}
	}
	for _, list := range []string{"[yes]", "[1]", "[on]"} {
		refusedWith(t, paramsJob+"stages:\n  s: {run: x, when: {params: {DRY_RUN: "+list+"}}}\n", "true or false")
	}
}

func TestAParamTheJobDoesNotHaveIsRefused(t *testing.T) {
	stage := func(params string) string {
		return paramsJob + "stages:\n  s: {run: x, when: {params: " + params + "}}\n"
	}
	refusedWith(t, stage("{TIRE: [full]}"), `stage s: when.params: "TIRE" is not a param of the job`)
	refusedWith(t, stage("{TIER: [huge]}"), `when.params.TIER: "huge" is not one of quick, full`)
	refusedWith(t, stage("{TIER: []}"), "when.params.TIER: no value")
	refusedWith(t, "metrics:\n  remember:\n    - {when: {params: {TIRE: [full]}}}\n", `metrics.remember: when.params: "TIRE" is not a param of the job`)
	if _, err := parseMetricsJob(t, paramsJob+"metrics:\n  remember:\n    - {when: {params: {TIER: [full]}}}\n"); err != nil {
		t.Fatalf("a remember rule on a param: %v", err)
	}
	// A source file's stages are checked against the job when the build
	// reads them: kipp check never sees them.
	j, err := parseMetricsJob(t, paramsJob)
	if err != nil {
		t.Fatal(err)
	}
	stages, err := stagesOf([]byte("stages:\n  s: {run: x, when: {params: {TIRE: [full]}}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := j.CheckWhenParams(stages); err == nil || !strings.Contains(err.Error(), `"TIRE"`) {
		t.Fatalf("got %v", err)
	}
}

func TestNeedsStatusWithoutNeedsIsRefused(t *testing.T) {
	for body, want := range map[string]string{
		"s: {run: x, when: {needs_status: always}}":                                               "stage s: when.needs_status needs needs",
		"s: {run: x, when: {needs_status: success}}":                                              "stage s: when.needs_status needs needs",
		"s: {run: x}\n  t: {needs: [s], run: x, when: {needs_status: often}}":                     `"often" is not one of [success failure always]`,
		"s: {run: x}\n  t: {needs: [s], run: x, when: {needs_status: always}}":                    "",
		"s: {run: x}\n  t: {needs: [s], run: x, when: {needs_status: failure, triggers: [cron]}}": "",
	} {
		_, err := stagesOf([]byte("stages:\n  " + body + "\n"))
		switch {
		case want == "" && err != nil:
			t.Errorf("%s: refused: %v", body, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%s: got %v, want %q", body, err, want)
		}
	}
	refusedWith(t, "metrics:\n  remember:\n    - {when: {status: [success], needs_status: always}}\n", "when.needs_status is a stage's")
}

func TestScanIsATriggerAStageCanBeLimitedTo(t *testing.T) {
	stages, err := stagesOf([]byte("stages:\n  s: {run: x, when: {triggers: [scan, webhook]}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	w := stages[0].When
	if !w.Holds(Push{Trigger: "scan"}) || w.Holds(Push{Trigger: "cron"}) {
		t.Fatal("when.triggers does not hold for scan alone")
	}
}
