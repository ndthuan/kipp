package config

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"path"
	"reflect"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// MetricPrefix starts the names of the metrics kipp records by itself; a
// report may not use it, so that no stage can pass its numbers off as
// kipp's.
const MetricPrefix = "kipp."

// The metrics kipp records for every build.
const (
	StageSeconds = MetricPrefix + "stage_seconds" // per stage, item the stage's name
	CaseSeconds  = MetricPrefix + "case_seconds"  // per JUnit case, item "<suite> / <case>"
)

// PerCaseMetrics are the metrics whose item is a test case. A suite has
// thousands of cases and only a case's recent trend is read, so their
// history is capped (retain.CaseHistoryBuilds) where a job-level metric's
// is not.
var PerCaseMetrics = []string{CaseSeconds}

// RememberStatuses are the build statuses a remember rule may name: those
// of a build whose stages ran to their end. A cancelled, timed out or lost
// build measured only part of what it would have.
var RememberStatuses = []string{"success", "unstable", "failed"}

// defaultRememberStatus is what a rule that names no status holds for: a
// build that ran to its end without failing.
var defaultRememberStatus = []string{"success", "unstable"}

// JobMetrics is a job's metrics: the store its builds' numbers go to, and
// which of them the store remembers.
type JobMetrics struct {
	// Store names the store; empty is the job's name. Several jobs may
	// name one.
	Store string `yaml:"store"`
	// Remember are the rules, the first that names a metric deciding it;
	// none is DefaultRemember.
	Remember []RememberRule `yaml:"remember"`
}

// RememberRule says which metrics a build's report updates the store with,
// and in which builds.
type RememberRule struct {
	// Metrics are the names the rule decides; none is every metric.
	Metrics []string `yaml:"metrics"`
	When    When     `yaml:"when"`
	line    int
}

// DefaultRemember is the rule of a job that gives none: every metric, from
// a build on the default branch that did not fail.
func DefaultRemember() RememberRule {
	on := true
	return RememberRule{When: When{DefaultBranch: &on, Status: slices.Clone(defaultRememberStatus)}}
}

// UnmarshalYAML implements yaml.Unmarshaler, to keep the rule's line for
// what is said of it later.
func (r *RememberRule) UnmarshalYAML(n *yaml.Node) error {
	type plain RememberRule
	if err := knownKeys(n, reflect.TypeFor[plain]()); err != nil {
		return err
	}
	if err := n.Decode((*plain)(r)); err != nil {
		return err
	}
	r.line = n.Line
	return nil
}

// Covers reports whether the rule decides the metric.
func (r RememberRule) Covers(metric string) bool {
	return r.Metrics == nil || slices.Contains(r.Metrics, metric)
}

func (r *RememberRule) check(params []Param) error {
	where := fmt.Sprintf("line %d: metrics.remember", r.line)
	for _, m := range r.Metrics {
		if !ValidName(m) {
			return fmt.Errorf("%s: metric %q: lower-case letters, digits, dot, dash and underscore only", where, m)
		}
	}
	w := r.When
	if w.NeedsStatus != "" {
		return fmt.Errorf("%s: when.needs_status is a stage's: a rule is read once the build has ended, and has no needs", where)
	}
	if w.empty() && len(w.Status) == 0 {
		return fmt.Errorf("%s: a rule needs when: status, default_branch, branches, tags, target_branches, triggers or params", where)
	}
	for _, s := range w.Status {
		if !slices.Contains(RememberStatuses, s) {
			return fmt.Errorf("%s: when.status %q is not one of %v: a build that did not run to its end measured part of what it would have", where, s, RememberStatuses)
		}
	}
	if err := w.checkKeys(where); err != nil {
		return err
	}
	return r.When.checkParams(where, params)
}

// HoldsAt reports whether a build of the push that ended with status is
// one the condition holds for: the stage keys as Holds reads them, and the
// status among those named (success and unstable when none is).
func (w When) HoldsAt(p Push, status string) bool {
	if !slices.Contains(RememberStatuses, status) {
		return false
	}
	want := w.Status
	if len(want) == 0 {
		want = defaultRememberStatus
	}
	return slices.Contains(want, status) && w.Holds(p)
}

// StoreName is the store the job's builds report into.
func (j *Job) StoreName() string {
	if j.Metrics != nil && j.Metrics.Store != "" {
		return j.Metrics.Store
	}
	return j.Name
}

// RememberRules are the job's rules, or the default one.
func (j *Job) RememberRules() []RememberRule {
	if j.Metrics == nil || len(j.Metrics.Remember) == 0 {
		return []RememberRule{DefaultRemember()}
	}
	return j.Metrics.Remember
}

// Remembers reports whether a build of the push that ended with status
// updates the store with the metric: the first rule that names it decides,
// and a metric no rule names is kept in history only.
func (j *Job) Remembers(metric string, p Push, status string) bool {
	for _, r := range j.RememberRules() {
		if r.Covers(metric) {
			return r.When.HoldsAt(p, status)
		}
	}
	return false
}

func (j *Job) checkMetrics() error {
	m := j.Metrics
	if m == nil {
		return nil
	}
	if m.Store != "" && !ValidName(m.Store) {
		return fmt.Errorf("metrics.store %q: lower-case letters, digits, dot, dash and underscore only", m.Store)
	}
	for i := range m.Remember {
		if err := m.Remember[i].check(j.Params); err != nil {
			return err
		}
	}
	return nil
}

// StageMetrics is what a stage says of metrics: the report it leaves, and
// the guards on it. A stage that declares it is given what its store
// remembers, in the file KIPP_METRICS names, with or without a report of
// its own.
type StageMetrics struct {
	// File is a glob of the stage's report files, in its directory; empty
	// is none.
	File  string  `yaml:"file"`
	Guard []Guard `yaml:"guard"`
}

// Guard is a rule on a metric the stage reports, against what the store
// remembers.
type Guard struct {
	Metric string `yaml:"metric"`
	// Item is a name, or a path.Match pattern of the names, of the items
	// guarded.
	Item string `yaml:"item"`
	// NoLower is the one rule there is, and must be said: a later rule is
	// a key of its own, not another value of this one.
	NoLower   bool    `yaml:"no_lower"`
	Tolerance float64 `yaml:"tolerance"`
	// OnFall is how the stage ends when the value fell: failed (the
	// default) or unstable.
	OnFall string `yaml:"on_fall"`
	// Required nil is true: an item the report lacks fails the guard.
	Required *bool `yaml:"required"`
}

// IsRequired reports whether an item the report lacks fails the guard.
func (g Guard) IsRequired() bool { return g.Required == nil || *g.Required }

// check refuses metrics a stage could not report or guard as written, and
// fills a guard's defaults.
func (m *StageMetrics) check(stage string) error {
	if m == nil {
		return nil
	}
	if m.File != "" {
		if _, err := path.Match(m.File, ""); err != nil {
			return fmt.Errorf("stage %s: metrics.file %q is not a pattern path.Match reads", stage, m.File)
		}
		if !fs.ValidPath(m.File) {
			return fmt.Errorf("stage %s: metrics.file %q leaves the stage's directory", stage, m.File)
		}
	}
	if len(m.Guard) > 0 && m.File == "" {
		return fmt.Errorf("stage %s: metrics.guard reads the stage's report, and metrics names no file", stage)
	}
	for i := range m.Guard {
		if err := m.Guard[i].check(); err != nil {
			return fmt.Errorf("stage %s: metrics.guard %d: %w", stage, i+1, err)
		}
	}
	return nil
}

func (g *Guard) check() error {
	if g.OnFall == "" {
		g.OnFall = "failed"
	}
	_, err := path.Match(g.Item, "")
	switch {
	case !ValidName(g.Metric) || strings.HasPrefix(g.Metric, MetricPrefix):
		return fmt.Errorf("metric %q: a name a report may use: lower-case letters, digits, dot, dash and underscore, not starting %s", g.Metric, MetricPrefix)
	case g.Item == "":
		return errors.New("item: the name, or a path.Match pattern, of the items guarded")
	case err != nil:
		return fmt.Errorf("item %q is not a pattern path.Match reads", g.Item)
	case !g.NoLower:
		return errors.New("no_lower: true is the one rule a guard has, and it is said")
	case g.Tolerance < 0 || math.IsInf(g.Tolerance, 0) || math.IsNaN(g.Tolerance):
		return fmt.Errorf("tolerance %v: a number of zero or more", g.Tolerance)
	case g.OnFall != "failed" && g.OnFall != "unstable":
		return fmt.Errorf("on_fall %q: failed or unstable", g.OnFall)
	}
	return nil
}
