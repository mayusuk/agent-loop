# Work plan: Scenario 11.1 step 9: assert the waiting item is held at base-update review (follow-up from #89)

**Issue:** [#90](https://github.com/mayusuk/agent-loop/issues/90) (bug)
**Branch:** `issue/90-scenario-11-1-step-9-assert-the-waiting`
**Base:** `origin/main @ bc820ec` (checked before branching)
**Status:** Draft

## Summary

`TestScenario11_1_Step9_DailyLimitHoldsUntilTheDayBoundary` checks that one item waits for the daily limit, but not where. It now also asserts that the waiting item is held at its base-update review, and that the first run after 00:00 for that item is that base-update review, so a wait at another stage fails the test.

## Context

Issue #90, left open from the review of PR #89. FR-16 AC3/AC4; scenario 11.1 step 9 (PRODUCT-SPEC). The core moves an item to `waiting_limit` on `DailyLimitReached`, keeping the refused run (`core/stages.go` `dailyLimit`); a base-update review starts from `baseUpdated` (FR-6 AC6). At the day boundary `nextStage` resumes it. The test harness records every decision (`rec.decisions`) and every started run with its review kind (`rec.starts`, `StageRun.Kind`); refused runs are not recorded as starts.

Root cause: a test gap, not a product bug. The assertion counted items in `waiting_limit` without checking the stage they were held at.

## Approach

Test-only change in `internal/workflows/scenario_test.go`. At 23:59, for the waiting item, check that the decision that moved it to `waiting_limit` came from `in_review` and that the decision before it was `base_updated` (the one that started the base-update review). After the run, check that the waiting item's first run after 00:00 is a `review` of kind `base_update`.

## Steps

- [ ] Assert the waiting item is held at its base-update review, before and after 00:00.

## Acceptance criteria

- The test asserts that the item waiting for the daily limit is held at the base-update review, so a wait at a different stage cannot pass.

## Decisions

None.

## Verification

`go test -race -count=50 -run TestScenario11_1_Step9 ./internal/workflows/` to check the stronger assertion is not flaky; temporarily breaking it (expecting a different stage) to see it fail.

## Risks and open questions

None.

## Outcome

_Filled in before shipping: what was done, deviations from this work plan's approach and why, follow-up issues filed. Details the specs should record go in Decisions, not here._
