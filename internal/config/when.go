package config

import (
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/ndthuan/kipp/internal/pipeline"
)

// TriggerKinds are the ways a build is started, as KIPP_TRIGGER names them.
var TriggerKinds = []string{"cron", "api", "web", "webhook", "after", "scan"}

// When is a stage's condition: every key given must hold for the stage to
// run, and a stage whose condition does not hold is skipped, as is what
// needs it under the success rule. A build not started by a push (cron,
// api, web, after) runs on the repo's default branch and has no branch
// name of its own.
type When struct {
	Branches       []string            `yaml:"branches"`
	Tags           []string            `yaml:"tags"`
	TargetBranches []string            `yaml:"target_branches"`
	DefaultBranch  *bool               `yaml:"default_branch"`
	Triggers       []string            `yaml:"triggers"`
	Params         map[string][]string `yaml:"params"`
	NeedsStatus    string              `yaml:"needs_status"`
	Status         []string            `yaml:"status"`
}

// Push is what a build knows of the push that started it.
type Push struct {
	Trigger               string
	Branch, DefaultBranch string
	Tag                   string
	Base                  string
	Params                map[string]string
	Pushed                bool
	Child                 bool
	PR                    int
}

// OnDefault reports whether the build runs on the default branch. A push
// whose default branch was not kept (a build queued by an older kipp) is
// not on it, so that a stage meant for the default branch alone is never
// run on a guess. A child's build is on it when it builds the default
// branch, whatever started it: a pull request never is.
func (p Push) OnDefault() bool {
	if p.Child {
		return p.PR == 0 && p.DefaultBranch != "" && p.Branch == p.DefaultBranch
	}
	if p.Trigger != "webhook" && !p.Pushed {
		return true
	}
	return p.DefaultBranch != "" && p.Branch == p.DefaultBranch
}

// Holds reports whether a stage under this condition runs for the push.
// No condition always holds.
func (w *When) Holds(p Push) bool {
	if w == nil {
		return true
	}
	if len(w.Triggers) > 0 && !slices.Contains(w.Triggers, p.Trigger) {
		return false
	}
	if w.DefaultBranch != nil && *w.DefaultBranch != p.OnDefault() {
		return false
	}
	if !listed(w.Branches, p.Branch) || !listed(w.Tags, p.Tag) || !listed(w.TargetBranches, p.Base) {
		return false
	}
	for name, values := range w.Params {
		if v, ok := p.Params[name]; !ok || !slices.Contains(values, v) {
			return false
		}
	}
	return true
}

func listed(patterns []string, name string) bool {
	return len(patterns) == 0 || name != "" && matchesAny(patterns, name)
}

func (w *When) check(stage string) error {
	if w == nil {
		return nil
	}
	if len(w.Status) > 0 {
		return fmt.Errorf("stage %s: when.status is read in metrics.remember, once a build has a status; a stage runs before its build has one", stage)
	}
	if w.empty() && w.NeedsStatus == "" {
		return fmt.Errorf("stage %s: when needs branches, tags, target_branches, default_branch, triggers, params or needs_status", stage)
	}
	if w.NeedsStatus != "" && !slices.Contains(pipeline.NeedsStatuses, w.NeedsStatus) {
		return fmt.Errorf("stage %s: when.needs_status %q is not one of %v", stage, w.NeedsStatus, pipeline.NeedsStatuses)
	}
	return w.checkKeys("stage " + stage)
}

func (w *When) empty() bool {
	return len(w.Branches) == 0 && len(w.Tags) == 0 && len(w.TargetBranches) == 0 && w.DefaultBranch == nil && len(w.Triggers) == 0 && len(w.Params) == 0
}

func (w *When) checkParams(where string, params []Param) error {
	if w == nil {
		return nil
	}
	return checkParamValues(where+": when.params", w.Params, params)
}

func checkParamValues(key string, listed map[string][]string, params []Param) error {
	for _, name := range slices.Sorted(maps.Keys(listed)) {
		i := slices.IndexFunc(params, func(p Param) bool { return p.Name == name })
		if i < 0 {
			return fmt.Errorf("%s: %q is not a param of the job", key, name)
		}
		values, err := params[i].values(listed[name])
		if err != nil {
			return fmt.Errorf("%s.%s: %w", key, name, err)
		}
		listed[name] = values
	}
	return nil
}

var boolSpellings = map[string]string{"true": "true", "True": "true", "TRUE": "true", "false": "false", "False": "false", "FALSE": "false"}

func (p Param) values(listed []string) ([]string, error) {
	if len(listed) == 0 {
		return nil, errors.New("no value, so it would hold for no build")
	}
	out := make([]string, len(listed))
	for i, v := range listed {
		switch {
		case p.Type == "bool":
			b, ok := boolSpellings[v]
			if !ok {
				return nil, fmt.Errorf("%q: a bool param is true or false", v)
			}
			v = b
		case p.Type == "choice" && !slices.Contains(p.Options, v):
			return nil, fmt.Errorf("%q is not one of %s", v, strings.Join(p.Options, ", "))
		}
		out[i] = v
	}
	return out, nil
}

// CheckWhenParams refuses stages whose conditions name a param the job
// does not have, or a value it could not be given. A job's own stages are
// checked as it is read; a source file's, by the build that reads them.
func (j *Job) CheckWhenParams(stages []*Stage) error {
	for _, s := range stages {
		if err := s.When.checkParams("stage "+s.Name, j.Params); err != nil {
			return err
		}
	}
	return nil
}

func (w *When) checkKeys(where string) error {
	for _, list := range []struct {
		key      string
		patterns []string
	}{{"branches", w.Branches}, {"tags", w.Tags}, {"target_branches", w.TargetBranches}} {
		for _, p := range list.patterns {
			if _, err := path.Match(p, ""); err != nil {
				return fmt.Errorf("%s: when.%s: %q is not a pattern path.Match reads", where, list.key, p)
			}
		}
	}
	for _, t := range w.Triggers {
		if !slices.Contains(TriggerKinds, t) {
			return fmt.Errorf("%s: when.triggers: %q is not one of %v", where, t, TriggerKinds)
		}
	}
	if len(w.Branches) > 0 && len(w.Triggers) > 0 && !slices.Contains(w.Triggers, "webhook") {
		return errors.New(where + ": when.branches names a pushed branch, and only a webhook build has one, but when.triggers leaves webhook out")
	}
	if len(w.Tags) > 0 && len(w.Triggers) > 0 && !slices.Contains(w.Triggers, "webhook") {
		return errors.New(where + ": when.tags names a pushed tag, and only a webhook build has one, but when.triggers leaves webhook out")
	}
	if len(w.Tags) > 0 && len(w.Branches) > 0 {
		return errors.New(where + ": when.tags and when.branches hold for no build: a push is to a branch or of a tag")
	}
	return nil
}
