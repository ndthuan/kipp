package config

import (
	"slices"
	"strings"
	"testing"
	"time"
)

const ownerJob = "name: acme\nsecrets: [DEPLOY]\nlocks: [shared]\nenv: {A: one, B: two}\ntriggers: {cron: \"0 3 * * *\"}\n" +
	"owner:\n  login: acme\n  token: GH_ACME\n"

func parseOwner(t *testing.T, body string) *Job {
	t.Helper()
	j, err := ParseJob("acme.yml", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestAnOwnerJobHasDefaultsAndBuildsNothingItself(t *testing.T) {
	j := parseOwner(t, ownerJob)
	o := j.Owner
	if o == nil || !j.IsOwner() || j.IsParent() || len(j.Stages) != 0 {
		t.Fatalf("an owner job is no parent and has no stages: %+v", j)
	}
	if o.Login != "acme" || o.Token != "GH_ACME" || o.Scan != DefaultScan || o.Clone != "https://github.com/%s.git" ||
		o.Settings.Path != ".kipp.yml" || o.Settings.PullRequests != PRMerge || o.Settings.Orphans.Days != DefaultOrphanDays {
		t.Fatalf("owner %+v", o)
	}
	if o.Cron != "0 3 * * *" || j.Triggers.Cron != "" {
		t.Fatalf("the cron is each repo's, and the owner job fires none: owner %q, job %q", o.Cron, j.Triggers.Cron)
	}
}

func TestAnOwnerJobTakesTheKeysItIsGiven(t *testing.T) {
	j := parseOwner(t, "name: acme\nowner:\n  login: acme\n  token: GH_ACME\n  include: [\"site-*\", api]\n  exclude: [\"*-old\"]\n"+
		"  path: ci/kipp.yml\n  pull_requests: both\n  scan: 10m\n  orphans: {days: 2}\n  clone: \"git@example.com:%s.git\"\n")
	o := j.Owner
	if o.Scan != 10*time.Minute || o.Settings.Path != "ci/kipp.yml" || o.Settings.PullRequests != PRBoth || o.Settings.Orphans.Days != 2 ||
		o.Clone != "git@example.com:%s.git" {
		t.Fatalf("owner %+v", o)
	}
	for name, in := range map[string]bool{"site-web": true, "SITE-api": true, "api": true, "site-old": false, "docs": false} {
		if got := o.Matches(name); got != in {
			t.Errorf("Matches(%q) = %v, want %v", name, got, in)
		}
	}
	if !parseOwner(t, ownerJob).Owner.Matches("anything") {
		t.Error("no include is every repo")
	}
}

func TestAnOwnerJobIsRefusedWhatIsNotItsOwn(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"both token and app": {"name: acme\nowner: {login: acme, token: T, app: {id: 1, key_file: /k.pem}}\n", "token or app, not both"},
		"app without id":     {"name: acme\nowner: {login: acme, app: {key_file: /k.pem}}\n", "owner.app.id"},
		"app without file":   {"name: acme\nowner: {login: acme, app: {id: 1}}\n", "owner.app.key_file"},
		"app relative file":  {"name: acme\nowner: {login: acme, app: {id: 1, key_file: k.pem}}\n", "owner.app.key_file"},
		"app bad name":       {"name: acme\nowner: {login: acme, app: {id: 1, key_file: /a/b c.pem}}\n", "owner.app.name"},
		"neither":            {"name: acme\nowner: {login: acme}\n", "owner.token"},
		"bad login":          {"name: acme\nowner: {login: acme/site, token: T}\n", "owner.login"},
		"stages":             {"name: acme\nowner: {login: acme, token: T}\nstages:\n  a: {run: x}\n", "no stages"},
		"multibranch too":    {"name: acme\nmultibranch: {repo: a/b}\nowner: {login: acme, token: T}\n", "a job is one of them"},
		"webhook":            {"name: acme\ntriggers: {webhook: {repo: a/b}}\nowner: {login: acme, token: T}\n", "triggers.webhook"},
		"after":              {"name: acme\ntriggers: {after: [x]}\nowner: {login: acme, token: T}\n", "triggers.after"},
		"short scan":         {"name: acme\nowner: {login: acme, token: T, scan: 1m}\n", "owner.scan"},
		"clone no %s":        {"name: acme\nowner: {login: acme, token: T, clone: \"https://x/y.git\"}\n", "owner.clone"},
		"bad pattern":        {"name: acme\nowner: {login: acme, token: T, include: [\"[\"]}\n", "owner.include"},
		"bad pr mode":        {"name: acme\nowner: {login: acme, token: T, pull_requests: sometimes}\n", "owner.pull_requests"},
		"repo with owner":    {"name: acme\nowner: {login: acme, token: T, repos: {\"acme/site\": {locks: [x]}}}\n", "owner.repos"},
		"secret name":        {"name: acme\nowner: {login: acme, token: T, repos: {site: {secrets: [\"a b\"]}}}\n", "owner.repos.site.secrets"},
		"store name":         {"name: acme\nowner: {login: acme, token: T, repos: {site: {metrics: {store: \"A B\"}}}}\n", "owner.repos.site.metrics.store"},
	} {
		_, err := ParseJob("acme.yml", []byte(tc.body))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v, want one naming %q", name, err, tc.want)
		}
	}
}

func TestANodeIsAMultibranchJobMadeFromTheOwnerJob(t *testing.T) {
	j := parseOwner(t, ownerJob+"  pull_requests: both\n")
	n := j.Node(RepoRecord{Owner: "acme", Key: "site", Repo: "acme/site", State: RepoLive})
	m := n.Multibranch
	if n.Name != "acme.site" || n.Folder != "acme" || !n.IsParent() || n.IsOwner() || n.Disabled {
		t.Fatalf("node %+v", n)
	}
	if m.Repo != "acme/site" || m.Clone != "https://github.com/acme/site.git" || m.Token != "GH_ACME" || m.PullRequests != PRBoth || m.Scan != 0 {
		t.Fatalf("multibranch %+v", m)
	}
	if n.Triggers.Cron != "0 3 * * *" || !slices.Equal(n.Secrets, []string{"DEPLOY"}) || !slices.Equal(n.Locks, []string{"shared"}) || n.Env["A"] != "one" {
		t.Fatalf("a node keeps the owner job's cron, secrets, locks and env: %+v", n)
	}
	if j.Owner.Settings.Repo != "" || j.Owner.Settings.Token != "GH_ACME" && j.Owner.Settings.Token != "" {
		t.Fatalf("making a node changed the owner: %+v", j.Owner.Settings)
	}
	gone := j.Node(RepoRecord{Owner: "acme", Key: "site", Repo: "acme/site", State: RepoGone})
	if !gone.Disabled || gone.Child(ChildRecord{Parent: gone.Name, Key: "main", Kind: KindBranch, State: ChildLive}).Name != "acme.site.main" ||
		!gone.Child(ChildRecord{Parent: gone.Name, Key: "main", State: ChildLive}).Disabled {
		t.Fatal("the children of a gone repo build no more")
	}
	if c := n.Child(ChildRecord{Parent: n.Name, Key: "main", Kind: KindBranch}); c.Folder != "acme" || c.Parent != "acme.site" {
		t.Fatalf("a child keeps its owner job: %+v", c)
	}
}

func TestARepoSettingsReplaceTheListsAndAddToTheEnv(t *testing.T) {
	j := parseOwner(t, ownerJob+"  repos:\n    Site:\n      locks: [deploy]\n      secrets: []\n      env: {B: three, C: four}\n    api:\n      secrets: [API_KEY]\n")
	site := j.Node(RepoRecord{Owner: "acme", Key: "site", Repo: "acme/site"})
	if !slices.Equal(site.Locks, []string{"deploy"}) || len(site.Secrets) != 0 || site.Env["A"] != "one" || site.Env["B"] != "three" || site.Env["C"] != "four" {
		t.Fatalf("site %+v %+v %+v", site.Locks, site.Secrets, site.Env)
	}
	api := j.Node(RepoRecord{Owner: "acme", Key: "api", Repo: "acme/API"})
	if !slices.Equal(api.Secrets, []string{"API_KEY"}) || !slices.Equal(api.Locks, []string{"shared"}) || j.Env["B"] != "two" {
		t.Fatalf("api %+v %+v; the owner's env is %v", api.Secrets, api.Locks, j.Env)
	}
	other := j.Node(RepoRecord{Owner: "acme", Key: "docs", Repo: "acme/docs"})
	if !slices.Equal(other.Secrets, []string{"DEPLOY"}) {
		t.Fatalf("a repo without settings has the job's: %v", other.Secrets)
	}
}

func TestARepoCanNameItsOwnMetricsStore(t *testing.T) {
	j := parseOwner(t, ownerJob+"  repos:\n    site:\n      metrics: {store: site}\n")
	if got := j.Node(RepoRecord{Owner: "acme", Key: "site", Repo: "acme/site"}).StoreName(); got != "site" {
		t.Fatalf("site reports into %q", got)
	}
	if got := j.Node(RepoRecord{Owner: "acme", Key: "docs", Repo: "acme/docs"}).StoreName(); got != "acme.docs" {
		t.Fatalf("a repo without settings reports into its own: %q", got)
	}
}

func TestARepoKeyHoldsNoDotAndNeverMeetsAnotherRepoOrAChild(t *testing.T) {
	none := func(string) bool { return false }
	if got := RepoKey("site", none); got != "site" {
		t.Fatalf("site -> %q", got)
	}
	for _, name := range []string{"Site", "my.repo", "a b", "ünï"} {
		got := RepoKey(name, none)
		if strings.Contains(got, ".") || !ValidName("acme."+got) || !strings.HasSuffix(got, "-"+keyHexOf(name)) {
			t.Errorf("%q -> %q", name, got)
		}
	}
	if a, b := RepoKey("site", func(k string) bool { return k == "site" }), RepoKey("site", none); a == b {
		t.Fatalf("a taken key gets a suffix: %q", a)
	}
	if RepoKey("My.Repo", none) == RepoKey("my.repo", none) {
		// Both fold to my-repo; only the hash of the name tells them apart.
		t.Fatal("two names that fold to one key share it")
	}
}

func keyHexOf(name string) string {
	k := RepoKey(name, func(string) bool { return true })
	return k[strings.LastIndex(k, "-")+1:]
}

func TestAFileNamedLikeARepoNodeIsRefused(t *testing.T) {
	dir := t.TempDir()
	writeJobs(t, dir, map[string]string{
		"acme":      "name: acme\nowner: {login: acme, token: T}\n",
		"acme.site": "name: acme.site\nstages:\n  a: {run: \"true\"}\n",
	})
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "is an owner job") {
		t.Fatalf("Load = %v", err)
	}
}

func writeJobs(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		write(t, dir+"/"+name+".yml", []byte(body))
	}
}

func TestAnOwnerJobSignsInAsAnAppNamedByItsKeyFile(t *testing.T) {
	j := parseOwner(t, "name: acme\nowner: {login: acme, app: {id: 123456, key_file: /data/apps/Acme-CI.pem}}\n")
	a := j.Owner.App
	if a == nil || a.ID != 123456 || a.KeyFile != "/data/apps/Acme-CI.pem" || a.Name != "acme-ci" || j.Owner.Token != "" {
		t.Fatalf("app %+v token %q", a, j.Owner.Token)
	}
	j = parseOwner(t, "name: acme\nowner: {login: acme, app: {id: 1, key_file: /k.pem, name: ci}}\n")
	if j.Owner.App.Name != "ci" {
		t.Fatalf("name %q", j.Owner.App.Name)
	}
	node := j.Node(RepoRecord{Owner: "acme", Key: "site", Repo: "acme/site", State: RepoLive})
	if node.Multibranch.App != j.Owner.App || node.Multibranch.Token != "" || !node.Multibranch.Authenticated() {
		t.Fatalf("a repo is called with its owner job's App: %+v", node.Multibranch)
	}
}

func TestLoadRefusesTwoAppsUnderOneNameAndTwoJobsOfOneAccountUnderOneApp(t *testing.T) {
	dir := t.TempDir()
	writeJobs(t, dir, map[string]string{
		"a": "name: a\nowner: {login: one, app: {id: 1, key_file: /x/ci.pem}}\n",
		"b": "name: b\nowner: {login: two, app: {id: 2, key_file: /y/ci.pem}}\n",
		"c": "name: c\nowner: {login: One, app: {id: 1, key_file: /x/ci.pem}}\n",
	})
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), `"ci"`) || !strings.Contains(err.Error(), "indexed by a too") {
		t.Fatalf("error %v", err)
	}
	dir = t.TempDir()
	writeJobs(t, dir, map[string]string{
		"a": "name: a\nowner: {login: one, app: {id: 1, key_file: /x/ci.pem}}\n",
		"b": "name: b\nowner: {login: two, app: {id: 1, key_file: /x/ci.pem}}\n",
	})
	if _, err := Load(dir); err != nil {
		t.Fatalf("one App serving two accounts: %v", err)
	}
}
