# Install

kipp is one binary, `kipp`, which is the server, the command line and
the standing runner. It runs on Linux and macOS, on amd64 and arm64. The
machine the server runs on needs a POSIX shell and git (git for jobs that
clone); nothing else.

- [Homebrew](#homebrew)
- [Docker](#docker)
- [Release archives](#release-archives)
- [From source](#from-source)
- [The first admin](#the-first-admin)
- [Upgrading](#upgrading)
- [Verifying a release by hand](#verifying-a-release-by-hand)
- [The update check](#the-update-check)
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

as your own user, and keeps it running. The data directory is
`$(brew --prefix)/var/kipp`; the server's log is
`$(brew --prefix)/var/log/kipp.log`.

It runs in `store` mode, the default of `--job-edits`: a job created or
changed through kipp (`kipp job`, the API) is kept in the database, and a
new install starts with the two example jobs `hello` and `hello-schedule`.
Job files you put in `$(brew --prefix)/etc/kipp` serve beside them, and
kipp never writes there; after a change to them, `kipp reload` (with a
token, see [Commands](cli.md#kipp-reload)) has the server read them again.
To have changes through kipp written to those files instead, run the
command above yourself with `--job-edits files`
([Changing jobs through kipp](jobs.md#changing-jobs-through-kipp)). To
upgrade, see [Upgrading](#upgrading).

`brew install --HEAD ndthuan/tap/kipp` builds the `main` branch from
source instead, with Go.

## Docker

```sh
docker run -d --name kipp -p 8085:8085 -v kipp-data:/var/lib/kipp ghcr.io/ndthuan/kipp
```

That is enough: the server keeps its jobs in its database (`store` mode,
the default of `--job-edits`) and starts with the two example jobs `hello`
and `hello-schedule` ([The example jobs](jobs.md#the-example-jobs)).

The image `ghcr.io/ndthuan/kipp` is built for `linux/amd64` and
`linux/arm64`. Each release `vX.Y.Z` is tagged `X.Y.Z`, `X.Y`, `X` and
`latest`.

It has two volumes, one for each kind of state:

- `/var/lib/kipp` is the data directory: the database (with the jobs
  changed through kipp and their history), the secrets' key, logs,
  artifacts, the cache and the workspaces. kipp alone writes it; back it up
  as a whole ([Backups](operations.md#backups)).
- `/etc/kipp` is the jobs directory: job files and `_vars.yml`, optional.
  The image has it empty, so a server with nothing mounted there loads no
  file.

The server runs as the user `kipp` (uid 10001). Its command is:

```sh
kipp serve --data /var/lib/kipp --jobs /etc/kipp --listen 0.0.0.0:8085 --shell /bin/bash
```

### Mounting a jobs directory

Job files kept beside the database, in git say, are mounted at
`/etc/kipp`, readable by uid 10001. What a change through kipp then does
decides how:

- **`store`** (the image's default): the change is kept in the database,
  over the job file of the same name, and kipp never writes the files.
  Mount the directory read-only:

  ```sh
  docker run -d --name kipp -p 8085:8085 \
    -v kipp-data:/var/lib/kipp -v ./jobs:/etc/kipp:ro ghcr.io/ndthuan/kipp
  ```

- **`files`**: the change is written to the job file, which stays the job.
  The mount must be writable (no `:ro`) by uid 10001: give the directory to
  that uid (`sudo chown -R 10001:10001 ./jobs`), or run the container as
  the host user who owns it (`--user "$(id -u):$(id -g)"`, with a data
  volume that user can write).

  ```sh
  docker run -d --name kipp -p 8085:8085 \
    -v kipp-data:/var/lib/kipp -v ./jobs:/etc/kipp ghcr.io/ndthuan/kipp \
    serve --data /var/lib/kipp --jobs /etc/kipp --job-edits files --listen 0.0.0.0:8085 --shell /bin/bash
  ```

  A server in `files` mode checks at start that it can write there, and
  refuses to start when it cannot. With the mount left `:ro` it says:

  ```
  kipp serve: --job-edits files cannot write job files in /etc/kipp as uid 10001: open /etc/kipp/.kipp-check-2509782686: read-only file system
  Make the directory writable by that user, drop :ro from a container's mount of it, or start with --job-edits store
  ```

  and `permission denied` in place of `read-only file system` when the
  directory is not writable by that uid.

A server that finds a job file at its first start does not create the
example jobs. One that ran before this kipp had `--job-edits` keeps loading
its mounted files after the upgrade, and creates no example job either.
`--job-edits off` refuses every change through kipp, as kipp did before.

With Compose, the two variants:

```yaml
services:
  kipp:
    image: ghcr.io/ndthuan/kipp
    ports: ["8085:8085"]
    volumes:
      - kipp-data:/var/lib/kipp
      - ./jobs:/etc/kipp:ro          # store mode: optional, read-only
    restart: unless-stopped

  # files mode: changes through kipp are written to ./jobs, owned by
  # uid 10001 (sudo chown -R 10001:10001 ./jobs).
  # kipp:
  #   image: ghcr.io/ndthuan/kipp
  #   command: serve --data /var/lib/kipp --jobs /etc/kipp --job-edits files --listen 0.0.0.0:8085 --shell /bin/bash
  #   ports: ["8085:8085"]
  #   volumes:
  #     - kipp-data:/var/lib/kipp
  #     - ./jobs:/etc/kipp
  #   restart: unless-stopped

volumes:
  kipp-data:
```

### Flags and the rest

To give `serve` more flags, pass the whole command after the image name:

```sh
docker run -d --name kipp -p 8085:8085 \
  -v kipp-data:/var/lib/kipp ghcr.io/ndthuan/kipp \
  serve --data /var/lib/kipp --jobs /etc/kipp --listen 0.0.0.0:8085 --shell /bin/bash \
  --base-url https://ci.example.com
```

The stages a server runs itself run inside its container, with bash,
git, an ssh client, curl, zstd, tzdata and the CA certificates. A stage
that needs more runs in an image of its own on a Docker engine
([Runners and engines](runners.md)), or the server's image is built
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

Each release also has `SHA256SUMS.sig`, a signature of `SHA256SUMS` by the
release key; [Verifying a release by hand](#verifying-a-release-by-hand)
checks it. Check an archive against the release's `SHA256SUMS`, and against
the build provenance GitHub keeps for it, which says that the archive was
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

Building needs Go 1.27.4 or newer, and make:

```sh
make build        # bin/kipp
```

`make build` first builds the Linux runners that kipp puts into containers
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

## Upgrading

An admin upgrades kipp from the app, or with `kipp upgrade`, and it is the
same for Homebrew, Docker and a release archive: kipp downloads the
release itself, checks its signature, backs up the database, restarts into
the new version, and puts the old version back if the new one does not
come up. Nobody runs `brew` or `docker`. [By hand](#upgrading-by-hand) is
for a server that is down, an install that cannot upgrade itself, and a
server with the update check off.

- [From the app](#from-the-app)
- [With kipp upgrade](#with-kipp-upgrade)
- [Checking an upgrade first](#checking-an-upgrade-first)
- [Upgrading by hand](#upgrading-by-hand)
- [A pinned image](#a-pinned-image)
- [Rolling back](#rolling-back)
- [When an upgrade fails](#when-an-upgrade-fails)
- [Installs from before this feature](#installs-from-before-this-feature)

What the files of an upgrade are, and how the launcher chooses among the
binaries, is in [Upgrading](operations.md#upgrading) in Operations.

### From the app

When a release newer than the running one exists and kipp can upgrade to
it ([the update check](#the-update-check) finds it), every page shows an
admin a banner, "kipp vX.Y.Z is available". **Remind me later** puts the
banner off for that release for 7 days, in a cookie of that browser; the
card below stays. **See and upgrade** goes to the **Upgrade** card on the
Server page (`/admin/server`, in the menu under the admin's name):

- It says what runs now and where it was installed from (Homebrew, a
  Docker image, a release archive), and links the new version's release
  notes.
- A tick box, ticked, **Wait for builds to end, then restart (safe
  restart)**, and the button **Upgrade to vX.Y.Z**.

A safe restart makes the server quiet ([Restarting without losing
builds](operations.md#restarting-without-losing-builds)), so that no
build starts and the triggers are refused, and waits until the builds that
run have ended (at most 2 hours, then it gives up, turns quiet off again
and restarts nothing). Only then does it back up the database and the
secrets' key, and restart. No build is lost. Unticking the box and pressing
the button asks again on a page of its own, which says how many running
builds will end as `lost`; the server then restarts at once.

The card shows the steps of the run: checking that it can run (room on the
disk for the backup and the download), downloading and verifying the
release, waiting for the builds, backing up, restarting. Every page shows
the step in a banner meanwhile. **Cancel upgrade** is there until the
backup begins; it turns quiet off again and leaves everything as it was.
Past that point only a [rollback](#rolling-back) undoes the upgrade.

When the server restarts, the page reconnects by itself, and gives up
with a message after 35 minutes (the 30 minutes the new version has to
come up, and five). Afterwards the card says "kipp is now vX.Y.Z", for 7
days, with a **Roll back** link; or it says why the upgrade did not
happen ([When an upgrade fails](#when-an-upgrade-fails)).

kipp downloads from its own repository's releases on GitHub alone (the
archive is at most 200 MiB), and refuses a redirect to any other host. It
checks the signature of `SHA256SUMS` with the release key compiled into
the running kipp, then the archive against `SHA256SUMS`, unpacks the binary
and runs it once (`kipp version`) before it changes anything.

An upgrade is an admin's, by browser or token alike, and a row of the
[audit](accounts.md#the-audit). Standing runners are not upgraded with the
server: drain each runner, upgrade the server, then upgrade and enable the
runners ([Runners and engines](runners.md#managing-them)).

The card has no button, and gives the lines to type instead, for a release
that has no `SHA256SUMS.sig` file (kipp does not install what is not
signed). There is no card at all in a dev build or with the update check
off. A server that cannot write the `bin/` and `upgrade/` directories of
its data directory (a volume mounted read-only, or owned by another user)
refuses when the button is pressed, and the answer has the same lines:
nothing has been changed.

### With kipp upgrade

```
kipp upgrade [status] [--check] [--dry-run [<version>] [--json]] [--cancel]
             [--rollback] [--no-safe [--yes]] [--wait-timeout <duration>]
             [--url <url>] [--token <token>]
```

With an admin's token, the same as the card ([Commands](cli.md#kipp-upgrade)):

```sh
kipp upgrade --check
# v1.1.0 is available and can be upgraded to: kipp upgrade
kipp upgrade
# upgrade: preflight (v1.0.1 to v1.1.0)
# upgrade: downloading (v1.0.1 to v1.1.0)
# upgrade: waiting (v1.0.1 to v1.1.0): 2 running, 0 queued
# upgrade: backup (v1.0.1 to v1.1.0)
# upgrade: switching (v1.0.1 to v1.1.0)
# upgrade: done (v1.0.1 to v1.1.0)
# kipp v1.1.0 is running
```

It prints a line each time the step changes (a quick one, such as
`verifying`, may pass unseen), follows the server through its
restart, and exits 0 when the new version runs, 1 when the upgrade failed
or was cancelled. `--no-safe --yes` restarts at once and ends the running
builds as `lost`; `--wait-timeout` is how long the safe restart waits
(default 2 hours); `--rollback` is [below](#rolling-back);
`--dry-run` is the next section.

### Checking an upgrade first

What can stop an upgrade is mostly seen only after the server has gone
quiet and restarted: a job file the new version refuses (every reload then
fails), a schema migration that fails, a disk without room. A dry run finds
these first, and changes nothing on the server: no quiet mode, no backup,
nothing in `bin/` or `upgrade/` of the data directory, no restart, no
pending record.

```sh
kipp upgrade --dry-run          # or --dry-run v1.4.0, which must be the latest
kipp upgrade --dry-run --json   # the same report as JSON
```

On the card, **Check this upgrade**, beside **Upgrade to vX.Y.Z**, does the
same and shows the report on the card. Each check says `passed`, `failed`
with the reason, or `skipped` when one it needs failed, in this order:

1. **release**: the release, found as the upgrade would find it (the same
   refusals: a dev build, the update check off, nothing newer, no
   signature file).
2. **space**: the room the upgrade asks for on the data directory's disk,
   and then the room for the dry run's own copy of the database beside it.
   It comes before anything is written.
3. **download**: the release downloaded into a temporary directory of the
   data directory, its `SHA256SUMS` signature and the archive's checksum
   checked with the same code as the upgrade, and the binary run once.
4. **jobs**: the new binary's `kipp check` on the server's jobs directory.
5. **migrate**: a copy of the database, made through SQLite as the
   upgrade's backup is, migrated by the new binary's `kipp migrate`, then
   checked with `PRAGMA integrity_check`.
6. **builds**: how many builds run and wait now, which a safe restart would
   wait for, so you know how long the real one's quiet would last.

The command exits 0 when every check passed and 1 otherwise; the temporary
files are removed at the end, pass or fail. The new binary runs as a
process of its own on the copies alone, never on the live data directory.
The running server drives the dry run and the new binary does the job
check and the migration, so it needs a server and a release that both know
it. It is an admin's, as the upgrade is, and a row of the audit
(`upgrade.dry-run`, with `passed` or `failed`).

### Upgrading by hand

For a server the app cannot upgrade, do what the app does yourself: make
it quiet, stop it, back up, put the new program in place, start it, check.

```sh
kipp quiet on --wait --timeout 2h
```

Then, for each kind of install, stop the server and copy `db.sqlite` and
`secret.key` out of its data directory (a stopped server has folded its
write-ahead log into the file, so a plain copy is whole; see
[Backups](operations.md#backups)), and put the new version in place:

- **Homebrew**:

  ```sh
  brew services stop kipp
  cp $(brew --prefix)/var/kipp/db.sqlite $(brew --prefix)/var/kipp/secret.key /backups/
  brew upgrade kipp
  brew services start kipp
  ```

- **Docker**: pull the new tag, and run the container again from it with
  the same volumes and flags. The image has no `sqlite3`, so copy the two
  files with another container:

  ```sh
  docker pull ghcr.io/ndthuan/kipp:1.1.0
  docker stop kipp
  docker run --rm -v kipp-data:/var/lib/kipp -v "$PWD/backup":/backup \
    alpine cp /var/lib/kipp/db.sqlite /var/lib/kipp/secret.key /backup/
  docker rm kipp
  docker run -d --name kipp -p 8085:8085 \
    -v kipp-data:/var/lib/kipp -v ./jobs:/etc/kipp ghcr.io/ndthuan/kipp:1.1.0
  ```

- **A release archive**: download and [check](#release-archives) the new
  archive, stop the service, copy the two files, replace the `kipp` binary
  with the one in the archive, and start the service
  ([Running as a service](operations.md#running-as-a-service)).

Then `kipp quiet status` says `quiet off` (quiet is in memory, so the
restart ended it), and the Server page says the new version. The new
version upgrades the database when it starts, and keeps a copy of the old
one ([A release with a newer schema](operations.md#a-release-with-a-newer-schema)).

If the data directory holds a binary from an earlier upgrade by the app
that is higher than the one you installed, that one runs, not yours: the
Server page says "data directory". Install a release that is at least as
high, or see [A pinned image](#a-pinned-image).

With Homebrew, the `kipp` on your `PATH` stays the version Homebrew
installed after an upgrade by the app, so `kipp version` there prints that
one, and the Server page prints the version that runs.

### A pinned image

A Docker image of a given tag is meant to run that tag. After an upgrade by
the app the new binary is in the data volume, not in the image, and a
container made from an older image (or the same image again) keeps running
the higher binary in the volume. To have the image's own kipp run, whatever
the volume holds, set `KIPP_USE_BUNDLED_BINARY=1`:

```sh
docker run -d --name kipp -e KIPP_USE_BUNDLED_BINARY=1 -p 8085:8085 \
  -v kipp-data:/var/lib/kipp -v ./jobs:/etc/kipp ghcr.io/ndthuan/kipp:1.1.0
```

Then the tag is the version, and an upgrade is a new tag, [by
hand](#upgrading-by-hand). A pinned server refuses an upgrade or a
rollback started in the app, `kipp upgrade` or the API (409) before it
downloads anything: the launcher is the part that carries one out, and
the variable skips it. The Server page then shows the manual lines and no
Upgrade or Roll back link. Leave the variable unset if you want the app to
do it. The
variable is read by any install, though a pinned image is where it is
used.

### Rolling back

After an upgrade that came up, the server can go back to the version it ran
before: **Roll back to vX.Y.Z** on the card, or `kipp upgrade --rollback`.
It waits for the builds (`--no-safe --yes` does not), puts the backup of the
upgrade back and starts the earlier version, which is the version of the
executable that started kipp if that was what ran before.

**A rollback loses everything written since the upgrade**: builds, users,
tokens, secrets and changes made on the pages, because the database is
the one the backup took. The logs and artifacts are files and stay, but a
build number the restored database gives out again clears the logs an
earlier build of that number left. The version rolled back from is not
chosen again by itself.

A rollback is possible while the backup exists: a week after an upgrade
came up, the backup is removed the next time the server starts, and the
card stops offering it. After that, going back is what the next section
does by hand.

A rollback is not offered either when the version it would go back to is
not lower than the one running, which is what happens after `brew upgrade`
or a container recreated from a newer image: the executable that started
kipp is then as new as the data directory's binary, takes over, and the
data directory forgets the version before. The API answers 409 and nothing
is touched, since restoring the old database under the same version would
only lose data.

By hand, when the server will not start or the backup is older: stop it,
put the files of the backup (`db.sqlite` and `secret.key` in
`<data>/upgrade/backup-<version>-<time>/`) in the data directory over the
two there, remove `db.sqlite-wal` and `db.sqlite-shm`, and make the earlier
version the one that runs: remove `<data>/bin/current` for the executable's
own (the image, Homebrew's or the archive's), or write the version in it
(`v1.0.1`) if its file `<data>/bin/kipp-v1.0.1` is still there. Then
start the server.

```sh
cd /var/lib/kipp
cp upgrade/backup-v1.0.0-20261103T101500Z/db.sqlite db.sqlite
cp upgrade/backup-v1.0.0-20261103T101500Z/secret.key secret.key
rm -f db.sqlite-wal db.sqlite-shm bin/current
```

### When an upgrade fails

Before the restart, the upgrade fails with nothing changed: no room on the
disk, a download or a signature that does not check, builds still running
at the end of the wait, a backup that could not be made. The card says "The
upgrade to vX.Y.Z did not finish, and nothing was changed" and why;
`kipp upgrade` prints the reason and exits 1; quiet is off again.

```
upgrade: failed (v1.0.1 to v1.0.2): checksum check failed: kipp_1.0.2_linux_arm64.tar.gz does not match its sha256 in SHA256SUMS
```

After the restart, the launcher keeps the new version as a child process for
up to 30 minutes, and counts it healthy when `/api/v1/health` answers
`{"ok":true}` within 90 seconds of the end of the database migration. When
it does not (it exits, or is not healthy by then) the launcher stops it,
puts the backup back, remembers the version as rejected, and starts the
version before. The card says "vX.Y.Z did not start; back on vY.Y.Y", the
reason, and links **The end of its log**: the last 4 KiB the failed
version printed. The server's own log has the line `the new version is not
healthy; going back`, and the same record is in `GET
/api/v1/admin/upgrade` as `last_failure`, and in
`<data>/upgrade/last-failure.json` (`version`, `from`, `reason`, `log_tail`,
`at`). Nothing is tried again by itself; start the upgrade again when the
cause is gone. The data written between the restart and the check is lost
with the rest, as in a rollback.

From the restart of an upgrade until the next start, the process a
service manager or `docker` sees (PID 1 in a container) is the launcher,
with the server as its child: it passes `SIGTERM`, `SIGINT` and `SIGHUP`
to the child, and its exit code on.

### Installs from before this feature

kipp 1.0.0 and earlier have no update check, no Upgrade card, no
`kipp upgrade` and no launcher, so the first step is by hand, once:
[upgrade by hand](#upgrading-by-hand) to the first release that has the
Upgrade card ([the changelog](../CHANGELOG.md) says which). The old
version makes no backup of its own, so take the backup that section has;
it is also the way back, together with the old program. From then on
the app upgrades, with Homebrew, Docker and archives alike. In Docker the
data volume must be writable by the user the image runs as (uid 10001),
which it has to be for the server to run at all.

## Verifying a release by hand

Each release has `SHA256SUMS`, the checksums of its archives, and
`SHA256SUMS.sig`, a signature of that file made with the release key by the
release workflow. The key's public half is `internal/update/release.pub` in
the repository, and is compiled into every kipp, which checks the same
signature before it installs anything. The signature is in the
[minisign](https://jedisct1.github.io/minisign/) file format, so the
`minisign` tool checks it with no Go; read the key from the tag of the
release, so that the key and the release belong together:

```sh
V=v1.1.0
curl -fsSLO https://github.com/ndthuan/kipp/releases/download/$V/SHA256SUMS
curl -fsSLO https://github.com/ndthuan/kipp/releases/download/$V/SHA256SUMS.sig
curl -fsSLo release.pub https://raw.githubusercontent.com/ndthuan/kipp/$V/internal/update/release.pub
minisign -Vm SHA256SUMS -x SHA256SUMS.sig -p release.pub
shasum -a 256 -c SHA256SUMS --ignore-missing
```

`minisign` prints `Signature and comment signature verified` and exits 0
for a genuine file, and `Signature verification failed` and exits 1 for one
that was changed or signed by another key. The last line checks the
archives you downloaded against the signed list (`sha256sum -c` on Linux).
`minisign` is in Homebrew and in most Linux distributions' packages.

With Go, in a checkout of the repository at the tag, the release tool
checks the signature against the key compiled into that checkout, and one
archive against the list:

```sh
git clone --branch $V https://github.com/ndthuan/kipp && cd kipp
go run ./cmd/kipp-release verify -sums ../SHA256SUMS -sig ../SHA256SUMS.sig \
  -archive ../kipp_1.1.0_linux_amd64.tar.gz
# ../kipp_1.1.0_linux_amd64.tar.gz: signature and checksum ok
```

`-pub <file>` checks against another key, and leaving out `-archive`
checks the signature alone. The tool also has `keygen` and `sign` for whoever
releases a fork (`scripts/sign-release.sh sign <dir>` is the release
workflow's use of it); it is not part of the archives or the image.

The signature covers `SHA256SUMS`, so it vouches for the archives in the
list and for what an upgrade installs from them. It does not cover the
Docker image.

## The update check

A server of a release (not a dev build) asks GitHub whether a newer one
exists: one unauthenticated `GET` of
`https://api.github.com/repos/ndthuan/kipp/releases/latest` when it starts
and about every 12 hours after. The request carries what any HTTP request
does, including a `User-Agent` of `kipp/<version>`, and GitHub sees the
server's address as it sees any client's; it carries nothing about your
jobs, builds, users or install. A draft or a pre-release is not an answer.
The Server page and `GET /api/v1/admin/server` then say which version is
the newest, link its release notes, and whether kipp can upgrade to it (the
release has a `SHA256SUMS.sig` file; its signature is checked when an admin
starts the upgrade, not by the lookup). The answer is kept in
`update-check.json` in the data directory, so a restart does not ask again
inside the interval. A server that cannot reach GitHub, or is refused, says
nothing and tries again later.

It is on by default. Turn it off with `kipp serve --no-update-check`, or
with `KIPP_NO_UPDATE_CHECK=1` (or `true`) in the server's environment; the server then
makes no request at all, and knows of no release, so the banner, the card
and `kipp upgrade` have nothing to offer: [upgrade by
hand](#upgrading-by-hand). `kipp version` never asks.

## Limits

- Windows is not supported, for the server or for a runner.
- A stage on the server's machine runs as the server's user, isolated
  from that user's other files by nothing but a process group. Stages
  that must be kept apart run on a runner, in a container, or under
  another user ([Security](accounts.md#security)).
- The engines kipp runs containers on are Docker engines, reached over a
  Unix socket, TCP with TLS, or ssh.
- Logins are kipp's own users and passwords.
- Webhooks, multibranch jobs and organization folders work with GitHub
  and GitHub Enterprise (`serve --github-api`).
