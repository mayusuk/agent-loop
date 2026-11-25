# Work plan: Profile: nested lists with floats report a false duplicate (follow-up from #85)

**Issue:** [#86](https://github.com/mayusuk/agent-loop/issues/86) (bug)
**Branch:** `issue/86-profile-nested-lists-with-floats-report`
**Base:** `origin/main @ a6f091c` (checked before branching)
**Status:** Draft

## Summary

A float placeholder inside a nested list or table no longer makes two distinct list items look equal, so `release_apps = [[1.0], [1.5]]` stops reporting a false "items at 0 and 1 are equal". The duplicate check now skips a pair when either item holds a float placeholder anywhere inside it.

## Context

`decodeBytes` (`internal/profile/load.go`) replaces each disallowed float with a not-whole placeholder (`checkFloats`, `notWhole`) before JSON Schema validation, and `schemaProblems` drops a `uniqueItems` result when either duplicate item is itself a placeholder (review R1/R7 on #85, D17).

Root cause: that check looks only at the item's own key (`release_apps[0]`). In `[[1.0], [1.5]]` the placeholders sit at `release_apps[0][0]` and `release_apps[1][0]`; `notWhole(1.0)` is 1.5 and `notWhole(1.5)` is 1.5, so both inner lists become `[1.5]`, the schema reports them equal, and the check doesn't see the nested floats. `distinctPlaceholders` only separates placeholders within one list.

## Approach

In `schemaProblems`, treat a `uniqueItems` pair as comparable only when neither item has a float placeholder at its key or any key under it (new helper `floatWithin`). Of the two fixes the issue offers, this is the one that mirrors the existing rule: a placeholder can't be compared, wherever it sits. The nested items already get a type error at their own keys, so nothing is lost.

## Steps

- [x] _Each step small enough to commit on its own._
- [x] _For a bug, the first step is a test that fails because of it._

## Acceptance criteria

- `release_apps = [[1.0], [1.5]]` reports no duplicate problem.
- A real duplicate with no float in it is still reported.
- A test covers it.

## Decisions

- **D1** [gap] TECHNICAL-SPEC profile validation (D17 on #36/#85): a list's duplicate-item check ignores any pair where either item contains a disallowed float (or inf/nan) at any depth, so `[[1.0], [1.0]]` reports only the items' own problems, not a duplicate. Why: the placeholder hides the float's real value, and the item is already wrong.

## Verification

Standard checks only.

## Risks and open questions

None.

## Outcome

_Filled in before shipping: what was done, deviations from this work plan's approach and why, follow-up issues filed. Details the specs should record go in Decisions, not here._
