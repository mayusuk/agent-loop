# Work plan: CHANGELOG and release notes for v0.1.0

**Issue:** [#141](https://github.com/mayusuk/agent-loop/issues/141) (feature)
**Branch:** `issue/141-changelog-and-release-notes-for-v0-1-0`
**Base:** `origin/main @ 5aef6f1` (checked before branching)
**Status:** Done

## Summary

Adds `CHANGELOG.md` with a `0.1.0` section, and makes the GitHub release for a `v*` tag use that section as its notes instead of GoReleaser's commit list. The release workflow fails if the CHANGELOG has no section for the tag.

## Context

- TECHNICAL-SPEC §15 (v0.1 row) and PRODUCT-SPEC §12 define what v0.1 ships; the CHANGELOG summarises them.
- #124 (work plan `docs/work/124-*.md`) added `.goreleaser.yaml` and `.github/workflows/release.yml`: GoReleaser v2.20.3 pinned, `brews` formula to `mayusuk/homebrew-tap`, actions pinned to SHAs (tested by `cmd/agent-loop/release_test.go`). Its D2 (platforms), D3 (version is the tag without `v`), D5 (handlers formula) and D7 (`brews` deprecation makes `goreleaser check` exit non-zero) apply.
- `cmd/agent-loop/version.go`: `-X main.version={{ .Version }}` gives `0.1.0` on a tag build, `dev` otherwise (covered by `TestVersion`).
- Style: `agent-loop-handlers/CHANGELOG.md` (heading per version, a `Contract versions` line, `### Added`, `### Known issues`).
- #138: macOS builds are not signed with a Developer ID; #140 / mayusuk/agent-loop-handlers#58: the handlers formula is coming.

## Approach

- `CHANGELOG.md` at the root, in the handlers' style, with `## 0.1.0`: what v0.1 ships, install, platforms, contract version, what waits for v0.2/v0.3, known issues.
- `scripts/release-notes.sh <tag-or-version> [changelog]`: prints the body of the `## <version>` section (leading `v` stripped), trimmed of blank lines at the ends; exits non-zero if the section is missing or empty.
- `release.yml`: a step writes the notes to `$RUNNER_TEMP/release-notes.md` from `$GITHUB_REF_NAME`, then GoReleaser runs `release --clean --release-notes <file>`. GoReleaser version, `brews` and the SHA pins stay.
- `.goreleaser.yaml`: `release.footer` links to the full CHANGELOG; `CHANGELOG.md` goes into the archives with the getting-started guide.
- Tests in `cmd/agent-loop/release_test.go`: the script extracts a section, rejects a missing or empty one, strips `v`; the repository's CHANGELOG has a non-empty `0.1.0` section; release.yml passes `--release-notes`.

## Steps

- [x] Add `CHANGELOG.md` with the 0.1.0 section.
- [x] Add `scripts/release-notes.sh` and its tests.
- [x] Use it in the release workflow and GoReleaser config; test the workflow wiring.
- [x] Check locally: `goreleaser check`, `release --snapshot --clean` (with `GORELEASER_CURRENT_TAG=v0.1.0` and without), snapshot binary `version`, `--release-notes` output.

## Acceptance criteria

- `CHANGELOG.md` has a `0.1.0` section summarising v0.1 (TECHNICAL-SPEC §15), install (`brew install mayusuk/tap/agent-loop`, `agent-loop sign`), supported platforms (#136 D2), contract version, and known limits (#138).
- The GitHub release uses the CHANGELOG section, not only a commit list. The pinned GoReleaser version and the `brews` config stay.
- `agent-loop version` prints `0.1.0` for a tag build and `dev` otherwise.

## Decisions

- **D1** [gap] TECHNICAL-SPEC §15 / #124 D3, release notes: a tag `vX.Y.Z` release's notes are the body of the `## X.Y.Z` section of `CHANGELOG.md`, plus a footer linking to the full CHANGELOG; GoReleaser's commit list is not used for tag releases. The release workflow fails before publishing if the section is missing or empty. Why: the issue allows header/footer or `--release-notes`; a whole-section file keeps the notes in one place, and failing stops a release with no notes.
- **D2** [gap] CHANGELOG format: one `## <version>` heading per release (no `v`, no date), a `Contract versions:` line, then `### Added` and `### Known issues` (and `### Not in this release` for scope that waits for a later release), matching `agent-loop-handlers/CHANGELOG.md`. Why: no spec defines it; the issue points to the handlers' file.
- **D3** [gap] #124 D3, archive contents: the release archives carry `CHANGELOG.md` beside `docs/GETTING-STARTED.md`. Why: an archive install should carry its own release notes.
- **D5** [ambiguity] Issue #141, "`agent-loop version` prints `dev` otherwise": read as "a build with no version information". A release build prints the tag without `v` (`agent-loop 0.1.0`, from `-X main.version`); `go install ...@v0.1.0` prints `v0.1.0`; a plain `go build` in a git checkout prints the pseudo-version Go 1.24+ stamps from VCS (for example `agent-loop v0.0.0-20261126034210-5aef6f120701`, `+dirty` if modified), kept because it names the commit; only a build without VCS or module information (`-buildvcs=false`, `(devel)`) prints `dev`. Why: the existing `resolvedVersion` (#124) already prefers the module version, and the issue only asks to check it.
- **D6** [gap] D1 / D2, section boundaries: a line is a `## ` heading only outside a fenced code block (a ```` ``` ```` or `~~~` fence, closed by the same marker, indented or not), so a `## ` line in a shell or Markdown example neither ends a section's release notes nor starts one. Why: no spec defines the CHANGELOG's parsing; without it a code example would publish cut-off notes with exit 0 (PR #145 review R3).
- **D4** [open] GETTING-STARTED's banner still says "Status: draft ... The commands don't exist yet", which is stale for v0.1.0; not changed here (it belongs with the guide's check against the real tools before v0.1 ships).

## Verification

- `go run github.com/goreleaser/goreleaser/v2@v2.20.3 check` (non-zero only for the known `brews` deprecation, #124 D7).
- `GORELEASER_CURRENT_TAG=v0.1.0 go run ...@v2.20.3 release --snapshot --clean --release-notes <notes>`; the snapshot binary's `agent-loop version`.
- A plain `go build` prints `agent-loop dev`.

## Risks and open questions

- The notes step runs on `macos-latest`, so the script must work with BSD `awk` and `sh` (and GNU on CI's Ubuntu tests).

## Outcome

Done as planned. `CHANGELOG.md` has the 0.1.0 section; `scripts/release-notes.sh` extracts a version's section and fails on a missing or empty one; `release.yml` writes the notes from `$GITHUB_REF_NAME` and passes them with `--release-notes`; `.goreleaser.yaml` adds a footer linking to the CHANGELOG and ships `CHANGELOG.md` in the archives. GoReleaser stays pinned at v2.20.3 and `brews` is unchanged.

Checked locally: `goreleaser@v2.20.3 check` reports only the known `brews` deprecation (#124 D7); `GORELEASER_CURRENT_TAG=v0.1.0 ... release --snapshot --clean --release-notes <notes>` succeeds, its archives hold `CHANGELOG.md` and `docs/GETTING-STARTED.md`, and the darwin arm64 binary prints `agent-loop 0.1.0-SNAPSHOT-1261187`; the same ldflags with `0.1.0` print `agent-loop 0.1.0`; `-buildvcs=false` prints `agent-loop dev`. A plain `go build` prints a pseudo-version (D5).

No follow-up issues filed.

Review round 1 (PR #145): R1 added `TestIssue141_GoreleaserKeepsBrewsFooterAndArchiveChangelog` (brews to the tap, the footer, `CHANGELOG.md` in archives); R2 added the `0.1.0` tag-build rows to `TestVersion`; R3 made `scripts/release-notes.sh` ignore `## ` lines inside code fences (D6, `TestIssue141_ReleaseNotesIgnoreHeadingsInCodeFences`, checked with BSD awk and Ubuntu's mawk). No pushbacks.
