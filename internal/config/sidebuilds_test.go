package config

import "testing"

const sideBuildsJob = "params:\n  EXTRA: {type: choice, options: ['off', 'on', linux]}\n  TIER: {default: quick}\n"

func TestASideBuildIsOneWhoseParamsHoldForTheRule(t *testing.T) {
	j, err := parseMetricsJob(t, sideBuildsJob+"side_builds: {params: {EXTRA: ['on', linux]}}\n")
	if err != nil {
		t.Fatal(err)
	}
	for params, want := range map[string]bool{"off": false, "on": true, "linux": true, "": false} {
		given := map[string]string{"TIER": "quick"}
		if params != "" {
			given["EXTRA"] = params
		}
		if got := j.IsSideBuild(given); got != want {
			t.Errorf("EXTRA=%q: side build %v, want %v", params, got, want)
		}
	}
}

func TestAJobWithoutSideBuildsHasNone(t *testing.T) {
	j, err := parseMetricsJob(t, sideBuildsJob)
	if err != nil {
		t.Fatal(err)
	}
	if j.IsSideBuild(map[string]string{"EXTRA": "on"}) {
		t.Fatal("a job with no side_builds made a side build")
	}
}

func TestEveryChildAndRepoOfAJobHasItsSideBuilds(t *testing.T) {
	j, err := ParseJob("site.yml", []byte("name: site\nmultibranch: {repo: acme/site}\n"+sideBuildsJob+"side_builds: {params: {EXTRA: ['on']}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c := j.Child(ChildRecord{Key: "main", Branch: "main"}); !c.IsSideBuild(map[string]string{"EXTRA": "on"}) {
		t.Error("a child lost its parent's side_builds")
	}
	o := parseOwner(t, "name: acme\nowner: {login: acme, token: GH_TOKEN}\n"+sideBuildsJob+"side_builds: {params: {EXTRA: ['on']}}\n")
	n := o.Node(RepoRecord{Owner: "acme", Key: "site", Repo: "acme/site", State: RepoLive})
	if !n.Child(ChildRecord{Key: "main", Branch: "main"}).IsSideBuild(map[string]string{"EXTRA": "on"}) {
		t.Error("a repo's child lost its owner job's side_builds")
	}
}

func TestASideBuildsRuleNoBuildOrEveryBuildMatchesIsRefused(t *testing.T) {
	for body, want := range map[string]string{
		"side_builds: {}\n":                                     "side_builds: params names at least one param",
		"side_builds: {params: {}}\n":                           "side_builds: params names at least one param",
		"side_builds: {params: {EXTRA: [on]}, branches: [x]}\n": "field branches not found",
		"side_builds: {params: {TIRE: [on]}}\n":                 `side_builds.params: "TIRE" is not a param of the job`,
		"side_builds: {params: {EXTRA: [huge]}}\n":              `side_builds.params.EXTRA: "huge" is not one of off, on, linux`,
		"side_builds: {params: {EXTRA: []}}\n":                  "side_builds.params.EXTRA: no value",
		"side_builds: {params: {EXTRA: ['off', 'on']}}\n":       "side_builds: a build with every param at its default is one, so every ordinary build would be",
		"side_builds: {params: {TIER: [quick]}}\n":              "side_builds: a build with every param at its default is one",
	} {
		refusedWith(t, sideBuildsJob+body, want)
	}
	if _, err := parseMetricsJob(t, sideBuildsJob+"side_builds: {params: {EXTRA: ['off'], TIER: [full]}}\n"); err != nil {
		t.Fatalf("a rule that holds for a default and another value: %v", err)
	}
}
