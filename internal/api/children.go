package api

import (
	"cmp"
	"context"
	"net/http"
	"slices"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/index"
	"github.com/ndthuan/kipp/internal/store"
)

// ChildView is one child of a multibranch job, with its last build. Job
// is empty for a pull request from a fork, which has no job.
type ChildView struct {
	store.Child
	Job  string
	Last *store.Build
}

// statusRank orders the statuses of builds from the best to the worst.
var statusRank = map[store.Status]int{
	store.Success: 1, store.Queued: 2, store.Running: 3, store.Unstable: 4,
	store.Cancelled: 5, store.Lost: 6, store.Timeout: 7, store.Failed: 8,
}

// childrenOf counts the parent's children in the job set that build, and
// finds the worst of their last builds.
func childrenOf(snap *config.Snapshot, parent string, last map[string]*store.Build) (int, store.Status) {
	n, worst := 0, store.Status("")
	for _, name := range snap.Names {
		j := snap.Jobs[name]
		if j.Parent != parent || j.Disabled {
			continue
		}
		n++
		if b := last[name]; b != nil && statusRank[b.Status] > statusRank[worst] {
			worst = b.Status
		}
	}
	return n, worst
}

// familyOf is what a job's row says of the children it has: how many, the
// worst of their last builds, and whether the job or one of them has a build
// running or queued.
func familyOf(snap *config.Snapshot, name string, j *config.Job, last map[string]*store.Build, active map[string]bool) (int, store.Status, bool) {
	n, worst := childrenOf(snap, name, last)
	if j.IsOwner() {
		n, worst = reposOf(snap, name, last)
	}
	return n, worst, active[name] || n > 0 && familyActive(snap, name, active)
}

// familyActive reports whether a job under parent, or in the owner's folder,
// has a build running or queued.
func familyActive(snap *config.Snapshot, parent string, active map[string]bool) bool {
	return slices.ContainsFunc(snap.Names, func(name string) bool {
		j := snap.Jobs[name]
		return active[name] && (j.Parent == parent || j.Folder == parent)
	})
}

// RepoView is one repo of an owner job, with how many children it builds
// and the worst of their last builds.
type RepoView struct {
	index.RepoStatus
	Children int
	Worst    store.Status
}

// reposOf counts the live repos of the owner job in the job set, and finds
// the worst last build among every child of them.
func reposOf(snap *config.Snapshot, owner string, last map[string]*store.Build) (int, store.Status) {
	n, worst := 0, store.Status("")
	for _, name := range snap.Names {
		j := snap.Jobs[name]
		if j.Folder != owner || j.Disabled {
			continue
		}
		if j.Parent == "" {
			n++
		}
		if b := last[name]; b != nil && statusRank[b.Status] > statusRank[worst] {
			worst = b.Status
		}
	}
	return n, worst
}

// family fills in a job's parent, or a multibranch job's children.
func (s *Service) family(ctx context.Context, v *JobView) error {
	if v.Job != nil && v.Job.Parent != "" {
		v.Parent = v.Job.Parent
		return nil
	}
	if v.Job == nil {
		p, err := s.deps.Store.ParentOf(ctx, v.Name)
		v.Parent = p
		return err
	}
	if !v.Job.IsParent() {
		return nil
	}
	kids, err := s.children(ctx, v.Name)
	v.Children = kids
	return err
}

// Children are a multibranch job's children as u may see them: the ones
// that build, then the gone ones, then the pull requests from forks.
func (s *Service) Children(ctx context.Context, u *store.User, parent string) ([]ChildView, error) {
	job, err := s.history(ctx, u, parent)
	if err != nil {
		return nil, err
	}
	if job != nil && !job.IsParent() {
		return nil, ErrNotFound
	}
	return s.children(ctx, parent)
}

func (s *Service) children(ctx context.Context, parent string) ([]ChildView, error) {
	rows, err := s.deps.Store.Children(ctx, parent)
	if err != nil {
		return nil, err
	}
	last, err := s.deps.Store.LastBuilds(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ChildView, 0, len(rows))
	for _, c := range rows {
		v := ChildView{Child: c, Last: last[c.Job()]}
		if !c.FromFork {
			v.Job = c.Job()
		}
		out = append(out, v)
	}
	// Live before gone, forks last; in key order within each.
	rank := func(c ChildView) int {
		switch {
		case c.FromFork:
			return 2
		case c.State == config.ChildGone:
			return 1
		}
		return 0
	}
	slices.SortStableFunc(out, func(a, b ChildView) int { return cmp.Compare(rank(a), rank(b)) })
	return out, nil
}

func (a *API) children(w http.ResponseWriter, r *http.Request, u *store.User) {
	kids, err := a.svc.Children(r.Context(), u, r.PathValue("name"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := make([]childV1, 0, len(kids))
	for _, c := range kids {
		out = append(out, toChildV1(c))
	}
	writeList(w, out, "")
}
