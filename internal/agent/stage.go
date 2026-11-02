package agent

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

	"github.com/ndthuan/kipp/internal/agentproto"
	"github.com/ndthuan/kipp/internal/runner"
)

// cancelGrace is how long a cancelled stage's process group has between
// SIGTERM and SIGKILL, as on the server.
const cancelGrace = 10 * time.Second

// maxJunitBytes is the most the junit and metrics files of one stage may
// come to: the server reads no report above 16 MiB, and a stage has a few.
const maxJunitBytes = 64 << 20

// runStage runs a's command in dir, its output to out, until it exits or
// cancel is closed. ctx ending kills it too: the agent is stopping.
func runStage(ctx context.Context, shell string, a *agentproto.Assignment, dir string, env []string, out io.Writer, cancel <-chan struct{}) (int, error) {
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

// stageDirs makes the build's workspace and the stage's directory inside
// it, through the workdir as a root so that a link there cannot lead out.
func stageDirs(workdir string, a *agentproto.Assignment) (workspace, dir string, err error) {
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

// removeWorkspace removes a build's directory under the workdir; never the
// workdir itself, which a container's agent was given as the workspace.
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

// reportTar is the files the stage's junit and metrics globs match in
// dir, as a tar whose names are their paths inside dir; an empty pattern
// is none. Only regular files count, each once, and nothing outside dir is
// read. env, what the stage wrote to its KIPP_ENV file, goes in under its
// own name when there is any.
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

// writeBaseline writes the stage's KIPP_METRICS into the build's workspace
// ws, through it as a root, and returns its path.
func writeBaseline(ws string, a *agentproto.Assignment) (string, error) {
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

// addEnv puts the stage's KIPP_ENV file into the report tar, when it wrote any.
func addEnv(tw *tar.Writer, env []byte) error {
	if len(env) == 0 {
		return nil
	}
	if err := tw.WriteHeader(&tar.Header{Name: agentproto.EnvTarName, Mode: 0o600, Size: int64(len(env)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err := tw.Write(env)
	return err
}

// envFilePath is the stage's KIPP_ENV file in the build's workspace ws.
func envFilePath(ws, stage string) (rel, abs string, err error) {
	rel = filepath.FromSlash(agentproto.EnvFile(stage))
	if name := filepath.Base(rel); name != "env-"+stage || !filepath.IsLocal(rel) {
		return "", "", fmt.Errorf("stage %q is not a name a file may take", stage)
	}
	return rel, filepath.Join(ws, rel), nil
}

// resetEnvFile makes the stage's KIPP_ENV file in the workspace, through it
// as a root, empty, and returns its path. It is run before every try, so
// that a try that failed leaves nothing for the one after it.
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

// readEnvFile reads the stage's KIPP_ENV file, and one byte more than the
// most it may hold of a longer one, which is how the server knows. A file
// that is not there, or that leads out of the workspace, is nothing.
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
	data, err := io.ReadAll(io.LimitReader(f, agentproto.MaxEnvFile+1))
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

// sink passes writes on until one fails and swallows them after: a log
// upload that broke must not make the stage's own writes fail, which
// would end a command that has nothing wrong with it.
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

// err is the write that failed, if one did.
func (s *sink) err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if errors.Is(s.failed, io.ErrClosedPipe) {
		return errors.New("the server stopped reading the log")
	}
	return s.failed
}
