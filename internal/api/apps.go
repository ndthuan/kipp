package api

import (
	"context"
	"slices"
	"strings"

	"github.com/ndthuan/kipp/internal/github"
	"github.com/ndthuan/kipp/internal/store"
	"github.com/ndthuan/kipp/internal/webhook"
)

// AppUse is a GitHub App the loaded owner jobs sign in as, by its name in
// kipp, whoever made it.
type AppUse struct {
	Name    string
	ID      int64
	KeyFile string
	// Jobs are the owner jobs that name it, by name.
	Jobs []string
	// Secret is the kipp secret its webhook is signed with, and SecretSet
	// whether it holds a value.
	Secret    string
	SecretSet bool
	// KeyProblem says why the key file cannot sign, empty when it can.
	KeyProblem string
}

// OwnerApps are the Apps the loaded owner jobs sign in as, by name. The job
// files hold two jobs that name one App to the same ID and key file, so
// each name has one of each.
func (s *Service) OwnerApps(ctx context.Context, u *store.User) ([]AppUse, error) {
	if !u.Admin {
		return nil, ErrForbidden
	}
	infos, err := s.deps.Store.SecretInfos(ctx)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, i := range infos {
		set[i.Name] = true
	}
	snap := s.deps.Dispatch.Snapshot()
	byName := map[string]*AppUse{}
	for _, name := range snap.Names {
		job := snap.Jobs[name]
		if !job.IsOwner() || job.Owner.App == nil {
			continue
		}
		app := job.Owner.App
		use := byName[app.Name]
		if use == nil {
			secret := webhook.AppSecretName(app.Name)
			use = &AppUse{Name: app.Name, ID: app.ID, KeyFile: app.KeyFile, Secret: secret, SecretSet: set[secret]}
			if err := github.CheckKeyFile(app.KeyFile); err != nil {
				use.KeyProblem = err.Error()
			}
			byName[app.Name] = use
		}
		use.Jobs = append(use.Jobs, name)
	}
	out := make([]AppUse, 0, len(byName))
	for _, use := range byName {
		slices.Sort(use.Jobs)
		out = append(out, *use)
	}
	slices.SortFunc(out, func(a, b AppUse) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}
