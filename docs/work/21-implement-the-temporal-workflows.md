# Work plan: Implement the Temporal workflows

**Issue:** [#21](https://github.com/mayusuk/agent-loop/issues/21) (feature)
**Branch:** `issue/21-implement-the-temporal-workflows`
**Base:** `origin/main @ 3a9ef7a` (checked before branching)
**Status:** Done

## Summary

Adds the three v0.1 Temporal workflows in `internal/workflows`: `DeploymentWorkflow` (the deployment pause), `ProjectWorkflow` (polling, reconcile, picking and claiming, WIP, the daily agent-run and spend limits and their reset) and `ItemWorkflow` (one item's lifecycle driven by `core.Decide`, running the T15 activities). It also adds the supervisor activities, the worker registration with task queues and worker versioning, and a SQLite store for coder runs behind the `CoderRuns` interface.

## Context

- TECHNICAL-SPEC §6.1–§6.5 (workflows, supervisor loop, item loop, task queues, time limits), §9 (idempotency), §10 (projection), §14 (workflow and replay tests); PRODUCT-SPEC FR-2, FR-10 AC3, FR-12 AC1, FR-16 AC1, AC3, AC4, NFR-1.
- T15 (#20, PR #61) built the activities as plain methods on `workflows.Activities`, left the Temporal heartbeat hook to this task (its D1), builds the `-<n>` branch from `core.RunStage.Branch` (its D19), and defined `CoderRuns` (its D6).
- T7 (#12) left to this task: counting handler spend toward `limits.daily_spend` (its D16), the stage time limit as the `RunStage` StartToCloseTimeout (D17), and `core.ProjectResumed` to merging items when a pause ends (D12).
- T6 (#11) left: filling `HeadMoved.Commits` (its D6) and what happens after the loop's own merge (its D13).
- `internal/core` gives `Decide`, `Admit`, `DayReset`, `Pick` and `ClaimFor`; the project and item workflows only call them.

## Approach

- `internal/workflows/wire.go`: signal, query and queue names, and the serialisable signal envelopes (`ItemSignal` wraps every core event the project routes).
- `item.go`: `ItemWorkflow`. A loop takes the next event (signal, stage outcome, recheck timer), calls `core.Decide`, and runs each action. Adapter actions are activities on the `core` queue with retries. `RunStage`, `UpdateBase`, `CheckMerge` and `Merge` run in a coroutine so signals keep arriving: trust check, admission from the project, then the activity; the outcome comes back as an event. `StopRun` cancels the coroutine. Continue-as-new after 2,000 decisions while nothing runs.
- `project.go`: `ProjectWorkflow`. Poll timer, supervisor timer, day timer. Each poll routes events to item workflows, then picks and claims; each supervisor pass reconciles and continues as new. Admission requests come in as signals and are answered by signal. Spend reports add to the day's spend.
- `deployment.go`: `DeploymentWorkflow`: the deployment pause, pushed to registered projects.
- `supervisor.go`: activities for loading an item's facts for `core.Started`, polling and normalising events, building candidates, claiming, escalating dependencies, reconciling, closing items, and writing the projection.
- `worker.go`: registration, task queue names (`cap:<sorted caps>`, `core`), worker versioning options, the Temporal heartbeat hook.
- `internal/projection/coderruns.go`: the `coder_runs` table and a `CoderRuns` implementation, with a query for the runs live in a time window (for #16's pusher attribution).
- `stage.go`: `StageReport.Cost` from a valid result's `usage.cost`.

## Steps

- [x] Add the Temporal SDK, the coder-run store, and `StageReport.Cost`.
- [x] Wire types, worker registration and the supervisor activities.
- [x] `ItemWorkflow` with tests (claim through merge, escalation and resume, crash reruns).
- [x] `ProjectWorkflow` and `DeploymentWorkflow` with tests (duplicate start, daily limit wait and reset, spend limit, pause).
- [x] Replay test against a recorded history.

## Acceptance criteria

- Workflow tests with the Temporal test environment and fake adapters cover: claim through merge, an escalation and resume, and a daily-limit wait and reset.
- A duplicate start of the same item is refused (TestFR2AC4_...).
- A replay test against recorded histories passes (TECHNICAL-SPEC §14).
- Killing the worker during each stage and restarting it loses no completed result and runs no completed stage again (TestNFR1AC1_...).

## Decisions

- **D1** [gap] FR-16 AC3, TECHNICAL-SPEC §10: the day's spend lives only in `ProjectWorkflow` state (it survives continue-as-new). Unlike the run count, it isn't rebuilt from `stage_runs` after a lost workflow, because `stage_runs` has no cost column. Why: §10 names only the count's rebuild.
- **D2** [gap] FR-16 AC3, INTEGRATION-SPEC §5.1, #12 D16: only a valid result's `usage.cost` counts (`StageReport.Cost`); an invalid result, a crash or a timeout adds nothing. The item signals the cost to the project when the run ends. The currency isn't checked. Reaching `limits.daily_spend` refuses agent stages and picking exactly as the run limit does (`waiting_limit`, one owner notice a day, reset at 00:00).
- **D3** [gap] INTEGRATION-SPEC §9.8 step 6, #20 D6: `CoderRuns` is implemented by `projection.CoderRuns` on a new `coder_runs` table (run, project, item, start, end, head). `LiveCoderRuns(project, from, to)` returns the runs live in a poll window, for #16's pusher attribution. The table is added to schema version 1, since no database has shipped.
- **D4** [ambiguity] TECHNICAL-SPEC §6.3 "with an Update": Temporal can't send an Update from workflow code, so the item sends an `admit` signal and the project answers with an `admission` signal carrying the run ID. Read as a request and reply, not the Temporal Update API.
- **D5** [gap] FR-10 AC3, #12 D12: every stage start asks for admission: agent stages for a daily unit, and the base update, merge check and merge (no unit) so a pause holds them too. A paused project holds the request and answers it when the project and deployment are both unpaused; it then signals `ProjectResumed` to every running item (the core uses it only while merging).
- **D6** [gap] FR-10 AC3, TECHNICAL-SPEC §6.1: a `ProjectWorkflow` starting with no state registers with `deployment` by signal; the deployment answers with its pause and signals every later change to each registered project. A deployment workflow that isn't running leaves projects unpaused.
- **D7** [gap] TECHNICAL-SPEC §6.3 worker versioning: workers use worker deployment `agent-loop` with the build ID. `ItemWorkflow` is pinned to its build; `ProjectWorkflow` and `DeploymentWorkflow` auto-upgrade, because they run forever and continue as new each reconcile, so a change to them must be patched with `workflow.GetVersion`.
- **D8** [gap] FR-12 AC1, TECHNICAL-SPEC §6.2 step 3: the reconcile re-reads each running item and signals `ItemClosed` for one closed meanwhile. It can't start workflows for claimed items that have none, since no port lists items with `in_progress` (`Candidates` lists ready items only).
- **D9** [gap] D44, INTEGRATION-SPEC §6.2: `Started.ClosedChanges` on a new claim is n−1 of the open change's branch; with no change on a loop branch, it is the number of consecutive loop branches `<prefix><item>`, `-2`, … already on the remote. Why: no port lists closed changes.
- **D10** [gap] TECHNICAL-SPEC §6.2 step 2: feed events for items with no running workflow are dropped; picking reads unclaimed items afresh each poll, and an owner-opened change is adopted by implement (FR-3 AC3).
- **D11** [gap] FR-18 AC1, #20 D14: when an item closes, each running item it blocks gets `DependencyClosed`, done when its change merged or an owner closed it as completed.
- **D12** [gap] FR-13 AC5, AC6, #11 D6: `HeadMoved.Commits` for an `Unknown` pusher are the commits `CodeHost.Trust` names (those since the last trusted head). A coder or owner push carries none.
- **D13** [gap] FR-7 AC9: a review, thread or dismissal event reads every manual comment on the change and sends one `ManualCommentChanged` each. `Reopened` is set by the item workflow when a comment a human had cleared is no longer cleared, since the port's event doesn't name the thread.
- **D14** [gap] #11 D13, FR-7: after the loop's own merge (and any follow-up), the workflow clears `in-progress`, closes the item if the tracker didn't (keys `<wf>#<claim>:merged:marker` and `:merged:close_item`), frees the WIP slot and ends.
- **D15** [ambiguity] #10 D22, FR-10 AC5: `Started.RoundsAdded` sums `rounds_added` over every resume record on the item, from every claim.
- **D16** [gap] TECHNICAL-SPEC §6.5, #12 D17: `RunStage` runs with StartToCloseTimeout = the stage limit, a 2-minute heartbeat timeout, one attempt, and the core's deadline cleared, so the limit counts from the admitted start. A StartToClose timeout or the runner's own timed-out report is `StageTimedOut`; any other activity error is `StageCrashed`. Stopping a run cancels the activity and waits for it to clean up.
- **D17** [gap] TECHNICAL-SPEC §10 `stage_runs`: a run's result is its outcome, or `timed_out`, `worker_lost` (§6.5) or `stopped` (cancelled by a close or a merge by hand).
- **D18** [gap] FR-16 AC3, AC4: project notices use keys `project/<p>:daily_limit:<day>` and `project/<p>:daily_reset:<day>`. The 00:00 timer wakes the running items refused that day; the reset notice is sent when any item waited or the limit was reached.
- **D19** [gap] TECHNICAL-SPEC §6.1: an item workflow is a child of the project run with ID reuse *allow duplicate* and parent-close policy *abandon*, so it outlives the project's continue-as-new; the project tracks items by the `item_ended` signal. A child start whose ID is running is refused (`ErrItemRunning`, FR-2 AC4). Claiming is two activities (`PlanClaim` reads the version and claim number, `Claim` writes), so a retried claim reuses its key.
- **D20** [open] FR-13 AC7: candidates take the item's current markers; a ready label added by a non-owner isn't filtered, since `Item` doesn't say who set a marker. The GitHub adapter would need to report it.
- **D21** [open] #10 D22: replies waiting for a ruling and the next review kind aren't rebuilt for a new claim; `Started` carries only what the records hold.

## Verification

`wf.py verify`, plus `GOOS=linux go build ./...` and `CGO_ENABLED=0 GOOS=darwin go build ./...`. The histories in `internal/workflows/testdata/histories/` were recorded on a local Temporal dev server with `AGENT_LOOP_LIVE=1 AGENT_LOOP_RECORD=1 go test -run TestT16_RecordHistories ./internal/workflows/`; CI only replays them.

## Risks and open questions

- Temporal SDK v1.45.0 is the newest release whose `go` directive (1.24) doesn't raise the module's Go 1.25.0.

## Outcome

Done as planned: the three workflows, the supervisor activities, worker registration (queues, versioning, heartbeat hook), the coder-run store, spend counting, and tests for every acceptance criterion (`TestFR2AC5_ClaimThroughMerge`, `TestFR10AC2_EscalationAndResume`, `TestFR16AC4_DailyLimitWaitAndReset`, `TestFR16AC3_DailySpendLimitWaitAndReset`, `TestFR2AC4_DuplicateStartRefused`, `TestT16_ReplayRecordedHistories`, `TestNFR1AC1_WorkerKilledInEveryStage`).

Deviations: `UpdateBase` now reads the change's branch when the request names none (the workflow doesn't hold branch names), and `SetMarker` puts change markers on the change. The CLI and server wiring that start workers and workflows is T17 (#22). No follow-up issues filed: D20 and D21 go to the spec issue.
