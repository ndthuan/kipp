// Package placement chooses where a stage with runs_on runs: an image asks
// for a Docker engine, no image for a standing agent, and either must carry
// every label the stage names. It knows nothing of how a place is reached.
package placement

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// The two kinds of place.
const (
	KindAgent  = "agent"
	KindEngine = "engine"
)

// Candidate is one place as it is now.
type Candidate struct {
	Name   string
	Kind   string
	Labels []string
	// Free is a place that takes a stage now: an agent online, enabled and
	// with an executor to spare; an engine enabled and below its cap.
	Free bool
	// Reachable is false for an engine whose last probe failed, and for an
	// agent that is offline. Why says what failed, and since when.
	Reachable bool
	Why       string
	// Idle is since when an agent has had nothing to run.
	Idle time.Time
	// Room is how many more containers an engine may start; Reuse is an
	// engine that already runs a container of this build and image, which
	// takes the stage whatever its room.
	Room  int
	Reuse bool
}

// Want is what a stage asks for.
type Want struct {
	Labels []string
	Image  string
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
	kind := KindAgent
	if w.Image != "" {
		kind = KindEngine
	}
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

// better orders the places that may take a stage: a container of the build
// first, then the agent idle longest, or the engine with the most room.
func better(a, b Candidate) int {
	switch {
	case a.Reuse != b.Reuse:
		if a.Reuse {
			return -1
		}
		return 1
	case a.Kind == KindAgent:
		return a.Idle.Compare(b.Idle)
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

// none says that nothing could ever take the stage, and, when a place of
// the other kind carries its labels, which kind it asks for and why.
func none(kind string, labels []string, other bool) string {
	switch {
	case !other:
		return "no agent or engine carries labels " + Show(labels)
	case kind == KindEngine:
		return "no engine carries labels " + Show(labels) + ": a stage with an image runs in a container on an engine"
	}
	return "no agent carries labels " + Show(labels) + ": a stage with no image runs on a standing agent"
}

func article(kind string) string {
	if kind == KindEngine {
		return "an engine"
	}
	return "an agent"
}
