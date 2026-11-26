package runner

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func bornOf(pid int, _ string) (string, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", err
	}
	if int(kp.Proc.P_pid) != pid {
		return "", unix.ESRCH
	}
	at := kp.Proc.P_starttime
	return fmt.Sprintf("%d.%06d", at.Sec, at.Usec), nil
}

func stillLeads(g Group) (bool, error) {
	born, err := bornOf(g.Pgid, "")
	return err == nil && g.Born != "" && born == g.Born, err
}
