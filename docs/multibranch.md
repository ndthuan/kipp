# Multibranch jobs

A job with `multibranch:` builds one repo's branches, pull requests and
pushed tags, each as a job of its own, a child, whose stages are read from a file in
the very commit it builds. It is Jenkins' Multibranch Pipeline in kipp's
terms: [`source`](jobs.md#stages-from-a-repo-source) plus discovery.

The job file stays in the jobs directory and holds what a repo may not
grant itself: secrets, locks, concurrency, timeout, keep, cron, `after`
and the hooks. The repo's file holds stages and an `env:` and nothing
else.

- [The job file](#the-job-file)
- [What is built](#what-is-built)
- [Tags](#tags)
- [The scan](#the-scan)
- [Commit statuses](#commit-statuses)
- [Pull requests](#pull-requests)
- [Children](#children)
- [What a child's stages get](#what-a-childs-stages-get)
- [Webhook](#webhook)

## The job file

```yaml
name: site
group: site
multibranch:
  repo: acme/site                  # owner/name, for the API and the hook
  clone: git@github.com:acme/site.git   # optional; default https://github.com/<repo>.git
  path: .kipp.yml                  # default .kipp.yml
  branches: [default]              # default [default]: only the default branch
  pull_requests: merge             # merge (default) | head | both | off
  branch_with_pr: skip             # skip (default) | build
  rebuild_on_base: false           # default false
  tags: {exclude: ["*-rc*"]}       # default: every pushed tag; false for none
  scan: 30m                        # default 30m, at least 5m; 0 turns it off
  orphans: {days: 7}               # default 7
  token: GITHUB_TOKEN_ACME         # a kipp secret's name; optional
secrets: [DEPLOY_KEY]
locks: [site-ports]
triggers:
  cron: "H 3 * * *"                # builds the default branch's child
keep: {builds: 50}
```

and in the repo, `.kipp.yml`:

```yaml
env: {MODE: fast}              # every stage's; optional
stages:
  test: {run: make test, junit: "reports/*.xml"}
  deploy: {needs: [test], when: {default_branch: true}, env: {TARGET: staging}, run: ./deploy.sh}
  release: {needs: [test], when: {tags: ["v*"]}, run: ./release.sh}
```

- `multibranch` takes the place of `stages` and `source`. It refuses
  `triggers.webhook` (the job has a webhook of its own, [below](#webhook))
  and an absolute `workdir`; a relative one is a path inside the clone.
- `params` apply to every child.
- `fingerprint` ([Reusing a green run](jobs.md#reusing-a-green-run-reuse-and-fingerprint))
  is taken for every child's build, and a stage of the repo's file may
  name `reuse` without a `tree`: it gates the checkout kipp made, whose
  tree (for a pull request, the merge's) each child build records. An
  owner job's repo replaces the fingerprint with
  `owner.repos.<name>.fingerprint`
  ([Organization folders](organization-folders.md)).
- The file's `env:`, and a stage's own, are variables as the job file's
  are, in the same order ([Variables](jobs.md#variables)). The file may not
  list secrets, and a name that starts with `KIPP_`, `CI` and the name of
  a secret the job lists are refused there: such a build fails before any
  stage, with a line in its `prepare` log naming the file and the key.
- `kipp check` reads the job file with no server and no network; the
  repo's file is read when a build runs.
- [`examples/jobs/site-multibranch.yml`](../examples/jobs/site-multibranch.yml)
  is the example above under another name.

## What is built

By default: the default branch, and every other branch only once it has
a pull request. A branch pushed with no pull request builds nothing;
opening the pull request builds it at once, and from then on every push
to it builds. `branches` takes `default` (the repo's default branch,
whatever it is called), names and `path.Match` patterns; `["*"]` builds
every branch, with a pull request or without.

| Event | Built |
|---|---|
| push to a branch `branches` matches, no open pull request from it | the branch's child, at the pushed head |
| push to a branch with an open pull request, `branch_with_pr: skip` | nothing for the branch: the pull request's `synchronize` builds the pull request's child |
| same, `branch_with_pr: build` | the branch's child too |
| pull request opened, reopened, synchronized, or its base changed | the pull request's child (unless `pull_requests: off`) |
| pull request closed or merged | its child becomes gone, and its queued and running builds are cancelled |
| push to a base branch with open pull requests | nothing for them, unless `rebuild_on_base: true` |
| branch deleted | its child becomes gone, and its queued and running builds are cancelled |
| push of a tag `tags` lets through | the tags' child, at the commit the tag names ([Tags](#tags)) |
| tag deleted, or a tag `tags` leaves out | nothing |

## Tags

A multibranch job builds every tag pushed to its repo, with no key
needed. `tags` narrows that with the [tag filter](jobs.md#tag-filters)
every tag-building job shares: `tags: {include: ["v*"], exclude:
["*-rc*"]}` builds `v1.2.0` and not `v1.2.0-rc1` (an exclude wins), and
`tags: false` builds no tag.

**A job that has a multibranch or owner job builds every tag once kipp
is upgraded to a version with tag builds.** A tag's build runs the
stages of the tagged commit's `.kipp.yml` whose `when` lets it: a stage
with no `when` runs, as on any build, and so does one with `when:
{default_branch: false}`, which a tag's build is not on. A job whose
stages should not run for a tag says `tags: false`, or gives those
stages `when: {default_branch: true}` or `when: {tags: [...]}`.

- Every tag is built by one child, `<parent>.tags` (kind `tag`), with a
  build per tag pushed, so the job's page lists it once, with the last
  tag built, and its build numbers count the tags.
- A tag's build clones the commit the tag names, in a clean workspace,
  and reads the stages from `.kipp.yml` there; for an annotated tag kipp
  takes the push's `head_commit.id`, the tagged commit, not the tag
  object. Its stages get `KIPP_TAG` (`v1.2.0`), an empty `KIPP_BRANCH`,
  `KIPP_REF` `refs/tags/<tag>` and `KIPP_COMMIT`, so `when: {tags:
  ["v*"]}` holds and `when: {default_branch: true}` does not.
- A tag deleted builds nothing, and a branch's push never builds the
  tags' child. Two tags pushed on one commit are two builds; the same
  push delivered twice (two hooks of one repo) is built once.
- The tags' child cannot be built by hand: Build on its page is refused
  (409). Build again on a tag's build builds the same tag at the same
  commit.
- Another job's `after: [<parent>]` runs when a tag's build ends
  `success` or `unstable`, as it does after the default branch's, with
  `KIPP_UPSTREAM_TAG` the tag (empty after the default branch's build;
  [after](jobs.md#after)).
- Its commit status is `kipp/<parent>/tag`, so it never overwrites the
  default branch's `kipp/<parent>` on the same commit
  ([Commit statuses](#commit-statuses)).
- It keeps the parent's `keep`, and a tag's build that ends `success` or
  `unstable` is kept by kipp, as the record of a release, until someone
  stops keeping it ([Keeping a build](builds.md#keeping-a-build)).
- A scan lists branches and pull requests, never tags: a tag pushed while
  kipp was not listening is built by pushing it again, or by Build again
  on an earlier build of it.

An ordinary job with `triggers.webhook.tags` ([webhook](jobs.md#webhook))
still builds tags as before, for a release job outside the repo.

## The scan

Webhooks can be missed, so kipp also reads the repo itself. With `token`
set (the name of a kipp secret holding a GitHub token that may read the
repo), kipp scans each multibranch job when the server starts and at
every reload of the job files, then every `scan` (default `30m`, at least
`5m`, `0` for never).

A scan lists the branches and open pull requests through the GitHub API
(`serve --github-api`, default `https://api.github.com`, for GitHub
Enterprise), makes a child only where the file at `path` exists in that
commit, marks as gone what it no longer finds, and queues a build, with
trigger `scan`, for a head no build has seen and none is queued or
running for. The first scan builds each candidate once. At most two scans
run at once, and a failed scan changes nothing: its error is on the
parent's page, never with the token in it.

**Scan now** on the parent's page, or `POST /api/v1/jobs/<parent>/scan`
(answers 202; needs the `build` grant), runs one at once. A job with no
`token` is not scanned, and its page says so: no scan and no statuses.
The token is the server's: a stage sees it only if the job lists it in
`secrets`. A stage may be limited to scan builds with
`when: {triggers: [scan]}`.

## Commit statuses

With a `token`, a child's build also says its state on the commit it
built, so that a pull request shows it. The token needs the `repo:status`
scope (a fine-grained token: Commit statuses, read and write). kipp
posts `pending` once the clone has told it which commit it builds, then,
when the build ends:

| The build ended | State |
|---|---|
| success, unstable | `success` |
| a stage failed, or the build timed out | `failure` |
| cancelled, lost (the server stopped under it) | `error` |
| it failed before any stage ran (no file at `path`, a clone or a merge that failed) | `error` |

The context is `kipp/<parent>`; under `pull_requests: both` the build of
a pull request's head alone is `kipp/<parent>/head`, and a tag's build is
`kipp/<parent>/tag`, so that two builds of one commit do not overwrite
one another. The description is
`<child> #<n> <status>`, and `target_url` is the build's page under
`serve --base-url`, left out when that flag is not given. For a pull
request the commit is its head, not the merge. A status that cannot be
posted (the token lacks the scope, GitHub does not answer) is one line in
the build's `hooks` log, `kipp: commit status <state> not posted: ...`,
and never changes the build.

## Pull requests

Under `pull_requests: merge` (the default), a pull request's build builds
its head merged into the tip of its base branch as of the build, merged
by kipp itself: it fetches the base branch and the head with their
history (`--filter=blob:none`, no `--depth`), checks the base tip out,
merges the head into it with `git merge-tree --write-tree` (git 2.38 or
newer) and checks out the merge commit, made as `kipp <kipp@localhost>`
with the base and the head as its parents. The base
tip is fetched when the build starts, not taken from the event, so a base
that moved since is the one merged into; GitHub's `refs/pull/<n>/merge`
is not used, because GitHub makes it later and it can still be the merge
before the last push. `.kipp.yml` is read from the merged tree. A head
that does not merge fails the build before any stage, with
`kipp: <head branch> does not merge cleanly into <base>@<short sha>` in
its `prepare` log.

`head` builds the head commit alone, and `both` makes two children,
`pr-<n>` (the merge) and `pr-<n>-head` (the head alone).

A pull request whose head is in another repo, a fork, is never built:
its code would run with the job's secrets on the server's machine. It is
listed on the parent's page as not built.

## Children

- A child is a job named `<parent>.<key>`. For a branch, the key is its
  name lower-cased with every character outside `[a-z0-9._-]` as `-`, and
  `-<6 hex digits>` of the SHA-256 of the name after it when that changed
  anything or the key is another child's (`main` stays `site.main`;
  `feature/login` is `site.feature-login-<hex>`). For a pull request it is
  `pr-<n>`, and `pr-<n>-head` for its head alone under `both`. The pushed
  tags are the child `tags`, which a branch called `tags` never takes
  (it gets the hex suffix). A job file whose name starts with a
  multibranch job's name and a dot is refused.
- A child has everything of its parent but its cron, `after` and webhook,
  and has its own build numbers, history and concurrency. So
  `rerun_on_restart: true` on the parent holds for the build of every
  branch and pull request. `locks` are
  shared by name as always, so builds that must take turns across
  branches name a lock.
- `cron` and `after` belong to the parent and build the default branch's
  child, and so does a build of the parent asked for by hand (its Build
  button, `POST /api/v1/jobs/<parent>/builds`, `kipp run <parent>`).
  Another job's `after: [site]` runs when the default branch's child or
  a tag's build succeeds or ends unstable, and after no other child; `after: [site.main]`
  is refused, with `site` as the name to use. Until kipp has
  heard of the default branch (a push to it, or a scan, makes its child),
  such a build is refused.
- A child's build clones `clone` at the commit a push named, or, when no
  push started it (cron, after, api, web), at the tip of its branch as of
  the clone, and records the commit it built (`built_commit` in
  `GET /api/v1/builds/<id>`). A commit without the file fails the build
  before any stage, with `kipp: no .kipp.yml at <short sha>` (the path as
  configured) in its `prepare` log. A build of a pull request asked for
  by hand builds its head as GitHub has it then, `refs/pull/<n>/head`.
- A child whose branch is deleted or whose pull request is closed is
  gone: it builds no more and is listed apart on the parent's page. A
  build of it still queued or running is cancelled, its log ending `kipp:
  cancelled: pull request #<n> is closed` (or `branch <name> is gone`) and
  the child's activity saying the same. The commit's status then goes back
  to the verdict of the child's build of it before, when there was one: a
  pull request merged the moment it went green moves its base, and under
  `rebuild_on_base` the rebuild on that base can start before GitHub says
  the pull request closed. Without one the status is `error`. After
  `orphans.days` it leaves the job set, and its builds are then kept as a
  removed job's are (90 days). A branch pushed again, or a pull request
  reopened, is the same child again.
- The job list shows the parent once, with how many children it builds
  and the worst status of their last builds. Its page lists them (a
  branch, or `PR #n` with its title and author, the head commit, the last
  build), then the gone ones, then the pull requests from forks it does
  not build. A child's page links up to its parent.
  `GET /api/v1/jobs/<parent>/children` answers the same rows (`job`,
  `key`, `kind`, `branch`, `pr`, `title`, `author`, `url`, `head_commit`,
  `base_branch`, `from_fork`, `state`, `gone_at`, `last_seen_at`, `last`).
- A grant on the parent is a grant on each of its children, and the
  grants editor lists parents only. A child gone from the job set, or one
  whose parent's file was removed, stays readable by whoever holds the
  parent's grant for as long as any of its builds is kept.
- Security is that of `source`: whoever can push to the repo runs
  commands on the server's machine with the job's secrets.

## What a child's stages get

Beside the [variables of every build](jobs.md#the-variables-kipp-sets),
a child's stages get:

- `KIPP_COMMIT`: the head commit built, whatever started the build;
- `KIPP_BRANCH`: its branch, for every trigger; on a pull request's
  build, the head branch; on a tag's build, empty, and `KIPP_TAG` is the
  tag;
- `KIPP_DEFAULT_BRANCH`;
- `KIPP_PR` (the number), `KIPP_BASE_BRANCH`, `KIPP_BASE_COMMIT` (under
  `merge`, the base tip merged into), `KIPP_PR_TITLE`, `KIPP_PR_URL` (the
  pull request's page) and `KIPP_PR_AUTHOR` (the login that opened it),
  which are set and empty on a branch's build.

The title, page and author are read from the child's row when the build
prepares; a webhook or a scan writes them, and writes them again when
they change. The build records the same (`pr`, `base_branch`,
`base_commit`). A child's `when: {default_branch: true}` holds on the
default branch's child alone, whatever started the build: never on a
pull request, Build by hand included.

A stage on a runner shares no files with the server, so one that needs
the tree clones it, and redoes the merge from those variables:

```sh
git init -q . && git remote add origin "$REPO_URL"
git fetch origin "$KIPP_BASE_COMMIT" "$KIPP_COMMIT" &&
  git checkout --detach "$KIPP_BASE_COMMIT" &&
  git merge --no-ff --no-edit "$KIPP_COMMIT"
```

(`REPO_URL` being the stage's own way to the repo, and the merge made
under whatever identity the runner's git has; kipp's is
`-c user.name=kipp -c user.email=kipp@localhost`.)

## The build's token

A child's `KIPP_TOKEN` views the child and nothing else unless its
`.kipp.yml` asks for more with `permissions:`, written as in
[a job file](jobs.md#the-builds-token-permissions); `self` is the child
itself. Anyone who can push can write that file, so the job file keeps a
ceiling:

```yaml
max_permissions:
  site-deploy: [view, build]
  self: cancel
```

A `.kipp.yml` that asks for a permission outside it fails the build before
its first stage, with the permissions over the ceiling and `job site would
have to allow them in max_permissions`. With no `max_permissions:` the
ceiling is the default, `view` on the child itself. The job's own
`permissions:` is not inherited by the children; only `max_permissions:`
concerns them. Grants are compared by job name and action as written, so a
job the child would inherit through its parent's grant is not inside the
ceiling unless it is named.

## Webhook

The parent's webhook is `POST /api/v1/hooks/github/<parent>`, signed
with the secret `GITHUB_WEBHOOK_<PARENT>` and checked exactly as any
job's ([webhook](jobs.md#webhook)), for the events `push` (of a branch or
a tag) and `pull_request`. An event it builds is answered 202 with
`{"builds": [{"job", "id", "number"}]}` (a build the child's concurrency
dropped says `error` in place of the id, and a commit that has a build
queued or running already says `skipped`); one it builds nothing for, 200
with `ignored` and why. A child takes no deliveries of its own.
