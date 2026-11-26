package jobstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ndthuan/kipp/internal/config"
)

// The kinds of change a policy is told of, in KIPP_POLICY_CHANGE: one for
// each way a change reaches commit, and Check for kipp check --policy.
const (
	ChangeCreate  = "create"
	ChangeReplace = "replace"
	ChangeDelete  = "delete"
	ChangeRestore = "restore"
	ChangeResolve = "resolve"
	ChangeCheck   = "check"
)

// DefaultPolicyTimeout is how long a policy may run when Policy.Timeout is
// zero.
const DefaultPolicyTimeout = 60 * time.Second

// PolicyOutputLimit is how much of a policy's output a refusal carries.
const PolicyOutputLimit = 8 << 10

// Policy is a program a site gives kipp to refuse a job set kipp itself
// cannot judge. It is run as `Path <dir>`, dir holding the candidate set
// as a jobs directory: one <job>.yml per job, and _vars.yml when the set
// has one. Exit 0 accepts; any other exit, a failure to start and the
// timeout refuse, and the refusal carries the program's output.
type Policy struct {
	Path    string
	Timeout time.Duration
	Env     []string
}

// PolicyChange says what is being decided: Job is the job changed ("" for
// a directory as a whole), Kind one of the Change constants, Author who
// makes it.
type PolicyChange struct {
	Job, Kind, Author string
}

// Run is nil when the policy accepts files and a *config.Errors, one item
// per line of the policy's output, when it refuses. A nil Policy accepts
// everything.
func (p *Policy) Run(ctx context.Context, files config.Files, c PolicyChange) error {
	if p == nil {
		return nil
	}
	dir, err := os.MkdirTemp("", "kipp-policy-")
	if err != nil {
		return policyRefusal("cannot make its directory: " + err.Error())
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := writeCandidate(dir, files); err != nil {
		return err
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultPolicyTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out := &cappedBuffer{limit: PolicyOutputLimit}
	cmd := exec.CommandContext(ctx, p.Path, dir)
	cmd.Env = append(append([]string{}, p.Env...), "KIPP_POLICY_JOB="+c.Job, "KIPP_POLICY_CHANGE="+c.Kind, "KIPP_POLICY_AUTHOR="+c.Author)
	cmd.Stdout, cmd.Stderr, cmd.WaitDelay = out, out, time.Second
	err = cmd.Run()
	if err == nil {
		return nil
	}
	var lines []string
	if text := strings.TrimSpace(out.buf.String()); text != "" {
		lines = strings.Split(text, "\n")
	}
	if out.cut {
		lines = append(lines, fmt.Sprintf("(output cut at %d KiB)", PolicyOutputLimit>>10))
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		lines = append(lines, fmt.Sprintf("timed out after %s", timeout))
	} else if _, exited := errors.AsType[*exec.ExitError](err); !exited || len(lines) == 0 {
		lines = append(lines, err.Error())
	}
	return policyRefusal(lines...)
}

func writeCandidate(dir string, files config.Files) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return policyRefusal("cannot open its directory: " + err.Error())
	}
	defer func() { _ = root.Close() }()
	for name, text := range files {
		if !filepath.IsLocal(name) || strings.ContainsAny(name, `/\`) {
			return policyRefusal(fmt.Sprintf("%q is not a file name", name))
		}
		if err := root.WriteFile(name, text, 0o600); err != nil {
			return policyRefusal("cannot write " + name + ": " + err.Error())
		}
	}
	return nil
}

func policyRefusal(lines ...string) error {
	items := make([]error, len(lines))
	for i, l := range lines {
		items[i] = errors.New("job policy: " + strings.TrimRight(l, "\r"))
	}
	return &config.Errors{Items: items}
}

type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
	cut   bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := b.limit - b.buf.Len(); n > room {
		b.cut = true
		p = p[:room]
	}
	b.buf.Write(p)
	return n, nil
}
