package runner

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"syscall"
	"time"
)

// MarkEnv is a variable every stage is started with, holding a random
// value of its own. Where the system lets a process's environment be read
// back, it is half of what tells the stage's leader from any process that
// later comes to have its pid. It is not a secret and is not masked: all
// it could gain anyone who learned it is to have their own process group
// killed in the stage's place after a crash.
const MarkEnv = "KIPP_MARK"

// Group is what is kept of a started stage: the id of its process group,
// which is the pid of the process leading it, and what tells that process
// from any other that may come to have the pid. Born is that: when the
// system says the leader was born, in the system's own figure, and on
// Linux the stage's mark after it. It is kept as it was made and only ever
// held against the same made again; it is empty where there is nothing to
// read.
type Group struct {
	Pgid int
	Born string
}

// ErrNotProven is Reap's answer for a group that exists and cannot be
// shown to be the stage's.
var ErrNotProven = errors.New("not shown to be the stage's")

// Reap implements Runner.
func (Local) Reap(g Group) (bool, error) { return reap(g, cancelGrace) }

// reap signals the group only when the process leading it, the one whose
// pid is the group's id, is shown to be the stage's own shell or what that
// became by exec. The id alone proves nothing: once the stage's group is
// gone the system hands its number to a new process, which may lead a
// group of its own. What shows it differs by system, and stillLeads says
// what on each. A group whose leader has exited, or can no longer be
// shown to be the stage's, is left running: nothing then tells it from a
// stranger's.
//
// Between the check and the signal the leader could exit and its number
// be handed out again; a system does not reuse a number that fast, and a
// stage that is being killed has the same window.
func reap(g Group, grace time.Duration) (bool, error) {
	if g.Pgid <= 1 { // kill(-1) and kill(0) mean far more than one group
		return false, nil
	}
	if errors.Is(syscall.Kill(-g.Pgid, 0), syscall.ESRCH) {
		return false, nil
	}
	ours, err := stillLeads(g)
	if errors.Is(err, errors.ErrUnsupported) {
		return false, fmt.Errorf("process group %d: this system does not say which process one is: %w", g.Pgid, err)
	}
	if err != nil || !ours {
		return false, fmt.Errorf("process group %d: %w", g.Pgid, ErrNotProven)
	}
	_ = syscall.Kill(-g.Pgid, syscall.SIGTERM)
	if !groupGoneBy(g.Pgid, time.Now().Add(grace), nil) {
		_ = syscall.Kill(-g.Pgid, syscall.SIGKILL)
		// So that whoever asked can count on the group being gone.
		groupGoneBy(g.Pgid, time.Now().Add(strayGrace), nil)
	}
	return true, nil
}

// carries reports whether env, the zero-separated entries of a process's
// environment, holds the mark as a whole entry.
func carries(env []byte, mark string) bool {
	if mark == "" {
		return false
	}
	entry := "\x00" + MarkEnv + "=" + mark + "\x00"
	return bytes.Contains(slices.Concat([]byte{0}, env, []byte{0}), []byte(entry))
}

// statBorn is the start time in a line of Linux's /proc/<pid>/stat: the
// 22nd field, in clock ticks since the boot. The second field is the
// command's name in brackets and may hold spaces and brackets itself, so
// the fields are counted from the last closing bracket.
func statBorn(stat []byte) (string, bool) {
	_, rest, ok := bytes.Cut(stat, []byte(") "))
	for ok && bytes.Contains(rest, []byte(") ")) {
		_, rest, ok = bytes.Cut(rest, []byte(") "))
	}
	fields := bytes.Fields(rest)
	const startTime = 22 - 3 // rest begins at the third field
	if !ok || len(fields) <= startTime {
		return "", false
	}
	return string(fields[startTime]), true
}
