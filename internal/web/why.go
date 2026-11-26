package web

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/overview"
	"github.com/ndthuan/kipp/internal/store"
)

type why struct {
	Verb   string
	Stages []string
}

type whyRowData struct {
	Build *store.Build
	Why   *why
}

func whyRow(b *store.Build, w *why) whyRowData { return whyRowData{b, w} }

func commitShort(c string) string {
	if len(c) != 40 || strings.Trim(c, "0123456789abcdef") != "" {
		return ""
	}
	return c[:7]
}

func (s *Site) jobWhys(ctx context.Context, builds []*store.Build) (map[int64]*why, error) {
	var ids []int64
	for _, b := range builds {
		if b.Status.Red() {
			ids = append(ids, b.ID)
		}
	}
	stopped, err := s.st.StoppedStages(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := map[int64]*why{}
	for _, b := range builds {
		if b.Status.Red() {
			out[b.ID] = whyOf(b.Status, stopped[b.ID])
		}
	}
	return out, nil
}

func (s *Site) whys(ctx context.Context, o *overview.Overview) (map[int64]*why, error) {
	var builds []*store.Build
	for _, f := range o.Failing {
		builds = append(builds, f.Build)
	}
	return s.jobWhys(ctx, builds)
}

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

type notice struct {
	Class string
	Text  string
	Log   string
}

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
