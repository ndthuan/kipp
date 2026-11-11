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

// statusTimeout is how long a status may take to be posted: a build is
// never held up longer than this by a GitHub that does not answer.
const statusTimeout = 15 * time.Second

// CommitStatus is what a build of a multibranch job's child says of the
// commit it built: the state GitHub knows (pending, success, failure or
// error), the context it says it under, a line of description, and the
// build's page when the server has a base URL.
type CommitStatus struct {
	SHA, State, Context, Description, TargetURL string
}

// reports reports whether the build of job says its state to GitHub: a
// child of a multibranch job that names a token, on a server that posts.
func (d *Dispatcher) reports(job *config.Job) bool {
	return d.opts.Status != nil && job.Origin != nil && job.Multibranch != nil && job.Multibranch.Authenticated()
}

// statusContext is what the child's statuses are said under: the parent's
// name, or the owner job's for a repo of one (the commit says the repo).
// The extra build of a pull request's head under both, and a tag's build,
// say theirs apart, so that they never overwrite the build of the merge or
// of the default branch on the same commit.
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

// finalState is the state GitHub is told a build ended in. Only a build
// that ran its stages and failed is a failure: one that was stopped, lost
// or never got to a stage did not say anything about the code.
func finalState(status store.Status, ranStages bool) string {
	switch {
	case status.Succeeded():
		return "success"
	case status == store.Cancelled, status == store.Lost, status == store.Failed && !ranStages:
		return "error"
	}
	return "failure"
}

// statusCommit is the commit the build's statuses are on: the one it built,
// or while that is not known the one a push named.
func (d *Dispatcher) statusCommit(b *store.Build) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if b.BuiltCommit == "" {
		return b.Commit()
	}
	return b.BuiltCommit
}

// sayFinal posts how the build ended. A build cancelled because its child
// went said nothing of the code, so the commit gets back the state of the
// job's build of it before, when there is one: a pull request merged the
// moment it went green moves its base, and the rebuild on that base can
// start before GitHub says the pull request closed.
func (d *Dispatcher) sayFinal(ctx context.Context, b *store.Build, job *config.Job, e *buildEnv, status store.Status, ranStages bool) {
	bg := context.WithoutCancel(ctx)
	if _, gone := errors.AsType[childGone](context.Cause(ctx)); gone && status == store.Cancelled && d.reports(job) {
		if old, state := d.verdictBefore(bg, b); old != nil {
			d.post(bg, b, job, e, state, old.Number, old.Status)
			return
		}
	}
	d.say(bg, b, job, e, finalState(status, ranStages), status)
}

// verdictBefore is the job's latest build before b that ended on b's
// commit, among its recent builds, and the state it told GitHub; nil when
// there is none. A cancelled or lost one said nothing and is passed over.
func (d *Dispatcher) verdictBefore(ctx context.Context, b *store.Build) (*store.Build, string) {
	sha := d.statusCommit(b)
	builds, err := d.st.Builds(ctx, b.Job, verdictLookback)
	if err != nil || sha == "" {
		return nil, ""
	}
	for _, old := range builds {
		if old.ID >= b.ID || old.BuiltCommit != sha || !old.Status.Terminal() || old.Status == store.Cancelled || old.Status == store.Lost {
			continue
		}
		return old, finalState(old.Status, d.ranStages(ctx, old.ID))
	}
	return nil, ""
}

// ranStages reports whether the build started a stage, as finalState
// needs to know.
func (d *Dispatcher) ranStages(ctx context.Context, id int64) bool {
	stages, err := d.st.Stages(ctx, id)
	return err == nil && slices.ContainsFunc(stages, func(s *store.Stage) bool { return s.StartedAt != nil })
}

// verdictLookback is how many of a job's latest builds verdictBefore reads.
const verdictLookback = 50

// say posts one state of a child's build. A status that cannot be posted
// is a line in the build's hooks log and changes nothing else; a build
// with no commit to say it on says nothing.
func (d *Dispatcher) say(ctx context.Context, b *store.Build, job *config.Job, e *buildEnv, state string, how store.Status) {
	d.post(ctx, b, job, e, state, b.Number, how)
}

// post posts state on b's commit, said to be of build number of the job,
// which ended how.
func (d *Dispatcher) post(ctx context.Context, b *store.Build, job *config.Job, e *buildEnv, state string, number int, how store.Status) {
	if !d.reports(job) {
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

// jobURL is the address of the job's page, empty when --base-url was not
// given: the address kipp listens on is not one a stage elsewhere reaches.
func (d *Dispatcher) jobURL(job string) string {
	base := strings.TrimRight(d.opts.PublicURL, "/")
	if base == "" {
		return ""
	}
	return base + "/jobs/" + url.PathEscape(job)
}

// buildURL is the address of the build's page, empty as jobURL is.
func (d *Dispatcher) buildURL(job string, number int) string {
	base := d.jobURL(job)
	if base == "" {
		return ""
	}
	return base + "/" + strconv.Itoa(number)
}
