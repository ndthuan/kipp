package cli

import (
	"os/exec"
	"strings"
	"testing"
)

// probes is a stage that calls the server back with its own KIPP_TOKEN and
// prints the status of each answer.
const probes = `
code() { curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $KIPP_TOKEN" "$@"; }
echo "own=$(code "$KIPP_URL/api/v1/jobs/e2e")"
echo "other=$(code "$KIPP_URL/api/v1/jobs/other")"
echo "start-self=$(code -X POST "$KIPP_URL/api/v1/jobs/e2e/builds")"
echo "start-other=$(code -X POST "$KIPP_URL/api/v1/jobs/other/builds")"
echo "users=$(code "$KIPP_URL/api/v1/admin/users")"
echo "secrets=$(code "$KIPP_URL/api/v1/admin/secrets")"
echo "server=$(code "$KIPP_URL/api/v1/admin/server")"
echo "cache=$(code "$KIPP_URL/api/v1/cache/e2e/lookup?key=k")"
echo "runner=$(code -X POST -d '{}' "$KIPP_URL/api/v1/runner/connect")"
echo "mcp=$(code -X POST -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' "$KIPP_URL/mcp")"
`

func TestAStageReachesTheServerThroughItsKippTokenWithinItsGrantAndNoFurther(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl on this machine")
	}
	e2e := "name: e2e\npermissions:\n  other: [view, build]\nstages:\n  s:\n    run: |" + strings.ReplaceAll("\n"+probes, "\n", "\n      ") + "\n"
	s := &served{t: t, stderr: &syncBuffer{}, args: []string{"--mcp"}}
	s.data, s.jobs, s.token = dataAndJobs(t, map[string]string{
		"e2e.yml":   e2e,
		"other.yml": "name: other\nstages:\n  s: {run: 'true'}\n",
	})
	s.serve()
	code, out, errOut := s.kipp("run", "e2e", "--wait")
	eq(t, "exit: "+errOut, code, 0)
	for _, want := range []string{
		"own=200", "other=200", "start-self=403", "start-other=201",
		"users=403", "secrets=403", "server=403", "cache=401", "mcp=200",
	} {
		contains(t, out, "] "+want+"\n")
	}
	if strings.Contains(out, "runner=2") {
		t.Fatalf("the runner protocol took a build's token:\n%s", out)
	}
	if strings.Contains(out, "kippb_") {
		t.Fatalf("the token reached the log:\n%s", out)
	}
}
