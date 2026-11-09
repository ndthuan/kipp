// Package runnerbin holds the runner binaries kipp puts into the containers
// it starts, embedded at build time so that nothing is downloaded, pulled
// or kept beside the server.
package runnerbin

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
)

// The binaries are built into bin/ by make runners; the README keeps the
// directory there when they are not.
//
//go:embed bin
var embedded embed.FS

// For is the runner for an engine of that system and architecture.
func For(goos, goarch string) ([]byte, error) { return lookup(embedded, goos, goarch) }

func lookup(fsys fs.FS, goos, goarch string) ([]byte, error) {
	data, err := fs.ReadFile(fsys, "bin/kipp-runner_"+goos+"_"+goarch)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no runner binary for %s/%s in this build of kipp: build it with make build", goos, goarch)
	}
	return data, err
}
