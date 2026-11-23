# Work plan: Prove scenario §11.1 end to end

**Issue:** [#25](https://github.com/mayusuk/agent-loop/issues/25) (feature)
**Branch:** `issue/25-prove-scenario-11-1-end-to-end`
**Base:** `origin/main @ 0d97ec4` (checked before branching)
**Status:** In progress

## Summary

Writes the v0.1 acceptance scenario, PRODUCT-SPEC §11.1 steps 1–9, as tests against the fake adapter, scripted handlers and Temporal's test environment, and fixes what the scenario finds. Adds the live variants behind `AGENT_LOOP_LIVE=1`: the scenario on a GitHub test repository, the workflows on a real Temporal server and on the Compose stack (deferred from #22, review R3), and the trust conformance cases on GitHub (deferred from #16, D12). Records the polling budget for TECHNICAL-SPEC Q4.

## Context

- PRODUCT-SPEC §11.1 (the scenario; "every step runs twice", fake and a real repository, only step 6 differs), §8 item 7 (no credential in logs, tracker text or handler output), NFR-1 AC1 (worker kills), NFR-4 AC2 (inbound blocked), FR-5–FR-8, FR-10, FR-13 AC5–AC7, FR-16 AC3–AC4.
- TECHNICAL-SPEC §8.1 (merge conditions "right before merging"), §9 (idempotency keys), §14 (tests), Q4 (polling budget), T15 (polling).
- Plan T20, D2 (test names `TestScenario11_1_Step<n>_...`), D3 (starter handlers in `mayusuk/agent-loop-handlers`), D5 (live runs need `AGENT_LOOP_LIVE=1`).
- Deferred here: #22 Risks and review R3 (a real Temporal server and the Compose file are exercised only here); #16 D12 (C10, C13, C15 join the live conformance run).
- Builds on the `internal/workflows` test harness (`wf_harness_test.go`, `harness_test.go`), the fake adapter's script methods, `projection.DB.Board`, and the GitHub stub's request log.

## Approach

- `internal/workflows/scenario_harness_test.go`: a scenario harness over `wfHarness`: git and workspaces backed by the fake's commit graph (new `fake.BranchHead`, `IsAncestor`, `Parents`), handlers scripted per item and stage, gates that pass at each merge check unless the item is held, the real SQLite projection (for the board) tee'd with the recorder, taps on every text the worker posts, and the workflow log captured. Handlers echo their tokens into their output, and `noCredentials` checks every log, the projection files and every posted text for any issued token or token shape.
- `internal/workflows/scenario_test.go`: `TestScenario11_1_Step1_...` to `Step9_...`, each its own project run with WIP 2.
- Fixes the scenario needs (D3, D4): the merge check and a review's result carry the manual comments read at that moment; uncounted reviews of one commit get distinct record keys.
- Live: `TestScenario11_1_Live` (GitHub test repository, real server and worker), `TestLiveTemporal_...` (real Temporal and Compose), the live conformance run with the trust cases, and `TestQ4_PollingBudget` against the GitHub stub. `docs/LIVE-SCENARIO.md` documents the setup.

## Steps

- [x] Scenario harness and steps 1–3, with the fixes they need (fresh manual comments, uncounted review keys) and their core tests.
- [x] Steps 4–9, with the board fix step 8 needs.
- [ ] Q4 polling budget measured against the GitHub stub.
- [ ] Live variants: real Temporal and Compose, trust conformance on GitHub, the scenario on a GitHub test repository; `docs/LIVE-SCENARIO.md`.

## Acceptance criteria

- Each step of §11.1 is a named subtest (`TestScenario11_1_Step1_...` through `Step9`) that passes against the fakes in CI.
- No credential appears in any log captured during the scenario (step 8, PRODUCT §8 item 7).
- The live variant is documented in `docs/` with the setup it needs, and is skipped without `AGENT_LOOP_LIVE=1`.

## Decisions

- **D1** [ambiguity] PRODUCT-SPEC §11.1, plan D2: each step is a top-level test `TestScenario11_1_Step<n>_<name>` (the plan's naming), and each runs its own project (WIP 2, one host) seeded with the items the step names and the state it starts from, rather than one run carrying A–D through every step. Why: a step's assertions then don't depend on the timing of the others, and a failure names the step. Where a step needs another item's effect (step 2: A merged first; step 7: a third item waits for D's slot), that item is in the step's run. Owner actions happen at fixed workflow times, and an item's required gate is held pending until the step's owner action, so "before it merges" is deterministic.
- **D2** [gap] §11.1 step 6 on `fake`: the account that is neither the coder nor an owner is a human with write access made a (non-owner) admin for the push and demoted right after, because the fake's loop-branch ruleset refuses everyone else, as GitHub's does. Trust reports the commit with the `admin_not_owner` cause. Step 4's kill is the activity failing after the review was posted and before its result reached the workflow, as in `TestNFR1AC1_WorkerKilledInEveryStage`.
- **D3** [gap] TECHNICAL-SPEC §8.1 ("checks the conditions right before merging"), FR-7 AC9, FR-6 AC3: the merge check (`MergeChecked.Manual`) and a review's result (`StageResult.Manual`) carry every manual comment on the change as read at that moment, and the core records them (with the reopen rule of #21 D13) before deciding. Without this the core decided from the last poll: in step 1 a thread the owner resolved during the manual-reply review was still open at the merge check that followed (an extra address run), and in step 3 the round-limit escalation didn't name the comment left during round 3. A comment added since the last poll now also holds a merge it would have missed.
- **D4** [gap] NFR-2 AC1, #20 D3: the record key of a review that isn't a round gets `:n<seq>`, where seq is the number of such reviews already completed under the claim (`ItemState.UncountedReviews`, `ReviewSpec.Seq`); a crash rerun keeps it. #20 D3 let "a final review run twice on the same commit" return the first review; step 3's second manual-reply review, after an address run that left the head, then reused the first one's `open` ruling and the item looped to an escalation.
- **D5** [gap] PRODUCT-SPEC §8 item 7, step 8: "any log" is checked as the workflow and activity log, every file in the run directories except the handler's own `out/` files, the projection database files, and every text the worker posts (change, review, finding, reply and tracker comment bodies, follow-ups). Every scripted handler echoes its tokens into what it returns, and none may reach those. The `out/` files are what the handler itself wrote; the worker masks them before posting.
- **D6** [open] FR-13 AC7, FR-10 AC1: when a human who isn't an owner removes `needs-owner`, the loop ignores it but doesn't put the label back, so the escalation is no longer visible as a label and the owner resumes by adding and removing it (step 6 does). Whether the loop should restore the label is for the spec.
- **D7** [gap] FR-11 AC1: a decision that only records (an event with no effect, such as a non-owner's label change) keeps the item's next action and wait on the board. Before, it cleared them, so an escalated item showed neither after the next ignored event (step 8).

## Verification

`wf.py verify`, plus `GOOS=linux go build ./...` and `CGO_ENABLED=0 GOOS=darwin go build ./...`.

## Risks and open questions

- The live variants can't run in CI or in this session (no sandbox repository, apps or Compose images here); they are written against the same adapters the stub tests cover and are checked by hand.

## Outcome

_Filled in before shipping._
