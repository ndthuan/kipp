package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Status of a build.
type Status string

// The statuses of a build. All but the first two are terminal. Unstable is
// a build none of whose stages failed and at least one of which ended with
// an exit code its job calls unstable.
const (
	Queued    Status = "queued"
	Running   Status = "running"
	Success   Status = "success"
	Unstable  Status = "unstable"
	Failed    Status = "failed"
	Cancelled Status = "cancelled"
	Lost      Status = "lost"
	Timeout   Status = "timeout"
)

// Terminal reports whether no further change comes.
func (s Status) Terminal() bool { return s != Queued && s != Running }

// Green and Red are the two colours a dashboard gives a finished build:
// green is success or unstable, red is failed, timeout or lost. Any other
// status, cancelled included, has neither.
func (s Status) Green() bool { return s == Success || s == Unstable }

// Red is failed, timeout or lost: see Green.
func (s Status) Red() bool { return s == Failed || s == Timeout || s == Lost }

// Succeeded reports whether the build ended in a way that what follows it
// takes as a success: the on_success hook and the jobs that run after it.
// An unstable build did.
func (s Status) Succeeded() bool { return s == Success || s == Unstable }

// StageStatus of one stage.
type StageStatus string

// The statuses of a stage.
const (
	StagePending   StageStatus = "pending"
	StageRunning   StageStatus = "running"
	StageSuccess   StageStatus = "success"
	StageUnstable  StageStatus = "unstable"
	StageFailed    StageStatus = "failed"
	StageSkipped   StageStatus = "skipped"
	StageCancelled StageStatus = "cancelled"
)

// Build is one row of builds.
type Build struct {
	ID            int64
	Job           string
	Number        int
	Status        Status
	Trigger       string // cron, api, web, webhook, scan or after
	TriggerDetail string
	// TriggerBranch is the branch a webhook's commit was pushed to; empty
	// for any other trigger, and for a webhook build queued before it was
	// kept.
	TriggerBranch string
	// TriggerDefaultBranch is the repo's default branch as the push named
	// it; empty for any other trigger, and for a webhook build queued
	// before it was kept.
	TriggerDefaultBranch string
	// TriggerTag is the tag a webhook build was pushed for, which then has
	// no TriggerBranch; empty for every other build. A build made again
	// from one carries it.
	TriggerTag string
	// PushedCommit is the commit of a push that a build made again from a
	// webhook's carries; a webhook's own is its TriggerDetail. Commit reads
	// both.
	PushedCommit string
	// UpstreamStatus is how the build this one runs after ended. It is
	// empty for any other trigger.
	UpstreamStatus Status
	// UpstreamCommit is the commit the build this one runs after built,
	// recorded with the trigger, as the upstream build may be gone by the
	// time this one runs. It is empty for any other trigger.
	UpstreamCommit string
	// UpstreamTag is the tag the build this one runs after was pushed
	// for, empty when that build was not a tag's.
	UpstreamTag string
	Params      map[string]string
	StartedBy   string
	QueuedAt    time.Time
	StartedAt   *time.Time
	FinishedAt  *time.Time
	ConfigHash  string
	// Parent is the multibranch job of a child's build, empty for any
	// other. BuiltCommit is the commit the build checked out, which a
	// child's build records whatever started it. PR, BaseBranch and
	// BaseCommit are a pull request's: its number, and the branch and the
	// commit its head was merged into.
	Parent      string
	BuiltCommit string
	PR          int
	BaseBranch  string
	BaseCommit  string
	// KeptBy and KeptAt say who kept the build out of retention and when;
	// empty and nil for a build that is under the job's rules.
	KeptBy string
	KeptAt *time.Time
	// RerunOf is the number of the build of the same job this one runs
	// again, stopped for a restart; RerunAs is the number of the build
	// that runs a stopped one again. Both are 0 on any other build.
	RerunOf, RerunAs int
}

// Stage is one row of stages. Needs is what the stage waited for in this
// build, whatever the job's file says since; never nil when read.
type Stage struct {
	BuildID    int64
	Name       string
	Needs      []string
	Status     StageStatus
	ExitCode   *int
	StartedAt  *time.Time
	FinishedAt *time.Time
	// RanOn is where the stage ran: local, a runner's name, or an engine's
	// container as engine/ordinal. Empty before it started.
	RanOn string
	// Tries is how many times the stage's command ran: 1 unless its
	// retries ran it again.
	Tries int
	// Reused is the green run the stage was reused from instead of
	// running; nil for a stage that ran, or did not.
	Reused *StageRef
}

// Commit is the commit a push named for this build, whether the build is
// the webhook's or one made again from it; empty for a build no push
// started.
func (b *Build) Commit() string {
	if b.PushedCommit == "" && b.Trigger == "webhook" {
		return b.TriggerDetail
	}
	return b.PushedCommit
}

// Pushed reports whether the build runs a push's branch and commit: a
// webhook's, or one made again from it.
func (b *Build) Pushed() bool { return b.Trigger == "webhook" || b.PushedCommit != "" }

const buildCols = `id, job, number, status, trigger, trigger_detail, params, started_by, queued_at, started_at, finished_at, config_hash, upstream_status, trigger_branch, trigger_default_branch, parent, built_commit, pr, base_branch, base_commit, pushed_commit, kept_by, kept_at, trigger_tag, rerun_of, rerun_as, upstream_commit, upstream_tag`

// CreateBuild inserts b with the job's next number and fills ID, Number and QueuedAt.
func (s *Store) CreateBuild(ctx context.Context, b *Build) error {
	if b.QueuedAt.IsZero() {
		b.QueuedAt = time.Now()
	}
	if b.Params == nil {
		b.Params = map[string]string{}
	}
	params, err := json.Marshal(b.Params)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `INSERT INTO job_counters (job, last_number) VALUES (?, 1)
		ON CONFLICT(job) DO UPDATE SET last_number = last_number + 1 RETURNING last_number`, b.Job).Scan(&b.Number); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO builds (job, number, status, trigger, trigger_detail, params, started_by, queued_at, config_hash, upstream_status, trigger_branch, trigger_default_branch, parent, built_commit, pr, base_branch, base_commit, pushed_commit, trigger_tag, rerun_of, upstream_commit, upstream_tag)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			b.Job, b.Number, b.Status, b.Trigger, b.TriggerDetail, string(params), b.StartedBy, ts(b.QueuedAt), b.ConfigHash, b.UpstreamStatus, b.TriggerBranch, b.TriggerDefaultBranch,
			b.Parent, b.BuiltCommit, b.PR, b.BaseBranch, b.BaseCommit, b.PushedCommit, b.TriggerTag, b.RerunOf, b.UpstreamCommit, b.UpstreamTag)
		if err != nil {
			return err
		}
		b.ID, err = res.LastInsertId()
		return err
	})
}

func scanBuild(row scanner) (*Build, error) {
	var b Build
	var params string
	var started, finished, kept sql.NullString
	var queued string
	if err := row.Scan(&b.ID, &b.Job, &b.Number, &b.Status, &b.Trigger, &b.TriggerDetail, &params, &b.StartedBy, &queued, &started, &finished, &b.ConfigHash, &b.UpstreamStatus, &b.TriggerBranch, &b.TriggerDefaultBranch,
		&b.Parent, &b.BuiltCommit, &b.PR, &b.BaseBranch, &b.BaseCommit, &b.PushedCommit, &b.KeptBy, &kept, &b.TriggerTag, &b.RerunOf, &b.RerunAs, &b.UpstreamCommit, &b.UpstreamTag); err != nil {
		return nil, notFound(err)
	}
	if err := json.Unmarshal([]byte(params), &b.Params); err != nil {
		return nil, err
	}
	var err error
	if b.QueuedAt, err = parseSetTS(queued); err != nil {
		return nil, err
	}
	if b.StartedAt, err = parseTS(started); err != nil {
		return nil, err
	}
	if b.FinishedAt, err = parseTS(finished); err != nil {
		return nil, err
	}
	if b.KeptAt, err = parseTS(kept); err != nil {
		return nil, err
	}
	return &b, nil
}

// queryBuilds selects builds by a clause written in this file; every value
// in it is a bound argument.
func (s *Store) queryBuilds(ctx context.Context, where string, args ...any) ([]*Build, error) {
	return collect(ctx, s.db, scanBuild, `SELECT `+buildCols+` FROM builds `+where, args...)
}

// Build by id.
func (s *Store) Build(ctx context.Context, id int64) (*Build, error) {
	return scanBuild(s.db.QueryRowContext(ctx, `SELECT `+buildCols+` FROM builds WHERE id = ?`, id))
}

// BuildByNumber is the job's build with that number.
func (s *Store) BuildByNumber(ctx context.Context, job string, number int) (*Build, error) {
	return scanBuild(s.db.QueryRowContext(ctx, `SELECT `+buildCols+` FROM builds WHERE job = ? AND number = ?`, job, number))
}

// Builds of a job, newest first.
func (s *Store) Builds(ctx context.Context, job string, limit int) ([]*Build, error) {
	return s.queryBuilds(ctx, `WHERE job = ? ORDER BY id DESC LIMIT ?`, job, limit)
}

// BuildsBefore are the job's builds numbered below before, newest first.
func (s *Store) BuildsBefore(ctx context.Context, job string, before, limit int) ([]*Build, error) {
	return s.queryBuilds(ctx, `WHERE job = ? AND number < ? ORDER BY id DESC LIMIT ?`, job, before, limit)
}

// PreviousCommits are the commits of the latest build of the job before
// build id that has one, and of the latest of those that ended success; a
// build's commit is the one it checked out, else the one a push named.
// Only builds still kept count. Either is empty when none is found.
func (s *Store) PreviousCommits(ctx context.Context, job string, id int64) (last, lastSuccess string, err error) {
	const page = 50
	for cursor := id; last == "" || lastSuccess == ""; {
		builds, err := s.queryBuilds(ctx, `WHERE job = ? AND id < ? ORDER BY id DESC LIMIT ?`, job, cursor, page)
		if err != nil {
			return "", "", err
		}
		for _, b := range builds {
			commit := cmp.Or(b.BuiltCommit, b.Commit())
			if commit == "" {
				continue
			}
			last = cmp.Or(last, commit)
			if b.Status == Success {
				lastSuccess = cmp.Or(lastSuccess, commit)
			}
		}
		if len(builds) < page {
			break
		}
		cursor = builds[len(builds)-1].ID
	}
	return last, lastSuccess, nil
}

// BuildsWithStatus in any of the statuses, oldest first.
func (s *Store) BuildsWithStatus(ctx context.Context, statuses ...Status) ([]*Build, error) {
	marks := make([]string, len(statuses))
	args := make([]any, len(statuses))
	for i, st := range statuses {
		marks[i], args[i] = "?", st
	}
	return s.queryBuilds(ctx, `WHERE status IN (`+strings.Join(marks, ",")+`) ORDER BY id`, args...)
}

// LastBuilds is the newest build of every job that has one.
func (s *Store) LastBuilds(ctx context.Context) (map[string]*Build, error) {
	builds, err := s.queryBuilds(ctx, `WHERE id IN (SELECT MAX(id) FROM builds GROUP BY job)`)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*Build, len(builds))
	for _, b := range builds {
		out[b.Job] = b
	}
	return out, nil
}

// ActiveJobs is every job with a build running or queued now.
func (s *Store) ActiveJobs(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT job FROM builds WHERE status IN (?, ?)`, Queued, Running)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var job string
		if err := rows.Scan(&job); err != nil {
			return nil, err
		}
		out[job] = true
	}
	return out, rows.Err()
}

// RecentFinished is up to n of the newest finished builds of every job,
// newest first. A job with none finished has no entry.
func (s *Store) RecentFinished(ctx context.Context, n int) (map[string][]*Build, error) {
	builds, err := s.queryBuilds(ctx, `WHERE id IN (SELECT id FROM (
		SELECT id, ROW_NUMBER() OVER (PARTITION BY job ORDER BY id DESC) AS rn
		FROM builds WHERE status NOT IN (?, ?)) WHERE rn <= ?) ORDER BY id DESC`, Queued, Running, n)
	if err != nil {
		return nil, err
	}
	out := map[string][]*Build{}
	for _, b := range builds {
		out[b.Job] = append(out[b.Job], b)
	}
	return out, nil
}

// RecentColoured is up to n of the newest builds of every job that ended
// green or red (Status.Green, Status.Red), newest first. A job with none
// has no entry.
func (s *Store) RecentColoured(ctx context.Context, n int) (map[string][]*Build, error) {
	builds, err := s.queryBuilds(ctx, `WHERE id IN (SELECT id FROM (
		SELECT id, ROW_NUMBER() OVER (PARTITION BY job ORDER BY id DESC) AS rn
		FROM builds WHERE status IN (?, ?, ?, ?, ?)) WHERE rn <= ?) ORDER BY id DESC`,
		Success, Unstable, Failed, Timeout, Lost, n)
	if err != nil {
		return nil, err
	}
	out := map[string][]*Build{}
	for _, b := range builds {
		out[b.Job] = append(out[b.Job], b)
	}
	return out, nil
}

// FinishedSince are the builds that ended at or after from, and the ones
// that have not ended: queued, running, or lost with no end recorded.
func (s *Store) FinishedSince(ctx context.Context, from time.Time) ([]*Build, error) {
	return s.queryBuilds(ctx, `WHERE finished_at >= ? OR finished_at IS NULL`, ts(from))
}

// Tally is how many builds ended green and how many ended red.
type Tally struct{ Green, Red int }

// Total is the builds that ended green or red.
func (t Tally) Total() int { return t.Green + t.Red }

// Percent is the share that ended green, in whole percent; 0 for none.
func (t Tally) Percent() int {
	if t.Total() == 0 {
		return 0
	}
	return (t.Green*100 + t.Total()/2) / t.Total()
}

// TalliesBetween counts, per job, the builds that ended in [from, to)
// green or red. A job with none has no entry.
func (s *Store) TalliesBetween(ctx context.Context, from, to time.Time) (map[string]Tally, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT job,
		SUM(status IN (?, ?)), SUM(status IN (?, ?, ?)) FROM builds
		WHERE finished_at >= ? AND finished_at < ? GROUP BY job`,
		Success, Unstable, Failed, Timeout, Lost, ts(from), ts(to))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]Tally{}
	for rows.Next() {
		var job string
		var t Tally
		if err := rows.Scan(&job, &t.Green, &t.Red); err != nil {
			return nil, err
		}
		if t.Total() > 0 {
			out[job] = t
		}
	}
	return out, rows.Err()
}

// SetBuildStatus moves a build on; Running stamps started_at, a terminal status finished_at.
func (s *Store) SetBuildStatus(ctx context.Context, id int64, status Status, at time.Time) error {
	col := ""
	switch {
	case status == Running:
		col = `, started_at = ?`
	case status.Terminal():
		col = `, finished_at = ?`
	}
	args := []any{status}
	if col != "" {
		args = append(args, ts(at))
	}
	args = append(args, id)
	return requireChange(s.db.ExecContext(ctx, `UPDATE builds SET status = ?`+col+` WHERE id = ?`, args...))
}

// SetBuildCommits records the commit a build checked out and, for a pull
// request merged into its base, the base's commit.
func (s *Store) SetBuildCommits(ctx context.Context, id int64, built, base string) error {
	return requireChange(s.db.ExecContext(ctx, `UPDATE builds SET built_commit = ?, base_commit = ? WHERE id = ?`, built, base, id))
}

// LostStage is a stage that was running when its server died, with the
// process group recorded for it: Pgid is 0 when none was.
type LostStage struct {
	Job    string
	Number int
	Name   string
	Pgid   int
	Born   string
}

// MarkRunningLost is run once at start: a build still running belonged to
// a dead process. It returns how many builds that was, and the stages that
// were running in them, which it cancels.
func (s *Store) MarkRunningLost(ctx context.Context, at time.Time) (int64, []LostStage, error) {
	var n int64
	var stages []LostStage
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		stages, err = collect(ctx, tx, func(row scanner) (l LostStage, err error) {
			return l, row.Scan(&l.Job, &l.Number, &l.Name, &l.Pgid, &l.Born)
		}, `SELECT b.job, b.number, s.name, s.pgid, s.born FROM stages s JOIN builds b ON b.id = s.build_id
		WHERE s.status = ? ORDER BY b.id, s.seq`, StageRunning)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE builds SET status = ?, finished_at = ? WHERE status = ?`, Lost, ts(at), Running)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE stages SET status = ?, finished_at = ? WHERE status = ?`, StageCancelled, ts(at), StageRunning); err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return 0, nil, err
	}
	return n, stages, nil
}

// KeepBuild marks a build as kept by by from at: OldBuilds no longer offers
// it, nor counts it toward the job's limit.
func (s *Store) KeepBuild(ctx context.Context, id int64, by string, at time.Time) error {
	return requireChange(s.db.ExecContext(ctx, `UPDATE builds SET kept_by = ?, kept_at = ? WHERE id = ?`, by, ts(at), id))
}

// UnkeepBuild puts a build back under the job's rules.
func (s *Store) UnkeepBuild(ctx context.Context, id int64) error {
	return requireChange(s.db.ExecContext(ctx, `UPDATE builds SET kept_by = '', kept_at = NULL WHERE id = ?`, id))
}

// OldBuilds lists finished builds of a job beyond keepBuilds (the newest kept)
// or older than keepDays, oldest first. Zero means that rule is off. A build
// that was kept is never listed and is not one of the newest keepBuilds.
func (s *Store) OldBuilds(ctx context.Context, job string, keepBuilds, keepDays int, now time.Time) ([]int64, error) {
	var conds []string
	args := []any{job}
	if keepBuilds > 0 {
		conds = append(conds, `id NOT IN (SELECT id FROM builds WHERE job = ? AND kept_at IS NULL ORDER BY id DESC LIMIT ?)`)
		args = append(args, job, keepBuilds)
	}
	if keepDays > 0 {
		conds = append(conds, `queued_at < ?`)
		args = append(args, ts(now.AddDate(0, 0, -keepDays)))
	}
	if len(conds) == 0 {
		return nil, nil
	}
	return collect(ctx, s.db, scanOne[int64], `SELECT id FROM builds WHERE job = ? AND status NOT IN ('queued','running') AND kept_at IS NULL AND (`+strings.Join(conds, " OR ")+`) ORDER BY id`, args...)
}

// DeleteBuild removes a build and, by cascade, its stages and tests.
func (s *Store) DeleteBuild(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM builds WHERE id = ?`, id)
	return err
}

// PutStages creates the pending rows in the order given, each with the
// names it needs.
func (s *Store) PutStages(ctx context.Context, buildID int64, names []string, needs map[string][]string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for i, n := range names {
			list, err := json.Marshal(append([]string{}, needs[n]...))
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO stages (build_id, seq, name, needs, status) VALUES (?, ?, ?, ?, ?)`, buildID, i, n, string(list), StagePending); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetStage moves a stage on; Running stamps started_at, anything else finished_at.
func (s *Store) SetStage(ctx context.Context, buildID int64, name string, status StageStatus, exit *int, at time.Time) error {
	col := `finished_at`
	if status == StageRunning {
		col = `started_at`
	}
	var code any
	if exit != nil {
		code = *exit
	}
	return requireChange(s.db.ExecContext(ctx, `UPDATE stages SET status = ?, exit_code = COALESCE(?, exit_code), `+col+` = ? WHERE build_id = ? AND name = ?`, status, code, ts(at), buildID, name))
}

// SetStageGroup records the process group a running stage was started in,
// for the server that finds the stage still running after this one died.
func (s *Store) SetStageGroup(ctx context.Context, buildID int64, name string, pgid int, born string) error {
	return requireChange(s.db.ExecContext(ctx, `UPDATE stages SET pgid = ?, born = ? WHERE build_id = ? AND name = ?`, pgid, born, buildID, name))
}

// SetStageRanOn records where a stage runs.
func (s *Store) SetStageRanOn(ctx context.Context, buildID int64, name, ranOn string) error {
	return requireChange(s.db.ExecContext(ctx, `UPDATE stages SET ran_on = ? WHERE build_id = ? AND name = ?`, ranOn, buildID, name))
}

// SetStageTries records how many times a stage's command ran.
func (s *Store) SetStageTries(ctx context.Context, buildID int64, name string, tries int) error {
	return requireChange(s.db.ExecContext(ctx, `UPDATE stages SET tries = ? WHERE build_id = ? AND name = ?`, tries, buildID, name))
}

// Stages of a build, in the order they were put.
func (s *Store) Stages(ctx context.Context, buildID int64) ([]*Stage, error) {
	return collect(ctx, s.db, scanStage, `SELECT build_id, name, needs, status, exit_code, started_at, finished_at, ran_on, tries, reused_job, reused_number, reused_stage FROM stages WHERE build_id = ? ORDER BY seq`, buildID)
}

// StoppedStages are the stages of the builds that ended badly: the failed
// ones, and those a timeout or a lost server stopped (cancelled, or still
// running), by build id and in the order the stages were put. One query
// serves every build of a page.
func (s *Store) StoppedStages(ctx context.Context, buildIDs []int64) (map[int64][]*Stage, error) {
	if len(buildIDs) == 0 {
		return nil, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(buildIDs)), ",")
	args := make([]any, 0, len(buildIDs)+3)
	for _, id := range buildIDs {
		args = append(args, id)
	}
	args = append(args, StageFailed, StageCancelled, StageRunning)
	rows, err := collect(ctx, s.db, scanStage, `SELECT build_id, name, needs, status, exit_code, started_at, finished_at, ran_on, tries, reused_job, reused_number, reused_stage FROM stages
		WHERE build_id IN (`+marks+`) AND status IN (?, ?, ?) ORDER BY build_id, seq`, args...)
	if err != nil {
		return nil, err
	}
	out := map[int64][]*Stage{}
	for _, st := range rows {
		out[st.BuildID] = append(out[st.BuildID], st)
	}
	return out, nil
}

func scanStage(row scanner) (*Stage, error) {
	var st Stage
	var needs string
	var code sql.NullInt64
	var started, finished sql.NullString
	var reused StageRef
	if err := row.Scan(&st.BuildID, &st.Name, &needs, &st.Status, &code, &started, &finished, &st.RanOn, &st.Tries, &reused.Job, &reused.Number, &reused.Stage); err != nil {
		return nil, err
	}
	if reused.Job != "" {
		st.Reused = &reused
	}
	if err := json.Unmarshal([]byte(needs), &st.Needs); err != nil {
		return nil, fmt.Errorf("stored needs of stage %s: %w", st.Name, err)
	}
	if code.Valid {
		c := int(code.Int64)
		st.ExitCode = &c
	}
	var err error
	if st.StartedAt, err = parseTS(started); err != nil {
		return nil, err
	}
	if st.FinishedAt, err = parseTS(finished); err != nil {
		return nil, err
	}
	return &st, nil
}
