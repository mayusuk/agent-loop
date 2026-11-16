# Work plan: Implement the core stage loop: implement, review, address and next stage

**Issue:** [#10](https://github.com/mayusuk/agent-loop/issues/10) (feature)
**Branch:** `issue/10-implement-the-core-stage-loop-implement`
**Base:** `origin/main @ 9e4f006` (checked before branching)
**Status:** Done

## Summary

Adds the core's pure decision function `Decide(ItemState, Event, Policy) (ItemState, []Action)` to `internal/core`, for the implement, review and address part of the lifecycle: stage results, attempts, time limits and crash reruns, review kinds and stale reviews, round counting and the round allowance, base-update mode, manual-comment replies and rulings, escalation from a stage, and the next-stage rules with both pre-stage checks. Every call returns a decision record. Merge decisions (T6, #11) and owner and supervisor rules (T7, #12) plug into marked extension points.

## Context

- PRODUCT-SPEC §6 (diagram edges for `implementing`, `in_review`, `addressing`, `escalated` from a stage, `next_stage`), FR-3, FR-4, FR-5, FR-6, FR-10 AC2, AC5, AC8, FR-16 AC1, FR-18, NFR-1 AC2, NFR-5 AC1, D16, D31, D36, D41, D42.
- TECHNICAL-SPEC §4 (core model), §6.3 (slots asked by the workflow), §6.5 (time limits, crash reruns, attempts), §8.2 (next-stage order and pre-stage checks), §8.1 cause 3 (base update without an agent).
- INTEGRATION-SPEC §4.1 (`run_id`, attempt), §4.2 (review and address inputs), §5.2 (FR-5 AC2 by the worker), §5.3 (base update), §6.3 (review record, stale), §6.5 (status reason codes).
- `internal/core` imports only the standard library and `contract/` (`imports_test.go`). So core can't use `internal/ports` or `internal/profile`: it has its own `Policy` (profile work plan D1) and gets no `UntrustedText` at all, only references (ports work plan D1). Core reuses `contract.Stage`, `contract.ReviewKind`, `contract.AddressMode`, `contract.Outcome`, `contract.FindingStatus`, `contract.ReplyKind` and the status reason constants.

## Approach

Files in `internal/core`:

- `state.go`: `State` (lifecycle states), `ItemState` (change, current run, rounds used and added, base updates, findings, latest review, manual comments, pending manual rulings, pending review kind, owner-push flag, escalation, block, decision counter), `Clone`.
- `policy.go`: `Policy` (max rounds, resume rounds, blocking threshold, max base updates, per-stage limits) with FR-16 AC1 defaults.
- `event.go`: sealed `Event` interface: `Started`, `StageResult` (with per-stage outcomes the worker reports after posting), `StageCrashed`, `StageTimedOut`, `DailyLimitReached`, `HeadMoved`, `ManualCommentChanged`, `BaseUpdated`, `BaseConflicts`, `BaseUpdateRefused`, `Resumed`, `DependencyClosed`, `BlockRemoved`, `DayBoundary`.
- `action.go`: sealed `Action` interface: `RunStage`, `UpdateBase`, `CheckMerge`, `SetMarker`, `PostStatus`, `Notify`, `Wait`, `RecordDecision`, and `DecisionRecord`.
- `decide.go`: `Decide` dispatch by event and state, decision record. Extension points: `decideMerging` (T6), owner and supervisor events (T7).
- `stages.go`: stage start (pre-stage checks), results, retries, crashes, time limits, escalation, base update.
- `nextstage.go`: the six §8.2 rules.
- `rounds.go`: round counting and rebuilding from review records (FR-6 AC5).

Tests: table tests per diagram edge, per §8.2 rule and pre-stage check, round counting per kind, crash reruns, and a property test that replays random event sequences twice.

## Steps

- [x] Types, policy, events, actions, `Decide` dispatch and decision records.
- [x] Stage results, attempts, time limits, crash reruns, escalation (FR-16 AC1, FR-18).
- [x] Reviews: kinds, stale reviews, rulings, round counting, FR-6 AC3/AC8 escalation.
- [x] Address: FR-5 AC2, manual replies, base-update mode and the agent-free base update.
- [x] Next-stage rules, resume, blocked and waiting_limit exits.
- [x] Property test for determinism.

## Acceptance criteria

- Table tests cover every edge of the PRODUCT-SPEC §6 diagram that touches `implementing`, `in_review`, `addressing`, `escalated` (from a stage) and `next_stage`.
- Each of the six TECHNICAL-SPEC §8.2 rules, and both pre-stage checks, has a test (TestFR10AC2_..., TestFR6AC8_...).
- Tests cover round counting for every review kind, including a stale review keeping its kind (FR-6 AC5, AC6, D36).
- Crash reruns keep the attempt number, and the 4th crash escalates with `worker_lost` (TestFR16AC1_..., NFR-1 AC2).
- `Decide` is deterministic: a property test replays random event sequences twice and gets the same output.

## Decisions

- **D1** [gap] TECHNICAL-SPEC §4, NFR-5 AC1: `Decide` keeps the spec's signature, and every call returns a `RecordDecision` action as its last action, including calls that change nothing. The record names the event, the state before and after, the facts it was based on (head, latest review commit and kind, rounds used and allowed, base updates, blocking finding IDs, uncleared manual comment IDs), the stage result (stage, run ID, outcome), the rule applied, and the actions. Why: the signature is fixed by §4, and a decision with no record would break NFR-5 AC1.
- **D2** [gap] TECHNICAL-SPEC §4 `Policy`: `core.Policy` is core's own type (`MaxRounds`, `ResumeRounds`, `BlockingThreshold`, `MaxBaseUpdates`, `Stages` with `TimeLimit` and `Attempts`), filled by the workflow from `profile.Policy`. A stage with no limits uses FR-16 AC1's defaults (2h, 2 attempts); a zero `BlockingThreshold`, `MaxRounds`, `ResumeRounds` or `MaxBaseUpdates` uses the TECHNICAL-SPEC §13 default. Why: core can't import `internal/profile`.
- **D3** [gap] FR-3 AC3, FR-10 AC2: after a claim (`Started`), the first stage is chosen by the §8.2 rules 2–6, so an item with no change is implemented and an adopted change is reviewed, addressed or merged by its review state. `Started` carries the open change and its review records. Why: the specs give the rules for leaving a wait, and adopting is the same question.
- **D4** [ambiguity] FR-6 AC6: the first review of a change (no earlier non-stale review) is `round`, even when an owner pushed or opened the change. Reading taken: "`round` otherwise (for example the first review)".
- **D5** [ambiguity] FR-6 AC6, TECHNICAL-SPEC §8.2 rule 3: a review that follows an address run or a base update takes that run's kind even after a wait. Rule 3 gives `manual_reply` only when the replies came from a run in `manual` mode. After a `findings` run that also replied to manual comments, the review is `round`. Why: PRODUCT-SPEC wins, and FR-6 AC6 ("`round` after a run for blocking findings, even if it also replied to manual comments") and FR-10 AC5 (a resume after FR-6 AC8 starts "with the review that was refused") need that kind.
- **D6** [ambiguity] FR-6 AC6, FR-12 AC4 owner-push row: an owner push between an address run (or base update) and its review doesn't change that review's kind. An owner push makes the next review `owner_push` only when no address run or base update is waiting for its review. Reading taken: the review is still "right after" the run.
- **D7** [gap] FR-4 AC3: a review result is stale when the worker marks it stale or its commit isn't the change head the core last saw. The replacement review keeps the kind (D36), starts at attempt 1 as a new review (not a retry), and passes both pre-stage checks again.
- **D8** [gap] FR-6 AC7, PRODUCT §14.17 F-80: only non-stale `base_update` reviews count toward `max_base_updates`. A base update is refused, and the item escalated with `base_update_limit`, when that count has reached the limit. Why: F-80's first resolution, matching FR-4 AC3's rule that a stale review is only recorded.
- **D9** [gap] FR-16 AC1: the attempt limit counts every attempt, the first included. A `failed` result (or one treated as failed, FR-18 AC3) below the limit runs the same stage again with the next attempt number, in a new session and with a new deadline. At the limit, the item escalates with `retries_exhausted`. A time-limit stop escalates at once with `time_limit` (FR-18 AC4).
- **D10** [gap] FR-16 AC1, D42, TECHNICAL-SPEC §6.5: the core counts crash reruns per attempt. Crashes 1 to 3 rerun the stage with the same attempt number and a new `run_id`; the 4th escalates with `worker_lost`. A rerun gets a new deadline. Counting restarts with each new attempt.
- **D11** [gap] INTEGRATION-SPEC §4.1: `run_id` is `<workflow id>#<claim>:<stage>:<attempt>:<start>`, where `<start>` numbers every handler start in the claim (1, 2, 3, ...), crash reruns, retries, new reviews, stale replacements and resumed starts included. So every start has its own ID while `attempt` stays the counted attempt. A result, crash, timeout or daily-limit refusal whose `run_id` isn't the running one is ignored and recorded. Why: a late result from an earlier run must not be applied to a later one (review R1).
- **D12** [gap] FR-16 AC3, TECHNICAL-SPEC §6.3, §8.2: the core can't see the daily count, so the second pre-stage check is made by the workflow when it acts on `RunStage` (it asks the project for a unit before starting). A refusal comes back as `DailyLimitReached`, which moves the item to `waiting_limit` and keeps the pending run. At `DayBoundary` the rules choose again, and if they choose the same stage the pending run's attempt and crash counts are kept. `UpdateBase` is not an agent run and needs no unit (TECHNICAL-SPEC §8.1 cause 3).
- **D27** [gap] TECHNICAL-SPEC §9: a marker write's idempotency key names the marker and its value (`<workflow id>#<claim>:marker:<marker>:<true|false>:<seq>`), so two marker writes in one decision, such as clearing `blocked` and setting `needs_owner`, never share a key (review R2).
- **D13** [gap] INTEGRATION-SPEC §6.5 reason codes added for stage escalations: `retries_exhausted`, `time_limit`, `needs_owner`, `blocked_no_dependency`, `dependency_not_done`, `base_update_limit`, plus the existing `round_limit` and `worker_lost`.
- **D14** [gap] FR-18 `blocked`: the worker reports the named dependency's state with the result (`open`, `done`, `not_done`). Open: the item goes to `blocked`. Already closed as done: the next stage is chosen at once. Closed any other way: escalate with `dependency_not_done`. No dependency: escalate with `blocked_no_dependency`.
- **D15** [gap] FR-5 AC2, AC5: the core applies FR-5 AC2 again from the worker's report, where each reply says whether it counts (a `deferred` reply counts only with an open `deferred_to` item). A run that fails AC2 is treated as `failed`. A `base_update` run whose head didn't move is also `failed` (FR-5 AC6).
- **D16** [gap] FR-5 AC1, INTEGRATION-SPEC §4.2: `answer_manual` lists every manual comment that isn't cleared, except empty "changes requested" reviews, including comments whose earlier reply still waits for a ruling. `rule_manual` lists the comments that got a counting reply and that no non-stale review has ruled on since.
- **D17** [gap] FR-4 AC8, FR-4 AC3: manual rulings and finding statuses are applied only from a non-stale review. A stale review's rulings are ignored, and its replies stay waiting for a ruling.
- **D18** [gap] FR-6 AC3, AC8, AC9: after any non-stale review that leaves a blocking finding, the item escalates with `round_limit` when the rounds used have reached the allowance, naming the blocking finding IDs and the uncleared manual comment IDs; otherwise it goes to `addressing`. When a `round` review is refused by the first pre-stage check, the item escalates with `round_limit` and names no finding. The escalation remembers which case it was, so a resume after the first starts with an address run (rule 1) and after the second with the refused review (FR-10 AC5).
- **D19** [gap] FR-10 AC5, INTEGRATION-SPEC §6.5: a resume from a `round_limit` escalation adds `resume_rounds` to the allowance, and its `resume` status record carries `rounds_added`. A resume from any other escalation adds nothing (`rounds_added` 0).
- **D20** [gap] FR-10 AC8: a resume starts the next stage as a new run with attempt 1 and no crash reruns, whatever the stage was before.
- **D21** [gap] Extension points for T6 and T7: entering `merging` emits `CheckMerge{Head}`, and events in `merging` go to `decideMerging`, which in this task records the event and does nothing. The agent-free base update (`beginBaseUpdate`, D41) and the `manual` address mode are implemented here, but only T6's merge causes 3 and 4 start them. Owner pause, abort, release, close and the daily-limit day count are T7's.
- **D22** [open] FR-6 AC5, TECHNICAL-SPEC §10: `Started` rebuilds rounds used, base updates, findings and the latest review from the change's review records (non-stale only), and the round allowance from the resume records' `rounds_added`. Replies waiting for a ruling and the kind of a review waiting after an address run are not in any tracker record the specs define, so a lost workflow can't rebuild them. The reconcile task (T16) has to settle where they come from.
- **D23** [gap] TECHNICAL-SPEC §4: an event that doesn't apply in the current state (for example `DayBoundary` outside `waiting_limit`, or a stage result while no stage runs) changes nothing and is recorded with the rule `ignored`.
- **D24** [gap] FR-12 AC4 owner-push row: a head move by the coder outside a run (for example the agent-free base merge) never makes the next review `owner_push`. Only a push the event marks as an owner's does.

- **D25** [gap] FR-6 AC6, D36: once the core chooses a review's kind, it keeps that kind for every retry, crash rerun, stale replacement, daily-limit restart and resumed start, until a non-stale review completes. Why: D36 keeps the kind across a stale review, and FR-10 AC5 needs the refused review's kind after a resume; one rule covers both.
- **D26** [ambiguity] FR-10 AC2, TECHNICAL-SPEC §8.2 rule 4: "the change's head has no review yet" also holds when an address run or base update finished and its review hasn't completed, even if the run didn't move the head. Why: FR-10 AC2 says the item "continues from the stage after the last completed one", and without this an address run that only replied would be followed by a second address run after a wait (rule 5).

## Verification

Standard checks. `go test -race ./internal/core/...` covers the table tests and the property test.

## Risks and open questions

- D22: rebuilding pending manual replies after a lost workflow is left to the reconcile task.

## Outcome

Done as planned in `internal/core` (`state.go`, `policy.go`, `event.go`, `action.go`, `decide.go`, `stages.go`, `nextstage.go`), standard library and `contract/` only. 87 tests and subtests (table tests per diagram edge, one per §8.2 rule and pre-stage check, round counting per kind, crash reruns, and a 300-seed property test replaying 60 random events twice); statement coverage about 98%.

Deviations: the steps landed in one commit, because the handlers share the decision type and the tests share one harness. Two decisions came up while coding (D25, D26). Diagram edges that need T6 (untrusted commit during a stage, merging exits including `merging --> waiting_limit`) or T7 (pause, abort, owner-added `needs-owner`) are left to those tasks; their entry points are `decideMerging`, `beginBaseUpdate` and `startAddress` (D21).

No follow-up issues filed: D22 (rebuilding pending replies after a lost workflow) belongs to the workflow task T16 already in the plan.
