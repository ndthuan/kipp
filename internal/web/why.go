package web

import (
	"context"
	"fmt"
	"slices"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/overview"
	"github.com/ndthuan/kipp/internal/store"
)

// why says where a red build went wrong: Verb is how it ended and Stages the
// logs to read, the failed stages of a failed build and the one a timeout or
// a lost server stopped. The overview reads them for every failing job
// with one query.
type why struct {
	Verb   string
	Stages []string
}

func (s *Site) whys(ctx context.Context, o *overview.Overview) (map[int64]*why, error) {
	var ids []int64
	for _, f := range o.Failing {
		ids = append(ids, f.Build.ID)
	}
	stopped, err := s.st.StoppedStages(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := map[int64]*why{}
	for _, f := range o.Failing {
		out[f.Build.ID] = whyOf(f.Build.Status, stopped[f.Build.ID])
	}
	return out, nil
}

// whyOf reads the stages that failed or were stopped the way the status says
// they count: a failed build's failed stages, the others' stopped one. A
// failed build with none failed stopped in the prepare step.
func whyOf(status store.Status, stages []*store.Stage) *why {
	w := &why{Verb: "failed", Stages: culprits(status, stages)}
	switch status {
	case store.Timeout:
		w.Verb = "timed out"
	case store.Lost:
		w.Verb = "lost"
	}
	if status == store.Failed && len(w.Stages) == 0 {
		w.Stages = []string{config.PrepareLog}
	}
	return w
}

// culprits are the names of the stages that account for a red status: the
// failed ones, or for a timeout and a lost server the stage it stopped
// (first, as the build can have been stopped in more than one).
func culprits(status store.Status, stages []*store.Stage) []string {
	var out []string
	for _, st := range stages {
		stopped := st.Status == store.StageCancelled || st.Status == store.StageRunning
		if (status == store.Failed && st.Status == store.StageFailed) || (status != store.Failed && stopped) {
			out = append(out, st.Name)
		}
	}
	if status != store.Failed && len(out) > 1 {
		out = out[:1]
	}
	return out
}

// notice is what the top of a red build's page says: the headline, the
// class that colours it (as the status chips are), and the log it points at,
// whose last lines the page script copies in.
type notice struct {
	Class string
	Text  string
	Log   string
}

// redNotice is nil for a build that did not end red, and for one that was
// cancelled or ended unstable, whose chip says enough.
func redNotice(v *api.BuildView, logs []logView) *notice {
	b := v.Build
	if !b.Status.Red() {
		return nil
	}
	names := culprits(b.Status, v.Stages)
	stage := ""
	if len(names) > 0 {
		stage = names[0]
	}
	n := &notice{Class: "notice-warn"}
	switch {
	case b.Status == store.Timeout:
		n.Text = "Timed out after " + took(b.StartedAt, b.FinishedAt)
		if stage != "" {
			n.Text += " in stage " + stage
		}
	case b.Status == store.Lost:
		n.Text = "Lost"
		if stage != "" {
			n.Text += " during stage " + stage
		}
		n.Text += ": the runner stopped answering"
	case !slices.ContainsFunc(v.Stages, func(st *store.Stage) bool { return st.StartedAt != nil }):
		n.Class, n.Text, stage = "notice-error", "Failed before any stage ran", config.PrepareLog
	default:
		n.Class, n.Text = "notice-error", failedText(v.Stages, stage, len(names)-1)
	}
	if slices.ContainsFunc(logs, func(l logView) bool { return l.Name == stage }) {
		n.Log = stage
	}
	return n
}

func failedText(stages []*store.Stage, stage string, more int) string {
	if stage == "" {
		return "The build failed"
	}
	text := "Stage " + stage + " failed"
	if i := slices.IndexFunc(stages, func(st *store.Stage) bool { return st.Name == stage }); i >= 0 && stages[i].ExitCode != nil {
		text = fmt.Sprintf("%s, exit %d", text, *stages[i].ExitCode)
	}
	if more > 0 {
		text = fmt.Sprintf("%s and %d more", text, more)
	}
	return text
}
