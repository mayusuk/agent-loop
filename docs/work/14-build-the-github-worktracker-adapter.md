# Work plan: Build the GitHub WorkTracker adapter

**Issue:** [#14](https://github.com/mayusuk/agent-loop/issues/14) (feature)
**Branch:** `issue/14-build-the-github-worktracker-adapter`
**Base:** `origin/main @ 6ec9fe5` (checked before branching)
**Status:** Done

## Summary

Adds the GitHub `WorkTracker` in `internal/adapters/github`: labels as markers, the read–write–re-read claim with rollback and its claim record, the change feed from issue events and comments with ETags, native dependencies with close reasons and linked merges, author-checked records, `CreateItem` and `Close`. It runs over one shared REST/GraphQL client with a per-identity rate-limit budget and ETag cache that the `CodeHost` (T10) reuses. Tests run against a stateful stub GitHub (`githubtest`), including the tracker cases of the conformance suite; the live run is gated by `AGENT_LOOP_LIVE=1`.

## Context

- INTEGRATION-SPEC §6.1 (record markers, allowed authors, lookup places, I12), §6.2 (change link), §8.1–§8.2 (`WorkTracker`), §9.1–§9.5 (GitHub identities, markers, claim, feed, dependencies), §11 (C1–C5, C11, C12), §13 (profile keys). PRODUCT-SPEC §5.1 gives the default label names. TECHNICAL-SPEC §6.1 (claim number), §9 (idempotency). Plan task T9; plan D5 gates live tests.
- `internal/ports` (#8) fixes the types: `Record.Data` is the marker's whole JSON (D7), error classes are sentinels (D4), names (D5), `Item` fields (D9).
- `internal/adapters/fake` and `conformance/` (#13). Fake D2 (empty cursor = current end, one page per call), D6 (idempotency of writes without records), D8 (error classes) apply here too. Fake D7 is open and settled here (D1 below).
- `contract/` encodes the record JSON (`contract.Record`); the adapter uses it only to build the claim record.
- Tests must not call GitHub; live tests need `AGENT_LOOP_LIVE=1`.

## Approach

**`internal/adapters/github`** (package `github`, shared with T10/T11)
- `client.go`: `Client` for one repository: base URL, `http.Client`, a token source per identity (`ports.Identity` → `TokenSource`, which `*secrets.Lease` satisfies), JSON REST calls with `Link` paging, GraphQL calls, error classification into the port classes, an ETag cache per identity and URL (`If-None-Match`, a 304 serves the cached body), and a rate-limit budget per identity and resource (`core`, `graphql`) from the `x-ratelimit-*` and `Retry-After` headers.
- `records.go`: record markers on raw JSON (encode the last line, find it, read type and key), allowed authors per record type, actor conversion by numeric ID.
- `tracker.go`: `Tracker` with `Config` (labels per marker, the three identity actors, branch prefix, feed page size, clock). `Item`, `Candidates`, `SetMarker`, `Comment`, `FindRecords`, `CreateItem`, `Close`, `Claim`.
- `feed.go`: `Changes` from the repository issue events and the issue comments.
- `deps.go`: `AddDependency`, `Dependencies`, linked merged changes (branch names with a coder change record, closing references, `ClosedEvent.closer`).
- `validate.go`: `Validate`.

**`internal/adapters/github/githubtest`**: a stateful stub GitHub on `httptest`: issues, labels, comments, repository events, dependencies, pull requests and the GraphQL query for closing references; tokens map to actors; ETags and 304s; paging; rate-limit headers; injected failures. `Driver` is an HTTP conformance driver that acts with each actor's token, so the same code drives the stub and a live sandbox. T10 extends both.

**`conformance/`**: `RunTracker(t, f)` runs the tracker cases (C1–C5, C11, C12) with their code-host halves skipped when `Env.Host` is nil. C1 also checks that a claim leaves one claim record.

**`internal/adapters/fake`**: `Claim` posts the claim record too (D1), so the fake and GitHub agree.

## Steps

- [x] Shared client: errors, paging, ETags, rate budget, GraphQL; with tests.
- [x] Records and the stub GitHub (`githubtest`) with the HTTP driver.
- [x] Tracker reads and writes: `Item`, `Candidates`, `SetMarker`, `Comment`, `FindRecords`, `CreateItem`, `Close`, `Claim`; with tests.
- [x] Change feed with paging and cursors; dependencies and linked changes; `Validate`; with tests.
- [x] Fake claim record and `conformance.RunTracker`; conformance against the stub, and the live run gated by `AGENT_LOOP_LIVE=1`.

## Acceptance criteria

- Unit tests use recorded HTTP fixtures (no network) for every method and error class.
- `conformance.Run` passes against the adapter with fixtures, and against a sandbox repository when `AGENT_LOOP_LIVE=1`.
- A repeated write with the same key leaves one result (C1, NFR-2 AC1).
- A claim conflict after a human change rolls back and returns `ErrConflict` (C2).

## Decisions

- **D1** [ambiguity] INTEGRATION-SPEC §9.3 step 4, §8.2 (settles fake D7): `Claim` builds and posts the `claim` status record itself, after the re-check, as the loop identity: the comment text is `agent-loop claimed this item (claim <n>).` and the marker is `{"type":"status","v":1,"key":<key>,"item":<ref>,"event":"claim","claim":<n>}`, where *n* is one more than the loop-authored claim records already on the item (TECHNICAL-SPEC §6.1). The record is also Claim's idempotency ledger: a `Claim` whose key already has a claim record returns the item's current version and writes nothing. The core doesn't post a claim record after `Claim`; after a crash between steps 3 and 4, reconcile posts it with `Comment` and the same key. The fake does the same. Why: §9.3 says `Claim` posts it, and GitHub has no other place to remember that a key already claimed.
- **D2** [gap] INTEGRATION-SPEC §9.3 steps 1 and 3: an item's `Version` is `<updated_at, RFC 3339>|<label names, sorted, comma-separated>|<assignee IDs, sorted, comma-separated>`. The assignees let a retried claim check them against expect (D17). In step 3, "a human changed it" means the labels differ from the expected set (the read labels plus the in-progress label, minus the ready label), the issue was closed, or its assignees changed.
- **D3** [open] INTEGRATION-SPEC §9.3 step 3, NFR-2: the rollback removes the in-progress label if `Claim` added it and adds the ready label back if `Claim` removed it. If the rollback itself fails, `Claim` returns `ErrTransient` naming both failures (not `ErrConflict`), so the caller retries, and the tracker remembers the claim's label writes under its key: a retry with that key rolls them back again and then returns `ErrConflict`. Open: if the worker dies before a rollback succeeds, the item keeps `in-progress` with no claim record, which §9.3 step 4's reconcile rule would turn into a claim. The spec should say what reconcile does with an `in-progress` item whose claim was refused.
- **D4** [ambiguity] INTEGRATION-SPEC §9.4: the tracker feed reads the repository issue events (labels, closes, reopens, assignments, dependency changes) and the repository issue comments polled with `since` (for `Commented`). Pull-request entries are left to the `CodeHost`. The `issues?since` catch-all row is read by the `CodeHost` (T10) to pick the pull requests whose detail it reads; for the tracker, the full reconcile covers anything the events missed. Why: the ports have no "item changed" event, and comments are the one tracker change the issue events don't carry.
- **D5** [gap] INTEGRATION-SPEC §9.4, C12, fake D2: the cursor is `gh1:<last event id>:<last comment id>:<last comment created_at, unix>`. An empty cursor is the current end. Each call returns at most the configured page size, oldest first, events and comments merged by time (events first at equal times), and moves each source's part of the cursor only past what it returned. A cursor whose event ID is no longer in GitHub's events feed (it keeps 90 days) is `ErrConflict`, like an unparsable cursor, and the caller does a full reconcile (fake D8). Each event's `Version` and `AssigneeChanged.Human` are the item's state when the feed is read, not at the event.
- **D6** [gap] INTEGRATION-SPEC §8.2, §9.5: a closed issue's `state_reason` `completed`, or none, is `CloseCompleted`; `not_planned`, `duplicate` and any other reason are `CloseNotDone`. `Close` writes `completed` or `not_planned`. `ClosedBy` is the issue's `closed_by` (the latest `closed` event's actor). A number that is a pull request is `ErrNotFound` for every item method.
- **D7** [gap] INTEGRATION-SPEC §6.2, §9.5: a dependency's `ChangeMerged` is true when a merged pull request is linked to it: one whose head is `<branch_prefix><item>` or `<branch_prefix><item>-<n>` (*n* from 2, stopping at the first name with no pull request) and whose body has a coder-authored `change` record naming the item; or one in the issue's `closedByPullRequestsReferences`; or the `closer` of its latest `ClosedEvent` (one GraphQL query for both).
- **D8** [gap] INTEGRATION-SPEC §8.1, I13: the adapter is configured with the three identities as actors whose ID is the numeric ID of the app's bot user (the `user.id` on its comments and events), not the App ID, since that is the ID GitHub reports on what the app writes. Owners are matched by `user.id`. `Actor.Kind` is `Bot` for `type: Bot`, else `Human`.
- **D9** [gap] INTEGRATION-SPEC §6.1: on items, records are read from the issue body (author: the issue's creator) and its comments. A record counts only from: the loop for `status`, `approved_commits`, `followup`, `task`, `plan`; the coder for `change`, `proposal`, `reply`, `manual_reply`; the reviewer for `review`, `finding`. Only a marker on the last non-blank line counts (contract D14). Others are ignored.
- **D10** [gap] INTEGRATION-SPEC §8.2: `CreateItem` with an empty title uses `agent-loop <record type>` (GitHub refuses an empty title). Its markers become labels. The lookup lists `issues?creator=<loop login>&state=all&since=<Since>` and keeps issues created at or after `Since` by the loop's ID whose body record has the key.
- **D11** [gap] INTEGRATION-SPEC §8: HTTP results map to the port classes as: 404 and 410 `ErrNotFound`; 401 and 403 `ErrPermission`; 403 or 429 with `x-ratelimit-remaining: 0` or `Retry-After` `ErrRateLimited` (retry at `x-ratelimit-reset`, or now plus `Retry-After`, or now plus 60 s for a bare 429); 409, 412 and 422 `ErrConflict`; 5xx, network failures and unreadable responses `ErrTransient`. A GraphQL response with `errors` is `ErrNotFound` for type `NOT_FOUND`, `ErrRateLimited` for `RATE_LIMITED`, else `ErrTransient`.
- **D12** [gap] INTEGRATION-SPEC §9.4, TECHNICAL-SPEC Q4: the shared budget is kept per identity and per rate-limit resource (`core`, `graphql`) from the response headers. A call is refused locally with `ErrRateLimited` (until the reset) when the remaining count is at or below a reserve (default 100 for `core`, 200 points for `graphql`), and after a `Retry-After` until it passes. GET responses with an ETag are cached per identity and URL, and repeated with `If-None-Match`; a 304 doesn't count against the limit.
- **D13** [gap] INTEGRATION-SPEC §11, plan T9/T10: `conformance.RunTracker(t, f)` runs the tracker cases for an adapter with no `CodeHost` yet (`Env.Host` nil): C1, C2, C3, C5 and C11 without their code-host parts (C5 without its merged-change row), C4, and C12's tracker feed. `conformance.Run` requires `Env.Host`. The GitHub adapter moves to `Run` in T10. C1 also checks that a repeated claim leaves one claim record.
- **D14** [gap] Plan T9 "recorded HTTP fixtures": no sandbox recordings exist yet, so the fixtures are a stateful stub GitHub (`githubtest`) that answers the REST and GraphQL subset the adapter calls, as GitHub documents it, with injected error responses. Behaviours the stub assumes and the live run must confirm: issue-dependency events are named `blocked_by_added` and `blocked_by_removed`; repository `closed` events carry `state_reason`; adding a label an issue already has makes no event; creating an issue with labels makes a `labeled` event for each; the repository events list is newest first.
- **D15** [gap] INTEGRATION-SPEC §9.1, NFR-4 AC1, C3: on GitHub the coder and reviewer can't write item comments (`issues: read`), and §9.1 and NFR-4 AC1 rule out giving them `issues: write`. C3 therefore forges item records only as identities that can write there: the human and the owner forge a loop-only `status` record, and the loop forges a coder-only (`change`) and a reviewer-only (`review`) record. The fake and the stub run the same cases.
- **D16** [gap] INTEGRATION-SPEC §8.2, §9.5, §13: `Validate` reports, as errors, a profile adapter other than `github` (`wrong_adapter`), a repository other than the adapter's (`wrong_repo`), a marker mapped to a status (`status_marker_unsupported`) or to no label (`marker_unmapped`), an owner ID that isn't numeric (`owner_id_not_numeric`), and a mapped label missing from the repository (`label_missing`); and as a warning each open ready issue whose body has a `Blocked by #n` line with no native dependency (`text_dependency`). App permission checks are T11.
- **D17** [gap] INTEGRATION-SPEC §9.3, NFR-2: a `Claim` with no claim record for its key that finds the item open, with expect's assignees, no claim record posted since expect's `updated_at`, and expect's labels with this claim's in-progress write made and its ready removal made or still to make, takes that as its own interrupted claim (a failed or lost step 4, or a crash during or after step 2). It finishes step 2, then does steps 3 and 4. Any other difference is still `ErrConflict`. Two workers claiming one item at the same version with different keys can't be told apart this way, and they already race at step 2.

## Verification

Standard checks. The live run: `AGENT_LOOP_LIVE=1` with the sandbox variables documented in `live_test.go`.

## Risks and open questions

- The stub encodes my reading of GitHub's API; the live run is the check (D14). D3's reconcile case is for the owner.

## Outcome

- Added `internal/adapters/github`: `client.go` (shared client: error classes, paging, ETag cache, per-identity rate budget, GraphQL), `records.go` (markers and author checks), `tracker.go`, `feed.go`, `deps.go`, `validate.go`. Added `githubtest` (stub GitHub and an HTTP driver usable against a live sandbox).
- `conformance.RunTracker` passes against the stub (`TestConformance`, feed page 5) and against the fake with no host. `TestLiveConformance` runs the same cases on a sandbox when `AGENT_LOOP_LIVE=1`; it has not been run, as no sandbox credentials are available here.
- Unit tests cover every method against each error class (`TestEveryMethodClassifiesErrors`), every HTTP status class (`TestErrorClasses`), ETag 304s, the rate budget and `Retry-After`, paging, the claim's rollback at step 3 for label, assignee and close changes, a failed rollback, and each dependency link.
- The fake's `Claim` now posts the claim record and uses it as its ledger (D1); its kill-point test counts the claim comment.
- Deviation: the steps were committed as two commits (adapter with its tests, then conformance and fake), since the tests share the stub.
- No follow-up issues filed. D3 and D15 are open for the owner and go into the spec issue.
