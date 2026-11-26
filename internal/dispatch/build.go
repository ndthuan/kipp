package dispatch

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ndthuan/kipp/internal/cache"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/logtime"
	"github.com/ndthuan/kipp/internal/mask"
	"github.com/ndthuan/kipp/internal/pipeline"
	"github.com/ndthuan/kipp/internal/runner"
	"github.com/ndthuan/kipp/internal/runnerhub"
	"github.com/ndthuan/kipp/internal/store"
)

const statusAttempts = 5

var statusBackoff = 200 * time.Millisecond

type buildEnv struct {
	workspace            string
	workdir              string
	env                  []string
	serverEnv            []string
	vars                 []config.Var
	varsErr              error
	fileEnv              map[string]string
	remoteEnv            []string
	secrets              []string
	files                map[string][]byte
	stages               []*config.Stage
	dag                  *pipeline.DAG
	store                string
	reports              *reports
	cacheNS              cache.Namespace
	outs                 *stageOutputs
	secretNames          []string
	previous, previousOK string
	token                string
	fingerprintSum       string
}

func (d *Dispatcher) runBuild(ctx context.Context, r *run) {
	b, job := r.build, r.job
	defer d.release(b.ID)
	bg := context.WithoutCancel(ctx)
	if job.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, job.Timeout, timedOut(job.Timeout))
		defer cancel()
	}
	e := d.newEnv(b, job)
	status, ranStages := store.Failed, true
	err := d.prepare(ctx, b, job, e)
	d.prepared(b.ID)
	if err != nil {
		ranStages = false
		d.note(b, config.PrepareLog, e, err.Error())
		e.stages = nil
		if job.Source != nil || job.Multibranch != nil {
			e.workdir = e.workspace
		}
	} else {
		status = d.runStages(ctx, b, e)
	}
	status = d.settle(ctx, r, status)
	d.writeFinalStatus(bg, b, status)
	d.sayFinal(ctx, b, job, e, status, ranStages)
	d.finish(bg, b, job, e, status)
}

func (d *Dispatcher) writeFinalStatus(ctx context.Context, b *store.Build, status store.Status) {
	ended := time.Now()
	err := retry(statusAttempts, statusBackoff, func() error {
		err := d.st.SetBuildStatus(ctx, b.ID, status, ended)
		if err != nil {
			d.log.Warn("writing the build's final status", "job", b.Job, "number", b.Number, "status", status, "err", err)
		}
		return err
	})
	if err != nil {
		d.log.Error("the build ended but its row is stale: it reads running until the next restart",
			"job", b.Job, "number", b.Number, "status", status, "err", err)
	}
}

func retry(attempts int, backoff time.Duration, do func() error) error {
	var err error
	for i := range attempts {
		if err = do(); err == nil {
			return nil
		}
		if i < attempts-1 {
			time.Sleep(time.Duration(i+1) * backoff)
		}
	}
	return err
}

func (d *Dispatcher) settle(ctx context.Context, r *run, status store.Status) store.Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	r.settled = true
	if ctx.Err() == nil {
		return status
	}
	status, _ = stopped(ctx)
	return status
}

func stopped(ctx context.Context) (store.Status, string) {
	cause := context.Cause(ctx)
	if _, ok := errors.AsType[timedOut](cause); ok {
		return store.Timeout, "kipp: " + cause.Error()
	}
	_, byUser := errors.AsType[cancelledBy](cause)
	_, gone := errors.AsType[childGone](cause)
	_, restart := errors.AsType[stoppedForRestart](cause)
	_, replaced := errors.AsType[supersededBy](cause)
	if byUser || gone || restart || replaced {
		return store.Cancelled, "kipp: " + cause.Error()
	}
	return store.Lost, lostLine
}

func (d *Dispatcher) newEnv(b *store.Build, job *config.Job) *buildEnv {
	ws := filepath.Join(d.opts.DataDir, "workspace", job.Name, strconv.Itoa(b.Number))
	e := &buildEnv{workspace: ws, workdir: job.Workdir, stages: job.Stages, store: job.StoreName(), reports: &reports{}, outs: newStageOutputs()}
	if job.Workdir == "" || job.Source != nil || job.Multibranch != nil {
		e.workdir = filepath.Join(ws, job.Workdir)
	}
	e.cacheNS = cache.NamespaceOf(b.Pushed(), b.TriggerBranch, b.TriggerDefaultBranch, job.Name, job.CacheWritesDefault)
	e.vars, e.varsErr = d.Snapshot().Vars.For(job)
	e.token = d.mintToken(b, job)
	e.env, e.remoteEnv, e.secrets = d.environment(b, job, e, nil)
	e.serverEnv = e.env
	return e
}

func (d *Dispatcher) prepare(ctx context.Context, b *store.Build, job *config.Job, e *buildEnv) error {
	if err := os.MkdirAll(e.workspace, 0o750); err != nil {
		return err
	}
	if err := d.clearLogs(b); err != nil {
		return err
	}
	var err error
	if e.previous, e.previousOK, err = d.st.PreviousCommits(ctx, b.Job, b.ID); err != nil {
		return fmt.Errorf("the job's earlier commits: %w", err)
	}
	secrets, err := d.opts.Secrets(ctx, job.Secrets)
	if err != nil {
		return fmt.Errorf("secrets: %w", err)
	}
	if err := checkScopes(job, secrets); err != nil {
		return err
	}
	if err := checkDerivedNames(b, job, secrets); err != nil {
		return err
	}
	if err := checkVars(job, e, secrets); err != nil {
		return err
	}
	d.useSecrets(b, job, e, secrets)
	if err := d.takeFingerprint(ctx, b, job, e); err != nil {
		return err
	}
	if err := d.repoStages(ctx, b, job, e, secrets); err != nil {
		return err
	}
	needs := make(map[string][]string, len(e.stages))
	on := map[string]string{}
	names := make([]string, len(e.stages))
	for i, s := range e.stages {
		names[i], needs[s.Name] = s.Name, s.Needs
		if s.When != nil && s.When.NeedsStatus != "" {
			on[s.Name] = s.When.NeedsStatus
		}
	}
	if e.dag, err = pipeline.New(needs, on); err != nil {
		return err
	}
	return d.st.PutStages(ctx, b.ID, names, needs)
}

func (d *Dispatcher) repoStages(ctx context.Context, b *store.Build, job *config.Job, e *buildEnv, secrets map[string]Secret) error {
	var err error
	var file *config.StageFile
	path := ""
	switch {
	case job.Source != nil:
		path = job.Source.Path
		file, err = d.cloneStages(ctx, b, job.Source, e)
	case job.Multibranch != nil:
		path = job.Multibranch.Path
		file, err = d.cloneChild(ctx, b, job, e, secrets)
	default:
		return nil
	}
	if err != nil {
		return err
	}
	e.stages, e.fileEnv = file.Stages, file.Env
	if err := d.grantRepoPermissions(b, job, e, path, file.Permissions); err != nil {
		return err
	}
	if err := checkFile(job, e, secrets, path); err != nil {
		return err
	}
	if err := errors.Join(job.CheckWhenParams(e.stages), job.CheckCaches(e.stages, e.declared()), job.CheckReuse(e.stages, false), job.CheckStageLocks(e.stages)); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	d.useSecrets(b, job, e, secrets)
	return nil
}

func (d *Dispatcher) clearLogs(b *store.Build) error {
	root, err := os.OpenRoot(d.LogsDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return root.RemoveAll(filepath.Join(b.Job, strconv.Itoa(b.Number)))
}

func (d *Dispatcher) cloneStages(ctx context.Context, b *store.Build, src *config.Source, e *buildEnv) (*config.StageFile, error) {
	ref := src.Ref
	if commit := b.Commit(); commit != "" {
		ref = commit
	}
	masked := *e
	masked.secrets = append(slices.Clip(e.secrets), config.URLCredentials(src.Repo)...)
	var built string
	err := d.logged(b, config.CloneLog, &masked, func(out io.Writer) error {
		if err := d.opts.Clone(ctx, e.workspace, src.Repo, ref, out); err != nil {
			return err
		}
		var herr error
		if built, herr = d.opts.Head(ctx, e.workspace); herr != nil {
			built = ""
			if ref == b.Commit() {
				built = ref
			} else {
				_, _ = fmt.Fprintf(out, "kipp: could not tell which commit the clone checked out: %v\n", herr)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("clone %s@%s: %w", config.RedactURL(src.Repo), ref, redacted(err, src.Repo))
	}
	if built != "" {
		d.setCommits(ctx, b, built, "")
	}
	d.recordTree(ctx, b, e.workspace)
	root, err := os.OpenRoot(e.workspace)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(src.Path)
	if err != nil {
		return nil, err
	}
	file, err := config.ParseStageFile(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", src.Path, err)
	}
	return file, nil
}

func (d *Dispatcher) environment(b *store.Build, job *config.Job, e *buildEnv, secrets map[string]Secret) (local, remote, values []string) {
	pass := d.opts.PassEnv
	if pass == nil {
		pass = DefaultPassEnv
	}
	vars := d.buildVars(b, job, e, secrets)
	local = slices.Concat(PassEnv(os.Environ(), pass), vars, []string{"KIPP_WORKSPACE=" + e.workspace, "KIPP_OS=" + runtime.GOOS, "KIPP_ARCH=" + runtime.GOARCH})
	remote = slices.Concat(RunnerEnv(os.Environ(), d.opts.RunnerEnv), vars)
	masks := secretMasks(secrets)
	if e.token != "" {
		masks = append(masks, e.token)
	}
	return local, remote, masks
}

func (d *Dispatcher) useSecrets(b *store.Build, job *config.Job, e *buildEnv, secrets map[string]Secret) {
	e.env, e.remoteEnv, e.secrets = d.environment(b, job, e, secrets)
	server := *e
	server.fileEnv = nil
	e.serverEnv, _, _ = d.environment(b, job, &server, secrets)
	e.files = secretFileData(secrets)
	e.secretNames = nil
	for name, sec := range secrets {
		e.secretNames = append(e.secretNames, name)
		if sec.Kind == config.SecretUserPass {
			e.secretNames = append(e.secretNames, config.DerivedSecretNames(name)...)
		}
	}
}

func (d *Dispatcher) buildVars(b *store.Build, job *config.Job, e *buildEnv, secrets map[string]Secret) []string {
	var env []string
	add := func(k, v string) { env = append(env, k+"="+v) }
	for _, v := range e.vars {
		add(v.Name, v.Value)
	}
	for _, vars := range []map[string]string{job.Env, e.fileEnv, b.Params} {
		for _, k := range slices.Sorted(maps.Keys(vars)) {
			add(k, vars[k])
		}
	}
	env = append(env, secretVars(secrets)...)
	add("KIPP_JOB", job.Name)
	add("KIPP_BUILD", strconv.Itoa(b.Number))
	add("KIPP_TRIGGER", b.Trigger)
	add("KIPP_URL", d.opts.BaseURL)
	add("CI", "true")
	add("KIPP_BUILD_ID", strconv.FormatInt(b.ID, 10))
	add("KIPP_BUILD_URL", d.buildURL(job.Name, b.Number))
	add("KIPP_JOB_URL", d.jobURL(job.Name))
	add("KIPP_STARTED_BY", startedBy(b))
	add("KIPP_REPO", repoOf(job))
	add("KIPP_REF", refOf(b))
	add("KIPP_PREVIOUS_COMMIT", e.previous)
	add("KIPP_PREVIOUS_SUCCESSFUL_COMMIT", e.previousOK)
	add("KIPP_BRANCH", b.TriggerBranch)
	add("KIPP_TAG", b.TriggerTag)
	add("KIPP_DEFAULT_BRANCH", b.TriggerDefaultBranch)
	if b.Parent != "" {
		env = append(env, childVars(b, job.Origin)...)
	}
	if b.Parent == "" {
		add("KIPP_COMMIT", cmp.Or(b.Commit(), b.BuiltCommit))
	}
	if b.Trigger == "after" {
		add("KIPP_UPSTREAM_BUILD", b.TriggerDetail)
		if b.UpstreamStatus != "" {
			add("KIPP_UPSTREAM_STATUS", string(b.UpstreamStatus))
		}
		add("KIPP_UPSTREAM_COMMIT", b.UpstreamCommit)
		add("KIPP_UPSTREAM_TAG", b.UpstreamTag)
	}
	if e.token != "" {
		add("KIPP_TOKEN", e.token)
	}
	return env
}

type stageResult struct {
	name string
	how  pipeline.Outcome
}

func outcome(status store.StageStatus) pipeline.Outcome {
	switch status {
	case store.StageSuccess:
		return pipeline.Succeeded
	case store.StageUnstable:
		return pipeline.Unstable
	}
	return pipeline.Failed
}

func pushOf(b *store.Build) config.Push {
	return config.Push{Trigger: b.Trigger, Pushed: b.Pushed(), Branch: b.TriggerBranch, DefaultBranch: b.TriggerDefaultBranch, Tag: b.TriggerTag, Base: b.BaseBranch, Params: b.Params, Child: b.Parent != "", PR: b.PR}
}

func (d *Dispatcher) runStages(ctx context.Context, b *store.Build, e *buildEnv) store.Status {
	dag := e.dag
	byName := make(map[string]*config.Stage, len(e.stages))
	for _, s := range e.stages {
		byName[s.Name] = s
	}
	push := pushOf(b)
	for _, s := range e.stages {
		if !s.When.Holds(push) {
			for _, skipped := range dag.Skip(s.Name) {
				d.setStage(ctx, b, skipped, store.StageSkipped, nil)
			}
		}
	}
	results := make(chan stageResult)
	for inFlight := 0; ; inFlight-- {
		for _, name := range dag.Ready() {
			dag.Start(name)
			inFlight++
			go func(s *config.Stage) { results <- stageResult{s.Name, d.runStage(ctx, b, e, s)} }(byName[name])
		}
		if inFlight == 0 {
			break
		}
		res := <-results
		for _, skipped := range dag.Finish(res.name, res.how) {
			d.setStage(ctx, b, skipped, store.StageSkipped, nil)
		}
	}
	switch {
	case dag.Failed():
		return store.Failed
	case dag.Unstable():
		return store.Unstable
	}
	return store.Success
}

func (d *Dispatcher) runStage(ctx context.Context, b *store.Build, e *buildEnv, s *config.Stage) pipeline.Outcome {
	tree := ""
	if s.Reuse != nil && ctx.Err() == nil {
		var reused bool
		if tree, reused = d.reuse(ctx, b, e, s); reused {
			return pipeline.Succeeded
		}
	}
	unlock, why := d.stageLocks(ctx, b, e, s)
	if why != "" {
		d.setStage(ctx, b, s.Name, why, nil)
		return outcome(why)
	}
	defer unlock()
	free, ok := func() {}, ctx.Err() == nil
	if !d.remote(s) {
		free, ok = d.executor(ctx)
	}
	if !ok {
		d.setStage(ctx, b, s.Name, store.StageCancelled, nil)
		return outcome(store.StageCancelled)
	}
	defer free()
	d.setStage(ctx, b, s.Name, store.StageRunning, nil)
	status, code := d.execStage(ctx, b, e, s)
	if status != store.StageSuccess {
		tree = ""
	}
	if err := d.st.SetStageRun(context.WithoutCancel(ctx), b.ID, s.Name, s.RunnerClass(), tree); err != nil {
		d.log.Warn("recording the class and tree of a stage", "build", b.ID, "stage", s.Name, "err", err)
	}
	d.setStage(ctx, b, s.Name, status, code)
	return outcome(status)
}

func (d *Dispatcher) executor(ctx context.Context) (func(), bool) {
	if !d.local.take(ctx) {
		return nil, false
	}
	free := func() { d.local.give(); d.kick() }
	if ctx.Err() != nil {
		free()
		return nil, false
	}
	return free, true
}

func (d *Dispatcher) execStage(ctx context.Context, b *store.Build, e *buildEnv, s *config.Stage) (store.StageStatus, *int) {
	status, exit := store.StageFailed, (*int)(nil)
	var took time.Duration
	err := d.logged(b, s.Name, e, func(out io.Writer) error {
		var sc localCache
		start, done, err := d.stageStarter(ctx, b, e, s, out, &sc)
		defer done()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			_, err = fmt.Fprintf(out, "kipp: %v\n", err)
			return err
		}
		r, err := d.runTries(ctx, b, s, start, out)
		if r.ran {
			exit, took = &r.code, r.took
		}
		if !r.exited {
			return err
		}
		status = exited(s, r.code)
		if status == store.StageSuccess || status == store.StageUnstable {
			sc.save()
		}
		return nil
	})
	if err != nil {
		d.log.Error("writing a stage log", "build", b.ID, "stage", s.Name, "err", err)
		status = store.StageFailed
	}
	if ctx.Err() != nil {
		status = store.StageCancelled
		_, why := stopped(ctx)
		d.note(b, s.Name, e, why)
	}
	if exit != nil {
		reported := d.report(b, e, s, d.reportDir(e, s))
		d.keep(ctx, b, e, s.Name, measured(s, took, reported, e.secrets))
		status = d.guard(ctx, b, e, s, reported, status)
		status = d.testsFailed(b, e, s, status)
		status = d.keepArtifacts(ctx, b, e, s, status)
	}
	d.collectEnv(b, e, s, status)
	return status, exit
}

type tried struct {
	code   int
	took   time.Duration
	tries  int
	ran    bool
	exited bool
}

func (d *Dispatcher) runTries(ctx context.Context, b *store.Build, s *config.Stage, start func(attempt int) (runner.Proc, error), out io.Writer) (tried, error) {
	retries := int(s.Retries)
	if d.remote(s) {
		retries = 0
	}
	var r tried
	for r.tries < retries+1 {
		if r.tries > 0 {
			if _, err := fmt.Fprintf(out, "kipp: try %d of %d\n", r.tries+1, retries+1); err != nil {
				return r, err
			}
		}
		code, err := d.tryOnce(ctx, b, s, start, out, &r)
		if !r.exited || err != nil || ctx.Err() != nil || exited(s, code) != store.StageFailed {
			return r, err
		}
	}
	return r, nil
}

func (d *Dispatcher) tryOnce(ctx context.Context, b *store.Build, s *config.Stage, start func(attempt int) (runner.Proc, error), out io.Writer, r *tried) (int, error) {
	proc, err := start(r.tries + 1)
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil
		}
		_, err = fmt.Fprintf(out, "kipp: %v\n", err)
		return 0, err
	}
	r.tries++
	started := time.Now()
	if g := proc.Group(); g.Pgid > 0 {
		d.recordGroup(ctx, b, s.Name, g)
	}
	proc, code, err := waitTaken(ctx, proc, func() (runner.Proc, error) { return start(r.tries) }, out)
	r.code, r.ran = code, true
	r.took += time.Since(started)
	if t, ok := proc.(interface{ Tries() int }); ok {
		r.tries = t.Tries()
	}
	d.recordTries(ctx, b, s.Name, r.tries)
	if err != nil {
		_, err = fmt.Fprintf(out, "kipp: %v\n", err)
		return code, err
	}
	r.exited = true
	return code, nil
}

func waitTaken(ctx context.Context, proc runner.Proc, again func() (runner.Proc, error), out io.Writer) (runner.Proc, int, error) {
	code, err := proc.Wait()
	for errors.Is(err, runnerhub.ErrNeverTaken) && ctx.Err() == nil {
		_, _ = fmt.Fprintf(out, "kipp: %v; it is placed again\n", err)
		next, startErr := again()
		if ctx.Err() != nil {
			return proc, -1, nil
		}
		if startErr != nil {
			return proc, -1, startErr
		}
		proc = next
		code, err = proc.Wait()
	}
	return proc, code, err
}

func (d *Dispatcher) recordTries(ctx context.Context, b *store.Build, name string, tries int) {
	if tries < 2 {
		return
	}
	if err := d.st.SetStageTries(context.WithoutCancel(ctx), b.ID, name, tries); err != nil {
		d.log.Warn("recording how many times a stage ran", "build", b.ID, "stage", name, "err", err)
	}
}

func (d *Dispatcher) reportDir(e *buildEnv, s *config.Stage) string {
	if d.remote(s) {
		return junitDir(e, s)
	}
	return filepath.Join(e.workdir, s.Workdir)
}

func (d *Dispatcher) stageStarter(ctx context.Context, b *store.Build, e *buildEnv, s *config.Stage, out io.Writer, sc *localCache) (start func(attempt int) (runner.Proc, error), done func(), err error) {
	if d.remote(s) {
		return func(int) (runner.Proc, error) { return d.startRemote(ctx, b, e, s, out) }, func() {}, nil
	}
	dir, err := stageDir(e.workdir, s.Workdir)
	if err != nil {
		return nil, func() {}, fmt.Errorf("could not make the stage's workdir %s: %w", s.Workdir, err)
	}
	env, err := d.stageVars(ctx, e, s, out)
	if err != nil {
		return nil, func() {}, fmt.Errorf("could not write the stage's KIPP_METRICS or KIPP_ENV: %w", err)
	}
	env = slices.Concat(e.outs.upstream(e.stages, s), stageEnv(s, b.Params), env)
	cv, err := d.cacheVars(b, e, s, d.opts.CacheURL)
	if err != nil {
		return nil, func() {}, err
	}
	env = append(env, cv...)
	if len(s.Cache) > 0 {
		sc.begin(declaredCaches(b, s, slices.Concat(e.env, stageEnv(s, b.Params)), out), cacheEnv(slices.Concat(e.env, env), dir, out))
	}
	if err := d.deliverInputs(ctx, b, e, s); err != nil {
		return nil, func() {}, fmt.Errorf("could not put the artifacts the stage is given: %w", err)
	}
	files, done, err := d.commandFiles(e)
	if err != nil {
		return nil, func() {}, err
	}
	env = append(env, files...)
	return func(attempt int) (runner.Proc, error) {
		if _, err := resetEnvFile(e.workspace, s.Name); err != nil {
			return nil, fmt.Errorf("could not empty the stage's KIPP_ENV: %w", err)
		}
		proc, err := d.r.Start(ctx, runner.Spec{Dir: dir, Command: s.Run, Env: slices.Concat(e.env, env, []string{"KIPP_ATTEMPT=" + strconv.Itoa(attempt)}), Output: out})
		if err != nil {
			return nil, fmt.Errorf("could not start the stage: %w", err)
		}
		if err := d.st.SetStageRanOn(context.WithoutCancel(ctx), b.ID, s.Name, "local"); err != nil {
			d.log.Warn("recording where a stage runs", "build", b.ID, "stage", s.Name, "err", err)
		}
		return proc, nil
	}, done, nil
}

func (d *Dispatcher) stageVars(ctx context.Context, e *buildEnv, s *config.Stage, out io.Writer) ([]string, error) {
	env := []string{"KIPP_STAGE=" + s.Name}
	if d.bin != "" {
		env = append(env, "KIPP_BIN="+d.bin)
	}
	v, err := resetEnvFile(e.workspace, s.Name)
	if err != nil {
		return nil, err
	}
	env = append(env, v)
	if data := d.baseline(ctx, e, s, out); data != nil {
		v, err := writeBaseline(e.workspace, s.Name, data)
		if err != nil {
			return nil, err
		}
		env = append(env, v)
	}
	return env, nil
}

func stageDir(base, sub string) (string, error) {
	if sub == "" {
		return base, nil
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	if err := root.MkdirAll(sub, 0o750); err != nil {
		return "", err
	}
	return filepath.Join(base, sub), nil
}

func exited(s *config.Stage, code int) store.StageStatus {
	switch {
	case code == 0:
		return store.StageSuccess
	case s.Unstable(code):
		return store.StageUnstable
	}
	return store.StageFailed
}

func (d *Dispatcher) recordGroup(ctx context.Context, b *store.Build, name string, g runner.Group) {
	if err := d.st.SetStageGroup(context.WithoutCancel(ctx), b.ID, name, g.Pgid, g.Born); err != nil {
		d.log.Warn("recording a stage's process group", "build", b.ID, "stage", name, "pgid", g.Pgid, "err", err)
	}
}

func (d *Dispatcher) setStage(ctx context.Context, b *store.Build, name string, status store.StageStatus, exit *int) {
	if err := d.st.SetStage(context.WithoutCancel(ctx), b.ID, name, status, exit, time.Now()); err != nil {
		d.log.Error("recording a stage", "build", b.ID, "stage", name, "status", status, "err", err)
	}
	d.changed()
}

func (d *Dispatcher) logged(b *store.Build, name string, e *buildEnv, write func(out io.Writer) error) (err error) {
	p := d.LogPath(b.Job, b.Number, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	f, err := OpenLog(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	times, err := OpenLog(p+logtime.Suffix, os.O_CREATE|os.O_WRONLY|os.O_APPEND)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, times.Close()) }()
	stamped := logtime.NewWriter(f, info.Size(), times)
	out := mask.NewWriter(stamped, e.secrets)
	defer func() { err = errors.Join(err, out.Flush(), stamped.Close()) }()
	return write(out)
}

// OpenLog opens a build's log through its directory as a root, so a link
// in there cannot lead a read or a write out of it. A log it creates is
// for the server's user alone: masking keeps out the secrets kipp knows
// of, not whatever else a command prints.
func OpenLog(path string, flag int) (*os.File, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return root.OpenFile(filepath.Base(path), flag, 0o600)
}

func (d *Dispatcher) note(b *store.Build, name string, e *buildEnv, line string) {
	line = strings.TrimRight(line, "\n") + "\n"
	if midLine(d.LogPath(b.Job, b.Number, name)) {
		line = "\n" + line
	}
	err := d.logged(b, name, e, func(out io.Writer) error {
		_, err := io.WriteString(out, line)
		return err
	})
	if err != nil {
		d.log.Error("writing a build log", "job", b.Job, "number", b.Number, "log", name, "err", err)
	}
}

func midLine(path string) bool {
	f, err := OpenLog(path, os.O_RDONLY)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return false
	}
	last := make([]byte, 1)
	_, err = f.ReadAt(last, info.Size()-1)
	return err == nil && last[0] != '\n'
}

const cloneWaitDelay = 2 * time.Second

func gitClone(ctx context.Context, dir, repo, ref string, out io.Writer) error {
	if strings.HasPrefix(repo, "-") || strings.HasPrefix(ref, "-") {
		return fmt.Errorf("refusing repo %q at ref %q: neither may start with a dash", config.RedactURL(repo), ref)
	}
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"fetch", "--depth", "1", cloneURL(repo), ref},
		{"checkout", "--quiet", "--detach", "FETCH_HEAD"},
	} {
		if err := runGit(ctx, dir, out, args...); err != nil {
			return err
		}
	}
	return nil
}

func cloneURL(repo string) string {
	if hasScheme(repo) {
		return repo
	}
	return "https://github.com/" + repo + ".git"
}

func runGit(ctx context.Context, dir string, out io.Writer, args ...string) error {
	return runGitTo(ctx, dir, out, out, args...)
}

func runGitTo(ctx context.Context, dir string, stdout, stderr io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.Env = append(gitEnv(os.Environ()), "GIT_TERMINAL_PROMPT=0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = cloneWaitDelay
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w", args[0], err)
	}
	return nil
}

func gitEnv(env []string) []string {
	return slices.DeleteFunc(env, func(kv string) bool {
		return strings.HasPrefix(kv, "GIT_AUTHOR_") || strings.HasPrefix(kv, "GIT_COMMITTER_")
	})
}

// ErrNoCleanMerge is a pull request's head that does not merge into its
// base without a conflict.
var ErrNoCleanMerge = errors.New("does not merge cleanly")

func gitMerge(ctx context.Context, dir, repo, base, baseCommit, head string, out io.Writer) (string, string, error) {
	if strings.HasPrefix(repo, "-") || strings.HasPrefix(base, "-") || strings.HasPrefix(head, "-") || strings.HasPrefix(baseCommit, "-") {
		return "", "", fmt.Errorf("refusing repo %q, base %q and head %q: none may start with a dash", config.RedactURL(repo), base, head)
	}
	baseRef := "refs/heads/" + base
	if baseCommit != "" {
		baseRef = baseCommit
	}
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"remote", "add", "origin", cloneURL(repo)},
		{"fetch", "--quiet", "--no-tags", "--filter=blob:none", "origin", "+" + baseRef + ":refs/kipp/base", "+" + head + ":refs/kipp/head"},
		{"checkout", "--quiet", "--detach", "refs/kipp/base"},
	} {
		if err := runGit(ctx, dir, out, args...); err != nil {
			return "", "", err
		}
	}
	baseSHA, err := gitRev(ctx, dir, "refs/kipp/base")
	if err != nil {
		return "", "", err
	}
	headSHA, err := gitRev(ctx, dir, "refs/kipp/head")
	if err != nil {
		return "", "", err
	}
	return baseSHA, headSHA, mergeHead(ctx, dir, out)
}

func mergeHead(ctx context.Context, dir string, out io.Writer) error {
	for attempt := 0; ; attempt++ {
		var said bytes.Buffer
		err := mergeOnce(ctx, dir, io.MultiWriter(out, &said))
		var exit *exec.ExitError
		if ctx.Err() != nil || !errors.As(err, &exit) || attempt > 0 || exit.ExitCode() != 128 ||
			!strings.Contains(said.String(), promisorFetchDied) {
			return err
		}
		_, _ = fmt.Fprintln(out, "kipp: git died fetching an object; merging again")
	}
}

func mergeOnce(ctx context.Context, dir string, out io.Writer) error {
	var tree bytes.Buffer
	err := runGitTo(ctx, dir, &tree, out, "merge-tree", "--write-tree", "refs/kipp/base", "refs/kipp/head")
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		_, _ = out.Write(tree.Bytes())
		return ErrNoCleanMerge
	}
	if err != nil {
		return err
	}
	var commit bytes.Buffer
	if err := runGitTo(ctx, dir, &commit, out, "-c", "user.name=kipp", "-c", "user.email=kipp@localhost",
		"-c", "commit.gpgsign=false", "commit-tree", strings.TrimSpace(tree.String()),
		"-p", "refs/kipp/base", "-p", "refs/kipp/head", "-m", "Merge the pull request's head into its base"); err != nil {
		return err
	}
	return runGit(ctx, dir, out, "-c", "core.hooksPath=/dev/null", "checkout", "--quiet", "--detach", strings.TrimSpace(commit.String()))
}

const promisorFetchDied = "from promisor remote"

func redacted(err error, repo string) error {
	if clean := config.RedactURL(repo); clean != repo && strings.Contains(err.Error(), repo) {
		return &redactedError{msg: strings.ReplaceAll(err.Error(), repo, clean), err: err}
	}
	return err
}

type redactedError struct {
	msg string
	err error
}

func (r *redactedError) Error() string { return r.msg }
func (r *redactedError) Unwrap() error { return r.err }

func hasScheme(repo string) bool {
	return slices.ContainsFunc([]string{"https://", "http://", "ssh://", "git@", "file://", "/"}, func(p string) bool {
		return strings.HasPrefix(repo, p)
	})
}

func gitHead(ctx context.Context, dir string) (string, error) { return gitRev(ctx, dir, "HEAD") }

func gitRev(ctx context.Context, dir, rev string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--verify", rev+"^{commit}")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse %s: %w", rev, err)
	}
	return strings.TrimSpace(string(out)), nil
}
