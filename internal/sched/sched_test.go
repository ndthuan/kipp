package sched

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

var zone = time.FixedZone("UTC+9", 9*60*60)

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func at(s string) time.Time { return must(time.ParseInLocation("2006-01-02 15:04", s, zone)) }

func TestAStepOnAStarCountsFromTheFieldsLowEnd(t *testing.T) {
	s := must(Parse("0 */2 * * *", "x"))
	got := s.Next(at("2026-10-10 13:05"))
	if want := at("2026-10-10 14:00"); !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestListsRangesAndStepsInEveryField(t *testing.T) {
	s := must(Parse("0,30 7-9 1 */3 1-5", "x"))
	// 1 Jan 2027 is a Friday: 07:00, 07:30, 08:00 ...
	got := s.Next(at("2026-12-31 23:59"))
	if want := at("2027-01-01 07:00"); !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if got2 := s.Next(got); !got2.Equal(at("2027-01-01 07:30")) {
		t.Fatalf("got %v", got2)
	}
}

func TestHSpreadsByNameAndIsStable(t *testing.T) {
	a := must(Parse("H 3 * * *", "tmp-cleanup"))
	b := must(Parse("H 3 * * *", "tmp-cleanup"))
	c := must(Parse("H 3 * * *", "weekly-report"))
	na, nb, nc := a.Next(at("2026-10-10 00:00")), b.Next(at("2026-10-10 00:00")), c.Next(at("2026-10-10 00:00"))
	if !na.Equal(nb) {
		t.Fatalf("H is not stable: %v %v", na, nb)
	}
	if na.Hour() != 3 || nc.Hour() != 3 {
		t.Fatalf("H left the hour: %v %v", na, nc)
	}
	if na.Equal(nc) {
		t.Fatalf("two names landed on the same minute %v; the hash does not spread", na)
	}
}

func TestHRangeStaysInside(t *testing.T) {
	s := must(Parse("H(0-29) * * * *", "x"))
	n := s.Next(at("2026-10-10 00:00"))
	if n.Minute() > 29 {
		t.Fatalf("H(0-29) gave minute %d", n.Minute())
	}
}

// Jenkins: all five fields must match; there is no day-of-month or
// day-of-week rule of cron's.
func TestBothDayFieldsMustMatch(t *testing.T) {
	s := must(Parse("0 0 13 * 5", "x"))
	// After 10 Oct 2026 the next Friday the 13th is in November.
	got := s.Next(at("2026-10-10 00:00"))
	if want := at("2026-11-13 00:00"); !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	s = must(Parse("0 0 */2 * 1", "x"))
	// Mondays on an odd day of the month: 19 Oct, then 9 Nov.
	got = s.Next(at("2026-10-10 00:00"))
	if want := at("2026-10-19 00:00"); !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if got, want := s.Next(got), at("2026-11-09 00:00"); !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestRefusesBadSpecs(t *testing.T) {
	for _, bad := range []string{
		"", "* * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *", "* * * * 8", "a * * * *", "H(5-70) * * * *", "*/0 * * * *",
		"5-3 * * * *", "1- * * * *", "-5 * * * *", "5/10 * * * *",
		"H(0-5)/10 * * * *", "H/70 * * * *", "* * H/29 * *", "* * * * H/8",
		"H5 * * * *", "H(5 * * * *", "H5) * * * *", "H(5) * * * *", "H(0-5 * * * *", "H0-5 * * * *", "H(0-5)x * * * *", "H() * * * *",
	} {
		if _, err := Parse(bad, "x"); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestNeverFiringReturnsZero(t *testing.T) {
	s := must(Parse("0 0 31 2 *", "x"))
	if n := s.Next(at("2026-10-10 00:00")); !n.IsZero() {
		t.Fatalf("31 Feb fired at %v", n)
	}
}

// minutes lists the minutes of one hour the schedule fires in.
func minutes(t *testing.T, spec, seed string) []int {
	t.Helper()
	s, err := Parse(spec, seed)
	if err != nil {
		t.Fatal(err)
	}
	var out []int
	hour := at("2026-10-10 05:00")
	for n := s.Next(hour.Add(-time.Minute)); !n.IsZero() && n.Before(hour.Add(time.Hour)); n = s.Next(n) {
		out = append(out, n.Minute())
	}
	return out
}

var seeds = []string{"a", "b", "site-smoke", "cleanup", "weekly-report", "x", "y", "z"}

// Jenkins: a step of one is no step, so H/1 is H.
func TestHWithAStepOfOneIsOneValueLikeH(t *testing.T) {
	for _, seed := range seeds {
		plain, stepped := minutes(t, "H * * * *", seed), minutes(t, "H/1 * * * *", seed)
		if len(stepped) != 1 || stepped[0] != plain[0] {
			t.Fatalf("seed %q: H/1 fires at %v, H at %v", seed, stepped, plain)
		}
	}
}

func TestHWithAStepFiresEveryStepFromAStableOffset(t *testing.T) {
	offsets := map[int]bool{}
	for _, seed := range seeds {
		got := minutes(t, "H/15 * * * *", seed)
		if len(got) != 4 || got[0] >= 15 {
			t.Fatalf("seed %q: H/15 gave %v", seed, got)
		}
		for i, m := range got {
			if m != got[0]+15*i {
				t.Fatalf("seed %q: H/15 gave %v, not 15 apart", seed, got)
			}
		}
		offsets[got[0]] = true
	}
	if len(offsets) < 2 {
		t.Fatalf("every seed landed on the same offset: %v", offsets)
	}
}

func TestHRangeGivesOneValueInsideForEverySeed(t *testing.T) {
	for _, seed := range seeds {
		got := minutes(t, "H(10-14) * * * *", seed)
		if len(got) != 1 || got[0] < 10 || got[0] > 14 {
			t.Fatalf("seed %q: H(10-14) gave %v", seed, got)
		}
		s := must(Parse("H H(0-5) * * *", seed))
		n := s.Next(at("2026-10-10 12:00"))
		if n.Hour() > 5 || !s.Next(n).Equal(n.AddDate(0, 0, 1)) {
			t.Fatalf("seed %q: H H(0-5) fired at %v then %v", seed, n, s.Next(n))
		}
	}
}

func TestAnHStepWiderThanItsRangeSaysBoth(t *testing.T) {
	_, err := Parse("H(0-5)/10 * * * *", "x")
	if err == nil || !strings.Contains(err.Error(), "step 10") || !strings.Contains(err.Error(), "6 values") {
		t.Fatalf("got %v", err)
	}
}

// Jenkins hashes a day of the month over 1-28, so that it exists in every
// month, and a day of the week over 0-6.
func TestHInADayFieldExistsEveryMonthAndWeek(t *testing.T) {
	days := map[int]bool{}
	for i := range 200 {
		seed := "job-" + strconv.Itoa(i)
		s := must(Parse("H H H * *", seed))
		n := s.Next(at("2027-02-01 00:00").Add(-time.Minute))
		if n.Month() != time.February || n.Day() > 28 {
			t.Fatalf("seed %q: a hashed day of the month skipped February: %v", seed, n)
		}
		if next := s.Next(n); next.Month() != time.March || next.Day() != n.Day() {
			t.Fatalf("seed %q: fired %v then %v", seed, n, next)
		}
		days[n.Day()] = true
		w := must(Parse("0 0 * * H", seed))
		first := w.Next(at("2026-10-10 00:00"))
		if !w.Next(first).Equal(first.AddDate(0, 0, 7)) {
			t.Fatalf("seed %q: a hashed day of the week fired %v then %v", seed, first, w.Next(first))
		}
	}
	if len(days) != 28 {
		t.Fatalf("200 seeds landed on %d days of the month, want all of 1-28", len(days))
	}
	// Written out, both fields keep their whole range.
	if _, err := Parse("0 0 29-31 * 5-7", "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse("0 0 H(1-31) * H(0-7)", "x"); err != nil {
		t.Fatal(err)
	}
}

func TestAStepOnARangeStartsAtItsLowEnd(t *testing.T) {
	got := minutes(t, "5-59/20 * * * *", "x")
	if len(got) != 3 || got[0] != 5 || got[1] != 25 || got[2] != 45 {
		t.Fatalf("5-59/20 gave %v", got)
	}
}

func TestSevenIsSundayToo(t *testing.T) {
	for _, spec := range []string{"0 0 * * 7", "0 0 * * 0", "0 0 * * 5-7"} {
		s := must(Parse(spec, "x"))
		// 10 Oct 2026 is a Saturday.
		if got, want := s.Next(at("2026-10-10 00:00")), at("2026-10-11 00:00"); !got.Equal(want) {
			t.Fatalf("%s: got %v want %v", spec, got, want)
		}
	}
}

// fires lists the minutes from from to to at which a tick, as the
// dispatcher makes one, fires s read in zone.
func fires(s *Schedule, zone *time.Location, from, to time.Time) []string {
	var out []string
	for m := from; m.Before(to); m = m.Add(time.Minute) {
		if s.Next(m.Add(-time.Minute).In(zone)).Equal(m) {
			out = append(out, m.In(zone).Format("01-02 15:04 MST"))
		}
	}
	return out
}

func TestATimeTheClocksSkipFiresOnceAtTheFirstMinuteAfterThem(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	// 2026-03-29: 02:00 CET is 03:00 CEST.
	from, to := time.Date(2026, 3, 28, 22, 0, 0, 0, time.UTC), time.Date(2026, 3, 29, 3, 0, 0, 0, time.UTC)
	for spec, want := range map[string]string{
		"30 2 * * *":   "[03-29 03:00 CEST]",
		"*/20 2 * * *": "[03-29 03:00 CEST]",
		"0 * * * *":    "[03-28 23:00 CET 03-29 00:00 CET 03-29 01:00 CET 03-29 03:00 CEST 03-29 04:00 CEST]",
		"30 3 * * *":   "[03-29 03:30 CEST]",
	} {
		s, err := Parse(spec, "x")
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprint(fires(s, paris, from, to)); got != want {
			t.Errorf("%s: %s, want %s", spec, got, want)
		}
	}
	s, _ := Parse("30 2 * * *", "x")
	if got := s.Next(time.Date(2026, 3, 29, 0, 0, 0, 0, paris)); got.Format("01-02 15:04 MST") != "03-29 03:00 CEST" {
		t.Errorf("next after midnight: %v", got)
	}
}

func TestATimeTheClocksRepeatFiresOnceTheFirstTime(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	// 2026-10-25: 03:00 CEST is 02:00 CET, so 02:00 to 02:59 come twice.
	from, to := time.Date(2026, 10, 24, 22, 0, 0, 0, time.UTC), time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC)
	for spec, want := range map[string]string{
		"30 2 * * *": "[10-25 02:30 CEST]",
		"0 * * * *":  "[10-25 00:00 CEST 10-25 01:00 CEST 10-25 02:00 CEST 10-25 03:00 CET 10-25 04:00 CET]",
	} {
		s, err := Parse(spec, "x")
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprint(fires(s, paris, from, to)); got != want {
			t.Errorf("%s: %s, want %s", spec, got, want)
		}
	}
}

func TestAZoneWithoutAChangeOfClocksFiresAtTheWallTime(t *testing.T) {
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := Parse("30 2 * * *", "x")
	got := fires(s, tokyo, time.Date(2026, 3, 28, 0, 0, 0, 0, tokyo), time.Date(2026, 3, 30, 0, 0, 0, 0, tokyo))
	if fmt.Sprint(got) != "[03-28 02:30 JST 03-29 02:30 JST]" {
		t.Errorf("%v", got)
	}
}

func TestAScheduleFiringMoreOftenThanTheGapIsFound(t *testing.T) {
	for _, c := range []struct {
		spec string
		want bool
	}{
		{"H/2 * * * *", true},
		{"*/5 * * * *", true},
		{"*/15 * * * *", false},
		{"0 9 * * *", false},
		{"0 * * * *", false},
		{"*/5 9 * * *", true},
		{"0,10 9 * * *", true},
		{"0 0 1 1 *", false},
	} {
		s := must(Parse(c.spec, "job"))
		// Every hour of a day gives the same answer, so it does not flip
		// with the time of day.
		for h := range 24 {
			from := at(fmt.Sprintf("2026-10-10 %02d:07", h))
			if got := s.FasterThan(15*time.Minute, from, 24*time.Hour); got != c.want {
				t.Errorf("%q from %v: got %v want %v", c.spec, from, got, c.want)
			}
		}
	}
}
