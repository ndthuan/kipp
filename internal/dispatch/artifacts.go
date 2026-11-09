package dispatch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/ndthuan/kipp/internal/artifacts"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/runnerproto"
	"github.com/ndthuan/kipp/internal/store"
)

// ArtifactsDir holds every build's kept files, as
// <job>/<number>/<stage>/<path>.
func (d *Dispatcher) ArtifactsDir() string { return filepath.Join(d.opts.DataDir, "artifacts") }

// StageArtifactsDir is where one stage of a build keeps its files.
func (d *Dispatcher) StageArtifactsDir(job string, number int, stage string) string {
	return filepath.Join(d.ArtifactsDir(), job, strconv.Itoa(number), stage)
}

// artifactsStaging is where the files a stage on a runner sent are put on
// the server, before they are kept with the stage's own glob.
func artifactsStaging(e *buildEnv, s *config.Stage) string {
	return filepath.Join(e.workspace, ".kipp-artifacts", s.Name)
}

// remoteArtifacts is what the hub needs of a stage's artifacts key.
func remoteArtifacts(e *buildEnv, s *config.Stage) (*runnerproto.Artifacts, string) {
	if s.Artifacts == nil {
		return nil, ""
	}
	return &runnerproto.Artifacts{Paths: s.Artifacts.Paths, Max: s.Artifacts.Max()}, artifactsStaging(e, s)
}

// keepArtifacts keeps what the stage's globs match, once its command has
// ended however it ended, and is the stage's status after it: a stage
// whose matches are over its max_size, or lead out of its directory,
// keeps none and fails, unless it was cancelled, which it stays.
func (d *Dispatcher) keepArtifacts(ctx context.Context, b *store.Build, e *buildEnv, s *config.Stage, status store.StageStatus) store.StageStatus {
	if s.Artifacts == nil {
		return status
	}
	src := filepath.Join(e.workdir, s.Workdir)
	if d.remote(s) {
		src = artifactsStaging(e, s)
	}
	dst := d.StageArtifactsDir(b.Job, b.Number, s.Name)
	err := d.storeArtifacts(ctx, b, s, src, dst)
	if err == nil {
		return status
	}
	d.note(b, s.Name, e, "kipp: "+err.Error())
	if rmErr := os.RemoveAll(dst); rmErr != nil {
		d.log.Error("removing a stage's refused artifacts", "build", b.ID, "stage", s.Name, "err", rmErr)
	}
	if status == store.StageCancelled {
		return status
	}
	return store.StageFailed
}

func (d *Dispatcher) storeArtifacts(ctx context.Context, b *store.Build, s *config.Stage, src, dst string) error {
	files, err := artifacts.Collect(src, s.Artifacts.Paths, s.Artifacts.Max())
	if err != nil {
		return err
	}
	if err := artifacts.Copy(src, dst, files); err != nil {
		return err
	}
	rows := make([]store.Artifact, len(files))
	for i, f := range files {
		rows[i] = store.Artifact{Stage: s.Name, Path: f.Path, Size: f.Size, SHA256: f.SHA256}
	}
	if err := d.st.PutArtifacts(context.WithoutCancel(ctx), b.ID, s.Name, rows); err != nil {
		return fmt.Errorf("artifacts: recording them: %w", err)
	}
	return nil
}

// inputsOf are the stages s is given the artifacts of, each with what
// they come to and where the server keeps them.
func (d *Dispatcher) inputsOf(ctx context.Context, b *store.Build, s *config.Stage) ([]runnerproto.Input, map[string]string, error) {
	if len(s.ArtifactsFrom) == 0 {
		return nil, nil, nil
	}
	ins := make([]runnerproto.Input, len(s.ArtifactsFrom))
	dirs := make(map[string]string, len(s.ArtifactsFrom))
	for i, from := range s.ArtifactsFrom {
		rows, err := d.st.StageArtifacts(ctx, b.ID, from)
		if err != nil {
			return nil, nil, err
		}
		ins[i] = runnerproto.Input{Stage: from}
		for _, r := range rows {
			ins[i].Size += r.Size
		}
		dirs[from] = d.StageArtifactsDir(b.Job, b.Number, from)
	}
	return ins, dirs, nil
}

// deliverInputs puts what each stage s is given kept under
// $KIPP_WORKSPACE/.kipp/artifacts/<stage>/, in place of what was there,
// through the workspace as a root: a stage before may have left a link
// there. A stage that kept nothing is an empty directory.
func (d *Dispatcher) deliverInputs(ctx context.Context, b *store.Build, e *buildEnv, s *config.Stage) error {
	for _, from := range s.ArtifactsFrom {
		rows, err := d.st.StageArtifacts(ctx, b.ID, from)
		if err != nil {
			return err
		}
		files := make([]artifacts.File, len(rows))
		for i, r := range rows {
			files[i] = artifacts.File{Path: r.Path, Size: r.Size, SHA256: r.SHA256}
		}
		rel := filepath.Join(".kipp", "artifacts", from)
		if err := emptyIn(e.workspace, rel); err != nil {
			return err
		}
		if err := artifacts.Copy(d.StageArtifactsDir(b.Job, b.Number, from), filepath.Join(e.workspace, rel), files); err != nil {
			return err
		}
	}
	return nil
}

// emptyIn makes rel inside dir an empty directory, through dir as a root.
func emptyIn(dir, rel string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := root.RemoveAll(rel); err != nil {
		return err
	}
	return root.MkdirAll(rel, 0o750)
}
