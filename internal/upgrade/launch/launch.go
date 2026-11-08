// Package launch lets a kipp that is about to serve hand over to a newer
// kipp binary kept in the data directory. The executable that started
// (the image's, Homebrew's, an archive's) stays as it was; the data
// directory's binary is the one an upgrade put there.
package launch

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/ndthuan/kipp/internal/update"
	"github.com/ndthuan/kipp/internal/upgrade/state"
)

// The variables the launcher reads and sets.
const (
	// EnvUseBundled=1 makes the executable run itself, whatever the data
	// directory holds.
	EnvUseBundled = "KIPP_USE_BUNDLED_BINARY"
	// EnvLaunched=1 is set on a process that was started from the data
	// directory, and that does not look again.
	EnvLaunched = "KIPP_LAUNCHED_FROM_DATADIR"
	// EnvBundledExe and EnvBundledVersion name the executable that handed
	// over, and its version.
	EnvBundledExe     = "KIPP_BUNDLED_EXE"
	EnvBundledVersion = "KIPP_BUNDLED_VERSION"
)

const (
	// keepFor is how long a binary lower than the running one stays.
	keepFor      = 7 * 24 * time.Hour
	probeTimeout = 10 * time.Second
)

// Run is called by serve before it does anything else. It returns when the
// running executable is the one to serve; when the data directory holds a
// higher release it does not return, because the process becomes that
// binary with the same arguments, environment and process ID. version is
// the running executable's, and args are serve's own arguments.
func Run(log *slog.Logger, data, version string, args []string) {
	// A build that is not a release never looks at bin/, not even to
	// clean it.
	if !update.Release(version) {
		return
	}
	if os.Getenv(EnvLaunched) != "1" && os.Getenv(EnvUseBundled) != "1" {
		switchTo(log, data, version, args)
	}
	cleanup(log, data, version, time.Now())
}

// switchTo replaces this process with the data directory's binary when it
// is the higher release; any reason it cannot is logged once and the
// running executable carries on.
func switchTo(log *slog.Logger, data, version string, args []string) {
	cur, err := state.ReadCurrent(data)
	if err != nil {
		log.Warn("not reading the data directory's binary", "err", err)
		return
	}
	if cur == "" || !update.Newer(cur, version) {
		return
	}
	if rejected, err := state.ReadRejected(data); err != nil {
		log.Warn("not reading the rejected versions", "err", err)
		return
	} else if slices.Contains(rejected, cur) {
		return
	}
	path := state.BinaryPath(data, cur)
	if err := usable(path, cur); err != nil {
		log.Warn("not using the data directory's binary; running the executable's own",
			"version", cur, "path", path, "err", err)
		return
	}
	exe, err := os.Executable()
	if err != nil {
		log.Warn("not switching: the executable's own path is unknown", "err", err)
		return
	}
	env := append(withoutKeys(os.Environ(), EnvBundledExe, EnvBundledVersion, EnvLaunched),
		EnvBundledExe+"="+exe, EnvBundledVersion+"="+version, EnvLaunched+"=1")
	argv := append([]string{path, "serve"}, args...)
	// Exec returns only when it failed, and nothing has changed then.
	err = syscall.Exec(path, argv, env)
	log.Warn("not using the data directory's binary; running the executable's own",
		"version", cur, "path", path, "err", err)
}

// usable checks that path is an executable file that says it is kipp ver.
func usable(path, ver string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("not an executable file")
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").Output()
	if err != nil {
		return fmt.Errorf("version failed: %w", err)
	}
	line, _, _ := strings.Cut(string(out), "\n")
	if got := strings.TrimSpace(line); got != "kipp "+ver {
		return fmt.Errorf("it says %q, not kipp %s", got, ver)
	}
	return nil
}

func withoutKeys(env []string, keys ...string) []string {
	return slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return slices.Contains(keys, k)
	})
}

// cleanup removes the binaries of releases lower than the running one that
// are older than a week. The one named by previous stays, however old, and
// so does the one a pending record names, since either is where a rollback
// goes.
func cleanup(log *slog.Logger, data, version string, now time.Time) {
	entries, err := os.ReadDir(state.BinDir(data))
	if err != nil {
		return
	}
	keep := map[string]bool{}
	if prev, _ := state.ReadPrevious(data); prev != "" {
		keep[prev] = true
	}
	if p, _ := state.ReadPending(data); p != nil {
		keep[p.Version] = true
	}
	for _, e := range entries {
		v, ok := state.BinaryVersion(e.Name())
		if !ok || keep[v] || !update.Newer(version, v) || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) <= keepFor {
			continue
		}
		path := filepath.Join(state.BinDir(data), e.Name())
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Warn("not removing an old binary", "path", path, "err", err)
		}
	}
}
