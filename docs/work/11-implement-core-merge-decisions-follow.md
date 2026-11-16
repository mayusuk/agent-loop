# Work plan: Implement core merge decisions, follow-ups and trust actions

**Issue:** [#11](https://github.com/mayusuk/agent-loop/issues/11) (feature)
**Branch:** `issue/11-implement-core-merge-decisions-follow`
**Base:** `origin/main @ 6855629` (checked before branching)
**Status:** Done

## Summary

Fills the merge extension point of `internal/core`: the FR-7 merge conditions checked in the TECHNICAL-SPEC §8.1 order, one cause at a time, with the merge pinned to the reviewed head and the `NotMergeable` re-check (I15). A merge files the FR-8 follow-up with finding references only. The trust decisions of FR-13 AC5 and AC6 run before implement, address and merge: an untrusted change escalates, the owner's resume approves exactly the named commits, and a fork is never approved.

## Context

- PRODUCT-SPEC §6 (the `merging` edges, untrusted-commit edges from a stage, `merged --> [*]`), FR-7 AC1–AC10, FR-8 AC1–AC5, FR-13 AC5, AC6, D17, D40, D41, D43.
- TECHNICAL-SPEC §8.1 (causes 1–6), §8.3 (pre-stage trust check), §9 (idempotency keys).
- INTEGRATION-SPEC §5.3 (base update), §6.5 (reason codes), §6.6 (follow-up item and its key), §8.3 (`MergeOutcome`, I15), §9.6 (manual comments, empty review), §9.7 (gates and merge), §9.8 (trust, approvals, pusher).
- T5 (#10, `docs/work/10-implement-the-core-stage-loop-implement.md`): `Decide`, the decision record and the hooks `decideMerging`, `enterMerging`, `beginBaseUpdate` and `startAddress` (its D21). `internal/core` imports only the standard library and `contract/`, so the trust report and merge outcome are core's own types that mirror `internal/ports`.
- T7 (#12) owns owner actions, including a merge by hand (FR-12 AC4), so it is not done here.

## Approach

- `trust.go`: `TrustReport` (causes and commits, as the adapter reports them) and its `Check(approved)` verdict (untrusted, fork, commits not approved, reason code). The `TrustRefused` handler for the pre-stage check, the escalation helper, the untrusted push seen during a review, and owner approval on resume.
- `merge.go`: `MergeChecked` (the facts the workflow read for `CheckMerge`) and `MergeResult` events, the `Merge`, `FileFollowUp` and `ApproveCommits` actions, `decideMerging` with the §8.1 order, the merge-stage deadline, the `NotMergeable` flag, and the follow-up.
- `state.go`, `event.go`, `action.go`, `decide.go`, `stages.go`: new state (approved commits, unknown pushes, merge check), `Started.ApprovedCommits`, `RunStage.Trust`, `CheckMerge.Recheck`, `PostStatus.Commits` and `Gates`, new reason codes, the hook calls.
- Tests: `merge_test.go` (one per §8.1 cause chosen first, pinning, I15, AC9, FR-6 AC9, FR-8) and `trust_test.go` (FR-13 AC5, AC6), plus the new events in the property test.

## Steps

- [x] Trust: verdict, pre-stage refusal, untrusted push during review, approval on resume, never a fork (FR-13 AC5, AC6).
- [x] Merge: §8.1 order, pinned merge, `MergeOutcome` handling with the I15 re-check, merge-stage time limit (FR-7).
- [x] Follow-up on a loop merge (FR-8).
- [x] Property test covers the new events.

## Acceptance criteria

- Each cause in TECHNICAL-SPEC §8.1 has a test that it is chosen first when several hold (TestFR7AC6_...).
- The merge is pinned to the reviewed head (TestFR7AC5_...).
- A manual approval never allows a merge, and the round limit never overrides an uncleared manual comment (TestFR7AC9_..., TestFR6AC9_...).
- Follow-up content contains no titles or reviewer text (TestFR8AC1_...).
- A resume after an untrusted-commit escalation approves exactly the named commits, and a later untrusted commit escalates again (TestFR13AC6_...).

## Decisions

- **D1** [gap] FR-7, TECHNICAL-SPEC §8.1: the merge check is two steps. `CheckMerge` asks the workflow to read the facts, and it answers with one `MergeChecked` event: the change head and its pusher, the trust report, whether the head contains the base tip, and the required gates. Manual comments are not in it: the workflow sends `ManualCommentChanged` for anything new it saw first, because the core holds the clearing from rulings and reopens (FR-7 AC9). Why: core is pure and the conditions are "checked right before merging".
- **D2** [gap] FR-13 AC5, TECHNICAL-SPEC §8.3: the pre-stage trust check before implement (when the item has a change) and address is made by the workflow when it acts on `RunStage`, like the daily limit (#10 D12). `RunStage.Trust` carries the approved commits, the workflow applies `TrustReport.Check`, and an untrusted change comes back as `TrustRefused` instead of a start. The trust check comes before the daily-limit check. The conflict-free base update (D41) has no trust check of its own, because the merge check that chose it just checked trust.
- **D3** [gap] FR-13 AC5, AC6: a trust report is untrusted when it names a fork, or names at least one commit the owner hasn't approved. A report with causes but every named commit approved is trusted. Why: approvals apply to commits (AC6), and the report lists the commits its causes make untrusted (INTEGRATION-SPEC §9.8 step 3).
- **D4** [gap] INTEGRATION-SPEC §6.5: an untrusted-change escalation takes its reason from the report's causes in this order: `fork_change` (new code) for a fork, `branch_outside_prefix`, `untrusted_commit` for a ruleset or admin cause, then `pusher_unknown`. The status record names the commits that aren't approved.
- **D5** [gap] FR-13 AC6, INTEGRATION-SPEC §9.8 step 4: resuming an untrusted-change escalation that isn't a fork writes an `approved_commits` record (`ApproveCommits`, key `<workflow id>#<claim>:approve:<seq>`) with exactly the escalation's commits, then chooses the next stage. Resuming a fork escalation approves nothing, so the next trust check escalates again. `Started` rebuilds the approved commits from those records.
- **D6** [ambiguity] FR-13 AC5 "an untrusted commit found while a review runs": it is a head move with pusher `Unknown` while the item is `in_review`. When that review's result arrives, whatever its outcome, the item escalates with `pusher_unknown` naming those commits, and the result is not applied. Reading taken: the review runs to its end, then the escalation replaces whatever the result would have done.
- **D7** [gap] FR-7 AC6, I15: a `HeadMoved` merge outcome checks the conditions again from the top instead of going straight to `in_review`, so a push by someone else meets cause 1 first. A `NotMergeable` outcome checks again too, and if causes 1 to 4 don't hold then, the item escalates with `merge_refused` (cause 5), even if a gate is pending by then. A `Failed` outcome escalates with `merge_failed`. A failed gate escalates with `gate_failed` naming the gates.
- **D8** [gap] FR-7 AC6 cause 6, FR-16 AC1: entering `merging` starts a merge-stage deadline from the merge stage's time limit (default 2h). A check with a pending or missing gate asks for another check after the poll interval (`CheckMerge.Recheck`), and escalates with `time_limit` (stage `merge`) once the check's time reaches the deadline. Each entry to `merging` starts a new deadline.
- **D9** [gap] FR-7 AC2: a blocking finding at the merge check (for example after the profile's threshold was lowered) sends the item to `addressing` in `findings` mode. It is checked right after cause 2, because it is about the reviewed head. Why: §8.1 lists no cause for it, since the review that sent the item to `merging` had none.
- **D10** [gap] FR-7 AC9, D43: cause 4 sends the item to `addressing` in `manual` mode when at least one uncleared manual comment isn't an empty "changes requested" review. When every uncleared one is empty, it escalates with `empty_review_needs_dismissal`, naming them.
- **D11** [gap] FR-8 AC1, AC3, INTEGRATION-SPEC §6.6: on the loop's own `Merged` outcome with unresolved findings (open or partially addressed), the core emits `FileFollowUp` with the key `followup:<change>` and one row per finding: ID, score, status, and the code-host comment ID the workflow turns into the link. The workflow takes the type and status from the profile, links the item and change, and comments on the item (FR-8 AC2, AC5). The rows are a struct with only those four fields, so no text can be put in them.
- **D12** [open] FR-8 AC4, FR-12 AC4: no follow-up after a merge by hand. The core files one only from its own merge outcome, and the hand-merge event is T7's (#12), which must not call it.
- **D13** [open] FR-7: after `Merged` the item is `merged` and every later event is ignored. Removing the loop's markers and ending the workflow after a merge isn't settled by the core specs. The workflow task (T16) decides it.

## Verification

Standard checks. `go test -race ./internal/core/...` covers the table tests and the property test.

## Risks and open questions

- D12 and D13 are left to T7 and T16.

## Outcome

Done as planned in `internal/core`: `trust.go` and `merge.go` are new, and `state.go`, `event.go`, `action.go`, `decide.go` and `stages.go` gained the new fields, reason codes and hook calls (`enterMerging` moved to `merge.go`). It still imports only the standard library and `contract/`. New tests: `merge_test.go` (each §8.1 cause chosen first when every later one also holds, the AC2 guard, both cause 4 forms, the merge-stage deadline, pinning, the `HeadMoved`, `NotMergeable` and `Failed` outcomes, AC9 and FR-6 AC9 loops, FR-8 AC1, AC3, AC4) and `trust_test.go` (the verdict table, the pre-stage refusal for implement and address, the untrusted push during a review, approvals on resume, forks). The property test now also replays `TrustRefused`, `MergeChecked`, `MergeResult` and unknown pushers. Statement coverage is 99.8%.

Deviations: the four steps landed in one commit, because trust and merge share the escalation and the check event, and the tests share one harness. `RunStage` gained a `Trust` field, which leaves T5's tests unchanged.

No follow-up issues filed. D12 belongs to T7 (#12) and D13 to T16, which are already in the plan.
