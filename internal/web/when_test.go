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
		{"in 9m", now.Add(9 * time.Minute), "in 9m 0s"},
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
