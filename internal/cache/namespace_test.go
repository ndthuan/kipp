package cache

import (
	"strings"
	"testing"
)

func TestOnlyAPushToTheDefaultBranchWritesDefault(t *testing.T) {
	cases := []struct {
		name                  string
		branch, defaultBranch string
		pushed, writesDefault bool
		want                  Namespace
	}{
		{"a push to the default branch", "main", "main", true, false, "default"},
		{"a push to another branch", "feature/x", "main", true, false, NamespaceOfBranch("feature/x")},
		{"a webhook that named no branch", "", "main", true, false, "j-site-smoke"},
		{"a webhook queued before the default branch was kept", "main", "", true, false, NamespaceOfBranch("main")},
		{"an api build", "", "", false, false, "j-site-smoke"},
		{"a web build", "main", "main", false, false, "j-site-smoke"},
		{"a build made again from a push", "feature/x", "main", true, false, NamespaceOfBranch("feature/x")},
		{"an after build", "", "", false, false, "j-site-smoke"},
	}
	for _, c := range cases {
		if got := NamespaceOf(c.pushed, c.branch, c.defaultBranch, "site-smoke", c.writesDefault); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestACronBuildWritesItsJobsNamespaceUnlessTheJobOptsIn(t *testing.T) {
	if got := NamespaceOf(false, "", "", "site-smoke", false); got != "j-site-smoke" {
		t.Fatalf("cron: %q", got)
	}
	if got := NamespaceOf(false, "", "", "site-smoke", true); got != "default" {
		t.Fatalf("cron with cache_writes_default: %q", got)
	}
	if got := NamespaceOf(true, "feature", "main", "site-smoke", true); got != "default" {
		t.Fatalf("a branch build of a job that vouches for its code: %q", got)
	}
}

func TestABranchNamedLikeANamespaceCannotAliasIt(t *testing.T) {
	seen := map[Namespace]string{}
	for _, branch := range []string{"default", "j-site-smoke", "b-x", "x", "X", "a/b", "a_b", "a-b", strings.Repeat("long/", 60)} {
		ns := NamespaceOf(true, branch, "main", "site-smoke", false)
		if ns == "default" || strings.HasPrefix(string(ns), "j-") {
			t.Errorf("branch %q is in namespace %q, which a job or default owns", branch, ns)
		}
		if other, dup := seen[ns]; dup {
			t.Errorf("branches %q and %q share namespace %q", branch, other, ns)
		}
		seen[ns] = branch
		if strings.ContainsAny(string(ns), "/\\") || len(ns) > 200 {
			t.Errorf("namespace %q is not one short file name", ns)
		}
	}
}
