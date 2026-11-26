package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/config"
)

var (
	tailEvery  = time.Second
	retryFirst = time.Second
)

const retries = 5

type params map[string]string

// String is for the flag package, which prints a default; there is none.
func (p params) String() string { return "" }

// Set takes one KEY=VALUE.
func (p params) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	if !ok || k == "" {
		return fmt.Errorf("want KEY=VALUE, got %q", s)
	}
	p[k] = v
	return nil
}

type paramFiles map[string]string

// String is for the flag package, which prints a default; there is none.
func (p paramFiles) String() string { return "" }

// Set reads one KEY=FILE. No more is read than a request could carry and
// one byte, which is enough for the request to be refused for its size.
func (p paramFiles) Set(s string) error {
	k, file, ok := strings.Cut(s, "=")
	if !ok || k == "" || file == "" {
		return fmt.Errorf("want KEY=FILE, got %q", s)
	}
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, api.MaxBody+1))
	if err != nil {
		return err
	}
	p[k] = string(data)
	return nil
}

func triggerBody(values params, files paramFiles) (map[string]any, error) {
	given, err := merged(values, files)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"params": given}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	if len(data) > api.MaxBody {
		return nil, refused(fmt.Sprintf("the params make a request of %d bytes; the server takes at most %d bytes", len(data), api.MaxBody))
	}
	return body, nil
}

func merged(values params, files paramFiles) (map[string]string, error) {
	out := maps.Clone(map[string]string(values))
	for k, v := range files {
		if _, both := out[k]; both {
			return nil, refused(fmt.Sprintf("%s is given by both -p and --param-file", k))
		}
		out[k] = v
	}
	return out, nil
}

type buildReply struct {
	ID     int64  `json:"id"`
	Job    string `json:"job"`
	Number int    `json:"number"`
	Status string `json:"status"`
	Stages []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"stages"`
}

func (b *buildReply) ended() bool { return b.Status != "queued" && b.Status != "running" }

func (b *buildReply) logs() []string {
	var names []string
	for _, s := range b.Stages {
		if s.Status != "pending" && s.Status != "skipped" {
			names = append(names, s.Name)
		}
	}
	if len(names) == 0 || b.ended() {
		names = append([]string{config.CloneLog, config.PrepareLog}, names...)
	}
	if b.ended() {
		names = append(names, config.HooksLog)
	}
	return names
}

const runUsage = `run <job> [-p KEY=VALUE]... [--param-file KEY=FILE]... [--wait] [--url <url>] [--token <token>]

With --wait it exits 0 if the build succeeds or ends unstable (stderr
then names the unstable stages) and 1 if it ends any other way, also
when the rest of its output could not be read. It exits 4 when
the build was queued and its end could not be learned (the server stopped
answering, or no longer knows the build): the build may still be running.
2 is a command line it would not take, 3 a token the server refused.
`

const exitUnknown = 4

func cmdRun(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	fs := newFlags(runUsage, stderr)
	values, files := params{}, paramFiles{}
	fs.Var(values, "p", "a parameter of the job, `KEY=VALUE`, the value as written; may be repeated")
	fs.Var(files, "param-file", "a parameter whose value is all of a file, `KEY=FILE`; may be repeated.\nA text param takes the file whole. A string param is one line, so it\nrefuses a file that ends in a newline")
	wait := fs.Bool("wait", false, "print the build's output as it runs, and exit by how it ended")
	newClient := clientFlags(fs)
	positional, err := parseArgs(fs, args)
	if err != nil {
		return exitFor(err)
	}
	if len(positional) != 1 {
		fs.Usage()
		return 2
	}
	c, b, err := startBuild(newClient, positional[0], values, files)
	if err != nil {
		return fail(stderr, "run", err)
	}
	say(stdout, "%s #%d queued (build %d)", b.Job, b.Number, b.ID)
	if !*wait {
		return 0
	}
	return waitFor(c, b, stdout, stderr)
}

func startBuild(newClient func() (*client, error), job string, values params, files paramFiles) (*client, *buildReply, error) {
	body, err := triggerBody(values, files)
	if err != nil {
		return nil, nil, err
	}
	c, err := newClient()
	if err != nil {
		return nil, nil, err
	}
	var b buildReply
	if err := c.call(http.MethodPost, "/jobs/"+url.PathEscape(job)+"/builds", body, &b); err != nil {
		return nil, nil, err
	}
	return c, &b, nil
}

func waitFor(c *client, b *buildReply, stdout, stderr io.Writer) int {
	err := follow(c, b, stdout, stderr)
	where := fmt.Sprintf("%s/jobs/%s/%d", c.url, url.PathEscape(b.Job), b.Number)
	switch {
	case err != nil && b.ended():
		say(stderr, "kipp run: %v; build %d ended as %s and its output is incomplete, see %s", err, b.ID, b.Status, where)
	case errors.Is(err, errRefused):
		return fail(stderr, "run", err)
	case err != nil:
		say(stderr, "kipp run: %v; build %d may still be running, see %s", err, b.ID, where)
		return exitUnknown
	}
	say(stdout, "%s #%d: %s", b.Job, b.Number, b.Status)
	switch b.Status {
	case "success":
		return 0
	case "unstable":
		say(stderr, "kipp run: %s", b.unstable())
		return 0
	}
	return 1
}

func (b *buildReply) unstable() string {
	var names []string
	for _, s := range b.Stages {
		if s.Status == "unstable" {
			names = append(names, s.Name)
		}
	}
	line := fmt.Sprintf("%s #%d ended unstable", b.Job, b.Number)
	if len(names) > 0 {
		line += "; unstable stages: " + strings.Join(names, ", ")
	}
	return line
}

func follow(c *client, b *buildReply, stdout, stderr io.Writer) error {
	t := &tail{c: c, build: b.ID, out: stdout, offset: map[string]int64{}, partial: map[string][]byte{}}
	failed := 0
	for {
		done, err := t.poll(b)
		switch {
		case errors.Is(err, errNoAnswer) && failed < retries:
			wait := retryFirst << failed
			failed++
			say(stderr, "kipp run: %v; the build goes on, asking again in %v", err, wait)
			time.Sleep(wait)
			continue
		case errors.Is(err, errNoAnswer):
			return fmt.Errorf("gave up after asking %d times: %w", failed+1, err)
		case err != nil || done:
			return err
		}
		failed = 0
		time.Sleep(tailEvery)
	}
}

type tail struct {
	c       *client
	build   int64
	out     io.Writer
	offset  map[string]int64
	partial map[string][]byte
}

func (t *tail) poll(b *buildReply) (bool, error) {
	if err := t.c.call(http.MethodGet, fmt.Sprintf("/builds/%d", t.build), nil, b); err != nil {
		return false, err
	}
	names := b.logs()
	more, err := t.copyAll(names)
	if err != nil || more || !b.ended() {
		return false, err
	}
	t.flush(names)
	return true, nil
}

func (t *tail) copyAll(names []string) (bool, error) {
	unfinished := false
	for _, name := range names {
		more, err := t.copy(name)
		if err != nil {
			return false, err
		}
		unfinished = unfinished || more
	}
	return unfinished, nil
}

func (t *tail) copy(name string) (bool, error) {
	for {
		data, next, more, err := t.c.chunk(t.build, name, t.offset[name])
		if err != nil || len(data) == 0 {
			return more, err
		}
		t.offset[name] = next
		data = append(t.partial[name], data...)
		whole := bytes.LastIndexByte(data, '\n') + 1
		for line := range bytes.Lines(data[:whole]) {
			_, _ = fmt.Fprintf(t.out, "[%s] %s", name, line)
		}
		t.partial[name] = data[whole:]
	}
}

func (t *tail) flush(names []string) {
	for _, name := range names {
		if rest := t.partial[name]; len(rest) > 0 {
			say(t.out, "[%s] %s", name, rest)
		}
	}
}
