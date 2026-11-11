package index

import (
	"context"
	"strings"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/dispatch"
	"github.com/ndthuan/kipp/internal/store"
)

// PushEvent is what a push to the repo says: the ref and the commit it
// now names (for a tag, the commit the tag names, not an annotated tag's
// object), whether it was deleted, and the repo's default branch.
type PushEvent struct {
	Repo, Ref, After string
	Deleted          bool
	DefaultBranch    string
}

// Push applies a push to the parent's repo: a branch it builds gets its
// child, made the first time, at the pushed head; a deleted branch's child
// becomes gone. A tag it builds is built by the tags' child.
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
	if err := x.setDefaultLocked(ctx, parent, ev.DefaultBranch); err != nil {
		return nil, err
	}
	defer x.publishLocked() // a new default branch changes what the children are on
	if tag, ok := strings.CutPrefix(ev.Ref, "refs/tags/"); ok {
		return x.tagLocked(ctx, m, parent, tag, ev)
	}
	branch, ok := strings.CutPrefix(ev.Ref, "refs/heads/")
	if !ok {
		return ignored("%s is neither a branch nor a tag", ev.Ref), nil
	}
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

// tagLocked plans the build of a pushed tag by the parent's tags' child,
// made the first time. A deleted tag, and one multibranch.tags leaves out,
// build nothing.
func (x *Index) tagLocked(ctx context.Context, m *config.Multibranch, parent, tag string, ev PushEvent) (*TriggerPlan, error) {
	switch {
	case ev.Deleted:
		return ignored("tag %s was deleted", tag), nil
	case m.Tags.Off:
		return ignored("tag %s is not built: tags: false", tag), nil
	case !m.Tags.Matches(tag):
		return ignored("tag %s is not built: tags leaves it out", tag), nil
	}
	c := store.Child{Parent: parent, Kind: config.KindTag}
	if old := x.tagsLocked(parent); old != nil {
		c = *old
	} else {
		c.Key = config.TagsKey
		if x.takenLocked(parent)(c.Key) {
			c.Key = config.ChildKey(c.Key, x.takenLocked(parent))
		}
	}
	c.HeadSHA, c.State, c.GoneAt, c.LastSeenAt = ev.After, config.ChildLive, nil, x.now()
	if err := x.putLocked(ctx, c); err != nil {
		return nil, err
	}
	req := dispatch.TriggerRequest{Job: c.Job(), Kind: "webhook", Detail: ev.After, Tag: tag, DefaultBranch: x.defaults[parent], By: "github"}
	return &TriggerPlan{Builds: []dispatch.TriggerRequest{req}}, nil
}

// tagsLocked is the row of the parent's tags' child, nil when it has none.
func (x *Index) tagsLocked(parent string) *store.Child {
	for _, c := range x.rows[parent] {
		if c.Kind == config.KindTag {
			return &c
		}
	}
	return nil
}
