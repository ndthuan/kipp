package index

import (
	"context"
	"strings"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/dispatch"
	"github.com/ndthuan/kipp/internal/store"
)

// PushEvent is what a push to the repo says: the ref and the commit it
// now names, whether it was deleted, and the repo's default branch.
type PushEvent struct {
	Repo, Ref, After string
	Deleted          bool
	DefaultBranch    string
}

// Push applies a push to the parent's repo: a branch it builds gets its
// child, made the first time, at the pushed head; a deleted branch's child
// becomes gone.
func (x *Index) Push(ctx context.Context, parent string, ev PushEvent) (*TriggerPlan, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	p, err := x.parentLocked(parent)
	if err != nil {
		return nil, err
	}
	m := p.Multibranch
	if !strings.EqualFold(ev.Repo, m.Repo) {
		return ignored("repository %s is not %s", ev.Repo, m.Repo), nil
	}
	branch, ok := strings.CutPrefix(ev.Ref, "refs/heads/")
	if !ok {
		return ignored("%s is not a branch", ev.Ref), nil
	}
	if err := x.setDefaultLocked(ctx, parent, ev.DefaultBranch); err != nil {
		return nil, err
	}
	defer x.publishLocked() // a new default branch changes what the children are on
	child := x.branchLocked(parent, branch)
	if ev.Deleted {
		if child == nil {
			return ignored("branch %s was deleted", branch), nil
		}
		return ignored("branch %s was deleted, and %s is gone", branch, child.Job()), x.goneLocked(ctx, *child)
	}
	plan := &TriggerPlan{}
	if m.RebuildOnBase {
		plan.Builds = x.onBaseLocked(m, parent, branch)
	}
	if plan.Ignored = x.skipLocked(m, parent, branch); plan.Ignored != "" {
		return plan, nil
	}
	c, err := x.liveBranchLocked(ctx, parent, branch, child, ev.After)
	if err != nil {
		return nil, err
	}
	own := dispatch.TriggerRequest{Job: c.Job(), Kind: "webhook", Detail: ev.After, Branch: branch, DefaultBranch: x.defaults[parent], By: "github"}
	plan.Builds = append([]dispatch.TriggerRequest{own}, plan.Builds...)
	return plan, nil
}

// skipLocked says why a push to branch builds nothing for it, "" when it
// builds the branch's child. A branch with an open pull request is the
// pull request's to build, unless branch_with_pr says build; the default
// branch is always its own.
func (x *Index) skipLocked(m *config.Multibranch, parent, branch string) string {
	def := x.defaults[parent]
	switch {
	case branch != def && m.PullRequests != config.PROff && x.openPRLocked(parent, branch):
		if m.BranchWithPR == config.BranchWithPRBuild {
			return ""
		}
		return "branch " + branch + " has an open pull request, whose synchronize builds it"
	case !m.Selects(branch, def):
		return "branch " + branch + " is not built: multibranch.branches does not name it"
	}
	return ""
}

// branchLocked is the row of the branch's child, nil when it has none.
func (x *Index) branchLocked(parent, branch string) *store.Child {
	for _, c := range x.rows[parent] {
		if c.Kind == config.KindBranch && c.Branch == branch {
			return &c
		}
	}
	return nil
}

// liveBranchLocked makes the branch's child live at head, with the key it
// had, or a new one.
func (x *Index) liveBranchLocked(ctx context.Context, parent, branch string, child *store.Child, head string) (store.Child, error) {
	now := x.now()
	c := store.Child{Parent: parent, Kind: config.KindBranch, Branch: branch}
	if child != nil {
		c = *child
	} else {
		c.Key = config.ChildKey(branch, x.takenLocked(parent))
	}
	c.HeadSHA, c.State, c.GoneAt, c.LastSeenAt = head, config.ChildLive, nil, now
	return c, x.putLocked(ctx, c)
}

// takenLocked reports whether a key is another child's of the parent.
func (x *Index) takenLocked(parent string) func(string) bool {
	return func(key string) bool {
		for _, c := range x.rows[parent] {
			if c.Key == key {
				return true
			}
		}
		return false
	}
}
