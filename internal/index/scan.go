package index

import (
	"context"
	"fmt"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/dispatch"
	"github.com/ndthuan/kipp/internal/github"
	"github.com/ndthuan/kipp/internal/store"
)

// ScanReport is what a scan found of the repo, and the builds it plans:
// a head that no build has seen.
type ScanReport struct {
	Builds  []dispatch.TriggerRequest
	Summary string
}

// found is what GitHub said of the repo, read before anything is changed.
type found struct {
	def      string
	branches []github.Branch    // the ones built, with the file at their head
	pulls    []PullRequestEvent // the open ones; a fork's carry no file check
}

// Scan reads the parent's repo from GitHub, makes its children live where
// the stages file exists at the head, makes gone the ones it no longer
// finds, and plans a build with trigger scan for every head that no build
// has seen: a delivery that was missed is caught by the next scan. Nothing
// is changed unless every call to GitHub answered.
func (x *Index) Scan(ctx context.Context, parent string, gh *github.Client) (*ScanReport, error) {
	x.mu.Lock()
	p, err := x.parentLocked(parent)
	x.mu.Unlock()
	if err != nil {
		return nil, err
	}
	m := p.Multibranch
	fnd, err := readRepo(ctx, gh, m)
	if err != nil {
		return nil, err
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.applyLocked(ctx, parent, m, fnd)
}

// readRepo asks GitHub for the repo, its candidate branches and pull
// requests, and whether each has the stages file at its head.
func readRepo(ctx context.Context, gh *github.Client, m *config.Multibranch) (*found, error) {
	def, err := gh.Repo(ctx, m.Repo)
	if err != nil {
		return nil, err
	}
	f := &found{def: def}
	if f.branches, err = branchesOf(ctx, gh, m, def); err != nil {
		return nil, err
	}
	if m.PullRequests == config.PROff {
		return f, nil
	}
	pulls, err := gh.OpenPulls(ctx, m.Repo)
	if err != nil {
		return nil, err
	}
	for _, pull := range pulls {
		ev := PullRequestEvent{Repo: m.Repo, DefaultBranch: def, Number: pull.Number, Title: pull.Title, Author: pull.Author, URL: pull.URL,
			HeadRef: pull.HeadRef, HeadSHA: pull.HeadSHA, HeadRepo: pull.HeadRepo, BaseRef: pull.BaseRef}
		if !ev.fromFork() {
			// A fork's pull request is listed and never built, so its
			// head is not read at all.
			ok, err := gh.HasFile(ctx, m.Repo, m.Path, pull.HeadSHA)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		f.pulls = append(f.pulls, ev)
	}
	return f, nil
}

// branchesOf are the branches the job builds that have the stages file at
// their head. The repo's branches are listed only when branches names more
// than the default one; otherwise the default branch is asked for alone.
func branchesOf(ctx context.Context, gh *github.Client, m *config.Multibranch, def string) ([]github.Branch, error) {
	var all []github.Branch
	if m.NamesBranches() {
		var err error
		if all, err = gh.Branches(ctx, m.Repo); err != nil {
			return nil, err
		}
	} else {
		sha, err := gh.Branch(ctx, m.Repo, def)
		if err != nil {
			return nil, err
		}
		all = []github.Branch{{Name: def, SHA: sha}}
	}
	var out []github.Branch
	for _, b := range all {
		if !m.Selects(b.Name, def) {
			continue
		}
		ok, err := gh.HasFile(ctx, m.Repo, m.Path, b.SHA)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, b)
		}
	}
	return out, nil
}

// applyLocked makes the children what the scan found, and plans the builds.
func (x *Index) applyLocked(ctx context.Context, parent string, m *config.Multibranch, f *found) (*ScanReport, error) {
	if _, err := x.parentLocked(parent); err != nil {
		return nil, err // a reload took it away meanwhile
	}
	if err := x.setDefaultLocked(ctx, parent, f.def); err != nil {
		return nil, err
	}
	defer x.publishLocked()
	seen := map[string]bool{}
	// The pull requests first: a branch with an open one is theirs to build.
	builds, forks, err := x.scanPullsLocked(ctx, m, parent, f.pulls, seen)
	if err != nil {
		return nil, err
	}
	for _, b := range f.branches {
		req, err := x.scanBranchLocked(ctx, m, parent, b, seen)
		if err != nil {
			return nil, err
		}
		if req != nil {
			builds = append(builds, *req)
		}
	}
	gone, err := x.goneUnseenLocked(ctx, parent, seen)
	if err != nil {
		return nil, err
	}
	unseen, err := x.unseenHeadsLocked(ctx, builds)
	if err != nil {
		return nil, err
	}
	for i := range unseen {
		unseen[i].Kind, unseen[i].By = "scan", "scan"
	}
	summary := fmt.Sprintf("%d branches, %d pull requests (%d from forks, not built), %d gone", len(f.branches), len(f.pulls), forks, gone)
	return &ScanReport{Builds: unseen, Summary: summary}, nil
}

// scanPullsLocked makes the children of the open pull requests live, and
// plans their builds; it counts the ones from a fork, which have none.
func (x *Index) scanPullsLocked(ctx context.Context, m *config.Multibranch, parent string, pulls []PullRequestEvent, seen map[string]bool) ([]dispatch.TriggerRequest, int, error) {
	var builds []dispatch.TriggerRequest
	forks := 0
	for _, ev := range pulls {
		plan, err := x.openLocked(ctx, m, parent, ev)
		if err != nil {
			return nil, 0, err
		}
		for _, key := range prKeys(m, ev) {
			seen[key] = true
		}
		if ev.fromFork() {
			forks++
		}
		builds = append(builds, plan.Builds...)
	}
	return builds, forks, nil
}

// prKeys are the keys of the children of a pull request.
func prKeys(m *config.Multibranch, ev PullRequestEvent) []string {
	ks := kinds(m.PullRequests)
	if ev.fromFork() {
		ks = ks[:1]
	}
	out := make([]string, len(ks))
	for i, kind := range ks {
		out[i] = config.PRKey(ev.Number, kind, m.PullRequests)
	}
	return out
}

// scanBranchLocked makes the branch's child live at the head the scan saw
// and asks for its build, unless the branch is a pull request's to build.
func (x *Index) scanBranchLocked(ctx context.Context, m *config.Multibranch, parent string, b github.Branch, seen map[string]bool) (*dispatch.TriggerRequest, error) {
	child := x.branchLocked(parent, b.Name)
	if x.skipLocked(m, parent, b.Name) != "" {
		if child != nil {
			seen[child.Key] = true // still there, and not its to build
		}
		return nil, nil
	}
	c, err := x.liveBranchLocked(ctx, parent, b.Name, child, b.SHA)
	if err != nil {
		return nil, err
	}
	seen[c.Key] = true
	return &dispatch.TriggerRequest{Job: c.Job(), Kind: "webhook", Detail: b.SHA, Branch: b.Name, DefaultBranch: x.defaults[parent]}, nil
}

// goneUnseenLocked makes gone every live child the scan did not see, and
// says how many it made gone.
func (x *Index) goneUnseenLocked(ctx context.Context, parent string, seen map[string]bool) (int, error) {
	n := 0
	for _, c := range append([]store.Child(nil), x.rows[parent]...) {
		if c.State != config.ChildLive || seen[c.Key] {
			continue
		}
		if err := x.goneLocked(ctx, c); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// unseenHeadsLocked are the requests whose head differs from the one the
// child's last build built, and that no queued or running build is of. A
// build a webhook or a scan was started for counts as of its head even
// before it cloned, so that one that failed to clone is not tried again
// at every scan. A delivery that queues a build between this and the
// caller's trigger can make one build twice, which does no harm.
func (x *Index) unseenHeadsLocked(ctx context.Context, reqs []dispatch.TriggerRequest) ([]dispatch.TriggerRequest, error) {
	if len(reqs) == 0 {
		return nil, nil
	}
	last, err := x.st.LastBuilds(ctx)
	if err != nil {
		return nil, err
	}
	active, err := x.st.BuildsWithStatus(ctx, store.Queued, store.Running)
	if err != nil {
		return nil, err
	}
	onItsWay := map[string]bool{}
	for _, b := range active {
		onItsWay[b.Job+"@"+headOf(b)] = true
	}
	var out []dispatch.TriggerRequest
	for _, r := range reqs {
		if headOf(last[r.Job]) == r.Detail || onItsWay[r.Job+"@"+r.Detail] {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// OnItsWay reports whether a build of the job for the commit is queued or
// running: a delivery that asks for it again, by another route or hook of
// the same repo, has nothing to add.
func (x *Index) OnItsWay(ctx context.Context, job, commit string) (bool, error) {
	active, err := x.st.BuildsWithStatus(ctx, store.Queued, store.Running)
	if err != nil {
		return false, err
	}
	for _, b := range active {
		if b.Job == job && (headOf(b) == commit || b.TriggerDetail == commit && b.Trigger == "scan") {
			return true, nil
		}
	}
	return false, nil
}

// headOf is the commit a build is of: the one it checked out, else the one
// its webhook or scan named.
func headOf(b *store.Build) string {
	switch {
	case b == nil:
		return ""
	case b.BuiltCommit != "":
		return b.BuiltCommit
	case b.Commit() != "":
		return b.Commit()
	case b.Trigger == "scan":
		return b.TriggerDetail
	}
	return ""
}
