# Work plan: Secrets: over-granted token isn't revoked at once when the run is already closed (follow-up from #38)

**Issue:** [#43](https://github.com/mayusuk/agent-loop/issues/43) (bug)
**Branch:** `issue/43-secrets-over-granted-token-isn-t`
**Base:** `origin/main @ de8b903` (checked before branching)
**Status:** Draft

## Summary

`Run.issue` now treats a token that GitHub over-granted (handed back with `ErrPermission` because its revoke failed) like any other token that arrives after `Close`: if the run is already closed it revokes it at once, with a context the run's cancellation doesn't end, and returns `ErrRunClosed`.

## Context

TECHNICAL-SPEC §7.3 and INTEGRATION-SPEC §9.8 step 6: a run's tokens are revoked when it ends. `GitHubApps.Token` returns an over-granted token together with `ErrPermission` when it can't revoke it (work plan #17). **Root cause:** in `internal/secrets/run.go`, the error branch of `Run.issue` appends that token to `issued` without checking `closed`. When `Close` already ran, the token stays live until some later `Close`, and the caller sees `ErrPermission` instead of `ErrRunClosed`. The success branch already handles this case (review R1/R6 of #38, D13 of #17).

## Approach

In `internal/secrets/run.go`, route both branches (plain token, and over-granted token with error) through the same closed check: keep the token if the run is open, else revoke it now with `context.WithoutCancel(ctx)` and keep it only if that revoke fails. Tests in `internal/secrets/run_test.go`.

## Steps

- [ ] Test: an over-granted token that arrives after `Close` is revoked at once and `Issue` returns `ErrRunClosed`; on a failed revoke it is kept for the next `Close` (fails before the fix).
- [ ] Fix `Run.issue`.

## Acceptance criteria

- An over-granted token issued after the run closed is revoked at once with a context that ignores the run's cancellation.
- The caller gets `ErrRunClosed`, not `ErrPermission`.
- If that revoke fails, the token is kept so the next `Close` retries it.
- An over-granted token issued while the run is open still returns `ErrPermission` and is revoked by `Close`.

## Decisions

- **D1** [gap] TECHNICAL-SPEC §7.3: when an over-granted token arrives after the run closed, `Issue` returns `ErrRunClosed` joined with the late revoke's error only, not with the over-grant `ErrPermission`, the same as the path for a plain late token. Why: the issue asks for `ErrRunClosed`, and the over-grant no longer matters once the token is revoked; a caller checking `ErrPermission` first would otherwise treat a closed run as a permission failure.

## Verification

None beyond the standard checks.

## Risks and open questions

None.

## Outcome

_Filled in before shipping: what was done, deviations from this work plan's approach and why, follow-up issues filed. Details the specs should record go in Decisions, not here._
