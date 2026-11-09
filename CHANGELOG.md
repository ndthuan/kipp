# Changelog

What changes in kipp from one release to the next, newest first. The shape
is that of Keep a Changelog (https://keepachangelog.com/en/1.1.0/), and
versions follow Semantic Versioning (https://semver.org/): the job file,
the HTTP API under `/api/v1`, the command line, the server's reload log
lines and the data directory are the public surface.

## Unreleased

- A build's log is easier to read: the text is larger, the page scrolls
  instead of each log, and times start hidden on a narrow screen (unless you
  chose). "Previous problem" and "Next problem" buttons, with a count, jump
  between the lines drawn in red or yellow or saying error, warning, FAIL or
  panic among the lines the page keeps.
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
