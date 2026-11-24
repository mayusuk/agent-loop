# Work plan: Gate check in Host.Validate reads only the first 100 check runs

**Issue:** [#68](https://github.com/mayusuk/agent-loop/issues/68) (bug)
**Branch:** `issue/68-gate-check-in-host-validate-reads-only`
**Base:** `origin/main @ 7f40ba3` (checked before branching)
**Status:** Draft

## Summary

`Host.Validate` reads only the first page (100) of check runs and of commit statuses on the base tip, so a gate reported past that page is falsely warned `gate_unknown`. Both listings are now read across every page. `Host.Gates` shares the same reader, so it gets the same fix.

## Context

- `internal/adapters/github/hostvalidate.go` `validateGates` (work plan #23 D11, FR-1 AC2) calls `Client.Get` once on `/commits/{sha}/check-runs` and `/commits/{sha}/status` with `per_page=100`. GitHub pages both: the check runs in `check_runs` and the statuses in `statuses`, each with a `Link: rel="next"` header. Only page 1 was read.
- `internal/adapters/github/gates.go` `Host.Gates` (§9.7, C9) has the identical single-page read. Past 100 runs a required gate shows `Missing` and the item waits in merging.
- `Client.Pages` already follows `Link` headers. The `githubtest` stand-in only pages bare-array lists, not lists wrapped in an object, so it could not reproduce the bug.

## Approach

- `githubtest`: let a response page a list held under a key of an object body (`listKey`), and use it for check runs and the combined status, as GitHub does.
- `gates.go`: one helper, `commitChecks`, reads every page of both listings through `Client.Pages`. `validateGates` and `Gates` use it.

## Steps

- [x] Tests that fail because of the bug: a gate whose only check run, or only status, is past the first 100 on the base tip is warned `gate_unknown`; `Gates` reports a required gate past the first page as `Missing`. Includes the stand-in paging change.
- [ ] Paginate both listings through a shared helper used by `validateGates` and `Gates`.

## Acceptance criteria

- A gate whose check run or status is past the first 100 on the base tip isn't reported `gate_unknown`.
- Both listings (check runs and statuses) are paginated.

## Decisions

- **D1** [gap] §9.7 (C9): `Host.Gates` reads every page of check runs and statuses too, not only `Validate`. Why: it has the same first-page read, and a required gate past it would hold the item in merging as `Missing`; the issue names only `Validate`.

## Verification

Standard checks only.

## Risks and open questions

GitHub caps the check-runs listing at the 1000 most recent runs per commit; beyond that a gate can still be unseen. Not addressed: no realistic repository reaches it on one commit.

## Outcome

_Filled in before shipping: what was done, deviations from this work plan's approach and why, follow-up issues filed. Details the specs should record go in Decisions, not here._
