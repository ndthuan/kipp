package api

import (
	"testing"

	"github.com/ndthuan/kipp/internal/config"
)

func TestTheJobFilesEnvIsShownOverTheVariablesVarsFileGivesAndNamesItself(t *testing.T) {
	vars, err := config.ParseVars([]byte("global: {A: from-vars, B: from-vars}\n"))
	if err != nil {
		t.Fatal(err)
	}
	got, why := fileVars(vars, &config.Job{Name: "a", Env: map[string]string{"B": "mine", "C": "mine"}})
	want := []config.Var{{Name: "A", Value: "from-vars", Source: "_vars.yml global"}, {Name: "B", Value: "mine", Source: "job file"}, {Name: "C", Value: "mine", Source: "job file"}}
	if why != "" || len(got) != len(want) {
		t.Fatalf("vars %v (%q), want %v", got, why, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("var %d is %v, want %v", i, got[i], want[i])
		}
	}
}
