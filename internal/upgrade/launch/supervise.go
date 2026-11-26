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

type timing struct {
	window, cap time.Duration
	grace       time.Duration
	poll        time.Duration
	settle      time.Duration
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

type launcher struct {
	log           *slog.Logger
	data, version string
	args          []string
	t             timing
	stdout        io.Writer
	stderr        io.Writer

	now   func() time.Time
	pause func(c *child, d time.Duration)
	probe func(addr string) bool
	exit  func(int)
	exec  func(path string, argv, env []string) error
	spawn func(cmd *exec.Cmd) error
}

func newLauncher(log *slog.Logger, data, version string, args []string) *launcher {
	return &launcher{log: log, data: data, version: version, args: args, t: timingFromEnv(log),
		stdout: os.Stdout, stderr: os.Stderr, now: time.Now, pause: pauseFor, probe: probeHealth,
		exit: os.Exit, exec: syscall.Exec, spawn: (*exec.Cmd).Start}
}

func pauseFor(c *child, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-c.done:
	case <-t.C:
	}
}

func launched() bool { return os.Getenv(EnvLaunched) == "1" }

func (l *launcher) bundledVersion() string {
	if launched() {
		return os.Getenv(EnvBundledVersion)
	}
	return l.version
}

func (l *launcher) bundledExe() (string, error) {
	if launched() && os.Getenv(EnvBundledExe) != "" {
		return os.Getenv(EnvBundledExe), nil
	}
	return os.Executable()
}

func (l *launcher) previousFor(from string) string {
	if from == "" || from == state.Bundled || from == l.bundledVersion() {
		return state.Bundled
	}
	return from
}

func (l *launcher) processPending() {
	supervised := false
	for {
		p, err := state.ReadPending(l.data)
		if err != nil {
			l.log.Warn("not reading the pending upgrade", "err", err)
		}
		if p == nil {
			l.chooseAgain(supervised)
			return
		}
		unlock, free := l.lockData()
		if !free {
			l.exit(1)
			return
		}
		unlock()
		if p.Kind == state.KindRollback {
			l.rollback(p)
			return
		}
		res := l.upgrade(p)
		switch {
		case res.restart:
			supervised = true
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

func (l *launcher) lockData() (unlock func(), free bool) {
	lock, err := state.Lock(l.data)
	switch {
	case errors.Is(err, state.ErrLocked):
		l.log.Error("another kipp serve is running on the data directory; leaving the pending record as it is",
			"dir", l.data)
		return nil, false
	case err != nil:
		l.log.Warn("not locking the data directory", "err", err)
		return func() {}, true
	}
	return func() { _ = lock.Close() }, true
}

func (l *launcher) chooseAgain(supervised bool) {
	if !supervised || !launched() {
		return
	}
	exe, err := l.bundledExe()
	if err != nil {
		l.log.Error("not choosing the version again: the original executable is unknown", "err", err)
		return
	}
	argv := append([]string{exe, "serve"}, l.args...)
	err = l.exec(exe, argv, withoutKeys(os.Environ(), EnvBundledExe, EnvBundledVersion, EnvLaunched, EnvSupervised))
	l.log.Error("not choosing the version again; running this executable's own", "err", err)
}

type result struct {
	ended   bool
	code    int
	restart bool
	target  string
}

type child struct {
	cmd      *exec.Cmd
	done     chan struct{}
	err      error
	tail     *tailBuffer
	mu       sync.Mutex
	stopping bool
}

func (c *child) signaled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopping
}

func (l *launcher) upgrade(p *state.Pending) result {
	path := state.BinaryPath(l.data, p.Version)
	if err := usable(path, p.Version); err != nil {
		l.log.Error("the pending version cannot run", "version", p.Version, "err", err)
		return l.rollBackUnhealthy(p, nil, "the binary cannot run: "+err.Error())
	}
	_ = os.Remove(state.MigratingPath(l.data))
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigs)
	c, err := l.start(path)
	if err != nil {
		l.log.Error("not starting the pending version", "version", p.Version, "err", err)
		return l.rollBackUnhealthy(p, nil, "it did not start: "+err.Error())
	}
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
		<-c.done
		return result{ended: true, code: exitCode(c.err)}
	default:
		return l.rollBackUnhealthy(p, c, reason)
	}
}

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
	c.cmd.WaitDelay = l.t.grace
	if err := l.spawn(c.cmd); err != nil {
		return nil, err
	}
	go func() {
		c.err = c.cmd.Wait()
		close(c.done)
	}()
	return c, nil
}

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

func (l *launcher) becomeCurrent(p *state.Pending) {
	prev := l.previousFor(p.From)
	steps := []struct {
		what string
		do   func() error
	}{
		{"taking the backup out of the pending record", func() error {
			if p.Backup == "" {
				return nil
			}
			kept := *p
			kept.Backup = ""
			return state.WritePending(l.data, kept)
		}},
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

func (l *launcher) rollBackUnhealthy(p *state.Pending, c *child, reason string) result {
	tail := ""
	if c != nil {
		l.stop(c)
		tail = c.tail.String()
	}
	unlock, free := l.lockData()
	if !free {
		return result{ended: true, code: 1}
	}
	defer unlock()
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

func (l *launcher) rollback(p *state.Pending) {
	unlock, free := l.lockData()
	if !free {
		l.exit(1)
		return
	}
	defer unlock()
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
