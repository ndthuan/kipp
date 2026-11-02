package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ndthuan/kipp/internal/agentproto"
	"github.com/ndthuan/kipp/internal/cache"
	"github.com/ndthuan/kipp/internal/runner"
)

// doneWait is how long an agent keeps trying to say how a stage ended
// before it gives up on the server: the server counts the agent offline
// long before. An agent that is stopping tries for stopWait.
const (
	doneWait = 2 * time.Minute
	stopWait = 5 * time.Second
)

// runJob runs one assignment with its output streamed to the server as it
// is written, polls for a cancel while it runs and says how it ended.
func (a *agent) runJob(ctx context.Context, j *job) {
	as := j.a
	defer a.finishJob(j)
	pr, pw := io.Pipe()
	uploaded := make(chan error, 1)
	go func() {
		resp, err := a.do(ctx, http.MethodPost, stagePath(as.ID, agentproto.LogSuffix), "text/plain", pr)
		if err == nil {
			_ = resp.Body.Close()
		}
		_ = pr.CloseWithError(err) // the stage's writes are swallowed from then on
		uploaded <- err
	}()
	polled := make(chan struct{})
	stopPolling := make(chan struct{})
	go func() { defer close(polled); a.pollControl(ctx, j, stopPolling) }()

	out := &sink{w: pw}
	code, err := a.execute(ctx, j, out)
	close(stopPolling)
	_ = pw.Close()
	if upErr := <-uploaded; upErr != nil && err == nil {
		err = fmt.Errorf("the log did not reach the server: %w", upErr)
	}
	<-polled
	a.said(j)
	// However the command ended: a failed build's report is what is
	// looked for first.
	err = errors.Join(err, a.sendArtifacts(ctx, j))
	a.sayDone(ctx, j, code, err)
}

// execute makes the stage's directory and runs the command in it.
func (a *agent) execute(ctx context.Context, j *job, out io.Writer) (int, error) {
	name := a.nameOf()
	ws, dir, err := stageDirs(a.c.Workdir, j.a)
	if err != nil {
		_, _ = fmt.Fprintf(out, "kipp: could not make the stage's directory on agent %s: %v\n", name, err)
		return -1, err
	}
	if err := a.fetchInputs(ctx, j, ws); err != nil {
		_, _ = fmt.Fprintf(out, "kipp: could not put the stage's inputs on agent %s: %v\n", name, err)
		return -1, err
	}
	var extra []string
	if a.c.Bin != "" {
		extra = append(extra, "KIPP_BIN="+a.c.Bin)
	}
	if j.a.Baseline != nil {
		path, err := writeBaseline(ws, j.a)
		if err != nil {
			_, _ = fmt.Fprintf(out, "kipp: could not write the stage's KIPP_METRICS on agent %s: %v\n", name, err)
			return -1, err
		}
		extra = append(extra, "KIPP_METRICS="+path)
	}
	envPath, err := resetEnvFile(ws, j.a.Stage)
	if err != nil {
		_, _ = fmt.Fprintf(out, "kipp: could not make the stage's KIPP_ENV on agent %s: %v\n", name, err)
		return -1, err
	}
	extra = append(extra, "KIPP_ENV="+envPath)
	vars, remove, err := a.writeSecretFiles(j.a, out)
	if err != nil {
		return -1, err
	}
	defer remove()
	extra = append(extra, vars...)
	tmp, removeTmp, err := runner.StageTemp(a.c.tmpDir())
	if err != nil {
		_, _ = fmt.Fprintf(out, "kipp: could not make the stage's KIPP_TMP on agent %s: %v\n", name, err)
		return -1, err
	}
	defer removeTmp()
	extra = append(extra, "KIPP_TMP="+tmp, "KIPP_OS="+runtime.GOOS, "KIPP_ARCH="+runtime.GOARCH)
	env := stageEnv(a.c.Env, j.a, ws, name, extra...)
	specs, hits, cenv := restoreDeclared(j.a, dir, env, out)
	code, err := j.runTries(ctx, out, func() (int, error) {
		if _, err := resetEnvFile(ws, j.a.Stage); err != nil {
			return -1, err
		}
		return runStage(ctx, a.c.Shell, j.a, dir, append(slices.Clip(env), "KIPP_ATTEMPT="+strconv.Itoa(j.tries)), out, j.cancel)
	})
	if err == nil && ctx.Err() == nil && (code == 0 || slices.Contains(j.a.UnstableExit, code)) {
		cache.SaveDeclared(specs, hits, cenv)
	}
	return code, err
}

// writeSecretFiles writes the stage's file secrets to the agent's own
// directory, and gives the variables that name them and the func that removes
// them, which is safe to run when there are none.
func (a *agent) writeSecretFiles(as *agentproto.Assignment, out io.Writer) ([]string, func(), error) {
	if len(as.SecretFiles) == 0 {
		return nil, func() {}, nil
	}
	files := make(map[string][]byte, len(as.SecretFiles))
	for _, f := range as.SecretFiles {
		files[f.Name] = f.Data
	}
	vars, remove, err := runner.WriteSecretFiles(a.c.secretFilesDir(), files)
	if err != nil {
		_, _ = fmt.Fprintf(out, "kipp: could not write the stage's secret files on agent %s: %v\n", a.nameOf(), err)
		return nil, nil, err
	}
	return vars, remove, nil
}

// runTries runs the stage's command, and again while it ends with a code
// that would end the stage failed and retries remain. A cancel, the agent
// stopping and a command that could not run end it at once.
func (j *job) runTries(ctx context.Context, out io.Writer, run func() (int, error)) (code int, err error) {
	j.tries = 0
	for {
		j.tries++
		code, err = run()
		if err != nil || ctx.Err() != nil || code == 0 || slices.Contains(j.a.UnstableExit, code) || j.tries > j.a.Retries {
			return code, err
		}
		select {
		case <-j.cancel:
			return code, err
		default:
		}
		_, _ = fmt.Fprintf(out, "kipp: try %d of %d\n", j.tries+1, j.a.Retries+1)
	}
}

// restoreDeclared restores the assignment's declared caches before its
// command, in dir, reading the cache server and token from env.
func restoreDeclared(a *agentproto.Assignment, dir string, env []string, out io.Writer) ([]cache.Declared, []bool, cache.Env) {
	var specs []cache.Declared
	for _, c := range a.Cache {
		specs = append(specs, cache.Declared{Key: c.Key, RestoreKeys: c.RestoreKeys, Paths: c.Paths, Scope: c.Scope})
	}
	home, _ := os.UserHomeDir()
	cenv := cache.Env{
		Getenv: func(k string) string {
			for _, kv := range slices.Backward(env) {
				if v, ok := strings.CutPrefix(kv, k+"="); ok {
					return v
				}
			}
			return ""
		},
		Dir: dir, Home: home, Stdout: out, Stderr: out,
	}
	return specs, cache.RestoreDeclared(specs, cenv), cenv
}

// pollControl asks the server, a long-poll after another, whether to stop.
func (a *agent) pollControl(ctx context.Context, j *job, stop <-chan struct{}) {
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-stop:
			cancel()
		case <-pctx.Done():
		}
	}()
	for pctx.Err() == nil {
		var c agentproto.Control
		err := a.call(pctx, http.MethodGet, stagePath(j.a.ID, agentproto.ControlSuffix), nil, &c)
		switch {
		case err == nil && c.Action == agentproto.ActionCancel:
			j.stop()
			return
		case err != nil && !retryable(err):
			return
		case err != nil && !sleep(pctx, a.c.Retry):
			return
		}
	}
}

// sayDone tells the server the exit code and sends the junit and metrics
// files and the stage's KIPP_ENV file, made again while the server cannot
// be reached for up to doneWait. The agent may be stopping, so ctx ending
// does not stop it.
func (a *agent) sayDone(ctx context.Context, j *job, code int, runErr error) {
	wait := doneWait
	if ctx.Err() != nil {
		wait = stopWait
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), wait)
	defer cancel()
	tarball, terr := a.junit(j)
	msg := ""
	if err := errors.Join(runErr, terr); err != nil {
		msg = err.Error()
	}
	for {
		resp, err := a.do(ctx, http.MethodPost, donePath(j.a.ID, code, j.tries, msg), agentproto.JunitType, bytes.NewReader(tarball))
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		if se, ok := errors.AsType[*statusError](err); ok && se.code == http.StatusGone {
			// The server ended the stage itself (the agent was offline) or
			// started again and knows nothing of it: either way no one will
			// say its build ended here.
			a.forgotten(j)
			return
		}
		if !retryable(err) || !sleep(ctx, a.c.Retry) {
			a.c.Log.Warn("the server was not told how a stage ended", "stage", j.a.Stage, "job", j.a.Job, "number", j.a.Number, "err", err)
			return
		}
	}
}

func (a *agent) junit(j *job) ([]byte, error) {
	ws, dir, err := stageDirs(a.c.Workdir, j.a)
	if err != nil {
		return nil, nil
	}
	return reportTar(dir, readEnvFile(ws, j.a.Stage), j.a.Junit, j.a.Metrics)
}

// forgotten marks j as a stage the server no longer knows, whose
// workspace goes when it is finished.
func (a *agent) forgotten(j *job) {
	a.mu.Lock()
	j.killed = true
	a.mu.Unlock()
}

// finishJob forgets the assignment, and removes the workspace of one the
// server no longer knew: nobody will say its build ended.
func (a *agent) finishJob(j *job) {
	a.mu.Lock()
	delete(a.running, j.a.ID)
	killed := j.killed
	a.mu.Unlock()
	if killed {
		a.endBuilds([]agentproto.Ended{{Workspace: j.a.Workspace}})
	}
}
