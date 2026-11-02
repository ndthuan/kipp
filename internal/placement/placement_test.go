package placement

import (
	"testing"
	"time"
)

func TestPickFollowsTheRulesOfPlacement(t *testing.T) {
	t0 := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	agent := func(name string, free, online bool, idle time.Time, labels ...string) Candidate {
		return Candidate{Name: name, Kind: KindAgent, Labels: labels, Free: free, Reachable: online, Idle: idle}
	}
	engine := func(name string, room int, reachable bool, labels ...string) Candidate {
		return Candidate{Name: name, Kind: KindEngine, Labels: labels, Free: room > 0, Room: room, Reachable: reachable, Why: "unreachable since 14:02: dial tcp: refused"}
	}
	linux := Want{Labels: []string{"linux"}}
	image := Want{Labels: []string{"linux"}, Image: "golang:1.27"}
	cases := []struct {
		name string
		want Want
		cs   []Candidate
		pick string
		why  Why
		line string
	}{
		{"nothing at all", linux, nil, "", WhyNone, "no agent or engine carries labels [linux]"},
		{"labels not all carried", Want{Labels: []string{"linux", "arm64"}}, []Candidate{agent("a", true, true, t0, "linux")}, "", WhyNone, "no agent or engine carries labels [linux, arm64]"},
		{"an image and only agents", image, []Candidate{agent("a", true, true, t0, "linux")}, "", WhyNone, "no engine carries labels [linux]: a stage with an image runs in a container on an engine"},
		{"no image and only engines", linux, []Candidate{engine("e", 2, true, "linux")}, "", WhyNone, "no agent carries labels [linux]: a stage with no image runs on a standing agent"},
		{"the agent is busy", linux, []Candidate{agent("a", false, true, t0, "linux")}, "", WhyBusy, "waiting for an agent with labels [linux]"},
		{"the agent is offline", linux, []Candidate{agent("a", true, false, t0, "linux")}, "", WhyBusy, "waiting for an agent with labels [linux]"},
		{"the engine is unreachable", image, []Candidate{engine("mac", 2, false, "linux")}, "", WhyUnreachable, "waiting for engine mac (unreachable since 14:02: dial tcp: refused)"},
		{"the engine is full", image, []Candidate{engine("mac", 0, true, "linux")}, "", WhyBusy, "waiting for an engine with labels [linux]"},
		{"the agent idle longest", linux, []Candidate{agent("new", true, true, t0.Add(time.Minute), "linux"), agent("old", true, true, t0, "linux", "x")}, "old", Picked, ""},
		{"the engine with the most room", image, []Candidate{engine("small", 1, true, "linux"), engine("big", 3, true, "linux")}, "big", Picked, ""},
		{"the engine already running the build's container", image, []Candidate{engine("big", 3, true, "linux"), {Name: "mine", Kind: KindEngine, Labels: []string{"linux"}, Reachable: true, Reuse: true}}, "mine", Picked, ""},
		{"a free agent beside a busy one", linux, []Candidate{agent("busy", false, true, t0, "linux"), agent("free", true, true, t0.Add(time.Hour), "linux")}, "free", Picked, ""},
	}
	for _, c := range cases {
		got, why, line := Pick(c.want, c.cs)
		if got.Name != c.pick || why != c.why || line != c.line {
			t.Errorf("%s: picked %q, %q, %q; want %q, %q, %q", c.name, got.Name, why, line, c.pick, c.why, c.line)
		}
	}
}
