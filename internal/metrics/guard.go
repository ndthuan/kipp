package metrics

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// The verdicts of a guard on one item.
const (
	Held        = "held"
	NoBaseline  = "no baseline"
	Fell        = "fell"
	Missing     = "missing"
	NotReported = "not reported"
)

// The outcomes of a guard: how it ends the stage, held being not at all.
const (
	OutcomeHeld     = "held"
	OutcomeFailed   = "failed"
	OutcomeUnstable = "unstable"
)

// GuardResult is what one guard found of one item.
type GuardResult struct {
	Metric, Item  string
	Before, After *float64
	Tolerance     float64
	Verdict       string
	Outcome       string
	From          *From
}

// GuardAll is every result of the guards base holds, guard by guard and
// item by item, against what the stage reported.
func GuardAll(base File, reported []Metric) []GuardResult {
	var out []GuardResult
	for _, g := range base.Guards {
		out = append(out, Check(g, reported, base)...)
	}
	return out
}

// Check is what the guard finds of each item it names, an exact name or
// a path.Match pattern: the items of the metric the report holds or the
// store remembers. A guarded item neither holds is missing, so a guard
// whose pattern matches nothing is not passed for want of an item.
func Check(g FileGuard, reported []Metric, base File) []GuardResult {
	now := map[string]float64{}
	for _, m := range reported {
		if m.Name == g.Metric {
			maps.Copy(now, m.Values)
		}
	}
	var was FileMetric
	for _, m := range base.Metrics {
		if m.Name == g.Metric {
			was = m
		}
	}
	named := map[string]bool{}
	for _, values := range []map[string]float64{now, was.Values} {
		for item := range values {
			if guards(g.Item, item) {
				named[item] = true
			}
		}
	}
	items := slices.Sorted(maps.Keys(named))
	if len(items) == 0 {
		items = []string{g.Item}
	}
	out := make([]GuardResult, 0, len(items))
	for _, item := range items {
		out = append(out, judge(g, item, now, was))
	}
	return out
}

func guards(pattern, item string) bool {
	ok, err := path.Match(pattern, item)
	return item == pattern || (ok && err == nil)
}

func judge(g FileGuard, item string, now map[string]float64, was FileMetric) GuardResult {
	r := GuardResult{Metric: g.Metric, Item: item, Tolerance: g.Tolerance, Verdict: Held, Outcome: OutcomeHeld}
	if v, ok := was.Values[item]; ok {
		r.Before = &v
		if from, ok := was.From[item]; ok {
			r.From = &from
		}
	}
	v, reported := now[item]
	if reported {
		r.After = &v
	}
	switch {
	case !reported && g.Required:
		r.Verdict, r.Outcome = Missing, g.onFall()
	case !reported:
		r.Verdict = NotReported
	case r.Before == nil:
		r.Verdict = NoBaseline
	case fell(*r.Before, v, g.Tolerance):
		r.Verdict, r.Outcome = Fell, g.onFall()
	}
	return r
}

func fell(before, value, tolerance float64) bool {
	slack := 1e-9 * math.Max(1, math.Abs(before))
	return before-value > tolerance+slack
}

func (g FileGuard) onFall() string {
	if g.OnFall == OutcomeUnstable {
		return OutcomeUnstable
	}
	return OutcomeFailed
}

// Worst is how the results end a stage together: failed over unstable
// over held.
func Worst(rs []GuardResult) string {
	worst := OutcomeHeld
	for _, r := range rs {
		switch r.Outcome {
		case OutcomeFailed:
			return OutcomeFailed
		case OutcomeUnstable:
			worst = OutcomeUnstable
		}
	}
	return worst
}

// LinePrefix starts every line a guard writes in a stage's log.
const LinePrefix = "kipp: metrics guard: "

// Line is the result as the stage's log says it:
//
//	kipp: metrics guard: coverage total 87.19 -> 86.90 (-0.29), remembered from site-deploy #123, tolerance 0.01: failed
//
// A side with no value says why instead: "no baseline" for the one
// remembered, "missing" or "not reported" for the one reported.
func (r GuardResult) Line() string {
	before, after := NoBaseline, r.Verdict
	digits := 2
	if r.Before != nil && r.After != nil && *r.Before != *r.After && math.Abs(*r.Before-*r.After) < 0.005 {
		digits = max(decimals(*r.Before), decimals(*r.After))
	}
	if r.Before != nil {
		before = strconv.FormatFloat(*r.Before, 'f', digits, 64)
	}
	if r.After != nil {
		after = strconv.FormatFloat(*r.After, 'f', digits, 64)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s%s %s %s -> %s", LinePrefix, r.Metric, r.Item, before, after)
	if r.Before != nil && r.After != nil {
		delta := strconv.FormatFloat(*r.After-*r.Before, 'f', digits, 64)
		if !strings.HasPrefix(delta, "-") {
			delta = "+" + delta
		}
		fmt.Fprintf(&b, " (%s)", delta)
	}
	if r.Before != nil && r.From != nil {
		fmt.Fprintf(&b, ", remembered from %s #%d", r.From.Job, r.From.Number)
	}
	fmt.Fprintf(&b, ", tolerance %s: %s", strconv.FormatFloat(r.Tolerance, 'f', -1, 64), r.Outcome)
	return b.String()
}

func decimals(v float64) int {
	s := strconv.FormatFloat(v, 'f', -1, 64)
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return len(s) - i - 1
	}
	return 0
}

// The exit codes of metrics check.
const (
	CheckHeld      = 0
	CheckFailed    = 1
	CheckNoMetrics = 2
	CheckNoGuard   = 3
)

// CheckUsage is the command line of metrics check.
const CheckUsage = "metrics check <glob>"

// CheckCommand is metrics check <glob>, inside a stage: the guards
// KIPP_METRICS holds, against the report files the glob matches (a path,
// absolute or from the working directory, whose last element may be a
// pattern), each result a line on out. It needs no server and no token,
// so that a script deciding something (a merge, a deploy) learns the
// verdict before it decides; the guard after the stage still runs.
func CheckCommand(args []string, getenv func(string) string, out io.Writer) int {
	if len(args) != 1 || args[0] == "" {
		_, _ = fmt.Fprintf(out, "usage: kipp %s\n", CheckUsage)
		return CheckNoMetrics
	}
	base, err := readFile(getenv("KIPP_METRICS"))
	if err != nil {
		_, _ = fmt.Fprintf(out, "kipp: metrics check: %v\n", err)
		return CheckNoMetrics
	}
	if len(base.Guards) == 0 {
		_, _ = fmt.Fprintln(out, "kipp: metrics check: the stage declares no guard")
		return CheckNoGuard
	}
	dir, glob := splitGlob(args[0])
	reported, errs := ParseGlob(dir, glob)
	for _, err := range errs {
		_, _ = fmt.Fprintf(out, "kipp: metrics: %v\n", err)
	}
	results := GuardAll(base, reported)
	for _, r := range results {
		_, _ = fmt.Fprintln(out, r.Line())
	}
	if Worst(results) == OutcomeFailed {
		return CheckFailed
	}
	return CheckHeld
}

func readFile(p string) (File, error) {
	var f File
	if p == "" {
		return f, errors.New("KIPP_METRICS is not set: a stage that declares metrics has it")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return f, fmt.Errorf("KIPP_METRICS: %w", err)
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return f, fmt.Errorf("KIPP_METRICS %s: %w", p, err)
	}
	return f, nil
}

func splitGlob(p string) (dir, glob string) {
	if !filepath.IsAbs(p) && fs.ValidPath(filepath.ToSlash(p)) {
		return ".", filepath.ToSlash(p)
	}
	return filepath.Dir(p), filepath.Base(p)
}
