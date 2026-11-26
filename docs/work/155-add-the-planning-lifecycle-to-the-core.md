# Work plan: Add the planning lifecycle to the core

**Issue:** [#155](https://github.com/mayusuk/agent-loop/issues/155) (feature)
**Branch:** `issue/155-add-the-planning-lifecycle-to-the-core`
**Base:** `origin/main @ 462c8fc` (checked before branching)
**Status:** Draft

## Summary

Adds the planning lifecycle to the pure core: `DecidePlan(PlanState, Event, Policy) (PlanState, []Action)` decides every planning edge of PRODUCT-SPEC §6 (mint, proposal, materialise and their escalations) with a decision record per call, and `MintAllowed(ProjectPlanning) (bool, Reason)` gives FR-9 AC1's refusal reasons. The workflows that act on them come in T4 to T7.

## Context

- Plan `docs/plans/2026-11-25-v0-2-minter-materialise-and-multiple.md`, Approach "Planning lifecycle in the core" and task T2.
- PRODUCT-SPEC §6 (planning states and edges), FR-9 AC1, AC5, AC7, FR-10 AC1, FR-12 AC4 (plan-proposal rows), FR-13 AC7, FR-16 AC1, FR-17 AC1 to AC9, FR-18, D33.
- TECHNICAL-SPEC §4 (core model, decision records), §6.1 (`MintWorkflow`, `MaterialiseWorkflow`), §9 (idempotency keys). INTEGRATION-SPEC §4.1 (`run_id`), §5.2 (mint), §6.1 (`proposal`, `plan` records).
- Existing core: `Decide`, `ItemState`, events (`StageResult`, `StageCrashed`, `StageTimedOut`, `DailyLimitReached`, `DayBoundary`, `ChangeMerged`, `ChangeClosed`, `MarkerChanged`), `Role`, `Policy.limits`, `DecisionRecord`. `internal/core` imports only the standard library and `contract/` (not `contract/tasklist`).

## Approach

- `internal/core/plan.go`: planning states as values of `State` (§6 lists them with the item states); `PlanState`, `PlanRun`, `PlanEscalation`; the planning events (`MintRequested`, `MaterialiseResult`, `PlanFixMerged`, `PlanAbandoned`), and `StageResult.Mint`; the planning actions (`RunStage` reused for mint, `Materialise`, `MarkProposal`, `RecordPlan`, `EndPlan`); `DecidePlan`.
- Proposal merge and close reuse `ChangeMerged` and `ChangeClosed` (the proposal is a change), and `needs-owner` removal reuses `MarkerChanged`. Run crashes, timeouts, the daily limit and the day boundary reuse the item events by run ID.
- `DecisionRecord` gains an optional `Plan *PlanBasis` (proposal, plan key, attempt) for planning decisions; item decisions leave it nil.
- `internal/core/mintallowed.go`: `ProjectPlanning`, `Reason`, `MintAllowed`.
- Tests: `plan_test.go` (a table over every §6 planning edge, FR-12 AC4 rows, FR-17 AC8, FR-10 AC1), `mintallowed_test.go` (FR-9 AC1, AC5).

## Steps

- [ ] Planning states, `PlanState`, events, actions and `DecidePlan`, with the edge table tests.
- [ ] `MintAllowed` with its reasons and tests.
- [ ] Outcome and verify.

## Acceptance criteria

- Every planning edge in PRODUCT-SPEC §6 has a table test over `DecidePlan`.
- FR-9 AC1: `MintAllowed` refuses with a distinct reason for each condition, and allows minting while items are blocked, escalated or in review.
- FR-9 AC5: `complete` leads to `complete`, and a later mint from `complete` is allowed.
- FR-17 AC8: a plan fix merged by the owner restarts materialise with a new attempt count, a fix merged by a non-owner escalates again, and a merged change to a plan that isn't in `materialise_escalated` starts nothing (D33).
- FR-12 AC4: each plan-proposal row has a test.
- FR-10 AC1: mint and materialise escalations mark the proposal when there is one, and always notify.
- `internal/core/imports_test.go` still passes.

## Decisions

- **D1** [gap] PRODUCT-SPEC §6 (`materialising --> ready`): a plan whose items were all created is in the planning state `materialised`. §6 names no planning state for it (the edge goes to the items' `ready`). Why: the plan needs a state of its own for the board, for `MintAllowed` and for D33 (a later merged change to it starts nothing).
- **D2** [gap] TECHNICAL-SPEC §4, NFR-5 AC1: planning states are values of the core's `State`, so a planning decision record has the same shape as an item's: `From` and `To` are planning states, `Basis` is empty, and a new optional `Plan` basis holds the proposal, plan key and attempt. Why: §4 defines the record for items only.
- **D3** [gap] INTEGRATION-SPEC §6.5, FR-17 AC6, AC8, FR-12 AC4: planning escalation reasons are `needs_owner`, `retries_exhausted`, `time_limit` and `worker_lost` as for items, plus `invalid_task_list` (FR-17 AC6), `plan_not_approved` (the latest change to the plan wasn't merged by the owner, FR-17 AC8) and `merged_by_non_owner` (a proposal or plan fix merged by a non-owner account, FR-12 AC4). Why: the specs name the causes but not their codes.
- **D4** [gap] FR-17 AC6, AC7, AC8: an invalid task list or a plan not approved escalates at once and uses no retry. Only `failed` is retried. Why: retrying can't fix either, and §6 lists them beside "retries used up" as separate causes.
- **D5** [ambiguity] FR-12 AC4 ("Owner closes a plan proposal without merging"), FR-13 AC7: a close of the proposal by any account records the plan as rejected. Reading: the FR-12 AC4 intro makes closing rows apply to any human, because a close changes tracker facts; FR-13 AC7 limits only merges of a proposal to owner accounts.
- **D6** [ambiguity] FR-13 AC7, FR-12 AC4: a merge of the proposal or a plan fix by any role other than an owner account (another human, a bot, or one of the three identities) is a non-owner merge: it is recorded and sends the plan to, or keeps it in, `materialise_escalated` with reason `merged_by_non_owner`, naming the merge commit. Reading: "a human who is not an owner account" covers every non-owner actor, since only owner merges approve a plan (§4.1).
- **D7** [gap] FR-9 AC1: `MintAllowed` checks, in this order, and reports the first that holds: `project_paused`, `minter_running`, `open_proposal`, `materialise_unfinished`, `daily_limit`. "Another minter running" is a plan in `minting` or the caller's running-minter flag. A plan in `materialise_escalated` counts as an unfinished materialise even when no materialise ever ran (a non-owner merge, PRODUCT-SPEC §14.17 F-81). Plans in `mint_escalated`, `complete`, `plan_rejected`, `plan_abandoned` and `materialised` never refuse. Why: FR-9 AC1 lists the conditions but not their codes or an order when several hold.
- **D8** [gap] FR-9 AC5, §6 (`mint_escalated --> minting`, `complete --> minting`): `MintRequested` is accepted from no state, `mint_escalated` and `complete`, and the plan state continues (its decision numbers and idempotency keys carry on). In any other state it is ignored; the caller checks `MintAllowed` first. Why: the diagram's "owner runs mint again" edges leave the same state.
- **D9** [gap] FR-16 AC1, TECHNICAL-SPEC §13: materialise's attempts and time limit come from `Policy.Stages["materialise"]`, defaulting to 2 attempts and 2 hours like the other stages. Why: FR-16 AC1 gives defaults for agent stages and merge only.
- **D10** [gap] FR-16 AC3, §6: a mint run the daily limit refuses (`DailyLimitReached`) stays in `minting` and waits; the next `DayBoundary` starts it again with the same attempt. There is no planning `waiting_limit` state. Why: §6 has no such state for planning, and FR-16 AC3 only says no agent stage starts.
- **D11** [gap] FR-10 AC1: a mint or materialise escalation emits `MarkProposal` (`needs_owner` on the proposal) when the plan has a proposal, then always `Notify` (event `escalate`, with the reason) and `Wait`. It posts no status comment. When the owner's fix restarts materialise, or the owner abandons the plan, the loop removes `needs_owner` from the proposal. A non-owner fix in `materialise_escalated` escalates again: a new notification, and the marker is set again idempotently. Why: FR-10 AC1 asks for the marker and the notification only, and §5.1 says the loop removes `needs-owner` when the owner resumes.
- **D12** [gap] FR-17 AC9, FR-12 AC4: abandon (`PlanAbandoned`) is accepted only in `materialise_escalated`; elsewhere it changes nothing. A reject and an abandon each emit `RecordPlan` (`rejected` or `abandoned`) for the workflow to write on the proposal or tracking item, and `EndPlan`. Why: FR-17 AC9 says the abandon is recorded but not how, and the record format belongs to T6/T7.
- **D13** [gap] FR-9 AC5, AC7: mint `complete` emits `Notify` (event `complete`) and `EndPlan`; mint `done` without a proposal reference or plan key is `failed` (FR-18 AC3). Why: "the loop reports that the project is complete" has no channel named.
- **D14** [gap] INTEGRATION-SPEC §4.1, TECHNICAL-SPEC §9: a planning run ID is `<workflow id>#<mint id>:<stage>:<attempt>:<start>` and an idempotency key `<workflow id>#<mint id>:<verb>:<seq>`, with the provisional mint ID (plan D6) in the claim number's place. Why: §4.1 defines the run ID for items only.
- **D15** [open] PRODUCT-SPEC §6, FR-10 AC3: a project or deployment pause holding mint and materialise is the workflows' admission check (T5, T6), not a `DecidePlan` event; `MintAllowed` refuses a new mint while paused. Why: not needed in the core; recorded so T5/T6 cover it.

## Verification

Standard checks; `go test -race -count=1 -run 'TestFR9|TestFR10AC1_Plan|TestFR12AC4_Plan|TestFR17|TestPlan' ./internal/core/`.

## Risks and open questions

- T5/T6 may want a different split of the plan state between `MintWorkflow` and `MaterialiseWorkflow`; `PlanState` is one value through the whole lifecycle, which either workflow can hold.

## Outcome

_Filled in before shipping._
