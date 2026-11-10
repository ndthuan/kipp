package config

import (
	"fmt"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Reuse is a stage's reuse: the stage ends success without running its
// command when a build kept in a job of From, or of the stage's own job,
// has a stage that ended success on the same git tree, with the same
// fingerprint and the same runner class.
type Reuse struct {
	// From names jobs; a name stands for that job and every job whose name
	// starts with it and a dot, its children and its repos' nodes.
	From []string `yaml:"from"`
	// Stage is the stage that must have ended success in a job of From. In
	// the stage's own job, the stage of its own name counts.
	Stage string `yaml:"stage"`
	// Tree is the git work tree whose tree is looked for, with $VARS
	// expanded; empty is the checkout kipp made for the build.
	Tree string `yaml:"tree"`
	// Force names a bool param of the job; true runs the stage in full.
	Force string `yaml:"force"`
}

// LocalClass is the runner class of a stage on the server's machine.
const LocalClass = "local"

// RunnerClass is the kind of place the stage runs: LocalClass, or its
// sorted labels and its image. A green run counts only for a stage of the
// same class, since the machine is part of what the run proved.
func (s *Stage) RunnerClass() string {
	if !s.Remote() {
		return LocalClass
	}
	class := "runs_on " + strings.Join(slices.Sorted(slices.Values(s.RunsOn)), ",")
	if s.Image != "" {
		class += " image " + s.Image
	}
	return class
}

// checkReuse refuses a reuse that names nothing to match, or that sits on
// a stage which would have produced something a skipped run cannot.
func (s *Stage) checkReuse() error {
	r := s.Reuse
	if r == nil {
		return nil
	}
	none := func(key string) error {
		return fmt.Errorf("stage %s: reuse with %s: a stage reused from a green run does not run, so it has no %s", s.Name, key, key)
	}
	switch {
	case len(r.From) == 0:
		return fmt.Errorf("stage %s: reuse.from names no job: the jobs whose green runs count", s.Name)
	case r.Stage == "":
		return fmt.Errorf("stage %s: reuse.stage: the stage that must have ended success in a job of reuse.from", s.Name)
	case !ValidName(r.Stage):
		return fmt.Errorf("stage %s: reuse.stage %q is not a stage name", s.Name, r.Stage)
	case r.Force != "" && !ValidEnvName(r.Force):
		return fmt.Errorf("stage %s: reuse.force %q is not a param name", s.Name, r.Force)
	case s.Retries > 0:
		return none("retries")
	case s.Junit != "":
		return none("junit")
	case s.Metrics != nil:
		return none("metrics")
	case s.Artifacts != nil:
		return none("artifacts")
	case r.Tree != "" && s.Remote():
		return fmt.Errorf("stage %s: reuse.tree with runs_on: the tree is read on the server's machine, and the stage's work tree is elsewhere", s.Name)
	}
	for _, from := range r.From {
		if !ValidName(from) {
			return fmt.Errorf("stage %s: reuse.from %q is not a job name", s.Name, from)
		}
	}
	return nil
}

// CheckReuse refuses a reuse whose force is not a bool param of the job,
// and in a job of its own stages (plain), one without a tree: kipp checks
// nothing out for such a job, so it knows no tree of its own.
func (j *Job) CheckReuse(stages []*Stage, plain bool) error {
	for _, s := range stages {
		r := s.Reuse
		if r == nil {
			continue
		}
		if plain && r.Tree == "" {
			return fmt.Errorf("stage %s: reuse.tree: the job checks out no repo of kipp's, so the stage names the work tree it gates", s.Name)
		}
		if r.Force == "" {
			continue
		}
		i := slices.IndexFunc(j.Params, func(p Param) bool { return p.Name == r.Force })
		switch {
		case i < 0:
			return fmt.Errorf("stage %s: reuse.force names %s, and the job has no param of that name", s.Name, r.Force)
		case j.Params[i].Type != "bool":
			return fmt.Errorf("stage %s: reuse.force names %s, a %s param; it must be a bool", s.Name, r.Force, j.Params[i].Type)
		}
	}
	return nil
}

// fingerprintOf reads a fingerprint: absent is none, and anything given
// is a list of one or more commands, each a string.
func fingerprintOf(where string, n yaml.Node) ([]string, error) {
	if n.Kind == 0 {
		return nil, nil
	}
	bad := fmt.Errorf("line %d: %s: a list of one or more shell commands, each a string, like [go version]", n.Line, where)
	if n.Kind != yaml.SequenceNode || len(n.Content) == 0 {
		return nil, bad
	}
	out := make([]string, 0, len(n.Content))
	for _, c := range n.Content {
		if c.Kind != yaml.ScalarNode || c.ShortTag() != "!!str" || strings.TrimSpace(c.Value) == "" {
			return nil, bad
		}
		out = append(out, c.Value)
	}
	return out, nil
}
