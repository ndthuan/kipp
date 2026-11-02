package store

import (
	"context"
	"database/sql"
	"strings"
)

// Artifact is one file a stage of a build kept.
type Artifact struct {
	Stage, Path string
	Size        int64
	SHA256      string
}

// PutArtifacts records what a stage kept, in place of what it had.
func (s *Store) PutArtifacts(ctx context.Context, buildID int64, stage string, files []Artifact) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM artifacts WHERE build_id = ? AND stage = ?`, buildID, stage); err != nil {
			return err
		}
		for _, f := range files {
			if _, err := tx.ExecContext(ctx, `INSERT INTO artifacts (build_id, stage, path, size, sha256) VALUES (?, ?, ?, ?, ?)`,
				buildID, stage, f.Path, f.Size, f.SHA256); err != nil {
				return err
			}
		}
		return nil
	})
}

// Artifacts of a build, by stage and path.
func (s *Store) Artifacts(ctx context.Context, buildID int64) ([]Artifact, error) {
	return collect(ctx, s.db, scanArtifact, `SELECT stage, path, size, sha256 FROM artifacts WHERE build_id = ? ORDER BY stage, path`, buildID)
}

// StageArtifacts of a build's one stage, by path.
func (s *Store) StageArtifacts(ctx context.Context, buildID int64, stage string) ([]Artifact, error) {
	return collect(ctx, s.db, scanArtifact, `SELECT stage, path, size, sha256 FROM artifacts WHERE build_id = ? AND stage = ? ORDER BY path`, buildID, stage)
}

func scanArtifact(row scanner) (a Artifact, err error) {
	return a, row.Scan(&a.Stage, &a.Path, &a.Size, &a.SHA256)
}

// LatestArtifacts is the newest build of the job whose status is one of
// statuses and whose stage kept something, or kept path when it is not
// empty; ErrNotFound when there is none.
func (s *Store) LatestArtifacts(ctx context.Context, job, stage, path string, statuses []Status) (int64, error) {
	q := `SELECT b.id FROM builds b WHERE b.job = ? AND EXISTS (SELECT 1 FROM artifacts a WHERE a.build_id = b.id AND a.stage = ?`
	args := []any{job, stage}
	if path != "" {
		q += ` AND a.path = ?`
		args = append(args, path)
	}
	marks := make([]string, len(statuses))
	for i, st := range statuses {
		marks[i] = "?"
		args = append(args, st)
	}
	q += `) AND b.status IN (` + strings.Join(marks, ",") + `) ORDER BY b.id DESC LIMIT 1`
	var id int64
	err := s.db.QueryRowContext(ctx, q, args...).Scan(&id)
	return id, notFound(err)
}

// ArtifactBuild is a build that keeps artifacts, with what they come to.
type ArtifactBuild struct {
	ID     int64
	Job    string
	Number int
	Size   int64
}

// ArtifactBuilds are the ended builds that keep artifacts and were not kept
// out of retention, oldest first, and what every build's artifacts come to,
// running and kept ones included.
func (s *Store) ArtifactBuilds(ctx context.Context) ([]ArtifactBuild, int64, error) {
	var total int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size), 0) FROM artifacts`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := collect(ctx, s.db, func(row scanner) (b ArtifactBuild, err error) {
		return b, row.Scan(&b.ID, &b.Job, &b.Number, &b.Size)
	}, `SELECT b.id, b.job, b.number, SUM(a.size) FROM artifacts a JOIN builds b ON b.id = a.build_id
		WHERE b.status NOT IN ('queued','running') AND b.kept_at IS NULL GROUP BY b.id ORDER BY b.id`)
	return rows, total, err
}

// DeleteArtifacts forgets every file a build kept.
func (s *Store) DeleteArtifacts(ctx context.Context, buildID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM artifacts WHERE build_id = ?`, buildID)
	return err
}
