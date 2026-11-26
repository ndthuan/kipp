# AGENTS.md

The guide for whoever changes kipp, people and coding agents alike.

kipp is a job-first CI and scheduler in a single binary: jobs are YAML
files in a directory, builds run as shell commands on the server's machine,
a runner or a container, and the rest is one SQLite file. README.md and
docs/ are the user's view: the job file, the commands, the data directory.

## Layout

- `cmd/kipp/main.go` is wiring only. Behaviour lives in `internal/<pkg>`,
  with its tests beside it.
- `api/` is the OpenAPI description of the HTTP API: `openapi.yaml` and the
  files in `paths/`, `schemas/`, `responses/` and `parameters/` are the
  sources, `openapi.gen.yaml` is what `make openapi` bundles them to (a file
  under `schemas/` is the component of its base name, a file under `paths/`
  is one path). `internal/apispec` reads it and `apispec/apispectest` holds
  handlers to it; neither is in `bin/kipp`, and a test holds that.
- Dependencies point one way: `config` and `store` at the bottom, then
  `dispatch`, `api`, `web`, and `cli` on top.
- A rule is written once, in the lowest package that knows it, and
  imported upwards: `config.ValidName`, `config.ValidEnvName`, the names
  of a build's own logs, `api.MaxBody`.
- No interface before a second implementation needs it. `runner.Runner`
  is the one exception (a remote runner would be the second).

## Tests and the gate

- Test first. A change without a failing test first is not done. A test's
  name states the rule it holds.
- The gate is `make quality`, green before a change is proposed. It runs
  gofmt, `go vet`, golangci-lint (staticcheck runs inside it, with
  modernize, nolintlint and gocognit), govulncheck (a known
  vulnerability the code reaches fails it), `go test -race`, the node
  harnesses for the page scripts in `internal/web/static/` (they print
  `NOT VERIFIED` where there is no node, and that is not a pass),
  actionlint on the workflows and shellcheck on `scripts/` (`NOT VERIFIED`
  without shellcheck, the same), and `kipp check` on `testdata/jobs` and
  `examples/jobs`. The Makefile
  builds the pinned tools with the toolchain `go.mod` names, so no
  `GOTOOLCHAIN` is needed by hand.
- `make test` writes `coverage.out`; `make cover` turns it into
  `coverage.json`, a kipp metrics report (metric `coverage`, one item per
  package and `total`). CI holds the total to the newest green run on
  `main`, as a kipp guard with `no_lower` does: a fall of more than 0.1
  points fails the pull request, and the job summary lists each package
  whose coverage moved (`COVERAGE_BASE=<report> make cover` does the same
  locally).
- No function above cognitive complexity 15. One over the limit is split,
  not excused.
- A finding is fixed in the code, never hidden from the linter: no
  `//nolint`, and no code written only so the linter cannot see what it
  flags. What is left is a finding whose flagged behaviour is the feature
  (running a job's command, a test about a file's mode); it goes in the one
  exclusions list in `.golangci.yml`, by file and rule, with the reason.
- A test waits on an event, never on the clock: `time.Sleep` in a
  `_test.go` file is refused by the linter. Code that runs on timers is
  tested in a `testing/synctest` bubble (a fake clock); a state the code
  can announce (a channel, a hook, a change counter such as
  `Dispatcher.Changes`) is waited for on that; what it cannot announce is
  waited for with `internal/waitfor.Until`, whose 30 second deadline is
  only a safety net. A sleep that stays is one whose subject is real time
  across a process, listed in `.golangci.yml` with its reason.
- Process and timing tests must hold under `go test -race -count=3 ./...`;
  one that fails one time in three is a bug.
- Dependencies are pinned in `go.mod`; check for the newest version before
  a bump.
- The flake hunt (`.github/workflows/flake-hunt.yml`, weekly and by hand)
  runs every package under `-race -shuffle=on` with two busy loops, as
  parallel shards so no job runs much over 30 minutes: the slowest packages
  are named in the hunt step, a shard each, and the shard `rest` is every
  package not named, so a new package needs no change there. It runs at
  `-cpu 1` on Linux and macOS and at `-cpu 4` on Linux only. It is not a
  gate, and a normal run does not find what it finds. A failing test turns
  the job red and is listed in the job summary (package, test, failed runs,
  `-cpu`, shuffle seed, first lines), built by `internal/devtools/flakes`
  from `go test -json`. A test it reports is fixed at the root, a race in
  the test or a bug in the code, never retried or skipped. Keep the hunt
  cheap by these rules, which people apply (nothing automates them):
  - The count is the `HUNT_COUNT` repository variable, else 20, and a
    dispatch can set it once. It is never below 10, which the job enforces.
  - After four green weekly runs in a row, set `HUNT_COUNT` to 10. After a
    run that finds a flaky test, set it back to 20 (delete the variable)
    until four runs in a row are green again.
  - When a package's shard grows past about 30 minutes, give it a shard of
    its own (a line in `named` and in the matrix); when a package's repeats
    do not fit the 60 minute `-timeout` (`web`, `api` and `dispatch` today),
    cut it into parts (`parts` and the matrix), each running its share of
    the count; when a shard named there no longer holds a package, the job
    fails, so remove it.

## Commit messages

English, a subject of at most 80 characters that says what the change
does, and if there is a body, an empty second line before it. Commit under
your own name and email, in UTC (see A public repository).

## Public surfaces

Others read these, so a change to one is a decision, not a refactor:

- HTTP statuses and bodies, and the description of them in `api/`: an API
  change updates the spec in the same commit, and the gate fails
  otherwise. A route nobody described, an operation nobody serves, an
  answer the spec does not allow (a status, a content type, a field more or
  fewer than its schema has) and an operation no test got a good answer
  from each fail `go test`. Run `make openapi` after editing the sources and
  commit `api/openapi.gen.yaml` with them. A test that serves a handler
  wraps it in `apispectest.Wrap`.
- `/mcp`, served with `serve --mcp` and documented in `docs/mcp.md`: its
  tools and their arguments. It is not under `/api/v1`, so `api/` does not
  describe it; `internal/mcp` tests it directly. So are the routes of
  `serve --mcp-oauth` (`/.well-known/oauth-*`, `/oauth/...`), documented
  there and tested in `internal/oauth` and `internal/web`.
- CLI flags and exit codes.
- The job file format.
- The two reload lines in the server's log, "jobs reloaded" and "reload
  refused": deploy scripts read them.
- The database's migrations (`internal/store/migrate.go`): append-only.
  The first entry is the whole schema kipp starts from (schema 1). Data
  directories made by each entry exist, so a schema change is a new entry
  at the end of the list and an existing entry is never edited or removed;
  `migrate_test.go` holds the sum of every entry, and the new entry's sum
  is appended there with it. An entry runs inside the one
  transaction of the upgrade with foreign keys off, so a change SQLite's
  `ALTER TABLE` cannot make is a rebuild (create the new table, copy the
  rows, drop the old one, rename the new one) and every key is checked
  before it commits. The test that upgrades a database of every schema to
  the newest covers a new entry by itself; add one for what the entry does
  to the rows already there.

## Demo data and screenshots

The pictures in `docs/screenshots` come from a made-up installation, never
from a real one: no name, address or log of anyone's server appears in
them. Take them again when a page they show changes, and with the same
commit as the change.

1. Make the demo: `go run ./internal/devtools/demo -dir <new dir>` fills
   `<dir>/data` and `<dir>/jobs` with about 70 jobs in four groups and
   none, 14 days of builds that overlap and queue, jobs that fail, flap or
   end unstable, a few that sleep so that builds are running and waiting,
   one multibranch job with live and gone branches and pull requests, and
   coverage reports from three unit jobs for the metrics pages. The same
   seed and time make the same store. It also times the overview on a
   store of a realistic size. It is not in the shipped binaries.
2. Add an admin and start a server on it, alone on its ports, with eight
   executors (six builds run, two slots stay free for the build the
   screenshots start), the pages over HTTPS with a self-signed certificate
   (the session cookie is `Secure`):

   ```sh
   go build -o bin/kipp ./cmd/kipp
   openssl rand -hex 12 > <dir>/.pass
   bin/kipp user add demo --admin --password-stdin --data <dir>/data < <dir>/.pass
   bin/kipp serve --data <dir>/data --jobs <dir>/jobs --executors 8 \
     --listen 127.0.0.1:18085 --tls-listen 127.0.0.1:18443 \
     --timezone UTC --no-update-check
   ```

3. In another shell, take the pictures:

   ```sh
   KIPP_DEMO_URL=https://127.0.0.1:18443 KIPP_DEMO_USER=demo \
   KIPP_DEMO_PASSWORD="$(cat <dir>/.pass)" KIPP_DEMO_JOBS=<dir>/jobs \
   PLAYWRIGHT_CHANNEL=chrome make screenshots
   ```

   `make screenshots` installs Playwright (not a dependency of the repo) in
   a directory of its own and runs `internal/devtools/screenshots/shoot.cjs`,
   which writes a job with secrets and a job whose build fails into the
   jobs directory, sets one secret, makes an edit wait in conflict, runs the
   failing build, and writes every picture (it accepts the demo's
   self-signed certificate): 1440 px wide in light, the
   overview also in dark, and 390 px wide for the pages that must not scroll
   sideways on a phone. `PLAYWRIGHT_CHANNEL=chrome` uses the Chrome
   installed on the machine; leave it out to have Playwright fetch its own.
4. Stop the server and look at every picture before committing it. The
   demo changes what it wrote, so start again from step 1 for another run.

## Style

- No comments, except those a language's style requires: in Go, the doc
  comment of a package and of each exported name (revive's `exported` rule
  holds them), saying what it is or does; and the reason beside each entry
  of the exclusions list in `.golangci.yml`. Directives (`//go:build`,
  `//go:embed`, `# shellcheck`), a shebang, the version beside a pinned
  action's SHA and a Makefile target's `## help` text are not comments and
  stay. What a script writes, a heredoc's text, is its output, not a
  comment. `examples/` and the seed jobs in `internal/jobstore/seed` are
  for users to read and keep theirs. `internal/devtools/comments` holds
  this in `go test`: Go by its syntax tree, the workflows, Makefile,
  Dockerfile, scripts, page scripts and styles line by line.
- Secrets never reach a log or a test fixture; a secret in a test is an
  obvious test value.
- ASCII punctuation only: straight quotes, hyphens, three dots.

## A public repository

Everything here is public, the history included, and stays so after a
force push: GitHub keeps every pull request's commits. So nothing of
anyone's own deployment goes into a file, a commit message, a branch name
or a pull request:

- no hostname, domain, IP address, machine or runner name, path under a
  home directory, or account of a real installation; tests and docs use
  `example.com`, `ci.example.com`, `192.0.2.x` and made-up names;
- no key of an outside tracker (a ticket, an issue in another system) in a
  commit message or a branch name; say what the change does instead;
- no CI configuration of a private installation (`.kipp.yml` and the
  like); kipp's CI is the GitHub Actions in `.github/workflows`, and no
  outside server reports commit statuses or receives webhooks from this
  repository;
- commits are made in UTC (`TZ=UTC git commit`, and for a rebase
  `TZ=UTC`, so author and committer dates both carry `+0000`).

## The latest build

kipp has no release yet. Every push to `main` whose CI passed is published
by the `latest` job of `.github/workflows/ci.yml`: the archives, a
`SHA256SUMS` signed with the release key and their build provenance on
the GitHub release `latest` (a pre-release; its tag moves to the commit,
and new files are uploaded before old ones are removed), and the image
`ghcr.io/ndthuan/kipp:latest`. Nothing else is tagged.

A build is named by `scripts/version.sh`: `git describe` of the newest
`vX.Y.Z` tag when there is one, else `latest-<commit time, UTC>-<12 hex
of the commit>`, with `-dirty` for changes not committed. Such a name is
never a release, so the update check and `kipp upgrade` offer nothing to
a `latest` build, and a build that is not a release ignores
`<data>/bin/`. `latest` is for trying kipp; the README says so.

## Releasing

1. Rename `## Unreleased` in `CHANGELOG.md` to `## [X.Y.Z] - YYYY-MM-DD`
   and check that its lists say what changed since the last release. Set
   `info.version` in `api/openapi.yaml` to `X.Y.Z` and run `make openapi`
   (a test holds it equal to the newest `## [X.Y.Z]` heading). The
   release's notes are that section, word for word
   (`scripts/release-notes.sh vX.Y.Z` prints them).
2. Merge it into `main` with CI green.
3. Publish a GitHub release whose new tag `vX.Y.Z` (or
   `vX.Y.Z-<pre-release>`) is on that commit. `.github/workflows/release.yml`
   does the rest: it refuses a tag that is not a release's
   (`scripts/check-version.sh --release`), is not on `main` or has no
   changelog section; requires the `ci` workflow to be green on that exact
   commit (a push to `main` that completed with success, waited for up to
   30 minutes if still running, `scripts/check-ci.sh`) and never runs the
   gate again; builds the archives (`make dist`) and
   smoke-runs each on a runner of its own platform, and the container image
   on each architecture (`scripts/smoke.sh`); then signs `SHA256SUMS` with
   the secret `RELEASE_SIGNING_KEY` and checks the signature against
   `internal/update/release.pub` (`scripts/sign-release.sh`), attaches the
   archives, `SHA256SUMS`, `SHA256SUMS.sig` and their build provenance to
   the release, pushes the image
   to `ghcr.io/ndthuan/kipp` (with its provenance), writes the Homebrew
   formula into `ndthuan/homebrew-tap` (`scripts/brew-formula.sh`), and
   installs and smoke-runs what it published, from the tap on macOS and
   from the registry on Linux, and verifies the downloaded `SHA256SUMS`,
   its signature and an archive. The first job refuses a tag while
   `internal/update/release.pub` is still the placeholder
   (`scripts/check-release-key.sh`), and the release is signed by the
   secret alone, so the secret must hold the key that file belongs to.
4. Start a new `## Unreleased` section for what comes next.

`make dist` builds the same archives locally, in `dist/`, named by
`scripts/version.sh` unless `VERSION` says otherwise; a version that is
neither a release tag nor one `scripts/version.sh` or `git describe`
prints is refused.
`scripts/smoke.sh bin <path>` and `scripts/smoke.sh image <ref>` run the
smoke test against a binary or an image of your own.

## Contributing

Changes come as pull requests with the gate green; CI runs `make gogate`
on Linux and macOS and `make js` once on Linux in a job of its own, all at
once, builds the archives and the image, and smoke-runs them
(`.github/workflows/ci.yml`). CONTRIBUTING.md says how to send one.

`.github/workflows/flake-hunt.yml` runs once a week and on demand, never on
a push or a pull request; Tests and the gate says how it runs and how it is
kept cheap.

AGENTS.md is changed by maintainers only, since coding agents here read
it as their instructions: `.github/CODEOWNERS` names the owner for it
and for `.github/`, and `.github/workflows/protected-files.yml` (the check
`agents-md`, to be required by the ruleset of `main`) fails a pull request
that changes it from anyone without push access. That check runs on
`pull_request_target`, so a pull request cannot change it, and reads only
the list of files, never the pull request's code; keep it so. To review
a pull request from a fork, coding agents read AGENTS.md from `main`, not
from the pull request.
