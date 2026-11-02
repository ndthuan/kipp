package runner

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// bornOf is when the process was started, by the wall clock to the
// microsecond. The mark is not part of it: macOS shows nobody the
// environment of a system binary, /bin/sh included, so it could not be
// read back.
func bornOf(pid int, _ string) (string, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", err
	}
	if int(kp.Proc.P_pid) != pid { // the answer for a pid nobody has
		return "", unix.ESRCH
	}
	at := kp.Proc.P_starttime
	return fmt.Sprintf("%d.%06d", at.Sec, at.Usec), nil
}

// stillLeads reports whether the process with the group's id is the one
// recorded: it is if it was born in the same microsecond of the wall
// clock. That rests on how pids are handed out, not on a rule that forbids
// the contrary: a pid comes round again only after every other number has
// been tried, which no system does within a microsecond. An exec changes
// neither the pid nor the birth.
func stillLeads(g Group) (bool, error) {
	born, err := bornOf(g.Pgid, "")
	return err == nil && g.Born != "" && born == g.Born, err
}
