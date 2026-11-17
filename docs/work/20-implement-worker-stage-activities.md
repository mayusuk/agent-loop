# Work plan: Implement worker stage activities

**Issue:** [#20](https://github.com/mayusuk/agent-loop/issues/20) (feature)
**Branch:** `issue/20-implement-worker-stage-activities`
**Base:** `origin/main @ 107d790` (checked before branching)
**Status:** Done

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

- [x] Git operations in `internal/workspace` (remote head, merge-tree, merge commit, push, parents, ancestry, file lines, resolved paths), with tests on real repos.
- [x] `Activities`, `StageRequest`/`StageReport`, the common stage flow (tokens, workspace, input, run, revoke, validate) and implement (open or adopt).
- [x] Review: finding IDs, record, status table, stale, approve, resolve threads.
- [x] Address: FR-5 AC2 re-check, base-update merge commit check, replies.
- [x] Base update without an agent.
- [x] Adapter activities: markers, status comments with mention, notify, merge check, trust check, merge, follow-up, approved commits.
- [x] Outcome, verify, ship.

## Acceptance criteria

- Against the fake adapter: an implement run opens one change, and a rerun adopts it (TestFR3AC3_...).
- A review run posts one review with the status table, and a retried post leaves one review (TestNFR2AC1_...).
- An address run posts one reply per finding.
- A conflict-free base update makes exactly one merge commit and runs no handler. A base update with conflicts runs an address handler in `base_update` mode (TestFR5AC6_...).
- The loop identity's token never reaches a handler (a test checks the environment and the helper, PRODUCT §8 item 8).

## Decisions

- **D1** [gap] TECHNICAL-SPEC §6.5, T16: the activities are plain Go methods on `workflows.Activities` with serialisable arguments; Temporal isn't imported yet. Heartbeats go through a `Heartbeat` hook (default every 30 s) that T16 sets to Temporal's activity heartbeat. Why: Temporal arrives with T16.
- **D2** [gap] TECHNICAL-SPEC §9, INTEGRATION-SPEC §4.1: the keys of the records a stage run writes are its `run_id` (which holds the workflow ID and the claim number) plus a verb: `<run_id>:open_change`, `:review`, `:review:F<n>` (each finding), `:resolve:F<n>`, `:reply:F<n>`, `:manual_reply:MC-<id>`, `:depend`. The adapter activities pass on the core action's key; the base update uses `UpdateBase.Key` as its coder run ID.
- **D3** [gap] NFR-2 AC1, NFR-1 AC1, TECHNICAL-SPEC §9, INTEGRATION-SPEC §4.1: record keys hold no run ID, since a crash rerun gets a new one. Every key is `<workflow id>#<claim>:` plus the decision's parts: a review is `review:<kind>:r<round>:<commit>`; a reply is `reply:<mode>:after<n>:<finding>` (and `manual_reply:…:<comment>`), where n is the number of review records on the change when the run posts; `resolve:<finding>`, `depend:<item>`, `open_change:<branch>`. A crash rerun sees the same claim, round, commit and review records, so it finds what the interrupted run posted and, for review, runs no handler. A new round, a new commit or a new review gives new keys. A final review run twice on the same commit under the same claim returns the first review, which NFR-2 AC1 allows. Review round 1 (R1) changed this from an open gap.
- **D4** [gap] TECHNICAL-SPEC §6.5: `RunStage` itself is never retried, so each record write inside it is retried up to 3 times, 1 s apart, on `ErrTransient`. Its idempotency key makes a retry after a write that landed leave one record.
- **D5** [gap] TECHNICAL-SPEC §7.3, INTEGRATION-SPEC §3.4, §9.1: token scopes. Handler read token (`GH_TOKEN`, every stage): `contents`, `metadata`, `issues`, `pull_requests` read. Coder handler push token: the same with `contents: write`. Coder worker token (base merge only): `contents: write`, `pull_requests: write`, `issues`, `metadata` read. Both handler tokens are issued fresh per run (runner D18) and revoked as soon as the handler ends, before anything is posted.
- **D6** [gap] INTEGRATION-SPEC §9.8 step 6 (I11): the worker reports each coder run through a `CoderRuns` interface (`Started`, `Ended` with the head it left), implemented by the trust work (#16). A run starts before its coder tokens are issued and ends after they are revoked, so no coder token is live outside a recorded run. The head at the end is the branch's remote head.
- **D7** [gap] FR-4 AC7: whether branch protection requires an approval is a worker setting (`Config.RequireApproval`) filled by the caller from the code host. A review approves only when it is set, the review isn't stale and no open or partially addressed finding is at or above the threshold. Why: no port reports it.
- **D8** [gap] INTEGRATION-SPEC §5.2 review, §6.3: the finding state a review starts from is the latest non-stale review record on the change; new IDs continue after the highest ID in any review record, stale ones included, so an ID is never reused. A ruled ID that no record holds is kept with its ruling. The record's `round` is the core's `ReviewSpec.Round`. New findings' `comment` is left empty in the record (the thread doesn't exist before the post); the outcome gives the core each thread read back after posting.
- **D9** [gap] INTEGRATION-SPEC §5.2 review: a new finding's title is posted as the bold first line of its body (`FindingPost` has no title). Rulings' and manual rulings' `note_file` texts are appended to the review summary as `**F5** (partially_addressed): ...`.
- **D10** [gap] FR-4 AC3, §6.3: a stale review resolves no threads and never approves, since its statuses aren't used.
- **D11** [gap] §5.2 address: `done.head` must be the branch's remote head (as for implement). In `base_update` mode the head must have two parents, the first the input head, and the input `base_tip` must be an ancestor of the second.
- **D12** [gap] §6.1 `manual_reply`, I8: a reply to a finding with no thread, and a manual reply to a review body, is a change comment whose first line is `**Reply to <ID>** (<kind>)`. Why: the port gives no review URL to link.
- **D13** [gap] INTEGRATION-SPEC §5.2 review: `review.resolved_paths` for a `base_update` review is worked out by the review run itself from the reviewed commit: when it has two parents, the paths that conflict when they are merged without a working tree, plus every path whose content differs from that merge; empty after a clean merge. So no state passes from the address run.
- **D14** [gap] FR-18 `blocked`, core D14: a named dependency is first added as the item's dependency (`AddDependency`), then its state read from `Dependencies`: done when its change merged or an owner closed it as completed, open while open, otherwise not done. A dependency that doesn't exist is not done.
- **D15** [gap] FR-10 AC4, FR-18: the handler's question (`needs_owner`) or reason (`blocked`) is redacted and saved as `detail.md` in the run directory; `StageReport.DetailFile` names it, and `PostStatus` quotes it under "The handler wrote:". Why: untrusted text can't go into workflow history.
- **D16** [gap] INTEGRATION-SPEC §6.5, FR-10 AC1: status comment text. An escalation starts with the owner mentions (`@login` each), says "agent-loop needs you: <cause>. Stage: <stage>.", lists findings, manual comments, commits and gates, and ends "When it is settled, remove `needs-owner` to resume the item." The cause texts for each reason code are in `adapter.go`. Other events have no mention.
- **D17** [gap] FR-8 AC1, AC5: the follow-up's title is "Follow-up: findings left open on change <ref>"; its link column is the change URL, plus "(thread <id>)" when the finding has a thread, since the port gives no thread URL. The comment on the original item carries a `followup` record with the key `followup:<change>:comment`. The type defaults to `type:feature`.
- **D18** [gap] INTEGRATION-SPEC §5.3: the base merge commit's message is "Merge <base> into <branch>". A retry that finds the branch head is a merge whose first parent is the expected head and which contains the base reports `BaseUpdated` with it. A head that already contains the base reports `BaseUpdated` with no push.
- **D19** [gap] INTEGRATION-SPEC §3.3, D44: the branch is the change's branch when there is a change; otherwise the activity builds it from `core.RunStage.Branch`: `<branch_prefix><item>` for n <= 1, `<branch_prefix><item>-<n>` for n >= 2. `StageRequest.Branch` is gone. The change record's `n` comes from the branch name. Review round 1 (R2) moved this from T16 to here.
- **D20** [gap] FR-18 AC3: a handler the runner refuses to start (for example an unsandboxed handler on `local`) is a `failed` result; a workspace or token failure, or a cancelled context, is an activity error.

## Verification

Standard checks. The git operations are tested against real local repositories (`file://` remotes); no test calls GitHub, Claude or Temporal.

## Risks and open questions

- The real Temporal registration and heartbeat wiring is T16's.

## Outcome

Built as planned: `internal/workflows` holds `RunStage` (implement, review, address), `UpdateBase` and the adapter activities; `internal/workspace/gitops.go` holds the worker's git operations. Tests run against the fake adapter, a fake runner and real local git repositories, and the loop-token test runs the real local runner with a test-binary handler.

Deviations: steps 2–6 landed in one commit, not one each. The status table itself is built by the adapter from the review record (GitHub adapter); the tests check the record lists every finding. Follow-ups: none for T16 from D3 or D19.

Review round 1: R1 (crash-rerun keys) and R2 (`-<n>` branch) were both fixed; no pushbacks. D3 and D19 were rewritten to match.
