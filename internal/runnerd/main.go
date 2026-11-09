package runnerd

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/ndthuan/kipp/internal/cache"
	"github.com/ndthuan/kipp/internal/metrics"
)

// The exit codes of a runner that stopped by itself.
const (
	ExitRefused = 3 // the server refused the token
	ExitVersion = 5 // the server is another kipp version
)

// Command is kipp-runner's command line, the runner kipp puts into a
// container: metrics check <glob> and cache, which a stage there runs as
// "$KIPP_BIN" metrics check as it would kipp's own on the server, or else
// the runner itself, as Main.
func Command(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "metrics" {
		if len(args) > 1 && args[1] == "check" {
			return metrics.CheckCommand(args[2:], os.Getenv, stdout)
		}
		_, _ = fmt.Fprintf(stderr, "usage: kipp-runner %s\n", metrics.CheckUsage)
		return metrics.CheckNoMetrics
	}
	if len(args) > 0 && args[0] == "cache" {
		return cache.Command(args[1:], cache.OSEnv(stdout, stderr))
	}
	return Main(args, stdin, stderr)
}

// Main runs a runner from its command line until SIGINT or SIGTERM, and
// is its exit code. The token is --token, - to read it from stdin, or
// KIPP_RUNNER_TOKEN.
func Main(args []string, stdin io.Reader, stderr io.Writer) int {
	fs := flag.NewFlagSet("runner", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "the server, as this machine reaches it")
	token := fs.String("token", "", "the runner's token; - reads it from stdin, and none is "+TokenEnv)
	workdir := fs.String("workdir", "", "where the builds' workspaces go")
	shell := fs.String("shell", "sh", "the shell a stage's command runs with, -c")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	c, err := configFrom(*url, *token, *workdir, *shell, stdin)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "kipp runner: %v\n", err)
		return 2
	}
	c.Log = slog.New(slog.NewTextHandler(stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = Run(ctx, c)
	switch _, version := errors.AsType[*VersionError](err); {
	case err == nil || errors.Is(err, context.Canceled):
		return 0
	case errors.Is(err, ErrTokenRefused):
		_, _ = fmt.Fprintf(stderr, "kipp runner: %v\n", err)
		return ExitRefused
	case version:
		_, _ = fmt.Fprintf(stderr, "kipp runner: %v\n", err)
		return ExitVersion
	}
	_, _ = fmt.Fprintf(stderr, "kipp runner: %v\n", err)
	return 1
}

func configFrom(url, token, workdir, shell string, stdin io.Reader) (Config, error) {
	if url == "" || workdir == "" {
		return Config{}, errors.New("--url and --workdir are needed")
	}
	switch token {
	case "-":
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return Config{}, err
		}
		token = strings.TrimSpace(line)
	case "":
		token = os.Getenv(TokenEnv)
	}
	if token == "" {
		return Config{}, errors.New("no token: --token, --token - for stdin, or " + TokenEnv)
	}
	return Config{URL: url, Token: token, Workdir: workdir, Shell: shell}, nil
}
