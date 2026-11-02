package dispatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ndthuan/kipp/internal/store"
)

// ErrNotEnded is a build asked to be built again while it still runs.
var ErrNotEnded = errors.New("has not ended")

// Again queues a build of the same job as build id, for by, as kind (web or
// api): its params, as they were, and the checkout it had where it had one,
// so that its commit, branch or tag and, for a pull request, the base commit its
// head was merged into are the ones the original built. A param the job no
// longer has is dropped, and one it gained takes its default. The refusals
// are Trigger's, and ErrNotEnded for a build that has not ended.
func (d *Dispatcher) Again(ctx context.Context, id int64, kind, by string) (*store.Build, error) {
	o, err := d.st.Build(ctx, id)
	if err != nil {
		return nil, err
	}
	if !o.Status.Terminal() {
		return nil, fmt.Errorf("build #%d %w: it is %s", o.Number, ErrNotEnded, o.Status)
	}
	req := TriggerRequest{
		Job: o.Job, Kind: kind, Detail: fmt.Sprintf("again #%d", o.Number), By: by,
		Branch: o.TriggerBranch, DefaultBranch: o.TriggerDefaultBranch, Tag: o.TriggerTag,
		Commit: o.Commit(), BaseCommit: o.BaseCommit,
	}
	if req.Commit == "" {
		req.BuiltCommit = o.BuiltCommit
	}
	if job := d.Snapshot().Jobs[o.Job]; job != nil {
		req.Params = map[string]string{}
		for _, p := range job.Params {
			if v, ok := o.Params[p.Name]; ok {
				req.Params[p.Name] = v
			}
		}
	}
	b, err := d.Trigger(ctx, req)
	if err != nil {
		return nil, err
	}
	line := fmt.Sprintf("#%d built again from #%d by %s", b.Number, o.Number, by)
	if err := d.st.AddActivity(ctx, b.Job, line, time.Now()); err != nil {
		d.log.Error("recording a build made again", "job", b.Job, "number", b.Number, "err", err)
	}
	return b, nil
}
