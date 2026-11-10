package config

import (
	"slices"
	"strings"
	"testing"
)

// reuseJob is a plain job whose gate stage reuses, with extra stage keys.
func reuseJob(reuse, extra string) string {
	return "name: deploy\nparams:\n  FULL_GATE: {type: bool}\n  WHO: {type: string}\nfingerprint: [go version]\n" +
		"stages:\n  checkout: {run: 'true'}\n  gate:\n    needs: [checkout]\n    run: 'true'\n    reuse: " + reuse + "\n" + extra
}

func TestAJobWithReuseAndFingerprintIsRead(t *testing.T) {
	j, err := ParseJob("deploy.yml", []byte(reuseJob("{from: [beenavee.site], stage: run, tree: $KIPP_WORKSPACE/src, force: FULL_GATE}", "")))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(j.Fingerprint, []string{"go version"}) {
		t.Fatalf("fingerprint %q", j.Fingerprint)
	}
	r := j.Stages[1].Reuse
	if r == nil || !slices.Equal(r.From, []string{"beenavee.site"}) || r.Stage != "run" || r.Tree != "$KIPP_WORKSPACE/src" || r.Force != "FULL_GATE" {
		t.Fatalf("reuse %+v", r)
	}
	if j.Stages[0].Reuse != nil {
		t.Fatal("a stage without reuse has one")
	}
}

func TestReuseRefusals(t *testing.T) {
	for name, c := range map[string]struct{ reuse, extra, want string }{
		"without from":               {"{stage: run, tree: /x}", "", "reuse.from names no job"},
		"with an empty from":         {"{from: [], stage: run, tree: /x}", "", "reuse.from names no job"},
		"a from not a job":           {"{from: [Not A Job], stage: run, tree: /x}", "", `reuse.from "Not A Job"`},
		"without stage":              {"{from: [ci], tree: /x}", "", "reuse.stage"},
		"stage not a name":           {"{from: [ci], stage: Run Me, tree: /x}", "", `reuse.stage "Run Me" is not a stage name`},
		"force naming nothing":       {"{from: [ci], stage: run, tree: /x, force: NOPE}", "", "no param of that name"},
		"force not a bool":           {"{from: [ci], stage: run, tree: /x, force: WHO}", "", "a string param; it must be a bool"},
		"with retries":               {"{from: [ci], stage: run, tree: /x}", "    retries: 2\n", "reuse with retries"},
		"with junit":                 {"{from: [ci], stage: run, tree: /x}", "    junit: out.xml\n", "reuse with junit"},
		"with metrics":               {"{from: [ci], stage: run, tree: /x}", "    metrics: {}\n", "reuse with metrics"},
		"with artifacts":             {"{from: [ci], stage: run, tree: /x}", "    artifacts: {paths: [x]}\n", "reuse with artifacts"},
		"a plain job's without tree": {"{from: [ci], stage: run}", "", "reuse.tree: the job checks out no repo"},
		"tree with runs_on":          {"{from: [ci], stage: run, tree: /x}", "    runs_on: [linux]\n", "reuse.tree with runs_on"},
	} {
		_, err := ParseJob("deploy.yml", []byte(reuseJob(c.reuse, c.extra)))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}

func TestAFingerprintIsANonEmptyListOfStrings(t *testing.T) {
	for _, bad := range []string{"fingerprint: []\n", "fingerprint: go version\n", "fingerprint: [1]\n", "fingerprint:\n", "fingerprint: ['']\n", "fingerprint: [[a]]\n"} {
		_, err := ParseJob("a.yml", []byte("name: a\n"+bad+"stages:\n  s: {run: 'true'}\n"))
		if err == nil || !strings.Contains(err.Error(), "fingerprint: a list of one or more shell commands") {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestAStageFileMayReuseWithoutATreeAndTheJobChecksItsForce(t *testing.T) {
	f, err := ParseStageFile([]byte("stages:\n  gate: {run: 'true', reuse: {from: [ci], stage: run, force: FULL}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	job := &Job{Name: "ci.site.main", Params: []Param{{Name: "FULL", Type: "bool"}}}
	if err := job.CheckReuse(f.Stages, false); err != nil {
		t.Fatalf("a stage of a checked-out repo needs no tree: %v", err)
	}
	if err := (&Job{Name: "ci.site.main"}).CheckReuse(f.Stages, false); err == nil {
		t.Fatal("a force naming no param of the job was taken")
	}
}

func TestARepoFingerprintReplacesTheOwnerJobs(t *testing.T) {
	j := parseOwner(t, "name: acme\nfingerprint: [go version]\nowner:\n  login: acme\n  token: GH_ACME\n  repos:\n    site:\n      fingerprint: [node --version]\n")
	if got := j.Node(RepoRecord{Owner: "acme", Key: "site", Repo: "acme/site"}).Fingerprint; !slices.Equal(got, []string{"node --version"}) {
		t.Fatalf("site takes %q", got)
	}
	node := j.Node(RepoRecord{Owner: "acme", Key: "docs", Repo: "acme/docs"})
	if !slices.Equal(node.Fingerprint, []string{"go version"}) {
		t.Fatalf("a repo without one takes the job's: %q", node.Fingerprint)
	}
	if child := node.Child(ChildRecord{Parent: node.Name, Key: "main", Branch: "main"}); !slices.Equal(child.Fingerprint, []string{"go version"}) {
		t.Fatalf("a child takes its node's: %q", child.Fingerprint)
	}
	_, err := ParseJob("acme.yml", []byte("name: acme\nowner:\n  login: acme\n  token: T\n  repos:\n    site: {fingerprint: []}\n"))
	if err == nil || !strings.Contains(err.Error(), "owner.repos.site.fingerprint") {
		t.Fatalf("an empty repo fingerprint: %v", err)
	}
}

func TestTheRunnerClassIsLocalOrTheSortedLabelsAndTheImage(t *testing.T) {
	for want, s := range map[string]*Stage{
		"local":                        {},
		"runs_on arm64,linux":          {RunsOn: Labels{"linux", "arm64"}},
		"runs_on linux image golang:1": {RunsOn: Labels{"linux"}, Image: "golang:1"},
	} {
		if got := s.RunnerClass(); got != want {
			t.Errorf("%+v: %q, want %q", s, got, want)
		}
	}
}
