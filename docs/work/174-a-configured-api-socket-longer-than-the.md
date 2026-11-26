# Work plan: A configured api.socket longer than the Unix socket limit fails with a bare 'invalid argument'

**Issue:** [#174](https://github.com/mayusuk/agent-loop/issues/174) (bug)
**Branch:** `issue/174-a-configured-api-socket-longer-than-the`
**Base:** `origin/main @ 958df78` (checked before branching)
**Status:** Done

## Summary

`api.Listen` and `api.Dial` refuse a socket path that doesn't fit a Unix socket address, with an error naming the path, its length and the limit, instead of the bare "invalid argument" from the kernel. The length check #139 added for `server init` moves into `internal/api` so all three share it.

## Context

#139 D1 set the limit: a path must be shorter than `sun_path` (108 bytes on Linux, 104 on macOS/BSD), leaving room for the NUL. `pickAPISocket` in `cmd/agent-loop/serverinit.go` checked it (#139 D2), but only for the path it writes. Root cause: `api.Listen` passes any path to `net.Listen`, which fails with `EINVAL` ("invalid argument"), and `api.Dial` builds a lazy gRPC client whose first call fails the same way, so `server start` and `board` etc. name neither the path nor the limit.

## Approach

Move `sunPathSize` to `internal/api` and add `api.CheckSocketPath(path) error`, which returns `the API socket path <p> is <n> bytes, too long for a Unix socket (at most <max> here)`. `Listen` and `Dial` call it first and prefix `api: `. `pickAPISocket` calls it and appends its `--config` hint.

## Steps

- [x] Tests: `Listen` and `Dial` with a path of exactly `sun_path` bytes fail naming the path, length and limit; the longest accepted path still listens and dials (fail before the fix).
- [x] Add `CheckSocketPath` to `internal/api`, call it from `Listen`, `Dial` and `pickAPISocket`.

## Acceptance criteria

- `server start` (`api.Listen`) with a too-long `[api] socket` fails with an error naming the path and the limit.
- Client commands (`api.Dial`) with a too-long socket fail the same way.

## Decisions

- **D1** [gap] TECHNICAL-SPEC §11: `api.Listen` and `api.Dial` refuse a socket path of `sun_path` bytes or more (the #139 D1 limit) before touching the file system or the network, with `api: the API socket path <path> is <n> bytes, too long for a Unix socket (at most <limit> here)`. Why: the issue asks for the path and limit; reusing #139's wording keeps `server init` and the server and client messages the same.

## Verification

Full local suite plus `GOOS=linux go build ./...` and `CGO_ENABLED=0 GOOS=darwin go build ./...`.

## Risks and open questions

None.

## Outcome

`CheckSocketPath` now lives in `internal/api`; `Listen` and `Dial` call it first, and `server init` wraps it with its `--config` hint, so the #139 message is unchanged. The cmd tests keep their own `sunPathSize` copy to derive boundary paths independently of the check. No deviations; no follow-ups.
