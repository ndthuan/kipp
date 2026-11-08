# The API

Everything a machine calls on a kipp server is under `/api/v1`: the JSON
API, the webhooks GitHub delivers to, and the calls between kipp and an
agent of the same version (tagged `internal`, with no promise of
stability).

- [The description](#the-description)
- [Authentication](#authentication)
- [Errors](#errors)
- [Shapes](#shapes)
- [Lists](#lists)
- [Query values](#query-values)
- [Routes](#routes)
- [Admin reads](#admin-reads)
- [Examples](#examples)

## The description

The OpenAPI 3.1 description of every route is
[api/openapi.yaml](../api/openapi.yaml), split into small files beside
it and bundled into one file,
[api/openapi.gen.yaml](../api/openapi.gen.yaml). A running server serves
the bundle at `GET /api/v1/openapi.yaml`, to anyone without a login
(`application/yaml`, with an `ETag` and a 304 for a matching
`If-None-Match`), so it is the description of the code that answers. The
tests hold the handlers to it: an answer the description does not allow
fails them, and so does a route it leaves out.

## Authentication

- **A token**: `Authorization: Bearer kipp_...`, made on the Account page
  or with `kipp token create` ([Tokens](accounts.md#tokens)). It acts as
  its user, with that user's grants.
- **A browser session**: the session cookie of a login. A request that
  changes something carries the session's CSRF token in `X-CSRF-Token`.

`GET /api/v1/health` and `GET /api/v1/openapi.yaml` need neither. The
webhooks are authenticated by their signature, the agent calls by the
agent's token, and the cache calls by the build's cache token. Anything
else without a token or a session is 401.

`GET /api/v1/health` answers `{"ok":true}` to anyone, and adds `version`
and `uptime_seconds` for a caller with a token or a session.

## Errors

Every 4xx and 5xx is a problem document (RFC 9457,
`application/problem+json`) with `type`, `title`, `status`, `detail` and a
stable `code` such as `not_found` or `job_disabled`, which a program
decides by:

```json
{"type": "https://github.com/ndthuan/kipp/blob/main/api/problems.md#not_found",
 "title": "Not found", "status": 404, "detail": "no such path", "code": "not_found"}
```

[api/problems.md](../api/problems.md) lists the codes with their status
and meaning. A path or a method the API has no route for is a 404 or a
405 problem (with `Allow`) too. A job, build or store the caller may not
view is 404, as one that does not exist is.

## Shapes

- An answer has the same fields every time: none is left out for having
  no value, it is `null` there, and a list is `[]`, never `null`.
- A time is RFC 3339 in UTC with exactly three fraction digits
  (`2026-10-30T12:00:00.123Z`), whatever zone the server runs in; the
  log's timestamps and the stream's `time` event too. The pages show
  times in the server's zone (`--timezone`, else the host's).
- A duration is a whole number with its unit in the name:
  `every_seconds` of a scan, `uptime_seconds`, `session_ttl_seconds`,
  `duration_ms` of a test case.
- A build is `{id, job, number, status}` wherever another answer refers
  to one (a `BuildRef`): the build a remembered metric came from, a
  queued build's `blocker`, the `saved_by` of a cache entry, a stage an
  agent runs. Its `status` is `null` once retention has removed the
  build.
- A build in a list, in a job's `last` and in the answer to a trigger has
  no `stages`; `GET /api/v1/builds/<id>` has its `stages` and `complete`,
  which is true when the build has ended and nothing more will be written
  for it.

## Lists

A list is never a bare array but
`{"items": [...], "next_cursor": string|null}`.

Six lists are paged: the builds of a job, a job's activity, a metric's
history, the audit, the cache entries and a build's test results. Each
takes `?limit=` (a whole number from 1; the default is the list's own, 50
builds and activity lines, 100 of the rest, and a number over 500 is
read as 500) and `?cursor=`. A page that is not the last carries a
`next_cursor`; send it back as `cursor` to read the page after, and stop
when it is `null`. A cursor is opaque and means the position after the
last row of its page, so rows added at the newest end while the pages are
read are never repeated or skipped. A cursor kipp did not give for that
list is a 400 `bad_input`.

A job's page, `GET /api/v1/jobs/<name>`, holds the first page of its
builds and of its activity in the same shape. Every other list answers
all its rows with a `next_cursor` of `null`.

## Query values

A query value that does not parse is a 400 `bad_input`, never a default:
`limit`, `cursor`, `offset`, and the flags `wait`, `download` and
`timestamps`, which are `true` or `false` (`1` is not accepted).
Parameters a route does not know are ignored.

## Routes

A resource is named once. A build is `/builds/<id>`, and also
`/jobs/<name>/builds/<number>` by its number in the job. What a stage
kept and printed is below the stage. A stage the build does not have is
a 404; `prepare`, `clone` and `hooks` are the build's own logs and are
found there too. Actions are a `POST` on a sub-resource. Everything only
an admin may read is below `/admin/`.

| Route | Needs | Answer |
|---|---|---|
| `GET /health` | nothing | `ok`, and `version` and `uptime_seconds` for a signed-in caller. |
| `GET /openapi.yaml` | nothing | The description. |
| `GET /jobs` | login | The jobs the caller may view, each with its last build and next run. |
| `GET /jobs/<name>` | `view` | The job, its params, the first pages of its builds and activity. |
| `GET /jobs/<name>/builds` | `view` | Its builds, newest first, paged. |
| `POST /jobs/<name>/builds` | `build` | Starts a build: body `{"params": {"KEY": "value"}}` or none. 201 with the build. |
| `GET /jobs/<name>/builds/<number>` | `view` | One build by its number in the job. |
| `GET /jobs/<name>/activity` | `view` | The lines of the job's activity (`at`, `line`), newest first, paged. |
| `GET /jobs/<name>/children` | `view` | A multibranch job's children ([Children](multibranch.md#children)). |
| `POST /jobs/<name>/scan` | `build` | Starts a scan of a multibranch or owner job; 202 with how it stands. |
| `GET /jobs/<name>/artifacts/latest` | `view` | A redirect to the newest build's artifacts ([Artifacts](builds.md#artifacts)). |
| `GET /builds/<id>` | `view` | The build with its stages. `?wait=true` holds the request until the build has ended, or the server's wait is over. |
| `POST /builds/<id>/cancel` | `cancel` | Tells the build to stop; 202 with the build. |
| `POST /builds/<id>/again` | `build` | Builds it again ([Building again](builds.md#building-again)); 202 with the new build. |
| `PUT`, `DELETE /builds/<id>/keep` | `build` | Keeps the build, or stops keeping it. |
| `GET /builds/<id>/stages/<stage>/log` | `view` | One chunk of the log, or the whole with `?download=true` ([Logs](builds.md#logs)). |
| `GET /builds/<id>/stages/<stage>/log/events` | `view` | The log as server-sent events. |
| `GET /builds/<id>/results` | `view` | The JUnit cases, paged. |
| `GET /builds/<id>/metrics` | `view` | What the build reported, and its guards ([Metrics](metrics.md#api)). |
| `GET /builds/<id>/artifacts`, `.../stages/<stage>/artifacts`, `.../artifacts/<path>`, `.../artifacts.zip` | `view` | Artifacts. |
| `GET /metrics`, `/metrics/<store>`, `.../remembered`, `.../history`, `.../compare` | `view` on a job that names the store | Metric stores ([Metrics](metrics.md#api)). |
| `GET /queue` | login | What runs and what waits ([The queue](builds.md#the-queue)). |
| `GET /queue/events` | login | A `changed` event each time the caller's queue changes. |
| `POST /admin/reload` | admin | Reads the job files again; a refused set is a 422 `jobs_refused` with the errors. |
| `GET`, `POST`, `DELETE /admin/quiet` | admin | Quiet mode ([Restarting without losing builds](operations.md#restarting-without-losing-builds)). |
| `GET`, `POST`, `DELETE /admin/upgrade`, `POST /admin/upgrade/rollback` | admin | Upgrading the server ([Upgrade](#upgrade)). |
| `GET /admin/users`, `/admin/users/<name>`, `/admin/secrets`, `/admin/server`, `/admin/engines`, `/admin/agents`, `/admin/cache`, `/admin/audit` | admin | [Admin reads](#admin-reads). |
| `POST /hooks/github/<job>` | a signature | A GitHub delivery for a job ([webhook](jobs.md#webhook)). |
| `POST /hooks/github-app/<app>` | a signature | A GitHub App's delivery ([GitHub App](organization-folders.md#github-app)). |
| `/agent/...` | an agent's token | Between the server and its agents; `internal`. |
| `/cache/<scope>/...` | a build's cache token | The keyed cache ([Cache](builds.md#cache)); `internal`. |

Every path in the table is under `/api/v1`. Limits that hold across
routes: a request body is 64 KiB at most (a webhook's 25 MiB), and one
user holds at most 16 streams and long-polls open at once
([Security](accounts.md#security)).

## Admin reads

An admin's token, or an admin's browser session, reads what the admin
pages show; anyone else is answered 403. None of these changes anything,
and none answers a password, a token, a hash or a secret's value.

- `GET /api/v1/admin/users`: every user, each as
  `GET /api/v1/admin/users/<name>` answers one: `name`, `admin`,
  `last_login_at`, `grants` (job to `view`, `build`, `cancel`),
  `gone_grants` (grants on jobs that are no longer loaded), `tokens`
  (`id`, `label`, `created_at`, `last_used_at`, `expires_at`, null for
  never) and `sessions`, the number of live browser sessions.
- `GET /api/v1/admin/secrets`: `set`, every secret held; `missing`, every
  name a loaded job asks for that none holds; and `out_of_scope`, every
  secret held whose scopes do not reach a loaded job that lists it. Each
  has `name`, `kind` (`text`, `file` or `userpass`; empty when missing),
  `user` (a `userpass` secret's user, else empty), `scopes` (`global`,
  `group:<name>` or `job:<pattern>`), `narrow` (true for a global secret
  that exactly one job lists), `updated_at` (null when missing), `jobs`
  that ask for it, and `webhook`, true for a name a job's webhook checks
  its deliveries with.
- `GET /api/v1/admin/server`: `version`, `go_version`, `schema`,
  `started_at`, `listen`, `base_url`, `executors`, `builds`,
  `default_runs_on`, `agent_env`, `agent_url`, `agent_listen`, `shell`,
  `session_ttl_seconds`, `timezone`, `data_dir`, `jobs_dir`,
  `free_disk_bytes` and `db_size_bytes` (null when they cannot be read),
  `last_reload_at`, `last_reload_error` (empty when the last reload was
  taken), `jobs`, how many are loaded, `origin` (where the binary came
  from: Homebrew, Docker image, or release archive or build),
  `update_check` (false with `--no-update-check`), `latest` and
  `notes_url` (the newest release and its page when it is newer than
  `version`, else empty), `upgradable` (that release carries the signed
  checksums an upgrade needs) and `checked_at` (when it was last looked up,
  null before any answer).
- `GET /api/v1/admin/engines`: each engine's `name`, `kind`, `url` (with
  no credentials in it), `labels`, `executors`, `enabled`, `reachable`,
  `since`, `version`, `last_error` and `running`, the stages that run on it as `job#number/stage`.
- `GET /api/v1/admin/agents`: each standing agent's `name`, `labels`,
  `executors`, `busy`, `enabled`, `online`, `since`, `version`, `os`,
  `arch`, the `stages` it runs and `last_seen_at`.
- `GET /api/v1/admin/cache`: the cache entries, paged: `id`, `scope`,
  `namespace`, `key`, `size`, `saved_at`, `saved_by` and
  `last_restored_at`.
- `GET /api/v1/admin/audit`: the audit, newest first, paged: `id`, `at`,
  `actor` (a user's name, `cli` or `kipp`), `action`, `target`, `detail`.
- `GET /api/v1/admin/quiet`: `quiet`, `since`, `running`, `queued` and
  `idle`.

## Upgrade

An admin's token, or an admin's browser session, upgrades the server to the
latest release or returns it to the version before, as the Upgrade card on
the Server page and `kipp upgrade` do ([Upgrading](install.md#upgrading)).
Each start or cancel is a row of the audit (`upgrade`, `upgrade.rollback`,
`upgrade.cancel`). Only one upgrade runs at a time.

| Route | Does |
|---|---|
| `GET /api/v1/admin/upgrade` | Where the upgrade is, the last failure, and what can be done now. |
| `POST /api/v1/admin/upgrade` | Starts an upgrade to the latest release. 202 with the first status; poll `GET` for the rest. |
| `DELETE /api/v1/admin/upgrade` | Cancels it, until its backup begins. 200 with the status. |
| `POST /api/v1/admin/upgrade/rollback` | Starts the return to the version before the last upgrade. 202 with the first status. |

The body of a `POST` is optional; none is a safe restart with the default
wait. Its keys are `safe_restart` (default true: quiet mode, wait until no
build runs or waits, then restart), `confirm_lost` (needed when
`safe_restart` is false: the builds that run end as `lost`), `version`
(must be the latest release; an upgrade only) and `wait_timeout` (a
duration such as `90m`, default `2h`). Any other key is a 400.

The status, from `GET` and every `POST`, has:

- `state`: `idle` before any upgrade, then `preflight`, `downloading`,
  `verifying`, `waiting`, `backup`, `switching`, and the ends `done`,
  `failed` and `cancelled`. It is kept in the data directory, so a server
  that has just restarted answers with it; while the server restarts, the
  route does not answer at all.
- `version` (the release being upgraded or returned to) and `from` (the
  version that ran when it began), `backup` (the name of the backup, once
  there is one), `running` and `queued` (the builds the wait is for) and
  `updated_at`; all empty or zero when idle.
- `error`: why the last run of this process ended without switching.
- `latest` and `upgradable`: the newer release the update check knows and
  whether it can be upgraded to.
- `unavailable`: why an upgrade cannot start now (a dev build, the update
  check off, one already running, nothing newer, a release with no
  signature file); empty when it can.
- `rollback_to`: the version a rollback goes to (`bundled` is the
  executable the server was installed as); empty when there is none, which
  is when there is no earlier version or no backup of an upgrade that came
  up.
- `last_failure`: null, or `version`, `from`, `reason`, `log_tail` (the end
  of what that version printed) and `at` of the last version the launcher
  put back.

A start answers 409 when it cannot happen: an upgrade already running, a dev
build, the update check off, nothing newer, a release with no signature
file, a record from an earlier switch still pending, nothing to roll back
to, or a data directory the server cannot write (the detail then has the
lines that upgrade this kind of install by hand). A body without
`confirm_lost` and with `safe_restart` false is a 400, as is a `version`
that is not the latest.

## Examples

Start a build with a param, and wait for it to end:

```sh
curl -fsS -X POST https://ci.example.com/api/v1/jobs/site-smoke/builds \
  -H "Authorization: Bearer $KIPP_TOKEN" -H 'Content-Type: application/json' \
  -d '{"params": {"TIER": "full"}}'
# {"id": 4712, "job": "site-smoke", "number": 9, "status": "queued", ...}

curl -fsS "https://ci.example.com/api/v1/builds/4712?wait=true" \
  -H "Authorization: Bearer $KIPP_TOKEN"
```

Download the newest successful build's report:

```sh
curl -fsSL -o report.html -H "Authorization: Bearer $KIPP_TOKEN" \
  "https://ci.example.com/api/v1/jobs/site-smoke/artifacts/latest?stage=unit&path=report.html"
```

`kipp run <job> --wait` does the first for you
([Commands](cli.md#kipp-run)).
