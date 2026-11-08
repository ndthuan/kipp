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
button, `kipp run`, a cron fire, a scan and an `after` trigger. A webhook
is answered 503 with `Retry-After: 60` and its delivery is not kept, so
it can be sent again (GitHub's Redeliver) once the server is back; a scan reads
nothing, and the next one finds the commits it missed. A build that was
already queued is accepted work and still runs, and a running one ends by
itself. A cron fire that was refused runs once, for its job, when quiet
ends.

Quiet is held in memory only, so a restart ends it. `--wait` returns
once nothing runs or waits; at `--timeout` (default 2 hours) it turns
quiet off again and exits 1, so a restart that waited too long never
leaves the server refusing builds.

The same is `POST /api/v1/admin/quiet` (on), `DELETE /api/v1/admin/quiet`
(off) and `GET /api/v1/admin/quiet`, which answer `quiet`, `since`,
`running`, `queued` and `idle` (true when nothing runs or waits); an
admin's token or session only, and each change is in the audit. The
Server page has the same switch, and every page says so while the server
is quiet.

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
               the database as it was before the last upgrade, at schema N
               (mode 600); see Upgrading
bin/           kipp-<version> binaries and the files that choose among them
               (`current`, `previous`, `pending`, `rejected`); see Upgrading
upgrade/       progress of an upgrade in the making
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

## Upgrading

Stop the server, put the new `kipp` in place of the old one, and start
`kipp serve` again:

- Homebrew: `brew upgrade kipp`, then `brew services restart kipp`.
- Docker: `docker pull ghcr.io/ndthuan/kipp`, then remove the container
  and run it again with the same volumes.
- An archive: replace the binary and restart the service.

Use [quiet mode](#restarting-without-losing-builds) first so that no
build is lost. Standing agents run the server's own version: drain each
(`kipp agent set <name> --disable`), upgrade the server, upgrade and
restart the agents, then enable them again.

### A newer binary in the data directory

A release build of `kipp serve` looks, before anything else, at
`<data>/bin/current` and the binary `<data>/bin/kipp-<version>` it names.
When that is a higher release than the executable's own, the process
becomes it (`exec`: same arguments, environment and process ID, so a
service manager sees one process) with `KIPP_LAUNCHED_FROM_DATADIR=1`,
`KIPP_BUNDLED_EXE` and `KIPP_BUNDLED_VERSION` set to the executable it
replaced; the Server page then shows "data directory (the executable's
own is vA.B.C)". Equal or lower versions, a version listed in
`<data>/bin/rejected`, a file that is missing, not executable or that does
not answer `version` with its own version: the executable's own runs, and
a broken file is logged once. Only plain releases (`vX.Y.Z`) take part; a
dev build, or any other version, ignores `bin/` completely.
`KIPP_USE_BUNDLED_BINARY=1` keeps the executable's own, for a pinned image.
At start, `kipp-<version>` files lower than the running version and older
than 7 days are removed, except the one `previous` names.

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
