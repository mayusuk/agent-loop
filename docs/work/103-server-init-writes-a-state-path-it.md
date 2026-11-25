# Work plan: server init writes a state path it never checks it can create

**Issue:** [#103](https://github.com/mayusuk/agent-loop/issues/103) (bug)
**Branch:** `issue/103-server-init-writes-a-state-path-it`
**Base:** `origin/main @ f4771cf` (checked before branching)
**Status:** In progress

## Summary

`agent-loop server init` now creates the projection database's directory before it finishes and says where the database will live. When it writes a new deployment config and the default directory (`~/.local/state/agent-loop`) can't be created, it falls back to a writable platform directory and writes that path instead. With an existing config whose directory can't be created, it fails with a message naming the key to change, instead of leaving `server start` to fail later.

## Context

- TECHNICAL-SPEC §13 lists the projection DB in the deployment config but gives no default path; the default `~/.local/state/agent-loop/projection.db` comes from #9 work plan D14 (`profile.DefaultProjectionPath`).
- Root cause: `runServerInit` (cmd/agent-loop/serverinit.go) writes `profile.DefaultProjectionPath` into the template without touching its directory. `projection.Open` runs `MkdirAll` only at `server start`, so a root-owned `~/.local` surfaces there as `projection: mkdir ...: permission denied`.
- The sandbox (`runners.DeniedReads`) already denies the configured projection DB and its journal files wherever it is, and the config directory, so a fallback path stays unreadable to handlers.

## Approach

In `runServerInit`:
1. For a new config, try the projection candidates in order, creating each one's directory (mode 0700): the default, then on macOS `~/Library/Application Support/agent-loop/projection.db`, then `projection.db` in the deployment config's directory (which server init has just created). Write the first that works; print a line naming each one skipped and why.
2. After loading the config (new or kept), create the configured projection directory; on failure exit 1 naming `[projection] path` in the config file. On success print `Projection database: <path>`.

Files: `cmd/agent-loop/serverinit.go`, `cmd/agent-loop/serverinit_test.go`.

## Steps

- [x] Test: with a temp HOME whose `~/.local` isn't writable, server init writes a writable projection path, creates its directory and reports it; with a kept config naming an uncreatable path it exits 1 (fails before the fix).
- [ ] Fix: create the projection directory in server init, with the fallback for new configs.

## Acceptance criteria

- server init creates the projection directory and reports it.
- When the default directory can't be created, a new config names a writable platform default (`~/Library/Application Support/agent-loop` on macOS).
- Tested with a temp HOME where the default parent isn't writable.

## Decisions

- **D1** [gap] TECHNICAL-SPEC §13 (deployment config, `projection.path`): when `~/.local/state/agent-loop` can't be created, a new config from `server init` uses `~/Library/Application Support/agent-loop/projection.db` on macOS, then `projection.db` in the deployment config's directory (`~/.config/agent-loop` by default) on any platform. The default stays `~/.local/state/agent-loop/projection.db`. Why: the spec names no default; the issue asks for a writable platform fallback, and the config directory is one server init has just created and the sandbox already denies.
- **D2** [gap] TECHNICAL-SPEC §11/§13 (`server init`): server init creates the projection database's directory (mode 0700) and prints `Projection database: <path>`. With an existing config whose directory can't be created it exits 1 and names `[projection] path` in that file; it never rewrites an existing config. Why: the spec doesn't say what server init checks, and an existing config is the owner's.
- **D3** [open] TECHNICAL-SPEC §13: `XDG_STATE_HOME` isn't honoured for the default projection path. Why: not needed for this fix.

## Verification

Full local suite plus `GOOS=linux go build ./...` and `CGO_ENABLED=0 GOOS=darwin go build ./...`. The new test skips when run as root, where permissions aren't enforced.

## Risks and open questions

The fallback changes where a new install keeps its database on machines with an unwritable `~/.local`; existing configs are untouched.

## Outcome

_Filled in before shipping: what was done, deviations from this work plan's approach and why, follow-up issues filed. Details the specs should record go in Decisions, not here._
