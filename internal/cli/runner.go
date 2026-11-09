package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/runnerd"
	"github.com/ndthuan/kipp/internal/runnerhub"
	"github.com/ndthuan/kipp/internal/store"
)

const runnerUsage = `runner --url <server> --workdir <dir> [--token <token> | --token -] [--shell sh]
       kipp runner add <name> --labels <a,b> [--executors N] --data <dir>
       kipp runner set <name> [--labels <a,b>] [--executors N] [--disable | --enable] --data <dir>
       kipp runner token <name> --data <dir>
       kipp runner rm <name> --data <dir>
       kipp runner ls --data <dir>

kipp runner with --url runs a standing runner on this machine: it dials the server, which says
its name and labels, and runs the stages placed on it. The token is --token, - to read it from
stdin, or KIPP_RUNNER_TOKEN. It runs the server's own version. The others register runners on
the server's data directory; a running server takes a change at its next reload. --disable
drains a runner: it takes nothing new, and what it runs finishes.`

// The words that make kipp runner a change on the data directory rather
// than a runner that runs.
var runnerAdmin = []string{"add", "set", "token", "rm", "ls"}

func cmdRunner(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || !isRunnerAdmin(args) {
		if len(args) == 0 {
			say(stderr, "usage: kipp %s", runnerUsage)
			return 2
		}
		return runnerd.Main(args, stdin, stderr)
	}
	fs := newFlags(runnerUsage, stderr)
	data := fs.String("data", "", "the server's data `directory` (required)")
	labels := fs.String("labels", "", "`labels`, comma-separated, a stage's runs_on chooses the runner by")
	executors := fs.Int("executors", 1, "how many stages it runs at once")
	disable := fs.Bool("disable", false, "take nothing new; what runs finishes")
	enable := fs.Bool("enable", false, "take stages again")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return exitFor(err)
	}
	af := runnerFlags{labels: *labels, executors: *executors, disable: *disable, enable: *enable, given: given(fs)}
	return onStore("runner", *data, fs.Usage, stderr, runnerAction(positional, af, stdout))
}

// isRunnerAdmin reports whether the arguments name a change on the data
// directory, which a runner's own (a URL, a directory) never are.
func isRunnerAdmin(args []string) bool {
	return slices.ContainsFunc(args, func(a string) bool { return slices.Contains(runnerAdmin, a) })
}

type runnerFlags struct {
	labels          string
	executors       int
	disable, enable bool
	given           map[string]bool
}

func runnerAction(args []string, af runnerFlags, stdout io.Writer) action {
	switch {
	case len(args) == 2 && args[0] == "add":
		return func(ctx context.Context, st *store.Store) error { return runnerAdd(ctx, st, args[1], af, stdout) }
	case len(args) == 2 && args[0] == "set":
		return func(ctx context.Context, st *store.Store) error { return runnerSet(ctx, st, args[1], af) }
	case len(args) == 2 && args[0] == "token":
		return func(ctx context.Context, st *store.Store) error { return runnerToken(ctx, st, args[1], stdout) }
	case len(args) == 2 && args[0] == "rm":
		return func(ctx context.Context, st *store.Store) error { return runnerRm(ctx, st, args[1]) }
	case len(args) == 1 && args[0] == "ls":
		return func(ctx context.Context, st *store.Store) error { return runnerLs(ctx, st, stdout) }
	}
	return nil
}

func runnerAdd(ctx context.Context, st *store.Store, name string, af runnerFlags, stdout io.Writer) error {
	if _, err := st.Runner(ctx, name); err == nil {
		return refused("a runner called " + name + " is there already: kipp runner set changes it")
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if !af.given["labels"] {
		return refused("runner add needs --labels")
	}
	a := &store.Runner{Name: name, Executors: 1, Enabled: true}
	if err := af.apply(a); err != nil {
		return err
	}
	plain, hash, err := auth.NewRunnerToken()
	if err != nil {
		return err
	}
	a.TokenHash = hash
	if err := st.PutRunner(ctx, *a); err != nil {
		return err
	}
	say(stdout, "token for runner %s, shown once:\n%s", name, plain)
	return record(ctx, st, "runner.add", name, api.RunnerDetail(a))
}

func runnerSet(ctx context.Context, st *store.Store, name string, af runnerFlags) error {
	a, err := findRunner(ctx, st, name)
	if err != nil {
		return err
	}
	if err := af.apply(a); err != nil {
		return err
	}
	if err := st.PutRunner(ctx, *a); err != nil {
		return err
	}
	return record(ctx, st, "runner.set", name, api.RunnerDetail(a))
}

func (af runnerFlags) apply(a *store.Runner) error {
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
	if err := runnerhub.CheckRunner(a.Name, a.Labels, a.Executors); err != nil {
		return refused(err.Error())
	}
	return nil
}

// runnerToken gives the runner a new token; the old one dies, and the runner
// that held it is refused at its next call.
func runnerToken(ctx context.Context, st *store.Store, name string, stdout io.Writer) error {
	a, err := findRunner(ctx, st, name)
	if err != nil {
		return err
	}
	plain, hash, err := auth.NewRunnerToken()
	if err != nil {
		return err
	}
	a.TokenHash = hash
	if err := st.PutRunner(ctx, *a); err != nil {
		return err
	}
	say(stdout, "token for runner %s, shown once:\n%s", name, plain)
	return record(ctx, st, "runner.token", name, "the old token no longer works")
}

func runnerRm(ctx context.Context, st *store.Store, name string) error {
	if err := st.DeleteRunner(ctx, name); errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no runner called %s", name)
	} else if err != nil {
		return err
	}
	return record(ctx, st, "runner.rm", name, "")
}

func runnerLs(ctx context.Context, st *store.Store, stdout io.Writer) error {
	as, err := st.Runners(ctx)
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

func findRunner(ctx context.Context, st *store.Store, name string) (*store.Runner, error) {
	a, err := st.Runner(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("no runner called %s", name)
	}
	return a, err
}
