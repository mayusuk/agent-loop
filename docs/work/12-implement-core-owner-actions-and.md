# Work plan: Implement core owner actions and supervisor rules

**Issue:** [#12](https://github.com/mayusuk/agent-loop/issues/12) (feature)
**Branch:** `issue/12-implement-core-owner-actions-and`
**Base:** `origin/main @ 0a0eccc` (checked before branching)
**Status:** Done

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

- [x] Owner events, the hold, pause, resume, abort, owner escalation and label changes by others.
- [x] Release, close and merge by hand, change closed and reopened, new branch (D44).
- [x] Picking, WIP, claim, FR-2 AC6.
- [x] Daily count, admission gate and day boundary.
- [x] Property test covers the new events; the FR-12 AC4 and §6 tables.

## Acceptance criteria

- Every v0.1 row of the FR-12 AC4 table, and of the §6 owner-actions table, has a table test (TestFR12AC4_...).
- Picking honours each exclusion in FR-2 AC1 and the priority order in AC2, and WIP never goes over the limit (TestFR2AC3_...).
- At the daily limit, no agent stage starts and items wait. At the day boundary, they continue (TestFR16AC3_..., TestFR16AC4_...).
- A label change by a human who isn't an owner has no effect (TestFR13AC7_...).

## Decisions

- **D1** [gap] §6 owner-actions table, FR-10 AC3, AC7, FR-12 AC4: an owner action that lets the running handler finish (pause, abort, adding `needs-owner`, release) moves the item to its target state at once and keeps the in-flight work (a handler run, the agent-free base merge, or a merge call) in `Running`. Its result is recorded as usual (rounds, findings, head, replies), but nothing new starts: retries, crash reruns, the next review, the merge check and the merge are dropped, and the next stage is chosen when the item resumes (FR-10 AC2, AC8). A merge call that completes still makes the item `merged`. A merging item with no merge call in flight just stops, because nothing in it is running.
- **D2** [conflict] §6 owner-actions table ("resumes a paused item → next_stage"), FR-10 AC2, TECHNICAL-SPEC §8.2 vs §5.1 `blocked`: a blocked item that is paused, or escalated by the owner, goes back to `blocked` when it resumes while its dependency is still open, and the dependency closing while it is held is still acted on. Owner: §5.1 overrides the §6 row; resuming a paused item that is still blocked by an open dependency returns it to `blocked`, because the loop removes `blocked` only when the dependency closes, on release, or when the owner removes it. The §6 table (and §8.2) to be amended through #56.
- **D3** [gap] §6, FR-12 AC4 ("a paused item stays paused and is escalated when it resumes"): every escalation that arises while an item is paused (from the recorded result, the owner adding `needs-owner`, or the change being closed) is kept and announced (marker, status record, notification) only when the pause ends. The owner adding `needs-owner` while work is in flight moves the item to `escalated` at once and announces it when the work ends; if the result itself escalates, that escalation replaces the owner's.
- **D4** [gap] INTEGRATION-SPEC §6.5 reason codes and events: `owner_escalated` (the owner added `needs-owner`), `change_closed`, `paused` (the `resume` record of a pause, `rounds_added` 0), `released` (`release` record), and `manual_action` records with `item_closed`, `merged_by_hand` and `change_reopened`. A pause posts a `pause` record and an abort an `abort` record.
- **D5** [gap] FR-10 AC5, FR-12 AC4: the owner removing `needs-owner` on a paused item settles the escalation (rounds added, commits approved, `resume` record), and the item stays paused. When the pause ends after a round-limit escalation with a blocking finding, it starts with an address run.
- **D6** [gap] FR-10 AC7, FR-12 AC4: under an abort or a pending release, the in-flight result is recorded, but the escalation or block it would cause is dropped: an aborted item never escalates, and a resume command has no effect on it.
- **D7** [gap] FR-12 AC4 release rows: a release waits for in-flight work, then removes `in-progress`, `blocked`, `needs-owner`, `paused` and `aborted` from the item and `aborted` from the change (set-to-value writes, so a label that isn't there is a no-op), posts the `release` record, and tells the workflow to free the WIP slot (`EndItem`). Removing `aborted` releases only an item in `aborted`.
- **D8** [gap] §6, TECHNICAL-SPEC §6.5: "stops a running handler" cancels only a handler run (`StopRun`). The agent-free base merge and a merge call are left to end, and their outcomes are ignored. A merge by hand removes only `in-progress`, as FR-12 AC4 says, closes the item if the tracker didn't (`CloseItem`), and files no follow-up.
- **D9** [gap] FR-12 AC4, FR-13 AC7: closing the item, closing or reopening the change, a merge, and assigning a human count from any account; a merge from the loop identity is the loop's own merge and is ignored here. Label changes from the loop's own identity come back in the feed and, like any other non-owner change, have no effect.
- **D10** [gap] FR-6 AC5, D44: when the change is closed, its rounds, base updates, findings, latest review and manual comments are dropped, because they belong to that change. The round allowance added by resumes is kept, because its records are on the item (TECHNICAL-SPEC §10). The implement `RunStage` carries `Branch` = closed changes + 1, and `Started.ClosedChanges` rebuilds the count.
- **D11** [gap] FR-12 AC4 reopen-change row: a reopened change is adopted only when the item has no change and nothing is in flight. Adopting it lowers the closed-change count. A `change_closed` escalation then ends without an owner resume (`needs-owner` removed, `manual_action` record) and the next stage is chosen; on a paused item the pending escalation is dropped.
- **D12** [gap] FR-10 AC3, TECHNICAL-SPEC §6.2, §6.3: a project or deployment pause is enforced by the workflow's gate before each start (`Admit`), for agent stages, merges and the base merge alike. A paused project answers `paused`, uses no daily unit, and the item stays in its state, which the board shows (§6). Picking stops too. Because the gate holds a merging item's checks, the workflow sends `ProjectResumed` when the pause ends, and the merge stage gets a new time limit from then (FR-10 AC8, FR-16 AC1, review R5).
- **D13** [gap] FR-16 AC3, AC4: the owner is notified once a day, at the first refusal. At the day boundary every `waiting_limit` item is woken; the owner is notified then only if the limit was reached on the day that ended. `limits.daily_agent_runs` of 0 or less uses the default 20, so the limit is never off.
- **D14** [gap] FR-2 AC1, AC2, INTEGRATION-SPEC §12.3: `Pick` returns a verdict for every candidate in priority order with the first exclusion that applies (closed, claimed, `plan`, not ready, type not configured, human assignee, `aborted`, `paused`, `needs-owner`, dependency not done, dependency open, then deployment pause, project pause, daily limit, WIP). An item with several types takes its best rank; ties go by creation time (oldest unless the priority says `newest`), then key. Items with no listed type come after the listed ones.
- **D15** [gap] FR-2 AC6, TECHNICAL-SPEC §9: the dependency escalation of an unclaimed item is found on every pass that sees a not-done dependency and no `needs-owner`, even while the project is paused. Its key is `item/<project>/<key>#0:dependency_not_done:<n>`, claim 0 because the item holds no claim, with *n* one more than the item's earlier such records, so each removal by the owner leads to one re-add.
- **D16** [gap] FR-16 AC3, INTEGRATION-SPEC §5.1, TECHNICAL-SPEC §13: the spec settles the daily spend limit (`usage.cost` is added to the daily spend when `limits.daily_spend` is set, and reaching it stops new work). The core sees no handler usage, so it is counted and checked in the workflow: issue #21 (T16) now lists counting spend toward the daily spend limit in its scope.
- **D17** [gap] TECHNICAL-SPEC §6.5, §6.3, FR-16 AC1: §6.5 sets a stage's time limit as the `RunStage` activity's `StartToCloseTimeout`, which runs from the activity's start, after the `Admit` gate. So the limit counts from the admitted start, and `RunStage.Deadline` (set when the core decides) is informational: the workflow (T16, #21) does not use it as the limit.
- **D18** [gap] §5.1, FR-10 AC7, D1: a merge call that completes under a hold closes the item, so the hold's labels (`blocked`, `needs-owner`, `paused`, `aborted`) are removed from the item and `aborted` from the change; `in-progress` is left as on any loop merge. An abort while the first implement run is in flight adds `aborted` to the change that run opens, when its result is recorded.

## Verification

Standard checks. `go test -race ./internal/core/...` covers the table tests and the property test.

## Risks and open questions

- Many owner actions meet many states. The tables run each row over every claimed state to catch gaps.

## Outcome

Done in `internal/core`: `owner.go` (roles, owner events, the hold, pause, resume, abort, owner escalation, release, close, merge by hand, change closed and reopened), `pick.go` (`Pick`, `ClaimFor`, `Candidate.ApplyMarker`) and `daily.go` (`DailyCount`, `Admit`, `DayReset`, `NextDayBoundary`); `decide.go` routes owner events first, then held events. Still only the standard library and `contract/`. Tests: every §6 owner-action row over every claimed state, the v0.1 FR-12 AC4 rows, FR-13 AC7 for every marker, role and claimed state, FR-2 AC1–AC6, FR-16 AC3–AC4; the property test now mixes in owner events. Coverage 98%.

Deviations: the steps landed in one commit, because the hold is shared by every owner action and the tests share one harness. The plan-proposal rows of FR-12 AC4 are v0.2 (mint and materialise) and are left out. No follow-up issues: D16 and D17 belong to T16 (#21), whose scope now names the spend limit.

Review round 1: R1 (abort during the first implement run now marks the new change, D18), R5 (`ProjectResumed` gives a merging item a new time limit, D12) and R6 (a merge under a hold clears the hold's labels, D18) led to code and tests. R2 is recorded as an owner ruling (D2) and filed on #56; R3 scoped into #21 (D16); R4 cites §6.5 (D17). No pushbacks.
