# AGENTS.md

The guide for whoever changes kipp, people and coding agents alike.

kipp is a job-first CI and scheduler in a single binary: jobs are YAML
files in a directory, builds run as shell commands on the server's machine,
an agent or a container, and the rest is one SQLite file. README.md and
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
  is the one exception (a remote agent would be the second).

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
  `examples/jobs`, and `make commits` (see Commit messages).
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
- Process and timing tests must hold under `go test -race -count=3 ./...`;
  one that fails one time in three is a bug.
- Dependencies are pinned in `go.mod`; check for the newest version before
  a bump.

## Commit messages

English, a subject of at most 80 characters that says what the change
does, and if there is a body, an empty second line and body lines of at
most 80 characters (a line that is one URL may be longer). Commit under
your own name and email.

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
- CLI flags and exit codes.
- The job file format.
- The two reload lines in the server's log, "jobs reloaded" and "reload
  refused": deploy scripts read them.
- The database's migrations (`internal/store/migrate.go`): append-only.
  Data directories made by each of them exist, so a schema change is a new
  entry at the end of the list and an existing entry is never edited or
  removed; `migrate_test.go` holds the sum of every released entry, and the
  new entry's sum is appended there with it. An entry runs inside the one
  transaction of the upgrade with foreign keys off, so a change SQLite's
  `ALTER TABLE` cannot make is a rebuild (create the new table, copy the
  rows, drop the old one, rename the new one) and every key is checked
  before it commits. The test that upgrades a database of every schema to
  the newest covers a new entry by itself; add one for what the entry does
  to the rows already there.

## Style

- A comment says why the code is as it is, never how it came to be.
- Secrets never reach a log or a test fixture; a secret in a test is an
  obvious test value.
- ASCII punctuation only: straight quotes, hyphens, three dots.

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
   changelog section; runs the gate; builds the archives (`make dist`) and
   smoke-runs each on a runner of its own platform, and the container image
   on each architecture (`scripts/smoke.sh`); then attaches the archives,
   `SHA256SUMS` and their build provenance to the release, pushes the image
   to `ghcr.io/ndthuan/kipp` (with its provenance), writes the Homebrew
   formula into `ndthuan/homebrew-tap` (`scripts/brew-formula.sh`), and
   installs and smoke-runs what it published, from the tap on macOS and
   from the registry on Linux.
4. Start a new `## Unreleased` section for what comes next.

`make dist` builds the same archives locally, in `dist/`, named by
`git describe` unless `VERSION` says otherwise; a version that is neither
a release tag nor what `git describe` prints is refused.
`scripts/smoke.sh bin <path>` and `scripts/smoke.sh image <ref>` run the
smoke test against a binary or an image of your own.

## Contributing

Changes come as pull requests with the gate green; CI runs it on Linux and
macOS, builds the archives and the image, and smoke-runs them
(`.github/workflows/ci.yml`). CONTRIBUTING.md says how to send one.
