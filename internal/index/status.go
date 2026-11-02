package index

import (
	"context"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/dispatch"
	"github.com/ndthuan/kipp/internal/github"
)

// PostStatus is the dispatcher's way of saying a build's state to the repo
// of its job: it opens the secret the job names in multibranch.token and
// posts the status to the API at api (GitHub's own when empty). A job that
// names no token gets no call at all, and a secret that cannot be opened is
// an error that names it and never holds its value.
func PostStatus(api string, token func(ctx context.Context, name string) (string, error)) func(context.Context, *config.Job, dispatch.CommitStatus) error {
	return PostStatusFor(api, token, nil)
}

// PostStatusFor is PostStatus for a server that can also sign in as the
// GitHub Apps of its owner jobs: a repo of one posts as its App's
// installation on the account.
func PostStatusFor(api string, token func(ctx context.Context, name string) (string, error), app AppClient) func(context.Context, *config.Job, dispatch.CommitStatus) error {
	return func(ctx context.Context, job *config.Job, s dispatch.CommitStatus) error {
		m := job.Multibranch
		if m == nil || !m.Authenticated() {
			return nil
		}
		gh, err := clientFor(ctx, api, token, app, m.App, m.Token, repoOwner(m.Repo))
		if err != nil {
			return err
		}
		return gh.PostStatus(ctx, m.Repo, s.SHA, github.Status{
			State: s.State, Context: s.Context, Description: s.Description, TargetURL: s.TargetURL,
		})
	}
}
