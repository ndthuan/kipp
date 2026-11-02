package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Child is one row of children: a branch or a pull request of a
// multibranch job's repo, built as the job <parent>.<key> or, from a fork,
// only listed.
type Child struct {
	Parent, Key, Kind, Branch string
	PR                        int
	Title, HeadSHA            string
	// Author and URL are a pull request's: the login that opened it and
	// its page. Empty on a row made before they were kept, until the next
	// event or scan fills them.
	Author, URL string
	BaseBranch  string
	FromFork    bool
	State       string
	GoneAt      *time.Time
	LastSeenAt  time.Time
}

// Job is the child's job name.
func (c Child) Job() string { return c.Parent + "." + c.Key }

const childCols = `parent, key, kind, branch, pr, title, head_sha, base_branch, from_fork, state, gone_at, last_seen_at, author, url`

func scanChild(row scanner) (Child, error) {
	var c Child
	var gone sql.NullString
	var seen string
	if err := row.Scan(&c.Parent, &c.Key, &c.Kind, &c.Branch, &c.PR, &c.Title, &c.HeadSHA, &c.BaseBranch, &c.FromFork, &c.State, &gone, &seen, &c.Author, &c.URL); err != nil {
		return c, err
	}
	var err error
	if c.GoneAt, err = parseTS(gone); err != nil {
		return c, err
	}
	c.LastSeenAt, err = parseSetTS(seen)
	return c, err
}

// PutChild writes the row of the child, whatever it held before.
func (s *Store) PutChild(ctx context.Context, c Child) error {
	var gone any
	if c.GoneAt != nil {
		gone = ts(*c.GoneAt)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO children (`+childCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(parent, key) DO UPDATE SET kind = excluded.kind, branch = excluded.branch, pr = excluded.pr, title = excluded.title,
	head_sha = excluded.head_sha, base_branch = excluded.base_branch, from_fork = excluded.from_fork, state = excluded.state,
	gone_at = excluded.gone_at, last_seen_at = excluded.last_seen_at, author = excluded.author, url = excluded.url`,
		c.Parent, c.Key, c.Kind, c.Branch, c.PR, c.Title, c.HeadSHA, c.BaseBranch, c.FromFork, c.State, gone, ts(c.LastSeenAt), c.Author, c.URL)
	return err
}

// Children of the parent in key order; of every parent for "".
func (s *Store) Children(ctx context.Context, parent string) ([]Child, error) {
	if parent == "" {
		return collect(ctx, s.db, scanChild, `SELECT `+childCols+` FROM children ORDER BY parent, key`)
	}
	return collect(ctx, s.db, scanChild, `SELECT `+childCols+` FROM children WHERE parent = ? ORDER BY key`, parent)
}

// DeleteChild removes the row of a child no build of which is left.
func (s *Store) DeleteChild(ctx context.Context, parent, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM children WHERE parent = ? AND key = ?
	AND NOT EXISTS (SELECT 1 FROM builds WHERE job = children.parent || '.' || children.key)`, parent, key)
	return err
}

// ParentOf is the multibranch job whose child job is, "" when job is no
// child. It is read from the rows and not from the name: a job of a file
// may have a dot in its name.
func (s *Store) ParentOf(ctx context.Context, job string) (string, error) {
	var parent string
	err := s.db.QueryRowContext(ctx, `SELECT parent FROM children WHERE parent || '.' || key = ?`, job).Scan(&parent)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return parent, err
}

// Parents maps the name of every child job to its parent's.
func (s *Store) Parents(ctx context.Context) (map[string]string, error) {
	rows, err := s.Children(ctx, "")
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, c := range rows {
		out[c.Job()] = c.Parent
	}
	return out, nil
}

// SetDefaultBranch keeps the repo's default branch as a multibranch job
// last heard it.
func (s *Store) SetDefaultBranch(ctx context.Context, parent, branch string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO multibranch (parent, default_branch, updated_at) VALUES (?, ?, ?)
	ON CONFLICT(parent) DO UPDATE SET default_branch = excluded.default_branch, updated_at = excluded.updated_at`, parent, branch, ts(at))
	return err
}

// DefaultBranches is the default branch each multibranch job last heard.
func (s *Store) DefaultBranches(ctx context.Context) (map[string]string, error) {
	type pair struct{ parent, branch string }
	rows, err := collect(ctx, s.db, func(row scanner) (p pair, err error) {
		return p, row.Scan(&p.parent, &p.branch)
	}, `SELECT parent, default_branch FROM multibranch`)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, p := range rows {
		out[p.parent] = p.branch
	}
	return out, nil
}
