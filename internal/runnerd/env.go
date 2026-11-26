package runnerd

import (
	"slices"

	"github.com/ndthuan/kipp/internal/runnerproto"
)

func stageEnv(own []string, a *runnerproto.Assignment, workspace, runnerName string, extra ...string) []string {
	return slices.Concat(own, a.Env, []string{"KIPP_WORKSPACE=" + workspace, "KIPP_RUNNER=" + runnerName}, extra)
}
