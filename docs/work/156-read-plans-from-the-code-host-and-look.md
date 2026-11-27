# Work plan: Read plans from the code host and look up plan records

**Issue:** [#156](https://github.com/mayusuk/agent-loop/issues/156) (feature)
**Branch:** `issue/156-read-plans-from-the-code-host-and-look`
**Base:** `origin/main @ 462c8fc` (checked before branching)
**Status:** Done

## Summary

Adds `CodeHost.PlanAt`, which finds the latest base-branch commit that changed a plan directory, the merged change that brought it in and who merged it, and returns the plan's files at that commit (plan D5, FR-17 AC1, AC8). Plan proposals get their record lookups: `OpenChange` opens a proposal on a plan branch idempotently by its `proposal` record, and `Change.Plan` names a proposal's plan key. New conformance cases C19–C21 hold the fake and GitHub adapters to it.

## Context

- Plan `docs/plans/2026-11-25-v0-2-minter-materialise-and-multiple.md` T3 and D5. Materialise (T6) reads the plan through this method; mint (T4) opens proposals through `OpenChange`.
- INTEGRATION-SPEC §6.1 (record table: `task`, `plan`, `proposal` lookups), §7 (plan directory layout), §8.3 (`CodeHost`), §11 (conformance). PRODUCT-SPEC FR-17 AC1, AC3, AC4, AC8; FR-13 AC7 (actors by stable ID, no "is owner" in adapters).
- `internal/ports/methodset_test.go` checks that the Go interfaces equal the §8 code blocks, so the spec and the port change together.
- `CreateItem` already looks up records from `NewItem.Since` in both adapters (GitHub: `GET /issues?creator=<loop>&state=all&since=`), for every record type. This issue adds the conformance case that proves it for `task` and `plan`.
- `OpenChange` today requires an item and is keyed on the `change` record. A proposal has no item.
- The GitHub stand-in (`githubtest`) already models commits with file snapshots, the contents API for files and squash/merge merges. The fake's commits have no files yet.

## Approach

- **Port** (`internal/ports/codehost.go`, INTEGRATION-SPEC §8.3): `PlanAt(ctx, dir string) (PlanVersion, error)` at the end of `CodeHost`. `PlanVersion{Commit, Change, MergedBy *Actor, MergedAt, Files []PlanFile}`, `PlanFile{Path, Size, Content UntrustedText}`. `Change` gains `Plan string`.
- **GitHub** (`internal/adapters/github/plan.go`): `GET /commits?sha=<base>&path=<dir>&per_page=1`; `GET /commits/{sha}/pulls`, keeping pull requests merged into the base, preferring the one whose merge commit is the commit, else the earliest merged; `GET /pulls/{n}` for `merged_by`; `GET /contents/<dir>?ref=<sha>` walked recursively, each file read with `GET /contents/<file>?ref=<sha>`. Proposals: `OpenChange` with a `proposal` record skips the item, requires a `<prefix>plan/` branch and a `plan` key, and looks up the coder's record on the branch's pull requests. `view` sets `Change.Plan` from the coder's proposal record.
- **Stand-in** (`githubtest`): the commits list with `sha` and `path` (git's default history simplification), `commits/{sha}/pulls`, contents directory listings, `merge_commit_sha` on pulls. The driver gets `PushFiles`.
- **Fake**: commits carry file snapshots (copied on push, three-way merged on merges); `PlanAt` with the same rules; `PushFiles`; proposals in `OpenChange` and `view`.
- **Conformance**: `Driver.PushFiles`; C19 (`PlanAt`), C20 (proposal records), C21 (`CreateItem` with `task` and `plan` records from `Since`).

## Steps

- [x] Port and spec: `PlanAt`, `PlanVersion`, `PlanFile`, `Change.Plan` in `internal/ports` and INTEGRATION-SPEC §8.3; C19–C21 in §11.
- [x] Fake: file snapshots, `PushFiles`, `PlanAt`, proposals; unit tests.
- [x] GitHub stand-in and adapter: commits, pulls-for-commit and directory contents in `githubtest`; `PlanAt` and proposals in the adapter; unit tests.
- [x] Conformance C19–C21 in `conformance/`, run against both adapters.

## Acceptance criteria

- FR-17 AC8: for a plan changed by two merged PRs, the method returns the later commit, its PR and its merger. The fake and GitHub adapters both pass the new conformance cases.
- FR-17 AC3: `CreateItem` with the same key returns the existing item when it is retried right after a write (as in C1), for `task` and `plan` records.
- FR-13 AC7: the merger is reported by stable account ID, with no "is owner" decision in the adapter.
- The GitHub adapter is tested against the in-memory GitHub stand-in, with no network calls.

## Decisions

- **D1** [gap] INTEGRATION-SPEC §8.3 (plan D5): the method is `PlanAt(ctx, dir string) (PlanVersion, error)`. `dir` is the plan directory from the repository root (`<plans_dir><plan-key>`, for example `plans/003-auth`), so the adapter needs no `plans_dir`. It returns `ErrNotFound` when no commit on the base branch ever changed `dir`. Why: D5 names what the method must do but not its shape.
- **D2** [gap] INTEGRATION-SPEC §8.3, FR-17 AC8: "the latest base-branch commit that changed the plan" is the first commit of the base branch's history filtered by the path, with git's default history simplification (a merge commit counts only if the directory differs from every parent). After a merge-commit merge it is the change's own commit, not the merge commit. Why: this is what GitHub's commits API returns, and the fake matches it.
- **D3** [gap] INTEGRATION-SPEC §8.3, FR-17 AC8: the change that brought the commit in is a change merged into the base branch that contains it: the one whose merge commit is that commit if any, else the earliest merged. A commit no merged change contains (a direct push to the base) has an empty `Change` and nil `MergedBy`, and the core treats that as not owner-merged. Open or closed-unmerged changes never count (FR-17 AC4). Why: the spec doesn't say what to report when several or no changes contain the commit.
- **D4** [gap] INTEGRATION-SPEC §8.3, §7: `PlanVersion.Files` is every regular file under `dir` at the commit, recursively, sorted by path, with paths relative to `dir` (`plan.md`, `tasks.yaml`, `tasks/login-api.md`) and content as `UntrustedText`. Symlinks and submodules are left out. A file over 1 MiB has its `Size` but empty `Content` (GitHub's contents API doesn't return it), and materialise rejects it as too large (§7: body files at most 64 KiB). A commit that deleted the directory returns an empty `Files`, not `ErrNotFound`. Why: §7 lists the files but not how they are read.
- **D5** [gap] INTEGRATION-SPEC §6.1 (`proposal`), §8.3: proposals go through `OpenChange` with a `proposal` record and no `Item`. The branch must be under `<branch_prefix>plan/` and the record must carry `plan` (the plan key), else `ErrConflict`. A repeat finds the coder's proposal record with the key on the pull requests whose head is the branch (§6.1). Only the coder may open one. Why: §6.1 gives the lookup but no port method; a new method isn't needed.
- **D6** [gap] INTEGRATION-SPEC §8.3: `Change` gains `Plan`, the plan key from the coder's `proposal` record, set only on a change whose branch is under `<branch_prefix>plan/`. A proposal has no `Item`, and the GitHub adapter doesn't look up closing references for it. Why: the project supervisor (T6) must tell an owner-merged proposal and its plan key from the `Merged` event's change.
- **D7** [gap] INTEGRATION-SPEC §11: C19 checks `PlanAt` (two merged changes to a plan, the later one's commit, change and merger by ID; unrelated and unmerged changes ignored; files at the commit; `ErrNotFound` for a missing plan; `ErrConflict` for a `dir` that names a file, is empty or has a `..` segment, per D9). C20 checks proposal records (idempotent `OpenChange`, `Change.Plan`, refusal off a plan branch). C21 checks `CreateItem` repeats for `task` and `plan` records from `Since`, and is a tracker case. The conformance `Driver` gets `PushFiles(as, branch, files)`, which pushes commits that set those files. Why: the issue asks for the next free C numbers without naming the cases.
- **D8** [open] Plan D6 (mint-`<n>`): counting the coder's `proposal` records needs a lookup over every proposal, which no port method gives. T4 settles it.
- **D9** [gap] INTEGRATION-SPEC §8.3: an empty `dir`, one with a `..` segment, or one that names a file is `ErrConflict`. A leading or trailing `/` is ignored. Why: the method takes a path, and the spec doesn't say which paths are valid.
- **D10** [open] CLAUDE.md (Testing rules): it says the conformance suite is C1–C18, now C1–C21; the coder agent may not edit CLAUDE.md, so the owner updates it. `docs/LIVE-SCENARIO.md` is updated.

## Verification

Standard checks. Conformance runs against the fake and the GitHub stand-in.

## Risks and open questions

- The live GitHub conformance run (`AGENT_LOOP_LIVE=1`) isn't run here; C19 merges with squash only, so a squash-only sandbox repository is fine.

## Outcome

Done as planned. `PlanAt`, `PlanVersion`, `PlanFile` and `Change.Plan` are in `internal/ports` and INTEGRATION-SPEC §8.3, with C19–C21 in §11. The fake's commits now carry file snapshots so it can answer `PlanAt`; its `Push` writes `conformance.txt` (or `conformance-base.txt` on the base) as the stand-in's does. The stand-in gained the commits list with path filtering, `commits/{sha}/pulls`, directory listings in the contents API and `merge_commit_sha`. The conformance `Driver` gained `PushFiles`. One existing fake test opened a `proposal`-typed change for an item as the loop; it now uses a `change` record, since proposals have their own rules (D5). `CreateItem` needed no change: both adapters already look up from `Since` for every record type, and C21 proves it for `task` and `plan`. The live conformance run was not run. No follow-up issues.
