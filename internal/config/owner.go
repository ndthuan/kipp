package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The states of a repo of an owner job: live is indexed, gone left the
// set (archived, excluded, deleted, or its stages file went away) and is
// kept for orphans.days like a gone child.
const (
	RepoLive = "live"
	RepoGone = "gone"
)

// Owner is a job that indexes every repo of one GitHub owner that holds
// the stages file at its default branch. Each such repo is a node, a
// multibranch job made from the owner's settings, named <owner job>.<key>;
// the nodes' children are the branches and pull requests, as in a
// multibranch job. The job file keeps what a repo may not grant itself:
// secrets, locks, cron and the rest.
type Owner struct {
	Login string
	// Token names the kipp secret the GitHub API is called with; empty
	// when App is how kipp signs in.
	Token string
	// App is the GitHub App kipp signs in as instead of with a token.
	App *App
	// Include and Exclude are repo names and path.Match patterns, read
	// without regard to case; no include is every repo.
	Include, Exclude []string
	// Clone is the URL of a repo's clone with one %s for owner/name.
	Clone string
	// Scan is how often the owner is scanned; 0 is never.
	Scan time.Duration
	// Cron is the owner job's cron, which builds the default branch of
	// every repo: the owner job itself builds nothing.
	Cron string
	// Settings is what every repo's multibranch job starts from: its path,
	// branches, pull request mode and the rest. Repo and Clone are filled
	// in per repo.
	Settings Multibranch
	// Repos are the per repo settings, by the repo's lower-cased name.
	Repos map[string]RepoSettings
}

// RepoSettings is what a repo has other than the owner job's: nil leaves
// what the job gives, and a list given, even an empty one, replaces it.
// Env is added to the job's; Metrics replaces the job's where given.
type RepoSettings struct {
	Locks, Secrets []string
	Env            map[string]string
	Metrics        *JobMetrics
}

// ownerFile is the YAML shape of owner:.
type ownerFile struct {
	Login         string   `yaml:"login"`
	Token         string   `yaml:"token"`
	App           *appFile `yaml:"app"`
	Include       []string `yaml:"include"`
	Exclude       []string `yaml:"exclude"`
	Clone         string   `yaml:"clone"`
	Path          string   `yaml:"path"`
	Branches      []string `yaml:"branches"`
	PullRequests  string   `yaml:"pull_requests"`
	BranchWithPR  string   `yaml:"branch_with_pr"`
	RebuildOnBase bool     `yaml:"rebuild_on_base"`
	Scan          *string  `yaml:"scan"`
	Orphans       *struct {
		Days *int `yaml:"days"`
	} `yaml:"orphans"`
	Repos map[string]repoSettingsFile `yaml:"repos"`
}

type repoSettingsFile struct {
	Locks   []string          `yaml:"locks"`
	Secrets []string          `yaml:"secrets"`
	Env     map[string]string `yaml:"env"`
	Metrics *JobMetrics       `yaml:"metrics"`
}

// App is a GitHub App kipp signs in as: its ID and the file its private
// key is in. Name is what the App is called to kipp: the route its webhook
// is delivered to is /api/v1/hooks/github-app/<name>, and the secret the webhook
// is signed with is named from it.
type App struct {
	ID      int64
	KeyFile string
	Name    string
}

// appFile is owner.app.
type appFile struct {
	ID      int64  `yaml:"id"`
	KeyFile string `yaml:"key_file"`
	Name    string `yaml:"name"`
}

var appNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// CheckAppName is nil when name can name a GitHub App in kipp: it is a
// part of a route and of a secret's name.
func CheckAppName(name string) error {
	if !appNameRe.MatchString(name) {
		return fmt.Errorf("%q: lower-case letters, digits and - , starting with a letter or a digit", name)
	}
	return nil
}

// app reads owner.app. The name is the key file's, without its directory
// and its extension, unless the file gives one.
func (f *appFile) app() (*App, error) {
	name := f.Name
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(f.KeyFile), filepath.Ext(f.KeyFile))
	}
	name = strings.ToLower(name)
	switch {
	case f.ID <= 0:
		return nil, errors.New("owner.app.id: the App's ID, a number like 123456")
	case f.KeyFile == "" || !filepath.IsAbs(f.KeyFile):
		return nil, fmt.Errorf("owner.app.key_file %q: the absolute path of the App's private key", f.KeyFile)
	case !appNameRe.MatchString(name):
		return nil, fmt.Errorf("owner.app.name %q: lower-case letters, digits and - (kipp takes the key file's name when none is given)", name)
	}
	return &App{ID: f.ID, KeyFile: filepath.Clean(f.KeyFile), Name: name}, nil
}

var loginRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]*$`)

// owner reads owner: with its defaults filled in.
func (f *ownerFile) owner() (*Owner, error) {
	switch {
	case !loginRe.MatchString(f.Login):
		return nil, fmt.Errorf("owner.login %q: a GitHub user or organization, like acme", f.Login)
	case f.Token != "" && f.App != nil:
		return nil, errors.New("owner: give token or app, not both")
	case f.App == nil && f.Token == "":
		return nil, errors.New("owner.token: the name of the kipp secret the GitHub API is called with, or owner.app")
	}
	var app *App
	if f.App != nil {
		var err error
		if app, err = f.App.app(); err != nil {
			return nil, err
		}
	}
	mf := multibranchFile{Path: f.Path, Branches: f.Branches, PullRequests: f.PullRequests, BranchWithPR: f.BranchWithPR,
		RebuildOnBase: f.RebuildOnBase, Scan: f.Scan, Orphans: f.Orphans, Token: f.Token}
	m, err := mf.settings("owner")
	if err != nil {
		return nil, err
	}
	o := &Owner{Login: f.Login, Token: f.Token, App: app, Include: f.Include, Exclude: f.Exclude, Clone: f.Clone,
		Scan: m.Scan, Settings: *m, Repos: map[string]RepoSettings{}}
	if o.Clone == "" {
		o.Clone = defaultCloneFormat
	}
	o.Settings.Scan = 0 // the owner scans its repos, and a repo's job does not scan itself
	return o, o.check(f)
}

// check refuses what no build could honour.
func (o *Owner) check(f *ownerFile) error {
	probe := o.Settings
	probe.Clone = fmt.Sprintf(o.Clone, "acme/site")
	if strings.Count(o.Clone, "%s") != 1 || strings.Contains(strings.ReplaceAll(o.Clone, "%s", ""), "%") {
		return fmt.Errorf("owner.clone %q: a URL with one %%s for owner/name, like https://github.com/%%s.git", RedactURL(o.Clone))
	}
	if err := probe.check("owner"); err != nil {
		return err
	}
	for _, p := range slicesConcat(o.Include, o.Exclude) {
		if _, err := path.Match(strings.ToLower(p), ""); err != nil || p == "" {
			return fmt.Errorf("owner.include and owner.exclude: %q is not a name or a pattern path.Match reads", p)
		}
	}
	for name, r := range f.Repos {
		if err := r.check(name); err != nil {
			return err
		}
		o.Repos[strings.ToLower(name)] = RepoSettings(r)
	}
	return nil
}

// check refuses a repo's settings that name nothing a build could use.
func (r repoSettingsFile) check(name string) error {
	if !repoNameRe.MatchString(name) {
		return fmt.Errorf("owner.repos: %q is not a repo name, like site (the owner is not part of it)", name)
	}
	for _, s := range r.Secrets {
		if !ValidEnvName(s) {
			return fmt.Errorf("owner.repos.%s.secrets: %q is not a secret name", name, s)
		}
	}
	for k := range r.Env {
		if !ValidEnvName(k) {
			return fmt.Errorf("owner.repos.%s.env: %q is not a variable name", name, k)
		}
	}
	if m := r.Metrics; m != nil {
		probe := Job{Name: name, Metrics: m}
		if err := probe.checkMetrics(); err != nil {
			return fmt.Errorf("owner.repos.%s.%w", name, err)
		}
	}
	return nil
}

var repoNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func slicesConcat(a, b []string) []string { return append(append([]string{}, a...), b...) }

// Matches reports whether the repo called name, without its owner, is in
// the job's set by its include and exclude patterns.
func (o *Owner) Matches(name string) bool {
	name = strings.ToLower(name)
	match := func(patterns []string) bool {
		for _, p := range patterns {
			if ok, err := path.Match(strings.ToLower(p), name); err == nil && ok {
				return true
			}
		}
		return false
	}
	return (len(o.Include) == 0 || match(o.Include)) && !match(o.Exclude)
}

// RepoRecord is what kipp keeps of one repo of an owner job.
type RepoRecord struct {
	Owner, Key, Repo, DefaultBranch string
	State                           string // RepoLive or RepoGone
	GoneAt, LastSeenAt              time.Time
}

// Name is the node's job name.
func (r RepoRecord) Name() string { return r.Owner + "." + r.Key }

// RepoKey is the key of a repo's node: its name lower-cased, with every
// character outside [a-z0-9_-] as a dash, and the first 6 hex digits of
// the SHA-256 of the name after it when that changed anything or when
// taken says another repo has it. A key holds no dot, so that a node's
// name and a child's never meet: acme.a.b is the branch b of acme.a and
// never the node of a repo called a.b.
func RepoKey(name string, taken func(string) bool) string {
	key := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			return r
		}
		return '-'
	}, strings.ToLower(name))
	if key == name && !taken(key) {
		return key
	}
	sum := sha256.Sum256([]byte(name))
	return key + "-" + hex.EncodeToString(sum[:])[:6]
}

// IsOwner reports whether the job is an owner job of a file, which
// builds through its repos' nodes.
func (j *Job) IsOwner() bool { return j.Owner != nil }

// Node is the multibranch job a live or gone repo of this owner job is:
// everything of the owner job but its cron, which is the repo's, and its
// webhook, which is the owner job's. A gone repo's node is disabled, and so
// are its children.
func (j *Job) Node(rec RepoRecord) *Job {
	o := j.Owner
	n := *j
	n.Name = rec.Name()
	n.Folder = j.Name
	n.Owner = nil
	n.Permissions = nil
	n.Triggers = Triggers{Cron: o.Cron}
	n.Gone = j.Gone || rec.State == RepoGone
	n.Disabled = j.Disabled || n.Gone
	m := o.Settings
	m.Repo, m.Clone, m.Token, m.App = rec.Repo, fmt.Sprintf(o.Clone, rec.Repo), o.Token, o.App
	n.Multibranch = &m
	_, name, _ := strings.Cut(rec.Repo, "/")
	if r, ok := o.Repos[strings.ToLower(name)]; ok {
		if r.Locks != nil {
			n.Locks = r.Locks
		}
		if r.Secrets != nil {
			n.Secrets = r.Secrets
		}
		if len(r.Env) > 0 {
			n.Env = mergeEnv(j.Env, r.Env)
		}
		if r.Metrics != nil {
			n.Metrics = r.Metrics
		}
	}
	return &n
}

func mergeEnv(base, over map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(over))
	maps.Copy(out, base)
	maps.Copy(out, over)
	return out
}

// setOwner reads owner:, which takes the place of stages and source.
func (j *Job) setOwner(f *ownerFile, hasStages bool) error {
	if f == nil {
		return nil
	}
	if j.Multibranch != nil {
		return errors.New("owner and multibranch: a job is one of them")
	}
	o, err := f.owner()
	if err != nil {
		return err
	}
	switch {
	case hasStages || j.Source != nil:
		return errors.New("owner: the stages are each repo's, in owner.path; an owner job has no stages or source")
	case j.Triggers.Webhook != nil:
		return errors.New("owner: the job's webhook is its own, POST /api/v1/hooks/github/" + j.Name + "; it has no triggers.webhook")
	case len(j.Triggers.After) > 0:
		return errors.New("owner: it builds nothing itself, so triggers.after has nothing to follow")
	case j.Workdir != "" && !filepath.IsLocal(j.Workdir):
		return fmt.Errorf("workdir %q: with owner it is a path inside each clone, like ci or services/api", j.Workdir)
	}
	// The cron is each repo's: the owner job fires none of its own.
	o.Cron, j.Triggers.Cron = j.Triggers.Cron, ""
	j.Owner = o
	return nil
}
