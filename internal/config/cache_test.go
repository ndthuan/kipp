package config

import (
	"strings"
	"testing"
)

func TestAStageNamesTheCacheScopesItReaches(t *testing.T) {
	job, err := ParseJob("a.yml", []byte("name: a\ncache_writes_default: true\nstages:\n  s: {cache_scopes: [acme, shared.tools], run: 'true'}\n  t: {run: 'true'}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := job.Stages[0].CacheScopes; len(got) != 2 || got[0] != "acme" || got[1] != "shared.tools" {
		t.Fatalf("scopes %v", got)
	}
	if len(job.Stages[1].CacheScopes) != 0 {
		t.Fatalf("a stage that names none has %v", job.Stages[1].CacheScopes)
	}
	if !job.CacheWritesDefault {
		t.Fatal("cache_writes_default was dropped")
	}
}

func TestACacheScopeIsANameOnceAndNotEmpty(t *testing.T) {
	for yml, why := range map[string]string{
		"{cache_scopes: [], run: 'true'}":     "cache_scopes",
		"{cache_scopes: [Acme], run: 'true'}": "scope",
		"{cache_scopes: [a/b], run: 'true'}":  "scope",
		"{cache_scopes: [a, a], run: 'true'}": "twice",
		"{cache_scopes: [\"\"], run: 'true'}": "scope",
	} {
		_, err := ParseJob("a.yml", []byte("name: a\nstages:\n  s: "+yml+"\n"))
		if err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("%s: error %v, want one saying %q", yml, err, why)
		}
	}
}

const declaredCache = `name: site-deploy
params:
  flavor: {type: choice, options: [a, b], default: a}
env:
  TOOLCHAIN: v2
secrets: [DEPLOY_KEY]
stages:
  s:
    cache:
      - key: "mod-${TOOLCHAIN}-${flavor}-${KIPP_JOB}"
        restore_keys: ["mod-${TOOLCHAIN}-"]
        paths: [~/.cache/mod, build/out]
        scope: acme
    run: 'true'
`

func TestAStageDeclaresItsCaches(t *testing.T) {
	job, err := ParseJob("site-deploy.yml", []byte(declaredCache))
	if err != nil {
		t.Fatal(err)
	}
	c := job.Stages[0].Cache
	if len(c) != 1 || c[0].Scope != "acme" || len(c[0].Paths) != 2 || len(c[0].RestoreKeys) != 1 {
		t.Fatalf("cache %+v", c)
	}
}

func TestAKeyExpandsParamsAndKippVariables(t *testing.T) {
	job, err := ParseJob("site-deploy.yml", []byte(declaredCache))
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"TOOLCHAIN": "v2", "flavor": "b", "KIPP_JOB": "site-deploy"}
	got, err := job.Stages[0].Cache[0].Expand(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	if got.Key != "mod-v2-b-site-deploy" || got.RestoreKeys[0] != "mod-v2-" {
		t.Fatalf("expanded to %+v", got)
	}
	if got.Paths[0] != "~/.cache/mod" {
		t.Fatalf("a path was changed: %v", got.Paths)
	}
	if _, err := job.Stages[0].Cache[0].Expand(func(string) (string, bool) { return "", false }); err == nil {
		t.Fatal("a variable the environment lacks expanded to nothing")
	}
}

func TestAnUnknownVariableInAKeyIsRefusedByCheck(t *testing.T) {
	for _, key := range []string{"go-${NOPE}", "go-${KIPP_NOPE}", "go-$HOME", "go-${}", "go-${A B}"} {
		yml := "name: a\nstages:\n  s: {cache: [{key: \"" + key + "\", paths: [x]}], run: 'true'}\n"
		_, err := loadOne(t, yml)
		if err == nil || !strings.Contains(err.Error(), "key") {
			t.Errorf("%s: error %v, want one about the key", key, err)
		}
	}
}

func TestASecretInAKeyIsRefusedByCheck(t *testing.T) {
	yml := "name: a\nsecrets: [DEPLOY_KEY]\nstages:\n  s: {cache: [{key: \"go-${DEPLOY_KEY}\", paths: [x]}], run: 'true'}\n"
	_, err := loadOne(t, yml)
	if err == nil || !strings.Contains(err.Error(), "DEPLOY_KEY") || !strings.Contains(err.Error(), "secret") {
		t.Fatalf("error %v, want one that says a secret is no part of a key", err)
	}
}

func TestADeclaredCacheNeedsAKeyAndPathsAndNamesAScopeOnce(t *testing.T) {
	for yml, why := range map[string]string{
		"{cache: [], run: 'true'}":                                            "cache",
		"{cache: [{paths: [x]}], run: 'true'}":                                "key",
		"{cache: [{key: a b, paths: [x]}], run: 'true'}":                      "key",
		"{cache: [{key: k}], run: 'true'}":                                    "paths",
		"{cache: [{key: k, paths: [x], scope: Acme}], run: 'true'}":           "scope",
		"{cache: [{key: k, paths: [x], restore_keys: ['a b']}], run: 'true'}": "restore_keys",
		"{cache: [{key: k, paths: [x]}, {key: k, paths: [y]}], run: 'true'}":  "twice",
		"{cache: [{key: k, paths: [x], bogus: 1}], run: 'true'}":              "bogus",
		"{cache: [{key: k, paths: [\"\"]}], run: 'true'}":                     "path",
	} {
		_, err := ParseJob("a.yml", []byte("name: a\nstages:\n  s: "+yml+"\n"))
		if err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("%s: error %v, want one saying %q", yml, err, why)
		}
	}
}

func TestASourceFilesCachesAreCheckedAgainstTheJob(t *testing.T) {
	job, err := ParseJob("a.yml", []byte("name: a\nsecrets: [TOKEN]\nsource: {repo: acme/site, path: ci/stages.yml}\n"))
	if err != nil {
		t.Fatal(err)
	}
	stages, err := stagesOf([]byte("stages:\n  s: {cache: [{key: \"k-${TOKEN}\", paths: [x]}], run: 'true'}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := job.CheckCaches(stages, nil); err == nil || !strings.Contains(err.Error(), "TOKEN") {
		t.Fatalf("a source file's cache named a secret and %v", err)
	}
}
