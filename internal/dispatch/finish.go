package dispatch

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/junit"
	"github.com/ndthuan/kipp/internal/mask"
	"github.com/ndthuan/kipp/internal/metrics"
	"github.com/ndthuan/kipp/internal/retain"
	"github.com/ndthuan/kipp/internal/runner"
	"github.com/ndthuan/kipp/internal/store"
)

const hookTimeout = 5 * time.Minute

// finish is everything after the status is final: the JUnit reports, the
// hook, the workspace, retention, the downstream jobs. ctx is never
// cancelled here. A build the shutdown ended gets no hook, as one a crash
// ended gets none.
func (d *Dispatcher) finish(ctx context.Context, b *store.Build, job *config.Job, e *buildEnv, status store.Status) {
	d.endRemote(ctx, b)
	if d.opts.Cache != nil {
		d.opts.Cache.Revoke(b.ID)
	}
	d.readJunit(ctx, b, e)
	d.remember(ctx, b, job, e, status)
	if status != store.Lost {
		d.runHook(ctx, b, job, e, status)
	}
	// Only the build's own folder: the job's is shared with its other builds.
	if err := os.RemoveAll(e.workspace); err != nil {
		d.log.Error("removing the workspace", "job", b.Job, "number", b.Number, "err", err)
	}
	if err := d.opts.Retain(ctx, d.st, job, d.LogsDir(), time.Now()); err != nil {
		d.log.Error("retention", "job", job.Name, "err", err)
	}
	d.trimArtifacts(ctx)
	if status.Succeeded() {
		d.triggerDownstream(ctx, b, job, status)
	}
}

// readJunit stores the reports the stages left, before the workspace goes.
// A report that does not parse is a line in its stage's log.
func (d *Dispatcher) readJunit(ctx context.Context, b *store.Build, e *buildEnv) {
	var tests []store.Test
	for _, s := range e.stages {
		if s.Junit == "" {
			continue
		}
		dir, ok := d.junitSource(e, s)
		if !ok {
			continue
		}
		cases, errs := junit.ParseGlob(dir, s.Junit)
		for _, err := range errs {
			d.note(b, s.Name, e, fmt.Sprintf("kipp: junit: %v", err))
		}
		stageTests := make([]store.Test, len(cases))
		for i, c := range cases {
			stageTests[i] = store.Test{
				Stage: s.Name, Suite: masked(c.Suite, e.secrets), Name: masked(c.Name, e.secrets),
				Status: c.Status, DurationMs: c.DurationMs, Message: masked(c.Message, e.secrets),
			}
		}
		tests = append(tests, stageTests...)
		d.keep(ctx, b, e, s.Name, []store.Metric{caseSeconds(stageTests)})
	}
	if len(tests) == 0 {
		return
	}
	if err := d.st.PutTests(ctx, b.ID, tests); err != nil {
		d.log.Error("storing tests", "build", b.ID, "err", err)
	}
}

// junitSource is the directory a stage's junit glob is read from; for a
// stage on an agent only what the agent sent is there, so a stage that
// matched nothing, or never ran, has none.
func (d *Dispatcher) junitSource(e *buildEnv, s *config.Stage) (string, bool) {
	if d.remote(s) {
		dir := junitDir(e, s)
		return dir, exists(dir)
	}
	dir, _ := stageDir(e.workdir, s.Workdir)
	return dir, true
}

// testsFailed is the stage's end once its junit files are read: with
// tests_failed set, a stage that would end success or unstable ends as the
// key says when a case failed or errored. Any other end is left alone. The
// files are read again, with their errors left to readJunit, once for the
// build, to say.
func (d *Dispatcher) testsFailed(b *store.Build, e *buildEnv, s *config.Stage, status store.StageStatus) store.StageStatus {
	if s.TestsFailed == "" || (status != store.StageSuccess && status != store.StageUnstable) {
		return status
	}
	dir, ok := d.junitSource(e, s)
	if !ok {
		return status
	}
	cases, _ := junit.ParseGlob(dir, s.Junit)
	failed := 0
	for _, c := range cases {
		if c.Status == "failed" {
			failed++
		}
	}
	if failed == 0 {
		return status
	}
	noun := "cases"
	if failed == 1 {
		noun = "case"
	}
	d.note(b, s.Name, e, fmt.Sprintf("kipp: %d failed test %s, the stage ends %s (tests_failed)", failed, noun, s.TestsFailed))
	if s.TestsFailed == "failed" {
		return store.StageFailed
	}
	return store.StageUnstable
}

// caseSeconds is kipp.case_seconds of a stage's cases, masked as they
// are. A skipped case took no time worth remembering over the time it took
// when it ran.
func caseSeconds(tests []store.Test) store.Metric {
	m := store.Metric{Name: config.CaseSeconds, Unit: "s", Values: map[string]float64{}}
	for _, t := range tests {
		if item := caseItem(t); t.Status != "skipped" && metrics.ValidItem(item) {
			m.Values[item] = float64(t.DurationMs) / 1000
		}
	}
	return m
}

// caseItem is a JUnit case as an item of kipp.case_seconds.
func caseItem(t store.Test) string {
	if t.Suite == "" {
		return t.Name
	}
	return t.Suite + " / " + t.Name
}

// masked is s with every secret value replaced, as a log would hold it.
func masked(s string, secrets []string) string {
	var out strings.Builder
	w := mask.NewWriter(&out, secrets)
	_, _ = io.WriteString(w, s) // a strings.Builder does not fail
	_ = w.Flush()
	return out.String()
}

// runHook runs on_success or on_failure in the build's directory, with the
// build's environment and KIPP_STATUS. It does not change the status. An
// unstable build runs on_success, and KIPP_STATUS tells the hook it was one.
func (d *Dispatcher) runHook(ctx context.Context, b *store.Build, job *config.Job, e *buildEnv, status store.Status) {
	hook := job.OnFailure
	if status.Succeeded() {
		hook = job.OnSuccess
	}
	if hook == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, hookTimeout)
	defer cancel()
	go func() { // a hook outlives its build's cancel, not the server
		select {
		case <-d.quit:
			cancel()
		case <-ctx.Done():
		}
	}()
	err := d.logged(b, config.HooksLog, e, func(out io.Writer) error {
		files, remove, err := d.commandFiles(e)
		if err != nil {
			_, err = fmt.Fprintf(out, "kipp: could not start the hook: %v\n", err)
			return err
		}
		defer remove()
		proc, err := d.r.Start(ctx, runner.Spec{Dir: e.workdir, Command: hook.Run, Env: slices.Concat(e.env, e.outs.all(), files, []string{"KIPP_STATUS=" + string(status), "KIPP_ATTEMPT=1"}), Output: out})
		if err != nil {
			_, err = fmt.Fprintf(out, "kipp: could not start the hook: %v\n", err)
			return err
		}
		if code, err := proc.Wait(); err != nil || code != 0 {
			_, err = fmt.Fprintf(out, "kipp: the hook exited %d: %v\n", code, err)
			return err
		}
		return nil
	})
	if err != nil {
		d.log.Error("writing the hook log", "build", b.ID, "err", err)
	}
}

// triggerDownstream starts every job whose triggers.after names this one
// and that is not disabled, and tells each how this build ended.
func (d *Dispatcher) triggerDownstream(ctx context.Context, b *store.Build, job *config.Job, status store.Status) {
	snap := d.Snapshot()
	up := job.Name
	if job.Parent != "" {
		// Another job runs after a multibranch job when its default
		// branch's child succeeds, and after no other child.
		if !job.OnDefaultBranch() {
			return
		}
		up = job.Parent
	}
	for _, name := range snap.Names {
		if down := snap.Jobs[name]; down.Disabled || !slices.Contains(down.Triggers.After, up) {
			continue
		}
		req := TriggerRequest{Job: name, Kind: "after", Detail: fmt.Sprintf("%s#%d", job.Name, b.Number), UpstreamStatus: status, By: "kipp"}
		if _, err := d.Trigger(ctx, req); err != nil {
			d.log.Warn("downstream trigger", "job", name, "after", up, "err", err)
		}
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// trimArtifacts removes the oldest builds' artifacts while all of them
// are over ArtifactsMax, one build's end at a time: two would count the
// same files twice.
func (d *Dispatcher) trimArtifacts(ctx context.Context) {
	if d.opts.ArtifactsMax <= 0 {
		return
	}
	d.trimMu.Lock()
	defer d.trimMu.Unlock()
	n, err := retain.ArtifactsOver(ctx, d.st, d.ArtifactsDir(), d.opts.ArtifactsMax, time.Now())
	if err != nil {
		d.log.Error("removing artifacts over --artifacts-max", "removed_builds", n, "err", err)
	} else if n > 0 {
		d.log.Info("removed artifacts over --artifacts-max", "removed_builds", n)
	}
}
