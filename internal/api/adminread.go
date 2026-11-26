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

const defaultAudit = 100

// UserInfo is what an admin sees of a user: never a password's hash or a
// token itself.
type UserInfo struct {
	Name        string
	Admin       bool
	LastLoginAt *time.Time
	Grants      map[string]store.Perm
	GoneGrants  []string
	Tokens      []*store.Token
	Sessions    int
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
	Name       string
	Kind, User string
	Scopes     []string
	Narrow     bool
	UpdatedAt  *time.Time
	Jobs       []string
	Webhook    bool
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
			continue
		}
		askJobSecrets(add, job)
	}
	for name, row := range out {
		slices.Sort(row.Jobs)
		out[name] = row
	}
	return out
}

func jobWho(job *config.Job) string {
	if job.Folder != "" {
		return job.Folder
	}
	return job.Name
}

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

func askJobSecrets(add func(name, job string, hook bool), job *config.Job) {
	who := jobWho(job)
	for _, secret := range job.Secrets {
		add(secret, who, false)
	}
	if job.IsOwner() && job.Owner.App != nil {
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
	Version, GoVersion       string
	Schema                   int
	StartedAt                time.Time
	Listen, BaseURL          string
	Executors                int
	DefaultRunsOn, RunnerEnv []string
	RunnerURL, RunnerListen  string
	Shell                    string
	SessionTTL               time.Duration
	TimeZone                 string
	DataDir, JobsDir         string
	FreeDisk                 uint64
	FreeDiskKnown            bool
	DBSize                   int64
	LastReloadAt             time.Time
	LastReloadError          string
	Jobs                     int
	JobEdits                 string
	JobConflicts             int
	Origin                   string
	UpdateCheck              bool
	Latest, NotesURL         string
	Upgradable               bool
	CheckedAt                time.Time
	SelfCheck                *storage.Check
}

// ServerInfo is for an admin; a token may read it.
func (s *Service) ServerInfo(_ context.Context, u *store.User) (*ServerInfo, error) {
	if !u.Admin {
		return nil, ErrForbidden
	}
	info := s.deps.ServerInfo()
	info.Jobs = len(s.deps.Dispatch.Snapshot().Names)
	if s.deps.Jobs != nil {
		info.JobEdits, info.JobConflicts = string(s.deps.Jobs.Mode()), len(s.deps.Jobs.Conflicts())
	}
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
