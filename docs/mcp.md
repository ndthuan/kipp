# The MCP endpoint

- [What it is](#what-it-is)
- [Turning it on](#turning-it-on)
- [What a caller may do](#what-a-caller-may-do)
- [The tools](#the-tools)
- [Connecting Claude Code](#connecting-claude-code)
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
`--agent-listen`. It speaks streamable HTTP and keeps no session between
requests, so a restart or a revoked token never leaves one behind. A
reverse proxy in front must pass the `Authorization` header and `POST`
through, as it does for the API ([Operations](operations.md)).

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
- No tool touches users, tokens, secrets, engines, agents, caches, the
  quiet mode, reloads or metrics stores. Create the token of a user whose
  grants are just what you want Claude to have.

A build started or cancelled through MCP is recorded as the API records
one: trigger `api`, started by the token's user.

## The tools

Tool results are JSON. A failure is a tool error whose text says why, for
the model to read.

| Tool | Arguments | What it gives |
| --- | --- | --- |
| `overview` | none | What is failing, flaky, recently failed, coming up, how each group stands, and the queue. |
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

## Limits

A request body is capped as the API's is (64 KiB). Every list is capped by
its limit, a log read by the byte and line limits above, and a test
message is cut at 500 bytes. There is no rate limit, as there is none on
the API for a token.
