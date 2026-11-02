package config

import (
	"strings"
	"testing"
)

// stagesOf reads the stages of a stages file, for tests that care for
// nothing else in it.
func stagesOf(data []byte) ([]*Stage, error) {
	f, err := ParseStageFile(data)
	if err != nil {
		return nil, err
	}
	return f.Stages, nil
}

// loadOne loads a directory of one job file, a.yml.
func loadOne(t *testing.T, yml string) (*Snapshot, error) {
	t.Helper()
	return loadFiles(t, map[string]string{"a": yml})
}

// loadFiles loads a directory of the named files, each name without .yml.
func loadFiles(t *testing.T, files map[string]string) (*Snapshot, error) {
	t.Helper()
	dir := t.TempDir()
	writeJobs(t, dir, files)
	return Load(dir)
}

func jobFile(name, extra string) string {
	return "name: " + name + "\n" + extra + "stages:\n  s: {run: 'true'}\n"
}

func TestVarsFileIsNoJobAndItsVariablesReachTheJobsThatItNames(t *testing.T) {
	snap, err := loadFiles(t, map[string]string{
		"_vars":      "global: {REGISTRY: registry.example.com}\ngroups:\n  site: {DEPLOY_HOST: staging.example.com}\njobs:\n  'site-*': {SMOKE_URL: 'https://staging.example.com'}\n",
		"site-smoke": jobFile("site-smoke", "group: site\n"),
		"other":      jobFile("other", ""),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Names) != 2 {
		t.Fatalf("jobs %v: the vars file is no job", snap.Names)
	}
	got, err := snap.Vars.For(snap.Jobs["site-smoke"])
	if err != nil {
		t.Fatal(err)
	}
	want := []Var{
		{"DEPLOY_HOST", "staging.example.com", "_vars.yml group site"},
		{"REGISTRY", "registry.example.com", "_vars.yml global"},
		{"SMOKE_URL", "https://staging.example.com", "_vars.yml jobs site-*"},
	}
	if len(got) != len(want) {
		t.Fatalf("vars %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("var %d is %v, want %v", i, got[i], want[i])
		}
	}
	other, _ := snap.Vars.For(snap.Jobs["other"])
	if len(other) != 1 || other[0].Name != "REGISTRY" {
		t.Fatalf("a job of no group and no pattern gets the global ones only: %v", other)
	}
}

func TestAMoreSpecificVarsLevelOverridesTheOneBefore(t *testing.T) {
	v, err := ParseVars([]byte("global: {A: g, B: g, C: g}\ngroups:\n  site: {B: grp, C: grp}\njobs:\n  'site-*': {C: pat}\n"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := v.For(&Job{Name: "site-web", Group: "site"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"A": "g", "B": "grp", "C": "pat"}
	for _, x := range got {
		if want[x.Name] != x.Value {
			t.Errorf("%s is %q, want %q", x.Name, x.Value, want[x.Name])
		}
	}
}

func TestAChildAndARepoJobMatchAPatternByTheNameOfTheFileTheyComeFrom(t *testing.T) {
	v, err := ParseVars([]byte("jobs:\n  'site-*': {X: '1'}\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range []*Job{{Name: "acme-site--main", Parent: "site-ci"}, {Name: "acme-web", Folder: "site-acme"}} {
		if got, _ := v.For(j); len(got) != 1 {
			t.Errorf("%s got %v", j.Name, got)
		}
	}
	if got, _ := v.For(&Job{Name: "web"}); len(got) != 0 {
		t.Errorf("a job the pattern does not name got %v", got)
	}
}

func TestVarsFileRefusesWhatNoStageCouldBeGiven(t *testing.T) {
	for why, src := range map[string]string{
		"unknown key":          "global: {A: b}\nextra: {}\n",
		"a KIPP_ name":         "global: {KIPP_X: b}\n",
		"CI":                   "groups:\n  site: {CI: 'no'}\n",
		"a name a shell lacks": "jobs:\n  a: {'1A': b}\n",
		"a pattern unread":     "jobs:\n  'a[': {A: b}\n",
		"a group name":         "groups:\n  Site: {A: b}\n",
		"a not-mapping":        "global: [a]\n",
		"two documents":        "global: {A: b}\n---\nglobal: {B: c}\n",
	} {
		if _, err := ParseVars([]byte(src)); err == nil {
			t.Errorf("%s: accepted", why)
		}
	}
	if _, err := ParseVars([]byte("global: {A: \"a\\0b\"}\n")); err == nil {
		t.Error("a zero byte accepted")
	}
}

func TestABadVarsFileFailsTheLoadAsABadJobFileDoes(t *testing.T) {
	_, err := loadFiles(t, map[string]string{"_vars": "global: {KIPP_X: b}\n", "a": jobFile("a", "")})
	if err == nil || !strings.Contains(err.Error(), "_vars.yml") || !strings.Contains(err.Error(), "KIPP_X") {
		t.Fatalf("Load = %v", err)
	}
}

func TestTwoPatternsSettingOneNameForAJobAreRefusedNamingBoth(t *testing.T) {
	_, err := loadFiles(t, map[string]string{
		"_vars":    "jobs:\n  'site-*': {URL: a}\n  '*-web': {URL: b}\n",
		"site-web": jobFile("site-web", ""),
	})
	if err == nil || !strings.Contains(err.Error(), `"*-web"`) || !strings.Contains(err.Error(), `"site-*"`) || !strings.Contains(err.Error(), "URL") {
		t.Fatalf("Load = %v", err)
	}
	if _, err := loadFiles(t, map[string]string{
		"_vars": "jobs:\n  'site-*': {URL: a}\n  '*-web': {URL: b}\n",
		"other": jobFile("other", ""),
	}); err != nil {
		t.Fatalf("patterns that clash on no job: %v", err)
	}
}

func TestAPatternVariableNamedLikeASecretTheJobListsFailsTheLoadAGlobalOneDoesNot(t *testing.T) {
	job := jobFile("a", "secrets: [TOKEN123]\n")
	if _, err := loadFiles(t, map[string]string{"_vars": "jobs:\n  a: {TOKEN123: x}\n", "a": job}); err == nil || !strings.Contains(err.Error(), "TOKEN123") {
		t.Fatalf("a pattern's: Load = %v", err)
	}
	if _, err := loadFiles(t, map[string]string{"_vars": "global: {TOKEN123: x}\n", "a": job}); err != nil {
		t.Fatalf("a global one is the build's to refuse: %v", err)
	}
}

func TestAnEnvMayNotNameKippOrCIOrASecretWhereverItIsDeclared(t *testing.T) {
	for why, yml := range map[string]string{
		"job env KIPP_":        "name: a\nenv: {KIPP_X: '1'}\nstages:\n  s: {run: x}\n",
		"job env CI":           "name: a\nenv: {CI: '1'}\nstages:\n  s: {run: x}\n",
		"job env a secret":     "name: a\nsecrets: [TOKEN123]\nenv: {TOKEN123: '1'}\nstages:\n  s: {run: x}\n",
		"stage env KIPP_":      "name: a\nstages:\n  s: {run: x, env: {KIPP_X: '1'}}\n",
		"stage env CI":         "name: a\nstages:\n  s: {run: x, env: {CI: '1'}}\n",
		"stage env a secret":   "name: a\nsecrets: [TOKEN123]\nstages:\n  s: {run: x, env: {TOKEN123: '1'}}\n",
		"stage env a bad name": "name: a\nstages:\n  s: {run: x, env: {'a-b': '1'}}\n",
	} {
		if _, err := ParseJob("a.yml", []byte(yml)); err == nil {
			t.Errorf("%s: accepted", why)
		}
	}
}

func TestAStagesFileTakesAnEnvAndRefusesTheSameNames(t *testing.T) {
	f, err := ParseStageFile([]byte("env: {MODE: fast}\nstages:\n  s: {run: x, env: {LEVEL: '2'}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Env["MODE"] != "fast" || f.Stages[0].Env["LEVEL"] != "2" {
		t.Fatalf("file %+v stage %+v", f.Env, f.Stages[0].Env)
	}
	for _, src := range []string{
		"env: {KIPP_X: a}\nstages:\n  s: {run: x}\n",
		"env: {CI: a}\nstages:\n  s: {run: x}\n",
		"stages:\n  s: {run: x, env: {KIPP_X: a}}\n",
		"secrets: [A]\nstages:\n  s: {run: x}\n",
	} {
		if _, err := ParseStageFile([]byte(src)); err == nil {
			t.Errorf("accepted %q", src)
		}
	}
}

func TestACacheKeyMayNameAVariableAnyFileGivesTheJob(t *testing.T) {
	key := func(name string) string {
		return "name: a\nstages:\n  s: {cache: [{key: 'k-${" + name + "}', paths: [x]}], run: x, env: {OWN: '1'}}\n"
	}
	files := func(name string) map[string]string {
		return map[string]string{"_vars": "global: {FROM_VARS: '1'}\n", "a": key(name)}
	}
	for _, name := range []string{"FROM_VARS", "OWN"} {
		if _, err := loadFiles(t, files(name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := loadFiles(t, files("NOPE")); err == nil {
		t.Error("a name no file gives the job was accepted")
	}
	if _, err := loadFiles(t, map[string]string{"a": key("FROM_VARS")}); err == nil {
		t.Error("a vars name without a vars file was accepted")
	}
}

func TestTheVarsFileIsPartOfTheSnapshotHash(t *testing.T) {
	a, err := loadFiles(t, map[string]string{"_vars": "global: {A: '1'}\n", "a": jobFile("a", "")})
	if err != nil {
		t.Fatal(err)
	}
	b, err := loadFiles(t, map[string]string{"_vars": "global: {A: '2'}\n", "a": jobFile("a", "")})
	if err != nil {
		t.Fatal(err)
	}
	if a.Hash == b.Hash {
		t.Fatal("a changed variable left the hash as it was")
	}
}
