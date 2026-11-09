# Builds

What a build is, what it keeps, and where to read it.

- [Statuses](#statuses)
- [Pages and ids](#pages-and-ids)
- [Logs](#logs)
- [The queue](#the-queue)
- [Building again](#building-again)
- [Keeping a build](#keeping-a-build)
- [Test results](#test-results)
- [Artifacts](#artifacts)
- [Cache](#cache)

Numbers a build measures are on a page of their own: [Metrics](metrics.md).

## Statuses

A build is `queued`, then `running`, and ends as `success`, `unstable`,
`failed`, `cancelled`, `timeout` or `lost`. A stage is `pending`, then
`running`, and ends as `success`, `unstable`, `failed`, `skipped` or
`cancelled`.

The log of a stage that was stopped ends with a line that says why:
`kipp: cancelled by <user>`, `kipp: the job's timeout of <d> passed`, or
`kipp: the server stopped while this ran`.

A server that is stopped ends its running builds as `lost` and kills
their stages. One that crashes kills nothing; the next `kipp serve` on
the data directory does, before it runs anything, for every stage whose
shell (or what it became by `exec`) is still running. On Linux it tells
them by `KIPP_MARK`, so a stage that runs its command under `env -i` is
left running. Lost builds are not run again; queued ones are. To restart
the server without losing builds, see
[Restarting without losing builds](operations.md#restarting-without-losing-builds).

A user with `cancel` on the job cancels a queued or running build on its
page, or with `POST /api/v1/builds/<id>/cancel`.

## Pages and ids

A build's page is under its job, by its number there:
`/jobs/<name>/<number>`. It shows the stages as a graph with their times,
the log of each, the test results, the artifacts and the metrics. The
API names a build by its id, unique on the server
(`/api/v1/builds/<id>`), or by its number in the job
(`/api/v1/jobs/<name>/builds/<number>`); `/builds/<id>` redirects to the
page.

A job's page lists its newest 50 builds, with a link to the older ones,
50 at a time. `GET /api/v1/jobs/<name>/builds` answers the same, paged
([Lists](api.md#lists)).

The job list shows, after each job's last build, a dot for each of its
five newest finished builds, newest first, each a link to its build:
round and green for `success`, round and amber for `unstable`, `timeout`
and `lost`, square and red for `failed`, round and grey for `cancelled`.
Queued and running builds have no dot; they are in the queue.

## Logs

Each stage has a log, and a build has three more of its own: `prepare`
(what went wrong before a stage ran), `clone` (the clone of a `source`
job or a multibranch child) and `hooks` (`on_success`, `on_failure`, and
commit statuses that could not be posted).

- A build's page shows the last 1 MiB of each log, 5000 lines at most,
  and follows it within those two bounds, dropping lines from the top (a
  single line longer than 1 MiB is shown alone). The whole log is the
  download beside it.
- kipp keeps when each line of a log began, beside the log, which stays
  as the build wrote it. The page shows the time of day before each line,
  hidden or shown again with the Timestamps box, which the browser
  remembers; a copy of the log leaves the times out.
- The page draws the colours and the bold, dim, italic and underline a
  build printed (SGR escape sequences, the 16 colours; a 256-colour or
  true-colour one is shown as the nearest of the 16). Every other control
  sequence is left out of what is shown; the download is the log as the
  build wrote it.
- Every secret the job lists is masked as `***`
  ([Masking](accounts.md#masking)).

Over the API:

| Request | Answer |
|---|---|
| `GET /api/v1/builds/<id>/stages/<stage>/log?offset=<n>` | One chunk of the log from byte `n`, with the log's size in `X-Kipp-Size`. This is how `kipp run --wait` reads it. |
| `GET /api/v1/builds/<id>/stages/<stage>/log?download=true` | The whole log. With `&timestamps=true` each line begins with its time (`2026-01-02T15:04:05.000Z`, RFC 3339 in UTC). |
| `GET /api/v1/builds/<id>/stages/<stage>/log/events` | A stream of server-sent events: each line as it is written, as `id:` (the offset after the line) and `data:`, closed by `event: end` once the build's logs are complete. With `?timestamps=true`, an `event: time` with the line's time comes before each line. `Last-Event-ID` or `?offset=` resumes it. |

A line that kipp kept no time for has none.

## The overview

The front page, `/`, says what needs a look; the whole job list, grouped as
before, is at `/jobs` (and `/jobs?group=<name>` for one group), linked from
the header. An old link to `/?group=<name>` still works: it leads on to
`/jobs?group=<name>`.

On a wide window (75rem and up) the page is a strip of three graphs, then
two columns, then the groups; on a narrower one everything is one column in
the order graphs, running and queued, failing, unstable, flaky, failed,
coming up, groups. A control beside the graphs sets how far back they look:
**24 hours** (the default, `/`) or **7 days** (`/?window=7d`); an address
with any other window shows the 24 hours.

The graphs are drawn by the server as SVG, so they need no script, and each
comes with its numbers as text for a screen reader:

- **Builds over time**: the builds that ended in each hour (each six hours
  for the week), stacked by how they ended: success, unstable, failed
  (`failed`, `timeout` and `lost`) and cancelled.
- **Slots and queue**: the most builds running at once in each column
  against the server's `--builds` limit (the dashed line), and under it how
  long the builds that started in the column waited in the queue, the median
  and the longest.
- **Pass rate by group**: for each group, the share of the builds that ended
  green in the window, how many builds that is of, and the change against
  the window before it of the same length ("92% (+4) of 130"). A group with
  no finished build in either window is left out.

The columns follow, the left one with:

- **Running and queued**: the queue, as below.
- **Coming up**: the next 8 builds a cron will start.

and the right one with:

- **Failing now**: every job whose newest finished build is red, with how
  long it has been red, the run of its recent builds as dots, and a `flaky`
  tag when it also flips as often as a flaky job does. The job that has
  been red for the shortest time is first.
- **Unstable now**: every job whose newest finished build is `unstable`, with
  how long the run of unstable builds it ends has lasted and its recent
  builds as dots. Unstable is still green everywhere else: in the groups,
  in Flaky and in the pass rate. A job that was unstable and then failed is
  in Failing now only.
- **Flaky**: a job that is green now but changed colour at least four times
  between consecutive builds among its last 20 finished ones (at least 5
  needed), with its share of red builds ("20% failed (4/20)") and its flips.
  `kipp serve --flaky-flips <n>` sets the four; 0 hides the section.
- **Failed in the last 24 hours** (or **7 days**, as the window says): up to
  10 red builds, newest first, with a count of the rest, and whether the job
  is green again.

**Groups** comes last across the full width: one line per group, linking to
its list, with how many jobs are green, red, never built, running and
disabled.

Green is a build that ended `success` or `unstable`; red is `failed`,
`timeout` or `lost`; `cancelled`, `skipped`, queued and running builds are
neither and are passed over (a cancelled build is only in the first graph).
Only jobs that build themselves are counted: a multibranch job and an owner
job (and its repos) stand for their children. Jobs removed from the job
files and disabled jobs are in no list, and a disabled job is counted in its
group's line. The graphs count the builds still kept (`keep` in
[Jobs](jobs.md#concurrency-locks-timeout-and-retention)), so a job that
builds more than 100 times a week needs a larger `keep.builds` for the
week's graphs to be whole. A user sees only the jobs they may view.
`GET /api/v1/overview?window=24h|7d` gives the same as JSON ([API](api.md)).

![The overview: failing, unstable, flaky and failed on the left, with the stages that failed named in each failing row, running and coming up on the right, then a strip of graphs (builds over time, slots and queue, pass rate by group) below; a group's row of the pass-rate graph says how many of its jobs are never built or disabled](screenshots/overview.png)

## The queue

What runs and what waits is at the top of the overview and the job list,
and on its own page, `/queue`, which the header links to from every page with a count
("2 running, 1 queued", or "idle").

- A running build shows the stages it runs, and for a stage that waits to
  be placed on a runner or an engine, what it waits for.
- A queued one says why it waits: behind a build of its job, for a lock
  and the build that holds it, for a free executor, or that its job is
  gone and it will not start. A build that holds a lock is named only to
  whoever may view its job; anyone else reads "another build".
- How many executors are busy is told to an admin alone: to anyone else
  a build waiting for a free executor is "waiting to start", since which
  it is would tell whether builds of other jobs hold every executor.

`GET /api/v1/queue` answers the same: `running`, each with
`waiting_stages`; `queued`, each with `waiting` (`job`, `lock`,
`executor`, `nothing` or `gone`, and `start` in place of `executor` and
`nothing` for whoever is not an admin), `lock` and `blocker`; and to an
admin `executors` and `busy`, which anyone else gets as `null`.

`GET /api/v1/queue/events` is a stream of server-sent events: one called
`changed` each time the queue its user may see changes, so a change to a
job the user may not view is never told. The event carries nothing; the
client reads the queue again. The pages follow it, so the queue, the
header's count and the job list change within a moment of a build being
queued, starting, moving to another stage or ending; while the stream is
down they ask every 30 seconds instead.

## Building again

A build that has ended can be built again: the button "Build again" on
its page, for a user with `build` on the job, or
`POST /api/v1/builds/<id>/again`.

- The new build has the same params (a param the job no longer has is
  dropped, one it gained takes its default).
- Where the original had a checkout, it has the same one: a webhook
  build's commit, branch or tag and default branch, so that
  `KIPP_COMMIT`, `KIPP_BRANCH`, `KIPP_TAG`, `KIPP_DEFAULT_BRANCH` and
  `when: {branches, tags, default_branch}` are as in the original; and a
  child's commit, pull request and base, the pull request's merge being
  redone against the base commit the original recorded and not the
  base's tip.
- Its trigger is `web` or `api` (what `when: {triggers}` reads), its
  `trigger_detail` is `again #<number>` of the original, and the job's
  activity says `#13 built again from #12 by alice`. A build made again
  from an `after` build has no upstream.
- It is refused as the Build button is: a job that is disabled, a server
  that is quiet, a trigger the job's concurrency drops; and by the API,
  409 while the build has not ended. A commit the git host no longer
  serves fails the new build in `prepare`, as any clone failure does.

## Keeping a build

A build that has ended can be kept out of retention: the button "Keep"
on its page, for a user with `build` on the job, or
`PUT /api/v1/builds/<id>/keep`. A kept build is never removed by the
job's `keep`, by the rule for removed jobs or by `--artifacts-max`, and
does not count toward `keep.builds`.

"Stop keeping" and `DELETE /api/v1/builds/<id>/keep` put it back under
the rules, which remove it as soon as one reaches it. The page says who
kept it and when, the job's list of builds tags it `kept`, the API's
build has `kept` (`{by, at}`, or null) and the job's activity says
`#12 kept by alice`. A build that has not ended is 409; keeping a kept
build changes nothing, and it stays kept by the first user.

## Test results

A stage's `junit:` is a glob of JUnit XML files in the stage's directory,
read once its command exits, wherever the stage ran. The cases are under
Results on the build's page, the failed first, and across the job's last
20 builds on the job's page. `tests_failed` decides whether a failed case
changes how the stage ends ([How a stage ends](jobs.md#how-a-stage-ends)).

`GET /api/v1/builds/<id>/results` answers the cases, paged: `stage`,
`suite`, `name`, `status` (`passed`, `failed`, `skipped`), `duration_ms`
and `message`.

## Artifacts

A stage keeps files with its build by naming them, as GitHub's
`upload-artifact` does:

```yaml
stages:
  unit:
    run: make test
    artifacts:
      paths: ["coverage/*", report.html, dist]  # globs inside the stage's directory
      max_size: 200MB                          # default 1GiB, at most 10GiB
```

- `paths` are `path.Match` globs inside the stage's directory (its
  `workdir`, else the build's): a `*` stops at a slash, and a match that
  is a directory keeps every file under it. A glob that climbs out with
  `..` or is absolute is refused by `kipp check`. A link is kept as the
  file it leads to inside the directory; a link that leads out of it ends
  the stage `failed` with
  `kipp: artifacts: <path> leads outside the stage's directory`, and a
  link to a directory is not followed. A glob that matches nothing keeps
  nothing and is no error.
- The files are kept once the stage's command has ended, however it
  ended: a failed build's report is the one looked for first. They are
  kept file by file in the data directory, under
  `artifacts/<job>/<number>/<stage>/<path>`, and go with their build
  under its job's `keep`.
- `max_size` is the most a stage's matches may come to (sizes like
  `500`, `64KiB`, `200MB`, `1.5GiB`; KB, MB, GB are powers of 1000, KiB,
  MiB, GiB of 1024). A stage whose matches come to more ends `failed`
  with `kipp: artifacts: <n> bytes over the stage's max_size of <m>` and
  keeps none of them.
- `serve --artifacts-max` (default `50GB`) bounds every build's artifacts
  together: when a build ends and they come to more, the artifacts of the
  oldest ended builds are removed until they fit, each removal a row of
  the audit (actor `kipp`, action `artifacts.remove`). A running build's
  are never removed, nor are a kept build's: kept artifacts can hold the
  total over the limit.
- A stage on a runner or in a container sends its files when its command
  ends, as a tar. The server takes only plain files inside the stage's
  directory, refuses a link, an absolute name or one with `..` in it, and
  stops reading at `max_size`. A refused upload ends the stage `failed`,
  saying why, and keeps none.

A stage is given what stages before it kept by naming them in
`artifacts_from`, as GitHub's `download-artifact` does:

```yaml
stages:
  build: {run: make dist, artifacts: {paths: [dist]}}
  deploy:
    needs: [build]
    artifacts_from: [build]
    run: rsync -a "$KIPP_WORKSPACE/.kipp/artifacts/build/dist/" /srv/site/
```

- Each stage named is put under `$KIPP_WORKSPACE/.kipp/artifacts/<stage>/`
  before the command starts, at the paths it kept them under, and the
  same path holds on the server, on a runner and in a container.
  Whatever was there before is replaced.
- `artifacts_from` names only stages the stage needs, directly or
  through others; `kipp check` refuses any other, and a name given
  twice. A stage named that kept nothing is an empty directory, so that
  the directory is there either way.

The build's page lists the files under Artifacts: stage, file, size, each
a download, and a zip of each stage's. The API, with `view` on the job:

| Request | Answer |
|---|---|
| `GET /api/v1/builds/<id>/artifacts` | Per stage: each file's `path`, `size`, `sha256` and `url`, and the stage's zip. |
| `GET /api/v1/builds/<id>/stages/<stage>/artifacts` | One stage's list, the same rows. |
| `GET /api/v1/builds/<id>/stages/<stage>/artifacts/<path>` | One file. |
| `GET /api/v1/builds/<id>/stages/<stage>/artifacts.zip` | A stage's files as one zip, made as it is sent. |
| `GET /api/v1/jobs/<job>/artifacts/latest?stage=<s>[&status=...][&path=<p>]` | A redirect (302) to the newest such build's stage, or with `path` to that file. |

`latest` takes one or more statuses, comma-separated or `status=` given
again, of an ended build (`success`, `unstable`, `failed`, `cancelled`,
`timeout`, `lost`; default `success`), and finds the newest build of the
job that ended so and whose stage kept something, or kept `path`; none is
404.

A download is always a download: `Content-Disposition: attachment`,
`Content-Type: application/octet-stream`, `X-Content-Type-Options:
nosniff` and `Content-Security-Policy: sandbox`, an HTML report included.
An HTML file opened as a page of the server would run with its origin,
with the session of whoever opened it; to view a report in the browser,
download it and open the file.

## The build's token

A build is given `KIPP_TOKEN`, a token of its own that calls the API back
within the job's `permissions:`, by default `view` on the job alone, ending
with the build. A build another build started is recorded as started by
`build <job>#<number>`. See
[The build's token](jobs.md#the-builds-token-permissions).

## Cache

A stage that names `cache_scopes` keeps files from one build to the next
(a package cache, a built dependency tree) under a key it chooses, in the
server's data directory, and gets them back with one command in its
`run`:

```yaml
stages:
  deps:
    cache_scopes: [site]
    run: |
      key="deps-$("$KIPP_BIN" cache hash go.sum)"
      "$KIPP_BIN" cache restore --key "$key" --restore-key deps- --path .cache/mod || true
      make deps
      "$KIPP_BIN" cache save --key "$key" --path .cache/mod
```

- `cache_scopes` is a list of names (lower-case letters, digits, `.`,
  `-` and `_`) that the stage's token reaches, and nothing else: a scope
  is how two jobs agree to share a cache (`acme` for a family of repos)
  and how all the others stay out of it. The stage is given
  `KIPP_CACHE_URL`, `KIPP_CACHE_TOKEN` and `KIPP_CACHE_SCOPE` (the first
  scope, which `--scope` overrides). The token is made for the build,
  lives in memory and ends with it; a server restart ends it too.
- `kipp cache restore --key K [--restore-key PREFIX]... --path P...`
  unpacks the entry of key `K`, and failing that the newest one whose key
  starts with a `--restore-key` prefix, tried in order. Exit 0 is an exact
  hit. Exit 1 is a miss, which includes a prefix hit (restored all the
  same, the log says which key), a server that cannot be reached and an
  entry that is damaged: a cache that does not answer slows a build, it
  does not fail it, so the stage chooses what a miss costs (`|| true`
  above). A command line that is wrong is exit 2.
- `kipp cache save --key K --path P...` packs the paths as `tar.zst` and
  sends them. A key is immutable: the first save wins, and a save of an
  existing key keeps the entry and says so, exit 0. Put what the entry
  depends on in the key (`kipp cache hash FILE...` prints a SHA-256 of
  the files named, in the order given). Exit 2 is a save that did not
  happen (no token, an entry over 10 GiB, the server refused).
- A key is 1 to 512 letters, digits and `.`, `_`, `+`, `=`, `-`.
- A path is relative to the stage's directory, absolute, or `~/` for the
  home of the user the stage runs as. An archive is unpacked only into
  the paths the command names: a member outside them, a `..`, a hard link
  or a device file is refused, and nothing is written below a symbolic
  link.

A stage can declare its caches instead, and its `run` then says nothing
of them: kipp restores each before the command and saves each after one
that ended `success` or `unstable`, as the commands above do (a miss is a
line of the log and no failure, and a failed stage saves nothing).

```yaml
stages:
  deps:
    cache:
      - key: "deps-${KIPP_BRANCH}"
        restore_keys: ["deps-"]
        paths: [.cache/mod]
        scope: site              # default: the first cache_scopes, else the job's name
    run: make deps
```

`${NAME}` in a key is read from the job's params and env and from
`KIPP_JOB`, `KIPP_BUILD`, `KIPP_TRIGGER`, `KIPP_BRANCH`,
`KIPP_DEFAULT_BRANCH`, `KIPP_TAG`, `KIPP_COMMIT`, `KIPP_PR`,
`KIPP_BASE_BRANCH`, `KIPP_BASE_COMMIT`, `KIPP_UPSTREAM_BUILD`,
`KIPP_UPSTREAM_STATUS` and `KIPP_STAGE`. A secret, or a name that is none
of these, is refused when the job file is read; a key that is no key once
expanded (a branch with a slash in it) skips that cache and says so in
the log. On a runner and in a container the runner restores and saves.

**Namespaces keep a pull request from poisoning the main branch's
cache.** A build reads its own namespace, then `default`, and writes its
own. A webhook build of the default branch writes `default`; a webhook
build of another branch writes that branch's; any other build (cron, by
hand, `after`, a pushed tag) writes its job's, since nothing says what
code it ran. A job that knows every build of it runs default-branch code
sets `cache_writes_default: true` and writes `default`.

- `serve --cache-max` (default `20GB`) bounds all entries together. Past
  it, and for an entry not restored in 7 days, the least recently
  restored go, each a row of the audit (actor `kipp`, action
  `cache.remove`).
- An admin sees and clears the caches on the Cache page (`/admin/cache`,
  in the menu under their name): the entries by scope with their size,
  when each was saved and last restored, and a button that removes one or
  a whole scope after a confirmation, in [sudo mode](accounts.md#sudo-mode).
  `GET /api/v1/admin/cache` lists them for an admin's token, and
  `kipp cache ls` and `kipp cache rm` do the same on the server's machine
  ([Commands](cli.md#kipp-cache)). Each removal is a row of the audit.
- A stage on a runner reaches the cache at `serve --runner-url`, which
  must be given explicitly: a default would be an address the runner may
  not reach, and the stage fails saying so. Inside a container,
  `kipp-runner cache` is the same command as `kipp cache`, and
  `$KIPP_BIN` names it.
