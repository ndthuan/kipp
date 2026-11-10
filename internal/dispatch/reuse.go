package dispatch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/mask"
	"github.com/ndthuan/kipp/internal/runnerproto"
	"github.com/ndthuan/kipp/internal/store"
)

// fingerprintTimeout is how long one fingerprint command may take.
const fingerprintTimeout = 30 * time.Second

// takeFingerprint runs the job's fingerprint commands with the build's
// environment and records their joined output and its SHA-256. Only the
// output counts, not the command: two jobs may reach the same tool by
// different paths. A command that fails fails the build, since a
// fingerprint that could not be taken must never match.
func (d *Dispatcher) takeFingerprint(ctx context.Context, b *store.Build, job *config.Job, e *buildEnv) error {
	if len(job.Fingerprint) == 0 {
		return nil
	}
	outs := make([]string, 0, len(job.Fingerprint))
	for _, command := range job.Fingerprint {
		out, err := fingerprintOnce(ctx, e, command)
		if err != nil {
			return err
		}
		outs = append(outs, out)
	}
	var masked bytes.Buffer
	w := mask.NewWriter(&masked, e.secrets)
	if _, err := w.Write([]byte(strings.Join(outs, "\n"))); err != nil {
		return err
	}
	if err := w.Flush(); err != nil {
		return err
	}
	sum := sha256.Sum256(masked.Bytes())
	e.fingerprintSum = hex.EncodeToString(sum[:])
	if err := d.st.SetBuildFingerprint(context.WithoutCancel(ctx), b.ID, masked.String(), e.fingerprintSum); err != nil {
		return fmt.Errorf("recording the fingerprint: %w", err)
	}
	return nil
}

// fingerprintOnce runs one command in the workspace and returns what it
// printed on its standard output, without the newlines it ends with.
func fingerprintOnce(ctx context.Context, e *buildEnv, command string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, fingerprintTimeout)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir, cmd.Env, cmd.Stdout = e.workspace, e.env, &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = cloneWaitDelay
	err := cmd.Run()
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "", fmt.Errorf("fingerprint %q did not end within %s", command, fingerprintTimeout)
	case err != nil:
		return "", fmt.Errorf("fingerprint %q: %w", command, err)
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

// recordTree keeps the git tree of the checkout kipp made for the build,
// which a later build may find as a green run. A tree that cannot be read
// is none, and nothing matches none.
func (d *Dispatcher) recordTree(ctx context.Context, b *store.Build, dir string) {
	tree, err := gitTree(ctx, dir)
	if err != nil {
		d.log.Warn("reading the tree of a build's checkout", "job", b.Job, "number", b.Number, "err", err)
		return
	}
	if err := d.st.SetBuildTree(context.WithoutCancel(ctx), b.ID, tree); err != nil {
		d.log.Error("recording the tree of a build's checkout", "job", b.Job, "number", b.Number, "err", err)
	}
}

// reuse decides, before a stage with reuse starts, whether a green run
// lets it end success without running. It says why in the stage's log,
// and returns the tree a run of the stage gates, empty when it gates none
// a later build could match.
func (d *Dispatcher) reuse(ctx context.Context, b *store.Build, e *buildEnv, s *config.Stage) (tree string, reused bool) {
	r := s.Reuse
	tree, why := treeOf(ctx, b, e, s)
	switch {
	case r.Force != "" && b.Params[r.Force] == "true":
		d.note(b, s.Name, e, fmt.Sprintf("%s: full run forced by %s", s.Name, r.Force))
		return tree, false
	case why != "":
		d.note(b, s.Name, e, fmt.Sprintf("%s: %s; running it", s.Name, why))
		return "", false
	case e.fingerprintSum == "":
		d.note(b, s.Name, e, s.Name+": the job takes no fingerprint, so no green run can match; running it")
		return tree, false
	}
	from, err := d.st.GreenStage(ctx, store.GreenQuery{Tree: tree, FingerprintSum: e.fingerprintSum, Class: s.RunnerClass(),
		From: r.From, Stage: r.Stage, Job: b.Job, JobStage: s.Name, Build: b.ID})
	if err != nil {
		d.log.Error("looking for a green run", "build", b.ID, "stage", s.Name, "err", err)
		d.note(b, s.Name, e, fmt.Sprintf("%s: could not look for a green run: %v; running it", s.Name, err))
		return tree, false
	}
	if from == nil {
		d.note(b, s.Name, e, fmt.Sprintf("%s: no green run of tree %s; running it", s.Name, shortTree(tree)))
		return tree, false
	}
	d.note(b, s.Name, e, fmt.Sprintf("%s skipped: tree %s green in %s #%d", s.Name, shortTree(tree), from.Job, from.Number))
	if err := d.st.ReuseStage(context.WithoutCancel(ctx), b.ID, s.Name, s.RunnerClass(), *from, time.Now()); err != nil {
		d.log.Error("recording a reused stage", "build", b.ID, "stage", s.Name, "err", err)
		return tree, false
	}
	d.changed()
	return "", true
}

// treeOf is the git tree of the work tree the stage gates, or why there is
// none to match: no repo there, or one with changes.
func treeOf(ctx context.Context, b *store.Build, e *buildEnv, s *config.Stage) (tree, why string) {
	dir := e.workspace // the checkout kipp made
	if s.Reuse.Tree != "" {
		dir = expandTree(s.Reuse.Tree, slices.Concat(e.env, e.outs.upstream(e.stages, s), stageEnv(s, b.Params)))
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(e.workdir, s.Workdir, dir)
		}
	}
	tree, err := gitTree(ctx, dir)
	if err != nil {
		return "", "no git work tree at " + dir
	}
	changed, err := gitChanges(ctx, dir, filepath.Join(e.workspace, filepath.Dir(runnerproto.EnvFile(s.Name))))
	switch {
	case err != nil:
		return "", fmt.Sprintf("could not tell whether the work tree at %s has changes: %v", dir, err)
	case changed:
		return "", "the work tree at " + dir + " has changes"
	}
	return tree, ""
}

// expandTree expands $VARS in a tree's path with env, NAME=value pairs in
// which the last of a name wins.
func expandTree(path string, env []string) string {
	vars := make(map[string]string, len(env))
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			vars[k] = v
		}
	}
	return os.Expand(path, func(k string) string { return vars[k] })
}

// shortTree is a tree as kipp's lines name it: its first 12 hex digits.
func shortTree(tree string) string {
	if len(tree) > 12 {
		return tree[:12]
	}
	return tree
}

// gitTree is the tree HEAD names in the git work tree dir.
func gitTree(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--verify", "HEAD^{tree}")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD^{tree} in %s: %w", dir, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitChanges reports whether the work tree dir differs from HEAD: a
// changed, staged or untracked file. kipp's own directory in the
// workspace, skip, is not the repo's and is left out.
func gitChanges(ctx context.Context, dir, skip string) (bool, error) {
	args := []string{"-C", dir, "status", "--porcelain"}
	if rel, err := filepath.Rel(dir, skip); err == nil && filepath.IsLocal(rel) {
		args = append(args, "--", ":(exclude)"+filepath.ToSlash(rel))
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("git status: %w", err)
	}
	return len(bytes.TrimSpace(out)) > 0, nil
}
