# Work plan: T0: Merge agent-loop-handlers into this repository

**Issue:** [#179](https://github.com/mayusuk/agent-loop/issues/179) (feature)
**Branch:** `issue/179-t0-merge-agent-loop-handlers-into-this`
**Base:** `origin/main @ 1bf63a0` (checked before branching)
**Status:** Draft

## Summary

Bring `mayusuk/agent-loop-handlers` into this repository as `handlers/` with its git history, in this repository's one Go module, so a contract change and the handler change it needs land in one PR (v0.2 plan D14). D34 still holds: `agent-loop-claude` stays a separate program, binary and Homebrew formula, and an import-guard test keeps `handlers/` importing only `contract/` from this module and keeps everything else from importing `handlers/`. One `.goreleaser.yaml` releases both binaries and both formulas.

## Context

- v0.2 plan D14 (owner ruling, 2026-11-26) and task T0 (#179); PRODUCT-SPEC D34, D38; INTEGRATION-SPEC §12; TECHNICAL-SPEC T7, §15 (release builds, starter handlers, release tags).
- `internal/core/imports_test.go` is the model for an import guard: parse every `.go` file with `go/parser` (ImportsOnly) and check each import.
- Both repositories release with GoReleaser v2.20.3 from a `v*` tag, with notes from `scripts/release-notes.sh` (#141, #150 here; agent-loop-handlers #58, #61). Both are at 0.1.0.
- The handlers' module is `github.com/mayusuk/agent-loop-handlers`, requiring `github.com/mayusuk/agent-loop` at a pseudo-version for `contract/`.

## Approach

- `git subtree add --prefix=handlers <handlers repo> main` (no squash), so `git log -- handlers/` shows its history.
- Delete the handlers' repository-level files that the root now owns: `handlers/go.mod`, `go.sum`, `.github/`, `.wf.toml`, `.goreleaser.yaml`, `.gitignore`, `scripts/`. Rewrite imports `github.com/mayusuk/agent-loop-handlers/...` to `github.com/mayusuk/agent-loop/handlers/...`; `go mod tidy`.
- Import guard `handlers/imports_test.go`: a `badImports(root)` walks a tree and reports (a) a file under `handlers/` importing a module package outside `contract/...` and `handlers/...`, and (b) a file outside `handlers/` importing `handlers/...`. Run on the repository root (must be empty) and on fixtures in `handlers/testdata/` (one per direction, must report).
- Release: one `.goreleaser.yaml` with the core's darwin and linux builds plus an `agent-loop-claude` build (CGO off, all four targets), two archives, two `brews`. One tag `vX.Y.Z` releases both at the same version. `scripts/release-notes.sh <tag>` prints the core section, then `## agent-loop-claude X.Y.Z` and the handlers' section, and fails if the tag isn't the handlers' `Release`. With a changelog argument it prints only that file's section, as now. The handlers' release tests move to the merged script, root config and workflow.
- Docs and specs: PRODUCT-SPEC D38, INTEGRATION-SPEC §12, TECHNICAL-SPEC T7/§3.1/§15, GETTING-STARTED, CLAUDE.md, `.wf.toml` notes, `handlers/README.md`.

## Steps

- [x] Merge the handlers repository's history at `handlers/` (D8).
- [x] One module: drop the handlers' module files and repository-level files, rewrite imports, tidy; all tests pass.
- [x] Import-guard test with fixtures for both directions.
- [ ] One release: merged `.goreleaser.yaml`, `release-notes.sh` and release tests; `goreleaser check` and a snapshot build.
- [ ] Docs and specs: D38, INTEGRATION-SPEC §12, TECHNICAL-SPEC, GETTING-STARTED, CLAUDE.md, `.wf.toml` notes, handlers README.

## Acceptance criteria

- D14: `go build ./...` builds both `agent-loop` and `agent-loop-claude`, and `go test -race -count=1 ./...` runs the handlers' tests, all passing on Ubuntu and macOS.
- D34: the import-guard test fails if a `handlers/` package imports anything in this module other than `contract/`, or if a package outside `handlers/` imports `handlers/`. Both directions are tested with a fixture.
- `git log -- handlers/` shows the handlers repository's history.
- `goreleaser check` passes, and a snapshot build (`goreleaser release --snapshot --clean`) produces both binaries and both formulas.
- The specs, GETTING-STARTED and CLAUDE.md name no separate handlers repository, except to say it was merged here and archived.

## Decisions

- **D1** [gap] TECHNICAL-SPEC §15 Release builds: the two programs are released in lockstep. One `vX.Y.Z` tag on this repository makes one GitHub release with both programs' archives and writes both formulas; `agent-loop-claude`'s `Release` must equal the tag's version, so the next handlers release is 0.2.0 with agent-loop 0.2.0. Why: the issue asks for one `.goreleaser.yaml`, which has one version per tag (GoReleaser OSS has no per-project tag prefixes), and both are at 0.1.0 already.
- **D2** [gap] TECHNICAL-SPEC §15 Release notes: `scripts/release-notes.sh <tag>` (no changelog argument) prints the `## X.Y.Z` section body of `CHANGELOG.md`, a blank line, `## agent-loop-claude X.Y.Z`, a blank line and the body of the `## X.Y.Z` section of `handlers/CHANGELOG.md`. It fails, printing nothing, when the version isn't `Release` in `handlers/cmd/agent-loop-claude/main.go` or either section is missing or empty, so every release needs a handlers section. With a changelog argument it prints only that file's section, without the `Release` check (as before). The core's fence and blank-line rules (#141 D6, #150 D1) now apply to both changelogs. Why: the issue says the handlers keep their own CHANGELOG section and the scripts merge; the two scripts' fence rules were the same CommonMark reading.
- **D3** [gap] TECHNICAL-SPEC §15 Starter handlers: `agent-loop-claude` is built with `CGO_ENABLED=0` for all four targets on the same `macos-latest` release runner. Its archives stay `agent-loop-claude_<version>_<os>_<arch>.tar.gz` with `README.md` and `CHANGELOG.md` (from `handlers/`) at the archive root. Its formula's homepage and caveat link are `https://github.com/mayusuk/agent-loop/tree/main/handlers`. The GitHub release has GoReleaser's default title (the tag), not `agent-loop-claude vX.Y.Z`, and keeps the core's CHANGELOG footer. Why: one release for both programs (D1).
- **D4** [ambiguity] D34, D14 import guard: "imports nothing from this module except `contract/`" is read as limiting only this module's packages: a `handlers/` file may import the standard library, third-party modules, `contract/...` and `handlers/...` itself. Test files' imports count too; `testdata` directories are skipped except as fixtures. The guard lives in `handlers/imports_test.go`, with fixtures in `handlers/testdata/importguard/`. Why: the handlers already use `jsonschema`, and D34 is about not depending on the core.
- **D5** [gap] TECHNICAL-SPEC §3.1 layout: the handlers' repository-level files are dropped (`go.mod`, `go.sum`, `.github/`, `.wf.toml`, `.goreleaser.yaml`, `.gitignore`, `scripts/`): the root's CI, workflow config, release config and script cover them. Their `docs/` (specs excerpts, agent-draft format, plans, work plans), `README.md` and `CHANGELOG.md` stay under `handlers/` unchanged except for links that named the old repository. The draft schema `$id` (internal only) becomes `https://github.com/mayusuk/agent-loop/handlers/agent-draft.schema.json`.
- **D6** [open] `handlers/docs/specs/agent-loop-excerpts.md` is a vendored copy of this repository's specs, now redundant in one repository. It is kept unchanged as the issue asks; deleting it (and pointing `handlers/README.md` at `specs/`) is left to the owner.
- **D8** [ambiguity] #179 History: the issue names `git subtree add --prefix=handlers`, but a subtree add keeps the old commits at their root paths, so `git log -- handlers/` shows only the merge commit and the acceptance criterion fails. The handlers' `main` (8cf3130, 24 commits) was instead rewritten with `git filter-branch --index-filter` to put every path under `handlers/` (authors, dates and messages kept; commit IDs change) and merged with `--allow-unrelated-histories`. Reading: the issue's acceptance criterion ("`git log -- handlers/` shows the handlers repository's history") is the goal and the command was the suggested means; D14 asks only to keep the history The owner can overrule at review.
- **D7** [open] Old work plans and CHANGELOG entries link `mayusuk/agent-loop-handlers#n`. They are history and are left as they are; GitHub redirects transferred issues.

## Verification

- `git log --oneline -- handlers/ | wc -l` shows the handlers' commits.
- `GOOS=linux go build ./...`, `CGO_ENABLED=0 GOOS=darwin go build ./...`.
- `go run github.com/goreleaser/goreleaser/v2@v2.20.3 check` and `release --snapshot --clean`: both binaries and both formulas in `dist/`.

## Risks and open questions

- `goreleaser check` exits non-zero on the deprecated `brews` section (TECHNICAL-SPEC §15 already records this); the check passes otherwise.
- The old repository's open issues, README and archive are owner steps after merge; the PR lists the commands.

## Outcome

_Filled in before shipping._
