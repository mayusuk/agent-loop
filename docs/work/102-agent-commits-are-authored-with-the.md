# Work plan: Agent commits are authored with the owner's git identity

**Issue:** [#102](https://github.com/mayusuk/agent-loop/issues/102) (bug)
**Branch:** `issue/102-agent-commits-are-authored-with-the`
**Base:** `origin/main @ f4771cf` (checked before branching)
**Status:** Draft

## Summary

Commits a handler makes in an implement or address run are now authored and committed as the coder bot, with the same signature the worker's own merge commits use, and the owner's global and system git config no longer applies to them. Review runs are unchanged.

## Context

Root cause: `internal/runners/local.go` builds the handler's environment from `InheritedEnv` (which includes `HOME`) and sets no git identity, so `git commit` inside the run reads the owner's `~/.gitconfig` (user.name/email, signing, hooks, aliases). The worker's own merge commits (`internal/workspace/gitops.go` `CommitMerge`) are right because they set `GIT_AUTHOR_*`/`GIT_COMMITTER_*` from `Activities.CoderSignature`, built once in `cmd/agent-loop/wire.go`. INTEGRATION-SPEC §9: the coder identity owns agent code; TECHNICAL-SPEC §7.2 local runner.

## Approach

- Move `Signature` to `internal/ports` (`workspace.Signature` stays as an alias, so callers don't change) and add `RunSpec.Author ports.Signature` (zero for review).
- `internal/workflows/stage.go` sets `Author: a.CoderSignature` for coder-identity runs only, so the signature is built once (wire.go).
- `internal/runners/local.go`: when `Author` is set, write an empty `gitconfig` in the run directory and set `GIT_CONFIG_GLOBAL` to it, `GIT_CONFIG_NOSYSTEM=1`, and `GIT_AUTHOR_NAME/EMAIL`, `GIT_COMMITTER_NAME/EMAIL`. All six names become reserved: `RunSpec.Env` and `PassEnv` may not set them.

## Steps

- [ ] _Each step small enough to commit on its own._
- [ ] _For a bug, the first step is a test that fails because of it._

## Acceptance criteria

- Implement and address runs set `GIT_AUTHOR_NAME/EMAIL` and `GIT_COMMITTER_NAME/EMAIL` to the coder app's bot signature, the one gitops uses.
- `GIT_CONFIG_GLOBAL` points at a run file, so the owner's global settings (signing, hooks, aliases) don't apply.
- Review runs need none of this.

## Decisions

- **D1** [gap] TECHNICAL-SPEC §7.2 (handler environment): coder runs also get `GIT_CONFIG_NOSYSTEM=1`, not only an isolated global config. Why: the system config can carry the same signing and hook settings; the worker's own git calls (`workspace/git.go`) already skip it.
- **D2** [gap] TECHNICAL-SPEC §7.2: `GIT_AUTHOR_NAME`, `GIT_AUTHOR_EMAIL`, `GIT_COMMITTER_NAME`, `GIT_COMMITTER_EMAIL`, `GIT_CONFIG_GLOBAL` and `GIT_CONFIG_NOSYSTEM` are reserved for the runner on every run: a profile's env or pass-env naming one is refused. Why: a profile must not be able to make agent commits look like someone else's.
- **D3** [gap] TECHNICAL-SPEC §7.2: the run's global git config is an empty file `gitconfig` in the run directory, removed with it. Review runs keep the inherited config. Why: the issue scopes the fix to coder runs.

## Verification

Standard checks plus `GOOS=linux go build ./...` and `CGO_ENABLED=0 GOOS=darwin go build ./...`.

## Risks and open questions

PR #98 also edits `env` in local.go; expect a merge conflict to resolve there.

## Outcome

_Filled in before shipping: what was done, deviations from this work plan's approach and why, follow-up issues filed. Details the specs should record go in Decisions, not here._
