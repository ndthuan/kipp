// Package placement chooses where a stage with runs_on runs: an image asks
// for a Docker engine, no image for a standing runner, and either must carry
// every label the stage names. It knows nothing of how a place is reached.
package placement

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"
)

// The two kinds of place.
const (
	KindRunner = "runner"
	KindEngine = "engine"
)

// Candidate is one place as it is now.
type Candidate struct {
	Name      string
	Kind      string
	Labels    []string
	Free      bool
	Reachable bool
	Why       string
	Disabled  bool
	Idle      time.Time
	Weight    int
	Room      int
	Reuse     bool
}

// Want is what a stage asks for.
type Want struct {
	Labels []string
	Image  string
}

func (w Want) kind() string {
	if w.Image != "" {
		return KindEngine
	}
	return KindRunner
}

// Why is the reason nothing was picked.
type Why string

// The reasons. Only WhyNone is a mistake: nothing that exists could ever
// take the stage, and waiting would hide it. The others pass.
const (
	Picked         Why = ""
	WhyNone        Why = "none"
	WhyBusy        Why = "busy"
	WhyUnreachable Why = "unreachable"
)

// Pick is where w runs, or why nowhere yet, with a line that says it.
func Pick(w Want, cs []Candidate) (Candidate, Why, string) {
	kind := w.kind()
	var carriers, others []Candidate
	for _, c := range cs {
		switch {
		case !carries(c.Labels, w.Labels):
		case c.Kind == kind:
			carriers = append(carriers, c)
		default:
			others = append(others, c)
		}
	}
	if len(carriers) == 0 {
		return Candidate{}, WhyNone, none(kind, w.Labels, len(others) > 0)
	}
	ready := slices.DeleteFunc(slices.Clone(carriers), func(c Candidate) bool { return !c.Reachable || (!c.Free && !c.Reuse) })
	if len(ready) > 0 {
		slices.SortStableFunc(ready, better)
		return ready[0], Picked, ""
	}
	if kind == KindEngine && !slices.ContainsFunc(carriers, func(c Candidate) bool { return c.Reachable }) {
		c := carriers[0]
		return Candidate{}, WhyUnreachable, fmt.Sprintf("waiting for engine %s (%s)", c.Name, c.Why)
	}
	return Candidate{}, WhyBusy, fmt.Sprintf("waiting for %s with labels %s", article(kind), Show(w.Labels))
}

// Available reports whether a place that may take w is there to wait for:
// one of its kind that carries every label, answers and is enabled, busy
// or not. A stage with somewhere else to go goes there only when none is.
func Available(w Want, cs []Candidate) bool {
	kind := w.kind()
	return slices.ContainsFunc(cs, func(c Candidate) bool {
		return c.Kind == kind && carries(c.Labels, w.Labels) && c.Reachable && !c.Disabled
	})
}

func better(a, b Candidate) int {
	switch {
	case a.Reuse != b.Reuse:
		if a.Reuse {
			return -1
		}
		return 1
	case a.Kind == KindRunner:
		return cmp.Or(cmp.Compare(b.Weight, a.Weight), a.Idle.Compare(b.Idle))
	}
	return b.Room - a.Room
}

func carries(have, want []string) bool {
	for _, l := range want {
		if !slices.Contains(have, l) {
			return false
		}
	}
	return true
}

// Show is labels as a stage's log names them: [linux, arm64].
func Show(labels []string) string { return "[" + strings.Join(labels, ", ") + "]" }

func none(kind string, labels []string, other bool) string {
	switch {
	case !other:
		return "no runner or engine carries labels " + Show(labels)
	case kind == KindEngine:
		return "no engine carries labels " + Show(labels) + ": a stage with an image runs in a container on an engine"
	}
	return "no runner carries labels " + Show(labels) + ": a stage with no image runs on a standing runner"
}

func article(kind string) string {
	if kind == KindEngine {
		return "an engine"
	}
	return "a runner"
}
