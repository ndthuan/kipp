# Operations

- [Running as a service](#running-as-a-service)
- [Behind a proxy](#behind-a-proxy)
- [Restarting without losing builds](#restarting-without-losing-builds)
- [Reload lines](#reload-lines)
- [The data directory](#the-data-directory)
- [Backups](#backups)
- [Upgrading](#upgrading)

## Running as a service

`kipp serve` stays in the foreground and logs to stderr, so a service
manager runs it. Homebrew's `brew services start kipp` and the Docker
image do this already ([Install](install.md)). [`examples/`](../examples)
has a start for the others:

- [`examples/systemd/kipp.service`](../examples/systemd/kipp.service): a
  user of its own, the filesystem read-only but for `/var/lib/kipp`, and
  `systemctl reload kipp` for `SIGHUP`.
- [`examples/systemd/kipp-agent.service`](../examples/systemd/kipp-agent.service):
  a standing agent on another machine, its token in an environment file.
- [`examples/launchd/com.example.kipp.plist`](../examples/launchd/com.example.kipp.plist):
  a launchd agent on macOS, with the `PATH` its stages need.

Whatever runs it, a stage runs as that user with that user's rights, and
the service's limits are the stages' limits too.

`GET /api/v1/health` answers `{"ok":true}` to anyone, for a probe. The
log is text lines of `time=... level=... msg=...`; every wrong password is
a `WARN` line.

## Behind a proxy

kipp speaks plain HTTP; for anything but the machine it runs on, put a
reverse proxy with TLS in front of it.
[`examples/proxy/Caddyfile`](../examples/proxy/Caddyfile) and
[`examples/proxy/nginx.conf`](../examples/proxy/nginx.conf) do what
follows.

- Pass the `Host` header through as the browser sent it: the login form
  is refused as cross-origin when it does not match.
- Set `X-Forwarded-For` and `X-Forwarded-Proto`. Start kipp with
  `--client-ip-header X-Forwarded-For` so that wrong passwords are
  counted by client too, and with `--trusted-proxy` naming the proxy's
  address when it is not on the same machine (loopback is the default).
- Send `Strict-Transport-Security` from the proxy: kipp cannot know that
  the browser came over HTTPS, so it does not.
- Do not buffer the answers: the log pages, `/api/v1/queue/events` and
  the `?wait=true` long-polls of `kipp run --wait` stream, and a quiet
  stream stays open, so give it a read timeout of an hour, not the usual
  minute.
- Allow a request body of 25 MiB for the webhooks (GitHub's own cap).
- Start kipp with `--base-url https://<the proxy's name>`, which is the
  `KIPP_URL` its stages and hooks link to, and the address webhooks and
  commit statuses name.
- Agents dial `/api/v1/agent/`. To keep them off the proxy, serve them on
  a listener of their own with `--agent-listen`
  ([Agents and engines](agents.md#the-servers-flags)).

## Restarting without losing builds

A restart ends every running build as `lost`. To restart when nothing
runs, make the server quiet first:

```sh
kipp quiet on --wait --timeout 2h   # refuse new builds, wait until none runs or waits
# ...restart the server...
kipp quiet status                   # quiet off: 0 running, 0 queued
```

While quiet, no build starts and every new trigger is refused with
`kipp is quiet: restarting once running builds end`: the API, a Build
button, `kipp run`, a cron fire, a scan and an `after` trigger. A person or a
schedule can try again, and a scan reads nothing (the next one finds the
commits it missed). A GitHub webhook is the exception, since GitHub never
sends one again by itself: a delivery whose signature checks out is answered
202 `{"kept": ...}` and stored in the database with its id, event and body,
and nothing starts. When quiet ends, and at the server's start after a
restart, the stored deliveries are handled in the order they arrived, exactly
as if they had just arrived, and then forgotten. The delivery id (and the
body's sum) is recorded when it is stored, so a delivery is built once
whether GitHub sends it again while quiet, after the restart or by Redeliver.
One stored for longer than a quiet lasts (more than two hours and ten
minutes) is handled all the same and logged with its age. The log says
`delivery kept while quiet` and `delivery replayed after quiet` for each. A
delivery with a bad signature is refused as always. A build that was
already queued is accepted work and still runs, and a running one ends by
itself. A cron fire that was refused runs once, for its job, when quiet
ends.

A kipp that predates this still answers 503 to a webhook that arrives while
it is quiet, so the restart that upgrades to this version may still lose one.

Quiet is held in memory only, so a restart ends it. `--wait` returns
once nothing runs or waits; at `--timeout` (default 2 hours) it turns
quiet off again and exits 1, so a restart that waited too long never
leaves the server refusing builds.

The same is `POST /api/v1/admin/quiet` (on), `DELETE /api/v1/admin/quiet`
(off) and `GET /api/v1/admin/quiet`, which answer `quiet`, `since`,
`running`, `queued` and `idle` (true when nothing runs or waits); an
admin's token or session only, and each change is in the audit. The
Server page has the same switch, and every page says so while the server
is quiet. An upgrade does this, and the restart, for you
([Upgrading](install.md#upgrading)).

## Reload lines

A reload (`kipp reload`, `SIGHUP`, the Server page) writes one of two
lines to the server's log, which a deploy script may wait for:

```
level=INFO msg="jobs reloaded" count=12
level=ERROR msg="reload refused; the previous job set stays" err="..."
```

The wording of these two lines is kept from release to release.

## The data directory

Only `kipp serve` creates it, for its owner alone (mode 700). One that is
already there keeps its mode, and a mode that lets group or others in is
logged as a warning. A database or key that group or others can read is
refused.

```
db.sqlite      builds, users, token hashes, sealed secrets (mode 600)
db.sqlite-wal, db.sqlite-shm
               SQLite's write-ahead log, there while the server runs
db.schema-N.sqlite
               the database as it was before the last schema upgrade, at
               schema N (mode 600); see Upgrading
bin/           kipp-<version> binaries an upgrade downloaded and the files
               that choose among them (`current`, `previous`, `pending`,
               `rejected`); see Upgrading
upgrade/       the backups of upgrades, their progress (`status.json`) and
               the last version put back (`last-failure.json`); see
               Upgrading
update-check.json
               the newest release the update check found, and when
secret.key     the key the secrets are sealed with (mode 600)
device.key     the key the device cookies are signed with (mode 600)
lock           held by the running server; one server per directory
logs/          <job>/<number>/<stage>.log, and <stage>.log.times beside it:
               when each line began
workspace/     <job>/<number>/, removed when the build ends
artifacts/     <job>/<number>/<stage>/<path>: the files stages kept,
               removed with their build
cache/         the keyed caches stages kept, as tar.zst
apps/          <name>.pem and <name>.json of the GitHub Apps made on the
               GitHub Apps page (directory mode 700)
secret-files/, stage-tmp/
               the file secrets and KIPP_TMP of the stages that run now,
               removed when each stage ends
```

A server that starts removes what a crashed one left in `workspace/`,
`secret-files/` and `stage-tmp/`.

## Backups

Back up `db.sqlite` and `secret.key` together: neither opens the secrets
alone. `device.key` need not be backed up: a server that makes a new one
only takes every browser for a stranger until its next login. Logs and
artifacts are files under `logs/` and `artifacts/`, and the cache can be
left out: it fills again.

While the server runs, the newest of the database is in `db.sqlite-wal`,
and a copy of `db.sqlite` alone can come out all but empty. Either stop
the server first, which folds the log into the file, or take the copy
through SQLite, which is safe while the server runs:

```sh
sqlite3 data/db.sqlite ".backup 'backup.sqlite'"
```

In Docker, back up the volume mounted at `/var/lib/kipp` with the
container stopped, or run the `sqlite3` line above from a container or
host that has `sqlite3` and the volume mounted.

An upgrade takes a backup of its own, of the database and the key, just
before it restarts ([The backup an upgrade makes](#the-backup-an-upgrade-makes)).

## Upgrading

How to upgrade, from the app, with `kipp upgrade` or by hand, and how to
roll back, is in [Install](install.md#upgrading). This section is what an
upgrade does in the data directory and how the next start chooses what to
run.

### The backup an upgrade makes

Once the server is quiet and no build runs (a safe restart) and just
before it restarts, an upgrade takes a backup in
`<data>/upgrade/backup-<version>-<time>/`, named for the version that was
running and the UTC time (`backup-v1.0.1-20261103T101500Z`). It holds
`db.sqlite`, copied through SQLite so that it is whole while the server is
running and checked with `PRAGMA integrity_check`, and `secret.key`, both
for their owner alone. Logs, artifacts, caches and `device.key` are not in
it. The backup is taken last so that what a rollback loses is only what was
written after the restart.

Before it starts, an upgrade checks that the disk of the data directory has
room for the backup (the database, its write-ahead log and the key) and
128 MiB more for the download, and says how much is missing when it has
not. A disk whose free space cannot be read does not stop it.

When the new version has come up, the backup is marked with a `succeeded`
file, and is kept 7 days from then: the next start of the server removes it
after that. A backup of an upgrade that failed is removed once a later
upgrade has succeeded. The backup a pending record names is never removed.
A rollback puts back the newest backup that has the mark.

### The files in bin/ and upgrade/

`<data>/bin/` holds the binaries an upgrade downloaded, `kipp-<version>`
(mode 755), and four small files that choose among them. Each is written by
a rename, so a reader sees the old file or the new, never half of either.

| File | Holds |
|---|---|
| `bin/current` | The version run by default, such as `v1.1.0`. Absent: none, the executable's own runs. |
| `bin/previous` | The version before it, or `bundled` when that was the executable that started kipp (the image's, Homebrew's or the archive's). A rollback goes there. Removed when the executable that started kipp is as high as `bin/current` and takes over. |
| `bin/pending` | JSON for the next start to carry out: `{"kind":"upgrade","version":"v1.1.0","backup":"backup-v1.0.0-20261103T101500Z","from":"v1.0.0"}`. `kind` is `upgrade` or `rollback`; a rollback names the version to return to in `version`, `bundled` allowed. Gone once carried out. `pending.refused` is one that could not be, because its backup is missing. |
| `bin/rejected` | One version per line that the start must never pick by the higher-wins rule below: a version that did not come up, or that was rolled back from. |

`<data>/upgrade/` holds the backups, and:

| File | Holds |
|---|---|
| `upgrade/status.json` | Where the upgrade is: `state` (`preflight`, `downloading`, `verifying`, `waiting`, `backup`, `switching`, `done`, `failed`, `cancelled`), `version`, `from`, `backup`, `running`, `queued`, `updated_at`. It survives the restart, which is how the Server page and `kipp upgrade` follow a run through it. |
| `upgrade/last-failure.json` | Why the last version that did not come up was put back: `version`, `from`, `reason`, `log_tail` (the end of what it printed), `at`. |
| `upgrade/migrating` | There while the server migrates its database; the launcher counts the wait for it separately. |

### How the next start picks a binary

A release build of `kipp serve` looks, before anything else, at
`<data>/bin/current` and the binary `<data>/bin/kipp-<version>` it names.
When that is a higher release than the executable's own, the process
becomes it (`exec`: same arguments, environment and process ID, so a
service manager sees one process) with `KIPP_LAUNCHED_FROM_DATADIR=1`,
`KIPP_BUNDLED_EXE` and `KIPP_BUNDLED_VERSION` set to the executable it
replaced. Higher wins, so a `brew upgrade` or a new image that is at least
as high as the data directory's binary runs itself.

The executable's own runs instead when the versions are equal or lower, the
version is listed in `bin/rejected`, the file is missing, is not
executable or does not answer `version` with its own version (a broken
file is logged once), or `KIPP_USE_BUNDLED_BINARY=1` is set, which is what
[a pinned image](install.md#a-pinned-image) uses. Only plain releases
(`vX.Y.Z`) take part; a dev build, or any other version, ignores `bin/`
completely.

At each start, `kipp-<version>` files lower than the running version and
older than 7 days are removed, except the one `previous` names and the one
a pending record names; so is a backup past its 7 days.

The Server page and `GET /api/v1/admin/server` say, as `origin`, where the
running kipp comes from: `Homebrew`, `Docker image`, or `release archive or
build`, or, when a binary of the data directory runs, `data directory (the
executable's own is vA.B.C)`. Homebrew is told by a `Cellar` directory in
the executable's path, and Docker by the image's `KIPP_INSTALL_SOURCE=docker`.
`kipp version` prints it too, on its second line, but cannot see the data
directory's.

### A pending version is tried, and undone if it does not come up

When `<data>/bin/pending` names a version, `kipp serve` runs that binary as
a child (`KIPP_SUPERVISED=1`), forwards `SIGTERM`, `SIGINT` and `SIGHUP`
to it, passes its output and exit code through, and stays the parent. The
child is healthy when its `/api/v1/health` (on `--listen`) answers
`{"ok":true}`: it then becomes `current`, the version before it becomes
`previous`, and `pending` goes. The deadline is 90 seconds, counted from
the end of the store's migration (a server holds `<data>/upgrade/migrating`
while it migrates), and 30 minutes in all. A child that exits, or is not
healthy by then, is stopped, the backup named in `pending` is put back,
the version goes into `<data>/bin/rejected`, the reason and the end of the
child's output into `<data>/upgrade/last-failure.json`, and the previous
version (or the executable itself, `bundled`) starts in place of the
launcher. Nothing is tried again until an upgrade is started again. A
`rollback` record is carried out before anything starts: its backup goes
back, and the target starts. One whose backup is missing is set aside as
`pending.refused`. A supervised server that exits with code 75 has written
a new record, which the launcher reads next. The new version starts not
quiet, so a build may start before the check passes; what it writes is
lost with the rest if the version is put back.

### A release with a newer schema

A release whose database schema is newer upgrades the database when it
starts, before it listens, and logs it:

```
level=INFO msg="database upgraded" from_schema=1 to_schema=2 copy=data/db.schema-1.sqlite
```

Before it changes anything it copies the database as it was to
`db.schema-<N>.sqlite` beside it, readable by its owner alone, and it
removes the copy of any earlier upgrade: one copy is kept, of the schema
the kipp before ran on. The upgrade is one transaction, so one that fails
leaves the database as it was, and `serve` exits saying why.

Only `serve` upgrades. Every other command that opens the data directory
(`user`, `token`, `secret`, `engine`, `agent`, `metrics`, `cache`)
refuses a database at any schema but its own and says what to do: until
the new `kipp serve` has started once, run those with the kipp the server
runs.

No kipp opens a database a newer one has upgraded. To go back a release,
stop the server, put the copy in the database's place, remove the
write-ahead log beside it, and start the older kipp:

```sh
mv data/db.schema-1.sqlite data/db.sqlite
rm -f data/db.sqlite-wal data/db.sqlite-shm
```

What was recorded after the upgrade, builds and users and secrets alike,
is not in the copy. The older kipp gives out again the build numbers given
since, and a build clears the logs an earlier build of its number left.
