# Problems

Every 4xx and 5xx under `/api/v1` is a problem document (RFC 9457),
`Content-Type: application/problem+json`:

```json
{"type": "https://github.com/ndthuan/kipp/blob/main/api/problems.md#not_found",
 "title": "Not found", "status": 404, "detail": "no such path", "code": "not_found"}
```

`code` is the stable name below, for programs to decide by; `title` is the
code's fixed summary; `detail` is the reason for a person and may change.
A client that knows no code decides by `status`.
The runner protocol's own refusals (`Refusal`)
stay protocol messages; a failure of plain HTTP there is a problem.

## bad_input

Status: 400
Title: The request is not valid

The request could not be read or a value in it is not one the server takes: a body that does not parse, a field outside its range, a name that is not valid. `detail` says which.

## bad_params

Status: 400
Title: The build parameters are refused

A build was asked for with parameters the job does not declare, or with a value it refuses.

## unauthorized

Status: 401
Title: A login or a token is required

The request carries no login and no token. Send a bearer token or sign in.

## signed_out

Status: 401
Title: The session or token is no longer valid

The session or token the request (or a stream it opened) was made with is gone or no longer belongs to the same user.

## forbidden

Status: 403
Title: Not allowed

The caller is known but may not do this: not an admin, or a job without the permission the call needs. A job the caller may not see is `not_found`, never this.

## csrf

Status: 403
Title: The CSRF token is missing or wrong

A session request that changes something without the `X-CSRF-Token` header, or with a wrong one. A request with a bearer token never needs it.

## sudo_required

Status: 403
Title: This change needs the password again

The change needs the caller's password again; send it with the request.

## wrong_password

Status: 403
Title: The password is wrong

The password sent is not the caller's.

## not_found

Status: 404
Title: Not found

No such path, no such thing, or a job the caller may not see: the answer is the same for all three.

## method_not_allowed

Status: 405
Title: Method not allowed

The path exists under other methods; `Allow` names them.

## request_timeout

Status: 408
Title: The request took too long to arrive

The body of a webhook delivery did not arrive within the server's deadline.

## conflict

Status: 409
Title: The request conflicts with the state of the server

The request conflicts with the state of the server in a way no more specific code names, for example a name that is taken or the last admin being demoted.

## job_disabled

Status: 409
Title: The job is disabled

A build of a disabled job was asked for.

## dropped

Status: 409
Title: The job is running or already queued, and drops builds

A build was asked for while the job runs or has one queued, and the job drops those.

## owner_job

Status: 409
Title: An owner job builds nothing itself

A build of an owner job was asked for. It builds nothing itself; build one of its repos.

## no_default_child

Status: 409
Title: The multibranch job has no build of its default branch yet

A build of a multibranch job was asked for before kipp has a child for its default branch. A push to that branch makes it.

## scan_running

Status: 409
Title: A scan of the repo is already running

A scan of the job's repo was asked for while one is running.

## no_scanner

Status: 409
Title: This server does not scan

A scan was asked for on a server that does not scan.

## no_token

Status: 409
Title: The job names no token to scan with

A scan was asked for on a job that names no token to read the repo with.

## not_cancellable

Status: 409
Title: The build can no longer be cancelled

The build has ended, or is past stopping.

## duplicate_delivery

Status: 409
Title: The delivery was received before

A webhook delivery with an id (or a body) the server has seen before. It is not built twice.

## precondition_failed

Status: 412
Title: A condition of the request does not hold

A conditional request (`If-Match` and the like) whose condition does not hold.

## too_large

Status: 413
Title: The body is too large

The body is over the limit: 64 KiB for the JSON API, the webhook's own cap for a delivery, the most an entry may be for the cache.

## range_not_satisfiable

Status: 416
Title: The range is outside the file

The `Range` of an artifact download is outside the file.

## jobs_refused

Status: 422
Title: The job files were refused

A reload read the job files and refused them; `detail` says why, and the jobs stay as they were.

## too_many_streams

Status: 429
Title: Too many streams are open for this caller

More streams (the queue's events, a log being followed) are open for this caller than one may have.

## too_many_attempts

Status: 429
Title: Too many wrong passwords; wait and try again

Too many wrong passwords. `Retry-After` says when to try again.

## internal

Status: 500
Title: Something failed inside the server

Something failed inside the server. The reason is in its log, never in the answer.

## quiet

Status: 503
Title: The server is quiet and takes no new builds

The server is quiet: it takes no new builds while it waits to restart. `Retry-After` says when to ask again.

## busy

Status: 503
Title: The server is busy; send the request again

The server is busy with other deliveries or password checks. `Retry-After` says when to send the request again.
