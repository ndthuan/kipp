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
	Env     []string
	Output  io.Writer
}

// Proc is a started stage.
type Proc interface {
	Wait() (int, error)
	Kill(grace time.Duration)
	Group() Group
}

// Runner starts stages.
type Runner interface {
	Start(ctx context.Context, spec Spec) (Proc, error)
	Reap(g Group) (bool, error)
}
