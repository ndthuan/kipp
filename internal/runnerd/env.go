package runnerd

import (
	"slices"

	"github.com/ndthuan/kipp/internal/runnerproto"
)

// stageEnv is what a stage on this runner runs with: the runner's own
// environment (an image's ENV, or what a standing runner was started
// with), then what the server gives the stage, then the variables only
// the runner knows, extra last. The server's own PATH, HOME or TMPDIR mean
// nothing here and are not in what it sends, unless --runner-env names one;
// a later entry wins over an earlier one of the same name.
func stageEnv(own []string, a *runnerproto.Assignment, workspace, runnerName string, extra ...string) []string {
	return slices.Concat(own, a.Env, []string{"KIPP_WORKSPACE=" + workspace, "KIPP_RUNNER=" + runnerName}, extra)
}
