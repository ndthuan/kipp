package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const upgradeUsage = `upgrade [status] [--check] [--dry-run [<version>] [--json]] [--cancel] [--rollback] [--no-safe [--yes]] [--wait-timeout <duration>] [--url <url>] [--token <token>]`

// upgradePoll is how often the command looks at the upgrade, and
// upgradeAway how long it waits for a server that has stopped answering: the
// launcher's 30 minutes for the new version to come up, and five more.
var (
	upgradePoll = 2 * time.Second
	upgradeAway = 35 * time.Minute
)

// dryRunWait is how long the command waits for the answer of a dry run:
// the server downloads the release and migrates a copy of the database
// before it answers.
const dryRunWait = time.Hour

// upgradeState is the server's answer to /api/v1/admin/upgrade.
type upgradeState struct {
	State       string `json:"state"`
	Version     string `json:"version"`
	From        string `json:"from"`
	Backup      string `json:"backup"`
	Running     int    `json:"running"`
	Queued      int    `json:"queued"`
	Error       string `json:"error"`
	Latest      string `json:"latest"`
	Upgradable  bool   `json:"upgradable"`
	Unavailable string `json:"unavailable"`
	RollbackTo  string `json:"rollback_to"`
	LastFailure *struct {
		Version string `json:"version"`
		Reason  string `json:"reason"`
	} `json:"last_failure"`
}

// upgradeBody is what starts an upgrade or a rollback.
type upgradeBody struct {
	SafeRestart bool   `json:"safe_restart"`
	ConfirmLost bool   `json:"confirm_lost,omitempty"`
	WaitTimeout string `json:"wait_timeout,omitempty"`
}

// cmdUpgrade upgrades a running server to the latest release, or returns it
// to the version before, and follows the steps until the new version has
// come up. By default it waits for the builds to end first, in quiet mode,
// as `kipp quiet on --wait` does.
func cmdUpgrade(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	fs := newFlags(upgradeUsage, stderr)
	o := upgradeFlags{
		check:    fs.Bool("check", false, "say whether a newer release can be upgraded to, and do nothing"),
		dryRun:   fs.Bool("dry-run", false, "make every check of the upgrade but the restart, and change nothing; exits non-zero when one fails"),
		json:     fs.Bool("json", false, "with --dry-run, print the report as JSON"),
		cancel:   fs.Bool("cancel", false, "cancel the upgrade that waits, before its backup begins"),
		rollback: fs.Bool("rollback", false, "return to the version before the last upgrade, with the data as it was then: anything written since is lost"),
		noSafe:   fs.Bool("no-safe", false, "do not wait for the builds to end: they end as lost"),
		yes:      fs.Bool("yes", false, "with --no-safe, confirm that the builds that run end as lost"),
		timeout:  fs.Duration("wait-timeout", 2*time.Hour, "how long to wait for the builds before giving up and leaving the server as it was"),
	}
	newClient := clientFlags(fs)
	positional, err := parseArgs(fs, args)
	if err != nil {
		return exitFor(err)
	}
	o.status = len(positional) == 1 && positional[0] == "status"
	if *o.dryRun && len(positional) == 1 {
		o.version = positional[0]
	}
	if !o.valid(positional) {
		fs.Usage()
		return 2
	}
	c, err := newClient()
	if err != nil {
		return fail(stderr, "upgrade", err)
	}
	return o.run(c, stdout, stderr)
}

// upgradeFlags are what `kipp upgrade` was asked.
type upgradeFlags struct {
	check, dryRun, json, cancel, rollback, noSafe, yes *bool
	status                                             bool
	version                                            string
	timeout                                            *time.Duration
}

// valid: at most one of the things to do, --yes only with --no-safe, and
// --json and a version only with --dry-run.
func (o upgradeFlags) valid(positional []string) bool {
	modes := 0
	for _, on := range []bool{*o.check, *o.dryRun, *o.cancel, *o.rollback, o.status} {
		if on {
			modes++
		}
	}
	words := 0
	if o.status || o.version != "" {
		words = 1
	}
	return len(positional) == words && modes <= 1 && (!*o.yes || *o.noSafe) && *o.timeout > 0 && (!*o.json || *o.dryRun)
}

func (o upgradeFlags) run(c *client, stdout, stderr io.Writer) int {
	var st upgradeState
	var err error
	show := func(text func() string) {
		if err == nil {
			say(stdout, "%s", text())
		}
	}
	switch {
	case *o.dryRun:
		return dryRun(c, o.version, *o.json, stdout, stderr)
	case *o.check:
		err = c.call(http.MethodGet, "/admin/upgrade", nil, &st)
		show(st.checkText)
	case o.status:
		err = c.call(http.MethodGet, "/admin/upgrade", nil, &st)
		show(st.statusText)
	case *o.cancel:
		err = c.call(http.MethodDelete, "/admin/upgrade", nil, &st)
		show(st.statusText)
	default:
		path := "/admin/upgrade"
		if *o.rollback {
			path += "/rollback"
		}
		body := upgradeBody{SafeRestart: !*o.noSafe, ConfirmLost: *o.yes, WaitTimeout: o.timeout.String()}
		if err = c.call(http.MethodPost, path, body, &st); err == nil {
			return followUpgrade(c, st, stdout, stderr)
		}
	}
	if err != nil {
		return fail(stderr, "upgrade", err)
	}
	return 0
}

func (u upgradeState) checkText() string {
	switch {
	case u.Unavailable != "" && u.Latest == "":
		return "no upgrade: " + u.Unavailable
	case u.Unavailable != "":
		return fmt.Sprintf("%s is available, but no upgrade: %s", u.Latest, u.Unavailable)
	}
	return fmt.Sprintf("%s is available and can be upgraded to: kipp upgrade", u.Latest)
}

func (u upgradeState) statusText() string {
	text := "upgrade: " + u.State
	if u.Version != "" {
		text += fmt.Sprintf(" (%s to %s)", orUnknown(u.From), u.Version)
	}
	if u.State == "waiting" {
		text += fmt.Sprintf(": %d running, %d queued", u.Running, u.Queued)
	}
	if reason := u.failure(); reason != "" && (u.State == "failed" || u.Error != "") {
		text += ": " + reason
	}
	return text
}

func orUnknown(v string) string {
	if v == "" {
		return "?"
	}
	return v
}

// failure is why the upgrade failed, as far as the server knows.
func (u upgradeState) failure() string {
	if u.Error != "" {
		return u.Error
	}
	if u.LastFailure != nil {
		return fmt.Sprintf("%s went back to the version before: %s", u.LastFailure.Version, u.LastFailure.Reason)
	}
	return ""
}

// followUpgrade prints each step as the server reaches it, through the
// restart, until the upgrade is done, failed or cancelled.
func followUpgrade(c *client, st upgradeState, stdout, stderr io.Writer) int {
	var away awayTimer
	last := ""
	for {
		if text := st.statusText(); text != last {
			say(stdout, "%s", text)
			last = text
		}
		if st.State == "switching" {
			away.begin()
		}
		if code, ended := st.ended(stdout, stderr); ended {
			return code
		}
		time.Sleep(upgradePoll)
		next := upgradeState{}
		err := c.call(http.MethodGet, "/admin/upgrade", nil, &next)
		switch {
		case errors.Is(err, errNoAnswer):
			away.begin()
		case err != nil:
			return fail(stderr, "upgrade", err)
		default:
			st = next
		}
		if away.over() {
			return fail(stderr, "upgrade", fmt.Errorf("the server was not done %s after it began to switch; see its log, and `kipp upgrade status` once it is back", upgradeAway))
		}
	}
}

// awayTimer counts from the first time the server is switching or not
// answering.
type awayTimer struct{ since time.Time }

func (a *awayTimer) begin() {
	if a.since.IsZero() {
		a.since = time.Now()
	}
}

func (a *awayTimer) over() bool { return !a.since.IsZero() && time.Since(a.since) > upgradeAway }

// ended is the exit code of an upgrade that is over, and false for one that
// is not.
func (u upgradeState) ended(stdout, stderr io.Writer) (int, bool) {
	switch u.State {
	case "done":
		say(stdout, "kipp %s is running", u.Version)
		return 0, true
	case "failed":
		return fail(stderr, "upgrade", errors.New(orDefault(u.failure(), "it failed; see the server's log"))), true
	case "cancelled":
		return 1, true
	}
	return 0, false
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// dryRunReport is the server's answer to /api/v1/admin/upgrade/dry-run.
type dryRunReport struct {
	Version string `json:"version"`
	From    string `json:"from"`
	OK      bool   `json:"ok"`
	Running int    `json:"running"`
	Queued  int    `json:"queued"`
	Steps   []struct {
		Name   string `json:"name"`
		Result string `json:"result"`
		Detail string `json:"detail"`
	} `json:"steps"`
}

// dryRun asks the server for every check of an upgrade but the restart and
// prints what each found; the exit code is 1 when one did not pass.
func dryRun(c *client, version string, asJSON bool, stdout, stderr io.Writer) int {
	c.http.Timeout = dryRunWait
	var body any
	if version != "" {
		body = map[string]string{"version": version}
	}
	var raw json.RawMessage
	if err := c.call(http.MethodPost, "/admin/upgrade/dry-run", body, &raw); err != nil {
		return fail(stderr, "upgrade", err)
	}
	var r dryRunReport
	if err := json.Unmarshal(raw, &r); err != nil {
		return fail(stderr, "upgrade", err)
	}
	if asJSON {
		say(stdout, "%s", raw)
	} else {
		say(stdout, "dry run of the upgrade from %s to %s, nothing was changed:", orUnknown(r.From), orUnknown(r.Version))
		for _, s := range r.Steps {
			say(stdout, "  %-7s  %-8s  %s", s.Result, s.Name, strings.ReplaceAll(s.Detail, "\n", "\n"+strings.Repeat(" ", 21)))
		}
		if r.OK {
			say(stdout, "every check passed: kipp upgrade")
		} else {
			say(stdout, "the upgrade would not go through: fix what failed and run the dry run again")
		}
	}
	if !r.OK {
		return 1
	}
	return 0
}
