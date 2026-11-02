// Package sched parses cron specs as Jenkins reads them (five fields plus
// H) and computes the next fire time. H spreads a job inside its slot by a stable
// hash of a seed, the job name, so two jobs with "H 3 * * *" do not fire
// together and the same job always fires at the same minute.
package sched

import (
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed spec. A field is a set of allowed values, and a
// minute fires when all five allow it (Jenkins; cron's own "either day
// field" rule does not exist here).
type Schedule struct {
	minute, hour, dom, month, dow [64]bool
}

// field is one position of a spec. A bare H hashes over min to hashMax:
// a day of the month every month has, and each day of the week once.
type field struct {
	name              string
	min, max, hashMax int
}

var fields = [5]field{{"minute", 0, 59, 59}, {"hour", 0, 23, 23}, {"day of month", 1, 31, 28}, {"month", 1, 12, 12}, {"day of week", 0, 7, 6}}

// Parse reads a five-field spec. seed decides where H lands.
func Parse(spec, seed string) (*Schedule, error) {
	parts := strings.Fields(spec)
	if len(parts) != 5 {
		return nil, fmt.Errorf("cron %q: want 5 fields, got %d", spec, len(parts))
	}
	s := &Schedule{}
	sets := [5]*[64]bool{&s.minute, &s.hour, &s.dom, &s.month, &s.dow}
	h := fnv.New32a()
	_, _ = h.Write([]byte(seed))
	hash := int(h.Sum32() >> 1)
	for i, p := range parts {
		for item := range strings.SplitSeq(p, ",") {
			if err := parseItem(item, fields[i], hash+i, sets[i]); err != nil {
				return nil, fmt.Errorf("cron %q, %s: %w", spec, fields[i].name, err)
			}
		}
	}
	if s.dow[7] { // 7 is Sunday too
		s.dow[0] = true
	}
	return s, nil
}

// parseItem handles one comma-separated item: a value, or a range, "*" or
// H, each of those three with an optional /step.
func parseItem(item string, f field, hash int, set *[64]bool) error {
	base, stepText, stepped := strings.Cut(item, "/")
	step := 1
	if stepped {
		var err error
		if step, err = strconv.Atoi(stepText); err != nil || step < 1 {
			return fmt.Errorf("bad step in %q", item)
		}
	}
	if r, isH := strings.CutPrefix(base, "H"); isH {
		lo, hi, err := hashRange(r, f)
		if err != nil {
			return err
		}
		return fillHashed(lo, hi, step, hash, set)
	}
	lo, hi := f.min, f.max
	if base != "*" {
		var err error
		if lo, hi, err = parseRange(base, f); err != nil {
			return err
		}
		if stepped && !strings.Contains(base, "-") {
			return fmt.Errorf("%q: a step follows a range, * or H", item)
		}
	}
	for v := lo; v <= hi; v += step {
		set[v] = true
	}
	return nil
}

// hashRange is what may follow H: nothing, for the field's hashed range, or "(lo-hi)".
func hashRange(r string, f field) (int, int, error) {
	if r == "" {
		return f.min, f.hashMax, nil
	}
	inner, ok := strings.CutPrefix(r, "(")
	if ok {
		inner, ok = strings.CutSuffix(inner, ")")
	}
	if !ok || !strings.Contains(inner, "-") {
		return 0, 0, fmt.Errorf("%q: H stands alone or takes a range, as in H(0-5)", "H"+r)
	}
	return parseRange(inner, f)
}

// fillHashed is H: one value in [lo,hi], or with a step above one every
// step-th value from a hashed offset, so "H/15" fires four times an hour at
// stable minutes. A step the range has no room for is refused.
func fillHashed(lo, hi, step, hash int, set *[64]bool) error {
	size := hi - lo + 1
	if step > size {
		return fmt.Errorf("step %d is more than the %d values H has to choose from", step, size)
	}
	if step == 1 {
		set[lo+hash%size] = true
		return nil
	}
	for v := lo + hash%step; v <= hi; v += step {
		set[v] = true
	}
	return nil
}

func parseRange(r string, f field) (int, int, error) {
	lo, hi, found := strings.Cut(r, "-")
	a, err := strconv.Atoi(lo)
	if err != nil {
		return 0, 0, fmt.Errorf("%q is not a number", lo)
	}
	b := a
	if found {
		if b, err = strconv.Atoi(hi); err != nil {
			return 0, 0, fmt.Errorf("%q is not a number", hi)
		}
	}
	if a < f.min || b > f.max || a > b {
		return 0, 0, fmt.Errorf("%q is outside %d-%d", r, f.min, f.max)
	}
	return a, b, nil
}

// Next is the first minute after `after` the schedule matches, in after's
// location, or the zero time if none comes within five years. The spec is
// read on the wall clock of that location: a time the clocks skip fires at
// the first minute after the skip, and a time they repeat fires the first
// time only.
func (s *Schedule) Next(after time.Time) time.Time {
	zone := after.Location()
	wall := wallOf(after)
	end := wall.AddDate(5, 0, 0)
	for {
		if wall = s.nextWall(wall, end); wall.IsZero() {
			return time.Time{}
		}
		if t := instant(wall, zone); t.After(after) {
			return t
		}
	}
}

// nextWall is the first wall-clock minute after wall that matches, before
// end, or the zero time. Wall-clock minutes are held as UTC times, where
// every minute exists once.
func (s *Schedule) nextWall(wall, end time.Time) time.Time {
	t := wall.Truncate(time.Minute).Add(time.Minute)
	for t.Before(end) {
		switch {
		case !s.month[int(t.Month())]:
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		case !s.dom[t.Day()] || !s.dow[int(t.Weekday())]:
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, time.UTC)
		case !s.hour[t.Hour()]:
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, time.UTC)
		case !s.minute[t.Minute()]:
			t = t.Add(time.Minute)
		default:
			return t
		}
	}
	return time.Time{}
}

// wallOf is what t's clock reads, to the minute, as a UTC time.
func wallOf(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, time.UTC)
}

// instant is when zone's clock reads wall: the first time, if it reads it
// twice, and the first minute after the skip, if it skips it. time.Date
// leaves both cases unspecified. The offsets a day either side are the
// ones in force around a change of clocks; no zone changes twice in a day.
func instant(wall time.Time, zone *time.Location) time.Time {
	before, after := offset(wall.Add(-24*time.Hour), zone), offset(wall.Add(24*time.Hour), zone)
	var first time.Time
	for _, off := range []time.Duration{before, after} {
		t := wall.Add(-off)
		if wallOf(t.In(zone)).Equal(wall) && (first.IsZero() || t.Before(first)) {
			first = t
		}
	}
	if !first.IsZero() {
		return first.In(zone)
	}
	// Skipped: under the earlier offset the time is still to come, under
	// the later one it has passed; the change of clocks is between.
	t := wall.Add(-max(before, after))
	for !wallOf(t.In(zone)).After(wall) {
		t = t.Add(time.Minute)
	}
	return t.In(zone)
}

// offset is zone's offset from UTC at the instant t names when read as UTC.
func offset(t time.Time, zone *time.Location) time.Duration {
	_, secs := t.In(zone).Zone()
	return time.Duration(secs) * time.Second
}
