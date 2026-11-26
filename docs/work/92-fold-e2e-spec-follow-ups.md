# Work plan: fold the e2e spec follow-ups into the specs

**Issue:** [#92](https://github.com/mayusuk/agent-loop/issues/92) (spec), with #97, #106, #108, #110, #113, #115, #117, #118, #122
**Branch:** `issue/92-fold-e2e-spec-follow-ups`
**Base:** `origin/main @ f4771cf`
**Status:** In review

## Summary

Writes the decisions that the e2e bug fixes recorded (#91, #94, #96, #99, #100, #101, #102, #103, #104, #105) into the specs, as the merged code already behaves. Docs only: no code change. `[open]` decisions are not decided; their issues stay open with a question for the owner.

## Context

Each spec follow-up issue lists Decisions. Every gap and ambiguity was checked against the code on `main` before it was written down (`internal/runners/local.go`, `internal/runners/sandbox.go`, `internal/workflows/stage.go`, `internal/workflows/item.go`, `internal/secrets/githubapp.go`, `cmd/agent-loop/serverinit.go`, `cmd/agent-loop/validate.go`, `internal/adapters/github/{validate,hostvalidate,setup}.go`, `internal/projection`, `conformance/`).

Already in the specs, so skipped: #110 D1 (TECHNICAL-SPEC §7.2 certificate checks), #115 D1 (product D45, §4.1, INTEGRATION-SPEC §6.3, §9.1) and D3 (C18). GETTING-STARTED already has #122 D5 (source install) and the #118 row in §8.

Areas left alone: TECHNICAL-SPEC §7.2 Keychain and T12 signing text (#95), the scenario test (#90).

## Approach

| Issue | Decision | Where |
|---|---|---|
| #92 | D1 deadline, D2 review branch | INTEGRATION-SPEC §3.3, §4.1; TECHNICAL-SPEC §6.5 |
| #97 | D1 tool caches, D3 `GH_CONFIG_DIR` | TECHNICAL-SPEC §7.2 |
| #106 | D1, D2 secrets read once, failures not kept | INTEGRATION-SPEC §9.1 |
| #108 | D1–D3 git identity, reserved variables | TECHNICAL-SPEC §7.2 |
| #110 | D2 key on every platform | TECHNICAL-SPEC §7.2 |
| #113 | D1 projection path fallback, D2 server init checks | TECHNICAL-SPEC §11, §13 |
| #115 | D2 retry bound, D4 order and bound | INTEGRATION-SPEC §6.3 |
| #117 | D1, D2 `status:ready` offer; D3 dependency hint | INTEGRATION-SPEC §12.2 step 3, §9.5 |
| #118 | D1–D3, D6, D7 hidden bypass lists; D4 `rulesets` table; D5 C17 | INTEGRATION-SPEC §9.8 steps 1 and 8, §11, §12.2 step 8; TECHNICAL-SPEC §10, §11; CLAUDE.md (C1–C18) |
| #118 | D8 (owner: verify and keep the design; verified 2026-11-25): IQ4 (d) records the result; the loop app token check stays open | INTEGRATION-SPEC §14 |
| #122 | D1–D4 dry-run heading, board "starting", HOST, `host check` | TECHNICAL-SPEC §10, §11 |

## Steps

- [x] Check each decision against the code and the specs.
- [x] Edit INTEGRATION-SPEC and TECHNICAL-SPEC.
- [x] Run the checks.

## Acceptance criteria

- Every gap and ambiguity decision in the ten issues is in the spec section it names, or was already there.
- No `[open]` decision is decided.

## Decisions

None.

## Verification

Standard checks (`wf.py verify --plan`).

## Risks and open questions

Left open in this PR: #97 D2 (caches for other toolchains) and #113 D3 (`XDG_STATE_HOME`); the owner has since ruled on both, and each is implemented in its own PR. #118 D8 is settled: the owner chose to verify on the sandbox repository and keep the design, and IQ4 (d) records the 2026-11-25 result.

## Outcome

Review round 1 (PR #125): R1 led to a change. The TECHNICAL-SPEC §7.2 "Reserved variables" bullet now says the variables are reserved (refused in the run's env and PassEnv) on every run, and that the six git variables are set only in coder runs and `GH_TOKEN` only with a read token, as `internal/runners/local.go` (`reservedEnv`, lines 177 and 268) and `internal/workflows/stage.go:292` do. No pushbacks.

Owner ruling after round 1: #118 D8 is settled (verify on the sandbox repository, keep the design). INTEGRATION-SPEC §14 IQ4 (d) now records the 2026-11-25 verification on mayusuk/mdtoc-e2e: every bypass-list edit moved `updated_at`, which has millisecond precision, and an unchanged list did not. Still open: whether the loop app's token sees the same `updated_at`. The PR now closes #118.
