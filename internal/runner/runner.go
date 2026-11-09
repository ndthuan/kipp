// Package runner starts a stage's command and can kill everything it
// started. Local is the one implementation; a remote runner would be the
// second, which is why the interface exists.
package runner

import (
	"context"
	"io"
	"time"
)

// Spec is what one stage runs.
type Spec struct {
	Dir     string
	Command string
	Env     []string  // the complete environment, KEY=VALUE
	Output  io.Writer // stdout and stderr, interleaved, already masked
}

// Proc is a started stage.
type Proc interface {
	// Wait blocks until the command has exited and its process group is
	// gone. The exit code is -1 when it was killed.
	Wait() (int, error)
	// Kill sends SIGTERM to the process group and SIGKILL after grace. It
	// blocks until the group is gone or grace has passed, and does nothing
	// once the stage has finished.
	Kill(grace time.Duration)
	// Group is what to keep of the stage for Reap, should this server die
	// before the stage does.
	Group() Group
}

// Runner starts stages.
type Runner interface {
	Start(ctx context.Context, spec Spec) (Proc, error)
	// Reap kills what a stage started by a server that has since died left
	// running, and reports whether it killed anything. Something that runs
	// under the group's id and cannot be shown to be the stage's is left
	// alone, with ErrNotProven; where no such thing can be shown at all the
	// error is errors.ErrUnsupported.
	Reap(g Group) (bool, error)
}
