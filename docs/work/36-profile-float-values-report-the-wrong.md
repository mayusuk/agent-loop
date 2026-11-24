# Work plan: Profile: float values report the wrong problem (follow-up from #33)

**Issue:** [#36](https://github.com/mayusuk/agent-loop/issues/36) (bug)
**Branch:** `issue/36-profile-float-values-report-the-wrong`
**Base:** `origin/main @ 430fcad` (checked before branching)
**Status:** Done

## Summary

A float in a profile or deployment file now gets the problem its key's type calls for: "must be a whole number" only where the schema wants an integer, and the schema's own type error everywhere else. Each bad float produces exactly one problem, with no extra "items are equal" or "must be a whole number" alongside "unknown key".

## Context

- D17 (work plan #9): a float is accepted only in `limits.daily_spend`; elsewhere, even `2.0`, it is a problem at its key. Review R6/R7 on PR #33 found the reporting wrong.
- Root cause, `internal/profile/load.go`: `checkFloats` runs before the schema check and reports every disallowed float as "must be a whole number" without knowing what the key expects (R6). It then replaces the float with `null`, so the schema sees two equal `null` items in a `uniqueItems` list, and it reports a problem even at keys the schema calls unknown (R7).

## Approach

- `checkFloats` no longer reports anything. It records each disallowed float's key and kind (finite or not) and replaces the value with a placeholder that the schema treats as a non-integer number (the value itself if it has a fraction, otherwise the value plus 0.5; 0.5 for inf/nan).
- After validation, the leaves at a float key are interpreted:
  - a type error that wants an integer: one problem, the key's override or "must be a whole number";
  - inf/nan: the schema's leaves are dropped;
  - otherwise the schema's leaves are reported as usual (for example "got number, want string").
- A `uniqueItems` error on a list that has a float item is dropped: the placeholders can't be compared meaningfully.
- Finally, a float key with no problem at itself or an ancestor (inf/nan, or a key the schema doesn't constrain) gets "must be a finite number" or "must be a whole number". A float under an unknown key or a wrongly typed table gets no extra problem.

## Steps

- [x] Tests for R6 and R7 that fail on the current code.
- [x] Rework `checkFloats` and `schemaProblems` as above.

## Acceptance criteria

- Only integer keys get the whole-number message; every other key keeps the schema's type error (`timezone = 1.5`, `follow_up.ready = 1.0`).
- One problem for each bad value (`release_apps = [1.0, 2.0]`, `bogus = 1.5`).

## Decisions

- **D1** [ambiguity] D17 (#9) / FR-1 AC2: a float outside `limits.daily_spend` is reported as "must be a whole number" only where the schema expects an integer (or the key's override message, such as `limits.daily_agent_runs`); at any other key it is the schema's type error ("got number, want string"). This narrows D17's "must be a whole number" wording, as the issue asks (R6).
- **D2** [gap] FR-1 AC2: one problem per bad value. A float under an unknown key gets only "unknown key"; a list with a float item does not also report "items at i and j are equal"; inf/nan reports only "must be a finite number". Why: R7.

## Verification

Standard checks only.

## Risks and open questions

A list with a float item does not report duplicate items until the float is fixed. Acceptable: the next load reports it.

## Outcome

Done as planned in `internal/profile/load.go`: `checkFloats` now only records floats and swaps in a non-whole placeholder; the schema's leaves at a float's key decide the problem, and `floatFallbacks` covers inf/nan and unconstrained keys. New tests in `internal/profile/keys_test.go` cover R6 (`timezone`, `follow_up.ready`, `merge_method`) and R7 (`release_apps` duplicates, unknown keys, inf/nan). No deviations, no follow-up issues.
