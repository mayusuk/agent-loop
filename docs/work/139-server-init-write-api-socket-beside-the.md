# Work plan: server init: write api.socket beside the config when there is no home directory (#134 D2)

**Issue:** [#139](https://github.com/mayusuk/agent-loop/issues/139) (feature)
**Branch:** `issue/139-server-init-write-api-socket-beside-the`
**Base:** `origin/main @ 5aef6f1` (checked before branching)
**Status:** Done

## Summary

With no home directory, `server init --config <path>` writes `[api] socket` as an absolute path to `api.sock` in the config file's directory, so the config it writes loads and `server init` succeeds. A socket path too long for a Unix socket is refused before any config is written. It also now says which `~/` projection candidates it skipped for lack of a home directory (#134 comment, PR #135 R1).

## Context

- #134 D2, resolved in #139 and recorded in the specs by PR #143 (TECHNICAL-SPEC §11 `server init` row, §13 "API socket" bullet; still open when this was planned).
- `cmd/agent-loop/serverinit.go` writes `profile.DefaultAPISocket` (`~/.config/agent-loop/api.sock`) unconditionally; `profile.LoadDeployment` then fails to expand `~` with no home directory.
- `pickProjectionPath` skips `~/` candidates silently when the home directory is unknown, though TECHNICAL-SPEC §13 asks `server init` to say which places it skipped.
- `internal/api.Listen` creates the socket directory with mode `0700` and chmods it, so the directory mode rule (§11) already holds at `server start`; `server init` creates the config directory `0700` too.
- Go's `net.Listen("unix", ...)` refuses names that don't fit `sun_path` (104 bytes on macOS, 108 on Linux).

## Approach

- `pickAPISocket(cfgDir)` in `serverinit.go`: with a home directory it returns the `~` default; without one it returns `<abs cfgDir>/api.sock`, refusing a path of `sun_path` size or longer with an error naming it. It runs before the config body is built, so a refused path writes nothing.
- In `pickProjectionPath`, report each `~/` candidate skipped for lack of a home directory.
- TECHNICAL-SPEC §11 (the API socket paragraph, which PR #143 doesn't touch): one sentence for the no-home-directory path and the length limit, consistent with PR #143's wording.

## Steps

- [x] Tests that fail: no home dir with `--config` in a temp dir gives a config that loads with the socket beside it; a too-long socket path is refused and no config is written; skipped `~/` candidates are reported.
- [x] Implement `pickAPISocket` and the skip message.
- [x] TECHNICAL-SPEC §11 text.

## Acceptance criteria

- With no home directory, `server init --config <path>` writes `[api] socket` as an absolute path to `api.sock` beside the config, and the config loads.
- A socket path too long for a Unix socket is refused with an error naming it.
- Tests: no home dir; `--config` in a temp dir; socket path too long.

## Decisions

- **D1** [gap] TECHNICAL-SPEC §11 (API socket length): a socket path is too long when its length in bytes is at least the platform's `sun_path` size (104 on macOS and other BSDs, 108 on Linux), so the longest accepted path is 103 or 107 bytes, leaving room for the terminating NUL. Why: the issue gives the sizes, not whether the NUL counts; leaving room for it works for every client, and Go on macOS already needs it.
- **D2** [gap] TECHNICAL-SPEC §11 (server init): the length check applies only to the socket path `server init` writes beside the config; the `~` default written when there is a home directory, and the socket of a kept config, are not checked by `server init` (`server start` still fails to listen on them). Why: the issue scopes the check to the no-home-directory path; refusing a kept config would be new behaviour.
- **D3** [gap] TECHNICAL-SPEC §13 (projection path): with no home directory, `server init` prints one line per `~/` projection candidate it skips: `Can't find the home directory (<error>); skipping <candidate> for the projection database`. Why: §13 asks it to say which places it skipped but gives no wording.

## Verification

Standard checks, plus `GOOS=linux go build ./...` and `CGO_ENABLED=0 GOOS=darwin go build ./...`.

## Risks and open questions

PR #143 edits the neighbouring TECHNICAL-SPEC §11 row and adds the §13 bullet; whichever merges second resolves the merge.

## Outcome

- `pickAPISocket` writes `~/.config/agent-loop/api.sock` with a home directory and `<config dir>/api.sock` (absolute) without one, refusing a path that doesn't fit `sun_path` with its NUL before any config is written.
- `pickProjectionPath` prints a line for each `~/` candidate skipped for lack of a home directory.
- TECHNICAL-SPEC §11 says both, pointing to §13, where PR #143 adds the "API socket" bullet.
- Tests (`cmd/agent-loop/serverinit_test.go`): no home with `--config` in a short temp dir gives a config that loads, names the socket in full and reports the skipped `~/` place; a too-long path is refused and nothing is written; a 103/107-byte path is accepted and can be listened on; with a home the default is kept. The first two fail on the old code.
- No deviations from the approach. No follow-up issues filed.
