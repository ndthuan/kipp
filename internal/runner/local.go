package runner

import (
	"context"
	"crypto/rand"
	"errors"
	"os/exec"
	"sync/atomic"
	"syscall"
	"time"
)

// Local runs `<Shell> -c <command>` on this machine in a new process group.
type Local struct {
	Shell string
}

const (
	waitDelay   = 2 * time.Second
	cancelGrace = 10 * time.Second
	strayGrace  = 2 * time.Second
	pollEvery   = 50 * time.Millisecond
)

// Start implements Runner. A nil Env is an empty environment, never the
// caller's own, but for the stage's mark.
func (l Local) Start(ctx context.Context, spec Spec) (Proc, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	shell := l.Shell
	if shell == "" {
		shell = "sh"
	}
	cmd := exec.Command(shell, "-c", spec.Command)
	cmd.Dir = spec.Dir
	mark := rand.Text()
	cmd.Env = append(append([]string{}, spec.Env...), MarkEnv+"="+mark)
	cmd.Stdout = spec.Output
	cmd.Stderr = spec.Output
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = waitDelay
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	born, _ := bornOf(cmd.Process.Pid, mark)
	p := &proc{cmd: cmd, pgid: cmd.Process.Pid, born: born, done: make(chan struct{})}
	go p.wait()
	go func() {
		select {
		case <-ctx.Done():
			p.Kill(cancelGrace)
		case <-p.done:
		}
	}()
	return p, nil
}

type proc struct {
	cmd      *exec.Cmd
	pgid     int
	born     string
	done     chan struct{}
	code     int
	err      error
	exitKill bool
	deadline atomic.Int64
}

func (p *proc) wait() {
	err := p.cmd.Wait()
	p.code = p.cmd.ProcessState.ExitCode()
	deadline := time.Now().Add(strayGrace)
	if d := p.deadline.Load(); d != 0 {
		p.exitKill = true
		deadline = time.Unix(0, d)
	}
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) && !errors.Is(err, exec.ErrWaitDelay) {
		p.err = err
	}
	p.signalGroup(syscall.SIGTERM)
	if !groupGoneBy(p.pgid, deadline, p.done) {
		p.signalGroup(syscall.SIGKILL)
	}
	close(p.done)
}

// Group implements Proc.
func (p *proc) Group() Group { return Group{Pgid: p.pgid, Born: p.born} }

// Wait implements Proc.
func (p *proc) Wait() (int, error) {
	<-p.done
	if p.exitKill && p.code == 0 {
		return -1, nil
	}
	return p.code, p.err
}

// Kill implements Proc.
func (p *proc) Kill(grace time.Duration) {
	if p.finished() {
		return
	}
	deadline := time.Now().Add(grace)
	p.deadline.CompareAndSwap(0, deadline.UnixNano())
	p.signalGroup(syscall.SIGTERM)
	if !groupGoneBy(p.pgid, deadline, p.done) && !p.finished() {
		p.signalGroup(syscall.SIGKILL)
	}
}

func (p *proc) finished() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func groupGoneBy(pgid int, deadline time.Time, finished <-chan struct{}) bool {
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	for {
		if errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) {
			return true
		}
		select {
		case <-finished:
			return true
		case <-tick.C:
		}
		if !time.Now().Before(deadline) {
			return false
		}
	}
}

func (p *proc) signalGroup(sig syscall.Signal) {
	_ = syscall.Kill(-p.pgid, sig)
}
