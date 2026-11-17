# Work plan: Implement worker stage activities

**Issue:** [#20](https://github.com/mayusuk/agent-loop/issues/20) (feature)
**Branch:** `issue/20-implement-worker-stage-activities`
**Base:** `origin/main @ 107d790` (checked before branching)
**Status:** In progress

## Summary

Adds the worker's activities in `internal/workflows`: `RunStage` for implement, review and address (prepare the workspace, issue and revoke the stage's tokens, write a references-only `input.json`, run the handler, validate the result, then post its records as the stage's identity), the agent-free base update of D41, and the adapter activities the core's actions need (markers, status comments with the owner mention, notifications, merge, merge checks, follow-ups, approved commits). The git operations the worker needs (remote head, `merge-tree`, merge commit, fast-forward push, parent and path checks) are added to `internal/workspace`.

## Context

- Plan T15 (`docs/plans/2026-11-15-v0-1-mvp.md`). Specs read: PRODUCT FR-3 AC3, FR-4, FR-5, FR-18, NFR-2 AC1, NFR-8 AC1, §8 item 8; INTEGRATION §3–§6 (handler start, credentials by stage, input, result, base update, records), §8.3; TECHNICAL §6.5, §7.3, §9.
- Builds on: `contract` (`Input`, `ValidateResult`, `MissingReplies`, `Record`), `core` actions and events (`RunStage`, `UpdateBase`, `StageResult`, `BaseUpdated`/`BaseConflicts`/`BaseUpdateRefused`, ...), `ports` (`WorkTracker`, `CodeHost`, `Workspace`, `Runner`, `SecretStore`, `Notifier`), `runners.ReadResult`, `secrets.Run` (per-run token set, revoked on close), `secrets.Redactor`, `notify.Mention`, `workspace.Git`.
- Trust internals (#16) are being built in parallel: the activities only call `CodeHost.Trust` and report coder runs through a small interface.
- Temporal isn't a dependency yet (T16 adds it). The activities are plain methods with context and serialisable arguments, so T16 registers them as they are; heartbeats go through a hook T16 sets.

## Approach

- `internal/workflows/activities.go`: `Activities` holds the ports, the profile values the worker needs, the redactor and a run directory root. Interfaces of its own: `GitOps` (implemented by `workspace.Git`) and `CoderRuns` (records each coder run's start and end for I11, implemented by the trust work).
- `stage.go`: `RunStage(ctx, StageRequest) (StageReport, error)`. One flow for every stage (tokens, workspace, input, run, revoke, validate), then a per-stage `post` step: implement opens or adopts the change; review assigns finding IDs, builds the full findings list from the latest non-stale review record, posts one review (stale when the head moved) and resolves addressed or withdrawn threads; address applies FR-5 AC2 with the open-item check, checks the base-update merge commit, and posts one reply per finding and manual comment.
- `baseupdate.go`: `UpdateBase(ctx, BaseUpdateRequest)`: workspace on the branch, `merge-tree`, one merge commit as the coder, a fast-forward push with the coder worker token, `BaseUpdated`/`BaseConflicts`/`BaseUpdateRefused`.
- `adapter.go`: `SetMarker`, `PostStatus` (plain text, status record, owner mention on escalations), `Notify`, `CheckMerge`, `TrustCheck`, `Merge`, `FileFollowUp`, `ApproveCommits`.
- `internal/workspace/gitops.go`: the git operations, tested against real local repositories.
- Tests use the fake adapter, a fake runner whose "handlers" are Go functions that write result files and push through the fake, and a fake `GitOps` backed by the fake's branches.

## Steps

- [ ] Git operations in `internal/workspace` (remote head, merge-tree, merge commit, push, parents, ancestry, file lines, resolved paths), with tests on real repos.
- [ ] `Activities`, `StageRequest`/`StageReport`, the common stage flow (tokens, workspace, input, run, revoke, validate) and implement (open or adopt).
- [ ] Review: finding IDs, record, status table, stale, approve, resolve threads.
- [ ] Address: FR-5 AC2 re-check, base-update merge commit check, replies.
- [ ] Base update without an agent.
- [ ] Adapter activities: markers, status comments with mention, notify, merge check, trust check, merge, follow-up, approved commits.
- [ ] Outcome, verify, ship.

## Acceptance criteria

- Against the fake adapter: an implement run opens one change, and a rerun adopts it (TestFR3AC3_...).
- A review run posts one review with the status table, and a retried post leaves one review (TestNFR2AC1_...).
- An address run posts one reply per finding.
- A conflict-free base update makes exactly one merge commit and runs no handler. A base update with conflicts runs an address handler in `base_update` mode (TestFR5AC6_...).
- The loop identity's token never reaches a handler (a test checks the environment and the helper, PRODUCT §8 item 8).

## Decisions

None yet.

## Verification

Standard checks. The git operations are tested against real local repositories (`file://` remotes); no test calls GitHub, Claude or Temporal.

## Risks and open questions

- The real Temporal registration and heartbeat wiring is T16's.

## Outcome

_Filled in before shipping._
