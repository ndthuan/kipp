package index

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/github"
	"github.com/ndthuan/kipp/internal/store"
)

// scanOwner reads the owner's repos, keeps their webhooks, and scans each
// live repo as a multibranch job is scanned. A repo whose scan failed does
// not stop the others; the errors are the scan's together.
func (s *Scanner) scanOwner(ctx context.Context, name string, o *config.Owner) (string, error) {
	gh, err := clientFor(ctx, s.api, s.opts.Token, s.opts.App, o.App, o.Token, o.Login)
	if err != nil {
		return "", err
	}
	disc, err := s.ix.DiscoverRepos(ctx, name, gh)
	if disc == nil {
		return "", err
	}
	errs := []error{err}
	s.mu.Lock()
	s.stateLocked(name).publicOnly = disc.PublicOnly
	s.mu.Unlock()
	if disc.PublicOnly {
		s.opts.Log.Warn("owner scan: another user's account lists its public repos only", "job", name, "owner", o.Login)
	}
	hooks := ""
	if o.App == nil { // an App has one webhook for every repo it is installed on
		hooks = s.keepHooks(ctx, name, gh)
	}
	total := repoScan{}
	for _, node := range disc.Nodes {
		res, err := s.scanRepo(ctx, node, gh)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", node, err))
		}
		total.queued += res.queued
		total.dropped += res.dropped
	}
	s.warnOverlaps()
	total.summary = fmt.Sprintf("%d repos (%d new, %d gone)%s", len(disc.Nodes), disc.Added, disc.Gone, hooks)
	return total.text(), errors.Join(errs...)
}

// keepHooks makes the webhooks of the owner's repos what they are meant to
// be, says in the log what is new about a repo whose hook it cannot keep,
// and returns a summary of how many it keeps.
func (s *Scanner) keepHooks(ctx context.Context, name string, gh *github.Client) string {
	if s.opts.Hook == nil {
		return ""
	}
	changes, err := s.ix.ManageHooks(ctx, name, gh, s.opts.Hook(ctx, name))
	if err != nil {
		s.opts.Log.Warn("owner scan: keeping a webhook", "job", name, "err", err)
	}
	kept := 0
	for _, c := range changes {
		switch {
		case c.Hook == store.HookOK:
			kept++
		case c.Changed:
			s.opts.Log.Warn("owner scan: no webhook kept, scanning instead", "job", name, "repo", c.Repo, "why", c.Note)
		}
	}
	return fmt.Sprintf(", %d of %d with a webhook", kept, len(changes))
}

// warnOverlaps says in the log each repo that more than one job indexes
// when it first does, and forgets the ones that stopped, so that a mistake
// is said once and said again if it comes back.
func (s *Scanner) warnOverlaps() {
	now := map[string]bool{}
	for _, o := range s.ix.Overlaps() {
		text := o.String()
		now[text] = true
		s.mu.Lock()
		seen := s.warned[text]
		s.warned[text] = true
		s.mu.Unlock()
		if !seen {
			s.opts.Log.Warn("two jobs index one repo", "repo", o.Repo, "jobs", strings.Join(o.Jobs, ","), "why", text)
		}
	}
	s.mu.Lock()
	for text := range s.warned {
		if !now[text] {
			delete(s.warned, text)
		}
	}
	s.mu.Unlock()
}

// OwnerStatus is what the page of an owner job shows beside its scan.
type OwnerStatus struct {
	Scan ScanStatus
	// Repos are the owner job's repos, live ones and gone ones.
	Repos []RepoStatus
	// App is the name of the GitHub App the owner job signs in as, empty
	// for a token.
	App string
	// OldHooks are the repos that carry a webhook kipp made and no longer keeps.
	OldHooks []string
	// PublicOnly says the last read could not list private repos.
	PublicOnly bool
	// Overlaps are the repos this owner job shares with another job.
	Overlaps []Overlap
}

// RepoStatus is one repo of an owner job.
type RepoStatus struct {
	Node, Repo, State string
	// Hook is "ok" where kipp keeps the repo's webhook and "off" where it
	// cannot, and Note says why.
	Hook, Note string
}

// Owner is how the owner job stands, nil when name is none.
func (s *Scanner) Owner(name string) *OwnerStatus {
	if s.ix.Owner(name) == nil {
		return nil
	}
	out := &OwnerStatus{Scan: s.Status(name), OldHooks: s.ix.OldHooks(name)}
	if a := s.ix.Owner(name).App; a != nil {
		out.App = a.Name
	}
	s.mu.Lock()
	if st := s.state[name]; st != nil {
		out.PublicOnly = st.publicOnly
	}
	s.mu.Unlock()
	for _, r := range s.ix.Repos(name) {
		out.Repos = append(out.Repos, RepoStatus{Node: r.Node(), Repo: r.Repo, State: r.State, Hook: r.Hook, Note: r.HookNote})
	}
	for _, o := range s.ix.Overlaps() {
		for _, j := range o.Jobs {
			if j == name {
				out.Overlaps = append(out.Overlaps, o)
			}
		}
	}
	return out
}
