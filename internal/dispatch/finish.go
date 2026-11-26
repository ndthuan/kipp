package dispatch

import (
	"context"
	"errors"
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

func (d *Dispatcher) finish(ctx context.Context, b *store.Build, job *config.Job, e *buildEnv, status store.Status) {
	d.endRemote(ctx, b)
	if d.opts.Cache != nil {
		d.opts.Cache.Revoke(b.ID)
	}
	if d.opts.BuildTokens != nil {
		defer d.opts.BuildTokens.Revoke(b.ID)
	}
	d.readJunit(ctx, b, e)
	d.remember(ctx, b, job, e, status)
	if status != store.Lost {
		d.runHook(ctx, b, job, e, status)
	}
	if err := os.RemoveAll(e.workspace); err != nil {
		d.log.Error("removing the workspace", "job", b.Job, "number", b.Number, "err", err)
	}
	if status.Succeeded() {
		d.keepTag(ctx, b, job)
	}
	if err := d.opts.Retain(ctx, d.st, job, d.LogsDir(), time.Now()); err != nil {
		d.log.Error("retention", "job", job.Name, "err", err)
	}
	d.trimArtifacts(ctx)
	if status.Succeeded() {
		d.triggerDownstream(ctx, b, job, status)
	}
}

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

func (d *Dispatcher) junitSource(e *buildEnv, s *config.Stage) (string, bool) {
	if d.remote(s) {
		dir := junitDir(e, s)
		return dir, exists(dir)
	}
	dir, _ := stageDir(e.workdir, s.Workdir)
	return dir, true
}

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

func caseSeconds(tests []store.Test) store.Metric {
	m := store.Metric{Name: config.CaseSeconds, Unit: "s", Values: map[string]float64{}}
	for _, t := range tests {
		if item := caseItem(t); t.Status != "skipped" && metrics.ValidItem(item) {
			m.Values[item] = float64(t.DurationMs) / 1000
		}
	}
	return m
}

func caseItem(t store.Test) string {
	if t.Suite == "" {
		return t.Name
	}
	return t.Suite + " / " + t.Name
}

func masked(s string, secrets []string) string {
	var out strings.Builder
	w := mask.NewWriter(&out, secrets)
	_, _ = io.WriteString(w, s)
	_ = w.Flush()
	return out.String()
}

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
	go func() {
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
		proc, err := d.r.Start(ctx, runner.Spec{Dir: e.workdir, Command: hook.Run, Env: e.hookEnv(slices.Concat(files, []string{"KIPP_STATUS=" + string(status), "KIPP_ATTEMPT=1"})...), Output: out})
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

func (d *Dispatcher) keepTag(ctx context.Context, b *store.Build, job *config.Job) {
	if !job.BuildsTags() {
		return
	}
	now := time.Now()
	if err := d.st.KeepBuild(ctx, b.ID, "kipp", now); err != nil {
		d.log.Error("keeping a tag's build", "job", b.Job, "number", b.Number, "err", err)
		return
	}
	line := fmt.Sprintf("#%d kept by kipp: the build of tag %s", b.Number, b.TriggerTag)
	if err := d.st.AddActivity(ctx, b.Job, line, now); err != nil {
		d.log.Error("recording a kept build", "job", b.Job, "number", b.Number, "err", err)
	}
}

func (d *Dispatcher) triggerDownstream(ctx context.Context, b *store.Build, job *config.Job, status store.Status) {
	if job.IsSideBuild(b.Params) {
		return
	}
	snap := d.Snapshot()
	up := job.Name
	if job.Parent != "" {
		if !job.OnDefaultBranch() && !job.BuildsTags() {
			return
		}
		up = job.Parent
	}
	kept := false
	for _, name := range snap.Names {
		if down := snap.Jobs[name]; down.Disabled || !slices.Contains(down.Triggers.After, up) {
			continue
		}
		_, err := d.Trigger(ctx, afterRequest(b, name, status))
		switch {
		case errors.Is(err, ErrQuiet):
			kept = d.keepAfter(ctx, b, name) || kept
		case err != nil:
			d.log.Warn("downstream trigger", "job", name, "after", up, "err", err)
		}
	}
	if kept && !d.Quiet() {
		d.TriggerKeptAfters(ctx)
	}
}

func afterRequest(b *store.Build, name string, status store.Status) TriggerRequest {
	return TriggerRequest{Job: name, Kind: "after", Detail: fmt.Sprintf("%s#%d", b.Job, b.Number), UpstreamStatus: status, UpstreamCommit: b.BuiltCommit, UpstreamTag: b.TriggerTag, By: "kipp"}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

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
