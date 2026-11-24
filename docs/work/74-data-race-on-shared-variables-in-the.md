# Work plan: Data race on shared variables in the live scenario test

**Issue:** [#74](https://github.com/mayusuk/agent-loop/issues/74) (bug)
**Branch:** `issue/74-data-race-on-shared-variables-in-the`
**Base:** `origin/main @ 904c047` (checked before branching)
**Status:** Done

## Summary

The live scenario test (`TestScenario11_1_Live`) shares step-local variables between the handler goroutine (`lv.handle`, which runs the scripted callbacks) and the test goroutine with no synchronisation. Under `go test -race` with `AGENT_LOOP_LIVE=1` that is a data race. This guards every such variable with `lv.mu`.

## Context

Review finding R5 on #73. Root cause: script callbacks (`review.before`, `address.manual`, `address.during`) run on the `lv.handle` goroutine started per scripthandler connection. The only ordering between them and the test goroutine goes through external processes (worker, GitHub), which the race detector and the Go memory model do not count as synchronisation.

Shared variables found:
- step 1: `manualIDs` and `ruled` (written in callbacks, read after `await`), and `threads` (appended on the test goroutine after `await`, read by `during` and `before` in callbacks).
- step 3: `first` (written in `before`, read after `await`) and `answered` (appended in `record`, read at the end).

Also `lv.drv.Tokens` (found in review R2 on #80): step 1's `during` callback calls `lv.asCoder`, which adds the coder's token to the driver's map while the `runGates` goroutine's `SetGate` calls, and every other driver call, read it in `githubtest.Driver.callRepo`. No other step shares variables with a callback. `lv.rows`, `lv.boards` and `lv.bad` are touched only by the test goroutine.

## Approach

Take `lv.mu` around every write and read of those variables, in `cmd/agent-loop/scenario_live_test.go`. Inside callbacks, copy the value under the lock and release it before calling `lv.problem`, `lv.asCoder` or the driver (`lv.problem` takes `lv.mu` too, and GitHub calls should not run while the lock is held). For the token map, `githubtest.Driver` gains `SetToken`, and its lookup reads the map under a lock, so a token added while calls run is synchronised; `asCoder` uses it. Test-only change: no product behaviour changes.

## Steps

- [x] Guard step 1's and step 3's shared variables with `lv.mu`.

## Acceptance criteria

- The live scenario test's variables shared between the handler goroutine and the test goroutine are guarded (issue #74), so `go test -race` with `AGENT_LOOP_LIVE=1` reports no race on them.

## Decisions

None.

## Verification

The live test needs real GitHub and Temporal (`AGENT_LOOP_LIVE=1`), so it cannot run here or in CI. Proof for the step variables is by inspection of each access; the driver's token map has an offline race test (`TestIssue74_SetTokenWhileCallsRun`), plus the standard checks (the file still compiles and vets under the race build: `go vet ./cmd/agent-loop/`, `go test -race -count=1 ./cmd/agent-loop/`).

## Risks and open questions

Deadlock if a callback held `lv.mu` while calling `lv.problem`. The fix releases the lock first.

## Outcome

Done as planned. In `cmd/agent-loop/scenario_live_test.go`, step 1's `threads`, `manualIDs` and `ruled` and step 3's `first` and `answered` are now read and written only under `lv.mu`. Callbacks copy values under the lock and release it before calling the driver or `lv.problem`, and the test goroutine reads snapshots under the lock after `await`. One small change: if step 3's owner comment fails, the callback now returns after reporting the problem, where it used to fall through. No follow-up issues.


Review round 1 (#80): R2 led to a change. `asCoder` wrote `lv.drv.Tokens` under `lv.mu`, but the driver read the map unlocked from the gate goroutine. `githubtest.Driver` now has `SetToken` and a locked token lookup, proved by `TestIssue74_SetTokenWhileCallsRun` under `-race` (it reports a data race with the old direct map write). R1 (CI pending) needed no code change.
