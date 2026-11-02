# Changelog

What changes in kipp from one release to the next, newest first. The shape
is that of Keep a Changelog (https://keepachangelog.com/en/1.1.0/), and
versions follow Semantic Versioning (https://semver.org/): the job file,
the HTTP API under `/api/v1`, the command line, the server's reload log
lines and the data directory are the public surface.

## Unreleased

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
