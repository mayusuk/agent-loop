# Work plan: The reviewer app can't resolve finding threads, and the failed resolve crashes the review run

**Issue:** [#100](https://github.com/mayusuk/agent-loop/issues/100) (bug)
**Branch:** `issue/100-the-reviewer-app-can-t-resolve-finding`
**Base:** `origin/main @ f4771cf` (checked before branching)
**Status:** Draft

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
- Workflows: `resolveRuled` resolves the threads of every finding a review record rules `addressed`/`withdrawn` that has a thread, each retried with `a.retry`, and ignores failures. It runs after a review is posted, when a crash rerun finds its review already posted, and before a merge. Since each review record carries the earlier statuses forward, every later review and the merge retry a resolve that failed.

## Steps

- [ ] Stub refuses the mutation for an app without `contents: write`; conformance C17 (fails on the reviewer-identity resolve); C6 tolerance.
- [ ] GitHub and fake `ResolveFinding` as the loop identity.
- [ ] Workflows: a failed resolve doesn't fail the review run; resolves retried on later reviews, crash reruns and merge. Tests.
- [ ] Specs: PRODUCT-SPEC §4.1, FR-7 AC9, D43; INTEGRATION-SPEC §6.3, §9.1, §9.6, §11 C16/C17.

## Acceptance criteria

- The loop identity resolves a finding's thread once the reviewer's record rules it `addressed` or `withdrawn` (owner decision on #100).
- A failed resolve never fails the review stage run; it is retried on its own and the status stays in the review records.
- The stub refuses `resolveReviewThread` for an app installed without `contents: write`, and a conformance case covers it.

## Decisions

- **D1** [gap] PRODUCT-SPEC §4.1, INTEGRATION-SPEC §6.3, §9.1: the loop identity resolves a finding's thread once the reviewer's review record rules it `addressed` or `withdrawn`; only the reviewer changes a finding's status (FR-4 AC5). The §4.1 row is split, §6.3 and the §9.1 token table (Reviewer worker no longer resolves; Loop resolves finding threads) are updated, and §9.6, FR-7 AC9, D43 and C16 now name the loop identity as the one whose resolve doesn't clear a human's reply. Owner: decision on #100 (2026-11-24), rejecting a reviewer app with `contents: write` and never resolving threads.
- **D2** [gap] INTEGRATION-SPEC §6.3: a failed resolve doesn't fail the review stage run. After the per-write retries (3 tries, 1 s apart, transient errors only) it is dropped; the resolve is tried again after every later non-stale review (whose record carries the status forward), when a crash rerun finds its review already posted, and before each merge. Why: the issue asks for an independent retry; these points need no new workflow activity and so no workflow versioning.
- **D3** [gap] INTEGRATION-SPEC §11: new conformance case C17: `ResolveFinding` resolves the thread as the loop identity (the feed reports `ThreadResolved` by the loop), and C6 skips an identity whose resolve the host refuses (`ErrPermission`), as GitHub does for the reviewer. Why: the issue asks for a case covering the refusal.

## Verification

Full local checks plus `GOOS=linux go build ./...` and `CGO_ENABLED=0 GOOS=darwin go build ./...`.

## Risks and open questions

- Repositories that require resolved conversations are already refused by `validate` (N4a), so a late resolve doesn't hold a merge.

## Outcome

_Filled in before shipping._
