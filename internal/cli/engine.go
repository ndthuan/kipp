package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/enginepool"
	"github.com/ndthuan/kipp/internal/store"
)

const engineUsage = `engine add <name> --docker <url> --labels <a,b> [--max N] [--ssh-key <file>] [--tls-ca <file> --tls-cert <file> --tls-key <file>] --data <dir>
       kipp engine set <name> [--docker <url>] [--labels <a,b>] [--max N] [--ssh-key <file>] [--tls-...] [--disable | --enable] --data <dir>
       kipp engine rm <name> --data <dir>
       kipp engine ls --data <dir>

An engine is a Docker engine a stage with runs_on and image runs in: unix:///path/to/docker.sock,
tcp://host:port with TLS, or ssh://user@host[:port][?socket=/path] to the engine's socket on that
host, whose key must be in the server user's known_hosts. A running server takes a change at its
next reload.`

// engineFlags are the flags of engine add and set; set changes only those
// given.
type engineFlags struct {
	url, labels, sshKey, tlsCA, tlsCert, tlsKey string
	max                                         int
	disable, enable                             bool
	given                                       map[string]bool
}

func cmdEngine(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	fs := newFlags(engineUsage, stderr)
	data := fs.String("data", "", "the server's data `directory` (required)")
	ef := &engineFlags{}
	fs.StringVar(&ef.url, "docker", "", "the engine's `URL`")
	fs.StringVar(&ef.labels, "labels", "", "`labels`, comma-separated, a stage's runs_on chooses the engine by")
	fs.IntVar(&ef.max, "max", 1, "how many containers it runs at once")
	fs.StringVar(&ef.sshKey, "ssh-key", "", "the private key `file` for an ssh:// engine (default the server's --ssh-key, else ~/.ssh)")
	fs.StringVar(&ef.tlsCA, "tls-ca", "", "the CA `file` of a tcp:// engine")
	fs.StringVar(&ef.tlsCert, "tls-cert", "", "the client certificate `file` of a tcp:// engine")
	fs.StringVar(&ef.tlsKey, "tls-key", "", "the client key `file` of a tcp:// engine")
	fs.BoolVar(&ef.disable, "disable", false, "take no new container; what runs finishes")
	fs.BoolVar(&ef.enable, "enable", false, "take containers again")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return exitFor(err)
	}
	ef.given = given(fs)
	return onStore("engine", *data, fs.Usage, stderr, engineAction(positional, ef, stdout))
}

// given is the flags the command line set.
func given(fs *flag.FlagSet) map[string]bool {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return set
}

func engineAction(args []string, ef *engineFlags, stdout io.Writer) action {
	switch {
	case len(args) == 2 && args[0] == "add":
		return func(ctx context.Context, st *store.Store) error { return engineAdd(ctx, st, args[1], ef) }
	case len(args) == 2 && args[0] == "set":
		return func(ctx context.Context, st *store.Store) error { return engineSet(ctx, st, args[1], ef) }
	case len(args) == 2 && args[0] == "rm":
		return func(ctx context.Context, st *store.Store) error { return engineRm(ctx, st, args[1]) }
	case len(args) == 1 && args[0] == "ls":
		return func(ctx context.Context, st *store.Store) error { return engineLs(ctx, st, stdout) }
	}
	return nil
}

func engineAdd(ctx context.Context, st *store.Store, name string, ef *engineFlags) error {
	if _, err := st.Engine(ctx, name); err == nil {
		return refused("an engine called " + name + " is there already: kipp engine set changes it")
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if !ef.given["docker"] || !ef.given["labels"] {
		return refused("engine add needs --docker and --labels")
	}
	e := &store.Engine{Name: name, Kind: "docker", Enabled: true, Max: ef.max}
	if err := ef.apply(e); err != nil {
		return err
	}
	if err := st.PutEngine(ctx, *e); err != nil {
		return err
	}
	return record(ctx, st, "engine.add", name, api.EngineDetail(e))
}

func engineSet(ctx context.Context, st *store.Store, name string, ef *engineFlags) error {
	e, err := st.Engine(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no engine called %s", name)
	} else if err != nil {
		return err
	}
	if err := ef.apply(e); err != nil {
		return err
	}
	if err := st.PutEngine(ctx, *e); err != nil {
		return err
	}
	return record(ctx, st, "engine.set", name, api.EngineDetail(e))
}

// apply puts the flags given into e, and refuses what kipp could not dial
// or choose by.
func (ef *engineFlags) apply(e *store.Engine) error {
	if ef.disable && ef.enable {
		return refused("--disable or --enable, not both")
	}
	if ef.given["docker"] {
		e.URL = ef.url
	}
	if ef.given["labels"] {
		labels, err := config.ParseLabels(ef.labels)
		if err != nil {
			return refused("--labels " + err.Error())
		}
		e.Labels = labels
	}
	if ef.given["max"] {
		e.Max = ef.max
	}
	ef.setIf("ssh-key", &e.SSHKey, ef.sshKey)
	ef.setIf("tls-ca", &e.TLSCA, ef.tlsCA)
	ef.setIf("tls-cert", &e.TLSCert, ef.tlsCert)
	ef.setIf("tls-key", &e.TLSKey, ef.tlsKey)
	e.Enabled = (e.Enabled || ef.enable) && !ef.disable
	if err := enginepool.Check(e); err != nil {
		return refused(err.Error())
	}
	return nil
}

// setIf sets *dst to v when the flag was given.
func (ef *engineFlags) setIf(flag string, dst *string, v string) {
	if ef.given[flag] {
		*dst = v
	}
}

func engineRm(ctx context.Context, st *store.Store, name string) error {
	if err := st.DeleteEngine(ctx, name); errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no engine called %s", name)
	} else if err != nil {
		return err
	}
	return record(ctx, st, "engine.rm", name, "")
}

func engineLs(ctx context.Context, st *store.Store, stdout io.Writer) error {
	es, err := st.Engines(ctx)
	if err != nil {
		return err
	}
	for _, e := range es {
		state := "enabled"
		if !e.Enabled {
			state = "disabled"
		}
		say(stdout, "%s\t%s\t%s\t%s\tmax %d\t%s", e.Name, e.Kind, enginepool.Redact(e.URL), strings.Join(e.Labels, ","), e.Max, state)
	}
	return nil
}
