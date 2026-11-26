package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/placement"
	"github.com/ndthuan/kipp/internal/runner"
	"github.com/ndthuan/kipp/internal/runnerhub"
	"github.com/ndthuan/kipp/internal/runnerproto"
	"github.com/ndthuan/kipp/internal/store"
)

// EngineSource is the Docker engines a stage with an image is placed on.
type EngineSource interface {
	Candidates(build int64, image string) []placement.Candidate
	Acquire(ctx context.Context, b *store.Build, engine, image string, out io.Writer) (string, error)
	EndBuild(ctx context.Context, build int64)
}

var placeEvery = time.Second

var errNoRunsOn = errors.New("the server runs no stages and the stage names no runs_on")

func (d *Dispatcher) remote(s *config.Stage) bool { return s.Remote() || !d.local.runsStages() }

func (d *Dispatcher) labelsOf(s *config.Stage) []string {
	if s.Remote() {
		return s.RunsOn
	}
	return d.opts.DefaultRunsOn
}

func (d *Dispatcher) startRemote(ctx context.Context, b *store.Build, e *buildEnv, s *config.Stage, out io.Writer) (runner.Proc, error) {
	labels := d.labelsOf(s)
	if len(labels) == 0 {
		return nil, errNoRunsOn
	}
	w := &placeWait{d: d, build: b.ID, stage: s.Name, out: out}
	defer w.end()
	p := &stagePlace{own: placement.Want{Labels: labels, Image: s.Image}, fallback: s.FallbackRunsOn, out: out}
	for {
		cs := d.candidates(b.ID, s.Image)
		want, fallback := p.want(cs)
		c, why, line := placement.Pick(want, cs)
		switch why {
		case placement.WhyNone:
			return nil, errors.New(line)
		case placement.Picked:
			p.picked(c, fallback)
			proc, err := d.startOn(ctx, b, e, s, c, fallback, out)
			if line = busyLine(c, err); line == "" {
				return proc, err
			}
		}
		w.say(line)
		if !d.sleep(ctx, placeEvery) {
			return nil, ctx.Err()
		}
	}
}

type stagePlace struct {
	own      placement.Want
	fallback []string
	out      io.Writer
	told     bool
}

func (p *stagePlace) want(cs []placement.Candidate) (placement.Want, bool) {
	if len(p.fallback) == 0 || placement.Available(p.own, cs) {
		return p.own, false
	}
	return placement.Want{Labels: p.fallback, Image: p.own.Image}, true
}

func (p *stagePlace) picked(c placement.Candidate, fallback bool) {
	if !fallback || p.told {
		return
	}
	p.told = true
	_, _ = fmt.Fprintf(p.out, "kipp: no %s with labels %s is online and enabled, so the stage runs on one with labels %s (fallback_runs_on)\n",
		c.Kind, placement.Show(p.own.Labels), placement.Show(p.fallback))
}

func busyLine(c placement.Candidate, err error) string {
	if errors.Is(err, runnerhub.ErrNoRunner) {
		return "waiting for " + c.Name
	}
	return ""
}

type placeWait struct {
	d     *Dispatcher
	build int64
	stage string
	out   io.Writer
	said  string
}

func (w *placeWait) say(line string) {
	if line != w.said {
		_, _ = fmt.Fprintf(w.out, "kipp: %s\n", line)
		w.said = line
		w.d.setWaiting(w.build, w.stage, line)
	}
}

func (w *placeWait) end() {
	w.d.setWaiting(w.build, w.stage, "")
}

func (d *Dispatcher) candidates(build int64, image string) []placement.Candidate {
	var cs []placement.Candidate
	if d.opts.Hub != nil {
		cs = append(cs, d.opts.Hub.Candidates()...)
	}
	if d.opts.Engines != nil {
		cs = append(cs, d.opts.Engines.Candidates(build, image)...)
	}
	return cs
}

func (d *Dispatcher) startOn(ctx context.Context, b *store.Build, e *buildEnv, s *config.Stage, c placement.Candidate, fallback bool, out io.Writer) (runner.Proc, error) {
	if d.opts.Hub == nil {
		return nil, errors.New("this server takes no runners")
	}
	who, workspace := runnerhub.Who{Name: c.Name}, filepath.Join(b.Job, strconv.Itoa(b.Number))
	if c.Kind == placement.KindEngine {
		name, err := d.opts.Engines.Acquire(ctx, b, c.Name, s.Image, out)
		if err != nil {
			if errors.Is(err, runnerhub.ErrNoRunner) {
				return nil, err
			}
			return nil, fmt.Errorf("engine %s: %w", c.Name, err)
		}
		who = runnerhub.Who{Build: b.ID, Name: name}
		workspace = ""
	}
	meta := runnerhub.StageMeta{
		BuildID: b.ID, Job: b.Job, Number: b.Number, Stage: s.Name,
		Workspace: workspace, Dir: config.RunnerDir(e.stages, s.Name), Workdir: s.Workdir, Junit: s.Junit, JunitDir: junitDir(e, s),
		Baseline: d.baseline(ctx, e, s, out),
	}
	if s.Metrics != nil {
		meta.Metrics = s.Metrics.File
	}
	meta.Artifacts, meta.ArtifactsDir = remoteArtifacts(e, s)
	var err error
	if meta.Inputs, meta.InputDirs, err = d.inputsOf(ctx, b, s); err != nil {
		return nil, fmt.Errorf("the artifacts the stage is given: %w", err)
	}
	cv, err := d.cacheVars(b, e, s, d.opts.RunnerURL)
	if err != nil {
		return nil, err
	}
	for _, c := range declaredCaches(b, s, slices.Concat(e.remoteEnv, stageEnv(s, b.Params)), out) {
		meta.Cache = append(meta.Cache, runnerproto.Cache{Key: c.Key, RestoreKeys: c.RestoreKeys, Paths: c.Paths, Scope: c.Scope})
	}
	meta.UnstableExit, meta.Retries = s.UnstableExit, int(s.Retries)
	meta.SecretFiles = remoteSecretFiles(e)
	placed := []string{"KIPP_STAGE=" + s.Name}
	if fallback {
		placed = append(placed, "KIPP_FALLBACK=1")
	}
	spec := runner.Spec{Command: s.Run, Env: slices.Concat(e.remoteEnv, e.outs.upstream(e.stages, s), stageEnv(s, b.Params), placed, cv), Output: out}
	proc, err := d.opts.Hub.Start(ctx, who, spec, meta)
	if err != nil {
		return nil, err
	}
	if err := d.st.SetStageRanOn(context.WithoutCancel(ctx), b.ID, s.Name, who.Name); err != nil {
		d.log.Warn("recording where a stage runs", "build", b.ID, "stage", s.Name, "err", err)
	}
	return proc, nil
}

func junitDir(e *buildEnv, s *config.Stage) string {
	return filepath.Join(e.workspace, ".kipp-runner", s.Name)
}

func (d *Dispatcher) sleep(ctx context.Context, wait time.Duration) bool {
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (d *Dispatcher) setWaiting(build int64, stage, line string) {
	d.mu.Lock()
	switch w := d.waiting[build]; {
	case line != "" && w == nil:
		d.waiting[build] = map[string]string{stage: line}
	case line != "":
		w[stage] = line
	case w != nil:
		delete(w, stage)
		if len(w) == 0 {
			delete(d.waiting, build)
		}
	}
	d.mu.Unlock()
	d.changed()
}

// Waiting is what each stage of the build that waits for a lock or to be
// placed waits for, nil when none does.
func (d *Dispatcher) Waiting(build int64) map[string]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.waitingOfLocked(build)
}

func (d *Dispatcher) waitingOfLocked(build int64) map[string]string {
	out := maps.Clone(d.waiting[build])
	for stage, w := range d.lockWaits[build] {
		if out == nil {
			out = map[string]string{}
		}
		out[stage] = LockWaitLine(w.lock)
	}
	return out
}

func (d *Dispatcher) endRemote(ctx context.Context, b *store.Build) {
	if d.opts.Hub != nil {
		d.opts.Hub.EndBuild(b.ID)
	}
	if d.opts.Engines != nil {
		d.opts.Engines.EndBuild(ctx, b.ID)
	}
}

// RunnerEnv keeps of environ the variables names names, by exact name: a
// stage on another machine has its own PATH and HOME, and gets the
// server's only of what is asked for by name.
func RunnerEnv(environ, names []string) []string {
	out := []string{}
	for _, kv := range environ {
		name, _, ok := strings.Cut(kv, "=")
		if ok && slices.Contains(names, name) {
			out = append(out, kv)
		}
	}
	return out
}

// ParseRunnerEnv reads a comma-separated --runner-env: variables' names.
func ParseRunnerEnv(list string) ([]string, error) {
	out := []string{}
	for item := range strings.SplitSeq(list, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if !config.ValidEnvName(item) {
			return nil, fmt.Errorf("--runner-env %q: a variable's name; a stage on a runner gets the server's variables by name only", item)
		}
		out = append(out, item)
	}
	return out, nil
}
