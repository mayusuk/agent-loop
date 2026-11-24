# Work plan: Report owner rulesets that let the loop app bypass required checks

**Issue:** [#71](https://github.com/mayusuk/agent-loop/issues/71) (bug)
**Branch:** `issue/71-report-owner-rulesets-that-let-the-loop`
**Base:** `origin/main @ 7f40ba3` (checked before branching)
**Status:** Draft

## Summary

`project validate` (and so `init`'s Validate step) now fails when an active repository ruleset on the base branch, other than the loop's base ruleset, holds rules beyond the push rules and lists the loop app in its bypass list. `init`'s Rulesets step reports it too. Such a ruleset lets the loop's merges skip the project's required checks, pull-request rules and "up to date", which INTEGRATION-SPEC §9.8 step 1 says still apply to them.

## Context

- INTEGRATION-SPEC §9.8 step 1: the loop app bypasses only the base-branch ruleset; "the project's own rules for the base branch (required checks, pull requests) still apply to its merges". §9.7 relies on "up to date" applying to the loop's merges.
- Root cause: `validateRulesets` (internal/adapters/github/hostvalidate.go) only judges the ruleset chosen for each required kind (`misfit`). After #70 D18 an owner ruleset with non-push rules never fits as the base ruleset, so init creates its own, which fits, and nothing looks at the owner ruleset's bypass list. `TestFR1AC5_RulesetsLeaveOwnerBaseRulesets/CIRulesetBypassedByLoopApp` keeps that state with no problem reported (review of #70, R2).

## Approach

- `hostvalidate.go`: a new check `loopBypassProblems(ds, fit, base)` called from `validateRulesets`. For each active repository-or-higher branch ruleset other than the fitted base ruleset whose include patterns cover the base branch (exact ref, `~ALL`, or a glob matching it, after `~DEFAULT_BRANCH` is resolved) and whose excludes don't, if it holds any rule outside `basePushRules` and lists the loop app (Integration) as a bypass actor, report `ruleset_bypasses_base_rules` as an error naming the ruleset and the bypassed rules, with the hint to remove the loop app from its bypass list.
- `setup.go`: `BaseRuleProblems` also returns these problems, so init's Rulesets step warns about them even when its Validate step is skipped.
- Tests: extend `TestFR1AC5_RulesetsLeaveOwnerBaseRulesets` (the R2 scenario) and add a focused hostvalidate test with positive, negative and edge cases.

## Steps

- [ ] _Each step small enough to commit on its own._
- [ ] _For a bug, the first step is a test that fails because of it._

## Acceptance criteria

- An owner ruleset that lets the loop app bypass required checks on the base branch is reported by `project validate` and by `init`.
- A test covers it.

## Decisions

- **D1** [ambiguity] INTEGRATION-SPEC §9.8 step 1, §12.2: the report is an error (`ruleset_bypasses_base_rules`), so `project validate` fails, not a warning. Reading: "the project's own rules still apply to its merges" is a requirement, and the same rules in the loop's own base ruleset are already an error (#24 D18). init's Rulesets step prints it as a warning, like the other base-rule problems, and doesn't change the owner's ruleset.
- **D2** [gap] INTEGRATION-SPEC §9.8 step 1: a ruleset counts when it is active, targets branches, covers the base branch (exact ref, `~ALL`, or a `fnmatch`-style pattern matching it, not excluded), holds any rule other than creation, update, deletion and non_fast_forward, and lists the loop app as an Integration bypass actor in any bypass mode. Organisation rulesets count too, since the loop app bypasses them the same way. Evaluate-mode and disabled rulesets don't.

## Verification

The four checks from `.wf.toml`, plus `GOOS=linux go build ./...` and `CGO_ENABLED=0 GOOS=darwin go build ./...`.

## Risks and open questions

GitHub's ref patterns are fnmatch with `**`; the check uses a simple glob matcher, which may miss exotic patterns (it never reports a false positive on exact refs).

## Outcome

_Filled in before shipping: what was done, deviations from this work plan's approach and why, follow-up issues filed. Details the specs should record go in Decisions, not here._
