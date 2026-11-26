# Work plan: Docs and specs: install agent-loop-claude with brew install mayusuk/tap/agent-loop-claude (#136 D5)

**Issue:** [#140](https://github.com/mayusuk/agent-loop/issues/140) (feature)
**Branch:** `issue/140-docs-and-specs-install-agent-loop`
**Base:** `origin/main @ 5aef6f1` (checked before branching)
**Status:** Done

## Summary

The getting-started guide installs the starter handlers with `brew install mayusuk/tap/agent-loop-claude`, keeping `go install` from a checkout as the alternative. TECHNICAL-SPEC T7 and §15 (X7) say the handlers are distributed through the same tap. The tap's README gets the `agent-loop-claude` row and install line in a separate PR, mayusuk/homebrew-tap#1. Docs only: no code change.

## Context

- Owner decision (2026-11-25, on #136 D5): `mayusuk/agent-loop-handlers` is public and gets a Homebrew formula in `mayusuk/homebrew-tap`. Its release pipeline is mayusuk/agent-loop-handlers#60 (#58): GoReleaser writes `Formula/agent-loop-claude.rb`, with no dependencies and a caveat that Claude Code must be installed and logged in.
- GETTING-STARTED §2 installs agent-loop with Homebrew (#124) but still builds the handlers from a checkout.
- PR #143 (open) adds a §15 "Release builds" part with a "Starter handlers" bullet recording the same decision. This change touches only the T7 row and the §15 v0.1 row, so both can merge.

## Approach

- `docs/GETTING-STARTED.md` §2: Homebrew install of `agent-loop-claude` after agent-loop, with the Claude Code prerequisite; `go install` as the alternative; the "From source" part points at either.
- `specs/TECHNICAL-SPEC.md`: T7 row and §15 v0.1 row name the tap.
- `mayusuk/homebrew-tap` README: formula row, install line, generated-formula note (separate PR).

## Steps

- [x] Update GETTING-STARTED §2.
- [x] Update TECHNICAL-SPEC T7 and §15.
- [x] Open the tap README PR (mayusuk/homebrew-tap#1).

## Acceptance criteria

- `docs/GETTING-STARTED.md`: the source install of the handlers is replaced with `brew install mayusuk/tap/agent-loop-claude`, `go install` kept as the alternative.
- TECHNICAL-SPEC §15 (X7) and T7: the starter handlers are distributed through the same tap.
- `mayusuk/homebrew-tap` README: the `agent-loop-claude` row and install line (separate PR).

## Decisions

- **D1** [gap] GETTING-STARTED §2: the guide checks the install with `agent-loop-claude version`, and says Claude Code must be installed and logged in first, matching the formula's caveat (agent-loop-handlers#58 D3). Why: the issue names only the install command.
- **D2** [gap] mayusuk/homebrew-tap README: the tap PR should merge after the first `agent-loop-claude` release writes the formula, so the README never names a formula the tap doesn't have. Why: the issue doesn't order the two.

## Verification

Standard checks only (no code change). The tap PR is a README change in a repository with no checks.

## Risks and open questions

None. Until mayusuk/agent-loop-handlers#60 merges and a `v*` tag is pushed, `brew install mayusuk/tap/agent-loop-claude` doesn't work yet; the `go install` alternative covers that window.

## Outcome

Done as planned: GETTING-STARTED §2 and TECHNICAL-SPEC T7 and §15 updated; tap README PR mayusuk/homebrew-tap#1 opened. No deviations, no follow-up issues.
