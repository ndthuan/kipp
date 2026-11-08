# Changelog

What changes in kipp from one release to the next, newest first. The shape
is that of Keep a Changelog (https://keepachangelog.com/en/1.1.0/), and
versions follow Semantic Versioning (https://semver.org/): the job file,
the HTTP API under `/api/v1`, the command line, the server's reload log
lines and the data directory are the public surface.

## Unreleased

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
- A release build of `kipp serve` runs the higher release of its own
  executable and the binary `<data>/bin/kipp-<version>` that
  `<data>/bin/current` names, by `exec` (the process ID stays). A dev build,
  a version listed in `<data>/bin/rejected`, a missing or broken file, and
  `KIPP_USE_BUNDLED_BINARY=1` keep the executable's own. `origin` in
  `GET /api/v1/admin/server` then reads "data directory (the executable's
  own is vA.B.C)". Old lower binaries in `bin/` are removed after 7 days.
  Nothing writes `bin/current` yet.
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
- Groundwork for verified upgrades, with no change to what kipp does yet:
  `internal/update` checks that a release's `SHA256SUMS` is signed by the
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
