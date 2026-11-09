# Accounts and security

- [Users and grants](#users-and-grants)
- [The Account page](#the-account-page)
- [Admin pages](#admin-pages)
- [Tokens](#tokens)
- [Passwords](#passwords)
- [Secrets](#secrets)
- [Masking](#masking)
- [Sudo mode](#sudo-mode)
- [The audit](#the-audit)
- [Security](#security)

## Users and grants

kipp has its own users, each with a name and a password. There is no
default user: the first admin is made on the server's machine with
`kipp user add <name> --admin --data <dir>`
([The first admin](install.md#the-first-admin)).

An admin sees and does everything. Any other user sees and does only
what their grants allow, job by job:

| Grant | Allows |
|---|---|
| `view` | Seeing the job, its builds, logs, artifacts, test results and metrics. A job one may not view does not exist for them: it is answered 404. |
| `build` | Starting a build, Build again, Keep, and Scan now. |
| `cancel` | Cancelling a build. |

`build` and `cancel` need `view` on the same job, since a job one may not
view answers 404: a set with either and no `view` is refused (the user's
page names the row, `kipp user grant` exits 2), and the user's page ticks
View by itself when Build or Cancel is ticked. A grant stored without
`view` by an earlier version shows View ticked, and saving the page stores
it. When the session needs the password again to save the grants, the
ticked form goes through that step and is saved once the password is right.

A grant on a multibranch job is a grant on each of its children, and a
grant on an owner job is a grant on every repo and child of it. An admin
sets grants on a user's page, every loaded job in one form, or with
`kipp user grant <name> <job> view,build,cancel`.

A user's name is that of a job: 1 to 64 lower-case letters, digits, `.`,
`-` and `_`.

## The Account page

Everyone who logs in has an **Account** page, under their name at the
top right:

- change the password: every other session of theirs ends, and the
  browser it was changed in gets a new one;
- make and revoke their own API tokens, each lasting 30, 90 or 365 days
  or for good;
- end their other sessions;
- with `--mcp-oauth`, see the apps they let in, such as a Claude custom
  connector, and disconnect them ([Connect Claude](mcp.md#connect-claude-claudeai-the-apps-claude-code)).

The theme picked at the top right of every page (light, dark, or the
system's) is kept in a cookie, `kipp_theme`, which is only a look.

## Admin pages

An admin has more pages in the menu under their name:

- **Users**: add a user (a password is made and shown once; the user
  changes it on their Account page), make one an admin or take it away,
  reset a password (with a box, off by default, to revoke the user's
  tokens too), set what the user may view, build and cancel on every
  loaded job in one form, remove a grant on a job no longer in the job
  files, make and revoke a user's tokens (for a service account), and
  delete a user. The last admin cannot be taken away or deleted, and
  nobody deletes themselves there.
- **Secrets**: every secret by name, its kind, its scopes, when it was
  set and which jobs name it; the names jobs ask for that no secret holds,
  webhook secrets included; the secrets a job lists that its scopes keep
  from it. Set, replace, rescope without the value, and delete. The Set a
  secret form shows only the fields of the kind chosen (a text value, a
  file upload, or a user and password). Scopes are picked from a list
  (`global`, `group:<name>` for each group, `job:<name>` for each job
  file), never typed, and shown as tags, each with an x that removes it;
  a `job:<pattern>` scope is set with `kipp secret`. A value goes in and
  never comes out; a user and password secret shows its user, never its
  password.
- **GitHub Apps**: every App the owner jobs sign in as, and every App
  made here, with what is wrong with each; and the form that makes a
  GitHub App for an [organization folder](organization-folders.md#github-app).
- **Agents**: the Docker engines and standing agents
  ([Agents and engines](agents.md#managing-them)).
- **Cache**: the keyed caches ([Cache](builds.md#cache)).
- **Server**: the version, the flags `serve` runs with, the data
  directory's free disk and database size, the last reload of the job
  files with a button to reload them, each webhook's payload URL and
  whether its secret is set, the quiet switch
  ([Restarting without losing builds](operations.md#restarting-without-losing-builds)),
  and the audit of every change, newest first.

![A user's page: what they may view, build and cancel on each job, set in one form](screenshots/user.png)

![The Secrets page: a secret with its kind, scopes, when it was set and the job that names it, a note that only one job names a global secret, and its scopes editor open with each scope as a tag with an x and an Add a scope list](screenshots/secrets.png)

The forms open from a button in the heading's row, and come back open
when they were refused or prefilled. A token, a password or a value shown
once is shown on the answer to the form that made it and nowhere else,
with a Copy button. Such a form makes its token or password once: sent
again, by a reload or the back button, it is answered 409 and nothing
more is made. Every change that cannot be undone (revoking a token,
resetting a password, taking admin away, deleting a user or a secret)
asks on a page of its own first.

The same changes can be made with `kipp user`, `kipp token` and
`kipp secret` on the server's machine ([Commands](cli.md)).

## Tokens

An API token acts as its user with the API and the `kipp` command:
`Authorization: Bearer kipp_...`. It cannot make a token, a user or a
secret, or change any other login or secret, admin or not.

- A user makes their own on the Account page; an admin makes one for any
  user on the user's page, or with
  `kipp token create <user> --label <text> [--expires 30|90|365] --data <dir>`.
  Without `--expires` it works until it is revoked.
- The token is printed once; kipp keeps only its hash.
- `kipp token list <user>` says when each was made, last used, and when
  it expires; `kipp token revoke <id>` ends one.
- The `kipp` command reads it from `--token`, `$KIPP_TOKEN`, or the file
  `$XDG_CONFIG_HOME/kipp/token` (mode 600). An argument shows in the
  process list, so prefer either of the others.

## Passwords

- A password a person chooses is 15 to 256 characters, with no rule on
  which kinds, as NIST SP 800-63B rev. 4 asks of a password that is the
  one factor. It may not be one of about 48,000 common and leaked
  passwords that long, kept in the binary (no network call), nor contain
  the user's name or `kipp`, in any case.
- A generated password is 43 random characters. `kipp user add` and
  `kipp user passwd` generate and print one unless `--password-stdin`
  reads it from the first line of stdin.
- A password is stored as an argon2id hash of 19 MiB, 2 passes and 1
  lane, the minimum the OWASP Password Storage Cheat Sheet names.
- `kipp user passwd <name>` ends the user's browser sessions, which
  somebody who had the old password may hold; `--revoke-tokens` revokes
  their tokens too. A password given on stdin that is the user's password
  already changes nothing and prints `<name>: password unchanged`, so a
  deploy script may set the same password on every run without logging
  anybody out.
- A new password, whoever sets it, also ends the user's connected apps
  (the OAuth grants of [the MCP endpoint](mcp.md)); deleting the user
  deletes them.

## Secrets

A secret is a named value that the stages of the jobs that list it in
`secrets:` are given ([Secrets in a stage](jobs.md#secrets-in-a-stage)).
It is sealed in the database with the data directory's `secret.key`, and
kipp also reads some for itself: a webhook's signing secret, a
multibranch or owner job's GitHub `token`, a notifier's token in a hook.

```sh
printf %s "$VALUE" | kipp secret set CHAT_TOKEN --data <dir>              # text, from stdin
kipp secret set DEPLOY_KEY --file ./id_ed25519 --data <dir>                # file
printf %s "$PASSWORD" | kipp secret set LOGIN --user deployer --data <dir> # user and password
kipp secret scope DEPLOY_KEY --scope group:site --scope 'job:site-*' --data <dir>
kipp secret list --data <dir>
kipp secret unset CHAT_TOKEN --data <dir>
```

A value, and a password, are read from stdin, never from an argument;
`--file -` reads the file from stdin, and `--file` with `--user` is
refused. Setting a name again replaces its value, and is how its kind
changes. `kipp secret list` prints each name with its kind and scopes.

The kinds:

| Kind | Value | A stage gets |
|---|---|---|
| `text` (default) | One line, 8 bytes to 64 KiB. | `NAME`, the value. |
| `file` | Any bytes, 1 byte to 1 MiB. | `NAME`, the path of a copy only its user reads. |
| `userpass` | A user (one line, no colon, at most 256 bytes) and a password (as `text`). | `NAME_USR`, `NAME_PSW`, and `NAME` as `user:password`. |

### Scopes

A secret has scopes, which say which jobs may list it, as Jenkins'
credentials have:

- `global`: every job. A new secret is `global` unless told otherwise.
- `group:<name>`: every job whose `group` is that name.
- `job:<pattern>`: a `path.Match` pattern against the job's name. A
  multibranch child and a repo of an owner job also match by the name of
  the job file they come from, so a scope on a parent or an owner job
  reaches what it builds.

A job that lists a secret whose scopes do not reach it fails before its
first stage, with `kipp: secret NAME is not in scope for job JOB` in its
`prepare` log. Scopes are checked when a build starts, not when the job
files are read, so `kipp check` and a reload do not look at them; the
Secrets page lists the secrets a loaded job names and cannot have, and
marks a global secret that only one job names. Scopes decide what a stage
receives: a secret kipp reads for itself (a `token:`, a webhook secret)
is used by the server whatever its scopes.

A secret is sealed under its own name, so a sealed value copied into
another secret's row does not open.

## Masking

Each secret value a build has is masked as `***` in every log where it
appears as it is, byte for byte. Only that exact value is: a stage that
prints it encoded (base64, URL-escaped), changed or a piece at a time
prints what it printed, so a stage that handles a secret should not print
it at all.

- A `file` secret that is valid UTF-8 is masked line by line: each line
  of 8 or more characters, once surrounding whitespace is trimmed, is
  masked as it appears, so a key printed whole shows `***` for each of its
  lines. A file that is not UTF-8 (a keystore) is not masked at all; do
  not print one.
- A `userpass` secret's password and `NAME` are masked; its user is not.
- `env:`, `_vars.yml`, params and `KIPP_ENV` values are plain text: shown
  on the job's page to whoever may view it, and not masked.

## Sudo mode

A change to logins, tokens, secrets, agents, engines, GitHub Apps or the
cache in the browser needs the user's password given in the last 10
minutes, as GitHub asks for it: a session cookie someone took is not
enough to make a token, add a user or set a secret. The page asks for the
password, says what was being done, and leads back to the page the change
came from, where it is sent again; a wrong password there counts as a
wrong login does. Changing one's own password needs the current one
instead, and ending one's other sessions or reloading the job files needs
neither. An API token never makes these changes. Approving an app on the
consent page ([Connect Claude](mcp.md#connect-claude-claudeai-the-apps-claude-code))
asks for it too, and a login that leads straight to that page counts as
having given it.

## The audit

Every change to a user, a grant, a token, a secret, an engine, an agent,
a cache entry or metrics, in the browser or with the `kipp` command, and
every reload, quiet switch and upgrade, is a row of the audit: who (a user's name,
`cli` for the command line, `kipp` for the server's own removals), what,
on whom, and when. No row holds a password, a token or a secret's value.
The audit is on the Server page and at `GET /api/v1/admin/audit`; the
newest 10,000 rows are kept.

## Security

### What a job can do

- A stage is a shell command run as the server's own user, with no
  isolation beyond a process group: whoever may write a job file, or push
  to a repo a job's `source`, multibranch or owner job names, runs
  commands as that user, and so does an admin, who sets the secrets and
  the grants.
- That user can read the data directory, the secrets' key and a GitHub
  App's key file. Run untrusted stages on an agent, in a container, or as
  another user ([Agents and engines](agents.md)).
- A stage is not given the server's whole environment, which may hold
  what the server was started with for its own sake (a cloud key, a
  token): only what `serve --pass-env` names, comma-separated, a name or a
  prefix with `*` after it (`AWS_*`). The default is
  `PATH,HOME,USER,LOGNAME,SHELL,LANG,LC_*,TZ,TMPDIR`; `--pass-env '*'`
  gives all of it. A value a stage needs is better in the job's `env` or a
  secret.
- A pull request from a fork is never built.

### The web UI and logins

- The server listens on `127.0.0.1:8085` in plain HTTP. For anything but
  that machine, put a reverse proxy with TLS in front of it
  ([Behind a proxy](operations.md#behind-a-proxy)). The proxy must pass
  the `Host` header through unchanged, or the login form is refused as
  cross-origin.
- The session cookie is `__Host-kipp_session`, marked `Secure`,
  `HttpOnly` and `SameSite=Lax`: a browser keeps it only over HTTPS (some
  also over `http://localhost`). A session lasts `serve --session-ttl`
  (default 30 days). The one exception is a browser on the server's own
  machine at `http://127.0.0.1:<port>` or `http://localhost:<port>`, with
  no proxy between, on a server that listens on a loopback address: its
  login gets a cookie of another name, `kipp_local_session`, that is not
  `Secure`, and that cookie is read on such a request only. A request
  that carries `Forwarded`, `Via` or an `X-Forwarded-` header, or a
  `Host` that is not this machine's, never gets it. A server that listens
  on any other address (`--listen 0.0.0.0:8085`) never sets or reads that
  cookie, since a proxy on the same machine reaches it over the loopback
  too.
- Every page carries `X-Robots-Tag: noindex, nofollow`.
- Two passwords are checked at a time, and a third place is kept for a
  login from a browser that has logged in as that name before or from the
  server's own machine, so strangers who fill the two cannot shut those
  out. A login that finds no place for 2 seconds is answered 503 and
  asked to try again.
- Wrong passwords are counted by name, in upper or lower case alike,
  whether the name exists or not. The first 5 in a row cost nothing;
  after them the name's password is not checked at all for 2 seconds,
  then 4, 8, and so on up to 15 minutes, and a login in that time is
  answered 429 with `Retry-After`. An attempt is counted before its
  password is checked, so logins sent at once are held to the same 5. A
  right password ends the run. The count is in memory: a restart clears
  it, and a count with no wrong password for a day is forgotten. It holds
  10,000 names and addresses; a flood of made-up names makes room only by
  dropping a count within its free 5 with no wait running, and with none
  such, a name not counted yet is answered 429 for 2 seconds.
- So that a count anybody can run up does not keep the name's owner out,
  a successful login also sets `__Host-kipp_device` (`Secure`, kept a
  year; `kipp_local_device` on the server's own machine): a random id
  signed with the data directory's `device.key` for that name. A browser
  that sends one made for the name tried is counted on its own. One made
  for another name, not signed by this server, or made before the name's
  password last changed counts as none. A login from the server's own
  machine has a count of its own, and the command line there is the way
  back in anyway.
- Every wrong password is logged at WARN with the name tried and the
  address it came from; a name no user has is logged as
  `unknown_name=true` with its length alone, since it may be a password
  typed in the wrong box. Behind a proxy that address is the proxy's:
  `serve --client-ip-header X-Forwarded-For` (or `CF-Connecting-IP`, or
  what the proxy sets) has wrong passwords counted by client address too,
  across every name it tries, and either count running out refuses the
  login. The header is believed only from a peer in `--trusted-proxy`
  (loopback by default); in a list the rightmost address is taken. An
  IPv6 client is counted by its /64.
- One user holds at most 16 streams open at once (log streams, the
  queue's events and `?wait=true` long-polls together); one more is
  answered 429. An open stream checks every 15 seconds that its session
  or token is still good and its user may still view what it shows, and
  ends when not.

### Webhooks

The webhook endpoints answer anyone who posts, so what an unsigned
request may cost is bounded:

- one without an `X-Hub-Signature-256` header is refused before its body
  is read;
- two bodies are read at once at most, and a third delivery is answered
  503 with `Retry-After`;
- a body must arrive within 30 seconds (408) and be 25 MiB at most
  (GitHub's own cap, 413);
- every job name, one with a webhook or not or none at all, is answered
  after the same work, so neither the answer nor its time tells which
  names have a webhook.

The signature covers the body alone, so the same body under a new
delivery id is a copy, and is answered 409.

### Credentials in a repo URL

A user and password in a `source.repo` or `clone` URL
(`https://user:token@host/...`) are left out of the URL wherever kipp
shows it, and masked in what git prints to the clone log. A git
credential helper or a deploy key for the server's user keeps them out of
the job file altogether.

Reporting a vulnerability: [SECURITY.md](../SECURITY.md).
