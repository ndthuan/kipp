package api

import (
	"context"
	"maps"
	"slices"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/storage"
	"github.com/ndthuan/kipp/internal/store"
	"github.com/ndthuan/kipp/internal/webhook"
)

// defaultAudit is how many audit rows a page shows.
const defaultAudit = 100

// UserInfo is what an admin sees of a user: never a password's hash or a
// token itself.
type UserInfo struct {
	Name        string
	Admin       bool
	LastLoginAt *time.Time
	// Grants are by job, the ones no longer loaded too; GoneGrants names
	// those, sorted.
	Grants     map[string]store.Perm
	GoneGrants []string
	Tokens     []*store.Token
	// Sessions is how many live browser sessions the user has.
	Sessions int
}

// Users are every user by name, for an admin; a token may read them.
func (s *Service) Users(ctx context.Context, u *store.User) ([]UserInfo, error) {
	if !u.Admin {
		return nil, ErrForbidden
	}
	all, err := s.deps.Store.Users(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]UserInfo, 0, len(all))
	for _, who := range all {
		info, err := s.userInfo(ctx, who)
		if err != nil {
			return nil, err
		}
		out = append(out, *info)
	}
	return out, nil
}

// User is one user, for an admin.
func (s *Service) User(ctx context.Context, u *store.User, name string) (*UserInfo, error) {
	if !u.Admin {
		return nil, ErrForbidden
	}
	who, err := s.someone(ctx, name)
	if err != nil {
		return nil, err
	}
	return s.userInfo(ctx, who)
}

func (s *Service) userInfo(ctx context.Context, who *store.User) (*UserInfo, error) {
	info := &UserInfo{Name: who.Name, Admin: who.Admin, LastLoginAt: who.LastLoginAt, GoneGrants: []string{}}
	var err error
	if info.Grants, err = s.deps.Store.Permissions(ctx, who.ID); err != nil {
		return nil, err
	}
	loaded := s.deps.Dispatch.Snapshot().Jobs
	for _, job := range slices.Sorted(maps.Keys(info.Grants)) {
		if loaded[job] == nil {
			info.GoneGrants = append(info.GoneGrants, job)
		}
	}
	if info.Tokens, err = s.deps.Store.Tokens(ctx, who.ID); err != nil {
		return nil, err
	}
	if info.Sessions, err = s.deps.Store.CountSessions(ctx, who.ID); err != nil {
		return nil, err
	}
	return info, nil
}

// SecretRow is a secret's name, when it was last set (nil for one that is
// not), and the loaded jobs that ask for it, sorted. Webhook is a name a
// job's webhook trigger checks its pushes with.
type SecretRow struct {
	Name string
	// Kind is text, file or userpass; empty for a name no secret holds.
	// User is a userpass secret's user, never its password.
	Kind, User string
	// Scopes are the jobs that may list the secret; empty for a name no
	// secret holds. Narrow marks a global secret that exactly one job
	// lists, which could be scoped to it.
	Scopes    []string
	Narrow    bool
	UpdatedAt *time.Time
	Jobs      []string
	Webhook   bool
}

// Secrets are what an admin sees of the secrets: every one that is set,
// and every name a loaded job asks for that none holds, each by name.
// OutOfScope are the secrets that are set but whose scopes do not reach a
// job that lists them, with those jobs: their builds fail.
type Secrets struct {
	Set, Missing, OutOfScope []SecretRow
}

// Secrets is never a value, for an admin; a token may read it.
func (s *Service) Secrets(ctx context.Context, u *store.User) (*Secrets, error) {
	if !u.Admin {
		return nil, ErrForbidden
	}
	infos, err := s.deps.Store.SecretInfos(ctx)
	if err != nil {
		return nil, err
	}
	asked := s.askedSecrets()
	listing := s.listingJobs()
	out := &Secrets{Set: []SecretRow{}, Missing: []SecretRow{}, OutOfScope: []SecretRow{}}
	for _, info := range infos {
		row := asked[info.Name]
		delete(asked, info.Name)
		row.Name, row.UpdatedAt, row.Kind, row.User, row.Scopes = info.Name, &info.UpdatedAt, info.Kind, info.User, info.Scopes
		if row.Jobs == nil {
			row.Jobs = []string{}
		}
		var unreached []string
		for _, job := range listing[info.Name] {
			if !config.ScopesReach(info.Scopes, job) {
				unreached = append(unreached, jobWho(job))
			}
		}
		if len(unreached) > 0 {
			slices.Sort(unreached)
			out.OutOfScope = append(out.OutOfScope, SecretRow{Name: info.Name, Kind: info.Kind, Scopes: info.Scopes,
				UpdatedAt: &info.UpdatedAt, Jobs: slices.Compact(unreached)})
		}
		row.Narrow = slices.Equal(info.Scopes, []string{config.ScopeGlobal}) && len(listing[info.Name]) == 1
		out.Set = append(out.Set, row)
	}
	for _, name := range slices.Sorted(maps.Keys(asked)) {
		out.Missing = append(out.Missing, asked[name])
	}
	return out, nil
}

// askedSecrets are the names the loaded jobs ask for, with the jobs that
// do: those in a job's secrets, the one its webhook trigger or, for a
// multibranch job, its own webhook is signed with, and the token a
// multibranch job calls GitHub with.
func (s *Service) askedSecrets() map[string]SecretRow {
	snap := s.deps.Dispatch.Snapshot()
	out := map[string]SecretRow{}
	add := func(name, job string, hook bool) {
		row := out[name]
		row.Name = name
		row.Webhook = row.Webhook || hook
		if !slices.Contains(row.Jobs, job) {
			row.Jobs = append(row.Jobs, job)
		}
		out[name] = row
	}
	for _, name := range snap.Names {
		job := snap.Jobs[name]
		if job.Parent != "" {
			continue // a child asks for what its parent does
		}
		askJobSecrets(add, job)
	}
	for name, row := range out {
		slices.Sort(row.Jobs)
		out[name] = row
	}
	return out
}

// jobWho is the name a job is shown by: a repo of an owner job by its owner
// job's.
func jobWho(job *config.Job) string {
	if job.Folder != "" {
		return job.Folder
	}
	return job.Name
}

// listingJobs are the loaded jobs that list each secret in secrets:, the
// jobs askedSecrets counts: a child lists what its parent does.
func (s *Service) listingJobs() map[string][]*config.Job {
	snap := s.deps.Dispatch.Snapshot()
	out := map[string][]*config.Job{}
	for _, name := range snap.Names {
		job := snap.Jobs[name]
		if job.Parent != "" {
			continue
		}
		for _, secret := range job.Secrets {
			out[secret] = append(out[secret], job)
		}
	}
	return out
}

// askJobSecrets says to add what one job of a file, or a repo of an owner
// job, asks for. A repo asks for what its owner job does: it has no webhook
// of its own, and its token is the owner job's.
func askJobSecrets(add func(name, job string, hook bool), job *config.Job) {
	who := jobWho(job)
	for _, secret := range job.Secrets {
		add(secret, who, false)
	}
	if job.IsOwner() && job.Owner.App != nil {
		// An App has one webhook of its own, and no token.
		add(webhook.AppSecretName(job.Owner.App.Name), job.Name, true)
		return
	}
	if job.Folder == "" && (job.Triggers.Webhook != nil || job.IsParent() || job.IsOwner()) {
		add(webhook.SecretName(job.Name), job.Name, true)
	}
	if job.IsParent() && job.Multibranch.Token != "" {
		add(job.Multibranch.Token, who, false)
	}
	if job.IsOwner() {
		add(job.Owner.Token, job.Name, false)
	}
}

// ServerInfo is what the server runs with: its build, the flags serve was
// given, and the state of its data directory and job files.
type ServerInfo struct {
	Version, GoVersion string
	Schema             int
	StartedAt          time.Time
	Listen, BaseURL    string
	// Executors is the server's own stages at once, 0 for none; Builds the
	// builds with something running at once.
	Executors, Builds int
	// Where stages on runners come from and what they get: the labels of a
	// stage that names none on a server of no executors, the variables of
	// the server's they are given, the URL a container's runner dials, and
	// the listener of /runner/ apart from Listen.
	DefaultRunsOn, RunnerEnv []string
	RunnerURL, RunnerListen  string
	Shell                    string
	SessionTTL               time.Duration
	TimeZone                 string
	DataDir, JobsDir         string
	// FreeDisk is the bytes free to kipp on the data directory's disk,
	// when FreeDiskKnown; DBSize is the database file's size, -1 when it
	// cannot be read.
	FreeDisk      uint64
	FreeDiskKnown bool
	DBSize        int64
	// LastReloadAt is when the job files were last read, at the start
	// or by a reload; LastReloadError is why that read was refused, empty
	// when it was taken.
	LastReloadAt    time.Time
	LastReloadError string
	// Jobs is how many jobs are loaded.
	Jobs int
	// Origin is where the running binary came from, for people to read.
	Origin string
	// UpdateCheck is whether the server looks for newer releases at all.
	UpdateCheck bool
	// Latest is the newest release when it is newer than Version, else
	// empty; NotesURL is its page, Upgradable whether it carries the
	// signature file, and CheckedAt when it was last looked up, zero before
	// any answer.
	Latest, NotesURL string
	Upgradable       bool
	CheckedAt        time.Time
	// SelfCheck is the last self-check of the database, nil before any.
	SelfCheck *storage.Check
}

// ServerInfo is for an admin; a token may read it.
func (s *Service) ServerInfo(_ context.Context, u *store.User) (*ServerInfo, error) {
	if !u.Admin {
		return nil, ErrForbidden
	}
	info := s.deps.ServerInfo()
	info.Jobs = len(s.deps.Dispatch.Snapshot().Names)
	if s.deps.Storage != nil {
		info.SelfCheck = s.deps.Storage.LastCheck()
	}
	return &info, nil
}

// Audit is a page of the audit, newest first: with before, the rows older
// than that one; 100 unless limit says otherwise, 500 at most. More says
// there are rows beyond the page.
func (s *Service) Audit(ctx context.Context, u *store.User, before int64, limit int) (page []*store.Audit, more bool, err error) {
	if !u.Admin {
		return nil, false, ErrForbidden
	}
	n := rows(limit, defaultAudit)
	found, err := s.deps.Store.AuditBefore(ctx, before, n+1)
	if err != nil {
		return nil, false, err
	}
	page, more = trim(found, n)
	return page, more, nil
}
