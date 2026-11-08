# Agents and engines

A stage runs on the server's machine unless it says `runs_on`. Then it
runs on one of two kinds of agent, both of which dial the server, never
the other way round, so that an agent behind a NAT or in a container
takes part:

- **A container on a Docker engine**, for a stage with `runs_on` and
  `image`.
- **A standing agent**, a `kipp agent` process on another machine, for a
  stage with `runs_on` and no `image`.

```yaml
stages:
  unit: {runs_on: [linux, arm64], image: "golang:1.27", run: go test ./...}
  package: {runs_on: macos, run: ./package.sh}
```

- [Engines](#engines)
- [Standing agents](#standing-agents)
- [Managing them](#managing-them)
- [Placement](#placement)
- [What a stage on an agent gets](#what-a-stage-on-an-agent-gets)
- [The server's flags](#the-servers-flags)
- [Security](#security)

## Engines

An admin registers a Docker engine once, with labels and how many
containers it runs at once:

```sh
kipp engine add builder --docker 'ssh://ci@192.0.2.10?socket=/run/user/1000/docker.sock' \
    --labels linux,arm64,docker --max 2 --data <dir>
kipp engine add local --docker unix:///var/run/docker.sock --labels linux,amd64 --max 4 --data <dir>
```

The URL is one of:

- `unix:///path/to/docker.sock`;
- `tcp://host:2376` with `--tls-ca`, `--tls-cert` and `--tls-key`. Plain
  `tcp://` is refused: the Docker API is root on that machine;
- `ssh://user@host[:port]` to the engine's socket there
  (`/var/run/docker.sock` unless `?socket=` says otherwise). The host's
  key must already be in the server user's `known_hosts`; the key is
  `--ssh-key`, else the server's `--ssh-key`, else the user's own under
  `~/.ssh`; and the ssh server must allow stream-local forwarding.

No `docker` CLI is needed on either end. The first stage of a build with
a given `image` on an engine starts a container of it, pulled when the
engine does not have it; the build's later stages of the same image run
in the same container, and it is removed when the build ends. kipp puts
its own static Linux agent into it (for the engine's architecture,
`amd64` or `arm64`), so any Linux image with a shell will do (git too,
for a stage that clones). Nothing of the host is mounted, no port is
published and no privilege is given.

An engine's URL is checked when it is added, not dialled: an engine that
is asleep is one a stage waits for, not a mistake.

## Standing agents

A standing agent runs on any Linux or macOS machine with a POSIX shell.
Register it on the server's data directory, which prints its token once:

```sh
kipp agent add build-1 --labels linux,amd64 --executors 2 --data <dir>
```

and start it on its own machine, with that token in `KIPP_AGENT_TOKEN`:

```sh
kipp agent --url https://ci.example.com --workdir /var/lib/kipp-agent/work
```

- The agent is told its name and labels by the server; `--executors` is
  how many stages it runs at once (default 1).
- `--token -` reads the token from stdin. `--token <token>` works too,
  but is seen by every user in `ps`.
- `--shell` is the shell a stage's command runs with (default `sh`).
- `--workdir` is where the builds' workspaces go.
- It exits 3 when the server refuses its token, and 5 when the server is
  another kipp version: an agent runs the server's own version.

[`examples/systemd/kipp-agent.service`](../examples/systemd/kipp-agent.service)
runs one under systemd, its token in an environment file.

## Managing them

On the data directory, as `kipp secret` does; a running server takes the
change at its next reload:

| Command | Does |
|---|---|
| `kipp engine add <name> --docker <url> --labels <a,b> [--max N] ...` | Registers an engine (`--max` default 1). |
| `kipp engine set <name> [--docker ...] [--labels ...] [--max N] [--disable \| --enable]` | Changes one. A disabled engine takes no new container; what runs finishes. |
| `kipp engine rm <name>` / `kipp engine ls` | Removes one / lists them. |
| `kipp agent add <name> --labels <a,b> [--executors N]` | Registers a standing agent and prints its token once. |
| `kipp agent set <name> [--labels ...] [--executors N] [--disable \| --enable]` | Changes one. `--disable` drains it: it takes nothing new, and what it runs finishes. |
| `kipp agent token <name>` | Gives it a new token and ends the old one. |
| `kipp agent rm <name>` / `kipp agent ls` | Removes one / lists them. |

An admin does the same on the **Agents** page (`/admin/agents`, in the
menu under their name), in [sudo mode](accounts.md#sudo-mode), and a
change made there takes effect at once. The page shows each engine and
agent with its labels, whether it is online or reachable, its version
and what it runs now. `GET /api/v1/admin/engines` and
`GET /api/v1/admin/agents` answer the same for an admin's token.

To upgrade: drain each standing agent, upgrade the server
([Upgrading](install.md#upgrading); an upgrade of the server does not touch
the agents), upgrade and restart the agents, then enable them again.
Containers take the server's own agent at their next start.

## Placement

A stage runs where every label it names is carried: an engine for a
stage with `image`, a standing agent for one without. Among those, the
agent idle longest, or the engine with the most room.

- When nothing registered carries the labels, the stage fails at once
  with `no agent or engine carries labels [...]`.
- When what carries them is busy, offline or cannot be reached (a laptop
  asleep), the stage waits, and its log and the queue say for what, until
  the job's `timeout`.

## What a stage on an agent gets

- Its environment: the agent's own environment, then the server's
  variables `serve --agent-env` names (exact names), then the job's
  variables, params and secrets, then the `KIPP_*` variables, with
  `KIPP_WORKSPACE` its workspace there and `KIPP_AGENT` the agent's name
  (`<engine>/<n>` for a container).
- No files of the server's: one that needs the repo clones it (from
  `KIPP_REPO` and `KIPP_COMMIT`), and artifacts of earlier stages come
  through `artifacts_from`.
- A `file` secret of its job is written by the agent, under a directory
  of its own in the machine's temporary directory (outside the
  workspace), and removed when the stage ends; an agent that starts
  removes what one that died left.
- Its `junit` and metrics files are read on the agent and sent back, and
  its `KIPP_METRICS` is written there. Its artifacts are sent as a tar
  when its command ends ([Artifacts](builds.md#artifacts)).
- Its caches go through `serve --agent-url` ([Cache](builds.md#cache)).

## The server's flags

| Flag | Does |
|---|---|
| `--executors N` | Stages the server runs itself at once (default 4). `0` runs none: a stage that names no `runs_on` goes where `--default-runs-on` says, or fails. Hooks and the clone of a `source` job still run on the server. |
| `--default-runs-on <labels>` | Where a stage with no `runs_on` goes on a server with `--executors 0`. |
| `--builds N` | How many builds with something running run at once (default `--executors`, or 4 when that is 0). |
| `--agent-env <names>` | The server's variables, by exact name, that a stage on an agent is also given. |
| `--agent-url <url>` | The URL a container's agent dials (default `--base-url`), and where a stage on an agent reaches the cache. |
| `--agent-listen <addr>` | A second listener that serves `/api/v1/agent/` and `/api/v1/cache/` and nothing else; `/api/v1/agent/` is then not on `--listen`. |
| `--ssh-key <file>` | The key for `ssh://` engines that name none. |

A build counts toward `--builds` while it prepares, while any of its
stages runs on the server, and from the start of a stage on an agent or
an engine to its end, however many of its stages that is. A build whose
stages all wait to be placed counts for nothing, and neither does one
between its stages, so builds waiting for a busy agent do not keep the
server's other jobs from starting; they still hold their job's place
among the running builds and its locks. When a place is free and no
slot is, the stage waits for the slot (its log says
`waiting for a free build slot`) before it takes the agent's executor or
starts a container, and it gets the first slot to free, before any build
that was queued later.

## Security

- The stage's environment, secrets included, travels on the agent's
  connection: give it TLS, as the web UI has, anywhere but a network of
  your own. `--agent-listen` is for a server whose proxy should not carry
  agents.
- A stage on a standing agent runs as the agent's user, so it can read
  the agent's token (from `/proc`, whatever the stage's environment says)
  and with it claim any stage later placed on that agent, with that
  stage's secrets. Put on one standing agent only jobs that may see each
  other's secrets, and give the token in `KIPP_AGENT_TOKEN` or on stdin.
- A container's token is good for its own build alone. A stage in a
  container runs beside its agent and can write what the agent sends, so
  the server checks every upload from an agent as it would an
  untrusted one.
