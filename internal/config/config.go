// Package config reads job files into an immutable snapshot and refuses
// anything the rest of kipp could not run exactly as written.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/ndthuan/kipp/internal/pipeline"
	"github.com/ndthuan/kipp/internal/sched"
)

// Concurrency says what a trigger does while the job has a build running.
type Concurrency string

// The three rules.
const (
	Queue    Concurrency = "queue"
	Drop     Concurrency = "drop"
	Parallel Concurrency = "parallel"
	// Supersede queues like Queue, and a new build cancels the builds of the
	// same job and the same ref (branch or tag; none for a cron, by-hand or
	// API build) still queued: the newest of a ref is the one worth running.
	Supersede Concurrency = "supersede"
)

// Job is one job file, validated.
type Job struct {
	Name        string
	Group       string
	Description string
	// Disabled keeps the file and the history and takes the job out of
	// use: its triggers are ignored and nobody builds it.
	Disabled bool
	// Gone is a multibranch child whose branch or pull request is gone, or
	// a repo of an owner job that is gone, and everything under it. It is
	// disabled too, but it is not a job anybody turned off.
	Gone     bool
	Params   []Param
	Triggers Triggers
	// TimeZone is the zone the job's cron is read in; nil is the
	// server's. Zone says which one applies.
	TimeZone    *time.Location
	Concurrency Concurrency
	Locks       []string
	Timeout     time.Duration
	Keep        Keep
	Env         map[string]string
	Secrets     []string
	// Workdir is where the stages run: with Source a path inside the
	// clone, otherwise a directory of the machine. Empty is the build's
	// workspace.
	Workdir   string
	Source    *Source
	Stages    []*Stage
	OnFailure *Hook
	OnSuccess *Hook
	// Metrics names the job's store and what it remembers; nil is the
	// store of the job's name, remembering by DefaultRemember.
	Metrics *JobMetrics
	// CacheWritesDefault lets every build of the job write the cache's
	// default namespace, which all builds read: its owner vouches that each
	// build runs default-branch code.
	CacheWritesDefault bool
	// Multibranch is a job built through its children, one per branch and
	// pull request of its repo; a child keeps its parent's.
	Multibranch *Multibranch
	// Owner is a job that indexes every repo of a GitHub owner: it builds
	// nothing itself, and each repo is a Node, a multibranch job.
	Owner *Owner
	// Folder is the owner job a repo's node was made from, and so the one
	// its children belong to as well; empty on any other job.
	Folder string
	// Parent is the multibranch job a child was made from, Origin what it
	// builds, and DefaultBranch the repo's default branch as kipp last
	// heard it; all three are empty on a job of a file.
	Parent        string
	Origin        *ChildRecord
	DefaultBranch string
}

// Param is a build parameter with its default. Description is what the
// build form says under the param's name: what to type, and what an empty
// field means.
type Param struct {
	Name        string   `yaml:"-"`
	Type        string   `yaml:"type"`
	Options     []string `yaml:"options"`
	Default     string   `yaml:"default"`
	Description string   `yaml:"description"`
}

// Triggers of a job; cron is a spec string, validated here.
type Triggers struct {
	Cron    string   `yaml:"cron"`
	Webhook *Webhook `yaml:"webhook"`
	After   []string `yaml:"after"`
}

// Webhook matches a GitHub push.
type Webhook struct {
	Repo string `yaml:"repo"`
	// Branches are names or path.Match patterns ("APP-*", where a star
	// stops at a slash). Git refuses * ? [ and a backslash in a branch's
	// name, so a name is only ever itself. Neither branches nor tags
	// watches every branch.
	Branches []string `yaml:"branches"`
	// Tags are names or patterns of the tags whose push starts the job,
	// read as Branches are; none watches no tag.
	Tags []string `yaml:"tags"`
}

// Watches reports whether a push to branch is this webhook's. A webhook
// with tags and no branches is a release's and watches no branch.
func (w *Webhook) Watches(branch string) bool {
	if len(w.Branches) == 0 {
		return len(w.Tags) == 0
	}
	return matchesAny(w.Branches, branch)
}

// WatchesTag reports whether a push of tag is this webhook's.
func (w *Webhook) WatchesTag(tag string) bool { return matchesAny(w.Tags, tag) }

// matchesAny reports whether name is one of patterns, or matches one.
func matchesAny(patterns []string, name string) bool {
	return slices.ContainsFunc(patterns, func(p string) bool {
		ok, err := path.Match(p, name)
		return err == nil && ok
	})
}

// Keep is the retention rule.
type Keep struct {
	Builds int `yaml:"builds"`
	Days   int `yaml:"days"`
}

// Source points at a stages file inside a repo kipp clones.
type Source struct {
	Repo string `yaml:"repo"`
	Ref  string `yaml:"ref"`
	Path string `yaml:"path"`
}

// Stage is one shell command in the DAG.
type Stage struct {
	Name  string   `yaml:"-"`
	Needs []string `yaml:"needs"`
	Run   string   `yaml:"run"`
	Junit string   `yaml:"junit"`
	// Env is what the stage's command is given besides the job's: after
	// every other variable, params apart.
	Env map[string]string `yaml:"env"`
	// UnstableExit lists the exit codes that end the stage as unstable
	// and not as failed: the command ran to its end and has something to
	// report that is no reason to stop what comes after it.
	UnstableExit ExitCodes `yaml:"unstable_exit"`
	// TestsFailed is how a stage that would end success or unstable ends
	// when its junit files hold a failed or errored case: unstable or
	// failed. Empty leaves the end to the exit code.
	TestsFailed string `yaml:"tests_failed"`
	// Retries is how many more times a command that would end the stage
	// failed is run again, 0 to MaxRetries.
	Retries RetryCount `yaml:"retries"`
	// RunsOn are the labels of the place the stage runs; none is the
	// server. With Image it is an engine's container, without it a
	// standing runner: two requests a shared label does not blur.
	RunsOn Labels `yaml:"runs_on"`
	Image  string `yaml:"image"`
	// Workdir is a path inside the stage's workspace, which on a runner is
	// the runner's and not the job's.
	Workdir string `yaml:"workdir"`
	// When is the condition the stage runs under; nil always runs.
	When *When `yaml:"when"`
	// Metrics is the stage's report and guards; nil is a stage that
	// neither reports nor reads metrics.
	Metrics *StageMetrics `yaml:"metrics"`
	// Artifacts are the files the stage keeps with its build, however it
	// ends; nil keeps none.
	Artifacts *StageArtifacts `yaml:"artifacts"`
	// ArtifactsFrom are the stages, each one this stage needs directly or
	// through others, whose artifacts it is given before it starts, under
	// $KIPP_WORKSPACE/.kipp/artifacts/<stage>/ wherever it runs.
	ArtifactsFrom []string `yaml:"artifacts_from"`
	// CacheScopes are the cache scopes the stage may restore from and save
	// to with kipp cache; none is a stage that reaches no cache.
	CacheScopes []string `yaml:"cache_scopes"`
	// Cache are the caches kipp restores before the command and saves after
	// it; their scopes are reached whether or not CacheScopes names them.
	Cache []CacheSpec `yaml:"cache"`
}

// Remote reports whether the stage runs somewhere other than the server.
func (s *Stage) Remote() bool { return len(s.RunsOn) > 0 }

// Unstable reports whether a command that exited with code ends the stage
// as unstable.
func (s *Stage) Unstable(code int) bool { return slices.Contains(s.UnstableExit, code) }

// ExitCodes is a stage's unstable_exit: one or more distinct exit codes, each
// one a command can fail with.
type ExitCodes []int

// UnmarshalYAML implements yaml.Unmarshaler. The codes are read from the
// nodes themselves: decoded into an int, 4.5 would be cut to 4 without a word.
func (c *ExitCodes) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.SequenceNode || len(n.Content) == 0 {
		return fmt.Errorf("line %d: unstable_exit is a list of one or more exit codes, like [4]", n.Line)
	}
	for _, item := range n.Content {
		code, err := exitCode(item)
		if err != nil {
			return fmt.Errorf("line %d: unstable_exit %q: %w", item.Line, item.Value, err)
		}
		if slices.Contains(*c, code) {
			return fmt.Errorf("line %d: unstable_exit names %d twice", item.Line, code)
		}
		*c = append(*c, code)
	}
	return nil
}

// exitCode reads one item of an unstable_exit list.
func exitCode(item *yaml.Node) (int, error) {
	outOfRange := errors.New("an exit code is a whole number from 1 to 255; 0 is success")
	switch {
	case item.Kind != yaml.ScalarNode:
		return 0, outOfRange
	case leadingZero(item.Value):
		// YAML reads 010 as octal 8, a shell reads it as 10, and to YAML 08
		// is no number at all: none of them is taken on trust.
		return 0, errors.New("an exit code is written without a leading zero")
	case item.ShortTag() == "!!str":
		return 0, errors.New("an exit code is a number, not text")
	}
	code, err := strconv.Atoi(item.Value)
	if item.ShortTag() != "!!int" || err != nil || code < 1 || code > 255 {
		return 0, outOfRange
	}
	return code, nil
}

// leadingZero reports whether s is digits that start with a zero and go on.
func leadingZero(s string) bool {
	return len(s) > 1 && s[0] == '0' && strings.Trim(s, "0123456789") == ""
}

// Hook runs after the build.
type Hook struct {
	Run string `yaml:"run"`
}

// file is the YAML shape; strict decoding refuses unknown keys.
type file struct {
	Name        string            `yaml:"name"`
	Group       string            `yaml:"group"`
	Description string            `yaml:"description"`
	Disabled    bool              `yaml:"disabled"`
	Params      ordered[Param]    `yaml:"params"`
	Triggers    Triggers          `yaml:"triggers"`
	TimeZone    string            `yaml:"timezone"`
	Concurrency string            `yaml:"concurrency"`
	Locks       []string          `yaml:"locks"`
	Timeout     string            `yaml:"timeout"`
	Keep        *Keep             `yaml:"keep"`
	Env         map[string]string `yaml:"env"`
	Secrets     []string          `yaml:"secrets"`
	Workdir     string            `yaml:"workdir"`
	Source      *Source           `yaml:"source"`
	Stages      ordered[Stage]    `yaml:"stages"`
	OnFailure   *Hook             `yaml:"on_failure"`
	OnSuccess   *Hook             `yaml:"on_success"`
	Metrics     *JobMetrics       `yaml:"metrics"`
	Multibranch *multibranchFile  `yaml:"multibranch"`
	Owner       *ownerFile        `yaml:"owner"`

	CacheWritesDefault bool `yaml:"cache_writes_default"`
}

type stagesFile struct {
	Env    map[string]string `yaml:"env"`
	Stages ordered[Stage]    `yaml:"stages"`
}

// StageFile is a stages file: a source job's, or a multibranch job's
// .kipp.yml. Its Env is given to every stage, under a stage's own.
type StageFile struct {
	Env    map[string]string
	Stages []*Stage
}

// DefaultKeepBuilds is how many builds a job keeps when it has no keep rule.
const DefaultKeepBuilds = 100

// RemovedKeepDays is how long a build of a job whose file is gone is kept.
// A job's own rule goes with its file, and the default one keeps the newest
// builds for ever, which for a job that will never build again is for ever.
const RemovedKeepDays = 90

// The logs a build has beside its stages' own: what went wrong before a
// stage ran, the clone of a source job, and the hook's output. No stage
// may take one of these names.
const (
	PrepareLog = "prepare"
	CloneLog   = "clone"
	HooksLog   = "hooks"
)

// CSRFField is the form field that carries the web UI's CSRF token beside
// a build's params, and so a name no param may take.
const CSRFField = "csrf"

var (
	nameRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// ValidName reports whether s may name a job or a stage: lower-case
// letters, digits, dot, dash and underscore, and so safe as a file name.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// ValidEnvName reports whether s may name a param, a secret or an env
// entry: a name a shell accepts for a variable.
func ValidEnvName(s string) bool { return envNameRe.MatchString(s) }

// decodeDoc decodes exactly one YAML document, refusing unknown keys.
func decodeDoc(data []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("the file is empty")
		}
		return err
	}
	if err := dec.Decode(new(yaml.Node)); !errors.Is(err, io.EOF) {
		return errors.New("more than one YAML document in the file")
	}
	return nil
}

// ParseJob reads one job file. filename is the base name; it must equal name + ".yml".
func ParseJob(filename string, data []byte) (*Job, error) {
	var f file
	if err := decodeDoc(data, &f); err != nil {
		return nil, err
	}
	j := &Job{
		Name: f.Name, Group: f.Group, Description: f.Description, Disabled: f.Disabled, Triggers: f.Triggers,
		Concurrency: Concurrency(f.Concurrency), Locks: f.Locks, Env: f.Env, Secrets: f.Secrets,
		Workdir: f.Workdir, Source: f.Source, OnFailure: f.OnFailure, OnSuccess: f.OnSuccess,
		Metrics: f.Metrics, Keep: Keep{Builds: DefaultKeepBuilds}, CacheWritesDefault: f.CacheWritesDefault,
	}
	if j.Concurrency == "" {
		j.Concurrency = Queue
	}
	steps := []func() error{
		func() error { return j.checkName(filename) },
		j.checkConcurrency,
		func() error { return j.setTimeoutAndKeep(f.Timeout, f.Keep) },
		func() error { return j.setParams(f.Params) },
		j.checkEnvNames,
		j.checkTriggers,
		func() error { return j.setTimeZone(f.TimeZone) },
		func() error { return j.setMultibranch(f.Multibranch, len(f.Stages.items) > 0) },
		func() error { return j.setOwner(f.Owner, len(f.Stages.items) > 0) },
		func() error { return j.setStages(f.Stages) },
		j.checkHooks,
		j.checkMetrics,
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return nil, err
		}
	}
	return j, nil
}

func (j *Job) checkName(filename string) error {
	if !ValidName(j.Name) {
		return fmt.Errorf("name %q: lower-case letters, digits, dot, dash and underscore only", j.Name)
	}
	if want := j.Name + ".yml"; filename != want {
		return fmt.Errorf("file %s holds job %q; the file must be called %s", filename, j.Name, want)
	}
	return nil
}

func (j *Job) checkConcurrency() error {
	if !slices.Contains([]Concurrency{Queue, Drop, Parallel, Supersede}, j.Concurrency) {
		return fmt.Errorf("concurrency %q: queue, drop, parallel or supersede", j.Concurrency)
	}
	return nil
}

func (j *Job) setTimeoutAndKeep(timeout string, keep *Keep) error {
	if timeout != "" {
		d, err := time.ParseDuration(timeout)
		if err != nil || d <= 0 {
			return fmt.Errorf("timeout %q: a duration like 30m or 2h", timeout)
		}
		j.Timeout = d
	}
	if keep != nil {
		if keep.Builds < 0 || keep.Days < 0 || (keep.Builds == 0 && keep.Days == 0) {
			return errors.New("keep: builds or days, above zero")
		}
		j.Keep = *keep
	}
	return nil
}

func (j *Job) setParams(o ordered[Param]) error {
	for _, p := range o.items {
		param, err := checkParam(p.name, p.item)
		if err != nil {
			return err
		}
		j.Params = append(j.Params, param)
	}
	return nil
}

// checkParam fills a param's type default and refuses what a shell or the UI could not use.
func checkParam(name string, p Param) (Param, error) {
	p.Name = name
	if !ValidEnvName(name) {
		return p, fmt.Errorf("param %q: a name a shell accepts", name)
	}
	if name == CSRFField {
		return p, fmt.Errorf("param %q: the name is reserved for the build form's own field", name)
	}
	p.Description = strings.TrimSpace(p.Description)
	if strings.ContainsRune(p.Description, 0) {
		return p, fmt.Errorf("param %s: a description holds no zero byte", name)
	}
	switch p.Type {
	case "", "string":
		p.Type = "string"
	case "text":
	case "bool":
		if p.Default == "" {
			p.Default = "false"
		}
		if p.Default != "true" && p.Default != "false" {
			return p, fmt.Errorf("param %s: a bool default is true or false", name)
		}
	case "choice":
		if len(p.Options) == 0 {
			return p, fmt.Errorf("param %s: choice needs options", name)
		}
		if p.Default == "" {
			p.Default = p.Options[0]
		}
		if !slices.Contains(p.Options, p.Default) {
			return p, fmt.Errorf("param %s: default %q is not an option", name, p.Default)
		}
	default:
		return p, fmt.Errorf("param %s: type %q is not string, text, bool or choice", name, p.Type)
	}
	return p, p.accepts(p.Default)
}

// accepts is nil when v is a value the param takes. A string is one line
// and a text any number of them.
func (p Param) accepts(v string) error {
	switch {
	case strings.ContainsRune(v, 0):
		return fmt.Errorf("param %s: a zero byte cannot be in a stage's environment", p.Name)
	case p.Type == "string" && strings.ContainsAny(v, "\r\n"):
		return fmt.Errorf("param %s: a string is one line; a param of type text takes several", p.Name)
	case p.Type == "bool" && v != "true" && v != "false":
		return fmt.Errorf("param %s: true or false, not %q", p.Name, v)
	case p.Type == "choice" && !slices.Contains(p.Options, v):
		return fmt.Errorf("param %s: %q is not one of %s", p.Name, v, strings.Join(p.Options, ", "))
	}
	return nil
}

func (j *Job) checkEnvNames() error {
	for _, s := range j.Secrets {
		if !ValidEnvName(s) {
			return fmt.Errorf("secret %q: a name a shell accepts", s)
		}
	}
	for _, p := range j.Params {
		if err := CheckEnvName(p.Name); err != nil {
			return fmt.Errorf("param %s: %w", p.Name, err)
		}
		if slices.Contains(j.Secrets, p.Name) {
			return fmt.Errorf("param %s: a secret this job lists has the name", p.Name)
		}
	}
	return j.CheckEnv("job file", j.Env)
}

// CheckEnv refuses an env: that declares a name it may not, or one of the
// job's secrets. where names the file, for the message.
func (j *Job) CheckEnv(where string, env map[string]string) error {
	if err := checkEnvMap(where, env); err != nil {
		return err
	}
	return j.CheckVarNames(where, slices.Sorted(maps.Keys(env)))
}

func (j *Job) checkTriggers() error {
	if j.Triggers.Cron != "" {
		if _, err := sched.Parse(j.Triggers.Cron, j.Name); err != nil {
			return err
		}
	}
	if slices.Contains(j.Triggers.After, j.Name) {
		return errors.New("triggers.after: a job cannot run after itself")
	}
	w := j.Triggers.Webhook
	if w == nil {
		return nil
	}
	if !strings.Contains(w.Repo, "/") {
		return errors.New("triggers.webhook.repo: owner/name")
	}
	for _, list := range []struct {
		key      string
		patterns []string
	}{{"branches", w.Branches}, {"tags", w.Tags}} {
		for _, p := range list.patterns {
			if _, err := path.Match(p, ""); err != nil {
				return fmt.Errorf("job %s: triggers.webhook.%s: %q is not a pattern path.Match reads", j.Name, list.key, p)
			}
		}
	}
	return nil
}

// setTimeZone reads the job's own zone. Only a cron has times to read in
// one, so a zone on a job without a cron is a mistake, not a no-op.
func (j *Job) setTimeZone(name string) error {
	if name == "" {
		return nil
	}
	if j.Triggers.Cron == "" {
		return fmt.Errorf("timezone %q: it is the zone triggers.cron is read in, and the job has no cron", name)
	}
	z, err := LoadZone(name)
	if err != nil {
		return fmt.Errorf("timezone: %w", err)
	}
	j.TimeZone = z
	return nil
}

// Zone is the zone the job's cron is read in: its own, else server, else
// the host's. A nil job has no zone of its own.
func (j *Job) Zone(server *time.Location) *time.Location {
	switch {
	case j != nil && j.TimeZone != nil:
		return j.TimeZone
	case server != nil:
		return server
	}
	return time.Local
}

// LoadZone reads an IANA zone name, like Europe/Paris or UTC. "Local"
// is refused: it names whatever zone the host has, which is what leaving
// the zone out already means, and it would follow the host when it moves.
func LoadZone(name string) (*time.Location, error) {
	if strings.TrimSpace(name) == "" || strings.EqualFold(name, "local") {
		return nil, fmt.Errorf("%q: an IANA zone name, like Europe/Paris or UTC", name)
	}
	z, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("%q is not an IANA zone name, like Europe/Paris or UTC", name)
	}
	return z, nil
}

// setMultibranch reads multibranch:, which takes the place of stages and
// source.
func (j *Job) setMultibranch(f *multibranchFile, hasStages bool) error {
	if f == nil {
		return nil
	}
	m, err := f.multibranch()
	if err != nil {
		return err
	}
	j.Multibranch = m
	return j.checkMultibranch(hasStages)
}

func (j *Job) setStages(o ordered[Stage]) error {
	if j.Multibranch != nil || j.Owner != nil {
		return nil
	}
	if (j.Source == nil) == (len(o.items) == 0) {
		return errors.New("exactly one of stages, source, multibranch and owner")
	}
	if j.Source != nil {
		if j.Source.Repo == "" || j.Source.Path == "" {
			return errors.New("source: repo and path")
		}
		if j.Source.Ref == "" {
			j.Source.Ref = "main"
		}
		// The stages are the repo's and run in its clone: a directory
		// elsewhere on the machine would hold none of it.
		if j.Workdir != "" && !filepath.IsLocal(j.Workdir) {
			return fmt.Errorf("workdir %q: with source it is a path inside the clone, like ci or services/api", j.Workdir)
		}
		return nil
	}
	stages, err := toStages(o)
	if err != nil {
		return err
	}
	j.Stages = stages
	if err := j.CheckWhenParams(stages); err != nil {
		return err
	}
	return j.CheckStagesEnv("job file", stages)
}

// CheckStagesEnv refuses a stage whose env: names one of the job's secrets.
func (j *Job) CheckStagesEnv(where string, stages []*Stage) error {
	for _, s := range stages {
		if err := j.CheckVarNames(where+": stage "+s.Name, slices.Sorted(maps.Keys(s.Env))); err != nil {
			return err
		}
	}
	return nil
}

func (j *Job) checkHooks() error {
	for _, h := range []*Hook{j.OnFailure, j.OnSuccess} {
		if h != nil && strings.TrimSpace(h.Run) == "" {
			return errors.New("a hook needs run")
		}
	}
	return nil
}

// ParseStageFile reads a source file, which may hold stages and an env and
// nothing else.
func ParseStageFile(data []byte) (*StageFile, error) {
	var f stagesFile
	if err := decodeDoc(data, &f); err != nil {
		return nil, fmt.Errorf("a source file holds stages and an env and nothing else: %w", err)
	}
	if len(f.Stages.items) == 0 {
		return nil, errors.New("a source file needs stages")
	}
	if err := checkEnvMap("stages file", f.Env); err != nil {
		return nil, err
	}
	stages, err := toStages(f.Stages)
	if err != nil {
		return nil, err
	}
	return &StageFile{Env: f.Env, Stages: stages}, nil
}

func toStages(o ordered[Stage]) ([]*Stage, error) {
	var stages []*Stage
	needs := map[string][]string{}
	for _, it := range o.items {
		s := it.item
		s.Name = it.name
		if err := s.check(); err != nil {
			return nil, err
		}
		if _, dup := needs[s.Name]; dup {
			return nil, fmt.Errorf("stage %s twice", s.Name)
		}
		needs[s.Name] = s.Needs
		stages = append(stages, &s)
	}
	if err := pipeline.Validate(needs); err != nil {
		return nil, err
	}
	for _, s := range stages {
		if err := s.checkArtifactsFrom(needs); err != nil {
			return nil, err
		}
	}
	return stages, nil
}

// check refuses a stage that could not run as written, on its own.
func (s *Stage) check() error {
	if !ValidName(s.Name) {
		return fmt.Errorf("stage %q: lower-case letters, digits, dot, dash and underscore only", s.Name)
	}
	if slices.Contains([]string{PrepareLog, CloneLog, HooksLog}, s.Name) {
		return fmt.Errorf("stage %q: the name is reserved for the build's own log", s.Name)
	}
	if strings.TrimSpace(s.Run) == "" {
		return fmt.Errorf("stage %s: run is empty", s.Name)
	}
	if err := s.checkPlace(); err != nil {
		return err
	}
	if err := checkEnvMap("stage "+s.Name, s.Env); err != nil {
		return err
	}
	if err := s.When.check(s.Name); err != nil {
		return err
	}
	if s.When != nil && s.When.NeedsStatus != "" && len(s.Needs) == 0 {
		return fmt.Errorf("stage %s: when.needs_status needs needs: it says how the stages it needs must have ended", s.Name)
	}
	if err := s.Artifacts.check(s.Name); err != nil {
		return err
	}
	if err := s.checkCacheScopes(); err != nil {
		return err
	}
	if err := s.checkCache(); err != nil {
		return err
	}
	if err := s.checkTestsFailed(); err != nil {
		return err
	}
	return s.Metrics.check(s.Name)
}

// MaxRetries is the most a stage's retries may be.
const MaxRetries = 10

// RetryCount is a stage's retries: a whole number from 0 to MaxRetries.
type RetryCount int

// UnmarshalYAML implements yaml.Unmarshaler. The count is read from the
// node itself: decoded into an int, 1.5 would be cut to 1 without a word.
func (r *RetryCount) UnmarshalYAML(n *yaml.Node) error {
	n2, err := strconv.Atoi(n.Value)
	if n.Kind != yaml.ScalarNode || n.ShortTag() != "!!int" || err != nil || n2 < 0 || n2 > MaxRetries {
		return fmt.Errorf("line %d: retries %q: a whole number from 0 to %d", n.Line, n.Value, MaxRetries)
	}
	*r = RetryCount(n2)
	return nil
}

// checkTestsFailed refuses a tests_failed that is not unstable or failed,
// or that has no junit report to read.
func (s *Stage) checkTestsFailed() error {
	switch {
	case s.TestsFailed == "":
		return nil
	case s.TestsFailed != "unstable" && s.TestsFailed != "failed":
		return fmt.Errorf("stage %s: tests_failed %q: unstable or failed", s.Name, s.TestsFailed)
	case s.Junit == "":
		return fmt.Errorf("stage %s: tests_failed reads the stage's junit report, and junit names no file", s.Name)
	}
	return nil
}

// checkCacheScopes refuses a cache_scopes that names no scope, one that is
// not a name (a scope is a directory of the server's), or one twice.
func (s *Stage) checkCacheScopes() error {
	if s.CacheScopes != nil && len(s.CacheScopes) == 0 {
		return fmt.Errorf("stage %s: cache_scopes names no scope; leave it out for a stage with no cache", s.Name)
	}
	for i, scope := range s.CacheScopes {
		switch {
		case !ValidName(scope):
			return fmt.Errorf("stage %s: cache scope %q: lower-case letters, digits, dot, dash and underscore only", s.Name, scope)
		case slices.Contains(s.CacheScopes[:i], scope):
			return fmt.Errorf("stage %s: cache scope %s twice", s.Name, scope)
		}
	}
	return nil
}

// checkPlace refuses a runs_on, image or workdir no place could honour.
func (s *Stage) checkPlace() error {
	switch {
	case s.Image != "" && !s.Remote():
		return fmt.Errorf("stage %s: image needs runs_on: an image runs on an engine, and an engine is chosen by label", s.Name)
	case strings.ContainsFunc(s.Image, func(r rune) bool { return r <= ' ' || r == 0x7f }):
		return fmt.Errorf("stage %s: image %q: an image reference, like golang:1.27, with no space in it", s.Name, s.Image)
	case s.Workdir != "" && !filepath.IsLocal(s.Workdir):
		return fmt.Errorf("stage %s: workdir is a path inside the stage's workspace, like src or services/api", s.Name)
	}
	return nil
}

// Needs is the DAG input: stage name to the names it needs.
func (j *Job) Needs() map[string][]string {
	m := make(map[string][]string, len(j.Stages))
	for _, s := range j.Stages {
		m[s.Name] = s.Needs
	}
	return m
}

// ResolveParams fills defaults and refuses unknown names and bad values.
func (j *Job) ResolveParams(given map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(j.Params))
	for _, p := range j.Params {
		v, ok := given[p.Name]
		if !ok {
			v = p.Default
		}
		if err := p.accepts(v); err != nil {
			return nil, err
		}
		out[p.Name] = v
	}
	for k := range given {
		if _, ok := out[k]; !ok {
			return nil, fmt.Errorf("no param called %s", k)
		}
	}
	return out, nil
}

func hashOf(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
