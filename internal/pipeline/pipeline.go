// Package pipeline resolves a build's stage DAG: which stages may start,
// what a finished stage releases, and what a failed one skips.
package pipeline

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
)

type state int

const (
	pending state = iota
	running
	success
	unstable
	failed
	// skipped is a stage that did not run because a stage before it
	// failed: to what needs it, a failure.
	skipped
	// passed is a stage that did not run because its own condition, or
	// its rule on how its needs ended, did not hold: it neither failed
	// nor succeeded.
	passed
)

// Outcome is how a stage ended, as the stages that need it see it.
type Outcome int

// The outcomes. The zero value is Failed, so that an outcome nobody set
// releases nothing.
const (
	Failed Outcome = iota
	// Unstable is a stage that ran to its end with something to report:
	// what needs it runs as after a success.
	Unstable
	Succeeded
)

// The rules a stage may have on how its needs ended, a job file's
// needs_status. None is NeedsSuccess.
const (
	// NeedsSuccess runs the stage once every need succeeded or ended
	// unstable.
	NeedsSuccess = "success"
	// NeedsFailure runs it once every need has ended and at least one
	// failed or was skipped by a failure.
	NeedsFailure = "failure"
	// NeedsAlways runs it once every need has ended, however.
	NeedsAlways = "always"
)

// NeedsStatuses are the rules, in the order a message lists them.
var NeedsStatuses = []string{NeedsSuccess, NeedsFailure, NeedsAlways}

// DAG is the live state of one build's stages.
type DAG struct {
	needs map[string][]string
	on    map[string]string
	state map[string]state
	depth map[string]int
}

// Validate refuses unknown needs, self needs and cycles.
func Validate(needs map[string][]string) error {
	_, err := depths(needs)
	return err
}

// New builds a DAG; the maps are copied. on gives a stage its rule on how
// its needs ended; a stage it does not name has NeedsSuccess.
func New(needs map[string][]string, on map[string]string) (*DAG, error) {
	depth, err := depths(needs)
	if err != nil {
		return nil, err
	}
	if err := checkRules(needs, on); err != nil {
		return nil, err
	}
	d := &DAG{needs: map[string][]string{}, on: maps.Clone(on), state: map[string]state{}, depth: depth}
	for n, ns := range needs {
		d.needs[n] = slices.Clone(ns)
		d.state[n] = pending
	}
	return d, nil
}

// checkRules refuses a rule kipp does not have, one for no stage, and one
// on a stage with no needs: nothing could ever release that stage.
func checkRules(needs map[string][]string, on map[string]string) error {
	for _, n := range slices.Sorted(maps.Keys(on)) {
		ns, ok := needs[n]
		switch rule := on[n]; {
		case !ok:
			return fmt.Errorf("needs_status for %q, which is not a stage", n)
		case rule != "" && !slices.Contains(NeedsStatuses, rule):
			return fmt.Errorf("stage %q: needs_status %q is not one of %v", n, rule, NeedsStatuses)
		case rule != "" && len(ns) == 0:
			return fmt.Errorf("stage %q has a needs_status and needs nothing", n)
		}
	}
	return nil
}

type walker struct {
	needs    map[string][]string
	depth    map[string]int
	visiting map[string]bool
}

// depths assigns each stage 1 + its deepest need, and finds cycles on the
// way. The stages are walked by name, so that one bad graph always gives
// the same error.
func depths(needs map[string][]string) (map[string]int, error) {
	w := &walker{needs: needs, depth: map[string]int{}, visiting: map[string]bool{}}
	for _, n := range slices.Sorted(maps.Keys(needs)) {
		if _, err := w.visit(n); err != nil {
			return nil, err
		}
	}
	return w.depth, nil
}

func (w *walker) visit(n string) (int, error) {
	if d, ok := w.depth[n]; ok {
		return d, nil
	}
	if w.visiting[n] {
		return 0, fmt.Errorf("stage %q needs itself, through a cycle", n)
	}
	w.visiting[n] = true
	d := 0
	for _, m := range w.needs[n] {
		if _, ok := w.needs[m]; !ok {
			return 0, fmt.Errorf("stage %q needs %q, which is not a stage", n, m)
		}
		md, err := w.visit(m)
		if err != nil {
			return 0, err
		}
		d = max(d, md+1)
	}
	w.visiting[n] = false
	w.depth[n] = d
	return d, nil
}

// Ready lists, sorted, the pending stages that may start now, as their
// rule on how their needs ended says.
func (d *DAG) Ready() []string {
	var out []string
	for n, s := range d.state {
		if s == pending && d.verdict(n) == start {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// Start marks a stage running.
func (d *DAG) Start(name string) { d.state[name] = running }

// Finish records a stage's outcome, and settles what that decides: the
// pending stages that will now never run, sorted.
func (d *DAG) Finish(name string, how Outcome) []string {
	switch how {
	case Succeeded:
		d.state[name] = success
	case Unstable:
		d.state[name] = unstable
	default:
		d.state[name] = failed
	}
	return d.settle()
}

// Skip marks a pending stage skipped before it runs, as a stage whose
// condition does not hold is, and settles what that decides. The names
// skipped, name first, come back; a stage no longer pending gives none.
func (d *DAG) Skip(name string) []string {
	if d.state[name] != pending {
		return nil
	}
	d.state[name] = passed
	return append([]string{name}, d.settle()...)
}

// What a pending stage's needs decide for it.
type verdict int

const (
	wait verdict = iota
	start
	skip // as a failure
	pass // as neither a failure nor a success
)

// ends is what a stage's needs have done so far.
type ends struct{ open, dead, passed bool }

func (d *DAG) ends(name string) ends {
	var e ends
	for _, m := range d.needs[name] {
		switch d.state[m] {
		case pending, running:
			e.open = true
		case failed, skipped:
			e.dead = true
		case passed:
			e.passed = true
		}
	}
	return e
}

// verdict applies the stage's rule to how its needs ended. The success
// rule skips as soon as one need failed; otherwise every rule waits for
// every need, so that a stage beside a condition that did not hold and a
// need that will fail is skipped as a failure whichever ends first.
func (d *DAG) verdict(name string) verdict {
	e := d.ends(name)
	switch d.on[name] {
	case NeedsAlways:
		if e.open {
			return wait
		}
		return start
	case NeedsFailure:
		switch {
		case e.open:
			return wait
		case e.dead:
			return start
		}
		return pass
	}
	switch {
	case e.dead:
		return skip
	case e.open:
		return wait
	case e.passed:
		return pass
	}
	return start
}

// settle skips every pending stage its needs have decided will not run,
// until none is left; the names come back sorted.
func (d *DAG) settle() []string {
	var names []string
	for changed := true; changed; {
		changed = false
		for n, s := range d.state {
			if s != pending {
				continue
			}
			switch d.verdict(n) {
			case skip:
				d.state[n] = skipped
			case pass:
				d.state[n] = passed
			default:
				continue
			}
			names = append(names, n)
			changed = true
		}
	}
	slices.Sort(names)
	return names
}

// Done is true when no stage is pending or running.
func (d *DAG) Done() bool {
	for s := range maps.Values(d.state) {
		if s == pending || s == running {
			return false
		}
	}
	return true
}

// Failed is true when any stage failed.
func (d *DAG) Failed() bool { return d.any(failed) }

// Unstable is true when any stage ended unstable, whatever the others did.
func (d *DAG) Unstable() bool { return d.any(unstable) }

func (d *DAG) any(want state) bool {
	for s := range maps.Values(d.state) {
		if s == want {
			return true
		}
	}
	return false
}

// Depth is 0 for a stage with no needs, else 1 + its deepest need.
func (d *DAG) Depth(name string) int { return d.depth[name] }

// Names lists every stage, sorted by depth then name, for display.
func (d *DAG) Names() []string {
	names := slices.Collect(maps.Keys(d.state))
	slices.SortFunc(names, func(a, b string) int {
		return cmp.Or(cmp.Compare(d.depth[a], d.depth[b]), cmp.Compare(a, b))
	})
	return names
}
