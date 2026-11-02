package config

import (
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
)

// VarsFile is the file at the top of the jobs directory that declares
// variables once for the jobs that name them. It is no job: its name does
// not fit a job's.
const VarsFile = "_vars.yml"

// Vars are the variables VarsFile declares: for every job, for the jobs of
// a group, and for the jobs a path.Match pattern names. They are plain
// values, never masked.
type Vars struct {
	Global map[string]string
	Groups map[string]map[string]string
	Jobs   map[string]map[string]string
}

// varsFile is the YAML shape; strict decoding refuses unknown keys.
type varsFile struct {
	Global map[string]string            `yaml:"global"`
	Groups map[string]map[string]string `yaml:"groups"`
	Jobs   map[string]map[string]string `yaml:"jobs"`
}

// The source a Var names for a value that came from the patterns.
const jobsSource = VarsFile + " jobs "

// Var is one variable a job gets from VarsFile, and where it came from.
type Var struct {
	Name, Value, Source string
}

// FromJobs reports whether the value came from a jobs: pattern.
func (v Var) FromJobs() bool { return strings.HasPrefix(v.Source, jobsSource) }

// ParseVars reads VarsFile.
func ParseVars(data []byte) (*Vars, error) {
	var f varsFile
	if err := decodeDoc(data, &f); err != nil {
		return nil, err
	}
	v := &Vars{Global: f.Global, Groups: f.Groups, Jobs: f.Jobs}
	if err := checkEnvMap("global", v.Global); err != nil {
		return nil, err
	}
	for _, g := range slices.Sorted(maps.Keys(v.Groups)) {
		if !ValidName(g) {
			return nil, fmt.Errorf("groups: %q is no group name: lower-case letters, digits, dot, dash and underscore only", g)
		}
		if err := checkEnvMap("groups."+g, v.Groups[g]); err != nil {
			return nil, err
		}
	}
	for _, p := range slices.Sorted(maps.Keys(v.Jobs)) {
		if _, err := path.Match(p, ""); err != nil {
			return nil, fmt.Errorf("jobs: %q is not a pattern path.Match reads", p)
		}
		if err := checkEnvMap("jobs."+p, v.Jobs[p]); err != nil {
			return nil, err
		}
	}
	return v, nil
}

// checkEnvMap refuses a declared name or value no stage could be given.
func checkEnvMap(where string, env map[string]string) error {
	for _, k := range slices.Sorted(maps.Keys(env)) {
		if err := CheckEnvName(k); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		if strings.ContainsRune(env[k], 0) {
			return fmt.Errorf("%s: %s: a zero byte cannot be in a stage's environment", where, k)
		}
	}
	return nil
}

// For is what VarsFile gives j, by name, with the source of each value: the
// global entries, then its group's, then those of the patterns that name
// it, each overriding the one before. Two patterns setting one name are an
// error naming both. A nil Vars gives none.
func (v *Vars) For(j *Job) ([]Var, error) {
	if v == nil {
		return nil, nil
	}
	got := map[string]Var{}
	for k, val := range v.Global {
		got[k] = Var{k, val, VarsFile + " global"}
	}
	if j.Group != "" {
		for k, val := range v.Groups[j.Group] {
			got[k] = Var{k, val, VarsFile + " group " + j.Group}
		}
	}
	setBy := map[string]string{}
	for _, p := range slices.Sorted(maps.Keys(v.Jobs)) {
		if !matchesJob(p, j) {
			continue
		}
		for k, val := range v.Jobs[p] {
			if prev, ok := setBy[k]; ok {
				return nil, fmt.Errorf("%s: jobs %q and %q both set %s for job %s", VarsFile, prev, p, k, j.Name)
			}
			setBy[k] = p
			got[k] = Var{k, val, jobsSource + p}
		}
	}
	return slices.SortedFunc(maps.Values(got), func(a, b Var) int { return strings.Compare(a.Name, b.Name) }), nil
}

// CheckEnvName refuses a name no env: or _vars.yml entry may declare: one a
// shell does not take, one starting with KIPP_, which kipp sets, and CI.
func CheckEnvName(name string) error {
	switch {
	case !ValidEnvName(name):
		return fmt.Errorf("env %q: a name a shell accepts", name)
	case strings.HasPrefix(name, "KIPP_"):
		return fmt.Errorf("env %s: names starting with KIPP_ are kipp's own", name)
	case name == "CI":
		return errors.New("env CI: kipp sets it")
	}
	return nil
}

// CheckVarNames refuses a variable that names a secret the job lists, which
// a plain value would hide behind or be hidden by.
func (j *Job) CheckVarNames(where string, names []string) error {
	for _, n := range names {
		if slices.Contains(j.Secrets, n) {
			return fmt.Errorf("%s: env %s is a secret this job lists", where, n)
		}
	}
	return nil
}
