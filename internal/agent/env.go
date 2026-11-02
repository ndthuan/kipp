package agent

import (
	"slices"

	"github.com/ndthuan/kipp/internal/agentproto"
)

// stageEnv is what a stage on this agent runs with: the agent's own
// environment (an image's ENV, or what a standing agent was started
// with), then what the server gives the stage, then the variables only
// the agent knows, extra last. The server's own PATH, HOME or TMPDIR mean
// nothing here and are not in what it sends, unless --agent-env names one;
// a later entry wins over an earlier one of the same name.
func stageEnv(own []string, a *agentproto.Assignment, workspace, agentName string, extra ...string) []string {
	return slices.Concat(own, a.Env, []string{"KIPP_WORKSPACE=" + workspace, "KIPP_AGENT=" + agentName}, extra)
}
