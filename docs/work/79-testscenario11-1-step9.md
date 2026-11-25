# Work plan: TestScenario11_1_Step9_DailyLimitHoldsUntilTheDayBoundary is flaky

**Issue:** [#79](https://github.com/mayusuk/agent-loop/issues/79) (bug)
**Branch:** `issue/79-testscenario11-1-step9`
**Base:** `origin/main @ a6f091c` (checked before branching)
**Status:** In progress

## Summary

Make `TestScenario11_1_Step9_DailyLimitHoldsUntilTheDayBoundary` deterministic. It assumed X and Y both finish their round reviews before either merges, but nothing ordered that, so sometimes neither item reached the daily limit. Both gates now stay pending until both items wait to merge.

## Context

Root cause (reproduced: 3 failures in 60 runs with `-race -count=60`). Agent runs are activities, which take no workflow time in Temporal's test environment, so X's and Y's item workflows race in real time. The test expects 4 agent runs before 00:00 (both implements, both round reviews) and a 5th, Y's base-update review, to be refused at the limit. In the failing runs X implemented, was reviewed and merged before Y's implement branched, so Y's branch already contained X and Y merged with no base update. Only 4 runs happened before 00:00, nothing waited (`0 items wait for the limit`), and no `daily_limit` notification went out (`notifications []`). The decision dump of a failing run shows both items going `in_review->merging->merged` with no base update.

This is a test ordering bug, not a product bug: either interleaving is valid behaviour.

## Approach

In the test, hold both items' gates (the scenario harness's `hold`/`release`, as step 8 does) and release them at 1h, after checking the board shows both in `merging`. Then one merges first and the other needs the base-update review, which is the 5th run and waits for the limit. Nothing the test asserts is removed or loosened; it gains a board check.

## Steps

- [x] Hold X's and Y's gates until both wait to merge, and check the board then (`internal/workflows/scenario_test.go`).
- [x] Run the test 300 times under `-race`, and the whole `internal/workflows` package repeatedly, to check for other flaky tests.

## Acceptance criteria

- At one minute before 00:00, exactly one of X and Y is in `waiting_limit`, every run.

## Decisions

None.

## Verification

`go test -race -count=300 -run TestScenario11_1_Step9 ./internal/workflows/` and the whole package with `-count=15`.

## Risks and open questions

None.

## Outcome

_Filled in before shipping._
