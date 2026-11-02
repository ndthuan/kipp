package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/ndthuan/kipp/internal/agent"
	"github.com/ndthuan/kipp/internal/agenthub"
	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

const agentUsage = `agent --url <server> --workdir <dir> [--token <token> | --token -] [--shell sh]
       kipp agent add <name> --labels <a,b> [--executors N] --data <dir>
       kipp agent set <name> [--labels <a,b>] [--executors N] [--disable | --enable] --data <dir>
       kipp agent token <name> --data <dir>
       kipp agent rm <name> --data <dir>
       kipp agent ls --data <dir>

kipp agent with --url runs a standing agent on this machine: it dials the server, which says
its name and labels, and runs the stages placed on it. The token is --token, - to read it from
stdin, or KIPP_AGENT_TOKEN. It runs the server's own version. The others register agents on
the server's data directory; a running server takes a change at its next reload. --disable
drains an agent: it takes nothing new, and what it runs finishes.`

// The words that make kipp agent a change on the data directory rather
// than an agent that runs.
var agentAdmin = []string{"add", "set", "token", "rm", "ls"}

func cmdAgent(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || !isAgentAdmin(args) {
		if len(args) == 0 {
			say(stderr, "usage: kipp %s", agentUsage)
			return 2
		}
		return agent.Main(args, stdin, stderr)
	}
	fs := newFlags(agentUsage, stderr)
	data := fs.String("data", "", "the server's data `directory` (required)")
	labels := fs.String("labels", "", "`labels`, comma-separated, a stage's runs_on chooses the agent by")
	executors := fs.Int("executors", 1, "how many stages it runs at once")
	disable := fs.Bool("disable", false, "take nothing new; what runs finishes")
	enable := fs.Bool("enable", false, "take stages again")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return exitFor(err)
	}
	af := agentFlags{labels: *labels, executors: *executors, disable: *disable, enable: *enable, given: given(fs)}
	return onStore("agent", *data, fs.Usage, stderr, agentAction(positional, af, stdout))
}

// isAgentAdmin reports whether the arguments name a change on the data
// directory, which an agent's own (a URL, a directory) never are.
func isAgentAdmin(args []string) bool {
	return slices.ContainsFunc(args, func(a string) bool { return slices.Contains(agentAdmin, a) })
}

type agentFlags struct {
	labels          string
	executors       int
	disable, enable bool
	given           map[string]bool
}

func agentAction(args []string, af agentFlags, stdout io.Writer) action {
	switch {
	case len(args) == 2 && args[0] == "add":
		return func(ctx context.Context, st *store.Store) error { return agentAdd(ctx, st, args[1], af, stdout) }
	case len(args) == 2 && args[0] == "set":
		return func(ctx context.Context, st *store.Store) error { return agentSet(ctx, st, args[1], af) }
	case len(args) == 2 && args[0] == "token":
		return func(ctx context.Context, st *store.Store) error { return agentToken(ctx, st, args[1], stdout) }
	case len(args) == 2 && args[0] == "rm":
		return func(ctx context.Context, st *store.Store) error { return agentRm(ctx, st, args[1]) }
	case len(args) == 1 && args[0] == "ls":
		return func(ctx context.Context, st *store.Store) error { return agentLs(ctx, st, stdout) }
	}
	return nil
}

func agentAdd(ctx context.Context, st *store.Store, name string, af agentFlags, stdout io.Writer) error {
	if _, err := st.Agent(ctx, name); err == nil {
		return refused("an agent called " + name + " is there already: kipp agent set changes it")
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if !af.given["labels"] {
		return refused("agent add needs --labels")
	}
	a := &store.Agent{Name: name, Executors: 1, Enabled: true}
	if err := af.apply(a); err != nil {
		return err
	}
	plain, hash, err := auth.NewAgentToken()
	if err != nil {
		return err
	}
	a.TokenHash = hash
	if err := st.PutAgent(ctx, *a); err != nil {
		return err
	}
	say(stdout, "token for agent %s, shown once:\n%s", name, plain)
	return record(ctx, st, "agent.add", name, api.AgentDetail(a))
}

func agentSet(ctx context.Context, st *store.Store, name string, af agentFlags) error {
	a, err := findAgent(ctx, st, name)
	if err != nil {
		return err
	}
	if err := af.apply(a); err != nil {
		return err
	}
	if err := st.PutAgent(ctx, *a); err != nil {
		return err
	}
	return record(ctx, st, "agent.set", name, api.AgentDetail(a))
}

func (af agentFlags) apply(a *store.Agent) error {
	if af.disable && af.enable {
		return refused("--disable or --enable, not both")
	}
	if af.given["labels"] {
		labels, err := config.ParseLabels(af.labels)
		if err != nil {
			return refused("--labels " + err.Error())
		}
		a.Labels = labels
	}
	if af.given["executors"] {
		a.Executors = af.executors
	}
	a.Enabled = (a.Enabled || af.enable) && !af.disable
	if err := agenthub.CheckAgent(a.Name, a.Labels, a.Executors); err != nil {
		return refused(err.Error())
	}
	return nil
}

// agentToken gives the agent a new token; the old one dies, and the agent
// that held it is refused at its next call.
func agentToken(ctx context.Context, st *store.Store, name string, stdout io.Writer) error {
	a, err := findAgent(ctx, st, name)
	if err != nil {
		return err
	}
	plain, hash, err := auth.NewAgentToken()
	if err != nil {
		return err
	}
	a.TokenHash = hash
	if err := st.PutAgent(ctx, *a); err != nil {
		return err
	}
	say(stdout, "token for agent %s, shown once:\n%s", name, plain)
	return record(ctx, st, "agent.token", name, "the old token no longer works")
}

func agentRm(ctx context.Context, st *store.Store, name string) error {
	if err := st.DeleteAgent(ctx, name); errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no agent called %s", name)
	} else if err != nil {
		return err
	}
	return record(ctx, st, "agent.rm", name, "")
}

func agentLs(ctx context.Context, st *store.Store, stdout io.Writer) error {
	as, err := st.Agents(ctx)
	if err != nil {
		return err
	}
	for _, a := range as {
		seen := "never seen"
		if a.LastSeenAt != nil {
			seen = fmt.Sprintf("seen %s, %s/%s, kipp %s", a.LastSeenAt.Local().Format("2006-01-02 15:04"), a.OS, a.Arch, a.Version)
		}
		state := "enabled"
		if !a.Enabled {
			state = "disabled"
		}
		say(stdout, "%s\t%s\texecutors %d\t%s\t%s", a.Name, strings.Join(a.Labels, ","), a.Executors, state, seen)
	}
	return nil
}

func findAgent(ctx context.Context, st *store.Store, name string) (*store.Agent, error) {
	a, err := st.Agent(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("no agent called %s", name)
	}
	return a, err
}
