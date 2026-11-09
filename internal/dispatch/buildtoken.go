package dispatch

import (
	"fmt"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

// mintToken is the KIPP_TOKEN of a build: a token of the build with the
// grants its job's permissions: name, and none for permissions: none or a
// server that makes none. A job that reads its stages from a repo holds the
// default until the file is read (grantRepoPermissions).
func (d *Dispatcher) mintToken(b *store.Build, job *config.Job) string {
	if d.opts.BuildTokens == nil || job.Permissions.NoToken() {
		return ""
	}
	return d.opts.BuildTokens.Mint(b.ID, job.Name, b.Number, job.Parent, permsOf(job.Grants()))
}

// grantRepoPermissions reads the permissions: of the stages file a build
// cloned. They are what the build asks for beyond its job's, and must stay
// within the job's max_permissions: a build over the ceiling fails before
// its first stage, saying what is over and whose key would allow it, and is
// never held back silently. A file that says none takes the token away, but
// not from a job whose own file gave it none already.
func (d *Dispatcher) grantRepoPermissions(b *store.Build, job *config.Job, e *buildEnv, path string, file *config.Permissions) error {
	if file == nil {
		return nil
	}
	if err := file.CheckSelfBuild(job.Name); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if err := job.RepoPermissions(file); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if e.token == "" || d.opts.BuildTokens == nil {
		return nil
	}
	if file.NoToken() {
		d.opts.BuildTokens.Revoke(b.ID)
		e.token = ""
		return nil
	}
	grants := job.Grants()
	for name, g := range file.Resolve(job.Name) {
		prev := grants[name]
		grants[name] = config.Grant{View: prev.View || g.View, Build: prev.Build || g.Build, Cancel: prev.Cancel || g.Cancel}
	}
	d.opts.BuildTokens.Regrant(b.ID, permsOf(grants))
	return nil
}

func permsOf(grants map[string]config.Grant) map[string]store.Perm {
	out := make(map[string]store.Perm, len(grants))
	for name, g := range grants {
		out[name] = store.Perm{View: g.View, Build: g.Build, Cancel: g.Cancel}
	}
	return out
}
