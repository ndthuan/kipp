# Job files

A job is one YAML file, `<name>.yml`, in the jobs directory the server
is started with (`kipp serve --jobs <dir>`). This page is the reference
for that file.

- [The file](#the-file)
- [An example](#an-example)
- [Params](#params)
- [Triggers](#triggers)
- [Concurrency, locks, timeout and retention](#concurrency-locks-timeout-and-retention)
- [Stages](#stages)
- [Conditions: when](#conditions-when)
- [How a stage ends](#how-a-stage-ends)
- [Hooks](#hooks)
- [Stages from a repo: source](#stages-from-a-repo-source)
- [Variables](#variables)
- [The variables kipp sets](#the-variables-kipp-sets)
- [Secrets in a stage](#secrets-in-a-stage)
- [Reloading](#reloading)

Two more kinds of job have pages of their own:
[multibranch jobs](multibranch.md), which build every branch and pull
request of a repo, and [organization folders](organization-folders.md),
which build every repo of a GitHub user or organization.

## The file

- The file is called `<name>.yml`, where `<name>` is the job's `name`:
  lower-case letters, digits, `.`, `-` and `_`. A file that ends in
  `.yaml` is an error, so that it is not silently ignored.
- `name` is required, and exactly one of `stages`, `source`
  ([below](#stages-from-a-repo-source)), `multibranch` and `owner`.
- An unknown key is an error. So is a file with more than one YAML
  document in it.
- `_vars.yml` at the top of the directory is no job: it holds variables
  for many jobs ([Variables](#variables)). Subdirectories and files that
  do not end in `.yml` are ignored.
- One bad file refuses the whole directory: `kipp check <dir>` lists
  every error without starting anything, and a running server that is
  asked to reload keeps the jobs it had.

## An example

```yaml
name: site-smoke              # lower-case letters, digits, . - _
group: site                   # the job list groups jobs by it
description: Smoke tests of the site.
disabled: false               # true: no trigger starts it and nobody can build it
params:                       # each is an environment variable of every stage
  TIER: {type: choice, options: [quick, full], default: quick}
  DRY_RUN: {type: bool, default: true}
  TARGET: {default: staging}  # type string: one line
  SECONDS:
    description: "Seconds to run for, 30 to 300. Empty is 120."
  NOTES: {type: text}         # any number of lines
timezone: Europe/Paris        # the zone the cron is read in; default the server's
triggers:
  cron: "H 3 * * *"
  webhook: {repo: acme/site, branches: [main, "release-*"]}
  after: [site-build]         # runs when that job succeeds or ends unstable
concurrency: queue            # queue (default), drop, parallel or supersede
locks: [checkout]             # no two builds holding the same lock run together
timeout: 2h
keep: {builds: 50, days: 30}  # either or both; default is 100 builds
env: {FOO: bar}               # plain values, not secrets
secrets: [CHAT_TOKEN]         # set with kipp secret set, masked in the logs
workdir: /srv/site            # default: the build's own workspace
metrics:                      # see metrics.md
  store: site
stages:
  lint: {run: make lint}
  unit:
    env: {GOFLAGS: -count=1}  # this stage's alone
    run: make test
    junit: "reports/*.xml"
    tests_failed: unstable
    artifacts: {paths: ["coverage/*", report.html], max_size: 200MB}
  smoke: {needs: [lint, unit], run: make smoke TIER=$TIER, retries: 2}
  linux: {runs_on: linux, image: "golang:1.27", workdir: src, run: go test ./...}
  deploy:
    needs: [smoke]
    artifacts_from: [unit]    # unit's files, under $KIPP_WORKSPACE/.kipp/artifacts/unit/
    when: {default_branch: true, params: {DRY_RUN: [false]}}
    run: ./deploy.sh
  cleanup: {needs: [deploy], when: {needs_status: always}, run: ./cleanup.sh}
on_failure: {run: ./notify.sh}
on_success: {run: ./notify.sh}
```

[`examples/jobs/`](../examples/jobs) has more, each read by `kipp check`.

![A job's page: its group and name, its description, the metrics store it reports into, a Build button, and its builds with status, trigger, start time and duration](screenshots/job.png)

The top-level keys:

| Key | What it is |
|---|---|
| `name` | The job's name, the file's name without `.yml`. Required. |
| `group` | A name the job list groups jobs by, and a secret's or a variable's scope can name. |
| `description` | A line shown on the job's page. |
| `disabled` | `true` keeps the job, its history and its file, and takes it out of use. |
| `params` | Build parameters ([Params](#params)). |
| `timezone` | The IANA zone the cron is read in. |
| `triggers` | `cron`, `webhook` and `after` ([Triggers](#triggers)). |
| `concurrency` | `queue` (default), `drop`, `parallel` or `supersede`. |
| `locks` | Names of locks a build holds while it runs. |
| `timeout` | How long a build may run, a duration like `30m` or `2h`. None is no limit. |
| `keep` | `{builds, days}`: how many builds, and for how long, are kept. |
| `env` | Plain variables for every stage ([Variables](#variables)). |
| `secrets` | Names of secrets the stages are given ([Secrets in a stage](#secrets-in-a-stage)). |
| `workdir` | Where the stages run: a directory of the machine, or with `source` a path inside the clone. Default the build's workspace. |
| `stages` | The stages ([Stages](#stages)). |
| `source` | Read the stages from a file in a repo instead ([source](#stages-from-a-repo-source)). |
| `multibranch` | Build a repo's branches and pull requests ([Multibranch jobs](multibranch.md)). |
| `owner` | Build every repo of a GitHub owner ([Organization folders](organization-folders.md)). |
| `on_success`, `on_failure` | Hooks run once the build has ended ([Hooks](#hooks)). |
| `metrics` | The store the builds' numbers go to, and what it remembers ([Metrics](metrics.md)). |
| `cache_writes_default` | `true` lets every build write the cache's `default` namespace ([Cache](builds.md#cache)). |

## Params

Each param is an environment variable of every stage, with the value the
build was started with, or its default.

| `type` | Value |
|---|---|
| `string` (the default) | One line. |
| `text` | Any number of lines. |
| `bool` | `true` or `false`; the default must be one of them. |
| `choice` | One of `options`; a `default` must be one of them. |

`default` is the value when none is given, and `description` is what the
build form says under the name: what to type, and what an empty field
means. A param's name is one a shell takes; it may not be `csrf` (the
build form's own field), start with `KIPP_`, be `CI`, or be the name of a
secret the job lists.

Params are fixed when a build is queued: a reload that changes a job's
params does not change the builds already waiting. Values are given with
`kipp run <job> -p KEY=VALUE` (or `--param-file KEY=FILE` for a value
that is a whole file), in the body of `POST /api/v1/jobs/<name>/builds`,
or on the job's Build form.

## Triggers

### cron

Five fields, as cron has them, with Jenkins' `H`: a value hashed from the
job's name (`H`, `H/15`, `H(0-29)`), so that jobs with the same line do
not all start in the same minute. As in Jenkins, and unlike cron, when
both day fields are restricted both must match.

A cron is read in a time zone: the job's `timezone`, else the server's
`--timezone`, else the host's (`TZ`, or the zone `/etc/localtime` links
to). A zone is an IANA name (`Europe/Paris`, `America/New_York`, `UTC`);
`Local`, an offset like `+07:00`, and a `timezone` on a job without a
cron are refused. A time the clocks skip (02:30 on the night they go
forward) fires once, at the first minute after the skip; a time they
repeat fires the first time only. The job's page shows the next build in
that zone, and `GET /api/v1/jobs/<name>` says the zone in `timezone`.

### webhook

```yaml
triggers:
  webhook: {repo: acme/site, branches: [main, "release-*"]}
  # or a pushed tag, for a release:
  # webhook: {repo: acme/site, tags: ["v*"]}
```

A push to GitHub starts a build. `repo` is `owner/name`. The webhook to
add on GitHub is:

- Payload URL: `<base-url>/api/v1/hooks/github/<job>`
- Content type: `application/json`
- Secret: the value of the kipp secret `GITHUB_WEBHOOK_<JOB>`, the job's
  name in upper case with `-` and `.` as `_` (`site-smoke` reads
  `GITHUB_WEBHOOK_SITE_SMOKE`). Set it with
  `kipp secret set GITHUB_WEBHOOK_SITE_SMOKE --data <dir>` or on the
  Secrets page.
- Events: the push event.

The Server page lists each webhook's payload URL and whether its secret
is set.

`branches` lists the branches a push builds: each an exact name or a
pattern as Go's `path.Match` reads it, where `*` matches within one part
of the name and stops at a slash: `branches: [main, "APP-*",
"epic/APP-*"]` builds `APP-12-fix` and `epic/APP-3-big` but not
`feature/APP-1-x`. A pattern `path.Match` cannot read is an error. With
no `branches` every branch builds, unless the job has `tags`. A push that
deletes a branch builds nothing.

`tags` lists the tags whose push builds, read as `branches` is
(`tags: ["v*"]` builds `v1.2.0`; `release/*` does not build
`release/a/b`). Publishing a GitHub release pushes its tag, so this is
how a release deploys: `branches: [main]` in one job for staging and
`tags: ["v*"]` in another for production, or both keys in one job and
`when: {branches: [main]}` and `when: {tags: ["v*"]}` on its stages. A job
with `tags` and no `branches` builds no branch; a branch push never
matches `tags` and a tag push never matches `branches`. A push that
deletes a tag builds nothing, nor does a ref that is neither a branch nor
a tag.

The build's `KIPP_COMMIT` is the commit the push names. For an annotated
tag GitHub's `after` is the tag object, so kipp takes the push's
`head_commit.id`, which GitHub sets to the tagged commit, and `after`
only when the push has none (a stage that needs the commit then reads
`git rev-parse "$KIPP_COMMIT^{commit}"`).

Each delivery is built once: its `X-GitHub-Delivery` id and a SHA-256 of
the job's name and the body are kept for 30 days, and either one again is
answered 409, GitHub's Redeliver button included. To build that commit
again, use Build again on the build's page ([Builds](builds.md#building-again)).
A delivery kipp answered 503 (busy or quiet), 408 or 401 was not kept and
may be sent again. What the endpoint does with an unsigned or oversized
request is in [Security](accounts.md#security).

A webhook build does not clone anything by itself: a `stages` job's
stages check the commit out themselves (from `KIPP_REPO` and
`KIPP_COMMIT`), and a `source` job clones it ([source](#stages-from-a-repo-source)).

### after

`after: [site-build]` starts a build when a build of `site-build` ends
`success` or `unstable`. Every job named must exist, a job cannot run
after itself, and a loop among jobs is refused. The build gets
`KIPP_UPSTREAM_BUILD` (`job#number`) and `KIPP_UPSTREAM_STATUS`, so that
it can refuse an unstable upstream.

### By hand

A user with `build` on the job starts one from the job's page (with a
form for its params), with `kipp run <job>`, or with
`POST /api/v1/jobs/<name>/builds`. Its trigger is `web` or `api`.

## Concurrency, locks, timeout and retention

- `concurrency: queue` (the default) runs a job's builds one after
  another, and drops a cron trigger while one is already waiting. `drop`
  drops every trigger while the job has a build queued or running.
  `parallel` runs them side by side. `supersede` queues like `queue`, and
  each new build cancels the queued builds of the same job and the same
  ref (never a running one): the same branch for a branch push, the same
  tag for a tag push, and for a build with no ref (cron, by hand, API) the
  other no-ref builds. A push to `main` never cancels a queued tag build,
  nor one branch's build another's. The job's activity says which build
  took their place: for a job whose newest trigger of a ref makes the older
  ones pointless, such as a tag deploy that waits on a lock.
- `locks` are names shared across jobs: no two builds holding the same
  lock run at once, whatever their jobs. A build waits for its locks
  before it starts, and holds them until its hooks have ended.
- `timeout` ends a build that runs longer as `timeout`. Without it a
  build may run for ever.
- `keep: {builds, days}` removes a job's oldest ended builds, with their
  logs, artifacts, test results and metrics history, beyond the newest
  `builds` (default 100) or older than `days`. A build kept on its page
  ([Keeping a build](builds.md#keeping-a-build)) is never removed and
  does not count toward `builds`. The sweep runs daily at 03:30 in the
  server's zone, and after each build.
- `disabled: true` ignores cron, webhook and `after`, refuses a build
  asked for by hand, and fails a build that was queued when a reload
  disabled the job.
- A job whose file is removed keeps its builds and logs, to read: its
  `keep` goes with its file, and from then on each build of it is kept
  for 90 days from when it was queued, and no more than the newest 100.

## Stages

`stages` is a map of stage name to stage, in the order written. A stage
name is lower-case letters, digits, `.`, `-` and `_`, and may not be
`prepare`, `clone` or `hooks`, which are the build's own logs.

| Key | What it is |
|---|---|
| `run` | The shell command, run as `<shell> -c <run>` (`serve --shell`, default `sh`). Required. |
| `needs` | Stages that must end first. |
| `when` | The condition the stage runs under ([Conditions](#conditions-when)). |
| `env` | Variables of this stage alone. |
| `workdir` | A path inside the build's directory, made if it is not there. |
| `junit` | A glob of JUnit XML files, read once the command exits ([Test results](builds.md#test-results)). |
| `tests_failed` | `unstable` or `failed`: how a stage ends when its JUnit files hold a failed case. |
| `unstable_exit` | Exit codes that end the stage `unstable` instead of `failed`. |
| `retries` | How many more times to run a failing command, 0 to 10. |
| `artifacts` | Files kept with the build ([Artifacts](builds.md#artifacts)). |
| `artifacts_from` | Stages whose artifacts are put in this stage's workspace. |
| `cache_scopes`, `cache` | The keyed caches the stage reaches ([Cache](builds.md#cache)). |
| `metrics` | Numbers the stage reports, and guards on them ([Metrics](metrics.md)). |
| `runs_on` | Labels of the agent or engine the stage runs on ([Agents and engines](agents.md)). |
| `image` | With `runs_on`, the image of the container the stage runs in. |

- Stages form a DAG through `needs`. Those that are ready run at once, up
  to the server's `--executors` (and an agent's executors). A stage is
  kipp's step: there is no level of steps inside one.
- All stages of a build share one workspace, `$KIPP_WORKSPACE`, removed
  when the build ends; a stage on an agent has a workspace of its own
  there.
- A stage's `workdir` is a path inside the directory its build runs in
  (the job's `workdir`, or the workspace; on an agent, the agent's
  workspace). One that leads out of it is refused.
- A stage is over when its command exits. What it left running in the
  background is killed 2 seconds later; a daemon a stage means to leave
  running must detach itself (`setsid`).
- `runs_on` sends a stage to an agent: a label, or a list of them
  (`[linux, arm64]`), each lower-case letters, digits, `.`, `-` and `_`.
  With `image` it runs in a container of that image on a Docker engine
  that carries the labels; without, on a standing agent that carries
  them. An `image` without `runs_on` is refused.

## Conditions: when

`when` is the condition a stage runs under, like `if:` in a GitHub
workflow. Every key given must hold:

```yaml
when:
  default_branch: true        # true: the repo's default branch; false: any other
  branches: ["release-*"]     # the pushed branch, names or path.Match patterns
  tags: ["v*"]                # the pushed tag (KIPP_TAG), names or patterns
  target_branches: [main]     # a pull request's base branch (KIPP_BASE_BRANCH)
  triggers: [webhook, cron]   # KIPP_TRIGGER is one of these
  params: {TIER: [full]}      # each named param's value is one of those listed
  needs_status: failure       # success (default), failure or always
```

A stage whose condition does not hold is skipped before anything runs,
and so is every stage that needs it under `success`. A skipped stage
fails nothing, so one job can check every branch and deploy only the
default one ([`examples/jobs/app-flow.yml`](../examples/jobs/app-flow.yml)).

- `default_branch` reads the default branch the push names
  (`repository.default_branch`, kept with the build as
  `KIPP_DEFAULT_BRANCH`). A build not started by a push (cron, api, web,
  after) counts as on the default branch and has no pushed branch, so
  `branches` never holds for it. A build of a pushed tag has no branch:
  `branches` never holds for it, `tags` holds for nothing else, and it is
  not on the default branch.
- `target_branches` holds only for a pull request's build
  ([Multibranch jobs](multibranch.md)).
- `triggers` names `cron`, `api`, `web`, `webhook`, `after` or `scan`.
- `params` names params of the job, each with the values it runs for; a
  bool param is `true` or `false`, and a param's default counts as its
  value. A name that is not a param of the job, a value a choice param
  does not offer and an empty list are refused.
- `needs_status` says how the stages a stage needs must have ended for it
  to run, like `if: failure()` and `if: always()` in a GitHub workflow:
  `success` (the default) once every need succeeded or ended unstable;
  `failure` once every need has ended and at least one failed, or was
  skipped because a stage before it failed; `always` once every need has
  ended, however. A stage skipped because its own `when` did not hold
  neither failed nor succeeded: `always` runs after it, `failure` does
  not. A cancel or the job's timeout still starts nothing new, `always`
  included. A stage that runs after a failure ends as it ran, and a green
  one does not make a failed build green
  ([`examples/jobs/app-e2e.yml`](../examples/jobs/app-e2e.yml) tears down
  a test environment however its tests ended). `needs_status` on a stage
  with no `needs` is refused.

Refused as well: `when` with no key, a pattern `path.Match` cannot read,
a trigger kipp does not have, `branches` or `tags` with `triggers` that
leave out `webhook`, and `branches` with `tags` (a push is to a branch or
of a tag, never both). There is no condition on the paths a push changed:
GitHub's push event lists the files of 20 commits at most.

## How a stage ends

A stage ends `success` when its command exits 0 and `failed` otherwise,
unless one of these says more:

- `unstable_exit: [4]` lists exit codes (1 to 255) that end the stage
  `unstable` instead: it ran to its end and has something to report that
  is no reason to stop.
- `tests_failed: unstable` or `failed` (needs `junit`) ends a stage that
  would end `success` or `unstable` as it says when its JUnit files hold
  at least one failed or errored case, wherever the stage ran; the
  stage's log says so in one line. A stage that failed, was cancelled or
  timed out is unchanged.
- A metrics `guard` can end it `failed` or `unstable` ([Guards](metrics.md#guards)).
- `retries: 2` runs the command again, in the same workspace and
  directory with the same environment, up to that many more times while
  it exits with a code that would end the stage `failed`. Each later try
  starts with the log line `kipp: try 2 of 3`; the stage's exit code and
  status are the last try's. A cancel, the job's timeout and the server
  stopping start no other try, and a `tests_failed` or guard verdict is
  not retried. The build page says `success, 2 tries`, and the API's
  stage has `tries`. The clone and the hooks are not retried.

What needs an unstable stage runs as after a success. A build with an
unstable stage and no failed one ends `unstable`: `on_success` runs (with
`KIPP_STATUS=unstable`), `on_failure` does not, and the jobs that name it
in `triggers.after` are built. A failed stage, a cancel, the timeout and
a server that stops each win over it.

## Hooks

`on_success` and `on_failure` each take a `run`. The hook runs once the
build's status is final, in the build's directory, for 5 minutes at most,
with every variable a stage has and `KIPP_STATUS`. It takes none of the
executors, so a slow one keeps no other build waiting; the job's `locks`
and its place under `concurrency` are held until it ends. Its output is
the build's `hooks` log.

[`examples/hooks/`](../examples/hooks) has hooks that tell Slack, Telegram
or ntfy, each reading its token from a secret.

## Stages from a repo: source

```yaml
name: site
source: {repo: acme/site, ref: main, path: ci/stages.yml}
secrets: [DEPLOY_KEY]
```

instead of `stages` clones the repo into the workspace and reads the
stages from that file, a file with `stages:` and an optional `env:` and
nothing else. `ref` defaults to `main`. A webhook build checks out the
pushed commit, and every build records the commit it cloned
(`built_commit` in `GET /api/v1/builds/<id>`, and `KIPP_COMMIT`). The
clone's output is the build's `clone` log.

- With `source`, `workdir` is a path inside the clone
  (`workdir: services/api`); one that leads out of it is refused.
- The stages in the file are checked against the job's params when the
  build reads them, and a build whose file names a param the job lacks
  fails before any stage runs.
- git is never asked for a password: a private repo needs an ssh URL, or
  a credential helper in the server's environment. The clone has the
  server's whole environment, whatever `--pass-env` says. A user and
  password in the URL (`https://user:token@host/...`) are left out of the
  URL wherever kipp shows it, and masked in what git prints.
- Whoever can push to the repo runs commands on the server's machine,
  with the job's secrets.

## Variables

A name is given to a stage in these places, and the later of these wins,
so that one name set in several is the most specific's. From the lowest:

1. `serve --pass-env`, the server's own environment (by default `PATH`,
   `HOME`, `USER`, `LOGNAME`, `SHELL`, `LANG`, `LC_*`, `TZ` and `TMPDIR`);
   on an agent, the agent's own environment and `serve --agent-env`;
2. `_vars.yml`, `global`;
3. `_vars.yml`, `groups.<group>` (the job's `group`);
4. `_vars.yml`, `jobs.<pattern>`;
5. the job file's top-level `env:`;
6. the top-level `env:` of a stages file (a `source` job's file, or a
   multibranch job's `.kipp.yml`);
7. what the stages it needs wrote to their `KIPP_ENV` files (below);
8. the stage's own `env:`, in the job file or the stages file;
9. the job's params.

Secrets and the variables kipp sets follow, and nothing above overrides
them: a name starting with `KIPP_`, the name `CI` and the name of a
secret the job lists are refused wherever they are declared. All of it is
plain text, shown on the job's page to whoever may view it and never
masked: a value a stage must not show goes into a secret. Values are
literal; `${X}` is not expanded inside one.

### _vars.yml

A file at the top of the jobs directory, beside the job files, read with
them by `kipp check` and by a reload (a bad one fails the reload):

```yaml
global:
  REGISTRY: registry.example.com
groups:
  site:
    DEPLOY_HOST: staging.example.com
jobs:
  "site-*":                      # path.Match, against the job's name
    SMOKE_URL: https://staging.example.com
```

The keys are `global`, `groups` and `jobs`; an unknown key is an error. A
name is one a shell takes and a value is a string of one line or several.
A `jobs` pattern matches the job's name, then the name of the job file it
comes from (a multibranch child its parent's, a repo of an owner job its
owner job's). A job that two patterns both give one name is an error
naming both: for a job of a file, the reload and `kipp check` refuse it;
for a child or a repo, which exist only after a scan, its builds fail
before any stage with the two patterns in the `prepare` log. The same
holds for a variable of `jobs` that names a secret the job lists; one of
`global` or `groups` that does fails only that job's builds. A job's page
lists the variables the files give it, each with the entry it came from.

### Handing variables on: KIPP_ENV

A stage hands variables to the stages after it through `KIPP_ENV`, as
GitHub's `GITHUB_ENV` does. Every stage, on the server or on an agent,
gets `KIPP_ENV`, the path of an empty file of its own. It writes lines
`NAME=value`, or `NAME<<DELIM`, the lines of the value and a line that is
`DELIM` for a value of several lines; blank lines are ignored, and a name
written twice has its last value.

```yaml
stages:
  version:
    run: echo "TAG=$(git describe --tags)" >> "$KIPP_ENV"
  notes:
    needs: [version]
    run: |
      {
        echo "NOTES<<EOF"
        git log --oneline "$(git describe --tags --abbrev=0)..HEAD"
        echo "EOF"
      } >> "$KIPP_ENV"
  publish:
    needs: [notes]
    run: ./publish.sh "$TAG" "$NOTES"
```

- The file is read once the stage has ended `success` or `unstable`. What
  a stage that failed, was cancelled or timed out wrote is ignored; with
  `retries`, the file is emptied before every try.
- A stage's variables reach the stages that need it, directly or through
  others, and not the stages beside it; when two of them wrote one name,
  the one that ended later gives it. A hook is given those of every stage
  that ended.
- A name refused in `env:` (`KIPP_*`, `CI`, a secret the job lists), a
  param's name, and a line that is neither form are skipped, each with
  one line `kipp: KIPP_ENV <stage>: ...` in the log of the stage that
  wrote it; the stage's outcome does not change. A file over 64 KiB is
  refused whole.
- Values are not masked, except where one equals a secret.

## The variables kipp sets

The variables under Build, Where it runs (but `KIPP_AGENT` and
`KIPP_BIN`), Git and History are set in every stage and in a hook, empty
when they have no value, so that a stage under `set -u` reads them; the
others are there where their line says. On an agent the values are the
same, but for the ones under Where it runs, which are the agent's.

- **Build**: `CI` (`true`), `KIPP_JOB`, `KIPP_BUILD` (the job's build
  number), `KIPP_BUILD_ID` (unique on the server), `KIPP_TRIGGER`
  (`cron`, `api` for the API and `kipp run`, `web` for the job's page,
  `webhook`, `after` or `scan`), `KIPP_STARTED_BY` (the user who started
  an `api` or `web` build; empty for the other triggers), `KIPP_URL` (the
  server's `--base-url`, or the address it listens on), `KIPP_BUILD_URL`
  and `KIPP_JOB_URL` (the build's page and the job's; empty without
  `--base-url`).
- **Where it runs**: `KIPP_WORKSPACE`, `KIPP_OS` and `KIPP_ARCH` (`linux`,
  `darwin`, `amd64`, `arm64`), `KIPP_AGENT` (on an agent only: its name,
  `<engine>/<n>` for a container), `KIPP_BIN` (the path of kipp's own
  binary where the stage runs: the server's, the `kipp` a standing agent
  runs, or in a container the `kipp-agent` kipp put there), and
  `KIPP_TMP`: a directory of the stage's own, empty when it starts,
  outside every workspace, shared by its tries and removed when the stage
  ends however it ends. A hook has its own.
- **Git**: `KIPP_REPO` (`owner/name`: a multibranch job's repo, else the
  repo of the job's `triggers.webhook`, for every build of that job),
  `KIPP_BRANCH` (the branch a webhook build's commit was pushed to, or a
  child's branch), `KIPP_DEFAULT_BRANCH` (the repo's default branch the
  push named), `KIPP_TAG` (the tag a webhook build was pushed for,
  `v1.2.0`; then `KIPP_BRANCH` is empty), `KIPP_REF` (`refs/heads/<branch>`,
  `refs/tags/<tag>`, or a pull request's `refs/pull/<n>/head`; empty when
  the build names none, and never read from a `source` job's `ref:`), and
  `KIPP_COMMIT`: the commit a push named (for a tag, the commit it names),
  else the commit the build checked out, which a `source` job and a
  multibranch child record in `built_commit`; empty for a build that
  cloned nothing and named no commit.
- **History**: `KIPP_PREVIOUS_COMMIT`, the commit of the job's latest
  earlier build that has one, and `KIPP_PREVIOUS_SUCCESSFUL_COMMIT`, the
  same among those that ended `success`; looked up when the build
  prepares, among the builds still kept. `KIPP_ATTEMPT` is `1` for the
  first run of a stage's command and `2`, `3`... as `retries` runs it
  again.
- **A pull request's build**: `KIPP_PR`, `KIPP_BASE_BRANCH`,
  `KIPP_BASE_COMMIT`, `KIPP_PR_TITLE`, `KIPP_PR_URL` and `KIPP_PR_AUTHOR`
  ([Multibranch jobs](multibranch.md#what-a-childs-stages-get)).
- **Upstream**: `KIPP_UPSTREAM_BUILD` (`job#number`) and
  `KIPP_UPSTREAM_STATUS` (`success` or `unstable`) for a build started by
  `after`, recorded when the build is queued.
- **Of a stage**: `KIPP_STAGE`, `KIPP_ENV`, `KIPP_METRICS` for a stage
  that declares `metrics` ([Metrics](metrics.md)), `KIPP_CACHE_URL`,
  `KIPP_CACHE_TOKEN` and `KIPP_CACHE_SCOPE` for a stage with caches
  ([Cache](builds.md#cache)), and in a hook `KIPP_STATUS`. `KIPP_MARK` is
  kipp's own: a random value for each stage, not a secret, by which a
  server started after a crash knows the stage's processes.

## Secrets in a stage

`secrets: [NAME, ...]` gives the job's stages and hooks those secrets.
A secret is set by an admin ([Secrets](accounts.md#secrets)) and has a
kind, which decides what a stage gets:

- `text` (the default): one line of 8 bytes to 64 KiB. The stage gets
  `NAME` with the value.
- `file`: any bytes, 1 byte to 1 MiB, such as a PEM key, a keystore or a
  service account's JSON. The stage gets `NAME`, the absolute path of a
  copy of the file that only its user reads, in a directory made for that
  stage (and for a hook, for that hook) outside the workspace. The copy is
  removed when the stage ends however it ends, and a server or an agent
  that starts again removes what a stage left when it died.
- `userpass`: a user and a password. The stage gets `NAME_USR`,
  `NAME_PSW` and `NAME` as `user:password`, which is what a basic-auth
  URL takes. The password and `NAME` are masked; the user is not. A job
  that lists `LOGIN` and also gives `LOGIN_USR` or `LOGIN_PSW` as an
  `env:` key, a param or a secret is refused when its build starts.

A secret's scopes decide which jobs may list it. A job that lists a
secret whose scopes do not reach it fails before its first stage, with
`kipp: secret NAME is not in scope for job JOB` in its `prepare` log.

Every value is masked as `***` in every log where it appears as it is,
byte for byte. A stage that prints it encoded, changed or a piece at a
time prints what it printed, so a stage that handles a secret should not
print it at all ([Masking](accounts.md#masking)).

## Reloading

`kipp reload`, `SIGHUP` to the server, or the button on the Server page
re-reads the directory. A directory with an error in it changes nothing:
the server keeps the jobs it had, and the error is on the Server page and
in the log ([Reload lines](operations.md#reload-lines)). A build already
queued keeps its params.
