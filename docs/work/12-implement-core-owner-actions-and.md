# Work plan: Implement core owner actions and supervisor rules

**Issue:** [#12](https://github.com/mayusuk/agent-loop/issues/12) (feature)
**Branch:** `issue/12-implement-core-owner-actions-and`
**Base:** `origin/main @ 0a0eccc` (checked before branching)
**Status:** In progress

## Summary

Adds the owner's actions to `internal/core`'s `Decide`: pause, resume and abort of an item, owner escalation, releasing the claim, closing or merging by hand, closing and reopening the change (with a new branch after a close, D44), and label changes from people who aren't owners (FR-13 AC7). Every owner action that "lets the running handler finish" records the result and then holds the item. It also adds the pure supervisor decisions for `ProjectWorkflow`: picking with the FR-2 AC1 exclusions and AC2 priority order, the WIP limit, claims, the FR-2 AC6 dependency escalation, and the daily agent-run count with its day boundary and the project and deployment pause gate (FR-16 AC3, AC4, FR-10 AC3).

## Context

- PRODUCT-SPEC §5.1 (labels), §6 (owner-actions table, claimed states), FR-2 AC1–AC6, FR-10 AC2, AC3, AC7, AC8, FR-12 AC4 (v0.1 rows; the plan-proposal rows belong to mint and materialise, v0.2), FR-13 AC7, FR-16 AC3, AC4, D44.
- TECHNICAL-SPEC §6.1 (claim number), §6.2 (supervisor pass), §6.3 (slots and the daily-limit unit), §6.5 (stopping a handler, daily reset), §8.2, §13 (`wip`, `types`, `priority`, `limits.daily_agent_runs`, `timezone`).
- INTEGRATION-SPEC §3.3 (branch `<prefix><key>-<n>`), §6.5 (status events), §8.1 (marker names, actor IDs), §9.3 (claim), §9.5 (dependency done), §12.3 (dry run lists the exclusion of every candidate).
- T5 (#10) and T6 (#11) work plans: `Decide`, the decision record, `nextStage`, `escalate`, `resumed`, the D12 daily-limit gate (`DailyLimitReached`, `DayBoundary`), and the merge stage. T5 D21 left owner pause, abort, release, close and the daily count to this task; T6 D12 leaves the hand merge here. `internal/core` imports only the standard library and `contract/`.

## Approach

- `owner.go` (new): `Role` and `Identities.Role` (owner by account ID, FR-13 AC7, INTEGRATION-SPEC §8.1); the owner events `MarkerChanged`, `OwnerCommand`, `Assigned`, `ItemClosed`, `ChangeClosed`, `ChangeMerged`, `ChangeReopened`, `Commented`; their handlers; and the **hold**: an owner action that lets in-flight work finish moves the item to its target state (`paused`, `aborted`, `escalated`, `released`) at once and remembers the in-flight stage in `ItemState.Running`. The event that ends that work is decided as usual, then the hold filters what may happen next (no stage starts; a paused item's escalation waits until it resumes).
- `decide.go`: routing: owner events first, then held events, then the existing merge and stage dispatch. `resumed` and `blockRemoved` learn the hold.
- `state.go`, `action.go`, `event.go`: `StateReleased`; `Running`, `PendingEscalation`, `ResumeAddress`, `ClosedChanges`; the `StopRun`, `CloseItem` and `EndItem` actions; `SetMarker.OnChange`; `RunStage.Branch`; `Started.ClosedChanges`; marker and reason constants.
- `pick.go` (new): `Candidate`, `PickPolicy`, `ProjectView`, `Pick` (verdict for every candidate in priority order, the picks within free WIP, and FR-2 AC6 escalations), `ClaimFor` (workflow ID, claim number, keys).
- `daily.go` (new): `DailyCount`, `Admit` (the gate before each start: pause, then the daily-limit unit), `DayReset`, `NextDayBoundary`.
- Tests: `owner_test.go` (a table per §6 owner-action row over every claimed state, and one test per FR-12 AC4 v0.1 row, `TestFR12AC4_...`, `TestFR13AC7_...`, `TestFR10AC3_...`, `TestFR10AC7_...`), `pick_test.go` (`TestFR2AC1_...` to `TestFR2AC6_...`), `daily_test.go` (`TestFR16AC3_...`, `TestFR16AC4_...`), and the new events in the property test.

## Steps

- [ ] Owner events, the hold, pause, resume, abort, owner escalation and label changes by others.
- [ ] Release, close and merge by hand, change closed and reopened, new branch (D44).
- [ ] Picking, WIP, claim, FR-2 AC6.
- [ ] Daily count, admission gate and day boundary.
- [ ] Property test covers the new events; the FR-12 AC4 and §6 tables.

## Acceptance criteria

- Every v0.1 row of the FR-12 AC4 table, and of the §6 owner-actions table, has a table test (TestFR12AC4_...).
- Picking honours each exclusion in FR-2 AC1 and the priority order in AC2, and WIP never goes over the limit (TestFR2AC3_...).
- At the daily limit, no agent stage starts and items wait. At the day boundary, they continue (TestFR16AC3_..., TestFR16AC4_...).
- A label change by a human who isn't an owner has no effect (TestFR13AC7_...).

## Decisions

- **D1** [gap] §6 owner-actions table, FR-10 AC3, AC7, FR-12 AC4: an owner action that lets the running handler finish (pause, abort, adding `needs-owner`, release) moves the item to its target state at once and keeps the in-flight work (a handler run, the agent-free base merge, or a merge call) in `Running`. Its result is recorded as usual (rounds, findings, head, replies), but nothing new starts: retries, crash reruns, the next review, the merge check and the merge are dropped, and the next stage is chosen when the item resumes (FR-10 AC2, AC8). A merge call that completes still makes the item `merged`. A merging item with no merge call in flight just stops, because nothing in it is running.

## Verification

Standard checks. `go test -race ./internal/core/...` covers the table tests and the property test.

## Risks and open questions

- Many owner actions meet many states. The tables run each row over every claimed state to catch gaps.

## Outcome

_Filled in before shipping._
