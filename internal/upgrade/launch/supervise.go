package launch

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ndthuan/kipp/internal/update"
	"github.com/ndthuan/kipp/internal/upgrade/backup"
	"github.com/ndthuan/kipp/internal/upgrade/state"
)

const (
	// EnvSupervised is set to 1 on the server the launcher runs as a child:
	// such a process serves, does not supervise and does not look at the
	// pending record.
	EnvSupervised = "KIPP_SUPERVISED"
	// EnvTiming overrides the deadlines, for tests: a comma list of
	// window=, cap=, grace=, poll= and settle= with Go durations.
	EnvTiming = "KIPP_UPGRADE_TIMING"
	// ExitRestart is the code a supervised server exits with after it has
	// written a pending record, so that the launcher handles it.
	ExitRestart = 75

	defaultListen = "127.0.0.1:8085"
	logTailBytes  = 4096
)

// timing is how long the launcher waits for a pending version.
type timing struct {
	// window counts from the end of the store's migration (from the start
	// when there is none), cap from the start, whatever the migration does.
	window, cap time.Duration
	// grace is how long a child stopped by SIGTERM has before SIGKILL.
	grace time.Duration
	poll  time.Duration
	// settle is how long a child must run, migration over, when its health
	// address is not known (--listen with port 0).
	settle time.Duration
}

func defaultTiming() timing {
	return timing{window: 90 * time.Second, cap: 30 * time.Minute, grace: 30 * time.Second,
		poll: time.Second, settle: 10 * time.Second}
}

func timingFromEnv(log *slog.Logger) timing {
	t := defaultTiming()
	raw := os.Getenv(EnvTiming)
	if raw == "" {
		return t
	}
	for kv := range strings.SplitSeq(raw, ",") {
		k, v, _ := strings.Cut(kv, "=")
		d, err := time.ParseDuration(v)
		dst := map[string]*time.Duration{"window": &t.window, "cap": &t.cap, "grace": &t.grace,
			"poll": &t.poll, "settle": &t.settle}[k]
		if err != nil || dst == nil || d <= 0 {
			log.Warn("ignoring a part of "+EnvTiming, "part", kv)
			continue
		}
		*dst = d
	}
	return t
}

// launcher carries out a pending record. The clock, the pause and the
// probe are fields so that a test can drive the deadlines.
type launcher struct {
	log           *slog.Logger
	data, version string
	args          []string
	t             timing
	stdout        io.Writer
	stderr        io.Writer

	now func() time.Time
	// pause waits d for the child c to change, and returns early when it
	// ends.
	pause func(c *child, d time.Duration)
	// probe asks the child at addr whether it is healthy.
	probe func(addr string) bool
	// exit ends the process with a child's code; exec replaces it.
	exit func(int)
	exec func(path string, argv, env []string) error
}

func newLauncher(log *slog.Logger, data, version string, args []string) *launcher {
	return &launcher{log: log, data: data, version: version, args: args, t: timingFromEnv(log),
		stdout: os.Stdout, stderr: os.Stderr, now: time.Now, pause: pauseFor, probe: probeHealth,
		exit: os.Exit, exec: syscall.Exec}
}

func pauseFor(c *child, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-c.done:
	case <-t.C:
	}
}

// launched reports whether this process was itself started from the data
// directory.
func launched() bool { return os.Getenv(EnvLaunched) == "1" }

// bundledVersion is the version of the executable that was started first.
func (l *launcher) bundledVersion() string {
	if launched() {
		return os.Getenv(EnvBundledVersion)
	}
	return l.version
}

// bundledExe is that executable.
func (l *launcher) bundledExe() (string, error) {
	if launched() && os.Getenv(EnvBundledExe) != "" {
		return os.Getenv(EnvBundledExe), nil
	}
	return os.Executable()
}

// previousFor is what the version being replaced is called in previous and
// in a rollback: Bundled when it is the original executable.
func (l *launcher) previousFor(from string) string {
	if from == "" || from == state.Bundled || from == l.bundledVersion() {
		return state.Bundled
	}
	return from
}

// processPending carries out the pending records until none is left. It
// returns when there is nothing (more) to do and the caller is to go on as
// it would have; it does not return when it handed over to another process
// or ended this one with a child's exit code.
func (l *launcher) processPending() {
	for {
		p, err := state.ReadPending(l.data)
		if err != nil {
			l.log.Warn("not reading the pending upgrade", "err", err)
			return
		}
		if p == nil {
			return
		}
		if p.Kind == state.KindRollback {
			l.rollback(p)
			return
		}
		res := l.upgrade(p)
		switch {
		case res.restart:
			continue
		case res.ended:
			l.exit(res.code)
			return
		default:
			l.execPrevious(res.target)
			return
		}
	}
}

// result is how the attempt at a pending version ended.
type result struct {
	// ended: the version ran and its process is over, with code.
	ended bool
	code  int
	// restart: it exited with ExitRestart, so a record is to be read again.
	restart bool
	// otherwise it was rolled back, to target (a version or Bundled).
	target string
}

// child is the pending version's process.
type child struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
	tail *tailBuffer
	// stopping is set once a signal of ours was handed to the child.
	mu       sync.Mutex
	stopping bool
}

func (c *child) signaled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopping
}

// upgrade runs the version p names as a child and settles it: healthy,
// it becomes current and the child's life is the launcher's; unhealthy,
// it is stopped and everything goes back to the old version.
func (l *launcher) upgrade(p *state.Pending) result {
	path := state.BinaryPath(l.data, p.Version)
	if err := usable(path, p.Version); err != nil {
		l.log.Error("the pending version cannot run", "version", p.Version, "err", err)
		return l.rollBackUnhealthy(p, nil, "the binary cannot run: "+err.Error())
	}
	// A file left by a process that died mid-migration would lengthen the wait.
	_ = os.Remove(state.MigratingPath(l.data))
	c, err := l.start(path)
	if err != nil {
		l.log.Error("not starting the pending version", "version", p.Version, "err", err)
		return l.rollBackUnhealthy(p, nil, "it did not start: "+err.Error())
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigs)
	go forward(sigs, c)

	l.log.Info("trying the pending version", "version", p.Version, "from", p.From)
	healthy, reason := l.waitHealthy(c)
	switch {
	case healthy:
		l.becomeCurrent(p)
		<-c.done
		code := exitCode(c.err)
		return result{ended: code != ExitRestart, restart: code == ExitRestart, code: code}
	case c.signaled():
		// An operator stopped the launcher while it waited. That says
		// nothing about the version: pending stays for the next start.
		<-c.done
		return result{ended: true, code: exitCode(c.err)}
	default:
		return l.rollBackUnhealthy(p, c, reason)
	}
}

// forward hands the signals the launcher gets to the child.
func forward(sigs <-chan os.Signal, c *child) {
	for {
		select {
		case s := <-sigs:
			c.mu.Lock()
			c.stopping = true
			c.mu.Unlock()
			_ = c.cmd.Process.Signal(s)
		case <-c.done:
			return
		}
	}
}

func (l *launcher) start(path string) (*child, error) {
	exe, err := l.bundledExe()
	if err != nil {
		return nil, err
	}
	c := &child{done: make(chan struct{}), tail: &tailBuffer{max: logTailBytes}}
	c.cmd = exec.Command(path, append([]string{"serve"}, l.args...)...)
	c.cmd.Env = append(withoutKeys(os.Environ(), EnvBundledExe, EnvBundledVersion, EnvLaunched, EnvSupervised),
		EnvBundledExe+"="+exe, EnvBundledVersion+"="+l.bundledVersion(), EnvLaunched+"=1", EnvSupervised+"=1")
	c.cmd.Stdout = io.MultiWriter(l.stdout, c.tail)
	c.cmd.Stderr = io.MultiWriter(l.stderr, c.tail)
	// A grandchild holding the pipes open must not keep Wait from returning.
	c.cmd.WaitDelay = l.t.grace
	if err := c.cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		c.err = c.cmd.Wait()
		close(c.done)
	}()
	return c, nil
}

// waitHealthy returns true once the child answers its health check, and
// otherwise why it will not: it exited, or a deadline passed.
func (l *launcher) waitHealthy(c *child) (bool, string) {
	start := l.now()
	base := start
	addr, known := healthAddr(l.args)
	for {
		select {
		case <-c.done:
			return false, "it " + describeExit(c.err) + " before it was healthy"
		default:
		}
		migrating := fileExists(state.MigratingPath(l.data))
		now := l.now()
		switch {
		case migrating:
			base = now
		case known && l.probe(addr):
			return true, ""
		case !known && now.Sub(base) >= l.t.settle:
			l.log.Warn("the health address is not known; judging by the process staying up")
			return true, ""
		}
		switch {
		case now.Sub(start) >= l.t.cap:
			return false, fmt.Sprintf("it was not healthy %s after it started", l.t.cap)
		case !migrating && now.Sub(base) >= l.t.window:
			return false, fmt.Sprintf("it did not answer its health check within %s", l.t.window)
		}
		l.pause(c, l.t.poll)
	}
}

// becomeCurrent records that the pending version came up. Each step is safe
// to repeat: a launcher that dies in the middle meets pending again.
func (l *launcher) becomeCurrent(p *state.Pending) {
	prev := l.previousFor(p.From)
	steps := []struct {
		what string
		do   func() error
	}{
		{"clearing the rejection", func() error { return state.Unreject(l.data, p.Version) }},
		{"marking the backup", func() error {
			if p.Backup == "" {
				return nil
			}
			return backup.MarkSucceeded(l.data, p.Backup, l.now())
		}},
		{"writing current", func() error { return state.WriteCurrent(l.data, p.Version) }},
		{"writing previous", func() error { return state.WritePrevious(l.data, prev) }},
		{"removing pending", func() error { return state.ClearPending(l.data) }},
	}
	for _, s := range steps {
		if err := s.do(); err != nil {
			l.log.Error("the new version is healthy but "+s.what+" failed; the next start tries it again",
				"version", p.Version, "err", err)
			return
		}
	}
	l.log.Info("the new version is healthy", "version", p.Version, "previous", prev)
}

// rollBackUnhealthy stops the child (if any), puts the backup back, records
// the failure and the rejection, and says which version to start.
func (l *launcher) rollBackUnhealthy(p *state.Pending, c *child, reason string) result {
	tail := ""
	if c != nil {
		l.stop(c)
		tail = c.tail.String()
	}
	l.log.Error("the new version is not healthy; going back", "version", p.Version, "reason", reason)
	if p.Backup != "" {
		if err := backup.Restore(l.data, p.Backup); err != nil {
			l.log.Error("the backup was not restored", "backup", p.Backup, "err", err)
			reason += "; the backup was not restored: " + err.Error()
		}
	}
	target := l.previousFor(p.From)
	l.afterRollback(p.Version, target)
	at := l.now().UTC()
	l.logErr("recording the failure", state.WriteFailure(l.data, state.Failure{
		Version: p.Version, From: p.From, Reason: reason, LogTail: tail, At: at}))
	l.logErr("writing the status", state.WriteStatus(l.data, state.Status{
		State: state.StateFailed, Version: p.Version, From: p.From, Backup: p.Backup, UpdatedAt: at}))
	return result{target: target}
}

// afterRollback is the state both kinds of return leave: the version left is
// rejected, current names where we went (none for the bundled executable),
// and pending is gone. The record goes last, so that a launcher that dies
// before it meets the record again.
func (l *launcher) afterRollback(left, target string) {
	if update.Release(left) {
		l.logErr("rejecting "+left, state.Reject(l.data, left))
	}
	if target == state.Bundled {
		l.logErr("clearing current", state.ClearCurrent(l.data))
	} else {
		l.logErr("clearing the rejection of the target", state.Unreject(l.data, target))
		l.logErr("writing current", state.WriteCurrent(l.data, target))
	}
	l.logErr("removing pending", state.ClearPending(l.data))
}

func (l *launcher) logErr(what string, err error) {
	if err != nil {
		l.log.Error("rollback: "+what, "err", err)
	}
}

// stop asks the child to end, and kills it when it has not within grace.
func (l *launcher) stop(c *child) {
	select {
	case <-c.done:
		return
	default:
	}
	_ = c.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-c.done:
	case <-time.After(l.t.grace):
		_ = c.cmd.Process.Kill()
		<-c.done
	}
}

// rollback carries out a rollback record: the backup goes back, the
// version left is rejected, and the target starts. A record whose backup is
// not there is set aside as pending.refused, and the caller goes on as
// though it had not been.
func (l *launcher) rollback(p *state.Pending) {
	if err := l.restoreFor(p); err != nil {
		l.log.Error("refusing the rollback record; it is kept as pending.refused", "err", err)
		if err := state.RefusePending(l.data); err != nil {
			l.log.Error("not setting the record aside", "err", err)
		}
		return
	}
	l.afterRollback(p.From, p.Version)
	l.execPrevious(p.Version)
}

func (l *launcher) restoreFor(p *state.Pending) error {
	if p.Backup == "" {
		return errors.New("the record names no backup")
	}
	if _, err := os.Stat(filepath.Join(backup.Dir(l.data), p.Backup, backup.DBFile)); err != nil {
		return fmt.Errorf("the backup is missing: %w", err)
	}
	return backup.Restore(l.data, p.Backup)
}

// execPrevious becomes the target: a binary of the data directory, or the
// original executable. It returns only when that failed, and the caller
// then carries on as the executable it is.
func (l *launcher) execPrevious(target string) {
	if target == state.Bundled {
		exe, err := l.bundledExe()
		if err != nil {
			l.log.Error("not going back: the original executable is unknown", "err", err)
			return
		}
		env := withoutKeys(os.Environ(), EnvBundledExe, EnvBundledVersion, EnvLaunched, EnvSupervised)
		l.execve(exe, exe, env)
		return
	}
	path := state.BinaryPath(l.data, target)
	exe, err := l.bundledExe()
	if err != nil {
		l.log.Error("not going back: the original executable is unknown", "err", err)
		return
	}
	env := append(withoutKeys(os.Environ(), EnvBundledExe, EnvBundledVersion, EnvLaunched, EnvSupervised),
		EnvBundledExe+"="+exe, EnvBundledVersion+"="+l.bundledVersion(), EnvLaunched+"=1")
	l.execve(path, target, env)
}

func (l *launcher) execve(path, name string, env []string) {
	argv := append([]string{path, "serve"}, l.args...)
	err := l.exec(path, argv, env)
	l.log.Error("not starting the earlier version; running this executable's own", "version", name, "err", err)
}

// healthAddr is where the server started with args answers its health
// check, and false when the arguments do not say (port 0).
func healthAddr(args []string) (string, bool) {
	listen := defaultListen
	for i, arg := range args {
		if arg == "--" {
			break
		}
		name := strings.TrimLeft(arg, "-")
		if dashes := len(arg) - len(name); dashes == 0 || dashes > 2 {
			continue
		}
		if v, ok := strings.CutPrefix(name, "listen="); ok {
			listen = v
		} else if name == "listen" && i+1 < len(args) {
			listen = args[i+1]
		}
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", false
	}
	if n, err := strconv.Atoi(port); err != nil || n == 0 {
		return "", false
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	return net.JoinHostPort(host, port), true
}

// probeHealth is true when addr answers {"ok":true} to the health check.
func probeHealth(addr string) bool {
	cl := http.Client{Timeout: 2 * time.Second}
	resp, err := cl.Get("http://" + addr + "/api/v1/health")
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		OK bool `json:"ok"`
	}
	return resp.StatusCode == http.StatusOK &&
		json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&body) == nil && body.OK
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// exitCode is the code the launcher ends with for a child's end: its own,
// or 128 plus the signal that killed it.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	return 1
}

func describeExit(err error) string {
	code := exitCode(err)
	if code > 128 {
		return fmt.Sprintf("was killed by signal %d", code-128)
	}
	return fmt.Sprintf("exited with code %d", code)
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}
