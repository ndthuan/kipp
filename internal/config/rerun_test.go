package config

import (
	"strings"
	"testing"
)

func TestRerunOnRestartIsOffByDefaultAndReadAsABoolean(t *testing.T) {
	j, err := ParseJob("a.yml", []byte("name: a\nstages:\n  s: {run: 'true'}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if j.RerunOnRestart {
		t.Fatal("rerun_on_restart is on without the key")
	}
	j, err = ParseJob("a.yml", []byte("name: a\nrerun_on_restart: true\nstages:\n  s: {run: 'true'}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !j.RerunOnRestart {
		t.Fatal("rerun_on_restart: true was dropped")
	}
}

func TestRerunOnRestartRefusesAValueThatIsNotABoolean(t *testing.T) {
	for _, v := range []string{"1", `"true"`, "maybe", "[true]"} {
		_, err := ParseJob("a.yml", []byte("name: a\nrerun_on_restart: "+v+"\nstages:\n  s: {run: 'true'}\n"))
		if err == nil || !strings.Contains(err.Error(), "bool") {
			t.Errorf("rerun_on_restart: %s: err %v, want a refusal naming bool", v, err)
		}
	}
}

func TestAChildOfAMultibranchJobKeepsItsParentsRerunOnRestart(t *testing.T) {
	j, err := ParseJob("site.yml", []byte("name: site\nrerun_on_restart: true\nmultibranch: {repo: acme/site}\n"))
	if err != nil {
		t.Fatal(err)
	}
	c := j.Child(ChildRecord{Parent: "site", Key: "pr-12", Kind: KindPR, Branch: "feature/login", PR: 12, State: ChildLive})
	if !c.RerunOnRestart {
		t.Fatal("the child of a parent with rerun_on_restart: true does not have it")
	}
}
