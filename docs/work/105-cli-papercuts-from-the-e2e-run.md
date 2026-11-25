# Work plan: CLI papercuts from the e2e run

**Issue:** [#105](https://github.com/mayusuk/agent-loop/issues/105) (bug)
**Branch:** `issue/105-cli-papercuts-from-the-e2e-run`
**Base:** `origin/main @ f2861f7` (checked before branching)
**Status:** Done

## Summary

Fixes the CLI papercuts from the e2e run on mayusuk/mdtoc-e2e: the board's HOST column is now filled for a running stage, the board (and the dry run) say a project is starting instead of printing Temporal's `unknown queryType state`, `help` marks `host check` as v0.3, the dry run says which items count as candidates, and GETTING-STARTED documents a source install.

## Context

- **Dry run without the server (FR-1 AC6, INTEGRATION-SPEC §12.3).** The dry run's candidates are `WorkTracker.Candidates`, "open items with the ready marker" (INTEGRATION-SPEC §8). The candidate list doesn't depend on the server: with and without it, `dryRun` runs the same `Candidates` and `core.Pick`, and `TestFR1AC6_DryRunMakesNoWriteAndStartsNoHandler` already proves that without a server a ready item with an open dependency is listed as `excluded: dependency_open`. Root cause of the e2e observation: before the server run, #4 and #5 didn't yet carry the `ready` label (wf `plan-issues` labels them `status:ready`, bridged by hand later; that is #104), so they weren't candidates. The fix here is to say in the output which items are candidates, so a missing item is explained.
- **Board while starting (FR-11 AC1).** `ProjectWorkflow` registers its `state` query only after its first `DailyRuns` activity, so a query in that window fails with a `serviceerror.QueryFailed` "unknown queryType state". `api.Service.Board` passed the raw message through.
- **`host check` (FR-14 AC4).** PRODUCT-SPEC §12 puts host bootstrap (FR-14 AC4) in v0.3; the help line had no release note, unlike `mint` and `plan`.
- **HOST column (FR-11 AC1, TECHNICAL-SPEC §10 `items.host`, `stage_runs.host`).** Nothing ever set a host: `projection.View.Host` is never filled, and `stage_runs.host` is always empty. Only the worker that runs `RunStage` knows its host, and only once the activity starts.
- **Install (GETTING-STARTED §2).** Shows only an illustrative `brew install`.

## Approach

- `internal/projection`: `SetRunHost(ctx, runID, host)` sets a started run's host. `Board` shows the host of the item's running stage run (not ended) and falls back to `items.host`.
- `internal/workflows`: `Activities.HostName`; `RunStage` records it on the run before the handler starts (best effort: a failure doesn't fail the run). `Projection` gains `SetRunHost`. `cmd/agent-loop/worker.go` sets `HostName` from `os.Hostname()`.
- `internal/api`: `ErrStarting`; `TemporalWorkflows` maps a `QueryFailed` naming an unknown query type to it; `Board` says "project X is starting: try again in a moment".
- `cmd/agent-loop`: help line `host check` gets "(v0.3, FR-14 AC4)"; the dry run's candidate heading names the rule.
- `docs/GETTING-STARTED.md`: a source-install block.

## Steps

- [x] Tests that fail today: board shows the running run's host; board shows "starting"; help marks every unbuilt command with its release.
- [x] Projection `SetRunHost` and board host; worker records its host on each run.
- [x] API `ErrStarting` and board message.
- [x] Help line for `host check`; dry-run candidate heading.
- [x] GETTING-STARTED source install.

## Acceptance criteria

- `agent-loop run --dry-run` without the server explains every candidate, and says that candidates are open items with the ready marker (FR-1 AC6).
- `agent-loop board` while the project workflow is starting says the project is starting, not `unknown queryType state`.
- `agent-loop help` marks `host check` as v0.3, as it does `mint`.
- GETTING-STARTED documents a source install, including PATH.
- The board's HOST column shows the host of a running stage (FR-11 AC1).

## Decisions

- **D1** [ambiguity] FR-1 AC6, INTEGRATION-SPEC §12.3: "every other candidate" is read as every open item with the ready marker (`WorkTracker.Candidates`, INTEGRATION-SPEC §8), with or without the server; open items without the marker aren't listed. The dry run now says so in its heading: "Candidates (open items with the ready marker), in pick order:". Why: the e2e gap was #4 and #5 lacking the `ready` label (#104), not the server; listing every open item would be a product change.
- **D2** [gap] FR-11 AC1, TECHNICAL-SPEC §11: while a project workflow hasn't registered its state query yet, the board's project line reads "state unknown: project X is starting: try again in a moment" (and the dry run reports the same reason). The dry run's Server line gives a reason the board reports for the project (starting, or not running) on its own, e.g. "Server: project X is starting: try again in a moment, so ...", without the "the server isn't running" prefix, which would contradict it (review R1); a failed dial or call still reads "the server isn't running ...", as #22 D9 says. Why: the spec doesn't cover a workflow that runs but can't answer yet; Temporal's raw error means nothing to the owner.
- **D3** [gap] FR-11 AC1, TECHNICAL-SPEC §10: the HOST is the worker's OS host name (`os.Hostname`, the name the `hosts` table already uses), recorded on the stage run when the `RunStage` activity starts. The board shows the host of the item's stage run that hasn't ended, and `-` when no stage runs. A failure to record the host doesn't fail the run. Why: only the worker knows which host picked the run up; the spec names the column but not its source or when it clears.
- **D4** [gap] TECHNICAL-SPEC §11, PRODUCT-SPEC §12: `help` marks `host check` "(v0.3, FR-14 AC4)", matching how `mint` and `plan` show their release. Why: FR-14 AC4 is v0.3.
- **D5** [gap] getting-started guide §2: the source install is `go install ./cmd/agent-loop` from a checkout, with `$(go env GOPATH)/bin` added to PATH; brew stays marked illustrative. Why: packaging isn't decided (the guide says so), and the issue asks for a working path today.

## Verification

Standard checks, plus `GOOS=linux go build ./...` and `CGO_ENABLED=0 GOOS=darwin go build ./...`.

## Risks and open questions

None needing the owner.

## Outcome

All five papercuts are handled. The board's HOST comes from the running stage run, which the worker now stamps with its host name (this also fills `stage_runs.host` for `agent-loop log`). The board and dry run say a starting project is starting. `help` marks `host check` v0.3, with a test that every unbuilt command names its release. GETTING-STARTED has a source install.

Deviation: the dry-run item needed no logic change. Its candidate list doesn't depend on the server, and an existing test already shows a dependency-open item excluded without one; the e2e gap was #4 and #5 lacking the `ready` label (#104). The output now names what a candidate is (D1). No follow-up issues.

Review round 1 (PR #123): R1 (bug) fixed: the dry run's Server line gives a board-reported reason on its own (`boardReason`), so a starting project reads "project X is starting", not "the server isn't running: ..." (D2 updated). R2 (tests) fixed: `TestFR11AC1_FailedHostWriteDoesNotFailTheRun` shows a failing `SetRunHost` leaves the run done, and that no host or no run ID records nothing. No pushbacks. Merged `origin/main`.
