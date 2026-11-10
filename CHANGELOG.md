# Changelog

What changes in kipp from one release to the next, newest first. The shape
is that of Keep a Changelog (https://keepachangelog.com/en/1.1.0/), and
versions follow Semantic Versioning (https://semver.org/): the job file,
the HTTP API under `/api/v1`, the command line, the server's reload log
lines and the data directory are the public surface.

## Unreleased

Added:

- `kipp upgrade --dry-run [vX.Y.Z]` (and `--json`), the button **Check this
  upgrade** on the Server page's Upgrade card and
  `POST /api/v1/admin/upgrade/dry-run` make every check of an upgrade but
  the restart, and change nothing on the server: they find the release,
  check the room on the data directory's disk, download the release into a
  temporary directory there and check its signature and checksum, have the
  new binary check the jobs directory and migrate a copy of the database
  (then `PRAGMA integrity_check`), and count the builds a safe restart would
  wait for. Each check is reported as passed, failed with the reason, or
  skipped; the command exits non-zero when one did not pass. An admin's,
  and a row of the audit (`upgrade.dry-run`).
- `kipp migrate --data <dir>` brings the database of a data directory no
  server runs on up to this kipp's schema; the dry run runs the new
  release's on a copy.
- A Storage page for admins (`/admin/storage`) and
  `GET /api/v1/admin/storage`: the disk, each part of the data directory,
  each table and index of the database, each job's builds, rows, logs and
  artifacts, and what to change: the `keep` line for a job over a tenth of
  the database, removing now the builds of a job whose file is gone
  (`DELETE /api/v1/admin/storage/jobs/<name>`, a row of the audit), a
  compact, and files kipp did not make.
- `kipp db compact`, the button **Compact database** and
  `POST /api/v1/admin/storage/compact` compact the database: quiet on,
  wait for the builds, check the disk's room, `VACUUM` (the first one
  turns on incremental `auto_vacuum`), `PRAGMA optimize`, quiet off, and a
  row of the audit (`db.compact`).
- A daily self-check of the database (`PRAGMA quick_check`) after the
  03:30 sweep, on the Storage page and as `self_check` in
  `GET /api/v1/admin/server`; a failure is logged at ERROR and shows a
  banner to admins.
- Below a floor of free disk (5 GiB or a twentieth of the disk, whichever
  is less) kipp removes leftover stage temp, the workspaces of jobs whose
  file is gone, and idle cache, in that order, until the floor is met.

Changed:

- The daily sweep gives the pages it frees back to the disk
  (`PRAGMA incremental_vacuum`) once a compact has turned incremental
  `auto_vacuum` on.
- `kipp.case_seconds` keeps the newest 30 builds of each case per job.

## [1.3.0] - 2026-11-10

Added:

- A stage may be reused: with `reuse: {from, stage, tree, force}` it ends
  `success` without running when a build still kept, in a job `from` names
  (or one under it) or in its own, ran that stage green on the same git
  tree, with the same toolset fingerprint and on the same runner class. A
  pull request's merge and the squash that lands it have the same tree, so
  a deploy finds its gate green in the pull request's build. The job-level
  `fingerprint` (and `owner.repos.<name>.fingerprint`) lists the commands
  whose output names the toolset. Every checkout kipp makes records its
  tree. The stage's log says why it ran or which build it reused, the build
  page says `success, reused from <job> #<n>`, and the API and MCP stage
  carry `reused`.
- A user's page puts Tokens (and Connected apps) above Grants. With the
  script, the grants grid has a filter by job name (hidden rows keep their
  ticks and are still saved), a box in each group's heading that ticks or
  clears a column for the rows shown (build or cancel brings view), and a
  save bar that stays at the bottom of the window with the count of jobs
  changed while the form differs from what was loaded. For an admin the grid
  is folded under the sentence that says the grants count only once admin is
  taken away.

- Metrics is a link in the header beside Jobs, no longer a line in the menu
  under the user's name. That menu holds the name, Account, for an admin an
  "Admin" group (Users, Secrets, GitHub Apps, Runners, Cache, Server), and
  Log out after a rule.

- The overview's Coming up folds the jobs whose schedule fires more often
  than every 15 minutes (the smallest gap over the next 24 hours) into one
  line, "N jobs run every few minutes", that opens to list them; the rows
  are the next 8 builds of the other jobs. `upcoming` in the API and MCP
  is unchanged.

- Pages tell a time within 24 hours of now relative ("3h 5m ago", "in 9m"),
  and a time further away as date and time without seconds; every such time
  is a `<time>` with the full value, seconds and zone, in its `title`, and
  stays current on an open page. The server page and the audit list keep the
  exact clock time.

- Every build is given `KIPP_TOKEN`, a token of its own (`kippb_...`) that
  calls the server's API and `/mcp` back, as GitHub's `GITHUB_TOKEN` does:
  it is made when the build starts, held in memory, masked in every log and
  revoked when the build ends or the server stops. By default it may `view`
  its own job and nothing else; it is never an admin and never reaches
  accounts, tokens, secrets, the admin routes, the runner protocol or the
  cache.
- `permissions:` in a job file, one action or a list of `view`, `build` and
  `cancel` (or `read` and `write`) for each job by name, `self` for the
  build's own job; `permissions: none` for a build with no token. `kipp
  check` refuses an unknown action or job and `build` on the job's own
  name, and warns about a job that lists a secret named `KIPP_TOKEN`, which
  the build's token now hides.
- `max_permissions:` on a `source`, multibranch or owner job: the ceiling for
  the `permissions:` the repo's stages file or `.kipp.yml` may ask for. A
  file over it fails its build before the first stage, naming what is over
  and the job that would have to allow it.
- A build's token may start a build of a job it has `build` on, recorded as
  started by `build <job>#<number>`, and never one of its own job (403
  `forbidden`). `api/` describes build tokens as bearer tokens.
- The job list has a status choice (All, Failing, Unstable, Running,
  Disabled) in `?status=`, which combines with `group` and reads an unknown
  value as All, and a name filter that narrows the rows as you type (`/`
  goes to it, Escape empties it). The filter is made by a script, so the
  page without one is as it was.

Changed:

- A button that stands alone on the account, server and user pages (End the
  other sessions, Reload the job files, Make kipp quiet, Take builds again,
  Cancel upgrade, Make an admin, Take admin away, Reset the password) has a
  border, like Keep on a build page. The quiet look stays in the header, menus
  and table rows.
- On a narrow screen the overview shows Now (running and waiting builds) third,
  after Failing now and Unstable now and before Flaky, and the markup holds
  that order, so keyboard and screen reader order is the order seen. Two
  columns look as they did, placed with grid areas.
- A job's build list shows a pushed commit as its first 7 characters (the
  full id is the tooltip), the branch or tag the build was for, and under a
  red build's status where it failed, with a link to that stage's log, as
  the overview does. The build page's header shortens the commit the same
  way. The API and MCP still give the full value.
- After the password step an admin form comes back open and filled as it
  was sent, for the admin to send once more: add a user, make a token (own
  and for a user), set a secret (name, kind and scopes), change a secret's
  scopes, make a GitHub App, add an engine and add a standing runner. A
  secret's value, file, user and password and every password field are
  never carried. Adding an engine or a standing runner now leads back to
  the runners page after the password, not to the overview.
- A refused admin form comes back as its own page, with the status it had,
  the form open, its fields as they were sent (never a secret or a
  password) and why at its top, instead of a bare error page: add a user
  and make a token (own and for a user) now do, and a refused change of a
  secret's scopes opens that secret's row and not the Set form. The name
  fields of users, secrets, GitHub Apps, engines and runners carry the
  pattern of their rule, so a browser says what is wrong before sending.
  The error page's link leads back to the page of this site the request
  came from, and to the jobs only when there is none. A refused number of
  containers or stages no longer starts with the words "bad input".
- On a phone (under 48rem) the tables of the overview, the queue, the job
  list and a job's and a build's pages stack into rows instead of scrolling
  sideways: the name, its status and its age or duration on the first line,
  every other cell under it with its column's name. From 48rem up nothing
  changes.

Fixed:

- On a phone the admin tables (secrets, users, runners, GitHub Apps, tokens)
  no longer widen the page: a table's scrolling box now holds what is placed
  inside it, so the page does not scroll sideways, and the form behind a
  row's button (a secret's scopes) opens over the page inside the screen.
  From 48rem up nothing changes.

## [1.2.0] - 2026-11-09

- A build's log is easier to read: the text is larger, the page scrolls
  instead of each log, and times start hidden on a narrow screen (unless you
  chose). "Previous problem" and "Next problem" buttons, with a count, jump
  between the lines drawn in red or yellow or saying error, warning, FAIL or
  panic among the lines the page keeps.
- Changed: agents are now runners, the word GitHub and GitLab use, and the
  old name is gone with no alias. The command is `kipp runner` (`kipp runner
  add|ls|set|token|rm` on the server's data directory, and `kipp runner
  --url ...` for a standing runner); the server's flags are `--runner-listen`,
  `--runner-url` and `--runner-env`; a runner reads its token from
  `KIPP_RUNNER_TOKEN`; a stage that runs on one gets `KIPP_RUNNER`; the binary
  kipp puts into a container is `kipp-runner`, and the example unit is
  `examples/systemd/kipp-runner.service`. In the API the list is
  `/api/v1/admin/runners`, the runners' own calls are under `/runner/`, the
  security scheme is `runnerToken`, and the admin settings carry
  `runner_env`, `runner_url` and `runner_listen`. In the web UI the page is
  `/admin/runners`, "Runners". The audit log names new changes `runner.add`,
  `runner.set`, `runner.token` and `runner.rm`; earlier rows keep their
  `agent.*` names. The store is renamed by the upgrade itself (the table
  keeps its rows and every token keeps working), but an existing
  installation must change its server flags, and each runner its
  `KIPP_RUNNER_TOKEN` and command line, when it upgrades.
- Cancel asks first: the queue's and the build page's button lead to a page
  that names the build and how long it has run, and only the button there
  cancels, then returns to the page you came from. A page the queue refreshes
  keeps its tab title (it was renamed "Queue - kipp"), and a cancelled,
  timed out or lost stage no longer shows "exit -1".
- The overview answers what is broken before anything else: what is failing,
  unstable, flaky or failed lately comes first, then what runs and what is
  coming up, and the graphs come last, at every width. A failing job's row
  names the stages that failed (or the one a timeout stopped) and links to
  their logs. The page of a failed, timed out or lost build opens with a
  notice that says which stage and how it ended, links to its log and shows
  the log's last lines; the stage table links each stage to its log, and the
  heading of each log carries the stage's status.
- On a touch screen the controls are at least 44px tall: buttons, the header
  menus, the range switch, links in tables and the grants checkboxes (the
  whole cell is the label), and the overview's dots have a 44px hit area
  without being drawn larger.
- Saving a user's grants when the session needs the password again no longer
  loses the ticks: the form goes through the password step and is saved once
  the password is right. Build and cancel need view, since a user who may not
  view a job gets 404 from it: the grants form ticks View by itself, and a set
  with build or cancel and no view is refused, by the user's page and by
  `kipp user grant` (it now exits 2 with the reason). A grant already stored
  without view shows View ticked, and saving the page stores it.
- A GitHub webhook that arrives while the server is quiet before a restart is
  kept, not answered 503: a verified delivery is stored (202), handled in
  arrival order when quiet ends or the server starts, and built once however
  often it is sent. GitHub never sends a webhook again by itself, so a push
  or a tag that came during a safe restart was lost. Other triggers are still
  refused while quiet. The database gains a table for the kept deliveries.
- A `?cursor=` on a job's builds list that names a build number below 1 or
  above 2147483647 is refused as bad input. A source that makes kipp fetch
  many different OAuth client metadata documents is slowed after the first
  few, as wrong passwords are.
- Security: `kipp cache restore` no longer recreates a symbolic link that
  points outside the restored paths (an absolute target, or one that climbs
  out of them), so a later step cannot be led out through a cached link.
- The dashboard no longer counts pull requests and branches that are gone as
  disabled jobs, and the Groups table is folded into the pass-rate graph: a
  group's row there says how many of its jobs were never built or are
  disabled, each a link to the group's list.
- On a loaded machine a stage's leader in the middle of an exec could be
  read as having dropped its environment, and so be left alone when the
  stage was stopped. The wait for an exec to settle is now two seconds, not
  a tenth of one.
- `/mcp` answered `403 invalid Host header` to a request that a reverse
  proxy or tunnel on the same machine forwarded with the public `Host`. A
  request on a loopback connection now passes with a loopback `Host` or the
  host of `--base-url`, and any other `Host` is still refused.
- A queued build ended before it started (its job disabled or removed) was
  reported done by a wait, and by the API's build view, a moment before the
  line saying why had reached the job's activity. It stays busy until the
  line is written.

- A pull request build could fail before its first stage with git's "could
  not fetch ... from promisor remote" and "not our ref": `git merge` checks
  the merged files out as it writes them, and in the blob-less clone its
  prefetch asked the remote for a conflict blob it had just written, which
  no remote has, failing the whole fetch. The merge is now made in the
  object database (`git merge-tree`) and its result checked out afterwards.

- A rollback after `brew upgrade`, or after a container was recreated from
  a newer image, no longer restores the old database and then hangs at
  "switching": the executable's own version is then as high as the running
  one, so there is nothing to go back to, and no rollback is offered (the
  API answers 409 before anything is touched). When the executable takes
  over like that, the data directory forgets its previous version. A
  server that still holds such an unfinished rollback record settles it as
  done when it starts.
- The front page uses the width of a wide window: a strip of three graphs
  (builds over time by outcome, builds running at once against the
  `--builds` limit with the queue wait, and the pass rate of each group
  against the window before), then two columns, then the groups. A new
  Unstable now list holds the jobs whose newest build is unstable. A control
  switches the graphs and the failed list between 24 hours and 7 days
  (`?window=7d`). `GET /api/v1/overview` takes `window=24h|7d` and adds
  `unstable` and `charts`, and the MCP `overview` tool takes `window` and
  returns the unstable jobs and a summary of the graphs. The database gets
  an index on `builds.finished_at`. `go run ./internal/devtools/demo` makes
  a demo installation for screenshots and timing.

## [1.1.1] - 2026-11-08

- A second SIGTERM or SIGINT sent as soon as the server had logged that it
  was shutting down could be swallowed, leaving a slow shutdown to run to
  its end; the signals are now handed back to their default before the
  shutdown begins.

- The release workflow no longer runs the gate a second time. It requires
  the `ci` workflow to have passed on the tagged commit (waiting up to 30
  minutes for a run still going) and fails if there is none or it failed.

## [1.1.0] - 2026-11-08

- A standing agent that asked a restarted server for work before it had
  connected to it again was told its token was refused and stopped for
  good. It is now told to connect again, and carries on.
- The release signing key is in place: `internal/update/release.pub` holds
  the public half of the key that signs each release's `SHA256SUMS`
  (`minisign` key id `7BD1AD4FDBC773E5`), replacing the test-only
  placeholder, so the release workflow can now publish a release.
- `kipp serve --mcp` serves an MCP endpoint at `/mcp` (streamable HTTP, on
  `--listen` only), so that Claude and other MCP clients can read the
  overview, jobs, builds, logs, test results and the queue, and run,
  rerun and cancel builds. It takes the `Authorization: Bearer kipp_...`
  header of an API token and lets the token's user do only what their
  grants allow. Without `--mcp` nothing is served there. New dependency:
  `github.com/modelcontextprotocol/go-sdk`. See [docs/mcp.md](docs/mcp.md).
- `kipp serve --mcp-oauth` (with `--mcp` and an `https://` `--base-url`)
  makes kipp an OAuth 2.1 authorization server for `/mcp`, so that Claude
  can use it as a custom connector from claude.ai, the apps and Claude
  Code: discovery documents, `/oauth/authorize` with a consent page (the
  scopes `kipp:read` and `kipp:build`), `/oauth/token` with PKCE S256 and
  rotating refresh tokens, `/oauth/register` and clients named by the URL
  of their metadata document, and `/oauth/revoke`. The Account page lists
  the connected apps and a user's page does for an admin; a new password
  or the deletion of the user ends them. The database gains tables for
  clients, grants and token hashes (a migration; going back to a kipp
  before it needs the copy the upgrade keeps). Without the flag nothing
  changes.
- The front page is an overview: what is running and queued, the jobs that
  are failing now and for how long, the flaky ones (green now, but changing
  colour often among their last 20 builds; `kipp serve --flaky-flips`, 4 by
  default, 0 hides them), the failures of the last 24 hours, the next
  scheduled builds and a line per group. The full job list moved to `/jobs`,
  linked from the header, and `/?group=` and `/?only=` lead on to it.
  `GET /api/v1/overview` gives the same sections as JSON.
- Admins see a banner on every page when a newer release can be installed
  ("Remind me later" puts it off for that release for 7 days, in a cookie),
  and the Server page has an Upgrade card: the running and the new version,
  a tick box for the safe restart, the steps of a run with a Cancel button
  until the backup, the page reconnecting by itself after the restart, and
  afterwards how it ended, with a Roll back link or the failure's reason and
  log. Unticking the box asks again on a page of its own.
- `kipp upgrade` upgrades a running server to the latest release and
  `kipp upgrade --rollback` returns it to the version before. By default it
  makes the server quiet, waits for the builds to end (`--wait-timeout`,
  2 hours), backs up the data directory and restarts into the new version,
  which the launcher puts back if it does not come up; `--no-safe --yes`
  restarts at once and the builds that run end as `lost`, `--cancel` ends an
  upgrade that has not reached its backup, `--check` and `status` only say
  what is known. The release archive is downloaded from the repository's own
  GitHub releases (at most 200 MiB, redirects only within GitHub's release
  hosts), its checksum and the signature of `SHA256SUMS` are checked, and the
  unpacked binary is run once before anything is switched. The API has
  `GET`, `POST` and `DELETE /api/v1/admin/upgrade` and `POST
  /api/v1/admin/upgrade/rollback`, for an admin and recorded in the audit
  log. Where the data directory cannot be written, and in a dev build, it
  refuses and prints the lines that upgrade by hand. A rollback loses
  everything written since the upgrade.
- An upgrade backs up `db.sqlite` (through SQLite, so it is whole while the
  server runs, and checked) and `secret.key` into
  `<data>/upgrade/backup-<version>-<time>/`, after the builds have ended and
  just before the restart, having checked that the disk has room. When the
  new version has come up the backup is kept 7 days, and is what a rollback
  puts back; the backup of an upgrade that failed goes once a later one
  succeeds. `kipp serve` runs a version an upgrade left in
  `<data>/bin/pending` as a child, forwarding `SIGTERM`, `SIGINT` and
  `SIGHUP`, and counts it healthy when `/api/v1/health` answers, 90 seconds
  after the database migration ends (30 minutes at most). If it does not
  come up, the backup is put back, the version goes in `bin/rejected`, the
  reason and the end of its output in `<data>/upgrade/last-failure.json`,
  and the version before starts. The state is in `bin/current`,
  `bin/previous`, `bin/pending`, `bin/rejected` and
  `upgrade/status.json`. [Upgrading](docs/install.md#upgrading) and
  [Operations](docs/operations.md#upgrading) describe them.
- The Docker image sets `KIPP_INSTALL_SOURCE=docker`, so that the Server page
  and `kipp version` say it is a Docker image; the upgrade keeps its binary in
  the data volume. `KIPP_USE_BUNDLED_BINARY=1` pins the image's own kipp, and a pinned
  server refuses an upgrade or a rollback (409, before anything is
  downloaded) instead of restarting on the same version; the Server page
  then shows the manual lines.
  `scripts/smoke-upgrade.sh` upgrades real containers end to end.
- Docs: Upgrading in `docs/install.md` (from the app, `kipp upgrade`, by hand
  for Homebrew, Docker and an archive, a pinned image, rollback, a failed
  upgrade, installs from before this), Verifying a release by hand, the update
  check, and `kipp upgrade` and the upgrade routes in `docs/cli.md` and
  `docs/api.md`.
- A release build of `kipp serve` runs the higher release of its own
  executable and the binary `<data>/bin/kipp-<version>` that
  `<data>/bin/current` names, by `exec` (the process ID stays). A dev build,
  a version listed in `<data>/bin/rejected`, a missing or broken file, and
  `KIPP_USE_BUNDLED_BINARY=1` keep the executable's own. `origin` in
  `GET /api/v1/admin/server` then reads "data directory (the executable's
  own is vA.B.C)". Old lower binaries in `bin/` are removed after 7 days.
- The server asks GitHub for the latest release at start and about every 12
  hours (one unauthenticated request carrying only a `User-Agent` of
  `kipp/<version>`; a dev build never asks) and the Server page and
  `GET /api/v1/admin/server` say when a newer one exists: the new fields
  are `origin`, `update_check`, `latest`, `notes_url`, `upgradable` and
  `checked_at`. `kipp serve --no-update-check`, or `KIPP_NO_UPDATE_CHECK=1`,
  turns it off. `kipp version` also prints where the binary was installed
  from (Homebrew, a Docker image, or a release archive or build).
- The release workflow signs `SHA256SUMS` with the release key, checks the
  signature against the public key in the repository before anything is
  public, publishes it as `SHA256SUMS.sig`, and verifies the downloaded
  files again afterwards. It refuses a tag while the key in the repository
  is the placeholder.
- Verified upgrades rest on this: `internal/update` checks that a release's `SHA256SUMS` is signed by the
  release key compiled into kipp (an ed25519 signature in the minisign file
  format) and that an archive is listed there, and `go run ./cmd/kipp-release`
  makes the key (`keygen`), signs a release (`sign`) and checks one
  (`verify`). The key in the repository is a placeholder until a release
  key replaces it, and `scripts/check-release-key.sh` refuses it.
- A child that becomes gone (its pull request closed or merged, its branch
  deleted, or the scan no longer finding it) has its queued and running
  builds cancelled, saying why in the log and the activity. A queued build
  of a gone child used to fail. Its commit gets back the status of the
  child's build of it before, so a pull request merged the moment it went
  green, whose rebuild on the moved base started before GitHub said it
  closed, no longer ends with a red status on its head.
- `concurrency: supersede`: a new build of the job cancels its builds of the
  same ref (same branch, same tag, or no ref for both) that are still
  queued, and the job's activity names the build that replaced each. A
  running build is left to end.
- The queue badge in the header shows an icon and a count for running and
  queued builds, so it no longer wraps on a narrow screen.
- The header stays on one row on a phone, as it does on a wider screen.

## [1.0.0] - 2026-11-02

The first stable release. It is rc.2 with the release candidates' name
removed; what kipp does is listed under 1.0.0-rc.1 below.

## [1.0.0-rc.2] - 2026-11-02

The first release made from the public repository. Nothing in kipp itself
changed since rc.1; the archives, the image and the Homebrew formula are now
published from `ndthuan/kipp`.

## [1.0.0-rc.1] - 2026-11-02

The first release.

### Added

- Jobs are YAML files in a directory, read again on `SIGHUP` or `kipp
  reload` and refused whole when one is wrong (`kipp check` says why
  beforehand). Shared variables live in `_vars.yml`.
- A job runs by cron (with `H` to spread jobs over the hour), by a GitHub
  webhook on a push, a tag or a pull request, after another job, or by
  hand from its page, the API or `kipp run`, with typed params.
- Stages form a DAG and share a workspace; each has `when:` conditions,
  retries, a timeout, locks, env and secrets, and hands variables to the
  stages after it through `KIPP_ENV`. A build gets the variables a CI user
  expects (`CI`, `KIPP_COMMIT`, `KIPP_BRANCH`, `KIPP_PR_*`, ...).
- Multibranch jobs build every branch and pull request of a repo, and owner
  jobs every repo of a GitHub user or organization, signing in as a GitHub
  App or with a token, and post commit statuses.
- Stages run on the server, on standing agents that dial in, or in
  containers on Docker engines reached over a socket, TCP or ssh.
- Every build has a page with its stages as a graph and a live log, JUnit
  test results, artifacts, a keyed cache shared between builds, metrics
  with guards that fail or mark a build unstable, and hooks on success or
  failure.
- Users log in with a password and get what an admin grants them on each
  job (view, build, cancel); tokens serve the API and the command line;
  changes to accounts ask for the password again (sudo mode) and are kept
  in an audit log.
- Secrets are values, files or a user and password, sealed in the database
  with the server's key, scoped to the jobs that may use them, and masked
  in the logs.
- The JSON API under `/api/v1`, described by `api/openapi.yaml`.
- Quiet mode: the server takes no new build while the running ones end,
  for a restart that loses nothing.
- Release archives for Linux and macOS on amd64 and arm64 with build
  provenance, a container image for linux/amd64 and linux/arm64 on
  `ghcr.io/ndthuan/kipp`, and a Homebrew formula in `ndthuan/tap`.
