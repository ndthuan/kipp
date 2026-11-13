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
	"sync"
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
	// Candidates are the engines as places for a stage of build in image.
	Candidates(build int64, image string) []placement.Candidate
	// Acquire is a container of the build and image on the engine, with
	// its runner connected to the hub: the one the build has, or a new one,
	// saying what it does (the image's pull) to out. It returns the runner's
	// name; an error that is runnerhub.ErrNoRunner means the engine filled up
	// meanwhile, and the stage waits again.
	Acquire(ctx context.Context, b *store.Build, engine, image string, out io.Writer) (string, error)
	// EndBuild removes the build's containers.
	EndBuild(ctx context.Context, build int64)
}

// placeEvery is how often a stage that waits for a place looks again.
var placeEvery = time.Second

// errNoRunsOn is a stage that has nowhere to run: the server runs none
// itself and neither the stage nor the server names a place.
var errNoRunsOn = errors.New("the server runs no stages and the stage names no runs_on")

// remote reports whether the stage runs somewhere other than the server.
func (d *Dispatcher) remote(s *config.Stage) bool { return s.Remote() || d.opts.RunsNoStages }

// labelsOf is where a stage on a runner asks to run.
func (d *Dispatcher) labelsOf(s *config.Stage) []string {
	if s.Remote() {
		return s.RunsOn
	}
	return d.opts.DefaultRunsOn
}

// startRemote places the stage on a runner or an engine, waiting while
// the places that could take it are busy or cannot be reached, and starts
// it there. A stage nothing could ever take fails at once. The build holds
// a slot from the start of the stage on its place until the stage ends, and
// none while it waits: a place found free is not claimed until there is a
// slot for the stage, so that an executor or a container is never held
// for a stage that cannot start.
func (d *Dispatcher) startRemote(ctx context.Context, b *store.Build, e *buildEnv, s *config.Stage, out io.Writer) (runner.Proc, error) {
	labels := d.labelsOf(s)
	if len(labels) == 0 {
		return nil, errNoRunsOn
	}
	w := &placeWait{d: d, build: b.ID, stage: s.Name, out: out}
	defer w.end()
	want := placement.Want{Labels: labels, Image: s.Image}
	for {
		c, why, line := placement.Pick(want, d.candidates(b.ID, s.Image))
		switch why {
		case placement.WhyNone:
			return nil, errors.New(line)
		case placement.Picked:
			proc, err := d.startWithSlot(ctx, b, e, s, c, out)
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

// busyLine is what a stage waits for after it was picked a place and
// startWithSlot said err, or "" when that is not a wait.
func busyLine(c placement.Candidate, err error) string {
	switch {
	case errors.Is(err, errNoSlot):
		return waitForSlot
	case errors.Is(err, runnerhub.ErrNoRunner):
		return "waiting for " + c.Name
	}
	return ""
}

// placeWait is a stage that waits to be placed, and what it has said so far.
type placeWait struct {
	d            *Dispatcher
	build        int64
	stage        string
	out          io.Writer
	said         string
	waitsForSlot bool
}

// say records what the stage waits for. The first wait, and every change
// of what it waits for, is a line of the stage's log.
func (w *placeWait) say(line string) {
	if forSlot := line == waitForSlot; forSlot != w.waitsForSlot {
		w.d.slotWaiter(forSlot)
		w.waitsForSlot = forSlot
	}
	if line != w.said {
		_, _ = fmt.Fprintf(w.out, "kipp: %s\n", line)
		w.said = line
		w.d.setWaiting(w.build, w.stage, line)
	}
}

// end clears what the stage waited for.
func (w *placeWait) end() {
	if w.waitsForSlot {
		w.d.slotWaiter(false)
	}
	w.d.setWaiting(w.build, w.stage, "")
}

// waitForSlot is what a stage waits for when its place is free and the
// server has no slot for it.
const waitForSlot = "waiting for a free build slot"

// errNoSlot is startWithSlot finding every slot taken.
var errNoSlot = errors.New(waitForSlot)

// startWithSlot starts the stage on the place picked once the build has a
// slot, taken before an engine starts a container. The slot is the stage's
// until the process it returns has been waited for; there is no slot, and
// no process, when it fails.
func (d *Dispatcher) startWithSlot(ctx context.Context, b *store.Build, e *buildEnv, s *config.Stage, c placement.Candidate, out io.Writer) (runner.Proc, error) {
	if !d.joinSlot(ctx, b.ID, false) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errNoSlot
	}
	proc, err := d.startOn(ctx, b, e, s, c, out)
	if err != nil {
		d.leaveSlot(b.ID)
		return nil, err
	}
	return &slotProc{Proc: proc, leave: func() { d.leaveSlot(b.ID) }}, nil
}

// slotProc is a stage on its place, which gives the build's slot back when
// it has been waited for.
type slotProc struct {
	runner.Proc
	once  sync.Once
	leave func()
}

// Tries is how many times the runner ran the command, when the process says.
func (p *slotProc) Tries() int {
	if t, ok := p.Proc.(interface{ Tries() int }); ok {
		return t.Tries()
	}
	return 1
}

// Wait waits for the process, then gives the slot back, once.
func (p *slotProc) Wait() (int, error) {
	defer p.once.Do(p.leave)
	return p.Proc.Wait()
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

// startOn starts the stage on the place picked: a runner, or the runner of
// a container on an engine.
func (d *Dispatcher) startOn(ctx context.Context, b *store.Build, e *buildEnv, s *config.Stage, c placement.Candidate, out io.Writer) (runner.Proc, error) {
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
		workspace = "" // the container's workdir is the build's alone
	}
	meta := runnerhub.StageMeta{
		BuildID: b.ID, Job: b.Job, Number: b.Number, Stage: s.Name,
		Workspace: workspace, Workdir: s.Workdir, Junit: s.Junit, JunitDir: junitDir(e, s),
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
	spec := runner.Spec{Command: s.Run, Env: slices.Concat(e.remoteEnv, e.outs.upstream(e.stages, s), stageEnv(s, b.Params), []string{"KIPP_STAGE=" + s.Name}, cv), Output: out}
	proc, err := d.opts.Hub.Start(ctx, who, spec, meta)
	if err != nil {
		return nil, err
	}
	if err := d.st.SetStageRanOn(context.WithoutCancel(ctx), b.ID, s.Name, who.Name); err != nil {
		d.log.Warn("recording where a stage runs", "build", b.ID, "stage", s.Name, "err", err)
	}
	return proc, nil
}

// junitDir is where the junit and metrics files a stage on a runner left
// are put on the server, for readJunit and stageMetrics to find with the
// stage's own globs.
func junitDir(e *buildEnv, s *config.Stage) string {
	return filepath.Join(e.workspace, ".kipp-runner", s.Name)
}

// sleep waits d, and reports false when ctx ended first.
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

// setWaiting records what a stage waits for to be placed; empty is
// nothing.
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

// waitingOf is a copy of what the build's stages wait for. mu is held.
// Waiting is what each stage of the build that waits to be placed waits
// for, nil when none does.
func (d *Dispatcher) Waiting(build int64) map[string]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.waitingOfLocked(build)
}

func (d *Dispatcher) waitingOfLocked(build int64) map[string]string {
	if w := d.waiting[build]; w != nil {
		return maps.Clone(w)
	}
	return nil
}

// endRemote tells the runners and the engines that the build is over.
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
