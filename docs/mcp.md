# The MCP endpoint

- [What it is](#what-it-is)
- [Turning it on](#turning-it-on)
- [What a caller may do](#what-a-caller-may-do)
- [The tools](#the-tools)
- [Connecting Claude Code](#connecting-claude-code)
- [Connect Claude (claude.ai, the apps, Claude Code)](#connect-claude-claudeai-the-apps-claude-code)
- [The sign-in routes](#the-sign-in-routes)
- [Limits](#limits)

## What it is

kipp can speak the [Model Context Protocol](https://modelcontextprotocol.io)
at `/mcp`, so that Claude (in a chat, on a phone, or Claude Code) can ask
what is red, read a failing log and start or cancel a build. It is the
same data and the same permissions as the [API](api.md), in tools a model
can call.

It is off unless you turn it on. Whoever deploys kipp chooses whether a
server gets one more remote surface.

## Turning it on

```
kipp serve --data /var/lib/kipp --jobs /etc/kipp --mcp
```

Without `--mcp`, `/mcp` is a path nothing serves, like any other, and no
MCP code runs. With it, `/mcp` is on `--listen` and never on
`--runner-listen`. It speaks streamable HTTP and keeps no session between
requests, so a restart or a revoked token never leaves one behind. A
reverse proxy in front must pass the `Authorization` header and `POST`
through, as it does for the API ([Operations](operations.md)).

Behind a reverse proxy or tunnel on the same machine, kipp sees every
request arrive on a loopback address. As a guard against DNS rebinding,
such a request must carry a `Host` of `localhost`, a loopback address or
the host of `--base-url`; any other `Host` is answered `403 Forbidden:
invalid Host header`. Give `--base-url` the public address, and the proxy
passes `Host` on as it is (or sets it to that host). A connection that is
not on loopback is not held to this, and the token is checked either way.

## What a caller may do

A caller authenticates with `Authorization: Bearer kipp_...`, an
[API token](accounts.md#tokens), checked by the same code as the API's
(its last use is recorded the same way). Sessions and cookies are not read
at all. A missing, unknown, revoked or expired token is `401` with
`WWW-Authenticate: Bearer`.

A token acts as its user, so what the tools show and do is what that
user's [grants](accounts.md#users-and-grants) allow:

- An admin's token sees every job.
- The token of a user with grants on two jobs sees those two, in every
  list, in the overview and in the queue.
- A job the user may not view is "not found" to every tool, as in the API.
- `run_build` and `build_again` need the `build` grant, `cancel_build` the
  `cancel` grant.
- No tool touches users, tokens, secrets, engines, runners, caches, the
  quiet mode, reloads or metrics stores. Create the token of a user whose
  grants are just what you want Claude to have.

A build started or cancelled through MCP is recorded as the API records
one: trigger `api`, started by the token's user.

## The tools

Tool results are JSON. A failure is a tool error whose text says why, for
the model to read.

| Tool | Arguments | What it gives |
| --- | --- | --- |
| `overview` | `window` (optional: `24h`, the default, or `7d`) | What is failing, unstable, flaky, how each group stands, a summary of the graphs (what ended, the peak of builds running, the longest wait, each group's pass rate), and the queue. |
| `list_jobs` | `group` (optional) | Each job: group, description, disabled, schedule and next run, last build. |
| `get_job` | `job` | Parameters (name, default, choices) to pass to `run_build`, stages, schedule, concurrency, recent builds, and for a multibranch job its branches and pull requests. |
| `list_builds` | `job`, `limit` (10, at most 50), `cursor` | Builds newest first, with a `next_cursor` for older ones. |
| `get_build` | `job`, `number` | Status, cause, parameters, times, each stage's status and duration, and a link when `--base-url` is given. |
| `read_log` | `job`, `number`, `stage`, `tail_lines` (200, at most 2000) or `offset` and `limit_bytes` (65536, at most 262144) | The tail of a stage's log, or a byte range, with the log's total size and whether the text was cut. Logs are as kipp stores them, secrets masked. |
| `test_results` | `job`, `number`, `limit` (50, at most 200) | Counts, then failures first, then skipped, then passed. |
| `list_artifacts` | `job`, `number` | The files each stage kept, with sizes. Not their content. |
| `queue` | none | What runs and what waits, and why. |
| `run_build` | `job`, `params` | Starts a build; returns its number and link. An unknown or bad parameter is refused, as by the API. |
| `build_again` | `job`, `number` | Starts a build like an earlier one. |
| `cancel_build` | `job`, `number` | Stops a build that has not ended. |

The reading tools are marked read-only. `run_build` and `build_again` are
marked not destructive and not idempotent, `cancel_build` destructive.

## Connecting Claude Code

Make a token for a user with the grants you want Claude to have
(`kipp token create <user>`, or the Account page), then:

```
claude mcp add --transport http kipp https://ci.example.com/mcp \
  --header "Authorization: Bearer $KIPP_TOKEN"
```

Any other MCP client that can send a header on a streamable HTTP
connection works the same way.

## Connect Claude (claude.ai, the apps, Claude Code)

A Claude custom connector signs in by OAuth, so that nobody pastes a token
into it. kipp is its own authorization server, and it is off unless asked
for:

```
kipp serve --data /var/lib/kipp --jobs /etc/kipp --mcp --mcp-oauth \
  --base-url https://ci.example.com
```

- `--mcp-oauth` needs `--mcp`, and `--base-url`, which is the address
  clients sign in at and the issuer of the tokens. It must be an `https://`
  origin with no path. `http://localhost` and `http://127.0.0.1` are
  accepted for trying it on one machine.
- claude.ai calls kipp from Anthropic's servers, even when you use it from
  your own browser or phone, so the base URL must be reachable from the
  internet over https. Claude Code runs on your machine and needs only a
  route from it.
- Without `--mcp-oauth` none of the routes below is served, they answer as
  any path nobody serves, and `/mcp` takes API tokens only, as before.

**Adding it to claude.ai.** In Settings, Connectors, Add custom connector,
give the URL `https://ci.example.com/mcp` and choose to sign in. Either
OAuth client option works: Claude's published identity (kipp reads the
client's details from the metadata document at its `client_id` URL) or
registering automatically (kipp's `/oauth/register`). Claude opens kipp's
login page, then the consent page.

**Claude Code.** `claude mcp add --transport http kipp https://ci.example.com/mcp`
and then `/mcp` in Claude Code to sign in; it follows the `401` that points
to the resource metadata.

**Consent.** The consent page names the app and the host it sends you
back to, and who you are. Two scopes:

| Scope | What it allows |
| --- | --- |
| `kipp:read` | Always granted: the reading tools. |
| `kipp:build` | `run_build`, `build_again` and `cancel_build`. A box that is on when the app asked for it; untick it for a connector that can only look. |

The scopes only narrow: the app can do what its user's
[grants](accounts.md#users-and-grants) allow and, beneath that, what the
scopes allow. A call that needs `kipp:build` the token lacks is `403` with
`WWW-Authenticate: Bearer error="insufficient_scope", scope="kipp:build"`.
A grant the user loses or a job they may no longer see takes effect on the
next call.

**Passwords.** Approving gives access for up to 90 days, so it needs the
password as making an API token does ([sudo mode](accounts.md#sudo-mode)).
You type it once if you reach the consent page through the login (the
login counts as having typed it), and once more if you were logged in
already and the 10 minutes have run out. Denying needs none.

**How long it lasts.** An access token lasts 1 hour. A refresh token lasts
30 days from its last use and each use gives a new one; a refresh token
used twice ends the whole grant, since somebody else holds a copy. No grant
lasts beyond 90 days from the consent: after that the app asks again. Only
hashes of tokens are stored. Tokens made this way start with `kippo_` and
`kippr_`, and are refused everywhere but `/mcp`: the REST API and the pages
take API tokens and sessions only.

**Connected apps.** The Account page lists the apps you have let in, with
what each may do, when it was connected and last used, and a Disconnect
button. An admin sees and disconnects a user's apps on the user's page.
A new password (yours, an admin's reset, or `kipp user passwd`) and the
deletion of the user end their apps, as they end sessions. The audit
records an app's registration, an approval, a disconnection and by whom,
and a refresh token used twice.

## The sign-in routes

These are on the main listener beside `/mcp` and are not under `/api/v1`,
so the OpenAPI description does not list them. Errors are JSON in RFC 6749's
form, `{"error": "...", "error_description": "..."}`, and every answer
carries `Cache-Control: no-store`.

| Route | What it does |
| --- | --- |
| `GET /.well-known/oauth-protected-resource` and `.../oauth-protected-resource/mcp` | RFC 9728: `resource` (`<base-url>/mcp`), `authorization_servers`, `scopes_supported`, `bearer_methods_supported` `["header"]`. |
| `GET /.well-known/oauth-authorization-server` | RFC 8414: the endpoints below, `code_challenge_methods_supported` `["S256"]`, `token_endpoint_auth_methods_supported` `["none"]`, `client_id_metadata_document_supported`, `authorization_response_iss_parameter_supported`. OpenID Connect discovery is not served. |
| `GET /oauth/authorize` | The consent page, for a logged-in user (anyone else is sent to the login and back). `client_id`, `redirect_uri`, `response_type=code`, `code_challenge` with `code_challenge_method=S256`, `scope`, `state`, and `resource` when given, which must be `<base-url>/mcp`. A client or redirect address that is not the app's is a `400` page; any other fault goes back to the app as `error` (`invalid_request`, `invalid_scope`, `invalid_target`, `unsupported_response_type`, `access_denied`), with `state` and `iss`. |
| `POST /oauth/authorize` | The form of the consent page, with its CSRF token: `decision` `approve` or `deny`, and `build` for the `kipp:build` box. Answers `303` to the app with `code`, `state` and `iss`. |
| `POST /oauth/token` | Form-encoded. `grant_type=authorization_code` with `code`, `code_verifier`, `client_id`, `redirect_uri` and `resource` when given; or `grant_type=refresh_token` with `refresh_token` and `client_id`. Answers `access_token`, `token_type` `Bearer`, `expires_in`, `refresh_token` and `scope`. A code works once, within 60 seconds. `400` with `invalid_grant` for a code or refresh token that is unknown, used, expired or another client's, and for a wrong verifier; `invalid_target` for another resource. |
| `POST /oauth/register` | RFC 7591, JSON. `redirect_uris` (required, at most 10) and `client_name`; answers `201` with a random `client_id` and `token_endpoint_auth_method` `none`. There are no client secrets. Slowed per client address like wrong passwords (`429` with `Retry-After`); a registration unused for 30 days is removed. |
| `POST /oauth/revoke` | RFC 7009: `token`, and `client_id` if the app sends it. Ends the grant the token belongs to, whichever kind it is; `200` even for a token nobody holds. |

**Clients.** All are public: PKCE with S256 is required and plain is
refused. A `client_id` that is an `https://` URL with a path is fetched
(GET, 5 seconds, 64 KiB, no redirect to anything but https, cached by its
`Cache-Control` for between 1 hour and 24 hours) and must hold the same
`client_id`, a `redirect_uris` list and a `client_name`. kipp refuses to
connect to a loopback, private or link-local address (checked on the
address it resolves to, so a name that later points inside is refused
too), so a client document must be on a public host. Any other
`client_id` is one `/oauth/register` returned.

**Redirect addresses** are matched exactly, string for string. The one
exception is an `http://` address on `localhost`, `127.0.0.1` or `[::1]`,
which takes any port (RFC 8252). A registration may name `https://`
addresses and those loopback ones; a private-use scheme such as
`app://callback` is refused.

## Limits

A request body is capped as the API's is (64 KiB). Every list is capped by
its limit, a log read by the byte and line limits above, and a test
message is cut at 500 bytes. There is no rate limit, as there is none on
the API for a token.
