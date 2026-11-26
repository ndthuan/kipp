package runner

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

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

const execSettles = 2 * time.Second

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
