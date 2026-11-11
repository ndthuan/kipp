package config

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/ndthuan/kipp/internal/cache"
)

// CacheSpec is one cache a stage declares: kipp restores it before the
// stage's command and saves it after, so that the command itself says
// nothing of the cache.
type CacheSpec struct {
	// Key names the entry; ${NAME} in it is read from the stage's
	// environment when the stage starts.
	Key string `yaml:"key"`
	// RestoreKeys are prefixes of keys to restore the newest entry of when
	// Key has none, tried in order. Such a restore is not an exact hit, so
	// the stage's result is saved under Key.
	RestoreKeys []string `yaml:"restore_keys"`
	// Paths are what the entry holds, relative to the stage's directory,
	// absolute or under ~/.
	Paths []string `yaml:"paths"`
	// Scope is the cache scope of the entry; empty is the stage's first
	// cache_scopes, else the job's name.
	Scope string `yaml:"scope"`
}

// varRe is a reference to a variable in a key. It is the only syntax a key
// expands: another $ is left as it is, and a key with one is refused as a
// key.
var varRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// KeyVars are the variables kipp sets for a build that a cache key may
// name, besides the job's params and env. The rest of KIPP_* is left out:
// it is not a name, a token or an address a key may carry.
var KeyVars = []string{
	"KIPP_JOB", "KIPP_BUILD", "KIPP_TRIGGER", "KIPP_BRANCH", "KIPP_DEFAULT_BRANCH",
	"KIPP_TAG", "KIPP_COMMIT", "KIPP_PR", "KIPP_BASE_BRANCH", "KIPP_BASE_COMMIT",
	"KIPP_UPSTREAM_BUILD", "KIPP_UPSTREAM_STATUS", "KIPP_UPSTREAM_COMMIT", "KIPP_UPSTREAM_TAG", "KIPP_STAGE",
}

// Expand is the spec with its keys read against lookup, which says a
// variable's value and whether it has one. A variable it lacks, or a key
// that is no key once expanded (a branch with a slash in it), is an error.
func (c CacheSpec) Expand(lookup func(string) (string, bool)) (CacheSpec, error) {
	out := c
	var err error
	if out.Key, err = expandKey(c.Key, lookup); err != nil {
		return c, fmt.Errorf("key %q: %w", c.Key, err)
	}
	out.RestoreKeys = make([]string, len(c.RestoreKeys))
	for i, k := range c.RestoreKeys {
		if out.RestoreKeys[i], err = expandKey(k, lookup); err != nil {
			return c, fmt.Errorf("restore_keys %q: %w", k, err)
		}
	}
	return out, nil
}

func expandKey(k string, lookup func(string) (string, bool)) (string, error) {
	var missing string
	out := varRe.ReplaceAllStringFunc(k, func(ref string) string {
		name := ref[2 : len(ref)-1]
		v, ok := lookup(name)
		if !ok && missing == "" {
			missing = name
		}
		return v
	})
	switch {
	case missing != "":
		return "", fmt.Errorf("${%s} is not set in this stage", missing)
	case !cache.ValidKey(out):
		return "", fmt.Errorf("is %q once expanded, which is not a key: 1 to 512 letters, digits and . _ + = -", out)
	}
	return out, nil
}

// checkCache refuses a cache that names no cache, one with no key or no
// path, a scope that is no name, or a key twice, on the stage's own. What
// its keys may name needs the job: Job.CheckCaches.
func (s *Stage) checkCache() error {
	if s.Cache != nil && len(s.Cache) == 0 {
		return fmt.Errorf("stage %s: cache declares no cache; leave it out for a stage with none", s.Name)
	}
	type id struct{ scope, key string }
	var seen []id
	for i, c := range s.Cache {
		where := fmt.Sprintf("stage %s: cache %d", s.Name, i+1)
		if err := c.check(where); err != nil {
			return err
		}
		at := id{c.Scope, c.Key}
		if slices.Contains(seen, at) {
			return fmt.Errorf("%s: key %q twice in scope %q", where, c.Key, c.Scope)
		}
		seen = append(seen, at)
	}
	return nil
}

func (c CacheSpec) check(where string) error {
	if c.Key == "" {
		return fmt.Errorf("%s: key is needed", where)
	}
	if err := checkKeyShape(c.Key); err != nil {
		return fmt.Errorf("%s: key %q: %w", where, c.Key, err)
	}
	for _, k := range c.RestoreKeys {
		if err := checkKeyShape(k); err != nil {
			return fmt.Errorf("%s: restore_keys %q: %w", where, k, err)
		}
	}
	if len(c.Paths) == 0 {
		return fmt.Errorf("%s: paths names no path", where)
	}
	for _, p := range c.Paths {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("%s: path is empty", where)
		}
	}
	if c.Scope != "" && !ValidName(c.Scope) {
		return fmt.Errorf("%s: scope %q: lower-case letters, digits, dot, dash and underscore only", where, c.Scope)
	}
	return nil
}

// checkKeyShape refuses a key that no value of its variables could make a
// key: what is left when each reference is one letter.
func checkKeyShape(k string) error {
	if !cache.ValidKey(varRe.ReplaceAllString(k, "x")) {
		return fmt.Errorf("is not a key: 1 to 512 letters, digits and . _ + = -, with ${NAME} for a variable")
	}
	return nil
}

// CheckCaches refuses stages whose cache keys name a variable the stage
// will not have: a secret, which a key would show in a file name and on
// the Cache page, or a name that is no param, env entry or KIPP_* variable
// of the build. extra are the names the job is given from outside its own
// file: what _vars.yml gives it, and a stages file's top-level env. A
// stage's own env counts for its keys. Load checks a job's own stages, once
// _vars.yml is read; a source file's are checked by the build that reads it.
func (j *Job) CheckCaches(stages []*Stage, extra []string) error {
	known := j.keyVars(extra)
	for _, s := range stages {
		for i, c := range s.Cache {
			for _, k := range append([]string{c.Key}, c.RestoreKeys...) {
				if err := j.checkKeyVars(k, known, s.Env); err != nil {
					return fmt.Errorf("stage %s: cache %d: key %q: %w", s.Name, i+1, k, err)
				}
			}
		}
	}
	return nil
}

func (j *Job) keyVars(extra []string) map[string]bool {
	known := map[string]bool{}
	for _, p := range j.Params {
		known[p.Name] = true
	}
	for k := range maps.Keys(j.Env) {
		known[k] = true
	}
	for _, k := range slices.Concat(KeyVars, extra) {
		known[k] = true
	}
	return known
}

func (j *Job) checkKeyVars(k string, known map[string]bool, stageEnv map[string]string) error {
	for _, m := range varRe.FindAllStringSubmatch(k, -1) {
		_, ownEnv := stageEnv[m[1]]
		switch name := m[1]; {
		case slices.Contains(j.Secrets, name):
			return fmt.Errorf("${%s} is a secret, and a secret is no part of a key", name)
		case !known[name] && !ownEnv:
			return fmt.Errorf("${%s} is no param, env entry or KIPP_* variable of this job (%s)", name, strings.Join(KeyVars, ", "))
		}
	}
	return nil
}
