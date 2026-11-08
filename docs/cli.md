# Commands

`kipp <command> -h` prints a command's flags. A flag is written `-name`
or `--name`.

| Command | Does | Talks to |
|---|---|---|
| [`kipp serve`](#kipp-serve) | Runs the server: web, API, scheduler and runner. | |
| [`kipp check`](#kipp-check) | Checks a jobs directory. | nothing |
| [`kipp reload`](#kipp-reload) | Has a running server read its jobs directory again. | the server |
| [`kipp quiet`](#kipp-quiet) | Stops a running server taking builds, before a restart. | the server |
| [`kipp upgrade`](#kipp-upgrade) | Upgrades a running server to the latest release, or back to the version before. | the server |
| [`kipp run`](#kipp-run) | Starts a build, and optionally waits for it. | the server |
| [`kipp user`](#kipp-user) | Adds, changes and lists users and their grants. | the data directory |
| [`kipp token`](#kipp-token) | Makes, revokes and lists API tokens. | the data directory |
| [`kipp secret`](#kipp-secret) | Sets, scopes, removes and lists secrets. | the data directory |
| [`kipp engine`](#kipp-engine) | Registers the Docker engines containers run on. | the data directory |
| [`kipp agent`](#kipp-agent) | Runs a standing agent, or registers them. | the server, or the data directory |
| [`kipp metrics`](#kipp-metrics) | Checks a stage's guards; reads and changes what metric stores remember. | the stage, or the data directory |
| [`kipp cache`](#kipp-cache) | A stage's keyed cache; lists and removes entries. | the server, or the data directory |
| `kipp version` | Prints the version, and on a second line where the binary was installed from. | nothing |

**Talking to a server.** `reload`, `quiet`, `upgrade` and `run` take `--url` (else
`$KIPP_URL`, else `http://127.0.0.1:8085`) and `--token` (else
`$KIPP_TOKEN`, else the file `$XDG_CONFIG_HOME/kipp/token`, mode 600). An
argument shows in the process list, so prefer either of the others.

**Working on the data directory.** `user`, `token`, `secret`, `engine`,
`agent add|set|token|rm|ls`, `metrics put|forget|ls` and `cache ls|rm`
open the database in `--data` themselves, on the server's machine, as the
user the server runs as, while it runs or not. Only `kipp serve` makes the
data directory; these refuse one that does not exist, and one whose
schema is not theirs ([Upgrading](operations.md#upgrading)). A running
server takes a change to engines and agents at its next reload; users,
tokens and secrets at once. Every change they make is a row of the audit
with the actor `cli`.

**Exit codes.** Unless a command says otherwise: 0 is success, 1 a
failure, 2 a command line it would not take, 3 a token the server
refused.

## kipp serve

```
kipp serve --data <dir> --jobs <dir> [flags]
```

Stays in the foreground and logs to stderr. `SIGHUP` re-reads the jobs
directory, as `kipp reload` does. `SIGINT` and `SIGTERM` stop the server;
a build running then ends as `lost`. A second one kills a server that is
still stopping, and leaves what its stages started running; so does a
crash. The next `serve` on the data directory kills what it finds of
those stages before it runs anything. One server at a time uses a data
directory.

| Flag | Default | Does |
|---|---|---|
| `--data <dir>` | required | The data directory: the database, the secrets' key, logs and workspaces. Made, mode 700, if it is not there ([The data directory](operations.md#the-data-directory)). |
| `--jobs <dir>` | required | The directory of job files. |
| `--listen <addr>` | `127.0.0.1:8085` | The address to listen on, in plain HTTP. |
| `--base-url <url>` | `http://<listen>` | The URL stages see as `KIPP_URL`. Build links, commit statuses' links, webhook URLs and GitHub App manifests need it given. |
| `--executors <n>` | 4 | Stages the server runs itself at once; 0 runs none ([Agents and engines](agents.md#the-servers-flags)). |
| `--flaky-flips <n>` | 4 | Changes of colour among a job's last 20 finished builds (at least 5 needed) that put it in the front page's Flaky section; 0 hides the section. |
| `--builds <n>` | `--executors`, or 4 when that is 0 | Builds with something running at once. |
| `--shell <sh>` | `sh` | The shell that runs stages, as `<shell> -c <command>`. |
| `--session-ttl <d>` | `720h` | How long a browser login lasts. |
| `--timezone <zone>` | the host's | The IANA zone a cron is read in when its job names none, and the pages' zone. |
| `--pass-env <names>` | `PATH,HOME,USER,LOGNAME,SHELL,LANG,LC_*,TZ,TMPDIR` | The server's environment variables a stage is given: names, `NAME_*` for a prefix, `*` for all. |
| `--client-ip-header <name>` | none | The header a proxy puts the client's address in, like `X-Forwarded-For`; failed logins are then counted by client too. |
| `--trusted-proxy <cidrs>` | `127.0.0.0/8,::1/128` | The proxies whose `--client-ip-header` is believed. |
| `--github-api <url>` | `https://api.github.com` | GitHub's API, for scans, commit statuses and webhooks (GitHub Enterprise has its own). |
| `--default-runs-on <labels>` | none | Where a stage with no `runs_on` goes on a server with `--executors 0`. |
| `--agent-env <names>` | none | The server's variables, by name, a stage on an agent is also given. |
| `--agent-url <url>` | `--base-url` | The URL a container's agent dials, and stages on agents reach the cache at. |
| `--agent-listen <addr>` | none | A second listener for `/api/v1/agent/` and `/api/v1/cache/` alone. |
| `--ssh-key <file>` | the user's own under `~/.ssh` | The private key for `ssh://` engines that name none. |
| `--artifacts-max <size>` | `50GB` | The most every build's artifacts come to; past it the oldest builds' are removed. |
| `--cache-max <size>` | `20GB` | The most every cache entry comes to; past it the least recently restored are removed. |
| `--mcp` | off | Serve an MCP endpoint at `/mcp` for Claude and other MCP clients, authenticated by API tokens and limited to their users' grants ([The MCP endpoint](mcp.md)). |
| `--mcp-oauth` | off | Let MCP clients such as a Claude custom connector sign in by OAuth, served by kipp itself. Needs `--mcp` and `--base-url` as an `https://` origin (`http://localhost` for trying it) ([Connect Claude](mcp.md#connect-claude-claudeai-the-apps-claude-code)). |
| `--no-update-check` | off (also `KIPP_NO_UPDATE_CHECK=1` or `true`) | Do not ask GitHub for the latest release ([The update check](install.md#the-update-check)). Without it the server knows of no release to upgrade to. |

Once it listens, it prints one line on stdout:

```
kipp v1.0.0 listening on http://127.0.0.1:8085, 3 jobs from /etc/kipp, data in /var/lib/kipp
```

A release build first looks in `<data>/bin/`: it runs the binary an upgrade
left there when that is a higher release than the executable's own, and
carries out an upgrade or a rollback the server asked for before it
restarted. `KIPP_USE_BUNDLED_BINARY=1` in the environment keeps the
executable's own and skips both, so `kipp upgrade` is refused. A dev build
ignores `bin/`. See
[Upgrading](operations.md#upgrading) in Operations.

## kipp check

```
kipp check <jobs-dir>
```

Reads a directory of job files and `_vars.yml`, as the server would,
with no server and no network, and prints `ok: <n> jobs` or every error.
Exits 0 when the directory would be taken, 1 when not, 2 on a wrong
command line. Secret scopes and a repo's stages file are not read: they
are checked when a build runs.

## kipp reload

```
kipp reload [--url <url>] [--token <token>]
```

Has a running server read its jobs directory again, as `SIGHUP` does,
with an admin's token. Prints `reloaded`, or the errors of a directory
that was refused (the server keeps the jobs it had) and exits 1.

## kipp quiet

```
kipp quiet on [--wait [--timeout <duration>]] | off | status [--url <url>] [--token <token>]
```

With an admin's token. `on` refuses new builds; with `--wait` it returns
once no build runs or waits, and at `--timeout` (default `2h`) it turns
quiet off again and exits 1. `off` takes builds again; `status` says
whether the server is quiet and how many builds run and wait. See
[Restarting without losing builds](operations.md#restarting-without-losing-builds).

## kipp upgrade

```
kipp upgrade [status] [--check] [--cancel] [--rollback] [--no-safe [--yes]] [--wait-timeout <duration>] [--url <url>] [--token <token>]
```

With an admin's token, upgrades the running server to the latest release,
as the Upgrade card on the Server page does ([Upgrading](install.md#upgrading)).
With no flag it makes the server quiet, waits for the builds to end, backs
up the data directory, restarts into the new version, and prints each step
until the version runs; the launcher puts the old version back if the new
one does not come up. The release is downloaded from the repository's
releases and its signature and checksum are checked first.

| Flag | Does |
|---|---|
| `--check` | Says whether a newer release can be upgraded to, and does nothing. |
| `status` | Says where an upgrade is: `upgrade: idle`, or the step, the two versions and, while it waits, how many builds run and queue. |
| `--cancel` | Cancels an upgrade that has not reached its backup; quiet is turned off again. |
| `--rollback` | Returns to the version before the last upgrade, with the data as the backup of that upgrade had it. Everything written since is lost. |
| `--no-safe` | Does not wait for the builds: it restarts at once, and the builds that run end as `lost`. Needs `--yes`. |
| `--yes` | With `--no-safe`, confirms that the running builds end as lost. |
| `--wait-timeout <d>` | How long the safe restart waits for the builds before it gives up, turns quiet off again and restarts nothing. Default `2h`. |

Exits 0 when the version runs (or for `--check`, `status` and `--cancel`
when they answered), 1 when the upgrade failed or was cancelled, 2 for a
command line it would not take (`--yes` without `--no-safe`, or two of the
modes), 3 for a token the server refused. It refuses with exit 1 in a dev
build, with the update check off, when there is nothing newer or the release
has no signature file, and where the data directory cannot be written; the
last says the lines that upgrade that kind of install by hand. It waits up
to 35 minutes after the restart for the server to answer.

## kipp run

```
kipp run <job> [-p KEY=VALUE]... [--param-file KEY=FILE]... [--wait] [--url <url>] [--token <token>]
```

Starts a build of the job, as the token's user, who needs `build` on it,
and prints `<job> #<n> queued (build <id>)`.

- `-p KEY=VALUE` gives a param, the value as written; repeat it.
- `--param-file KEY=FILE` gives a param whose value is all of a file. A
  `text` param takes the file whole; a `string` param is one line, so it
  refuses a file that ends in a newline.
- `--wait` prints the build's output as it runs, each line after its
  stage's name in brackets, and exits by how the build ended:

| Code | Meaning |
|---|---|
| 0 | The build succeeded, or ended unstable (one line on stderr then names the unstable stages). |
| 1 | The build ended and did not succeed, or its output could not be read. |
| 2 | A command line kipp would not take. |
| 3 | The server refused the token. |
| 4 | The build was queued and its end could not be learned (the server stopped answering, or no longer knows the build); it may still be running, and stderr says where to look. |

## kipp user

```
kipp user add <name> [--admin] [--password-stdin] --data <dir>
kipp user passwd <name> [--password-stdin] [--revoke-tokens] --data <dir>
kipp user admin <name> on|off --data <dir>
kipp user delete <name> --data <dir>
kipp user grant <name> <job> view,build,cancel [--jobs <dir>] --data <dir>
kipp user revoke <name> <job> --data <dir>
kipp user list --data <dir>
```

- `add` makes a user, an admin with `--admin`. It prints a generated
  password once, `<name> password: ...`, unless `--password-stdin` reads
  one from the first line of stdin ([Passwords](accounts.md#passwords)).
- `passwd` sets a new password the same way, and ends the user's browser
  sessions; `--revoke-tokens` revokes their tokens too. A password given
  on stdin that is the user's already changes nothing and prints
  `<name>: password unchanged`.
- `admin <name> on|off` makes a user an admin or takes it away, and
  refuses to take it from the last admin.
- `delete` removes a user with their grants, tokens and sessions, and
  refuses to remove the last admin.
- `grant` sets what a user may do on a job, replacing what they had;
  `revoke` takes it all away. With `--jobs <dir>` a job that is not in
  the job files is refused; without it the name is not checked, and the
  command says so.
- `list` prints each user, admin or not, and their grants.

## kipp token

```
kipp token create <user> --label <text> [--expires 30|90|365] --data <dir>
kipp token revoke <id> --data <dir>
kipp token list <user> --data <dir>
```

`create` prints the token once. `--expires` is the days it works for;
without it, until it is revoked. `list` prints each token's id, label,
when it was made and last used, and when it expires.

## kipp secret

```
kipp secret set <NAME> [--file <path>|-] [--user <user>] [--scope <scope>]... --data <dir>
kipp secret scope <NAME> --scope <scope>... --data <dir>
kipp secret unset <NAME> --data <dir>
kipp secret list --data <dir>
```

- `set` sets a `text` secret from stdin; with `--file` a `file` secret
  from that path (`-` is stdin); with `--user` a `userpass` secret for
  that user, the password from stdin. `--file` and `--user` together are
  refused. Without `--scope` a new secret is `global`, and one that is set
  keeps its scopes.
- `scope` replaces a secret's scopes without its value: `global`,
  `group:<name>` or `job:<pattern>`, repeated.
- `list` prints each name, its kind and its scopes; never a value.

See [Secrets](accounts.md#secrets).

## kipp engine

```
kipp engine add <name> --docker <url> --labels <a,b> [--max N] [--ssh-key <file>] [--tls-ca <file> --tls-cert <file> --tls-key <file>] --data <dir>
kipp engine set <name> [--docker <url>] [--labels <a,b>] [--max N] [--ssh-key <file>] [--tls-...] [--disable | --enable] --data <dir>
kipp engine rm <name> --data <dir>
kipp engine ls --data <dir>
```

`--max` is how many containers it runs at once (default 1). `--disable`
takes no new container; what runs finishes. See
[Engines](agents.md#engines).

## kipp agent

```
kipp agent --url <server> --workdir <dir> [--token <token> | --token -] [--shell sh]
kipp agent add <name> --labels <a,b> [--executors N] --data <dir>
kipp agent set <name> [--labels <a,b>] [--executors N] [--disable | --enable] --data <dir>
kipp agent token <name> --data <dir>
kipp agent rm <name> --data <dir>
kipp agent ls --data <dir>
```

With `--url` it runs a standing agent on this machine: it dials the
server, which says its name and labels, and runs the stages placed on it.
The token is `--token`, `-` to read it from stdin, or
`$KIPP_AGENT_TOKEN`. It exits 3 when the server refuses its token and 5
when the server is another kipp version.

`add` registers an agent and prints its token once; `--executors` is how
many stages it runs at once (default 1). `set --disable` drains it: it
takes nothing new, and what it runs finishes. `token` gives it a new
token and ends the old one. See [Standing agents](agents.md#standing-agents).

## kipp metrics

```
kipp metrics check <glob>
kipp metrics put <store> --file <report.json> [--job <name>] --data <dir>
kipp metrics forget <store> [--metric <name> [--item <item>]] --data <dir>
kipp metrics ls [--store <name>] [--jobs <dir>] --data <dir>
```

`check` runs inside a stage and applies its guards; the others work on
what the stores remember. See [Metrics, Commands](metrics.md#commands)
for each and for `check`'s exit codes.

## kipp cache

```
kipp cache restore --key K [--restore-key PREFIX]... --path P... [--scope S]
kipp cache save    --key K --path P... [--scope S]
kipp cache hash    FILE...
kipp cache ls --data <dir>
kipp cache rm <id>... --data <dir>
kipp cache rm --scope <scope> --data <dir>
```

`restore`, `save` and `hash` run inside a stage, with the cache variables
kipp gives it. `restore` exits 0 on an exact hit, 1 on a miss (a prefix
hit, nothing, or any error) and 2 on a wrong command line; `save` exits 0
when the entry is kept or was there already, and 2 when it was not saved.
`hash` prints a SHA-256 of the files named, in the order given.

`ls` lists the entries the server keeps, the least recently restored
first; `rm` removes the entries with those ids, or every entry of a
scope, each a row of the audit. See [Cache](builds.md#cache).
