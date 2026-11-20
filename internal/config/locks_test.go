package config

import (
	"slices"
	"strings"
	"testing"
)

func TestAStageOfTheJobFileTakesAnyLock(t *testing.T) {
	j, err := ParseJob("ci.yml", []byte("name: ci\nlocks: [checkout]\nstages:\n  build: {run: make}\n  deploy: {needs: [build], run: ./deploy, locks: [production, dns]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(j.Stages[1].Locks, []string{"production", "dns"}) || len(j.Stages[0].Locks) != 0 || !slices.Equal(j.Locks, []string{"checkout"}) {
		t.Fatalf("stage locks %v and %v, job locks %v", j.Stages[0].Locks, j.Stages[1].Locks, j.Locks)
	}
}

func TestAStagesLocksRefuseAnEmptyNameAndANameTwice(t *testing.T) {
	for in, want := range map[string]string{
		"[x, x]": "lock x twice",
		"['']":   "a lock has a name",
	} {
		_, err := ParseJob("ci.yml", []byte("name: ci\nstages:\n  s: {run: 'true', locks: "+in+"}\n"))
		if err == nil || !strings.Contains(err.Error(), "stage s: locks") || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v", in, err)
		}
	}
}

func TestAStagesFileReadsItsStagesLocks(t *testing.T) {
	f, err := ParseStageFile([]byte("stages:\n  deploy: {run: ./deploy, locks: [production]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.Stages[0].Locks, []string{"production"}) {
		t.Fatalf("got %v", f.Stages[0].Locks)
	}
}

func TestStageLocksIsRefusedOnAJobThatReadsNoRepoStages(t *testing.T) {
	if _, err := jobWith(t, "stage_locks: [production]\n"); err == nil || !strings.Contains(err.Error(), "stage_locks") {
		t.Fatalf("got %v", err)
	}
	for name, in := range map[string]string{
		"source":      "name: ci\nstage_locks: [production]\nsource: {repo: a/b, path: ci.yml}\n",
		"multibranch": "name: ci\nstage_locks: [production]\nmultibranch: {repo: a/b}\n",
		"owner":       "name: ci\nstage_locks: [production]\nowner: {login: a, token: T}\n",
	} {
		j, err := ParseJob("ci.yml", []byte(in))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !slices.Equal(j.StageLocks, []string{"production"}) {
			t.Errorf("%s: stage_locks %v", name, j.StageLocks)
		}
	}
	if _, err := ParseJob("ci.yml", []byte("name: ci\nstage_locks: [a, a]\nmultibranch: {repo: a/b}\n")); err == nil || !strings.Contains(err.Error(), "lock a twice") {
		t.Fatalf("a name twice: %v", err)
	}
}

func TestAStagesFileMayTakeOnlyTheLocksTheJobGrants(t *testing.T) {
	j, err := ParseJob("ci.yml", []byte("name: ci\nlocks: [checkout]\nstage_locks: [production]\nsource: {repo: a/b, path: ci.yml}\n"))
	if err != nil {
		t.Fatal(err)
	}
	granted := []*Stage{{Name: "deploy", Locks: []string{"production"}}, {Name: "build"}}
	if err := j.CheckStageLocks(granted); err != nil {
		t.Fatalf("a granted lock: %v", err)
	}
	err = j.CheckStageLocks([]*Stage{{Name: "deploy", Locks: []string{"production", "checkout"}}})
	if err == nil || !strings.Contains(err.Error(), "stage deploy: lock checkout") || !strings.Contains(err.Error(), "stage_locks") {
		t.Fatalf("a job lock is no grant to a stage: %v", err)
	}
}

func TestAChildKeepsItsParentsStageLocks(t *testing.T) {
	j, err := ParseJob("site.yml", []byte("name: site\nmultibranch: {repo: acme/site}\nstage_locks: [production]\n"))
	if err != nil {
		t.Fatal(err)
	}
	c := j.Child(ChildRecord{Parent: "site", Key: "main", Kind: KindBranch, Branch: "main", State: ChildLive})
	if !slices.Equal(c.StageLocks, []string{"production"}) {
		t.Fatalf("child %v", c.StageLocks)
	}
}

func TestARepoSettingsStageLocksReplaceTheOwnerJobs(t *testing.T) {
	j := parseOwner(t, "name: acme\nstage_locks: [shared]\nowner:\n  login: acme\n  token: GH_ACME\n  repos:\n    site:\n      stage_locks: [production]\n    api:\n      stage_locks: []\n")
	for repo, want := range map[string][]string{"acme/site": {"production"}, "acme/api": {}, "acme/docs": {"shared"}} {
		_, key, _ := strings.Cut(repo, "/")
		n := j.Node(RepoRecord{Owner: "acme", Key: key, Repo: repo})
		if !slices.Equal(n.StageLocks, want) {
			t.Errorf("%s: %v, want %v", repo, n.StageLocks, want)
		}
		if c := n.Child(ChildRecord{Parent: n.Name, Key: "main", Kind: KindBranch}); !slices.Equal(c.StageLocks, want) {
			t.Errorf("%s's child: %v, want %v", repo, c.StageLocks, want)
		}
	}
	if _, err := ParseJob("acme.yml", []byte("name: acme\nowner:\n  login: acme\n  token: T\n  repos:\n    site: {stage_locks: [x, x]}\n")); err == nil || !strings.Contains(err.Error(), "owner.repos.site.stage_locks") {
		t.Fatalf("a name twice: %v", err)
	}
}
