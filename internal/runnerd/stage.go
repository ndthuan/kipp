package runnerd

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/ndthuan/kipp/internal/runner"
	"github.com/ndthuan/kipp/internal/runnerproto"
)

const cancelGrace = 10 * time.Second

const maxJunitBytes = 64 << 20

func runStage(ctx context.Context, shell string, a *runnerproto.Assignment, dir string, env []string, out io.Writer, cancel <-chan struct{}) (int, error) {
	proc, err := runner.Local{Shell: shell}.Start(ctx, runner.Spec{Dir: dir, Command: a.Command, Env: env, Output: out})
	if err != nil {
		return -1, fmt.Errorf("could not start the stage: %w", err)
	}
	ended := make(chan struct{})
	defer close(ended)
	go func() {
		select {
		case <-cancel:
			proc.Kill(cancelGrace)
		case <-ended:
		}
	}()
	return proc.Wait()
}

func stageDirs(workdir string, a *runnerproto.Assignment) (workspace, dir string, err error) {
	for _, p := range []string{a.Workspace, a.Workdir} {
		if p != "" && !filepath.IsLocal(p) {
			return "", "", fmt.Errorf("%q is not a path inside the workdir", p)
		}
	}
	root, err := os.OpenRoot(workdir)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = root.Close() }()
	rel := filepath.Join(".", a.Workspace, a.Workdir)
	if err := root.MkdirAll(rel, 0o750); err != nil {
		return "", "", err
	}
	return filepath.Join(workdir, a.Workspace), filepath.Join(workdir, rel), nil
}

func removeWorkspace(workdir, ws string) error {
	if !filepath.IsLocal(ws) || filepath.Clean(ws) == "." {
		return nil
	}
	root, err := os.OpenRoot(workdir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return root.RemoveAll(ws)
}

func reportTar(dir string, env []byte, patterns ...string) ([]byte, error) {
	patterns = slices.DeleteFunc(slices.Clone(patterns), func(p string) bool { return p == "" })
	if len(patterns) == 0 && len(env) == 0 {
		return nil, nil
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	var paths []string
	for _, pattern := range patterns {
		if !fs.ValidPath(pattern) {
			return nil, fmt.Errorf("pattern %q leaves the stage's directory", pattern)
		}
		matched, err := fs.Glob(root.FS(), pattern)
		if err != nil {
			return nil, err
		}
		paths = append(paths, matched...)
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	if len(paths) == 0 && len(env) == 0 {
		return nil, nil
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := addEnv(tw, env); err != nil {
		return nil, err
	}
	for _, p := range paths {
		if err := addFile(tw, root, p, &buf); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeBaseline(ws string, a *runnerproto.Assignment) (string, error) {
	name := "metrics-" + a.Stage + ".json"
	if filepath.Base(name) != name || !filepath.IsLocal(name) {
		return "", fmt.Errorf("stage %q is not a name a file may take", a.Stage)
	}
	root, err := os.OpenRoot(ws)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	if err := root.MkdirAll(".kipp", 0o750); err != nil {
		return "", err
	}
	rel := filepath.Join(".kipp", name)
	if err := root.WriteFile(rel, a.Baseline, 0o600); err != nil {
		return "", err
	}
	return filepath.Join(ws, rel), nil
}

func addEnv(tw *tar.Writer, env []byte) error {
	if len(env) == 0 {
		return nil
	}
	if err := tw.WriteHeader(&tar.Header{Name: runnerproto.EnvTarName, Mode: 0o600, Size: int64(len(env)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err := tw.Write(env)
	return err
}

func envFilePath(ws, stage string) (rel, abs string, err error) {
	rel = filepath.FromSlash(runnerproto.EnvFile(stage))
	if name := filepath.Base(rel); name != "env-"+stage || !filepath.IsLocal(rel) {
		return "", "", fmt.Errorf("stage %q is not a name a file may take", stage)
	}
	return rel, filepath.Join(ws, rel), nil
}

func resetEnvFile(ws, stage string) (string, error) {
	rel, abs, err := envFilePath(ws, stage)
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(ws)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	if err := root.MkdirAll(filepath.Dir(rel), 0o750); err != nil {
		return "", err
	}
	return abs, root.WriteFile(rel, nil, 0o600)
}

func readEnvFile(ws, stage string) []byte {
	rel, _, err := envFilePath(ws, stage)
	if err != nil {
		return nil
	}
	root, err := os.OpenRoot(ws)
	if err != nil {
		return nil
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open(rel)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, runnerproto.MaxEnvFile+1))
	if err != nil {
		return nil
	}
	return data
}

func addFile(tw *tar.Writer, root *os.Root, name string, buf *bytes.Buffer) error {
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return err
	}
	if int64(buf.Len())+info.Size() > maxJunitBytes {
		return fmt.Errorf("the junit and metrics files come to more than %d bytes", maxJunitBytes)
	}
	if err := tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(name), Mode: 0o600, Size: info.Size(), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err = io.CopyN(tw, f, info.Size())
	return err
}

type sink struct {
	mu     sync.Mutex
	w      io.Writer
	failed error
}

func (s *sink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed == nil {
		_, s.failed = s.w.Write(p)
	}
	return len(p), nil
}

func (s *sink) err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if errors.Is(s.failed, io.ErrClosedPipe) {
		return errors.New("the server stopped reading the log")
	}
	return s.failed
}
