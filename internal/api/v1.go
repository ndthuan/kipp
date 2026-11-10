package api

import (
	"slices"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/dispatch"
	"github.com/ndthuan/kipp/internal/enginepool"
	"github.com/ndthuan/kipp/internal/index"
	"github.com/ndthuan/kipp/internal/store"
)

// The answers of /api/v1. In them every field is always there, null where it
// has no value, and a list is [] and never null (D19).

// orNil is the string a pointer holds, nil for the empty string.
func orNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// numOrNil is the number a pointer holds, nil for 0.
func numOrNil(n int) *int {
	if n == 0 {
		return nil
	}
	return &n
}

// statusOrNil is a build's status, nil when none is known.
func statusOrNil(s store.Status) *store.Status {
	if s == "" {
		return nil
	}
	return &s
}

// nonNil is list itself, or an empty list where it has none.
func nonNil[T any](list []T) []T {
	if list == nil {
		return []T{}
	}
	return list
}

// buildRefV1 is a build wherever another answer refers to one (D18). Its
// status is null once retention has removed the build, and its id then
// whatever the answer kept of it, 0 for a guard's origin.
type buildRefV1 struct {
	ID     int64         `json:"id"`
	Job    string        `json:"job"`
	Number int           `json:"number"`
	Status *store.Status `json:"status"`
}

func refOf(b *store.Build) buildRefV1 {
	return buildRefV1{b.ID, b.Job, b.Number, &b.Status}
}

// buildSummaryV1 is a build in a list, in a job's `last` and in the answer
// to a trigger: no stages.
type buildSummaryV1 struct {
	ID            int64             `json:"id"`
	Job           string            `json:"job"`
	Number        int               `json:"number"`
	Status        store.Status      `json:"status"`
	Trigger       string            `json:"trigger"`
	TriggerDetail string            `json:"trigger_detail"`
	Params        map[string]string `json:"params"`
	StartedBy     string            `json:"started_by"`
	QueuedAt      stamp             `json:"queued_at"`
	StartedAt     *stamp            `json:"started_at"`
	FinishedAt    *stamp            `json:"finished_at"`
	// What a build of a multibranch job's child built, null for any other
	// build; but BuiltCommit, which a source job's build has too.
	Parent      *string `json:"parent"`
	Branch      *string `json:"branch"`
	BuiltCommit *string `json:"built_commit"`
	PR          *int    `json:"pr"`
	BaseBranch  *string `json:"base_branch"`
	BaseCommit  *string `json:"base_commit"`
	// Kept says who kept the build out of retention and when, null for a
	// build under its job's rules.
	Kept *keptV1 `json:"kept"`
	// RerunOf and RerunAs link a build stopped for a restart and the one
	// that runs it again, by number; null on any other build.
	RerunOf *int `json:"rerun_of"`
	RerunAs *int `json:"rerun_as"`
}

type keptV1 struct {
	By string `json:"by"`
	At stamp  `json:"at"`
}

// buildV1 is one build with its stages: the answer for a build alone.
// Complete says nothing more will be written for it.
type buildV1 struct {
	buildSummaryV1
	Stages   []stageJSON `json:"stages"`
	Complete bool        `json:"complete"`
}

func toBuildSummaryV1(b *store.Build) buildSummaryV1 {
	out := buildSummaryV1{ID: b.ID, Job: b.Job, Number: b.Number, Status: b.Status, Trigger: b.Trigger, TriggerDetail: b.TriggerDetail,
		Params: b.Params, StartedBy: b.StartedBy, QueuedAt: stampOf(b.QueuedAt), StartedAt: stampPtr(b.StartedAt), FinishedAt: stampPtr(b.FinishedAt)}
	out.BuiltCommit = orNil(b.BuiltCommit)
	if b.Parent != "" {
		out.Parent, out.Branch = orNil(b.Parent), orNil(b.TriggerBranch)
		out.PR, out.BaseBranch, out.BaseCommit = numOrNil(b.PR), orNil(b.BaseBranch), orNil(b.BaseCommit)
	}
	if b.KeptAt != nil {
		out.Kept = &keptV1{By: b.KeptBy, At: stampOf(*b.KeptAt)}
	}
	out.RerunOf, out.RerunAs = numOrNil(b.RerunOf), numOrNil(b.RerunAs)
	return out
}

func toBuildsV1(builds []*store.Build) []buildSummaryV1 {
	out := make([]buildSummaryV1, 0, len(builds))
	for _, b := range builds {
		out = append(out, toBuildSummaryV1(b))
	}
	return out
}

func toBuildV1(v *BuildView) buildV1 {
	out := buildV1{buildSummaryV1: toBuildSummaryV1(v.Build), Stages: []stageJSON{}, Complete: v.Complete}
	for _, s := range v.Stages {
		out.Stages = append(out.Stages, stageJSON{s.Name, nonNil(s.Needs), s.Status, s.ExitCode, stampPtr(s.StartedAt), stampPtr(s.FinishedAt), s.RanOn, s.Tries, reusedOf(s.Reused)})
	}
	return out
}

// lastV1 is a job's or a child's last build as a summary, null for none.
func lastV1(b *store.Build) *buildSummaryV1 {
	if b == nil {
		return nil
	}
	return new(toBuildSummaryV1(b))
}

type jobV1 struct {
	Name          string          `json:"name"`
	Group         string          `json:"group"`
	Description   string          `json:"description"`
	Cron          string          `json:"cron"`
	Next          *stamp          `json:"next"`
	Last          *buildSummaryV1 `json:"last"`
	Removed       bool            `json:"removed"`
	Disabled      bool            `json:"disabled"`
	ChildrenCount int             `json:"children_count"`
	Worst         *store.Status   `json:"worst"`
}

func toJobV1(j JobSummary) jobV1 {
	return jobV1{j.Name, j.Group, j.Description, j.Cron, stampPtr(j.Next), lastV1(j.Last), j.Removed, j.Disabled, j.Children, statusOrNil(j.Worst)}
}

type paramV1 struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Options     []string `json:"options"`
	Default     string   `json:"default"`
	Description *string  `json:"description"`
}

// scanV1 is how scanning stands: the interval in whole seconds, 0 for none.
type scanV1 struct {
	Token        bool    `json:"token"`
	EverySeconds *int64  `json:"every_seconds"`
	Running      bool    `json:"running"`
	At           *stamp  `json:"at"`
	Result       *string `json:"result"`
	Error        *string `json:"error"`
}

func toScanV1(s *index.ScanStatus) *scanV1 {
	if s == nil {
		return nil
	}
	out := &scanV1{Token: s.Token, Running: s.Running, Result: orNil(s.Result), Error: orNil(s.Err)}
	if s.Every > 0 {
		out.EverySeconds = new(int64(s.Every.Seconds()))
	}
	if !s.At.IsZero() {
		out.At = stampPtr(&s.At)
	}
	return out
}

type repoV1 struct {
	Job           string        `json:"job"`
	Repo          string        `json:"repo"`
	State         string        `json:"state"`
	Hook          *string       `json:"hook"`
	HookNote      *string       `json:"hook_note"`
	ChildrenCount int           `json:"children_count"`
	Worst         *store.Status `json:"worst"`
}

type overlapV1 struct {
	Repo string   `json:"repo"`
	Jobs []string `json:"jobs"`
}

type ownerV1 struct {
	Login      string      `json:"login"`
	App        *string     `json:"app"`
	Scan       *scanV1     `json:"scan"`
	PublicOnly bool        `json:"public_only"`
	Repos      []repoV1    `json:"repos"`
	OldHooks   []string    `json:"old_hooks"`
	Overlaps   []overlapV1 `json:"overlaps"`
}

func toOwnerV1(v *JobView, job *config.Job) *ownerV1 {
	if v.Owner == nil {
		return nil
	}
	scan := v.Owner.Scan
	out := &ownerV1{Login: job.Owner.Login, Scan: toScanV1(&scan), App: orNil(v.Owner.App), PublicOnly: v.Owner.PublicOnly, Repos: []repoV1{},
		OldHooks: append([]string{}, v.Owner.OldHooks...), Overlaps: []overlapV1{}}
	for _, r := range v.Repos {
		out.Repos = append(out.Repos, repoV1{r.Node, r.Repo, r.State, orNil(r.Hook), orNil(r.Note), r.Children, statusOrNil(r.Worst)})
	}
	for _, o := range v.Owner.Overlaps {
		out.Overlaps = append(out.Overlaps, overlapV1{o.Repo, nonNil(slices.Clone(o.Jobs))})
	}
	return out
}

// jobDetailV1 holds its builds and its activity as the first pages of the
// lists of the same names.
type jobDetailV1 struct {
	Name        string                   `json:"name"`
	Removed     bool                     `json:"removed"`
	Disabled    bool                     `json:"disabled"`
	Group       string                   `json:"group"`
	Description string                   `json:"description"`
	Params      []paramV1                `json:"params"`
	Cron        string                   `json:"cron"`
	Next        *stamp                   `json:"next"`
	TimeZone    string                   `json:"timezone"`
	Concurrency config.Concurrency       `json:"concurrency"`
	Locks       []string                 `json:"locks"`
	Builds      listJSON[buildSummaryV1] `json:"builds"`
	Activity    listJSON[activityJSON]   `json:"activity"`
	Parent      *string                  `json:"parent"`
	Multibranch *string                  `json:"multibranch"`
	Scan        *scanV1                  `json:"scan"`
	Folder      *string                  `json:"folder"`
	Owner       *ownerV1                 `json:"owner"`
}

func toJobDetailV1(v *JobView) jobDetailV1 {
	out := jobDetailV1{Name: v.Name, Removed: v.Removed, Params: []paramV1{}, Locks: []string{}, Next: stampPtr(v.Next), Parent: orNil(v.Parent), Folder: orNil(v.Folder),
		Builds: listJSON[buildSummaryV1]{Items: toBuildsV1(v.Builds)}, Activity: listJSON[activityJSON]{Items: toActivityJSON(v.Activity)}}
	if v.Older > 0 {
		out.Builds.NextCursor = new(cursorFor("builds", int64(v.Older)))
	}
	if v.OlderActivity {
		out.Activity.NextCursor = new(cursorFor("activity", v.Activity[len(v.Activity)-1].ID))
	}
	if job := v.Job; job != nil {
		out.Group, out.Description, out.Cron, out.Concurrency = job.Group, job.Description, job.Triggers.Cron, job.Concurrency
		out.Disabled = job.Disabled
		out.TimeZone = v.Zone.String()
		out.Locks = append(out.Locks, job.Locks...)
		if job.IsParent() {
			out.Multibranch = orNil(job.Multibranch.Repo)
			out.Scan = toScanV1(v.Scan)
		}
		out.Owner = toOwnerV1(v, job)
		for _, p := range job.Params {
			out.Params = append(out.Params, paramV1{p.Name, p.Type, append([]string{}, p.Options...), p.Default, orNil(p.Description)})
		}
	}
	return out
}

type childV1 struct {
	Job        string          `json:"job"`
	Key        string          `json:"key"`
	Kind       string          `json:"kind"`
	Branch     string          `json:"branch"`
	PR         *int            `json:"pr"`
	Title      *string         `json:"title"`
	Author     *string         `json:"author"`
	URL        *string         `json:"url"`
	HeadCommit string          `json:"head_commit"`
	BaseBranch *string         `json:"base_branch"`
	FromFork   bool            `json:"from_fork"`
	State      string          `json:"state"`
	GoneAt     *stamp          `json:"gone_at"`
	LastSeenAt stamp           `json:"last_seen_at"`
	Last       *buildSummaryV1 `json:"last"`
}

func toChildV1(c ChildView) childV1 {
	return childV1{Job: c.Job, Key: c.Key, Kind: c.Kind, Branch: c.Branch, PR: numOrNil(c.PR), Title: orNil(c.Title), Author: orNil(c.Author), URL: orNil(c.URL), HeadCommit: c.HeadSHA,
		BaseBranch: orNil(c.BaseBranch), FromFork: c.FromFork, State: c.State, GoneAt: stampPtr(c.GoneAt), LastSeenAt: stampOf(c.LastSeenAt), Last: lastV1(c.Last)}
}

type queueV1 struct {
	Executors *int        `json:"executors"`
	Busy      *int        `json:"busy"`
	Running   []runningV1 `json:"running"`
	Queued    []queuedV1  `json:"queued"`
}

type runningV1 struct {
	ID            int64             `json:"id"`
	Job           string            `json:"job"`
	Number        int               `json:"number"`
	Trigger       string            `json:"trigger"`
	StartedBy     string            `json:"started_by"`
	QueuedAt      stamp             `json:"queued_at"`
	StartedAt     *stamp            `json:"started_at"`
	Stages        []string          `json:"stages"`
	WaitingStages map[string]string `json:"waiting_stages"`
}

type queuedV1 struct {
	ID        int64               `json:"id"`
	Job       string              `json:"job"`
	Number    int                 `json:"number"`
	Trigger   string              `json:"trigger"`
	StartedBy string              `json:"started_by"`
	QueuedAt  stamp               `json:"queued_at"`
	Waiting   dispatch.WaitReason `json:"waiting"`
	Lock      *string             `json:"lock"`
	Blocker   *buildRefV1         `json:"blocker"`
}

func toQueueV1(v *QueueView) queueV1 {
	out := queueV1{Running: []runningV1{}, Queued: []queuedV1{}}
	if v.Executors > 0 { // an admin's
		out.Executors, out.Busy = &v.Executors, &v.Busy
	}
	for _, r := range v.Running {
		waiting := r.Waiting
		if waiting == nil {
			waiting = map[string]string{}
		}
		b := r.Build
		out.Running = append(out.Running, runningV1{b.ID, b.Job, b.Number, b.Trigger, b.StartedBy, stampOf(b.QueuedAt), stampPtr(b.StartedAt), nonNil(r.Stages), waiting})
	}
	for _, w := range v.Queued {
		b := w.Build
		q := queuedV1{ID: b.ID, Job: b.Job, Number: b.Number, Trigger: b.Trigger, StartedBy: b.StartedBy, QueuedAt: stampOf(b.QueuedAt), Waiting: w.Waiting, Lock: orNil(w.Lock)}
		if k := w.Blocker; k != nil {
			q.Blocker = &buildRefV1{k.ID, k.Job, k.Number, &k.Status}
		}
		out.Queued = append(out.Queued, q)
	}
	return out
}

type engineV1 struct {
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	URL       string   `json:"url"`
	Labels    []string `json:"labels"`
	Executors int      `json:"executors"`
	Enabled   bool     `json:"enabled"`
	Reachable bool     `json:"reachable"`
	Since     *stamp   `json:"since"`
	Version   string   `json:"version"`
	LastError string   `json:"last_error"`
	Running   []string `json:"running"`
}

func toEngineV1(e enginepool.EngineStatus) engineV1 {
	return engineV1{e.Name, e.Kind, e.URL, nonNil(slices.Clone(e.Labels)), e.Max, e.Enabled, e.Reachable, stampSince(e.Since), e.Version, e.LastError, nonNil(slices.Clone(e.Running))}
}

// runnerStageV1 is a stage a runner runs or has been given.
type runnerStageV1 struct {
	Build buildRefV1 `json:"build"`
	Stage string     `json:"stage"`
}

type runnerV1 struct {
	Name       string          `json:"name"`
	Labels     []string        `json:"labels"`
	Executors  int             `json:"executors"`
	Busy       int             `json:"busy"`
	Enabled    bool            `json:"enabled"`
	Online     bool            `json:"online"`
	Since      *stamp          `json:"since"`
	Version    string          `json:"version"`
	OS         string          `json:"os"`
	Arch       string          `json:"arch"`
	Stages     []runnerStageV1 `json:"stages"`
	LastSeenAt *stamp          `json:"last_seen_at"`
}

// toRunnerV1 is a runner with its stages' builds as they stand: builds says
// how each ended, if it has.
func toRunnerV1(a RunnerInfo, builds map[int64]*store.Build) runnerV1 {
	out := runnerV1{Name: a.Name, Labels: nonNil(slices.Clone(a.Labels)), Executors: a.Executors, Busy: a.Busy, Enabled: a.Enabled, Online: a.Online,
		Since: stampSince(a.Since), Version: a.Version, OS: a.OS, Arch: a.Arch, Stages: []runnerStageV1{}, LastSeenAt: stampPtr(a.LastSeenAt)}
	for _, s := range a.Running {
		ref := buildRefV1{ID: s.BuildID, Job: s.Job, Number: s.Number}
		if b := builds[s.BuildID]; b != nil {
			ref.Status = &b.Status
		}
		out.Stages = append(out.Stages, runnerStageV1{ref, s.Stage})
	}
	return out
}

// stageBuildIDs are the builds a runner's stages belong to.
func stageBuildIDs(runners []RunnerInfo) []int64 {
	var ids []int64
	for _, a := range runners {
		for _, s := range a.Running {
			ids = append(ids, s.BuildID)
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

type cacheV1 struct {
	ID             int64       `json:"id"`
	Scope          string      `json:"scope"`
	Namespace      string      `json:"namespace"`
	Key            string      `json:"key"`
	Size           int64       `json:"size"`
	SavedAt        *stamp      `json:"saved_at"`
	SavedBy        *buildRefV1 `json:"saved_by"`
	LastRestoredAt *stamp      `json:"last_restored_at"`
}

func toCacheV1(e store.CacheEntry, builds map[int64]*store.Build) cacheV1 {
	out := cacheV1{ID: e.ID, Scope: e.Scope, Namespace: e.Namespace, Key: e.Key, Size: e.Size,
		SavedAt: stampSince(e.SavedAt), LastRestoredAt: stampSince(e.LastRestoredAt)}
	if b := builds[e.SavedByBuild]; b != nil {
		ref := refOf(b)
		out.SavedBy = &ref
	}
	return out
}

type rememberedV1 struct {
	Metric string      `json:"metric"`
	Item   string      `json:"item"`
	Value  float64     `json:"value"`
	Unit   string      `json:"unit"`
	Build  *buildRefV1 `json:"build"`
	At     stamp       `json:"at"`
}

// rememberedRef is the build that set a value, null for one put by hand.
func rememberedRef(id int64, job string, number int, status store.Status) *buildRefV1 {
	if id == 0 {
		return nil
	}
	return &buildRefV1{id, job, number, statusOrNil(status)}
}

type historyV1 struct {
	Build buildRefV1 `json:"build"`
	Stage string     `json:"stage"`
	Value float64    `json:"value"`
	Unit  string     `json:"unit"`
	At    stamp      `json:"at"`
}

type guardV1 struct {
	Metric     string      `json:"metric"`
	Item       string      `json:"item"`
	Remembered *float64    `json:"remembered"`
	Reported   *float64    `json:"reported"`
	Tolerance  float64     `json:"tolerance"`
	Verdict    string      `json:"verdict"`
	Outcome    string      `json:"outcome"`
	From       *buildRefV1 `json:"from"`
	Line       string      `json:"line"`
}

type stageMetricsV1 struct {
	Name    string         `json:"name"`
	Metrics []reportedJSON `json:"metrics"`
	Guards  []guardV1      `json:"guards"`
}

type buildMetricsV1 struct {
	Build  buildRefV1       `json:"build"`
	Store  string           `json:"store"`
	Stages []stageMetricsV1 `json:"stages"`
}

// toBuildMetricsV1 puts each row and each guard under its stage, as
// toBuildMetricsJSON does.
func toBuildMetricsV1(v *BuildMetricsView) buildMetricsV1 {
	out := buildMetricsV1{Build: refOf(v.Build), Store: v.Store, Stages: []stageMetricsV1{}}
	stage := func(name string) *stageMetricsV1 {
		i := slices.IndexFunc(out.Stages, func(s stageMetricsV1) bool { return s.Name == name })
		if i < 0 {
			out.Stages = append(out.Stages, stageMetricsV1{Name: name, Metrics: []reportedJSON{}, Guards: []guardV1{}})
			i = len(out.Stages) - 1
		}
		return &out.Stages[i]
	}
	for _, row := range v.Rows {
		st := stage(row.Stage)
		i := slices.IndexFunc(st.Metrics, func(m reportedJSON) bool { return m.Name == row.Metric })
		if i < 0 {
			st.Metrics = append(st.Metrics, reportedJSON{Name: row.Metric, Unit: row.Unit, Values: map[string]float64{}})
			i = len(st.Metrics) - 1
		}
		st.Metrics[i].Values[row.Item] = row.Value
	}
	for _, g := range v.Guards {
		st := stage(g.Stage)
		row := guardV1{g.Metric, g.Item, g.Remembered, g.Reported, g.Tolerance, g.Verdict, g.Outcome, nil, GuardLine(g)}
		if g.FromJob != "" {
			row.From = &buildRefV1{g.FromBuildID, g.FromJob, g.FromNumber, statusOrNil(g.FromStatus)}
		}
		st.Guards = append(st.Guards, row)
	}
	return out
}
