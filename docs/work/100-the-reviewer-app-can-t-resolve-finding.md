# Work plan: The reviewer app can't resolve finding threads, and the failed resolve crashes the review run

**Issue:** [#100](https://github.com/mayusuk/agent-loop/issues/100) (bug)
**Branch:** `issue/100-the-reviewer-app-can-t-resolve-finding`
**Base:** `origin/main @ f4771cf` (checked before branching)
**Status:** Done

## Summary

GitHub refuses `resolveReviewThread` for an app installed without `contents: write`, so the reviewer app (installed with `contents: read`) can't resolve finding threads, and the refusal fails the review stage run after the review is posted (`worker_lost`). Per the owner's decision on #100, the loop identity now resolves a finding's thread once the reviewer's review record rules it `addressed` or `withdrawn`; a failed resolve never fails the run and is retried on its own.

## Context

- Root cause: `internal/adapters/github/reviews.go` `ResolveFinding` sends the mutation `asReviewer`; GitHub checks the installation's permissions, not the token's (measured on mayusuk/mdtoc-e2e: a coder token narrowed to `contents: read` resolves, a reviewer token can't). `internal/workflows/review.go` `postReview` returns the resolve error, so the stage run fails after `PostReview` succeeded.
- The githubtest stub allows the mutation for any account, so no test caught it.
- Owner decision (comment on #100, 2026-11-24): the loop identity resolves; only the reviewer changes a finding's status (FR-4 AC5). Rejected: reviewer app with `contents: write`; never resolving.
- Specs touched: PRODUCT-SPEC §4.1 matrix, FR-7 AC9 wording, D43; INTEGRATION-SPEC §6.3, §9.1 token table, §9.6 (`resolvedBy`), C16 and a new C17.

## Approach

- Adapters: GitHub `ResolveFinding` sends the mutation as the loop identity; the fake resolves as `cfg.Loop`.
- githubtest: a bot whose app installation is known and lacks `contents: write` gets GraphQL `FORBIDDEN` "Resource not accessible by integration" for `resolveReviewThread`/`unresolveReviewThread`. `SetInstallPermissions(app, perms)` records an installation's permissions for apps registered with `SetAppID`; the test project installs the three apps with the §9.1 permissions.
- Conformance C17: `ResolveFinding` resolves the thread as the loop identity (the feed's `ThreadResolved` actor is the loop). C6 tolerates an identity whose resolve the host refuses (it can't clear anything either).
- Workflows: the review run no longer resolves threads. `StageReport.Resolve` names the findings the posted record newly rules `addressed`/`withdrawn` (compared with the latest earlier non-stale record, so a thread someone unresolved later isn't resolved again); a crash rerun that finds its review posted reports the same. The item workflow then runs a new `ResolveFindings` activity on the core queue (the loop identity's adapter activities), retried by Temporal up to 5 times with backoff; a final failure is logged and dropped. The decision to run it depends on the activity result, so recorded histories replay unchanged.

## Steps

- [x] Stub refuses the mutation for an app without `contents: write`; conformance C17 (fails on the reviewer-identity resolve); C6 tolerance.
- [x] GitHub and fake `ResolveFinding` as the loop identity.
- [x] Workflows: a failed resolve doesn't fail the review run; `ResolveFindings` activity retried on its own. Tests.
- [x] Specs: PRODUCT-SPEC §4.1, FR-7 AC9, D43; INTEGRATION-SPEC §6.3, §9.1, §9.6, §11 C16/C17.

## Acceptance criteria

- The loop identity resolves a finding's thread once the reviewer's record rules it `addressed` or `withdrawn` (owner decision on #100).
- A failed resolve never fails the review stage run; it is retried on its own and the status stays in the review records.
- The stub refuses `resolveReviewThread` for an app installed without `contents: write`, and a conformance case covers it.

## Decisions

- **D1** [gap] PRODUCT-SPEC §4.1, INTEGRATION-SPEC §6.3, §9.1: the loop identity resolves a finding's thread once the reviewer's review record rules it `addressed` or `withdrawn`; only the reviewer changes a finding's status (FR-4 AC5). The §4.1 row is split, product D45 records the decision, §6.3 and the §9.1 token table (Reviewer worker no longer resolves; Loop resolves finding threads) are updated, and §9.6, FR-7 AC9, D43 and C16 now name the loop identity as the one whose resolve doesn't clear a human's reply. Owner: decision on #100 (2026-11-24), rejecting a reviewer app with `contents: write` and never resolving threads.
- **D2** [gap] INTEGRATION-SPEC §6.3: a failed resolve doesn't fail the review stage run. The worker resolves after the run, in an activity of its own as the loop identity, retried up to 5 times (1 s backoff doubling, at most 1 min apart); if it still fails it is logged and dropped, and the finding's status stays in the review records. Only findings the record newly rules `addressed`/`withdrawn` are resolved, so a later review never re-resolves a thread someone unresolved (§6.3 "the worker doesn't undo the change"). Why: the issue asks for an independent, non-fatal retry; the specs set no retry bound.
- **D3** [gap] INTEGRATION-SPEC §11: new conformance case C17: `ResolveFinding` resolves the thread as the loop identity (the feed reports `ThreadResolved` by the loop), and C6 skips an identity whose resolve the host refuses (`ErrPermission`), as GitHub does for the reviewer. Why: the issue asks for a case covering the refusal.
- **D4** [gap] INTEGRATION-SPEC §6.3, FR-16: the resolves run after the review run's end is recorded, so their retries don't count as run time, and before the core's next decision, so the next stage (address, merge) sees the threads as the record rules them. Each try has a 1 min limit, so a hanging code host holds the item for at most about 6 min. Why: review R4 on #116; the specs set no order or bound.

## Verification

Full local checks plus `GOOS=linux go build ./...` and `CGO_ENABLED=0 GOOS=darwin go build ./...`.

## Risks and open questions

- Repositories that require resolved conversations are already refused by `validate` (N4a), so a late resolve doesn't hold a merge.

## Outcome

Done as planned, except for D2. The first approach retried failed resolves on every later review and before the merge; it was dropped because it would re-resolve a thread a human had unresolved, which §6.3 forbids. A `ResolveFindings` activity of its own, run once after the review, replaces it. The specs record the owner's decision as product D45 and new conformance case C17. `docs/LIVE-SCENARIO.md` now says C1–C17. No follow-up issues.

Review round 1 (#116): R1–R3 added tests (a stale review resolves nothing; `ResolveFindings` tries every finding and names the one that failed; C17 asserts the reviewer's own resolve is refused where `Env.ReviewerCannotResolve` is set, as for GitHub). R4 moved the resolves after the run's end is recorded and cut each try to 1 min (D4). No pushbacks.
