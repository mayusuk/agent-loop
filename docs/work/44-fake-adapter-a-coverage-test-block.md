# Work plan: Fake adapter: a coverage test block asserts nothing (follow-up from #40)

**Issue:** [#44](https://github.com/mayusuk/agent-loop/issues/44) (bug)
**Branch:** `issue/44-fake-adapter-a-coverage-test-block`
**Base:** `origin/main @ 430fcad` (checked before branching)
**Status:** Draft

## Summary

One block of `TestCoverageGaps` in the fake adapter's tests pushed during a coder run on a branch whose only change is closed, then asserted nothing, so it could never fail. It now asserts the push is attributed to the coder. A wrong comment about `DefaultConfig` and `Owners` is fixed too.

## Context

- Review R6 on PR #40, round 2 (score 2, tests).
- Root cause: the push in that block updates no open change, so the fake emits no `HeadMoved` event and the test had no public way to read the commit's pusher attribution. It called `Push` and `end()` and stopped.
- The fake attributes a push (`attribute` in `internal/adapters/fake/trust.go`): the branch's item comes from its open change, else its latest change (`itemOfBranch`). A push inside a live coder run for that item is `coder`; this block covers the "latest, closed change" fallback.
- `DefaultConfig` sets `Owners: []ports.Actor{Owner}` (`fake.go`), so the comment "New leaves Owners empty when unset" was wrong.
- CLAUDE.md testing rules: tests use the fake, no real services.

## Approach

Add a test-only `export_test.go` in package `fake` with `(*Fake).PusherOf(commit)`, which returns the same attribution a `HeadMoved` event would carry. The block then asserts `PusherOf(pushed) == ports.PusherCoder`. No production code changes.

## Steps

- [ ] Add `PusherOf` (test-only) and assert the pusher in the closed-change block; check the assertion fails when `itemOfBranch` ignores closed changes; fix the `Owners` comment.

## Acceptance criteria

- The closed-change block of `TestCoverageGaps` asserts the pusher attribution and can fail.
- The comment at the `DefaultConfig` line says correctly what it sets for `Owners`.

## Decisions

None.

## Verification

Mutation check: with `itemOfBranch` changed to skip closed changes, the test fails with `pusher "unknown", want coder`. Restored afterwards.

## Risks and open questions

None. The helper is compiled only in tests.

## Outcome

_Filled in before shipping._
