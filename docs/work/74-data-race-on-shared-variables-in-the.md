# Work plan: Data race on shared variables in the live scenario test

**Issue:** [#74](https://github.com/mayusuk/agent-loop/issues/74) (bug)
**Branch:** `issue/74-data-race-on-shared-variables-in-the`
**Base:** `origin/main @ 904c047` (checked before branching)
**Status:** Draft

## Summary

The live scenario test (`TestScenario11_1_Live`) shares step-local variables between the handler goroutine (`lv.handle`, which runs the scripted callbacks) and the test goroutine with no synchronisation. Under `go test -race` with `AGENT_LOOP_LIVE=1` that is a data race. This guards every such variable with `lv.mu`.

## Context

Review finding R5 on #73. Root cause: script callbacks (`review.before`, `address.manual`, `address.during`) run on the `lv.handle` goroutine started per scripthandler connection. The only ordering between them and the test goroutine goes through external processes (worker, GitHub), which the race detector and the Go memory model do not count as synchronisation.

Shared variables found:
- step 1: `manualIDs` and `ruled` (written in callbacks, read after `await`), and `threads` (appended on the test goroutine after `await`, read by `during` and `before` in callbacks).
- step 3: `first` (written in `before`, read after `await`) and `answered` (appended in `record`, read at the end).

No other step shares variables with a callback. `lv.rows`, `lv.boards` and `lv.bad` are touched only by the test goroutine.

## Approach

Take `lv.mu` around every write and read of those variables, in `cmd/agent-loop/scenario_live_test.go`. Inside callbacks, copy the value under the lock and release it before calling `lv.problem`, `lv.asCoder` or the driver (`lv.problem` takes `lv.mu` too, and GitHub calls should not run while the lock is held). Test-only change: no product behaviour changes.

## Steps

- [ ] Guard step 1's and step 3's shared variables with `lv.mu`.

## Acceptance criteria

- The live scenario test's variables shared between the handler goroutine and the test goroutine are guarded (issue #74), so `go test -race` with `AGENT_LOOP_LIVE=1` reports no race on them.

## Decisions

None.

## Verification

The live test needs real GitHub and Temporal (`AGENT_LOOP_LIVE=1`), so it cannot run here or in CI, and no offline test can fail because of this bug. Proof is by inspection of each access, plus the standard checks (the file still compiles and vets under the race build: `go vet ./cmd/agent-loop/`, `go test -race -count=1 ./cmd/agent-loop/`).

## Risks and open questions

Deadlock if a callback held `lv.mu` while calling `lv.problem`. The fix releases the lock first.

## Outcome

_Filled in before shipping: what was done, deviations from this work plan's approach and why, follow-up issues filed. Details the specs should record go in Decisions, not here._
