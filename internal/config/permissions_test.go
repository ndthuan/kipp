package config

import (
	"slices"
	"strings"
	"testing"
)

const stageBody = "stages:\n  a: {run: \"true\"}\n"

func jobWith(t *testing.T, extra string) (*Job, error) {
	t.Helper()
	return ParseJob("ci.yml", []byte("name: ci\n"+extra+stageBody))
}

func TestPermissionsTakeActionsListsAndAliases(t *testing.T) {
	j, err := jobWith(t, "permissions:\n  self: [view, cancel]\n  other: read\n  third: write\n  fourth: build\n")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Grant{
		"ci":     {View: true, Cancel: true},
		"other":  {View: true},
		"third":  {View: true, Build: true, Cancel: true},
		"fourth": {Build: true},
	}
	got := j.Grants()
	for name, g := range want {
		if got[name] != g {
			t.Errorf("%s: got %+v, want %+v", name, got[name], g)
		}
	}
}

func TestAJobWithNoPermissionsHoldsViewOnItselfAndNothingElse(t *testing.T) {
	j, err := jobWith(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if g := j.Grants(); len(g) != 1 || g["ci"] != (Grant{View: true}) {
		t.Fatalf("grants %+v", g)
	}
}

func TestViewOnSelfIsThereWhateverIsWritten(t *testing.T) {
	j, err := jobWith(t, "permissions:\n  self: cancel\n")
	if err != nil {
		t.Fatal(err)
	}
	if g := j.Grants()["ci"]; !g.View || !g.Cancel {
		t.Fatalf("self grant %+v", g)
	}
}

func TestPermissionsNoneIsTheOneWayToNoToken(t *testing.T) {
	j, err := jobWith(t, "permissions: none\n")
	if err != nil {
		t.Fatal(err)
	}
	if !j.Permissions.NoToken() {
		t.Fatal("permissions: none must give no token")
	}
	if (*Permissions)(nil).NoToken() {
		t.Fatal("no key is the default token")
	}
}

func TestPermissionsRefuseWhatIsNotAnAction(t *testing.T) {
	for name, in := range map[string]string{
		"unknown action": "permissions:\n  self: delete\n",
		"in a list":      "permissions:\n  self: [view, admin]\n",
		"no action":      "permissions:\n  other: []\n",
		"bad word":       "permissions: all\n",
		"bad name":       "permissions:\n  Bad Name: view\n",
		"twice":          "permissions:\n  a: view\n  a: build\n",
		"a list":         "permissions: [view]\n",
	} {
		if _, err := jobWith(t, in); err == nil {
			t.Errorf("%s: want a refusal", name)
		}
	}
}

func TestBuildOrWriteOnSelfOrTheJobsOwnNameIsRefused(t *testing.T) {
	for _, in := range []string{
		"permissions:\n  self: build\n", "permissions:\n  self: write\n",
		"permissions:\n  ci: [view, build]\n", "permissions:\n  ci: write\n",
	} {
		_, err := jobWith(t, in)
		if err == nil || !strings.Contains(err.Error(), "its own job") {
			t.Errorf("%q: got %v", in, err)
		}
	}
	if _, err := jobWith(t, "permissions:\n  self: [view, cancel]\n  ci: read\n"); err != nil {
		t.Errorf("view and cancel on itself are fine: %v", err)
	}
}

func TestMaxPermissionsIsRefusedOnAJobThatReadsNoRepoStages(t *testing.T) {
	if _, err := jobWith(t, "max_permissions:\n  other: view\n"); err == nil || !strings.Contains(err.Error(), "max_permissions") {
		t.Fatalf("got %v", err)
	}
	for name, in := range map[string]string{
		"source":      "name: ci\nmax_permissions:\n  other: view\nsource: {repo: a/b, path: ci.yml}\n",
		"multibranch": "name: ci\nmax_permissions:\n  other: view\nmultibranch: {repo: a/b}\n",
		"owner":       "name: ci\nmax_permissions:\n  other: view\nowner: {login: a, token: T}\n",
	} {
		if _, err := ParseJob("ci.yml", []byte(in)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestMaxPermissionsRefusesNoneAndBuildOnSelf(t *testing.T) {
	for _, extra := range []string{"max_permissions: none\n", "max_permissions:\n  self: write\n"} {
		in := "name: ci\n" + extra + "multibranch: {repo: a/b}\n"
		if _, err := ParseJob("ci.yml", []byte(in)); err == nil {
			t.Errorf("%q: want a refusal", extra)
		}
	}
}

func TestAJobNamingAnUnknownJobInItsPermissionsFailsTheLoad(t *testing.T) {
	_, err := loadFiles(t, map[string]string{
		"ci": "name: ci\npermissions:\n  nowhere: view\n" + stageBody,
	})
	if err == nil || !strings.Contains(err.Error(), `"nowhere", which is not a job`) {
		t.Fatalf("got %v", err)
	}
	_, err = loadFiles(t, map[string]string{
		"ci": "name: ci\nmax_permissions:\n  nowhere: view\nmultibranch: {repo: a/b}\n",
	})
	if err == nil || !strings.Contains(err.Error(), "max_permissions names") {
		t.Fatalf("got %v", err)
	}
}

func TestAJobMayNameAnotherJobAndAChildOfAMultibranchJob(t *testing.T) {
	_, err := loadFiles(t, map[string]string{
		"app": "name: app\nmultibranch: {repo: a/b}\n",
		"ci":  "name: ci\npermissions:\n  app: [view, build]\n  app.main: view\n" + stageBody,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAJobListingASecretNamedKippTokenGetsAWarning(t *testing.T) {
	snap, err := loadFiles(t, map[string]string{"ci": "name: ci\nsecrets: [KIPP_TOKEN]\n" + stageBody})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Warnings) != 1 || !strings.Contains(snap.Warnings[0], "KIPP_TOKEN") {
		t.Fatalf("warnings %q", snap.Warnings)
	}
}

func TestAStagesFileMayAskForPermissions(t *testing.T) {
	f, err := ParseStageFile([]byte("permissions:\n  other: [view, build]\n" + stageBody))
	if err != nil {
		t.Fatal(err)
	}
	if g := f.Permissions.Resolve("x.main")["other"]; !g.Build || !g.View {
		t.Fatalf("grant %+v", g)
	}
	if _, err := ParseStageFile([]byte("permissions:\n  other: delete\n" + stageBody)); err == nil {
		t.Fatal("an unknown action in a stages file is refused")
	}
}

func parent(t *testing.T, extra string) *Job {
	t.Helper()
	j, err := ParseJob("app.yml", []byte("name: app\n"+extra+"multibranch: {repo: a/b}\n"))
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestAChildKeepsItsParentsCeilingAndNotItsPermissions(t *testing.T) {
	p := parent(t, "permissions: {other: write}\nmax_permissions:\n  other: [view, build]\n")
	c := p.Child(ChildRecord{Parent: "app", Key: "main", Kind: KindBranch, Branch: "main"})
	if c.MaxPermissions == nil || c.Permissions != nil {
		t.Fatalf("child: max %+v, permissions %+v", c.MaxPermissions, c.Permissions)
	}
	if g := c.Grants(); len(g) != 1 || g["app.main"] != (Grant{View: true}) {
		t.Fatalf("a child holds the default, not its parent's grant: %+v", g)
	}
}

func TestAStagesFileWithinTheCeilingIsAllowed(t *testing.T) {
	c := parent(t, "max_permissions:\n  other: [view, build]\n  self: cancel\n").Child(ChildRecord{Parent: "app", Key: "main", Kind: KindBranch, Branch: "main"})
	f, err := ParseStageFile([]byte("permissions:\n  other: build\n  self: cancel\n" + stageBody))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RepoPermissions(f.Permissions); err != nil {
		t.Fatal(err)
	}
}

func TestAStagesFileOverTheCeilingNamesTheExcessAndTheParent(t *testing.T) {
	c := parent(t, "max_permissions:\n  other: view\n").Child(ChildRecord{Parent: "app", Key: "main", Kind: KindBranch, Branch: "main"})
	f, err := ParseStageFile([]byte("permissions:\n  other: write\n  third: read\n" + stageBody))
	if err != nil {
		t.Fatal(err)
	}
	err = c.RepoPermissions(f.Permissions)
	if err == nil {
		t.Fatal("want a refusal")
	}
	for _, want := range []string{"other: build, cancel", "third: view", "job app", "max_permissions"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q is missing from %v", want, err)
		}
	}
}

func TestWithNoCeilingOnlyTheDefaultIsAllowed(t *testing.T) {
	c := parent(t, "").Child(ChildRecord{Parent: "app", Key: "main", Kind: KindBranch, Branch: "main"})
	for in, ok := range map[string]bool{
		"": true, "permissions:\n  self: view\n": true, "permissions: none\n": true,
		"permissions:\n  self: cancel\n": false, "permissions:\n  other: read\n": false,
	} {
		f, err := ParseStageFile([]byte(in + stageBody))
		if err != nil {
			t.Fatal(err)
		}
		if got := c.RepoPermissions(f.Permissions) == nil; got != ok {
			t.Errorf("%q: allowed %v, want %v", in, got, ok)
		}
	}
}

func TestACeilingGrantedThroughAnInheritedJobDoesNotCount(t *testing.T) {
	c := parent(t, "max_permissions:\n  app: view\n").Child(ChildRecord{Parent: "app", Key: "main", Kind: KindBranch, Branch: "main"})
	f, err := ParseStageFile([]byte("permissions:\n  self: view\n  app.main: view\n" + stageBody))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RepoPermissions(f.Permissions); err != nil {
		t.Fatalf("self is the child's own name: %v", err)
	}
	f, _ = ParseStageFile([]byte("permissions:\n  app.other: view\n" + stageBody))
	if c.RepoPermissions(f.Permissions) == nil {
		t.Fatal("what is checked is what is written: app.other is not app")
	}
}

func TestAnOwnersCeilingBoundsItsReposAndTheirChildren(t *testing.T) {
	o := parseOwner(t, ownerJob+"max_permissions:\n  other: view\n")
	node := o.Node(RepoRecord{Owner: "acme", Key: "web", Repo: "acme/web"})
	child := node.Child(ChildRecord{Parent: node.Name, Key: "main", Kind: KindBranch, Branch: "main"})
	f, err := ParseStageFile([]byte("permissions:\n  other: view\n" + stageBody))
	if err != nil {
		t.Fatal(err)
	}
	if err := child.RepoPermissions(f.Permissions); err != nil {
		t.Fatalf("within the owner's ceiling: %v", err)
	}
	f, _ = ParseStageFile([]byte("permissions:\n  other: build\n" + stageBody))
	err = child.RepoPermissions(f.Permissions)
	if err == nil || !strings.Contains(err.Error(), "job acme would have to allow") {
		t.Fatalf("the owner job is the one to name: %v", err)
	}
}

func TestExcessIsSorted(t *testing.T) {
	f, _ := ParseStageFile([]byte("permissions:\n  b: write\n  a: build\n" + stageBody))
	got := f.Permissions.Excess(nil, "x")
	if !slices.IsSorted(got) || len(got) != 2 {
		t.Fatalf("excess %q", got)
	}
}
