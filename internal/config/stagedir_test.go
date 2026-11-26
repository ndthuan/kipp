package config

import "testing"

func TestEachStageOnARunnerHasADirectoryOfItsOwnUnlessItContinuesALineOfNeeds(t *testing.T) {
	j, err := ParseJob("a.yml", []byte(`name: a
stages:
  checkout: {run: 'true'}
  staging: {needs: [checkout], run: 'true'}
  production: {needs: [staging], run: 'true'}
  run: {run: 'true'}
  mac: {run: 'true'}
  build: {run: 'true'}
  test: {needs: [build], run: 'true'}
  lint: {needs: [build], run: 'true'}
  pack: {needs: [test, lint], run: 'true'}
`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"checkout": "checkout", "staging": "checkout", "production": "checkout",
		"run": "run", "mac": "mac",
		"build": "build", "test": "test", "lint": "lint", "pack": "pack",
	}
	for name, dir := range want {
		if got := RunnerDir(j.Stages, name); got != dir {
			t.Errorf("stage %s: directory %q, want %q", name, got, dir)
		}
	}
}
