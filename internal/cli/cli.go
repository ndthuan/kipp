// Package cli is kipp's command line: one function per subcommand, all wiring.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/ndthuan/kipp/internal/cache"
	"github.com/ndthuan/kipp/internal/update"
	"github.com/ndthuan/kipp/internal/version"
)

// Command runs one subcommand and returns the process exit status.
type Command func(args []string, stdin io.Reader, stdout, stderr io.Writer) int

const usage = `usage: kipp <command> [flags]

  serve     run the server: web, API, scheduler and runner
  check     validate a jobs directory and exit non-zero on any error
  reload    ask a running server to re-read its jobs directory
  quiet     on, off, status: stop a running server taking builds, before a restart
  run       start a build and optionally wait for it
  user      add, passwd, delete, grant, revoke, list
  token     create, revoke, list
  secret    set, unset, list
  engine    add, set, rm, ls: the Docker engines a stage with an image runs on
  agent     run a standing agent, or add, set, token, rm, ls them
  metrics   check, put, forget, ls: a stage's guards, what the metrics stores remember
  cache     restore, save, hash: a stage's keyed cache, kept by the server; ls, rm: what it keeps
  version   print the version
`

var commands = map[string]Command{
	"serve":   cmdServe,
	"check":   cmdCheck,
	"reload":  cmdReload,
	"quiet":   cmdQuiet,
	"run":     cmdRun,
	"user":    cmdUser,
	"token":   cmdToken,
	"secret":  cmdSecret,
	"engine":  cmdEngine,
	"agent":   cmdAgent,
	"metrics": cmdMetrics,
	"cache":   cmdCache,
	"version": cmdVersion,
}

// Main dispatches args[0] to its command.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		_, _ = io.WriteString(stdout, usage)
		return 0
	}
	cmd, ok := commands[args[0]]
	if !ok {
		_, _ = fmt.Fprintf(stderr, "kipp: no command called %q\n%s", args[0], usage)
		return 2
	}
	return cmd(args[1:], stdin, stdout, stderr)
}

// cmdCache is inside a stage: no data directory, only the URL and token the
// server gave it.
// ls and rm are the admin's, on the data directory as engine and agent are;
// the rest is the stage's.
func cmdCache(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 && (args[0] == "ls" || args[0] == "rm") {
		return cmdCacheAdmin(args, stdout, stderr)
	}
	return cache.Command(args, cache.OSEnv(stdout, stderr))
}

func cmdVersion(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	if _, err := parseArgs(newFlags("version", stderr), args); err != nil {
		return exitFor(err)
	}
	say(stdout, "kipp %s", version.Version)
	say(stdout, "installed from: %s", update.Origin())
	return 0
}

// say prints one line. A terminal that cannot be written to has nowhere
// to report that.
func say(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format+"\n", args...)
}

// jobCount is n as a number of jobs: "1 job", and "N jobs" for any other.
func jobCount(n int) string {
	if n == 1 {
		return "1 job"
	}
	return fmt.Sprintf("%d jobs", n)
}

// refused is input a command will not take: exit 2, where a failure is 1.
type refused string

func (r refused) Error() string { return string(r) }

// newFlags is one command's flags; usage is what follows "usage: kipp ".
func newFlags(usage string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(usage, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		say(stderr, "usage: kipp %s", usage)
		fs.PrintDefaults()
	}
	return fs
}

// parseArgs parses flags wherever they stand among the arguments, which it
// returns: the flag package alone stops at the first argument, and
// `kipp run hello --wait` must mean what `kipp run --wait hello` does.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// exitFor is the status after parseArgs fails; the flag package has
// already said why. Asking for help is not a failure.
func exitFor(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return 2
}

// fail reports a command's error and gives its exit status: 2 for input
// it would not take, 3 for a token the server refused, 1 for the rest.
func fail(stderr io.Writer, cmd string, err error) int {
	say(stderr, "kipp %s: %v", cmd, err)
	var bad refused
	switch {
	case errors.As(err, &bad):
		return 2
	case errors.Is(err, errRefused):
		return 3
	}
	return 1
}
