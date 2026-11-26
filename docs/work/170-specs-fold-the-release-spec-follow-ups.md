# Work plan: Specs: fold the release spec follow-ups #144, #146, #148, #151 and agent-loop-handlers #59, #62

**Issue:** [#170](https://github.com/mayusuk/agent-loop/issues/170) (feature), with #144, #146, #148, #151 and mayusuk/agent-loop-handlers#59, #62
**Branch:** `issue/170-specs-fold-the-release-spec-follow-ups`
**Base:** `origin/main @ 958df78` (checked before branching)
**Status:** Done

## Summary

Writes the decisions recorded by #141, #140, #139 and #150 (spec follow-ups #144, #146, #148, #151) and by agent-loop-handlers #58 and #61 (its follow-ups #59, #62) into TECHNICAL-SPEC §11, §13 and §15 "Release builds", as the merged code on `main` of each repository behaves. Docs only: no code change.

## Context

Follows PR #143 (`docs/work/142-specs-fold-the-spec-follow-ups-126-129.md`). Each decision was checked against `main` before it was written down: `cmd/agent-loop/serverinit.go` (socket length check, skip line, secrets path), `cmd/agent-loop/version.go`, `CHANGELOG.md`, `scripts/release-notes.sh`, `.goreleaser.yaml` and `.github/workflows/release.yml` here; and in mayusuk/agent-loop-handlers, its `.goreleaser.yaml`, `.github/workflows/release.yml`, `scripts/release-notes.sh` and `README.md`. The handlers repository has no specs, so its pipeline goes in §15 next to agent-loop's (X7), as #170 says.

Already in the specs, so skipped:
- #148 D1 (socket length with the NUL: 103 / 107 bytes) and D4 (secrets path beside the config) were applied with #139 (TECHNICAL-SPEC §11, §13 "Secrets path").
- #144 D4 (`[open]`, stale GETTING-STARTED banner) is a guide fix, not a spec detail; left to the guide's check before v0.1 ships.

Left to other work: the GETTING-STARTED and README install text for `agent-loop-claude` is PR #147 (#140), which merges after the handlers' v0.1.0 release; this records the decision only.

## Approach

| Issue | Decision | Where |
|---|---|---|
| #144 | D1: notes are the CHANGELOG section plus a footer; fail if missing or empty | TECHNICAL-SPEC §15 "Release notes" |
| #144 | D2: CHANGELOG format | TECHNICAL-SPEC §15 "CHANGELOG" |
| #144 | D3: archives carry `CHANGELOG.md` | TECHNICAL-SPEC §15 "Mechanics" |
| #144 | D5: what `agent-loop version` prints | TECHNICAL-SPEC §15 "Version string" (replaces the one-line rule) |
| #144 | D6: `## ` inside a fence is not a heading | TECHNICAL-SPEC §15 "Release notes" |
| #144 | D4 (`open`) | skipped: guide text, not spec |
| #151 | D1: CommonMark §4.5 fences, top level only | TECHNICAL-SPEC §15 "Release notes" |
| #148 | D1, D4 | already in the specs (#139) |
| #148 | D2: only the beside-config socket is length-checked | TECHNICAL-SPEC §11 |
| #148 | D3: wording of the skipped-candidate line | TECHNICAL-SPEC §13 "Projection path" |
| #146 | D1, D2: guide checks with `agent-loop-claude version`; PR #147 and the tap README follow the handlers' release | TECHNICAL-SPEC §15 "Starter handlers" (decision only) |
| handlers#59 | D1, D3, D4, D5, D6, D8: pure-Go Linux build, archives and caveat, tag-cut release, `brews`, title, fences | TECHNICAL-SPEC §15 "Starter handlers" |
| handlers#59 | D2: notes from the section, tag must equal `Release` | TECHNICAL-SPEC §15 "Starter handlers" |
| handlers#59 | D7: plan D16 resolved | already in the specs (§15 "Starter handlers", PR #143); the handlers plan change is its own repository's |
| handlers#62 | D1, D2: CommonMark fences | TECHNICAL-SPEC §15 "Starter handlers" (same rules as agent-loop's) |

## Steps

- [x] Check each decision against the code and the specs.
- [x] Edit TECHNICAL-SPEC §11, §13, §15.
- [x] Run the checks.

## Acceptance criteria

- Every decision in the six issues is in the spec section it cites, or was already there or skipped (mapping table above).
- #146 is recorded as decided.
- No product behaviour changes.

## Decisions

None.

## Verification

Standard checks (`wf.py verify --plan`), plus `GOOS=linux go build ./...` and `CGO_ENABLED=0 GOOS=darwin go build ./...`.

## Risks and open questions

§15 describes PR #147's install text as decided while it is still open; it merges after the handlers' v0.1.0 release.

## Outcome

Done as planned. TECHNICAL-SPEC §11 says only the beside-config socket is length-checked; §13 gives the skipped-candidate line; §15 "Release builds" gets the version string, CHANGELOG format, release-notes and fence rules, archive contents, and the handlers' release pipeline under "Starter handlers". No deviations, no follow-up issues.
