// Package index keeps the children of multibranch jobs: which branches and
// pull requests of a repo are built, as which child job, and what an event
// from the repo builds. It adds the live children to the job set the
// dispatcher holds; a child is declared by its repo's file, never by the
// server's own job set.
package index

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/dispatch"
	"github.com/ndthuan/kipp/internal/store"
)

// ErrBadEvent is an event that is the job's and cannot be built as sent.
var ErrBadEvent = errors.New("bad event")

// Index is every multibranch job's children. Its methods are safe to call
// at once.
type Index struct {
	st      *store.Store
	publish func(*config.Snapshot)
	now     func() time.Time

	// mu is held across every change: the row, the cache and the
	// published job set change together, so two events never publish a
	// set that misses what the other added.
	mu       sync.Mutex
	base     *config.Snapshot
	rows     map[string][]store.Child     // by parent, as the store holds them
	defaults map[string]string            // the repo's default branch by parent
	repos    map[string][]store.OwnerRepo // by owner job, as the store holds them
}

// New is an index over the store's rows. publish is handed every job set
// it makes, the job files' with the children added: the dispatcher's
// SetSnapshot.
func New(st *store.Store, publish func(*config.Snapshot)) *Index {
	return &Index{st: st, publish: publish, now: time.Now, base: &config.Snapshot{Jobs: map[string]*config.Job{}},
		rows: map[string][]store.Child{}, defaults: map[string]string{}, repos: map[string][]store.OwnerRepo{}}
}

// SetBase takes the job files' set, at the start and at every reload, and
// publishes it with the children. When the rows cannot be read, the set
// is published with the children read before, and the error returned.
func (x *Index) SetBase(ctx context.Context, base *config.Snapshot) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.base = base
	err := x.readLocked(ctx)
	x.publishLocked()
	return err
}

func (x *Index) readLocked(ctx context.Context) error {
	rows, err := x.st.Children(ctx, "")
	if err != nil {
		return err
	}
	defaults, err := x.st.DefaultBranches(ctx)
	if err != nil {
		return err
	}
	repos, err := x.st.OwnerRepos(ctx, "")
	if err != nil {
		return err
	}
	x.rows = map[string][]store.Child{}
	for _, c := range rows {
		x.rows[c.Parent] = append(x.rows[c.Parent], c)
	}
	x.repos = map[string][]store.OwnerRepo{}
	for _, r := range repos {
		x.repos[r.Owner] = append(x.repos[r.Owner], r)
	}
	x.defaults = defaults
	return nil
}

func (x *Index) publishLocked() { x.publish(x.Snapshot(x.base)) }

// Snapshot is base with the repos of its owner jobs added as multibranch
// jobs, and the children of its multibranch jobs and of those repos: the
// live ones, and the gone ones for orphans.days after they went. A child
// whose parent is not in base, and a pull request from a fork, is none.
func (x *Index) Snapshot(base *config.Snapshot) *config.Snapshot {
	now := x.now()
	withNodes := base.WithJobs(x.nodes(base, now)...)
	var kids []*config.Job
	for _, parent := range withNodes.Names {
		p := withNodes.Jobs[parent]
		if !p.IsParent() {
			continue
		}
		for _, c := range x.rows[parent] {
			if !inSet(p.Multibranch, c, now) {
				continue
			}
			job := p.Child(record(c))
			job.DefaultBranch = x.defaults[parent]
			kids = append(kids, job)
		}
	}
	return withNodes.WithJobs(kids...)
}

// inSet reports whether the child is a job now.
func inSet(m *config.Multibranch, c store.Child, now time.Time) bool {
	switch {
	case c.FromFork || !m.Builds(c.Kind):
		return false
	case c.State == config.ChildGone:
		return c.GoneAt != nil && now.Before(c.GoneAt.AddDate(0, 0, m.Orphans.Days))
	}
	return true
}

func record(c store.Child) config.ChildRecord {
	r := config.ChildRecord{Parent: c.Parent, Key: c.Key, Kind: c.Kind, Branch: c.Branch, PR: c.PR, Title: c.Title, Author: c.Author, URL: c.URL,
		HeadSHA: c.HeadSHA, BaseBranch: c.BaseBranch, FromFork: c.FromFork, State: c.State, LastSeenAt: c.LastSeenAt}
	if c.GoneAt != nil {
		r.GoneAt = *c.GoneAt
	}
	return r
}

// Children are the parent's rows, in key order.
func (x *Index) Children(parent string) []store.Child {
	x.mu.Lock()
	defer x.mu.Unlock()
	return slices.Clone(x.rows[parent])
}

// DefaultBranch is the repo's default branch as the parent last heard it.
func (x *Index) DefaultBranch(parent string) string {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.defaults[parent]
}

// Parents are the names of the multibranch jobs of the job files, in their
// order.
func (x *Index) Parents() []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	var out []string
	for _, name := range x.base.Names {
		if x.base.Jobs[name].IsParent() {
			out = append(out, name)
		}
	}
	return out
}

// Multibranch is the multibranch: of the job file called name, or of the
// repo's job of an owner job that is called so; nil when there is none.
func (x *Index) Multibranch(name string) *config.Multibranch {
	x.mu.Lock()
	defer x.mu.Unlock()
	if p := x.lookupLocked(name); p != nil && p.IsParent() {
		return p.Multibranch
	}
	return nil
}

// TriggerPlan is what an event builds: the builds to queue, or why it
// builds nothing.
type TriggerPlan struct {
	Builds  []dispatch.TriggerRequest
	Ignored string
	// Rebuild says the builds are asked for again on purpose, of a commit
	// that may have a build queued or running: a pull request moved to
	// another base is merged with it anew.
	Rebuild bool
}

func ignored(format string, args ...any) *TriggerPlan {
	return &TriggerPlan{Ignored: fmt.Sprintf(format, args...)}
}

// parentLocked is the multibranch job called name: a job file's, or a
// repo's of an owner job.
func (x *Index) parentLocked(name string) (*config.Job, error) {
	p := x.lookupLocked(name)
	if p == nil || !p.IsParent() {
		return nil, fmt.Errorf("%s is not a multibranch job", name)
	}
	return p, nil
}

// putLocked writes the row and keeps it in the cache.
func (x *Index) putLocked(ctx context.Context, c store.Child) error {
	if err := x.st.PutChild(ctx, c); err != nil {
		return err
	}
	rows := x.rows[c.Parent]
	i := slices.IndexFunc(rows, func(r store.Child) bool { return r.Key == c.Key })
	if i >= 0 {
		rows[i] = c
	} else {
		rows = append(rows, c)
		slices.SortFunc(rows, func(a, b store.Child) int { return strings.Compare(a.Key, b.Key) })
	}
	x.rows[c.Parent] = rows
	return nil
}

// setDefaultLocked keeps the repo's default branch an event named.
func (x *Index) setDefaultLocked(ctx context.Context, parent, branch string) error {
	if branch == "" || x.defaults[parent] == branch {
		return nil
	}
	if err := x.st.SetDefaultBranch(ctx, parent, branch, x.now()); err != nil {
		return err
	}
	x.defaults[parent] = branch
	return nil
}

// goneLocked makes the row gone, from now, unless it is already.
func (x *Index) goneLocked(ctx context.Context, c store.Child) error {
	if c.State == config.ChildGone {
		return nil
	}
	now := x.now()
	c.State, c.GoneAt, c.LastSeenAt = config.ChildGone, &now, now
	return x.putLocked(ctx, c)
}

// Sweep takes out of the job set the gone children past orphans.days,
// whose builds are then kept as a removed job's are, and removes the rows
// of children out of the set that have no build left.
func (x *Index) Sweep(ctx context.Context) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	now := x.now()
	var errs []error
	for parent, rows := range x.rows {
		p := x.lookupLocked(parent)
		for _, c := range rows {
			if p != nil && p.IsParent() && (c.FromFork && c.State == config.ChildLive || inSet(p.Multibranch, c, now)) {
				continue
			}
			errs = append(errs, x.st.DeleteChild(ctx, parent, c.Key))
		}
	}
	errs = append(errs, x.sweepReposLocked(ctx, now))
	errs = append(errs, x.readLocked(ctx))
	x.publishLocked()
	return errors.Join(errs...)
}
