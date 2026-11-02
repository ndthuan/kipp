package cli

import (
	"fmt"
	"io"
	"net/http"
	"time"
)

const quietUsage = `quiet on [--wait [--timeout <duration>]] | off | status [--url <url>] [--token <token>]`

// quietPoll is how often quiet on --wait looks at what still runs.
var quietPoll = 2 * time.Second

// quietState is the server's answer to /api/v1/admin/quiet.
type quietState struct {
	Quiet   bool   `json:"quiet"`
	Since   string `json:"since"`
	Running int    `json:"running"`
	Queued  int    `json:"queued"`
	Idle    bool   `json:"idle"`
}

func (q quietState) text() string {
	state := "off"
	if q.Quiet {
		state = "on"
	}
	return fmt.Sprintf("quiet %s: %d running, %d queued", state, q.Running, q.Queued)
}

// cmdQuiet makes a server quiet before a restart, as Jenkins does: it
// starts no build, the running and queued ones end, and a trigger is
// refused. `on --wait` returns once nothing is left; past --timeout it
// turns quiet off again and fails, so a restart that waited too long never
// stays refused.
func cmdQuiet(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	fs := newFlags(quietUsage, stderr)
	wait := fs.Bool("wait", false, "on: return once no build runs or waits")
	timeout := fs.Duration("timeout", 2*time.Hour, "on --wait: how long to wait before quiet is turned off again and the command fails")
	newClient := clientFlags(fs)
	positional, err := parseArgs(fs, args)
	if err != nil {
		return exitFor(err)
	}
	if len(positional) != 1 || (positional[0] != "on" && positional[0] != "off" && positional[0] != "status") || (*wait && positional[0] != "on") {
		fs.Usage()
		return 2
	}
	c, err := newClient()
	if err != nil {
		return fail(stderr, "quiet", err)
	}
	var q quietState
	switch positional[0] {
	case "status":
		err = c.call(http.MethodGet, "/admin/quiet", nil, &q)
	case "off":
		err = c.call(http.MethodDelete, "/admin/quiet", nil, &q)
	default:
		if err = c.call(http.MethodPost, "/admin/quiet", nil, &q); err == nil && *wait {
			q, err = waitIdle(c, q, *timeout, stdout)
		}
	}
	if err != nil {
		return fail(stderr, "quiet", err)
	}
	say(stdout, "%s", q.text())
	return 0
}

// waitIdle polls until nothing runs or waits. At the timeout it turns
// quiet off and says so.
func waitIdle(c *client, q quietState, timeout time.Duration, stdout io.Writer) (quietState, error) {
	deadline := time.Now().Add(timeout)
	last := ""
	for !q.Idle {
		if text := q.text(); text != last {
			say(stdout, "%s", text)
			last = text
		}
		if !time.Now().Before(deadline) {
			if err := c.call(http.MethodDelete, "/admin/quiet", nil, &q); err != nil {
				return q, fmt.Errorf("still busy after %s, and quiet could not be turned off: %w", timeout, err)
			}
			return q, fmt.Errorf("still busy after %s (%d running, %d queued): quiet is off again", timeout, q.Running, q.Queued)
		}
		time.Sleep(quietPoll)
		if err := c.call(http.MethodGet, "/admin/quiet", nil, &q); err != nil {
			return q, err
		}
	}
	return q, nil
}
