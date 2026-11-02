package api

import (
	"context"
	"slices"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

// ScopeChoices are the scopes an admin picks from: global, then a group:
// scope for each group the loaded jobs name, then a job: scope for each job
// file, each sorted. A child, and a repo of an owner job, is reached
// through the name of the file it comes from, so it is not offered apart.
type ScopeChoices struct {
	Groups, Jobs []string
}

// SecretScopeChoices are the scopes the loaded jobs make meaningful.
func (s *Service) SecretScopeChoices(_ context.Context, u *store.User) (*ScopeChoices, error) {
	if !u.Admin {
		return nil, ErrForbidden
	}
	snap := s.deps.Dispatch.Snapshot()
	out := &ScopeChoices{Groups: []string{}, Jobs: []string{}}
	for _, name := range snap.Names {
		job := snap.Jobs[name]
		if job.Parent != "" || job.Folder != "" {
			continue
		}
		out.Jobs = append(out.Jobs, config.ScopeJobPrefix+name)
		if g := config.ScopeGroupPrefix + job.Group; job.Group != "" && !slices.Contains(out.Groups, g) {
			out.Groups = append(out.Groups, g)
		}
	}
	slices.Sort(out.Groups)
	slices.Sort(out.Jobs)
	return out, nil
}
