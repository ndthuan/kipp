package web

import (
	"testing"
	"time"
)

func textOf(v *whenView) string {
	if v == nil {
		return ""
	}
	return v.Text
}

func TestWhenIsRelativeWithin24HoursAndAbsoluteBeyond(t *testing.T) {
	zone := time.FixedZone("ICT", 7*3600)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	when := whenIn(zone, func() time.Time { return now })
	cases := []struct {
		name string
		at   time.Time
		want string
	}{
		{"23h59m ago", now.Add(-23*time.Hour - 59*time.Minute), "23h 59m ago"},
		{"24h01m ago", now.Add(-24*time.Hour - time.Minute), "2026-11-09 18:59"},
		{"in 23h59m", now.Add(23*time.Hour + 59*time.Minute), "in 23h 59m"},
		{"in 24h01m", now.Add(24*time.Hour + time.Minute), "2026-11-11 19:01"},
		{"in 9m", now.Add(9 * time.Minute), "in 9m"},
		{"in 9m 59s", now.Add(9*time.Minute + 59*time.Second), "in 9m"},
		{"in 30s", now.Add(30 * time.Second), "in <1m"},
		{"30s ago", now.Add(-30 * time.Second), "<1m ago"},
		{"57m 52s ago", now.Add(-57*time.Minute - 52*time.Second), "57m ago"},
	}
	for _, c := range cases {
		if got := textOf(when(c.at)); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestWhenTitleHoldsSecondsAndTheServersZone(t *testing.T) {
	zone := time.FixedZone("ICT", 7*3600)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	at := time.Date(2026, 11, 10, 9, 24, 51, 0, time.UTC)
	got := whenIn(zone, func() time.Time { return now })(&at)
	want := whenView{At: "2026-11-10T09:24:51Z", Title: "2026-11-10 16:24:51 ICT", Abs: "2026-11-10 16:24", Text: ""}
	want.Text = "2h 35m ago"
	if got == nil || *got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestWhenOfNoTimeIsEmpty(t *testing.T) {
	when := whenIn(time.UTC, time.Now)
	var none *time.Time
	if got := when(none); got != nil {
		t.Errorf("nil time: %v", got)
	}
	if got := when(time.Time{}); got != nil {
		t.Errorf("zero time: %v", got)
	}
	if got := when("x"); got != nil {
		t.Errorf("not a time: %v", got)
	}
}

func TestACoarseTimeNeverCountsSeconds(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                               "<1m",
		59 * time.Second:                "<1m",
		time.Minute:                     "1m",
		57*time.Minute + 52*time.Second: "57m",
		2*time.Hour + 5*time.Minute + 9*time.Second: "2h 5m",
		24 * time.Hour:                "1d",
		10*24*time.Hour + 4*time.Hour: "10d",
	} {
		if got := coarse(d); got != want {
			t.Errorf("coarse(%v) = %q, want %q", d, got, want)
		}
	}
}

// A live elapsed time counts seconds and a past moment is coarse and still,
// the two told apart by the class queue.js ticks them by.
func TestAMomentIsElapsedWhenLiveAndCoarseWhenPast(t *testing.T) {
	zone := time.FixedZone("ICT", 7*3600)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	at := now.Add(-3*time.Hour - 5*time.Minute - 9*time.Second)
	for _, c := range []struct {
		name string
		got  *moment
		want moment
	}{
		{"elapsed", elapsedIn(zone, func() time.Time { return now })(at, "running"), moment{Kind: "elapsed", Word: "running", Text: "3h 5m"}},
		{"since", sinceIn(zone, func() time.Time { return now })(&at, "red"), moment{Kind: "since", Word: "red", Text: "3h 5m"}},
		{"past", pastIn(zone, func() time.Time { return now })(at, "seen"), moment{Kind: "past", Word: "seen", Text: "3h 5m ago"}},
	} {
		if c.got == nil {
			t.Fatalf("%s: no moment", c.name)
		}
		c.want.At, c.want.Title = "2026-11-10T08:54:51Z", "2026-11-10 15:54:51 ICT"
		if *c.got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, *c.got, c.want)
		}
	}
	short := elapsedIn(zone, func() time.Time { return now })(now.Add(-9*time.Minute-42*time.Second), "queued")
	if short.Text != "9m 42s" {
		t.Errorf("an elapsed time keeps its seconds: %q", short.Text)
	}
	var none *time.Time
	if sinceIn(zone, time.Now)(none, "red") != nil || sinceIn(zone, time.Now)("x", "red") != nil {
		t.Error("no time and not a time have no moment")
	}
}
