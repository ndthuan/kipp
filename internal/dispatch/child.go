package dispatch

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strconv"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

func ofChild(b *store.Build, job *config.Job) {
	if job.Origin == nil {
		return
	}
	b.Parent = job.Parent
	if b.TriggerBranch == "" {
		b.TriggerBranch = job.Origin.Branch
	}
	if b.TriggerDefaultBranch == "" {
		b.TriggerDefaultBranch = job.DefaultBranch
	}
	if config.IsPR(job.Origin.Kind) {
		b.PR, b.BaseBranch = job.Origin.PR, job.Origin.BaseBranch
	}
}

func childVars(b *store.Build, origin *config.ChildRecord) []string {
	commit := b.BuiltCommit
	if commit == "" {
		commit = b.Commit()
	}
	pr := ""
	if b.PR > 0 {
		pr = strconv.Itoa(b.PR)
	}
	var title, prURL, author string
	if origin != nil && config.IsPR(origin.Kind) {
		title, prURL, author = origin.Title, origin.URL, origin.Author
	}
	return []string{"KIPP_COMMIT=" + commit, "KIPP_PR=" + pr, "KIPP_BASE_BRANCH=" + b.BaseBranch, "KIPP_BASE_COMMIT=" + b.BaseCommit,
		"KIPP_PR_TITLE=" + title, "KIPP_PR_URL=" + prURL, "KIPP_PR_AUTHOR=" + author}
}

func childRef(b *store.Build, job *config.Job) string {
	if commit := cmp.Or(b.Commit(), b.BuiltCommit); commit != "" {
		return commit
	}
	switch {
	case job.BuildsTags():
		return "refs/tags/" + b.TriggerTag
	case config.IsPR(job.Origin.Kind):
		return "refs/pull/" + strconv.Itoa(job.Origin.PR) + "/head"
	}
	return "refs/heads/" + job.Origin.Branch
}

func (d *Dispatcher) cloneChild(ctx context.Context, b *store.Build, job *config.Job, e *buildEnv, secrets map[string]Secret) (*config.StageFile, error) {
	mb := job.Multibranch
	masked := *e
	masked.secrets = append(slices.Clip(e.secrets), config.URLCredentials(mb.Clone)...)
	var built, base string
	err := d.logged(b, config.CloneLog, &masked, func(out io.Writer) error {
		var err error
		built, base, err = d.checkout(ctx, b, job, e.workspace, out)
		return err
	})
	if built != "" || base != "" {
		d.setCommits(ctx, b, built, base)
	}
	if built != "" {
		d.say(ctx, b, job, e, "pending", store.Running)
	}
	if err != nil {
		return nil, err
	}
	d.recordTree(ctx, b, e.workspace)
	d.useSecrets(b, job, e, secrets)
	root, err := os.OpenRoot(e.workspace)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(mb.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("kipp: no %s at %s", mb.Path, shortSHA(built))
	}
	if err != nil {
		return nil, err
	}
	file, err := config.ParseStageFile(data)
	if err != nil {
		return nil, fmt.Errorf("%s at %s: %w", mb.Path, shortSHA(built), err)
	}
	return file, nil
}

func (d *Dispatcher) checkout(ctx context.Context, b *store.Build, job *config.Job, dir string, out io.Writer) (string, string, error) {
	repo, ref := job.Multibranch.Clone, childRef(b, job)
	if job.Origin.Kind == config.KindPR {
		return d.merge(ctx, b, job, dir, ref, out)
	}
	if err := d.opts.Clone(ctx, dir, repo, ref, out); err != nil {
		return "", "", fmt.Errorf("clone %s@%s: %w", config.RedactURL(repo), ref, redacted(err, repo))
	}
	built, err := d.opts.Head(ctx, dir)
	if err == nil {
		return built, "", nil
	}
	if ref == b.Commit() {
		return ref, "", nil
	}
	return "", "", fmt.Errorf("kipp: could not tell which commit the clone of %s checked out: %w", ref, err)
}

func (d *Dispatcher) merge(ctx context.Context, b *store.Build, job *config.Job, dir, head string, out io.Writer) (string, string, error) {
	repo, base := job.Multibranch.Clone, job.Origin.BaseBranch
	baseSHA, headSHA, err := d.opts.Merge(ctx, dir, repo, base, b.BaseCommit, head, out)
	switch {
	case errors.Is(err, ErrNoCleanMerge):
		return headSHA, baseSHA, fmt.Errorf("kipp: %s does not merge cleanly into %s@%s", job.Origin.Branch, base, shortSHA(baseSHA))
	case err != nil:
		return headSHA, baseSHA, fmt.Errorf("merge %s into %s of %s: %w", head, base, config.RedactURL(repo), redacted(err, repo))
	}
	return headSHA, baseSHA, nil
}

func (d *Dispatcher) setCommits(ctx context.Context, b *store.Build, built, base string) {
	if err := d.st.SetBuildCommits(context.WithoutCancel(ctx), b.ID, built, base); err != nil {
		d.log.Error("recording the commit a build built", "job", b.Job, "number", b.Number, "err", err)
	}
	d.mu.Lock()
	b.BuiltCommit, b.BaseCommit = built, base
	d.mu.Unlock()
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
