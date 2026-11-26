# Work plan: Specs: fold the spec follow-ups #126, #129, #131, #134, #136

**Issue:** [#142](https://github.com/mayusuk/agent-loop/issues/142) (feature), with #126, #129, #131, #134, #136
**Branch:** `issue/142-specs-fold-the-spec-follow-ups-126-129`
**Base:** `origin/main @ 5aef6f1` (checked before branching)
**Status:** Done

## Summary

Writes the decisions recorded by #95, #97, #113, #133 and #124 (spec follow-ups #126, #129, #131, #134, #136) into the specs, as the merged code on `main` behaves, plus the three resolutions #142 gives for the `[open]` items (#134 D2, #136 D5, #136 D6). Docs only: no code change.

## Context

Follows PR #125 (`docs/work/92-fold-e2e-spec-follow-ups.md`). Each gap and ambiguity was checked against the code on `main` before it was written down: `internal/secrets/codesign.go` (certificate, copy-and-rename signing, symlinks), `cmd/agent-loop/sign.go` (flags, macOS only), `cmd/agent-loop/keychaintrust.go`, `serverstart.go`, `worker.go` and `init.go` (`retrust`: stale items, locked keychain, `--check` stop), `cmd/agent-loop/validate.go` (`cache_reserved`), `internal/runners` (caches), `cmd/agent-loop/serverinit.go` (projection path, UTF-8 check), `.goreleaser.yaml` and `.github/workflows/release.yml`.

Already in the specs, so skipped:
- #126 D1 (certificate name and identifier) was already in TECHNICAL-SPEC §7.2 (PR #127); kept, moved into the new Keychain trust bullet.
- #129 D1–D7 were applied by PR #130 (TECHNICAL-SPEC §7.2 tool caches, §13 `caches` row; INTEGRATION-SPEC §13 `caches` row). Only the problem code `cache_reserved` (D5) was missing.
- #131 D1–D3 were applied by PR #132 (TECHNICAL-SPEC §13 projection path).
- #136 D1 (owner ruling) was applied to TECHNICAL-SPEC §7.2 by PR #137; GETTING-STARTED §2 already says it.
- #134 R1 comment (no home directory: say which `~/` places were skipped): TECHNICAL-SPEC §13 already asks for it; the code fix is #139.

Left to other issues: the GETTING-STARTED / §15 install rewrite for `agent-loop-claude` (#140); #139's own spec text for details beyond #134 D2 (for example its socket path length limit).

## Approach

| Issue | Decision | Where |
|---|---|---|
| #126 | D1–D5, D7, D9: certificate, per-user signing, copy-and-rename, stale items, what `server start` / `worker` check, locked keychain, `validate` / dry-run don't check | TECHNICAL-SPEC §7.2 (new "Keychain trust" bullet, split from "Reads denied") |
| #126 | D6, D9: `init --check` drift and stop, re-trust, ad-hoc warning, locked keychain in `init` | INTEGRATION-SPEC §12.2 step 2 |
| #126 | D8: `sign [--binary] [--identity] [--keychain]`, macOS only, symlinks, Cellar | TECHNICAL-SPEC §11 |
| #129 | D5: problem code `cache_reserved`, refused before the run starts | TECHNICAL-SPEC §7.2 |
| #129 | D1–D4, D6, D7 | already in the specs (PR #130) |
| #131 | D1–D3 | already in the specs (PR #132) |
| #134 | D1: non-UTF-8 value refused, no config written | TECHNICAL-SPEC §11 (`server init`) |
| #134 | D2 (resolved in #142): absolute `[api] socket` beside the config with no home directory, implemented in #139 | TECHNICAL-SPEC §11, §13 (new "API socket" bullet) |
| #136 | D1 | already in the specs (PR #137) |
| #136 | D2–D4, D7: targets, GoReleaser mechanics, tap and token, `brews` deprecation | TECHNICAL-SPEC §15 (new "Release builds") |
| #136 | D5 (owner, 2026-11-25): `agent-loop-claude` released to the same tap | TECHNICAL-SPEC §15 (decision only; docs rewrite is #140) |
| #136 | D6 (owner, applied 2026-11-25): `release tags` ruleset on `refs/tags/v*` | TECHNICAL-SPEC §15 |

## Steps

- [x] Check each decision against the code and the specs.
- [x] Edit INTEGRATION-SPEC and TECHNICAL-SPEC.
- [x] Run the checks.

## Acceptance criteria

- Every decision in the five issues is in the spec section it cites, or was already there (mapping table above).
- The three resolutions in #142 are recorded as decided.
- No product behaviour changes.

## Decisions

None.

## Verification

Standard checks (`wf.py verify --plan`), plus `GOOS=linux go build ./...` and `CGO_ENABLED=0 GOOS=darwin go build ./...`.

## Risks and open questions

#134 D2 is recorded as decided while #139, which implements it, is still open; until it merges, `server init` with no home directory still writes the `~` default socket. #139 may touch the same TECHNICAL-SPEC §11/§13 lines and will need a merge.

## Outcome

Done as planned. TECHNICAL-SPEC §7.2 gets a "Keychain trust (macOS)" bullet (split out of "Reads denied") and the `cache_reserved` code; §11 the full `sign` flags and the `server init` UTF-8 and socket rules; §13 an "API socket" bullet; §15 "Release builds". INTEGRATION-SPEC §12.2 step 2 gets the Keychain trust check. No deviations, no follow-up issues.
