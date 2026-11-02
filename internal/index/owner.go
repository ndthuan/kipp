package index

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/github"
	"github.com/ndthuan/kipp/internal/store"
)

// ErrNotOwner is a read of a name that is no owner job.
var ErrNotOwner = errors.New("not an owner job")

// Owners are the names of the owner jobs of the job files, in their order.
func (x *Index) Owners() []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	var out []string
	for _, name := range x.base.Names {
		if x.base.Jobs[name].IsOwner() {
			out = append(out, name)
		}
	}
	return out
}

// Owner is the owner: of the job file called name, nil when there is none.
func (x *Index) Owner(name string) *config.Owner {
	x.mu.Lock()
	defer x.mu.Unlock()
	if j := x.base.Jobs[name]; j != nil && j.IsOwner() {
		return j.Owner
	}
	return nil
}

// Repos are the rows of the owner job's repos, in key order.
func (x *Index) Repos(owner string) []store.OwnerRepo {
	x.mu.Lock()
	defer x.mu.Unlock()
	return slices.Clone(x.repos[owner])
}

// lookupLocked is the job called name: a job file's, else the repo's job
// of an owner job, which is in the job set while its repo is live or
// gone for less than orphans.days.
func (x *Index) lookupLocked(name string) *config.Job {
	if j := x.base.Jobs[name]; j != nil {
		return j
	}
	now := x.now()
	for owner, rows := range x.repos {
		job := x.base.Jobs[owner]
		if job == nil || !job.IsOwner() {
			continue
		}
		for _, r := range rows {
			if r.Node() == name && repoInSet(job.Owner, r, now) {
				return job.Node(repoRecord(r))
			}
		}
	}
	return nil
}

// nodes are the jobs of the repos of the base's owner jobs that are in
// the job set.
func (x *Index) nodes(base *config.Snapshot, now time.Time) []*config.Job {
	var out []*config.Job
	for _, name := range base.Names {
		job := base.Jobs[name]
		if !job.IsOwner() {
			continue
		}
		for _, r := range x.repos[name] {
			if repoInSet(job.Owner, r, now) {
				out = append(out, job.Node(repoRecord(r)))
			}
		}
	}
	return out
}

// repoInSet reports whether the repo's job is in the job set now.
func repoInSet(o *config.Owner, r store.OwnerRepo, now time.Time) bool {
	if r.State == config.RepoGone {
		return r.GoneAt != nil && now.Before(r.GoneAt.AddDate(0, 0, o.Settings.Orphans.Days))
	}
	return true
}

func repoRecord(r store.OwnerRepo) config.RepoRecord {
	rec := config.RepoRecord{Owner: r.Owner, Key: r.Key, Repo: r.Repo, State: r.State, LastSeenAt: r.LastSeenAt}
	if r.GoneAt != nil {
		rec.GoneAt = *r.GoneAt
	}
	return rec
}

// RepoNode is the job of the live repo called fullName of the owner job,
// which a delivery from that repo is routed to.
func (x *Index) RepoNode(owner, fullName string) (string, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, r := range x.repos[owner] {
		if r.State == config.RepoLive && strings.EqualFold(r.Repo, fullName) {
			return r.Node(), true
		}
	}
	return "", false
}

// Discovery is what a read of an owner's repos made of them.
type Discovery struct {
	// Nodes are the jobs of the repos that are live now.
	Nodes []string
	// Added and Gone count the repos that became live and the ones that
	// left the set in this read.
	Added, Gone int
	// PublicOnly says the owner is another user's account, whose private
	// repos the token cannot list.
	PublicOnly bool
}

// DiscoverRepos reads the owner's repos from GitHub and makes live the
// ones that are in the job's set: not archived, not a fork, named by
// include and exclude, and holding the stages file at the default branch.
// A repo that left the set becomes gone with its children. A repo whose
// file could not be looked for keeps the state it had, and the error says
// which; what GitHub did answer is applied all the same.
func (x *Index) DiscoverRepos(ctx context.Context, owner string, gh *github.Client) (*Discovery, error) {
	x.mu.Lock()
	job := x.base.Jobs[owner]
	x.mu.Unlock()
	if job == nil || !job.IsOwner() {
		return nil, fmt.Errorf("%w: %s", ErrNotOwner, owner)
	}
	o := job.Owner
	var listing *github.Listing
	var err error
	if o.App != nil {
		listing, err = gh.InstallationRepos(ctx)
	} else {
		listing, err = gh.OwnerRepos(ctx, o.Login)
	}
	if err != nil {
		return nil, err
	}
	var found []github.OwnerRepo
	unknown := map[string]bool{}
	var errs []error
	for _, r := range listing.Repos {
		if r.Archived || r.Fork || !o.Matches(r.Name) || r.DefaultBranch == "" {
			continue
		}
		switch ok, err := gh.HasFile(ctx, r.FullName, o.Settings.Path, r.DefaultBranch); {
		case err != nil:
			unknown[strings.ToLower(r.FullName)] = true
			errs = append(errs, err)
		case ok:
			found = append(found, r)
		}
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	d, err := x.applyReposLocked(ctx, owner, found, unknown)
	if err != nil {
		return nil, err
	}
	d.PublicOnly = listing.PublicOnly
	return d, errors.Join(errs...)
}

// applyReposLocked makes the owner job's rows what the read found.
func (x *Index) applyReposLocked(ctx context.Context, owner string, found []github.OwnerRepo, unknown map[string]bool) (*Discovery, error) {
	if j := x.base.Jobs[owner]; j == nil || !j.IsOwner() {
		return nil, fmt.Errorf("%w: %s", ErrNotOwner, owner) // a reload took it away meanwhile
	}
	defer x.publishLocked()
	d := &Discovery{}
	seen := map[string]bool{}
	for _, r := range found {
		key, added, err := x.liveRepoLocked(ctx, owner, r)
		if err != nil {
			return nil, err
		}
		seen[key] = true
		if added {
			d.Added++
		}
	}
	var err error
	if d.Gone, err = x.leaveUnseenLocked(ctx, owner, seen, unknown); err != nil {
		return nil, err
	}
	for _, row := range x.repos[owner] {
		if row.State == config.RepoLive {
			d.Nodes = append(d.Nodes, row.Node())
		}
	}
	return d, nil
}

// leaveUnseenLocked makes gone the owner's live repos that the read did not
// find, but for the ones it could not look at, and counts them.
func (x *Index) leaveUnseenLocked(ctx context.Context, owner string, seen, unknown map[string]bool) (int, error) {
	n := 0
	for _, row := range slices.Clone(x.repos[owner]) {
		if seen[row.Key] || row.State == config.RepoGone || unknown[strings.ToLower(row.Repo)] {
			continue
		}
		if err := x.leaveRepoLocked(ctx, row); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// liveRepoLocked makes the repo live in the owner's rows and says its key
// and whether it is new or came back.
func (x *Index) liveRepoLocked(ctx context.Context, owner string, r github.OwnerRepo) (key string, added bool, err error) {
	row, isNew := x.repoRowLocked(owner, r)
	wasGone := row.State != config.RepoLive
	row.State, row.GoneAt, row.LastSeenAt = config.RepoLive, nil, x.now()
	if err := x.putRepoLocked(ctx, row); err != nil {
		return "", false, err
	}
	if err := x.setDefaultLocked(ctx, row.Node(), r.DefaultBranch); err != nil {
		return "", false, err
	}
	return row.Key, isNew || wasGone, nil
}

// leaveRepoLocked makes the repo gone, and its live children with it.
func (x *Index) leaveRepoLocked(ctx context.Context, row store.OwnerRepo) error {
	now := x.now()
	row.State, row.GoneAt, row.LastSeenAt = config.RepoGone, &now, now
	if err := x.putRepoLocked(ctx, row); err != nil {
		return err
	}
	return x.goneAllLocked(ctx, row.Node())
}

// repoRowLocked is the owner's row of the repo, a new one with a key of
// its own when there is none.
func (x *Index) repoRowLocked(owner string, r github.OwnerRepo) (store.OwnerRepo, bool) {
	for _, row := range x.repos[owner] {
		if strings.EqualFold(row.Repo, r.FullName) {
			row.Repo = r.FullName
			return row, false
		}
	}
	taken := func(key string) bool {
		return slices.ContainsFunc(x.repos[owner], func(row store.OwnerRepo) bool { return row.Key == key })
	}
	return store.OwnerRepo{Owner: owner, Key: config.RepoKey(r.Name, taken), Repo: r.FullName}, true
}

// putRepoLocked writes the row and keeps it in the cache.
func (x *Index) putRepoLocked(ctx context.Context, r store.OwnerRepo) error {
	if err := x.st.PutOwnerRepo(ctx, r); err != nil {
		return err
	}
	rows := x.repos[r.Owner]
	if i := slices.IndexFunc(rows, func(o store.OwnerRepo) bool { return o.Key == r.Key }); i >= 0 {
		rows[i] = r
	} else {
		rows = append(rows, r)
		slices.SortFunc(rows, func(a, b store.OwnerRepo) int { return strings.Compare(a.Key, b.Key) })
	}
	x.repos[r.Owner] = rows
	return nil
}

// goneAllLocked makes gone every live child of the parent.
func (x *Index) goneAllLocked(ctx context.Context, parent string) error {
	var errs []error
	for _, c := range slices.Clone(x.rows[parent]) {
		if c.State == config.ChildLive {
			errs = append(errs, x.goneLocked(ctx, c))
		}
	}
	return errors.Join(errs...)
}

// sweepReposLocked removes the rows of the repos that are out of the job
// set and have no child left, and of the owner jobs that are gone from the
// job files.
func (x *Index) sweepReposLocked(ctx context.Context, now time.Time) error {
	var errs []error
	for owner, rows := range x.repos {
		job := x.base.Jobs[owner]
		for _, r := range rows {
			if job != nil && job.IsOwner() && repoInSet(job.Owner, r, now) {
				continue
			}
			errs = append(errs, x.st.DeleteOwnerRepo(ctx, owner, r.Key))
		}
	}
	return errors.Join(errs...)
}

// Overlap is a repo that more than one job indexes: a mistake of the job
// files' author, which kipp does not settle. Each job builds the repo.
type Overlap struct {
	Repo string
	Jobs []string
}

// Overlaps are the repos that an owner job indexes and another job does
// too, an owner job's or a multibranch job's, sorted by repo.
func (x *Index) Overlaps() []Overlap {
	x.mu.Lock()
	defer x.mu.Unlock()
	by := map[string][]string{}
	hasOwner := map[string]bool{}
	for _, name := range x.base.Names {
		j := x.base.Jobs[name]
		switch {
		case j.IsParent():
			by[strings.ToLower(j.Multibranch.Repo)] = append(by[strings.ToLower(j.Multibranch.Repo)], name)
		case j.IsOwner():
			for _, r := range x.repos[name] {
				if r.State != config.RepoLive {
					continue
				}
				key := strings.ToLower(r.Repo)
				by[key] = append(by[key], name)
				hasOwner[key] = true
			}
		}
	}
	var out []Overlap
	for repo, jobs := range by {
		if len(jobs) > 1 && hasOwner[repo] {
			out = append(out, Overlap{Repo: repo, Jobs: jobs})
		}
	}
	slices.SortFunc(out, func(a, b Overlap) int { return strings.Compare(a.Repo, b.Repo) })
	return out
}

func (o Overlap) String() string {
	return fmt.Sprintf("repo %s is indexed by %s: each of them builds it", o.Repo, strings.Join(o.Jobs, " and "))
}
