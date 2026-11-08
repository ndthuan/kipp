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
	"slices"
	"strconv"
	"strings"
	"time"
)

// The defaults of a multibranch job: the default branch and the pull
// requests are built, merged into their base, and nothing else.
const (
	DefaultChildPath   = ".kipp.yml"
	DefaultBranch      = "default"
	DefaultScan        = 30 * time.Minute
	MinScan            = 5 * time.Minute
	DefaultOrphanDays  = 7
	defaultCloneFormat = "https://github.com/%s.git"
)

// The ways a pull request is built (pull_requests).
const (
	PRMerge = "merge"
	PRHead  = "head"
	PRBoth  = "both"
	PROff   = "off"
)

// What a push to a branch that has an open pull request builds
// (branch_with_pr).
const (
	BranchWithPRSkip  = "skip"
	BranchWithPRBuild = "build"
)

// The kinds of a child: a branch, a pull request merged into its base, and
// a pull request's head alone.
const (
	KindBranch = "branch"
	KindPR     = "pr"
	KindPRHead = "pr-head"
)

// The states of a child: live builds, gone builds no more and is listed
// apart until it leaves the job set.
const (
	ChildLive = "live"
	ChildGone = "gone"
)

// Multibranch is a job that builds a repo's branches and pull requests as
// child jobs, each reading its stages from Path in the commit it builds.
// The job file keeps what a repo may not grant itself: secrets, locks,
// cron and the rest.
type Multibranch struct {
	Repo  string // owner/name
	Clone string // the URL a build clones
	Path  string // the stages file inside the repo
	// Branches are "default" (the repo's default branch, whatever it is
	// called), names and path.Match patterns of the branches built without
	// a pull request.
	Branches      []string
	PullRequests  string
	BranchWithPR  string
	RebuildOnBase bool
	// Scan is how often the repo is scanned; 0 is never.
	Scan    time.Duration
	Orphans Orphans
	// Token names the kipp secret the GitHub API is called with; empty
	// is no API at all, and no stage is given it unless secrets names it.
	Token string
	// App is the GitHub App an owner job's repo is called with instead of
	// a token; only an owner job has one.
	App *App
}

// Authenticated reports whether kipp can call GitHub for the repo: it has
// a token to open or an App to sign in as.
func (m *Multibranch) Authenticated() bool { return m.Token != "" || m.App != nil }

// Orphans is how long a gone child stays in the job set.
type Orphans struct {
	Days int
}

// multibranchFile is the YAML shape of multibranch:.
type multibranchFile struct {
	Repo          string   `yaml:"repo"`
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
	Token string `yaml:"token"`
}

var repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// multibranch reads multibranch: with its defaults filled in.
func (f *multibranchFile) multibranch() (*Multibranch, error) {
	if !repoRe.MatchString(f.Repo) {
		return nil, fmt.Errorf("multibranch.repo %q: owner/name, like acme/site", f.Repo)
	}
	m, err := f.settings("multibranch")
	if err != nil {
		return nil, err
	}
	m.Repo, m.Clone = f.Repo, f.Clone
	if m.Clone == "" {
		m.Clone = fmt.Sprintf(defaultCloneFormat, m.Repo)
	}
	return m, m.check("multibranch")
}

// settings are the keys a multibranch job and an owner job share, with
// their defaults; what is the key's parent in an error.
func (f *multibranchFile) settings(what string) (*Multibranch, error) {
	m := &Multibranch{
		Path: f.Path, Branches: f.Branches, PullRequests: f.PullRequests, BranchWithPR: f.BranchWithPR,
		RebuildOnBase: f.RebuildOnBase, Scan: DefaultScan, Orphans: Orphans{Days: DefaultOrphanDays}, Token: f.Token,
	}
	if m.Path == "" {
		m.Path = DefaultChildPath
	}
	if m.Branches == nil {
		m.Branches = []string{DefaultBranch}
	}
	if m.PullRequests == "" {
		m.PullRequests = PRMerge
	}
	if m.BranchWithPR == "" {
		m.BranchWithPR = BranchWithPRSkip
	}
	if f.Orphans != nil && f.Orphans.Days != nil {
		m.Orphans.Days = *f.Orphans.Days
	}
	return m, m.setScan(what, f.Scan)
}

func (m *Multibranch) setScan(what string, scan *string) error {
	if scan == nil {
		return nil
	}
	if *scan == "0" {
		m.Scan = 0
		return nil
	}
	d, err := time.ParseDuration(*scan)
	if err != nil || d < MinScan {
		return fmt.Errorf("%s.scan %q: a duration of at least %s, like 30m, or 0 for no scan", what, *scan, MinScan)
	}
	m.Scan = d
	return nil
}

// check refuses what no build could honour.
func (m *Multibranch) check(what string) error {
	switch {
	case strings.HasPrefix(m.Clone, "-"):
		return fmt.Errorf("%s.clone %q: a URL, which never starts with a dash", what, RedactURL(m.Clone))
	case !filepath.IsLocal(m.Path):
		return fmt.Errorf("%s.path %q: a path inside the repo, like .kipp.yml or ci/kipp.yml", what, m.Path)
	case len(m.Branches) == 0:
		return fmt.Errorf("%s.branches: at least one, like [default]; leave it out for the default branch alone", what)
	case !slices.Contains([]string{PRMerge, PRHead, PRBoth, PROff}, m.PullRequests):
		return fmt.Errorf("%s.pull_requests %q: merge, head, both or off", what, m.PullRequests)
	case !slices.Contains([]string{BranchWithPRSkip, BranchWithPRBuild}, m.BranchWithPR):
		return fmt.Errorf("%s.branch_with_pr %q: skip or build", what, m.BranchWithPR)
	case m.Orphans.Days < 1:
		return fmt.Errorf("%s.orphans.days %d: 1 or more", what, m.Orphans.Days)
	case m.Token != "" && !ValidEnvName(m.Token):
		return fmt.Errorf("%s.token %q: the name of a kipp secret", what, m.Token)
	}
	for _, p := range m.Branches {
		if _, err := path.Match(p, ""); err != nil || p == "" {
			return fmt.Errorf("%s.branches: %q is not default, a name or a pattern path.Match reads", what, p)
		}
	}
	return nil
}

// Selects reports whether a push to branch builds it without a pull
// request: branches names it, or says default and it is the default one.
func (m *Multibranch) Selects(branch, defaultBranch string) bool {
	return slices.ContainsFunc(m.Branches, func(p string) bool {
		if p == DefaultBranch {
			return defaultBranch != "" && branch == defaultBranch
		}
		ok, err := path.Match(p, branch)
		return err == nil && ok
	})
}

// NamesBranches reports whether branches names more than the default
// branch, so that a scan has to list the repo's branches to find them.
func (m *Multibranch) NamesBranches() bool {
	return slices.ContainsFunc(m.Branches, func(p string) bool { return p != DefaultBranch })
}

// Builds reports whether a child of kind is in the job set under the
// job's pull_requests: a branch always is.
func (m *Multibranch) Builds(kind string) bool {
	switch kind {
	case KindPR:
		return m.PullRequests == PRMerge || m.PullRequests == PRBoth
	case KindPRHead:
		return m.PullRequests == PRHead || m.PullRequests == PRBoth
	}
	return true
}

// ChildRecord is what kipp keeps of one child of a multibranch job.
type ChildRecord struct {
	Parent, Key, Kind, Branch string
	PR                        int
	// Title is a pull request's; HeadSHA the commit last seen at its head;
	// BaseBranch the branch a pull request is merged into.
	Title, HeadSHA, BaseBranch string
	// Author is the login that opened the pull request, URL its page.
	Author, URL string
	// FromFork is a pull request whose head is in another repo: listed,
	// never built.
	FromFork bool
	State    string // ChildLive or ChildGone
	// GoneAt is when it became gone; zero while it is live.
	GoneAt, LastSeenAt time.Time
}

// Name is the child's job name.
func (r ChildRecord) Name() string { return r.Parent + "." + r.Key }

// PRKey is the key of a pull request's child of kind.
func PRKey(n int, kind string, mode string) string {
	key := "pr-" + strconv.Itoa(n)
	if kind == KindPRHead && mode == PRBoth {
		key += "-head"
	}
	return key
}

var prKeyRe = regexp.MustCompile(`^pr-[0-9]+(-head)?$`)

// ChildKey is the key of a branch's child: its name lower-cased, with
// every character outside [a-z0-9._-] as a dash, and the first 6 hex
// digits of the SHA-256 of the name after it when that changed anything,
// when the result is a pull request's key, or when taken says another
// child has it. A key is so always a part of a job name, and two branches
// never share one.
func ChildKey(branch string, taken func(string) bool) string {
	key := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			return r
		}
		return '-'
	}, strings.ToLower(branch))
	if key == branch && !prKeyRe.MatchString(key) && !taken(key) {
		return key
	}
	sum := sha256.Sum256([]byte(branch))
	return key + "-" + hex.EncodeToString(sum[:])[:6]
}

// Child is the job a child of this multibranch job runs as: everything of
// the parent but its cron and after, which build the default branch's
// child through the parent, and its webhook, which is the parent's. A gone
// child is disabled: it stays readable and builds no more.
func (j *Job) Child(rec ChildRecord) *Job {
	c := *j
	c.Name = rec.Name()
	c.Parent = j.Name
	c.Origin = &rec
	c.Triggers = Triggers{}
	c.TimeZone = nil
	c.Disabled = j.Disabled || rec.State == ChildGone
	if rec.Kind != KindBranch && rec.Title != "" {
		c.Description = rec.Title
	}
	return &c
}

// IsParent reports whether the job is a multibranch job of a file, which
// builds through its children.
func (j *Job) IsParent() bool { return j.Multibranch != nil && j.Parent == "" }

// IsLeaf reports whether the job builds itself: it is neither a multibranch
// job, an owner job nor a repo's node of one, which only build through their
// children.
func (j *Job) IsLeaf() bool {
	return !j.IsParent() && !j.IsOwner() && (j.Folder == "" || j.Parent != "")
}

// OnDefaultBranch reports whether the job is the child of its repo's
// default branch.
func (j *Job) OnDefaultBranch() bool {
	return j.Origin != nil && j.Origin.Kind == KindBranch && j.DefaultBranch != "" && j.Origin.Branch == j.DefaultBranch
}

// DefaultChild is the child of the parent's default branch in the
// snapshot, nil when there is none yet.
func (s *Snapshot) DefaultChild(parent string) *Job {
	for _, j := range s.Jobs {
		if j.Parent == parent && j.OnDefaultBranch() {
			return j
		}
	}
	return nil
}

// WithJobs is the snapshot with jobs added, its names sorted again; the
// receiver is not changed. The hash stays the files'.
func (s *Snapshot) WithJobs(jobs ...*Job) *Snapshot {
	out := &Snapshot{Jobs: make(map[string]*Job, len(s.Jobs)+len(jobs)), Names: slices.Clone(s.Names), Hash: s.Hash, Vars: s.Vars}
	maps.Copy(out.Jobs, s.Jobs)
	for _, j := range jobs {
		if _, dup := out.Jobs[j.Name]; dup {
			continue
		}
		out.Jobs[j.Name] = j
		out.Names = append(out.Names, j.Name)
	}
	slices.Sort(out.Names)
	return out
}

// checkMultibranch refuses what a multibranch job may not have beside it:
// it has no stages of its own and is built by its repo's pushes.
func (j *Job) checkMultibranch(hasStages bool) error {
	switch {
	case j.Multibranch == nil:
		return nil
	case hasStages || j.Source != nil:
		return errors.New("multibranch: the stages are the repo's, in multibranch.path; a multibranch job has no stages or source")
	case j.Triggers.Webhook != nil:
		return errors.New("multibranch: the job's webhook is its own, POST /api/v1/hooks/github/" + j.Name + "; it has no triggers.webhook")
	case j.Workdir != "" && !filepath.IsLocal(j.Workdir):
		return fmt.Errorf("workdir %q: with multibranch it is a path inside the clone, like ci or services/api", j.Workdir)
	}
	return nil
}

// checkChildNames refuses a job file named like a child of a multibranch
// job: the name is the child's.
func checkChildNames(snap *Snapshot) []error {
	var errs []error
	for _, p := range snap.Names {
		var what string
		switch j := snap.Jobs[p]; {
		case j.IsParent():
			what = "a multibranch job, and the names %[2]s.<branch> and %[2]s.pr-<n> are its children's"
		case j.IsOwner():
			what = "an owner job, and the names %[2]s.<repo> are its repos'"
		default:
			continue
		}
		for _, n := range snap.Names {
			if strings.HasPrefix(n, p+".") {
				errs = append(errs, fmt.Errorf("%s.yml: %s is "+what, n, p))
			}
		}
	}
	return errs
}
