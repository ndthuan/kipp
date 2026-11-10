package cli

import (
	"strings"
	"testing"
	"time"
)

func TestDBCompactCompactsARunningServersDatabaseAndLeavesItTakingBuilds(t *testing.T) {
	old := compactPoll
	compactPoll = 20 * time.Millisecond
	t.Cleanup(func() { compactPoll = old })
	s := start(t, map[string]string{"plain.yml": "name: plain\nstages:\n  s: {run: 'true'}\n"})
	ok(t, "", "run", "plain", "--wait", "--url", s.url, "--token", s.token)

	code, out, errOut := s.kipp("db", "compact")
	eq(t, "compact "+errOut, code, 0)
	contains(t, out, "compacted: ", "incremental auto_vacuum is on")
	_, out, _ = s.kipp("quiet", "status")
	eq(t, "quiet after", out, "quiet off: 0 running, 0 queued\n")
	eq(t, "a build after", func() int { c, _, _ := s.kipp("run", "plain", "--wait"); return c }(), 0)

	code, out, _ = s.kipp("db", "compact")
	eq(t, "again", code, 0)
	if strings.Contains(out, "incremental auto_vacuum is on") {
		t.Fatalf("the second compact says it turned auto_vacuum on: %q", out)
	}
	code, _, _ = s.kipp("db", "shrink")
	eq(t, "a word it does not know", code, 2)
	code, _, errOut = kipp("", "db", "compact", "--url", s.url, "--token", "kipp_nobody")
	eq(t, "a token nobody holds", code, 3)
	contains(t, errOut, "refused")
}
