package cache

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// Exit codes of kipp cache. A restore that finds nothing, or cannot be
// served, is a miss and no failure of its own, as GitHub's cache is, so that
// a cache outage never fails a build by itself; a save or a hash that does
// not do what it was asked is 2.
const (
	ExitRestored = 0 // restore: the exact key; save: kept, or already there
	ExitMiss     = 1 // restore: a prefix hit, nothing, or any error
	ExitFailed   = 2 // save refused or failed; hash on a file it cannot read; usage
)

// Usage is the command's help.
const Usage = `usage: kipp cache restore --key K [--restore-key PREFIX]... --path P... [--scope S]
       kipp cache save    --key K --path P... [--scope S]
       kipp cache hash    FILE...`

// Env is what the command runs in: the stage's variables, directory and
// home, and where it speaks.
type Env struct {
	Getenv         func(string) string
	Dir, Home      string
	Stdout, Stderr io.Writer
	// HTTP is the client for the server; nil is the default.
	HTTP *http.Client
}

// OSEnv is the process's own.
func OSEnv(stdout, stderr io.Writer) Env {
	dir, _ := os.Getwd()
	home, _ := os.UserHomeDir()
	return Env{Getenv: os.Getenv, Dir: dir, Home: home, Stdout: stdout, Stderr: stderr}
}

// Command runs kipp cache with args (what follows "cache") and is its exit
// code.
func Command(args []string, e Env) int {
	if len(args) > 0 {
		switch args[0] {
		case "restore":
			return restore(args[1:], e)
		case "save":
			return save(args[1:], e)
		case "hash":
			return hashFiles(args[1:], e)
		}
	}
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		_, _ = fmt.Fprintln(e.Stdout, Usage)
		return 0
	}
	if len(args) > 0 {
		say(e, "no command %q", args[0])
	}
	_, _ = fmt.Fprintln(e.Stderr, Usage)
	return ExitFailed
}

// list is a flag given more than once.
type list []string

func (l *list) String() string { return strings.Join(*l, ",") }

// Set adds one more value of the flag.
func (l *list) Set(v string) error { *l = append(*l, v); return nil }

type options struct {
	key, scope  string
	restoreKeys list
	paths       list
}

func parse(name string, args []string, e Env, withPrefixes bool) (*options, error) {
	var o options
	fs := flag.NewFlagSet("cache "+name, flag.ContinueOnError)
	fs.SetOutput(e.Stderr)
	fs.StringVar(&o.key, "key", "", "the cache `key`")
	fs.StringVar(&o.scope, "scope", "", "the `scope`; default the first of the job's cache_scopes")
	fs.Var(&o.paths, "path", "a `path` to cache, relative to the stage's directory, absolute or ~/; repeat it")
	if withPrefixes {
		fs.Var(&o.restoreKeys, "restore-key", "a key `prefix` to restore the newest entry of when the key has none; repeat it, tried in order")
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	switch {
	case fs.NArg() > 0:
		return nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	case !ValidKey(o.key):
		return nil, errors.New("--key is 1 to 512 letters, digits and . _ + = -")
	case len(o.paths) == 0:
		return nil, errors.New("--path is needed at least once")
	}
	return &o, nil
}

func say(e Env, format string, a ...any) {
	_, _ = fmt.Fprintf(e.Stderr, "kipp cache: "+format+"\n", a...)
}

func restore(args []string, e Env) int {
	o, err := parse("restore", args, e, true)
	if err != nil {
		say(e, "%v", err)
		return ExitFailed
	}
	c, err := newClient(e.Getenv, o.scope, e.HTTP)
	if err != nil {
		say(e, "%v; counted as a miss", err)
		return ExitMiss
	}
	ctx := context.Background()
	found, err := c.lookup(ctx, o.key, o.restoreKeys)
	if errors.Is(err, errMiss) {
		say(e, "miss: no entry for key %s", o.key)
		return ExitMiss
	}
	if err != nil {
		say(e, "could not ask the server: %v; counted as a miss", err)
		return ExitMiss
	}
	body, err := c.fetch(ctx, found.ID)
	if err != nil {
		say(e, "could not fetch the entry %s: %v; counted as a miss", found.Key, err)
		return ExitMiss
	}
	defer func() { _ = body.Close() }()
	if err := Unpack(body, o.paths, e.Home, e.Dir); err != nil {
		say(e, "the entry %s could not be restored: %v; counted as a miss", found.Key, err)
		return ExitMiss
	}
	if found.Exact {
		say(e, "restored from key %s", found.Key)
		return ExitRestored
	}
	say(e, "restored from key %s, the newest entry that starts like the keys asked for; not an exact hit", found.Key)
	return ExitMiss
}

func save(args []string, e Env) int {
	o, err := parse("save", args, e, false)
	if err != nil {
		say(e, "%v", err)
		return ExitFailed
	}
	c, err := newClient(e.Getenv, o.scope, e.HTTP)
	if err != nil {
		say(e, "not saved: %v", err)
		return ExitFailed
	}
	pr, pw := io.Pipe()
	packed := make(chan error, 1)
	go func() {
		err := Pack(&limitWriter{w: pw, left: EntryMax}, o.paths, e.Home, e.Dir)
		_ = pw.CloseWithError(err) // nil ends the body, an error aborts the request
		packed <- err
	}()
	kept, err := c.put(context.Background(), o.key, pr)
	_ = pr.Close()
	if perr := <-packed; perr != nil {
		err = perr
	}
	switch {
	case err != nil:
		say(e, "not saved: %v", err)
		return ExitFailed
	case !kept:
		say(e, "key %s already exists and is never overwritten; not saved", o.key)
	default:
		say(e, "saved with key %s", o.key)
	}
	return ExitRestored
}

// errTooBig is an entry past EntryMax.
var errTooBig = errors.New("the entry is over 10 GiB")

// limitWriter refuses what comes to more than left bytes.
type limitWriter struct {
	w    io.Writer
	left int64
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.left {
		return 0, errTooBig
	}
	l.left -= int64(len(p))
	return l.w.Write(p)
}

func hashFiles(files []string, e Env) int {
	sum, err := Hash(files)
	if err != nil {
		say(e, "%v", err)
		return ExitFailed
	}
	_, _ = fmt.Fprintln(e.Stdout, sum)
	return 0
}
