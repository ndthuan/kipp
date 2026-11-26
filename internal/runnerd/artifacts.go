package runnerd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	"github.com/ndthuan/kipp/internal/artifacts"
	"github.com/ndthuan/kipp/internal/runnerproto"
)

func (a *daemon) sendArtifacts(ctx context.Context, j *job) error {
	spec := j.a.Artifacts
	if spec == nil {
		return nil
	}
	_, dir, err := stageDirs(a.c.Workdir, j.a)
	if err != nil {
		return err
	}
	files, err := artifacts.Collect(dir, spec.Paths, spec.Max)
	if err != nil {
		return err
	}
	var total int64
	for _, f := range files {
		total += f.Size
	}
	path := stagePath(j.a.ID, runnerproto.ArtifactsSuffix) + "?" + url.Values{"size": {strconv.FormatInt(total, 10)}}.Encode()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), doneWait)
	defer cancel()
	for {
		err := a.upload(ctx, path, dir, files)
		if err == nil {
			return nil
		}
		if retryable(err) && sleep(ctx, a.c.Retry) {
			continue
		}
		if se, ok := errors.AsType[*statusError](err); ok && se.code < 500 {
			return nil
		}
		a.c.Log.Warn("the stage's artifacts did not reach the server", "stage", j.a.Stage, "job", j.a.Job, "number", j.a.Number, "err", err)
		return fmt.Errorf("artifacts: they did not reach the server: %w", err)
	}
}

func (a *daemon) upload(ctx context.Context, path, dir string, files []artifacts.File) error {
	pr, pw := io.Pipe()
	go func() { _ = pw.CloseWithError(artifacts.TarFiles(pw, dir, files)) }()
	resp, err := a.do(ctx, http.MethodPut, path, runnerproto.JunitType, pr)
	_ = pr.Close()
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (a *daemon) fetchInputs(ctx context.Context, j *job, ws string) error {
	for _, in := range j.a.Inputs {
		if filepath.Base(in.Stage) != in.Stage || !filepath.IsLocal(in.Stage) {
			return fmt.Errorf("stage %q is not a name a directory may take", in.Stage)
		}
		if err := a.fetchInput(ctx, j, in, ws); err != nil {
			return fmt.Errorf("the artifacts of stage %s: %w", in.Stage, err)
		}
	}
	return nil
}

func (a *daemon) fetchInput(ctx context.Context, j *job, in runnerproto.Input, ws string) error {
	rel := filepath.Join(".kipp", "artifacts", in.Stage)
	if err := emptyDir(ws, rel); err != nil {
		return err
	}
	resp, err := a.do(ctx, http.MethodGet, stagePath(j.a.ID, runnerproto.InputsSuffix+url.PathEscape(in.Stage)), "", nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, err = artifacts.Untar(resp.Body, filepath.Join(ws, rel), in.Size, -1)
	return err
}

func emptyDir(ws, rel string) error {
	root, err := os.OpenRoot(ws)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := root.MkdirAll(filepath.Dir(rel), 0o750); err != nil {
		return err
	}
	return root.RemoveAll(rel)
}
