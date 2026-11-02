package cli

import (
	"testing"
	"time"
)

const slowJob = "name: slow\nstages:\n  s: {run: 'sleep 4'}\n"

func TestQuietOnStatusAndOffAreTheServersAndARefusedBuildSaysWhy(t *testing.T) {
	s := start(t, map[string]string{"plain.yml": "name: plain\nstages:\n  s: {run: 'true'}\n"})
	code, out, _ := s.kipp("quiet", "status")
	eq(t, "status", code, 0)
	eq(t, "what it says", out, "quiet off: 0 running, 0 queued\n")

	code, out, _ = s.kipp("quiet", "on")
	eq(t, "on", code, 0)
	eq(t, "what it says", out, "quiet on: 0 running, 0 queued\n")

	code, _, errOut := s.kipp("run", "plain")
	eq(t, "a build while quiet", code, 1)
	contains(t, errOut, "kipp is quiet: restarting once running builds end")

	code, out, _ = s.kipp("quiet", "off")
	eq(t, "off", code, 0)
	eq(t, "what it says", out, "quiet off: 0 running, 0 queued\n")
	eq(t, "a build after quiet", func() int { c, _, _ := s.kipp("run", "plain", "--wait"); return c }(), 0)

	code, _, errOut = kipp("", "quiet", "on", "--url", s.url, "--token", "kipp_nobody")
	eq(t, "a token nobody holds", code, 3)
	contains(t, errOut, "refused")
	code, _, _ = s.kipp("quiet", "sideways")
	eq(t, "a word it does not know", code, 2)
	code, _, _ = s.kipp("quiet", "off", "--wait")
	eq(t, "--wait on off", code, 2)
}

func TestQuietOnWaitReturnsWhenIdleAndTurnsQuietOffAtTheTimeout(t *testing.T) {
	old := quietPoll
	quietPoll = 50 * time.Millisecond
	t.Cleanup(func() { quietPoll = old })
	s := start(t, map[string]string{"slow.yml": slowJob})

	code, out, _ := s.kipp("quiet", "on", "--wait", "--timeout", "5s")
	eq(t, "idle server", code, 0)
	eq(t, "what it says", out, "quiet on: 0 running, 0 queued\n")
	ok(t, "", "quiet", "off", "--url", s.url, "--token", s.token)

	eq(t, "start", func() int { c, _, _ := s.kipp("run", "slow"); return c }(), 0)
	code, out, errOut := s.kipp("quiet", "on", "--wait", "--timeout", "300ms")
	eq(t, "a build outlasts the timeout", code, 1)
	contains(t, out, "quiet on: 1 running, 0 queued")
	contains(t, errOut, "still busy after 300ms", "quiet is off again")
	_, out, _ = s.kipp("quiet", "status")
	eq(t, "quiet is off again", out, "quiet off: 1 running, 0 queued\n")

	code, out, _ = s.kipp("quiet", "on", "--wait", "--timeout", "30s")
	eq(t, "the build ends", code, 0)
	eq(t, "what it ends with", out[len(out)-len("quiet on: 0 running, 0 queued\n"):], "quiet on: 0 running, 0 queued\n")
}
