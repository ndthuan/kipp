# Changelog

What changes in kipp from one release to the next, newest first. The shape
is that of Keep a Changelog (https://keepachangelog.com/en/1.1.0/), and
versions follow Semantic Versioning (https://semver.org/): the job file,
the HTTP API under `/api/v1`, the command line, the server's reload log
lines and the data directory are the public surface.

## Unreleased

The database schema starts again at 1: one migration makes the whole
schema. A data directory made by an earlier build is at a higher schema,
which this build refuses as a newer one's.

Added:

- The MCP tool `get_build` lists what each metrics guard of the build found
  (`guards`), so a model can tell why a build ended unstable, and the new
  tool `wait_build` waits up to 60 seconds for a build to end and answers as
  `get_build` does.
- `kipp serve --tls-listen <addr>` adds a listener that serves the same
  pages and API over HTTPS, with `--tls-cert` and `--tls-key`, or else a
  self-signed certificate for `localhost`, `127.0.0.1`, `::1`, its host and
  `--base-url`'s, kept in `<data>/tls/cert.pem` and made anew a month
  before it ends. `--listen` stays plain HTTP.
- Four MCP tools that read: `read_artifact` reads a text file a stage kept,
  by the byte; `search_log` finds the lines of a stage's log that match a
  regular expression, each with the offset `read_log` reads on from;
  `compare_builds` says what changed since an earlier build (parameters,
  commit, stages, tests newly failing and fixed); `test_history` lists a
  job's tests over its last builds, the ones that flip first.

- `side_builds: {params: {<param>: [<values>]}}` marks the builds of a job
  whose params hold for it as side builds, e.g. a nightly build of the
  default branch with a param that only runs extra stages: such a build
  posts no commit status, so it never paints the commit in place of its
  ordinary build, and starts no job that runs `after:` this one. A
  multibranch job's children and an owner job's repos have their job's
  rule; a rule that holds for a build given no params is refused.
- A standing runner has a weight, an integer that is 0 until set: of the
  runners that match a stage and have a free executor, the highest weight
  takes it, and equal weights keep the runner idle longest first. A stage
  still never waits for a heavier runner that is busy or offline. Set it
  with `kipp runner add --weight N`, `kipp runner set <name> --weight N` or
  the Runners page's Change form; `kipp runner ls`, the Runners page, the
  queue's heading and `GET /api/v1/admin/runners` (`weight`) and
  `GET /api/v1/queue` (`runners[].weight`) show it.
- A job whose newest 5 finished builds are all green is no longer listed as
  flaky on the overview, however often its colour changed before: a fixed
  job leaves the Flaky section as soon as it has been green five times in a
  row. `kipp serve --flaky-quiet <n>` sets the five, 0 turns the rule off.
- A stage takes `locks:` too, held only while it runs: a deploy can lock
  `production` around its `deploy` stage and leave the lock free while its
  build and tests run. Stage locks and job locks are one set of names and
  exclude each other. A stage takes all its locks at once before it is
  given an executor or a place, holds nothing while it waits, gives them
  back as it ends however it ends, and takes a freed lock before a queued
  build. A stage whose wait for another build's job lock would never end,
  since that build waits for one of its own, fails at once. A stages file
  in a repo (`source`, `.kipp.yml`) may take only the locks its job grants
  in the new `stage_locks` key, which an owner job's `owner.repos` can set
  per repo; a build whose file names another fails before any stage runs.
  The queue and the build's page say which lock a stage waits for and who
  holds it; `GET /api/v1/queue` gives a running build's
  `waiting_stage_locks`, and a job's detail its `stage_locks` and
  `locked_stages`. See docs/jobs.md.

Changed:

- Every login gets the `Secure` cookies `__Host-kipp_session` and
  `__Host-kipp_device`. The cookies `kipp_local_session` and
  `kipp_local_device` that a browser on the server's own machine got over
  plain HTTP are gone and no longer read: Chrome, Firefox and Edge keep the
  `Secure` cookie on `http://127.0.0.1` and `http://localhost`; Safari
  there needs `--tls-listen`. The overview's remembered tab is `Secure` too.
- A refresh token of `serve --mcp-oauth` used again within 30 seconds of
  its first use gets new tokens instead of ending the grant, so a client
  whose answer was lost, or that refreshes twice at once, stays signed in.
  A use after that still ends the grant.
- An MCP call that needs a scope the sign-in lacks is answered `403` with
  a challenge that names every scope the sign-in should hold, the ones it
  has among them (`scope="kipp:read kipp:build"`), so a client that asks
  for just those keeps what it had. An admin's call of a job tool without
  `kipp:jobs` is now such a `403` too, not an unknown tool. A tool the
  sign-in's scopes refuse says which scope it lacks, not that the user
  lacks a grant.
- `GET /oauth/authorize` answers a request with a fault (no PKCE, another
  resource, an unknown scope, a wrong response_type) with a page of its
  own instead of sending the error to the app's address: anyone can
  register an app with any https address, so kipp no longer redirects a
  user there before they chose. Only a denial on the consent page goes
  back to the app, as `access_denied`.
- The MCP tool `list_job_versions` refuses a `limit` outside 1 to 200, as
  the other tools refuse theirs, and the server's instructions tell an
  admin how the job tools are used.

- The MCP job tools refuse the etag `*`, which would change a job nobody
  read, and `read_log` holds a tail to `limit_bytes` (64 KiB by default) as
  it does a byte range, so a tail of long lines no longer fills a model's
  context with 256 KiB.

- The overview opens on the tab picked last in that browser, on Now when
  none was, whatever is failing, and no longer shows the `N failing` /
  `N unstable` / `Nothing failing` line above its tabs: the tab counts say
  it. `/?tab=<name>` still opens that tab.
- The Overview no longer shows the "N pull requests and branches red" line
  under Failing, Unstable and Flaky: it linked to a list that does not show
  them. The `hidden` counts stay in `GET /api/v1/overview` and the MCP
  `overview` tool.
- The job list's status tabs (All, Failing, Unstable, Running, Disabled)
  load their list when clicked, on a group's list too, and the address
  never changes: a reload goes back to All, `/jobs?status=<v>` still opens
  that tab, and the refresh keeps the open tab. The name filter stays and is
  applied to the new list. The tabs are `role="tab"` links that move with the
  arrow keys, Home and End.
- The Metrics page lists its stores in a box that scrolls under a fixed
  header, as the tables of a store already do.
- The overview shows Now, Failing, Unstable and Flaky as the tabs of one
  section, each with its count, and loads a tab's panel when it is clicked;
  it opens on Failing when any job is failing, otherwise on Now, and keeps
  the open tab through its refresh. The 24 hours / 7 days switch of the
  graphs loads the graphs alone. Neither puts anything in the address. The
  ended builds are a table of their own, Recent, under Now here and on the
  queue page. The user menu has one line between the name and the admin
  group and one before Log out.
- Sudo mode lasts 2 hours instead of 10 minutes, still counted from the
  moment the password was given: a change to logins, tokens, grants,
  secrets, GitHub Apps or jobs asks for the password once a sitting.
- Changing, enabling, disabling and removing an engine or a standing
  runner, setting the built-in runner's limit and removing cache entries
  no longer ask for the password: an admin makes them in their own browser
  session. Adding an engine or a runner and a runner's new token still ask;
  an API token still makes none of them.
- The Storage page is the Housekeeping page, at `/admin/housekeeping`; the
  old address leads to it. It opens at once and each section (disk and
  database, compact, recommendations, data directory, tables, jobs,
  self-check, disk floor) loads by itself, so that the walk of a big data
  directory holds back only what needs it. Recommendations, Data
  directory, Database and Jobs are tabs, each table in a box that scrolls
  under its header, and the page remembers the last tab in the browser (a
  link such as `#jobs` opens that one); Compact and Self-check sit side by
  side, one per line on a narrow screen. The API is unchanged.
- In the user menu the name leads to the Account page, and the "Account"
  item and the "Admin" label are gone. Admins find a new Deleted jobs item
  there: the Deleted jobs page now also lists the jobs whose file is gone
  and whose builds stay, with their last build and how many builds are
  kept. The "Removed" menu in the header and the removed jobs at the end of
  the Jobs page are gone, and `/jobs?only=removed` leads to that page. A
  user who is not an admin no longer gets a list of removed jobs, and keeps
  the page of one by its address. The API is unchanged.
- The builds of a job whose file is gone are kept 7 days from when they were
  queued, not 90 (still no more than the newest 100). This covers the builds
  of gone multibranch branches and pull requests too. The next sweep after an
  upgrade removes those older than 7 days; builds kept by hand stay.
- The overview's and `/queue`'s "Now" table, under what runs and waits, lists
  the 10 builds that ended last, of every status, in the jobs the viewer may
  view. Its heading is "Now" with the counts of what runs and waits. The API
  and the MCP tools are unchanged.
- On a phone, an ended build in "Now" says how it ended ("failed 1m ago") in
  the colour of that status, where it said only "ended".
- A hook (`on_success`, `on_failure`, ...) runs on the server, so its
  environment is the server's first: a stages file's `env:` and a stage's
  `KIPP_ENV` values only add names to it and never change one the job,
  `_vars.yml`, `--pass-env`, a param or a secret sets, and `PATH`, `HOME`,
  `LD_*`, `GIT_*` and the like are never taken from them. Stages get what
  they got before.
- A param named twice under `params:` is refused, as a stage named twice is.
- The daily sweep leaves the builds of jobs with no job file alone, and
  logs an error, when the job set is empty or more removed jobs would lose
  builds than the set has jobs: a jobs directory that is empty by mistake no
  longer costs every job its history. Remove now on the storage page still
  removes them.
- Each job keeps its newest 1000 activity lines, and a removed job's
  activity goes with its last build.
- A webhook delivery is read by one of four readers, at most two for one
  client address, and waits up to 3 seconds for a free one before 503.
- `/oauth/register` has a rate of its own: ten registrations in a row for a
  client address, then one a minute, where the wait grew to 15 minutes. A
  full table of clients makes room by removing the client unused longest
  that no grant names.
- A request body must arrive within 30 seconds, but for a runner's and a
  cache's uploads and the webhooks, which have their own bounds.
- The MCP tool `wait_build` counts among a user's 16 open streams and is
  refused past them.
- A second `kipp serve` on a data directory that is being served exits with
  1 before its launcher touches a pending upgrade there.
- Engine containers are labelled `kipp.server` with an id of the data
  directory, not the base URL: two servers that shared a Docker engine and a
  base URL removed each other's build containers. A container started by an
  earlier build is not removed by this one.
- A cache restore prefix matches keys in its own letter case only.
- The image tag `latest` is the build of `main` alone: a release no longer
  moves it.
- `examples/proxy`: nginx and Caddy pass a runner's log on as it comes and
  put no cap on a runner's request.

Fixed:

- The merge kipp makes of a pull request into its base is made by
  `kipp <kipp@localhost>` even when the server runs with `GIT_AUTHOR_*` or
  `GIT_COMMITTER_*` in its environment, which git puts before the identity
  kipp gives it.
- An MCP call the client gave up on, such as a `wait_build` it cancelled,
  is no longer logged as a server error.

- Stages of one build that run at once on one runner, or in one container,
  no longer share a workspace and write into each other's files: each stage
  there has `$KIPP_WORKSPACE` of its own, `<job>/<number>/<stage>/` under
  the runner's workdir (`<stage>/` under a container's). A stage that needs
  exactly one stage, and is the only one that needs it, still carries on in
  that stage's workspace. `artifacts_from` and the removal of the build's
  directory when the build ends are as they were.
- An `after` trigger that comes while the server is quiet, from a build that
  ends then, is no longer lost: it is kept in the database and runs when
  quiet ends, or at the next start when the server is restarted meanwhile.
  The database gains a table `pending_afters`.
- A cron fire refused while the server is quiet is kept in the database too,
  so a restart that follows the quiet no longer loses it: the job runs once
  at the next start, however many fires were refused. The database gains a
  table `pending_crons`.
- "Restart now" after an upgrade from a version that itself ran from the
  data directory starts the new version, where the older launcher served.
- A backup from before a version that was healthy is never put back by a
  later start, even when recording the upgrade's success failed.
- A server stop or restart no longer hangs while a stage runs on a runner or
  in a container; the build ends `lost`, as the others do.
- A build queued behind one a restart stops to run again stays queued and
  starts after the restart, where it started and was lost; a build
  cancelled by hand that was still ending is not run again.
- A stage given to a runner that goes offline before taking it is placed
  again instead of failing.
- A slow GitHub no longer holds up triggers, starts and cancels while a
  commit status is posted.
- A webhook delivery that met the quiet just after it was checked is kept
  (or answered 503), not answered 500 and lost; one answered 5xx can be
  sent again, where Redeliver was refused as a duplicate for 30 days.
- A reload that removes a multibranch job between two looks of the scanner
  no longer ends the server.
- A cache restore refuses a link whose target climbs out through another
  link, and a directory where a link is.
- A cache key of 248 to 512 characters saves; it failed every time.
- Half-written cache files a crash left are removed by the daily sweep.
- A cron step larger than its field (`*/9223372036854775807`) no longer
  panics the loader.
- A data directory whose path holds `?`, `#` or `%` opens its own database.
  A server that ran on such a path kept its database at the path cut short
  at that character; move it into the data directory before starting this
  build.
- A loopback `redirect_uri` with a user, a password or a fragment is
  refused at `/oauth/authorize` as it is at registration.
- The MCP tool `search_log` stops when its caller is gone.
- The page that creates a job answers 403 to someone who may not, before it
  reads the text they sent.
- `scripts/check-version.sh --release` refuses a tag with leading zeros
  (`v01.2.3`), as the update check does.
