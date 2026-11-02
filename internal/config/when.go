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
	// Branches are names or path.Match patterns of the branch a webhook
	// build was pushed to, read as triggers.webhook.branches reads them.
	Branches []string `yaml:"branches"`
	// Tags are names or path.Match patterns of the tag a webhook build was
	// pushed for, read as triggers.webhook.tags reads them.
	Tags []string `yaml:"tags"`
	// TargetBranches are names or path.Match patterns of the branch a pull
	// request is merged into (KIPP_BASE_BRANCH). Only a pull request has
	// one, so a build of a branch or of a build by hand never holds.
	TargetBranches []string `yaml:"target_branches"`
	// DefaultBranch true holds on the repo's default branch, false
	// everywhere else.
	DefaultBranch *bool `yaml:"default_branch"`
	// Triggers are the KIPP_TRIGGER values the stage runs for.
	Triggers []string `yaml:"triggers"`
	// Params name params of the job, each with the values it holds for.
	Params map[string][]string `yaml:"params"`
	// NeedsStatus is a stage's alone: how its needs must have ended for
	// it to run, one of pipeline.NeedsStatuses; empty is success.
	NeedsStatus string `yaml:"needs_status"`
	// Status is a remember rule's alone: the statuses of the builds it
	// holds for. A stage runs before its build has one.
	Status []string `yaml:"status"`
}

// Push is what a build knows of the push that started it.
type Push struct {
	Trigger string
	// Branch and DefaultBranch are a webhook build's: the branch pushed to
	// and the repo's default branch as GitHub sent it.
	Branch, DefaultBranch string
	// Tag is the tag a webhook build was pushed for; such a build has no
	// Branch.
	Tag string
	// Base is the branch a pull request's build merges into, empty for
	// every other build.
	Base string
	// Params are the build's, defaults filled in.
	Params map[string]string
	// Pushed is a build that runs a push's branch and commit without being
	// the webhook: one made again from a webhook's.
	Pushed bool
	// Child is a build of a multibranch job's child, which records its
	// branch and the default branch whatever started it; PR is its pull
	// request, 0 for a branch.
	Child bool
	PR    int
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
		// A param the build lacks (the job gained it after the build was
		// queued) has no value, not the empty one.
		if v, ok := p.Params[name]; !ok || !slices.Contains(values, v) {
			return false
		}
	}
	return true
}

// listed reports whether a key of patterns holds for name: no patterns
// always does, and a build without the name (a branch for a tag's build)
// never matches one.
func listed(patterns []string, name string) bool {
	return len(patterns) == 0 || name != "" && matchesAny(patterns, name)
}

// check refuses a condition that holds for nothing it could be asked.
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

// empty reports whether the condition names none of the keys that hold or
// not for a push, which a stage's and a remember rule's share.
func (w *When) empty() bool {
	return len(w.Branches) == 0 && len(w.Tags) == 0 && len(w.TargetBranches) == 0 && w.DefaultBranch == nil && len(w.Triggers) == 0 && len(w.Params) == 0
}

// checkParams refuses a param the job does not have and a value it could
// never be given, and writes a bool's true and false as a build has them;
// where says whose condition it is.
func (w *When) checkParams(where string, params []Param) error {
	if w == nil {
		return nil
	}
	for _, name := range slices.Sorted(maps.Keys(w.Params)) {
		i := slices.IndexFunc(params, func(p Param) bool { return p.Name == name })
		if i < 0 {
			return fmt.Errorf("%s: when.params: %q is not a param of the job", where, name)
		}
		values, err := params[i].values(w.Params[name])
		if err != nil {
			return fmt.Errorf("%s: when.params.%s: %w", where, name, err)
		}
		w.Params[name] = values
	}
	return nil
}

// boolSpellings are what YAML reads as a bool, each as a build has it.
var boolSpellings = map[string]string{"true": "true", "True": "true", "TRUE": "true", "false": "false", "False": "false", "FALSE": "false"}

// values checks what a condition lists for the param, a bool's written as
// true or false whatever spelling YAML read as one.
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

// checkKeys refuses a pattern, a trigger or a pair of keys no build could
// match; where says whose condition it is.
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
