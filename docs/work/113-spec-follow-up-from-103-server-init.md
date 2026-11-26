# Work plan: Spec follow-up from #103: server init writes a state path it never checks it can create

**Issue:** [#113](https://github.com/mayusuk/agent-loop/issues/113) (feature)
**Branch:** `issue/113-spec-follow-up-from-103-server-init`
**Base:** `origin/main @ abf1b87` (merged after branching; #125 folded D1 and D2)
**Status:** In progress

## Summary

`server init` now honours `XDG_STATE_HOME`: when it is set, a new deployment config defaults the projection database to `$XDG_STATE_HOME/agent-loop/projection.db`, otherwise `~/.local/state/agent-loop/projection.db`, then the existing fallbacks. This implements the owner's ruling on #113 D3 (2026-11-25).

## Context

- #103 added `pickProjectionPath` in `cmd/agent-loop/serverinit.go`: candidates are the default, then `~/Library/Application Support/agent-loop` on macOS, then the config directory; each is checked by `usableDir` (create 0700 + probe file).
- PR #125 folded #113 D1 and D2 into TECHNICAL-SPEC §11 and §13 and left D3 open. Owner ruling on #113: honour `XDG_STATE_HOME`.

## Approach

In `pickProjectionPath`, the first candidate is `$XDG_STATE_HOME/agent-loop/projection.db` (absolute) when `XDG_STATE_HOME` is set to an absolute path, else `profile.DefaultProjectionPath`. The fallbacks and the writable check stay as they are. Update TECHNICAL-SPEC §13 (projection path). Tests set a temp `HOME` and `XDG_STATE_HOME`; existing tests clear `XDG_STATE_HOME` so a developer's environment can't change them.

## Steps

- [x] Tests and code: XDG_STATE_HOME candidate in `pickProjectionPath`; existing tests clear `XDG_STATE_HOME`.
- [ ] TECHNICAL-SPEC §13 text for the default path.

## Acceptance criteria

- With `XDG_STATE_HOME` set, a new config from `server init` names `$XDG_STATE_HOME/agent-loop/projection.db` and creates that directory.
- With it unset, the default stays `~/.local/state/agent-loop/projection.db`.
- If the XDG directory can't be created or written to, the existing fallbacks apply and the skipped place is reported.

## Decisions

- **D1** [ambiguity] TECHNICAL-SPEC §13 (projection path): when `XDG_STATE_HOME` is set but `$XDG_STATE_HOME/agent-loop` can't be created or written to, `server init` goes straight to the existing fallbacks (Application Support on macOS, then the config directory); it does not also try `~/.local/state/agent-loop`. Why: the ruling reads "otherwise" — `~/.local/state` is the default only when `XDG_STATE_HOME` is unset.
- **D2** [gap] TECHNICAL-SPEC §13: an empty or relative `XDG_STATE_HOME` is treated as unset. Why: the XDG Base Directory spec says relative paths are invalid and must be ignored, and an empty value means "use the default".
- **D3** [gap] TECHNICAL-SPEC §13: the XDG path is written to the new config as an absolute path, and the loader's default for a config with no `[projection] path` stays `~/.local/state/agent-loop/projection.db`. Why: the ruling covers `server init`; a config it writes always names the path, so the server doesn't depend on the environment it starts in.

## Verification

Standard checks, plus `GOOS=linux go build ./...` and `CGO_ENABLED=0 GOOS=darwin go build ./...`.

## Risks and open questions

None.

## Outcome

_Filled in before shipping._
