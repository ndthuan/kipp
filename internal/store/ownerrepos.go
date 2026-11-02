package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// The states of a repo's webhook: kipp keeps it (HookOK), or cannot and
// says why in the row's note (HookOff); empty is not looked at yet.
const (
	HookOK  = "ok"
	HookOff = "off"
)

// OwnerRepo is one row of owner_repos: a repo of an owner job, which is
// the multibranch job <owner>.<key>.
type OwnerRepo struct {
	Owner, Key, Repo, State string
	GoneAt                  *time.Time
	LastSeenAt              time.Time
	// Hook is HookOK, HookOff or empty; HookNote says why it is off, and
	// HookFP is the fingerprint of the hook as kipp last wrote it.
	Hook, HookNote, HookFP string
}

// Node is the name of the repo's job.
func (r OwnerRepo) Node() string { return r.Owner + "." + r.Key }

const ownerRepoCols = `owner, key, repo, state, gone_at, last_seen_at, hook, hook_note, hook_fp`

func scanOwnerRepo(row scanner) (OwnerRepo, error) {
	var r OwnerRepo
	var gone sql.NullString
	var seen string
	if err := row.Scan(&r.Owner, &r.Key, &r.Repo, &r.State, &gone, &seen, &r.Hook, &r.HookNote, &r.HookFP); err != nil {
		return r, err
	}
	var err error
	if r.GoneAt, err = parseTS(gone); err != nil {
		return r, err
	}
	r.LastSeenAt, err = parseSetTS(seen)
	return r, err
}

// PutOwnerRepo writes the row of the repo, whatever it held before.
func (s *Store) PutOwnerRepo(ctx context.Context, r OwnerRepo) error {
	var gone any
	if r.GoneAt != nil {
		gone = ts(*r.GoneAt)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO owner_repos (`+ownerRepoCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(owner, key) DO UPDATE SET repo = excluded.repo, state = excluded.state, gone_at = excluded.gone_at,
	last_seen_at = excluded.last_seen_at, hook = excluded.hook, hook_note = excluded.hook_note, hook_fp = excluded.hook_fp`,
		r.Owner, r.Key, r.Repo, r.State, gone, ts(r.LastSeenAt), r.Hook, r.HookNote, r.HookFP)
	return err
}

// OwnerRepos are the repos of the owner job in key order; of every owner
// job for "".
func (s *Store) OwnerRepos(ctx context.Context, owner string) ([]OwnerRepo, error) {
	if owner == "" {
		return collect(ctx, s.db, scanOwnerRepo, `SELECT `+ownerRepoCols+` FROM owner_repos ORDER BY owner, key`)
	}
	return collect(ctx, s.db, scanOwnerRepo, `SELECT `+ownerRepoCols+` FROM owner_repos WHERE owner = ? ORDER BY key`, owner)
}

// DeleteOwnerRepo removes the row of a repo none of whose children is
// left, and the default branch kept for it.
func (s *Store) DeleteOwnerRepo(ctx context.Context, owner, key string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	node := owner + "." + key
	res, err := tx.ExecContext(ctx, `DELETE FROM owner_repos WHERE owner = ? AND key = ?
	AND NOT EXISTS (SELECT 1 FROM children WHERE parent = ?) AND NOT EXISTS (SELECT 1 FROM builds WHERE job = ?)`, owner, key, node, node)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM multibranch WHERE parent = ?`, node); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// FolderOf is the owner job whose repo the job is, or whose repo's child
// it is: the grant that reaches the job from above. It is read from the
// rows and not from the name, since a job of a file may hold a dot. A job
// of no owner job is "".
func (s *Store) FolderOf(ctx context.Context, job string) (string, error) {
	var owner string
	err := s.db.QueryRowContext(ctx, `SELECT owner FROM owner_repos WHERE owner || '.' || key = ?
	OR owner || '.' || key = (SELECT parent FROM children WHERE parent || '.' || key = ?)`, job, job).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return owner, err
}

// Folders maps the name of every repo's job and of every child of one to
// its owner job's.
func (s *Store) Folders(ctx context.Context) (map[string]string, error) {
	repos, err := s.OwnerRepos(ctx, "")
	if err != nil {
		return nil, err
	}
	nodes := make(map[string]string, len(repos))
	out := make(map[string]string, len(repos))
	for _, r := range repos {
		nodes[r.Node()] = r.Owner
		out[r.Node()] = r.Owner
	}
	kids, err := s.Children(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, c := range kids {
		if owner, ok := nodes[c.Parent]; ok {
			out[c.Job()] = owner
		}
	}
	return out, nil
}
