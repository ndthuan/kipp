# Changelog

What changes in kipp from one release to the next, newest first. The shape
is that of Keep a Changelog (https://keepachangelog.com/en/1.1.0/), and
versions follow Semantic Versioning (https://semver.org/): the job file,
the HTTP API under `/api/v1`, the command line, the server's reload log
lines and the data directory are the public surface.

## Unreleased

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
