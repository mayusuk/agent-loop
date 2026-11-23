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

- [ ] Scenario harness and steps 1–3, with the fixes they need (fresh manual comments, uncounted review keys) and their core tests.
- [ ] Steps 4–9.
- [ ] Q4 polling budget measured against the GitHub stub.
- [ ] Live variants: real Temporal and Compose, trust conformance on GitHub, the scenario on a GitHub test repository; `docs/LIVE-SCENARIO.md`.

## Acceptance criteria

- Each step of §11.1 is a named subtest (`TestScenario11_1_Step1_...` through `Step9`) that passes against the fakes in CI.
- No credential appears in any log captured during the scenario (step 8, PRODUCT §8 item 7).
- The live variant is documented in `docs/` with the setup it needs, and is skipped without `AGENT_LOOP_LIVE=1`.

## Decisions

- **D1** [ambiguity] PRODUCT-SPEC §11.1, plan D2: each step is a top-level test `TestScenario11_1_Step<n>_<name>` (the plan's naming), and each runs its own project (WIP 2, one host) seeded with the items the step names and the state it starts from, rather than one run carrying A–D through every step. Why: a step's assertions then don't depend on the timing of the others, and a failure names the step.

## Verification

`wf.py verify`, plus `GOOS=linux go build ./...` and `CGO_ENABLED=0 GOOS=darwin go build ./...`.

## Risks and open questions

- The live variants can't run in CI or in this session (no sandbox repository, apps or Compose images here); they are written against the same adapters the stub tests cover and are checked by hand.

## Outcome

_Filled in before shipping._
