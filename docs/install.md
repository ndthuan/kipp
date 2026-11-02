# Install

kipp is one binary, `kipp`, which is the server, the command line and
the standing agent. It runs on Linux and macOS, on amd64 and arm64. The
machine the server runs on needs a POSIX shell and git (git for jobs that
clone); nothing else.

- [Homebrew](#homebrew)
- [Docker](#docker)
- [Release archives](#release-archives)
- [From source](#from-source)
- [The first admin](#the-first-admin)
- [Limits](#limits)

## Homebrew

```sh
brew install ndthuan/tap/kipp
brew services start kipp
```

The formula installs the release archive for the machine's platform.
Its service runs:

```sh
kipp serve --data $(brew --prefix)/var/kipp --jobs $(brew --prefix)/etc/kipp --listen 127.0.0.1:8085
```

as your own user, and keeps it running. Job files go in
`$(brew --prefix)/etc/kipp`; the data directory is
`$(brew --prefix)/var/kipp`; the server's log is
`$(brew --prefix)/var/log/kipp.log`.

After a change to the job files, `kipp reload` (with a token, see
[Commands](cli.md#kipp-reload)) has the server read them again. To
upgrade, `brew upgrade kipp` and then `brew services restart kipp`; a
restart ends the builds that run then, so see
[Restarting without losing builds](operations.md#restarting-without-losing-builds).

`brew install --HEAD ndthuan/tap/kipp` builds the `main` branch from
source instead, with Go.

## Docker

```sh
docker run -d --name kipp -p 8085:8085 \
  -v kipp-data:/var/lib/kipp -v ./jobs:/etc/kipp ghcr.io/ndthuan/kipp
```

The image `ghcr.io/ndthuan/kipp` is built for `linux/amd64` and
`linux/arm64`. Each release `vX.Y.Z` is tagged `X.Y.Z`, `X.Y`, `X` and
`latest`.

It has two volumes, one for each kind of state:

- `/var/lib/kipp` is the data directory: the database, the secrets' key,
  logs, artifacts, the cache and the workspaces. kipp alone writes it;
  back it up as a whole ([Backups](operations.md#backups)).
- `/etc/kipp` is the jobs directory: the job files and `_vars.yml`. kipp
  only reads it.

The server runs as the user `kipp` (uid 10001), so a directory mounted at
`/etc/kipp` must be readable by that uid. Its command is:

```sh
kipp serve --data /var/lib/kipp --jobs /etc/kipp --listen 0.0.0.0:8085 --shell /bin/bash
```

To give `serve` more flags, pass the whole command after the image name:

```sh
docker run -d --name kipp -p 8085:8085 \
  -v kipp-data:/var/lib/kipp -v ./jobs:/etc/kipp ghcr.io/ndthuan/kipp \
  serve --data /var/lib/kipp --jobs /etc/kipp --listen 0.0.0.0:8085 --shell /bin/bash \
  --base-url https://ci.example.com
```

The stages a server runs itself run inside its container, with bash,
git, an ssh client, curl, zstd, tzdata and the CA certificates. A stage
that needs more runs in an image of its own on a Docker engine
([Agents and engines](agents.md)), or the server's image is built
`FROM ghcr.io/ndthuan/kipp` with what it needs added.

`docker kill -s HUP kipp` reloads the job files, as `kipp reload` does.
The image's health check asks `GET /api/v1/health`.

The session cookie is `Secure`. Chrome and Firefox keep a `Secure` cookie
from `http://localhost`, so they can log in at `http://localhost:8085`;
Safari does not. Anywhere else, put a reverse proxy with TLS in front of
the container ([Behind a proxy](operations.md#behind-a-proxy)).

## Release archives

Each release on the
[releases page](https://github.com/ndthuan/kipp/releases) has an archive
`kipp_<version>_<os>_<arch>.tar.gz` for `linux` and `darwin` (macOS) on
`amd64` and `arm64`. Each holds the `kipp` binary, which needs nothing
else, with the licence, the README, the changelog and THIRD_PARTY.md.

Check an archive against the release's `SHA256SUMS`, and against the
build provenance GitHub keeps for it, which says that the archive was
built by the repository's release workflow from the tagged commit:

```sh
shasum -a 256 -c SHA256SUMS --ignore-missing
gh attestation verify kipp_1.0.0_linux_amd64.tar.gz --repo ndthuan/kipp
tar xzf kipp_1.0.0_linux_amd64.tar.gz
sudo install -m 755 kipp_1.0.0_linux_amd64/kipp /usr/local/bin/kipp
```

The macOS binaries are not notarized, so Gatekeeper refuses one that a
browser downloaded until its quarantine flag is cleared:

```sh
xattr -d com.apple.quarantine kipp_1.0.0_darwin_arm64/kipp
```

To run it as a service, see
[Running as a service](operations.md#running-as-a-service).

## From source

Building needs Go 1.27.3 or newer, and make:

```sh
make build        # bin/kipp
```

`make build` first builds the Linux agents that kipp puts into containers
and embeds them. `make dist` builds the release archives and `SHA256SUMS`
in `dist/`; from the same commit, with the same Go and tar, they come out
byte for byte the same.

## The first admin

There is no default user and no default password. Once `kipp serve` has
started and made the data directory, make the first admin on the
server's machine:

```sh
kipp user add admin --admin --data <data-dir>
```

It prints a generated password once; `--password-stdin` reads one from
stdin instead (see [Passwords](accounts.md#passwords) for what is taken).
With Homebrew:

```sh
kipp user add admin --admin --data $(brew --prefix)/var/kipp
```

In the container:

```sh
docker exec kipp kipp user add admin --admin --data /var/lib/kipp
```

The command works on the data directory itself, so it runs as the user
the server runs as, while the server runs or not. Log in as `admin` with
that password; everyone else is added on the Users page or with
`kipp user add`.

## Limits

- Windows is not supported, for the server or for an agent.
- A stage on the server's machine runs as the server's user, isolated
  from that user's other files by nothing but a process group. Stages
  that must be kept apart run on an agent, in a container, or under
  another user ([Security](accounts.md#security)).
- The engines kipp runs containers on are Docker engines, reached over a
  Unix socket, TCP with TLS, or ssh.
- Logins are kipp's own users and passwords.
- Webhooks, multibranch jobs and organization folders work with GitHub
  and GitHub Enterprise (`serve --github-api`).
