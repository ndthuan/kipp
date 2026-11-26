package dispatch

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

const statusTimeout = 15 * time.Second

// CommitStatus is what a build of a multibranch job's child says of the
// commit it built: the state GitHub knows (pending, success, failure or
// error), the context it says it under, a line of description, and the
// build's page when the server has a base URL.
type CommitStatus struct {
	SHA, State, Context, Description, TargetURL string
}

func (d *Dispatcher) reports(b *store.Build, job *config.Job) bool {
	return d.opts.Status != nil && job.Origin != nil && job.Multibranch != nil && job.Multibranch.Authenticated() && !job.IsSideBuild(b.Params)
}

func statusContext(job *config.Job) string {
	ctx := "kipp/" + job.Parent
	if job.Folder != "" {
		ctx = "kipp/" + job.Folder
	}
	switch {
	case job.BuildsTags():
		ctx += "/tag"
	case job.Multibranch.PullRequests == config.PRBoth && job.Origin.Kind == config.KindPRHead:
		ctx += "/head"
	}
	return ctx
}

func finalState(status store.Status, ranStages bool) string {
	switch {
	case status.Succeeded():
		return "success"
	case status == store.Cancelled, status == store.Lost, status == store.Failed && !ranStages:
		return "error"
	}
	return "failure"
}

func (d *Dispatcher) statusCommit(b *store.Build) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if b.BuiltCommit == "" {
		return b.Commit()
	}
	return b.BuiltCommit
}

func (d *Dispatcher) sayFinal(ctx context.Context, b *store.Build, job *config.Job, e *buildEnv, status store.Status, ranStages bool) {
	bg := context.WithoutCancel(ctx)
	if by, ok := errors.AsType[supersededBy](context.Cause(ctx)); ok && status == store.Cancelled && by.commit != "" && by.commit == d.statusCommit(b) {
		return
	}
	if _, gone := errors.AsType[childGone](context.Cause(ctx)); gone && status == store.Cancelled && d.reports(b, job) {
		if old, state := d.verdictBefore(bg, b, job); old != nil {
			d.post(bg, b, job, e, state, old.Number, old.Status)
			return
		}
	}
	d.say(bg, b, job, e, finalState(status, ranStages), status)
}

func (d *Dispatcher) verdictBefore(ctx context.Context, b *store.Build, job *config.Job) (*store.Build, string) {
	sha := d.statusCommit(b)
	builds, err := d.st.Builds(ctx, b.Job, verdictLookback)
	if err != nil || sha == "" {
		return nil, ""
	}
	for _, old := range builds {
		if old.ID >= b.ID || old.BuiltCommit != sha || !old.Status.Terminal() || old.Status == store.Cancelled || old.Status == store.Lost || job.IsSideBuild(old.Params) {
			continue
		}
		return old, finalState(old.Status, d.ranStages(ctx, old.ID))
	}
	return nil, ""
}

func (d *Dispatcher) ranStages(ctx context.Context, id int64) bool {
	stages, err := d.st.Stages(ctx, id)
	return err == nil && slices.ContainsFunc(stages, func(s *store.Stage) bool { return s.StartedAt != nil })
}

const verdictLookback = 50

func (d *Dispatcher) say(ctx context.Context, b *store.Build, job *config.Job, e *buildEnv, state string, how store.Status) {
	d.post(ctx, b, job, e, state, b.Number, how)
}

func (d *Dispatcher) sayingQueued(id int64) (said func()) {
	ch := make(chan struct{})
	d.mu.Lock()
	d.saying[id] = ch
	d.mu.Unlock()
	return func() {
		d.mu.Lock()
		delete(d.saying, id)
		d.mu.Unlock()
		close(ch)
	}
}

func (d *Dispatcher) queuedSaid(id int64) {
	d.mu.Lock()
	ch := d.saying[id]
	d.mu.Unlock()
	if ch != nil {
		<-ch
	}
}

func (d *Dispatcher) post(ctx context.Context, b *store.Build, job *config.Job, e *buildEnv, state string, number int, how store.Status) {
	d.queuedSaid(b.ID)
	d.send(ctx, b, job, e, state, number, how)
}

func (d *Dispatcher) send(ctx context.Context, b *store.Build, job *config.Job, e *buildEnv, state string, number int, how store.Status) {
	if !d.reports(b, job) {
		return
	}
	sha := d.statusCommit(b)
	if sha == "" {
		return
	}
	s := CommitStatus{
		SHA: sha, State: state, Context: statusContext(job),
		Description: fmt.Sprintf("%s #%d %s", job.Name, number, how),
	}
	s.TargetURL = d.buildURL(job.Name, number)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), statusTimeout)
	defer cancel()
	if err := d.opts.Status(ctx, job, s); err != nil {
		d.note(b, config.HooksLog, e, fmt.Sprintf("kipp: commit status %s not posted: %v", state, err))
	}
}

func (d *Dispatcher) jobURL(job string) string {
	base := strings.TrimRight(d.opts.PublicURL, "/")
	if base == "" {
		return ""
	}
	return base + "/jobs/" + url.PathEscape(job)
}

func (d *Dispatcher) buildURL(job string, number int) string {
	base := d.jobURL(job)
	if base == "" {
		return ""
	}
	return base + "/" + strconv.Itoa(number)
}
