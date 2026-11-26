# Security

## Supported versions

Only the newest release gets fixes: a fix ships as a new release, not as a
patch to an older one. Upgrading is replacing the binary or the image
([Upgrading](docs/operations.md#upgrading)).

## Reporting a vulnerability

Report it privately, through GitHub's private vulnerability reporting:
the repository's **Security** tab, then **Report a vulnerability**. Please
do not open a public issue, pull request or discussion about it.

Say in the report:

- the version (`kipp version`) and the operating system;
- how kipp runs: the `serve` flags, and whether a proxy is in front;
- what an attacker needs to start with (no account, a user who may view
  one job, a token, an admin) and what they get;
- the steps to reproduce it, as small as you can make them, and what you
  saw;
- whether it is known elsewhere, and if you plan to publish it, when.

kipp has one maintainer, so the times here are aims, not promises: an
answer within a week saying whether it is taken as a vulnerability, and
for one that is, a fix released as soon as one is ready, with the report
credited in the changelog unless you would rather not be named.

## What is not a vulnerability

kipp runs commands; that is what it is for. These are by design, and the
[Security](docs/accounts.md#security) page says why:

- Whoever may write a job file, or push to a repository a job's `source`
  names, runs commands as the user kipp runs as. So does an admin, who
  can set the secrets and grant builds. A stage is isolated from that
  user's other files by nothing but a process group.
- A user with `build` on a job may start as many builds of it as they
  like; they wait in the queue.
- A secret's value is masked in the logs as it is. A stage that prints
  it encoded, reversed or in pieces is not masked.
- kipp speaks plain HTTP. TLS, and HSTS, are the proxy's in front of it.

A way around any of the limits the docs state (the login throttle,
sudo mode, the stream and webhook caps, what a user who may view one job
sees of others) is a vulnerability, and so is anything that lets
someone who is not one of the above run a command.
