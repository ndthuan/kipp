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

type found struct {
	def      string
	branches []github.Branch
	pulls    []PullRequestEvent
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

func (x *Index) applyLocked(ctx context.Context, parent string, m *config.Multibranch, f *found) (*ScanReport, error) {
	if _, err := x.parentLocked(parent); err != nil {
		return nil, err
	}
	if err := x.setDefaultLocked(ctx, parent, f.def); err != nil {
		return nil, err
	}
	defer x.publishLocked()
	seen := map[string]bool{}
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

func (x *Index) scanBranchLocked(ctx context.Context, m *config.Multibranch, parent string, b github.Branch, seen map[string]bool) (*dispatch.TriggerRequest, error) {
	child := x.branchLocked(parent, b.Name)
	if x.skipLocked(m, parent, b.Name) != "" {
		if child != nil {
			seen[child.Key] = true
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

func (x *Index) goneUnseenLocked(ctx context.Context, parent string, seen map[string]bool) (int, error) {
	n := 0
	for _, c := range append([]store.Child(nil), x.rows[parent]...) {
		if c.State != config.ChildLive || seen[c.Key] || c.Kind == config.KindTag {
			continue
		}
		if err := x.goneLocked(ctx, c); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

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

// OnItsWay reports whether a build of the job for the commit, and for the
// tag ("" for a branch's), is queued or running: a delivery that asks for
// it again, by another route or hook of the same repo, has nothing to add.
// A delivery that names the base it wants the build merged into ("" for
// none) is not served by a build that merged another: a queued build, and a
// running one that has not cloned yet, merge the base as it is then.
func (x *Index) OnItsWay(ctx context.Context, job, commit, tag, base string) (bool, error) {
	active, err := x.st.BuildsWithStatus(ctx, store.Queued, store.Running)
	if err != nil {
		return false, err
	}
	for _, b := range active {
		if b.Job == job && b.TriggerTag == tag && (headOf(b) == commit || b.TriggerDetail == commit && b.Trigger == "scan") &&
			(base == "" || b.BaseCommit == "" || b.BaseCommit == base) {
			return true, nil
		}
	}
	return false, nil
}

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
