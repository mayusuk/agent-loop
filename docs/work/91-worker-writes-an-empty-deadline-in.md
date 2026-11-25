# Work plan: Worker writes an empty deadline in input.json, so every stage fails

**Issue:** [#91](https://github.com/mayusuk/agent-loop/issues/91) (bug)
**Branch:** `issue/91-worker-writes-an-empty-deadline-in`
**Base:** `origin/main @ f4771cf` (checked before branching)
**Status:** Ready to ship

## Summary

The worker now writes a real `deadline` in every `input.json`: the run's admitted start plus the time limit the runner enforces. Review inputs also name the change's branch. Before this, every `input.json` broke `contract/v1/input.schema.json`, `agent-loop-claude` refused it, and every implement run failed within seconds. This was found in the first end-to-end run on a real repository (`mayusuk/mdtoc-e2e`).

## Context

- INTEGRATION-SPEC §4 and `contract/v1/input.schema.json`: `deadline` is required and is a `date-time`. `workspace.branch` is required, with `minLength: 1`.
- Root cause 1: `internal/workflows/item.go` clears `RunStage.Deadline` before the stage activity on purpose ("the limit counts from the admitted start", #12 D17). `writeInput` in `internal/workflows/stage.go` wrote `deadline` only when it was set, so it was always `""`.
- Root cause 2: a review's worktree is detached at `review.commit` (INTEGRATION-SPEC §3.3), so `Dir.Branch` is empty, and `writeInput` copied that.
- Why the tests missed both: no test checked the written `input.json` against the schema, and the fake and scripted handlers ignore both fields.

## Approach

- `internal/workflows/stage.go`: `runStage` works out the run's limit once (`timeLimit`, the stage limit cut to any deadline) and its deadline (`now + limit`). The runner gets that limit and `input.json` gets that deadline, so the two always agree.
- `writeInput`: an empty `workspace.branch` (a detached review) is filled with the change's branch (D2).
- Test `TestINT4_InputMatchesTheSchemaWithADeadline` in `internal/workflows/stage_test.go`: it checks every `input.json` of an implement and a review run against the embedded schema, with formats asserted, and checks that the deadline is the runner's time limit after now.

## Steps

- [x] Test that checks the written `input.json` against the schema. It fails on `main` with an empty deadline.
- [x] Write the deadline from the enforced limit.
- [x] Name the change's branch in review inputs.

## Acceptance criteria

- Every `input.json` the worker writes is valid against `contract/v1/input.schema.json`.
- `deadline` is the run's start plus the time limit the runner enforces.

## Decisions

- **D1** [gap] INTEGRATION-SPEC §4 `deadline`, #12 D17: the deadline in `input.json` is the activity's start plus the stage's time limit (cut to the core's deadline if one is set). It is the same limit the runner enforces, so a handler that stops at the deadline is never killed early. Why: #12 D17 drops the core's deadline so the limit counts from the admitted start, but the input still needs a deadline.
- **D2** [ambiguity] INTEGRATION-SPEC §3.3, §4: a review's worktree is detached, but `workspace.branch` names the change's branch, as the schema requires a non-empty branch and §3.3 says "the input names the branch". Handlers must not read it as "the checkout is on this branch" for a review. Why: the schema and §3.3 allow only this reading. The spec should say so in §4.

## Verification

- Before the fix, the new test fails with "jsonschema validation failed" on `/deadline`. After it, it fails on `/workspace/branch` for the review run until D2 was applied.
- The e2e run on `mayusuk/mdtoc-e2e` is repeated with the fixed worker.

## Risks and open questions

None.

## Outcome

Done as planned. Both causes are fixed, and a schema test now covers the worker's `input.json` for implement and review.
