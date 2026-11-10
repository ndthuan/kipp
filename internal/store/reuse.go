package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// StageRef names one stage of one build.
type StageRef struct {
	Job    string
	Number int
	Stage  string
}

// SetBuildTree records the git tree of the checkout kipp made for a build.
func (s *Store) SetBuildTree(ctx context.Context, id int64, tree string) error {
	return requireChange(s.db.ExecContext(ctx, `UPDATE builds SET built_tree = ? WHERE id = ?`, tree, id))
}

// SetBuildFingerprint records the joined output of the build's fingerprint
// commands and its SHA-256.
func (s *Store) SetBuildFingerprint(ctx context.Context, id int64, text, sum string) error {
	return requireChange(s.db.ExecContext(ctx, `UPDATE builds SET fingerprint = ?, fingerprint_sum = ? WHERE id = ?`, text, sum, id))
}

// BuildTree is the tree and the fingerprint's sum recorded for a build.
func (s *Store) BuildTree(ctx context.Context, id int64) (tree, fingerprintSum string, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT built_tree, fingerprint_sum FROM builds WHERE id = ?`, id).Scan(&tree, &fingerprintSum)
	return tree, fingerprintSum, notFound(err)
}

// SetStageRun records, for a stage that ran, the class of place it ran on
// and the tree it gated; an empty tree is none of its own, and the build's
// counts.
func (s *Store) SetStageRun(ctx context.Context, buildID int64, name, class, tree string) error {
	return requireChange(s.db.ExecContext(ctx, `UPDATE stages SET runner_class = ?, tree = ? WHERE build_id = ? AND name = ?`, class, tree, buildID, name))
}

// ReuseStage ends a stage success without a run, as reused from the green
// run from: no exit code, and started when it finished.
func (s *Store) ReuseStage(ctx context.Context, buildID int64, name, class string, from StageRef, at time.Time) error {
	return requireChange(s.db.ExecContext(ctx, `UPDATE stages SET status = ?, exit_code = NULL, started_at = ?, finished_at = ?, runner_class = ?,
		reused_job = ?, reused_number = ?, reused_stage = ? WHERE build_id = ? AND name = ?`,
		StageSuccess, ts(at), ts(at), class, from.Job, from.Number, from.Stage, buildID, name))
}

// GreenQuery is what a green run must match to be reused.
type GreenQuery struct {
	Tree, FingerprintSum, Class string
	// From are job names, each standing for the job and every job named
	// under it; Stage is the stage that counts in them.
	From  []string
	Stage string
	// Job and JobStage are the asking build's own job and stage, which
	// count too; Build is the asking build, which never does.
	Job, JobStage string
	Build         int64
}

// GreenStage is the newest stage of a build still kept that matches q and
// ended success by running: its tree (the stage's own, else its build's),
// its build's fingerprint and its runner class equal q's. Nil when none.
func (s *Store) GreenStage(ctx context.Context, q GreenQuery) (*StageRef, error) {
	if q.Tree == "" || q.FingerprintSum == "" {
		return nil, nil
	}
	scopes := []string{`(s.name = ? AND b.job = ?)`}
	args := []any{StageSuccess, q.Class, q.FingerprintSum, q.Build, q.Tree, q.Tree, q.JobStage, q.Job}
	for _, from := range q.From {
		scopes = append(scopes, `(s.name = ? AND (b.job = ? OR substr(b.job, 1, ?) = ?))`)
		args = append(args, q.Stage, from, len(from)+1, from+".")
	}
	var ref StageRef
	err := s.db.QueryRowContext(ctx, `SELECT b.job, b.number, s.name FROM stages s JOIN builds b ON b.id = s.build_id
		WHERE s.status = ? AND s.reused_job = '' AND s.runner_class = ? AND b.fingerprint_sum = ? AND b.id != ?
		AND (s.tree = ? OR (s.tree = '' AND b.built_tree = ?))
		AND (`+strings.Join(scopes, " OR ")+`)
		ORDER BY b.id DESC LIMIT 1`, args...).Scan(&ref.Job, &ref.Number, &ref.Stage)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ref, nil
}
