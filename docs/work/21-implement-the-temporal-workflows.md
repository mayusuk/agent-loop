# Work plan: Implement the Temporal workflows

**Issue:** [#21](https://github.com/mayusuk/agent-loop/issues/21) (feature)
**Branch:** `issue/21-implement-the-temporal-workflows`
**Base:** `origin/main @ 3a9ef7a` (checked before branching)
**Status:** In progress

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

- [ ] Add the Temporal SDK, the coder-run store, and `StageReport.Cost`.
- [ ] Wire types, worker registration and the supervisor activities.
- [ ] `ItemWorkflow` with tests (claim through merge, escalation and resume, crash reruns).
- [ ] `ProjectWorkflow` and `DeploymentWorkflow` with tests (duplicate start, daily limit wait and reset, spend limit, pause).
- [ ] Replay test against a recorded history.

## Acceptance criteria

- Workflow tests with the Temporal test environment and fake adapters cover: claim through merge, an escalation and resume, and a daily-limit wait and reset.
- A duplicate start of the same item is refused (TestFR2AC4_...).
- A replay test against recorded histories passes (TECHNICAL-SPEC §14).
- Killing the worker during each stage and restarting it loses no completed result and runs no completed stage again (TestNFR1AC1_...).

## Decisions

None yet.

## Verification

`wf.py verify`, plus `GOOS=linux go build ./...` and `CGO_ENABLED=0 GOOS=darwin go build ./...`.

## Risks and open questions

- Temporal SDK v1.45.0 is the newest release whose `go` directive (1.24) doesn't raise the module's Go 1.25.0.

## Outcome

_Filled in before shipping._
