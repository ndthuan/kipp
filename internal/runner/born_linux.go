package runner

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

// bornOf is when the process was started, in clock ticks since the boot,
// after the id of that boot (the count starts again with each one) and
// before the stage's mark.
func bornOf(pid int, mark string) (string, error) {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", err
	}
	ticks, ok := statBorn(stat)
	if !ok {
		return "", errors.New("/proc/" + strconv.Itoa(pid) + "/stat: no start time in it")
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(boot)) + ":" + ticks + ":" + mark, nil
}

// stillLeads reports whether the process with the group's id is the one
// recorded. Two things must hold. It was born in the same boot and the
// same clock tick: a tick is a hundredth of a second and many processes
// are born in one, so that alone would take a stranger with a reused pid
// for the stage. And its environment carries the stage's mark, which is
// random, was given to the stage alone and passes only to what the stage
// itself starts: no stranger has it. An exec changes neither, unless the
// stage chose to drop its environment, and then the group is left alone.
// While an exec is under way the environment reads as empty for a moment,
// which environOf waits out.
// The environment is the leader's own to read and its user's: one that
// cannot be read (a leader that changed user) is not shown to be ours.
func stillLeads(g Group) (bool, error) {
	at := strings.LastIndexByte(g.Born, ':')
	if at < 0 {
		return false, nil
	}
	mark := g.Born[at+1:]
	born, err := bornOf(g.Pgid, mark)
	if err != nil || born != g.Born {
		return false, err
	}
	env, err := environOf(g.Pgid)
	return err == nil && carries(env, mark), err
}

// execSettles is how long an environment that reads as empty is read
// again. Linux puts a process's new image in place before its environment
// during an exec, and in between shows it with none; that lasts as long as
// loading the binary, far less than this. One still empty after that was
// emptied on purpose.
const execSettles = 100 * time.Millisecond

// environOf is the process's environment, read again while it is empty
// for as long as an exec takes to settle.
func environOf(pid int) ([]byte, error) {
	settled := time.Now().Add(execSettles)
	for {
		env, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
		if err != nil || len(env) > 0 || time.Now().After(settled) {
			return env, err
		}
		time.Sleep(time.Millisecond)
	}
}
