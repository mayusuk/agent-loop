# Work plan: Sandboxed handlers can't run go build or go test: the Go build cache is outside the run's writable paths

**Issue:** [#96](https://github.com/mayusuk/agent-loop/issues/96) (bug)
**Branch:** `issue/96-sandboxed-handlers-can-t-run-go-build`
**Base:** `origin/main @ f4771cf` (checked before branching)
**Status:** Ready to ship

## Summary

The local runner now points `GOCACHE` and `XDG_CACHE_HOME` into the run's temp directory, unless the stage's env or `PassEnv` sets them. Before this, a sandboxed handler couldn't build or test Go code. The default cache (`~/Library/Caches/go-build`) is outside the paths a run may write, and the agent couldn't set `GOCACHE` itself without an approval a headless run can't give. This was found in the e2e run on `mayusuk/mdtoc-e2e`, where it made #4 escalate.

## Context

- TECHNICAL-SPEC §7.2: a run may write only the worktree, its out and result paths, and its per-run temp directory (`TMPDIR`).
- `internal/runners/local.go`: the handler's environment is `InheritedEnv` plus `PassEnv` and `RunSpec.Env`. `GOCACHE` isn't in it, so Go uses `os.UserCacheDir()`, which is `~/Library/Caches` on macOS and ignores `XDG_CACHE_HOME`.
- E2e evidence: the implement runs for mdtoc-e2e #2 and #3 found a writable `GOCACHE` by luck. The address run and round-2 review of PR #9 couldn't compile, and both implement attempts of #4 failed with "the sandbox denies writes to the Go build cache".

## Approach

- `ToolCaches` in `internal/runners/local.go` maps `GOCACHE` → `$TMPDIR/go-build` and `XDG_CACHE_HOME` → `$TMPDIR/cache`. These are set after `PassEnv` and `RunSpec.Env`, and only when neither has set them.
- Test `TestHandlerEnvironmentPutsToolCachesInTheRunsTempDir` checks both defaults, that the worker's own `GOCACHE` doesn't leak in, and that a stage env value wins.

## Steps

- [x] Test that fails without the change.
- [x] Set the tool caches in the runner.

## Acceptance criteria

- A sandboxed handler's `GOCACHE` and `XDG_CACHE_HOME` are inside the run's temp directory by default.
- A value set in the stage's env or `PassEnv` is kept.

## Decisions

- **D1** [gap] TECHNICAL-SPEC §7.2: the runner sets `GOCACHE` and `XDG_CACHE_HOME` to directories in the run's temp directory, because the toolchains' default caches are outside the writable paths. The cache starts empty on every run, which makes builds slower but keeps runs isolated. Why: §7.2 allows writes only there, and without a writable cache no Go project can be built or tested under the sandbox.
- **D2** [open] TECHNICAL-SPEC §7.2, §13: other toolchains need the same thing (npm's cache, pip's cache, the Go module cache for projects with dependencies, Cargo). The spec should say whether the runner sets a fixed list, or the profile declares per-project caches (for example a `stages.<s>.env` or a `sandbox.caches` key). It should also say whether a cache that persists between runs and is shared is allowed.

## Verification

- Without the change, the new test fails: `GOCACHE = "", want ".../tmp/go-build"`.
- The e2e run on `mayusuk/mdtoc-e2e` is repeated with the fixed worker.

## Risks and open questions

- A fresh cache on every run makes each build slower (D2).

## Outcome

Done as planned. D2 is left for the spec issue.
