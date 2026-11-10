package cli

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ndthuan/kipp/internal/config"
)

const dbUsage = `db compact [--url <url>] [--token <token>]`

// compactPoll is how often db compact asks how the compact is going, and
// compactSilence how long the server may not answer before it gives up.
var (
	compactPoll    = 2 * time.Second
	compactSilence = 30 * time.Minute
)

// compactState is the server's answer to /api/v1/admin/storage/compact.
type compactState struct {
	State       string `json:"state"`
	Running     int    `json:"running"`
	Queued      int    `json:"queued"`
	Before      int64  `json:"before_bytes"`
	After       int64  `json:"after_bytes"`
	Incremental bool   `json:"incremental"`
	Error       string `json:"error"`
	StartedAt   string `json:"started_at"`
	EndedAt     string `json:"ended_at"`
}

func (c compactState) text() string {
	switch c.State {
	case "waiting":
		return fmt.Sprintf("quiet, waiting for the builds to end: %d running, %d queued", c.Running, c.Queued)
	case "compacting":
		return "compacting the database"
	}
	return c.State
}

// cmdDB is the database of a running server: db compact asks it to compact
// its database (VACUUM) and follows the compact to its end. The server
// makes itself quiet first and waits for its builds, as a safe restart
// does, and turns quiet off again after.
func cmdDB(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	fs := newFlags(dbUsage, stderr)
	newClient := clientFlags(fs)
	positional, err := parseArgs(fs, args)
	if err != nil {
		return exitFor(err)
	}
	if len(positional) != 1 || positional[0] != "compact" {
		fs.Usage()
		return 2
	}
	c, err := newClient()
	if err != nil {
		return fail(stderr, "db compact", err)
	}
	var st compactState
	if err := c.call(http.MethodPost, "/admin/storage/compact", nil, &st); err != nil {
		return fail(stderr, "db compact", err)
	}
	if st, err = followCompact(c, st, stdout); err != nil {
		return fail(stderr, "db compact", err)
	}
	took := ""
	if start, serr := time.Parse(time.RFC3339, st.StartedAt); serr == nil {
		if end, eerr := time.Parse(time.RFC3339, st.EndedAt); eerr == nil {
			took = " in " + end.Sub(start).Round(time.Second).String()
		}
	}
	say(stdout, "compacted: %s to %s%s", config.Readable(st.Before), config.Readable(st.After), took)
	if st.Incremental {
		say(stdout, "incremental auto_vacuum is on: the retention gives the pages it frees back from now on")
	}
	return 0
}

// followCompact asks until the compact has ended, saying each change. While
// VACUUM runs the server may answer slowly or not at all; it asks again for
// up to compactSilence.
func followCompact(c *client, st compactState, stdout io.Writer) (compactState, error) {
	last := ""
	heard := time.Now()
	for st.State == "waiting" || st.State == "compacting" {
		if text := st.text(); text != last {
			say(stdout, "%s", text)
			last = text
		}
		time.Sleep(compactPoll)
		var next compactState
		err := c.call(http.MethodGet, "/admin/storage/compact", nil, &next)
		if errors.Is(err, errNoAnswer) && time.Since(heard) < compactSilence {
			continue
		}
		if err != nil {
			return st, err
		}
		st, heard = next, time.Now()
	}
	if st.State == "failed" {
		return st, errors.New(st.Error)
	}
	return st, nil
}
