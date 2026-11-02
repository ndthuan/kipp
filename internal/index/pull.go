package index

import (
	"context"
	"strings"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/dispatch"
	"github.com/ndthuan/kipp/internal/store"
)

// PullRequestEvent is what a pull request event says of it.
type PullRequestEvent struct {
	Repo, DefaultBranch string
	Action              string
	Number              int
	Title               string
	Author, URL         string
	// HeadRepo is the repo the head is in: another one is a fork, and so
	// is none, a fork that was deleted.
	HeadRef, HeadSHA, HeadRepo string
	BaseRef                    string
	// BaseChanged is an edit that moved the pull request to another base.
	BaseChanged bool
}

// PullRequest applies a pull request event: opened, reopened, synchronize
// and an edit of its base build its children (one under merge and head,
// two under both); closed makes them gone. A pull request from a fork is
// listed and never built: it would run with the job's secrets.
func (x *Index) PullRequest(ctx context.Context, parent string, ev PullRequestEvent) (*TriggerPlan, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	p, err := x.parentLocked(parent)
	if err != nil {
		return nil, err
	}
	m := p.Multibranch
	switch {
	case !strings.EqualFold(ev.Repo, m.Repo):
		return ignored("repository %s is not %s", ev.Repo, m.Repo), nil
	case m.PullRequests == config.PROff:
		return ignored("multibranch.pull_requests is off"), nil
	}
	if err := x.setDefaultLocked(ctx, parent, ev.DefaultBranch); err != nil {
		return nil, err
	}
	defer x.publishLocked()
	switch {
	case ev.Action == "closed":
		return x.closeLocked(ctx, parent, ev.Number)
	case ev.Action == "opened" || ev.Action == "reopened" || ev.Action == "synchronize" || ev.Action == "edited" && ev.BaseChanged:
		plan, err := x.openLocked(ctx, m, parent, ev)
		if plan != nil {
			plan.Rebuild = ev.Action == "edited"
		}
		return plan, err
	}
	return ignored("pull request #%d %s: nothing to build", ev.Number, ev.Action), nil
}

// fromFork reports whether the head is in a repo other than the base's.
func (ev PullRequestEvent) fromFork() bool { return !strings.EqualFold(ev.HeadRepo, ev.Repo) }

// kinds are the children a pull request has under the mode.
func kinds(mode string) []string {
	switch mode {
	case config.PRHead:
		return []string{config.KindPRHead}
	case config.PRBoth:
		return []string{config.KindPR, config.KindPRHead}
	}
	return []string{config.KindPR}
}

// openLocked makes the pull request's children live at its head and
// plans their builds; a fork's pull request gets one row, to be listed.
func (x *Index) openLocked(ctx context.Context, m *config.Multibranch, parent string, ev PullRequestEvent) (*TriggerPlan, error) {
	ks := kinds(m.PullRequests)
	if ev.fromFork() {
		ks = ks[:1]
	}
	plan := &TriggerPlan{}
	for _, kind := range ks {
		c, err := x.livePRLocked(ctx, m, parent, kind, ev)
		if err != nil {
			return nil, err
		}
		if c.FromFork {
			return ignored("pull request #%d is from a fork: not built", ev.Number), nil
		}
		plan.Builds = append(plan.Builds, dispatch.TriggerRequest{Job: c.Job(), Kind: "webhook", Detail: ev.HeadSHA,
			Branch: ev.HeadRef, DefaultBranch: x.defaults[parent], By: "github"})
	}
	return plan, nil
}

// livePRLocked writes the row of the pull request's child of kind, live
// at the event's head.
func (x *Index) livePRLocked(ctx context.Context, m *config.Multibranch, parent, kind string, ev PullRequestEvent) (store.Child, error) {
	c := store.Child{Parent: parent, Key: config.PRKey(ev.Number, kind, m.PullRequests)}
	if old := x.keyLocked(parent, c.Key); old != nil {
		c = *old
	}
	c.Kind, c.Branch, c.PR, c.Title, c.HeadSHA, c.BaseBranch = kind, ev.HeadRef, ev.Number, ev.Title, ev.HeadSHA, ev.BaseRef
	c.Author, c.URL = ev.Author, ev.URL
	c.FromFork, c.State, c.GoneAt, c.LastSeenAt = ev.fromFork(), config.ChildLive, nil, x.now()
	return c, x.putLocked(ctx, c)
}

// closeLocked makes the pull request's children gone.
func (x *Index) closeLocked(ctx context.Context, parent string, n int) (*TriggerPlan, error) {
	var gone []string
	for _, c := range x.rows[parent] {
		if c.Kind == config.KindBranch || c.PR != n {
			continue
		}
		if err := x.goneLocked(ctx, c); err != nil {
			return nil, err
		}
		gone = append(gone, c.Job())
	}
	if len(gone) == 0 {
		return ignored("pull request #%d was closed", n), nil
	}
	return ignored("pull request #%d was closed, and %s gone", n, strings.Join(gone, " and ")), nil
}

// keyLocked is the parent's row of that key, nil when there is none.
func (x *Index) keyLocked(parent, key string) *store.Child {
	for _, c := range x.rows[parent] {
		if c.Key == key {
			return &c
		}
	}
	return nil
}

// openPRLocked reports whether branch is the head of a live pull request
// of the repo itself.
func (x *Index) openPRLocked(parent, branch string) bool {
	for _, c := range x.rows[parent] {
		if c.Kind != config.KindBranch && !c.FromFork && c.State == config.ChildLive && c.Branch == branch {
			return true
		}
	}
	return false
}

// onBaseLocked are the builds of the live pull requests into base, for
// rebuild_on_base: each at the head it last saw, merged into the base as
// it is when the build clones.
func (x *Index) onBaseLocked(m *config.Multibranch, parent, base string) []dispatch.TriggerRequest {
	var out []dispatch.TriggerRequest
	for _, c := range x.rows[parent] {
		if c.Kind == config.KindBranch || c.FromFork || c.State != config.ChildLive || c.BaseBranch != base || !m.Builds(c.Kind) {
			continue
		}
		out = append(out, dispatch.TriggerRequest{Job: c.Job(), Kind: "webhook", Detail: c.HeadSHA, Branch: c.Branch,
			DefaultBranch: x.defaults[parent], By: "github"})
	}
	return out
}
