package config

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestMultibranchDefaultsBuildOnlyTheDefaultBranchAndPRs(t *testing.T) {
	j, err := ParseJob("site.yml", []byte("name: site\nmultibranch: {repo: acme/site}\n"))
	if err != nil {
		t.Fatal(err)
	}
	m := j.Multibranch
	want := Multibranch{
		Repo: "acme/site", Clone: "https://github.com/acme/site.git", Path: ".kipp.yml", Branches: []string{"default"},
		PullRequests: "merge", BranchWithPR: "skip", Scan: 30 * time.Minute, Orphans: Orphans{Days: 7},
	}
	if m == nil || !slices.Equal(m.Branches, want.Branches) {
		t.Fatalf("multibranch %+v", m)
	}
	got := *m
	got.Branches, want.Branches = nil, nil
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("defaults %+v, want %+v", got, want)
	}
	if !j.IsParent() || len(j.Stages) != 0 || j.Source != nil {
		t.Fatalf("a multibranch job is a parent with no stages of its own: %+v", j)
	}
	// Only the default branch builds without a pull request, whatever it
	// is called; pull requests are merged into their base.
	for branch, built := range map[string]bool{"trunk": true, "feature/x": false, "main": false} {
		if got := m.Selects(branch, "trunk"); got != built {
			t.Errorf("a push to %s with default branch trunk builds: %v, want %v", branch, got, built)
		}
	}
	if m.Selects("main", "") {
		t.Error("a branch is taken for the default one when the default one is not known")
	}
	if !m.Builds(KindPR) || m.Builds(KindPRHead) || !m.Builds(KindBranch) {
		t.Error("by default a pull request builds merged into its base, and its head alone does not")
	}
}

func TestMultibranchTakesEveryKeyItIsGiven(t *testing.T) {
	full := "name: site\nmultibranch:\n  repo: acme/site\n  clone: git@example.com:acme/site.git\n  path: ci/kipp.yml\n" +
		"  branches: [default, \"release/*\"]\n  pull_requests: both\n  branch_with_pr: build\n  rebuild_on_base: true\n" +
		"  scan: \"0\"\n  orphans: {days: 3}\n  token: GITHUB_TOKEN_ACME\nworkdir: services/api\n"
	j, err := ParseJob("site.yml", []byte(full))
	if err != nil {
		t.Fatal(err)
	}
	m := j.Multibranch
	if m.Clone != "git@example.com:acme/site.git" || m.Path != "ci/kipp.yml" || m.Scan != 0 || m.Orphans.Days != 3 ||
		!m.RebuildOnBase || m.BranchWithPR != "build" || m.Token != "GITHUB_TOKEN_ACME" || !m.Selects("release/2", "main") {
		t.Fatalf("multibranch %+v", m)
	}
	if !m.Builds(KindPR) || !m.Builds(KindPRHead) {
		t.Error("both builds the merge and the head")
	}
}

func TestMultibranchRefusesStagesSourceWebhookAndAbsoluteWorkdir(t *testing.T) {
	for body, want := range map[string]string{
		"stages:\n  s: {run: 'true'}\n":                          "no stages or source",
		"source: {repo: acme/site, path: ci.yml}\n":              "no stages or source",
		"triggers: {webhook: {repo: acme/site}}\n":               "has no triggers.webhook",
		"workdir: /srv/site\n":                                   "a path inside the clone",
		"multibranch: {repo: acme/site, path: /etc/kipp.yml}\n":  "multibranch.path",
		"multibranch: {repo: site}\n":                            "owner/name",
		"multibranch: {repo: acme/site, scan: 1m}\n":             "at least 5m0s",
		"multibranch: {repo: acme/site, pull_requests: all}\n":   "merge, head, both or off",
		"multibranch: {repo: acme/site, branch_with_pr: yes}\n":  "skip or build",
		"multibranch: {repo: acme/site, orphans: {days: 0}}\n":   "1 or more",
		"multibranch: {repo: acme/site, branches: []}\n":         "at least one",
		"multibranch: {repo: acme/site, branches: [\"[\"]}\n":    "path.Match",
		"multibranch: {repo: acme/site, token: not-a-name}\n":    "a kipp secret",
		"multibranch: {repo: acme/site, clone: --upload-pack}\n": "never starts with a dash",
		"multibranch: {repo: acme/site, pr: 1}\n":                "field pr not found",
	} {
		src := "name: site\n" + body
		if !strings.HasPrefix(body, "multibranch:") {
			src += "multibranch: {repo: acme/site}\n"
		}
		_, err := ParseJob("site.yml", []byte(src))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want it refused with %q", body, err, want)
		}
	}
	if _, err := ParseJob("site.yml", []byte("name: site\nmultibranch: {repo: acme/site}\nworkdir: services/api\nsecrets: [DEPLOY_KEY]\nparams:\n  TIER: {type: choice, options: [quick, full]}\n")); err != nil {
		t.Fatalf("a relative workdir, secrets and params are a multibranch job's: %v", err)
	}
}

func TestChildKeyKeepsASafeNameAndHashesAnyOther(t *testing.T) {
	none := func(string) bool { return false }
	for branch, want := range map[string]string{
		"main":           "main",
		"release-1.2_x":  "release-1.2_x",
		"PEN-3-x":        "pen-3-x-",
		"feature/login":  "feature-login-",
		"pr-12":          "pr-12-",
		"pr-12-head":     "pr-12-head-",
		"caf\u00e9":      "caf--",
		"Main":           "main-",
		"fix:everything": "fix-everything-",
	} {
		got := ChildKey(branch, none)
		if strings.HasSuffix(want, "-") {
			if !strings.HasPrefix(got, want) || len(got) != len(want)+6 || strings.Trim(got[len(want):], "0123456789abcdef") != "" {
				t.Errorf("%q: key %q, want %q and 6 hex digits", branch, got, want)
			}
		} else if got != want {
			t.Errorf("%q: key %q, want %q", branch, got, want)
		}
		if !ValidName("site." + got) {
			t.Errorf("%q: the child's name site.%s is not a job name", branch, got)
		}
	}
	if a, b := ChildKey("Feature/X", none), ChildKey("feature/x", none); a == b {
		t.Fatalf("two branches share the key %s", a)
	}
	taken := ChildKey("main", func(k string) bool { return k == "main" })
	if taken == "main" || !strings.HasPrefix(taken, "main-") {
		t.Fatalf("a key another child has was given again: %q", taken)
	}
	if first, again := ChildKey("PEN-3-x", none), ChildKey("PEN-3-x", none); first != again {
		t.Fatal("the key of a branch changes from one call to the next")
	}
}

func TestAFileNamedAfterAParentsChildIsRefused(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"site":      "name: site\nmultibranch: {repo: acme/site}\n",
		"site.main": "name: site.main\nstages:\n  s: {run: 'true'}\n",
		"site-docs": "name: site-docs\nstages:\n  s: {run: 'true'}\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name+".yml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "site.main.yml: site is a multibranch job") {
		t.Fatalf("a job file with a child's name loads: %v", err)
	}
	if strings.Contains(err.Error(), "site-docs") {
		t.Fatalf("a name that only starts like the parent's is refused: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "site.main.yml")); err != nil {
		t.Fatal(err)
	}
	snap, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	// What a job of a file is not: a child.
	if j := snap.Jobs["site-docs"]; j.IsParent() || j.Parent != "" {
		t.Fatalf("%+v", j)
	}
}

func TestAChildInheritsItsParentButCronAfterAndWebhook(t *testing.T) {
	j, err := ParseJob("site.yml", []byte("name: site\ngroup: web\nmultibranch: {repo: acme/site}\nsecrets: [DEPLOY_KEY]\nlocks: [site-ports]\n"+
		"timezone: Europe/Paris\ntriggers: {cron: 'H 3 * * *'}\nkeep: {builds: 5}\n"))
	if err != nil {
		t.Fatal(err)
	}
	c := j.Child(ChildRecord{Parent: "site", Key: "pr-12", Kind: KindPR, Branch: "feature/login", PR: 12, Title: "Log in", State: ChildLive})
	if c.Name != "site.pr-12" || c.Parent != "site" || c.Group != "web" || c.Triggers.Cron != "" || c.TimeZone != nil ||
		!slices.Equal(c.Secrets, []string{"DEPLOY_KEY"}) || !slices.Equal(c.Locks, []string{"site-ports"}) || c.Keep.Builds != 5 ||
		c.Multibranch == nil || c.IsParent() || c.Description != "Log in" || c.Disabled {
		t.Fatalf("child %+v", c)
	}
	if j.Name != "site" || j.Triggers.Cron == "" {
		t.Fatal("making a child changed its parent")
	}
	gone := j.Child(ChildRecord{Parent: "site", Key: "main", Kind: KindBranch, Branch: "main", State: ChildGone})
	if !gone.Disabled {
		t.Fatal("a gone child can still be built")
	}
	gone.DefaultBranch = "main"
	snap := (&Snapshot{Jobs: map[string]*Job{"site": j}, Names: []string{"site"}}).WithJobs(c, gone)
	if !slices.Equal(snap.Names, []string{"site", "site.main", "site.pr-12"}) || snap.DefaultChild("site") != gone {
		t.Fatalf("snapshot %v, default child %v", snap.Names, snap.DefaultChild("site"))
	}
}
