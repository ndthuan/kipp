# Organization folders

A job with `owner:` indexes every repo of one GitHub user or organization
that holds the stages file at its default branch, and makes each a
[multibranch job](multibranch.md) (Jenkins' Organization Folder). One job
file stands for all of an owner's repos: a new repo that gets a
`.kipp.yml` is built from the next scan on, and one that is archived or
loses the file is let go.

- [The job file](#the-job-file)
- [Authentication](#authentication)
- [Which repos](#which-repos)
- [Jobs](#jobs)
- [Webhooks](#webhooks)
- [GitHub App](#github-app)
- [Two jobs, one repo](#two-jobs-one-repo)

## The job file

```yaml
name: acme
owner:
  login: acme                      # a user or an organization
  token: GITHUB_TOKEN_ACME         # a kipp secret's name (or app:, below)
  include: ["*"]                   # repo names and path.Match patterns, any case
  exclude: ["*-archive"]
  clone: https://github.com/%s.git # one %s for owner/name
  path: .kipp.yml
  branches: [default]
  pull_requests: merge
  scan: 30m                        # at least 5m; 0 for never
  orphans: {days: 7}
  repos:
    api:                           # the repo's name, without the owner
      secrets: [API_DEPLOY_KEY]
      locks: [api-deploy]
      env: {REGION: eu}
      fingerprint: [go version]    # replaces the job's for this repo
secrets: [DEPLOY_KEY]
fingerprint: [go version, node --version]
triggers:
  cron: "H 3 * * *"
```

`owner` takes the place of `stages`, `source` and `multibranch`. `path`,
`branches`, `pull_requests`, `branch_with_pr`, `rebuild_on_base`, `scan`
and `orphans` mean what they do in a multibranch job, for every repo.

The job's `secrets`, `locks`, `env`, `timeout`, `keep` and `cron` are the
defaults of every repo. A repo listed under `owner.repos` replaces the
job's `secrets` or `locks` where it gives them (an empty list too), adds
its `env` to the job's, names its own `metrics` (`store` and
`remember`) where the job's would not suit it, and replaces the job's
`fingerprint` with its own `fingerprint` (a list of one or more commands,
never empty) for every build of that repo's node and children
([Reusing a green run](jobs.md#reusing-a-green-run-reuse-and-fingerprint)).
[`examples/jobs/acme.yml`](../examples/jobs/acme.yml) is the example
above.

## Authentication

Exactly one of two keys:

- `token` is the name of a kipp secret holding a GitHub token. A
  fine-grained token needs Contents and Metadata (read), Commit statuses
  (read and write), Pull requests (read) and, for kipp to keep the
  webhooks, Webhooks (read and write); a classic token needs `repo` and
  `admin:repo_hook`.
- `app` signs in as a GitHub App: `app: {id: 123456, key_file:
  /var/lib/kipp/apps/acme.pem}`. kipp signs a JWT with the key, trades it
  for the installation token of the account `login` (the App must be
  installed on it) and keeps that token until five minutes before it
  expires. See [GitHub App](#github-app).

## Which repos

kipp asks GitHub who `login` is. An organization is read through
`/orgs/<login>/repos`, the token's own account through
`/user/repos?affiliation=owner` (its private repos included) and any
other user through `/users/<login>/repos`, where the token sees public
repos only: the owner job's page and the log say so. With an App, the
repos are those it was installed on (`/installation/repositories`).

Archived repos and forks are skipped, then `include` (no `include` is
every repo) and `exclude` are applied to the name, and the repo is kept
where the file at `path` exists at its default branch. A repo that has
left the set, because it was archived, excluded, deleted or lost its
file, is gone like a deleted branch: its jobs are disabled, listed apart,
and leave the job set after `orphans.days`. A repo whose file could not
be looked for this time keeps the state it had.

## Jobs

- A repo is the multibranch job `<owner job>.<key>`, where the key is the
  repo's name lower-cased with every character outside `[a-z0-9_-]` as
  `-` (a dot too, so that `acme.a.b` is always the branch `b` of the repo
  `a`), and `-<6 hex digits>` of the SHA-256 of the name after it when
  that changed anything or another repo has the key. Its children are
  `<owner job>.<key>.<branch>` and `.pr-<n>`, as in a multibranch job.
- The job list shows the owner job once, with how many repos it builds
  and the worst status of their last builds. Its page lists the repos,
  with the state of each webhook, and **Scan now**
  (`POST /api/v1/jobs/<owner job>/scan`, `build` grant) reads the owner
  and every repo at once. `GET /api/v1/jobs/<owner job>` carries an
  `owner` object with `login`, `scan` and the `repos`.
- An owner job builds nothing itself: Build is refused (409), and its
  `cron` builds the default branch of every repo.
- A grant on the owner job is a grant on every repo and every child of
  it, the gone ones too; the grants editor lists owner jobs only.
- A `max_permissions:` on the owner job is the ceiling for the `permissions:`
  of the `.kipp.yml` of every repo and child under it, as it is for a
  multibranch job ([the build's token](multibranch.md#the-builds-token));
  `self` is each child itself.
- A commit status says `kipp/<owner job>` (`kipp/<owner job>/head`), for
  the commit of its repo.

## Webhooks

One URL serves every repo: `POST /api/v1/hooks/github/<owner job>`,
signed with `GITHUB_WEBHOOK_<OWNER JOB>`. A delivery is routed to the
repo it names (`repository.full_name`) and treated as that repo's own
multibranch job would; one from a repo the job does not index answers 200
with `ignored`, and the next scan finds the repo.

With `serve --base-url` given and the secret set, kipp creates the
webhook (events `push` and `pull_request`, JSON) on each live repo at the
scan, and repairs it where it was turned off, lost its events or points
elsewhere. GitHub never returns a secret, so kipp keeps a fingerprint of
what it wrote and writes again when the secret or the URL changes. A hook
is recognised by its URL.

**kipp never deletes a webhook**: the hooks on repos that left the set
are listed on the owner job's page for you to remove. Where kipp cannot
keep a hook (no `--base-url`, the secret not set, a token that may not
manage webhooks, a 404 or 403 from GitHub), the repo is left to the scan,
its row says why, and the log says it once, and again only when it
changes.

## GitHub App

An App has no user: the key file is the whole credential, and the repos
it reaches are the ones it was installed on (all of the account's, or the
ones picked).

```yaml
owner:
  login: acme
  app:
    id: 123456                          # the App's ID
    key_file: /var/lib/kipp/apps/acme.pem
    # name: acme                        # optional, see below
```

- **Permissions** the App needs: Contents read, Pull requests read,
  Commit statuses write, Metadata read (always granted). **Events**: Push
  and Pull request. Webhooks are the App's own, one for all its
  installations, so kipp does not create or repair any hook per repo.
- **The key file** is read on every JWT, so replacing it needs no
  restart. kipp refuses it when group or others can read it
  (`chmod 600`), as ssh does, and an error names the file and never its
  content. PKCS#1 and PKCS#8 RSA keys are read. It is as protected as
  kipp's data directory: a stage that runs on the server as the same OS
  user can read it, as it can the secrets' key, so run untrusted stages
  on a runner, in a container or as another user.
- **The App's name** is the key file's name without its extension,
  lower-cased (`acme.pem` is `acme`), or `app.name` when given:
  lower-case letters, digits and `-`. Two jobs with the same name must
  give the same `id` and `key_file`, and one App may not serve two owner
  jobs of the same `login`.
- **The webhook** of the App is `POST /api/v1/hooks/github-app/<name>`,
  signed with the kipp secret `GITHUB_APP_WEBHOOK_<NAME>` (upper case,
  `-` as `_`): `acme` reads `GITHUB_APP_WEBHOOK_ACME`. Set the same value
  as the App's webhook secret at GitHub. A delivery is routed to the
  owner job whose `login` owns the repository. A verified one that no job
  is for answers 200 with `ignored` and why; a wrong signature or a
  secret that is not set answers 401. The `installation` and
  `installation_repositories` events (an install, or repos added or
  removed) start a scan of that account's owner job at once, which is how
  a new repo is found without waiting for `scan`.
- **Moving from `token:` to `app:`** is a change of the job file. The
  hooks kipp made with the token stay on the repos (kipp never deletes
  one) and are listed on the owner job's page as old webhooks, for you to
  remove. Until then a push arrives twice, by the old hook and by the
  App, and kipp builds the commit once: a build of that commit queued or
  running already answers the second delivery with `ignored`.
- **Making the App.** An admin opens **GitHub Apps** in the menu under
  their name (`/admin/apps`), gives a name and optionally an
  organization, and kipp sends GitHub the manifest of the App above.
  After GitHub's confirmation kipp keeps the key in `<data>/apps/<name>.pem`
  (mode 0600, the directory 0700; an existing key is never replaced) and
  the webhook secret in `GITHUB_APP_WEBHOOK_<NAME>`, shows neither, and
  shows the `app:` line to paste and the link to install the App. It
  needs `serve --base-url`, and the password again
  ([sudo mode](accounts.md#sudo-mode)). An App made by hand at GitHub
  works the same way: put the key where you like and the secret under
  that name.
- **The Apps page lists both.** One row per App name: those made here,
  and those the loaded owner jobs sign in as, each with where it was made
  (here or at GitHub) and the owner jobs that use it. A row says what
  stands in the way: a job file that gives another ID or key file than
  the App made here under that name, a key file kipp cannot sign with
  (missing, not a private key in PEM, or readable by group or others),
  and a webhook secret that is not set. A name an App made by hand
  already has cannot be made here: kipp never replaces a key.

## Two jobs, one repo

A repo that an owner job indexes and another job (a multibranch job, or
another owner job) indexes too is built by each of them. kipp says so in
the log, once for each such repo when the server starts, at a reload and
at a scan, and again if the repo comes back after leaving.
