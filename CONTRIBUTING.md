# Contributing

Thank you for wanting to help. Before a large change, open an issue to
talk about it first.

[AGENTS.md](AGENTS.md) is the guide for anyone who changes kipp, people
and coding agents alike: the layout, the rules for tests, the style, and
which surfaces others depend on.

## Running it locally

You need Go (the version in `go.mod` or newer), git and a POSIX shell;
node for the page scripts' tests, and shellcheck for the scripts', are
used when they are there.

    make build                  # bin/kipp
    make quality                # the gate
    make help                   # every target

`bin/kipp serve --data ./data --jobs ./examples/jobs` starts a server on
`http://127.0.0.1:8085`; the README's Quick start makes its first user.
`scripts/smoke.sh bin bin/kipp` starts a throwaway server and runs a job on
it, and `scripts/smoke.sh image <ref>` does the same with an image built
from the Dockerfile.

## Commit messages

Write them in English: a subject of at most 80 characters, and if there is
a body, an empty second line and body lines of at most 80 characters (a
line that is only a URL may be longer). Commit under your own name and
email.

## Pull requests

- One change per pull request, with a test that failed before it.
- Coverage does not fall: CI fails a pull request whose total coverage is
  more than 0.1 points below `main`'s, and lists what moved per package.
- `make quality` green on your machine. CI runs it on Linux and macOS,
  and smoke-runs the archives and the container image built from it.
- A change to a public surface (HTTP, the command line, the job file, the
  log lines deploy scripts read, the database) says so, and adds a line
  to `## Unreleased` in `CHANGELOG.md`.
- Security problems go through [SECURITY.md](SECURITY.md), not a pull
  request.
