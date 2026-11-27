# Work plan: Adopt the minting layout in this repository

**Issue:** [#166](https://github.com/mayusuk/agent-loop/issues/166) (feature)
**Branch:** `issue/166-adopt-the-minting-layout-in-this`
**Base:** `main @ 462c8fc` (checked before branching)
**Status:** Draft

## Summary

Adds `agent-loop plan validate <dir>`, which checks a plan directory's `tasks.yaml` with `ValidateTasks` and the profile's types, and makes this repository ready to mint its own milestones: an `.agent-loop.toml` that names the specs as `source_of_truth`, a `plans/` directory with a README and a sample plan, and a CI step (mirrored in `.wf.toml`) that validates every `plans/*/` directory.

## Context

- Plan `docs/plans/2026-11-25-v0-2-minter-materialise-and-multiple.md`, task T13, D11 (`plan validate`), D14 (handlers live in `handlers/`).
- INTEGRATION-SPEC §7 (task list layout and invalid cases), FR-17 AC6, FR-9 AC2, TECHNICAL-SPEC §11 (CLI table), §13 (`source_of_truth`, `plans_dir`).
- `contract/tasklist.ValidateTasks(plan, file, bodies fs.FS, types, itemExists)` from T1 (#154); its D5 (nil `itemExists` skips external refs) and D10 (bodies read with `os.DirFS`, symlinks refused).
- `cmd/agent-loop`: `project validate` (`validate.go`, `project.go`: `loadProject`, `openAdapters`), the command table in `main.go`, test helpers `newTestProject` (fake adapters) and `runCLI`.
- `plan abandon` (FR-17 AC9) is T7 (#160) and stays unbuilt.

## Approach

- `cmd/agent-loop/plan.go`: `runPlan` dispatches `plan validate` and reports `plan abandon` as not implemented yet. `plan validate [--profile <path> | --project <name> [--config <path>]] <dir>`:
  - Types come from the profile: `--profile` (default `.agent-loop.toml` in the current directory), loaded with `profile.LoadProfile(path, nil)`; or the registered project's profile with `--project`.
  - With `--project`, it also opens the project's tracker (`openAdapters`) and checks each external `depends_on` ref with `WorkTracker.Item` (D11).
  - The plan key is the directory's base name. `tasks.yaml` must be a regular file in the directory. Bodies are read through `os.DirFS(dir)`.
  - It prints one line per problem and a summary; exit 0 when valid, 1 on any problem or on a profile or tracker it can't use, 2 on a usage error.
- TECHNICAL-SPEC §11: a row for `plan validate`.
- `.agent-loop.toml` at the root: tracker and code host `mayusuk/agent-loop`, `source_of_truth` in spec order, `plans_dir = "plans/"`, identities as `secret://` refs plus the owner account, and `agent-loop-claude` for implement, review, address and mint.
- `plans/README.md` and a sample plan, `plans/example/`.
- `scripts/validate-plans.sh`: runs `go run ./cmd/agent-loop plan validate` on each `plans/*/` directory, and says so and exits 0 when there are none. CI and `.wf.toml` run it after `build` and before `tests`.
- CLAUDE.md "How work is organised": new milestones are minted into `plans/`.

## Steps

- [x] `plan validate` with tests (every §7 invalid case, profile and tracker errors, usage), and the TECHNICAL-SPEC §11 row.
- [ ] `.agent-loop.toml` and a test that `project validate` accepts it with the fake adapters.
- [ ] `plans/README.md`, `plans/example/`, `scripts/validate-plans.sh`, the CI step and the `.wf.toml` step, and CLAUDE.md.

## Acceptance criteria

- FR-17 AC6: `agent-loop plan validate` on a valid plan exits 0. On each INTEGRATION-SPEC §7 invalid case it exits 1 and names the problem.
- FR-9 AC2: the repository's profile lists the specs in PRODUCT → INTEGRATION → TECHNICAL order as `source_of_truth`, and `project validate` accepts it (test with fake adapters).
- CI and `.wf.toml` run the plan check in the same position, and it passes on a sample plan under `plans/`.
- CLAUDE.md and `plans/README.md` describe the layout.

## Decisions

- **D1** [gap] Plan D11, TECHNICAL-SPEC §11: the command is `agent-loop plan validate [--profile <path> | --project <name> [--config <path>]] <dir>`. The types come from `--profile` (default `.agent-loop.toml` in the current directory), which needs no deployment config or secrets so CI can run it, or from the registered project's profile with `--project`. The two are exclusive. Why: D11 says "with the profile's types" but not where the profile comes from.
- **D2** [gap] Plan D11: external `depends_on` refs are checked only with `--project`, through the project's tracker (read-only loop token, as `project validate`). Without it, the output says how many external refs weren't checked. With `--project`, a tracker that can't be opened, or an `Item` read that fails other than not found, exits 1 and names the error: the owner asked for the check. Why: D11 says only "when a tracker is reachable".
- **D3** [ambiguity] INTEGRATION-SPEC §7, §8.1: an external ref is looked up as an `ItemRef` with one leading `#` removed (`#17` → `17`; `PROJ-12` as written). Reading: `#17` is GitHub's way of writing item `17`, and `ItemRef` holds the tracker-native key. Materialise (T6, #159) needs the same mapping. Why: §7 writes refs as `"#17"`, §8.1 writes `ItemRef` as `"42"`.
- **D4** [gap] Plan D11, FR-17 AC6: output is one line per problem, `error   <rule>: <location>: <message>` (the `ValidateTasks` rule names and order), then `<dir>: <n> problem(s)`, or `<dir>: valid, <n> task(s)`. Exit 0 when valid, 1 on any problem, missing `tasks.yaml`, or unusable profile, 2 on a usage error. Why: D11 says only "exits 1 and lists every problem"; this follows `project validate`'s layout.
- **D5** [gap] INTEGRATION-SPEC §7: the plan key is the directory's base name after cleaning the path (`plans/foo/` is `foo`), and `tasks.yaml` must be a regular file: a missing file, a directory, or a symbolic link is an error naming it. Why: as for bodies (#154 D10), a symlinked `tasks.yaml` could point outside the plan directory.
- **D6** [ambiguity] Plan T13: "leave out identity secrets" is read as: the profile keeps its required `[identities]` section with `secret://github-app/<identity>` references and the owner account (`mayusuk`, 18241114), and the app IDs and private keys stay in the secret store that `agent-loop init` fills. Why: the profile schema requires `identities`, and the references hold no secret.
- **D7** [gap] Plan T13, handlers/README.md: `stages.mint` names `["agent-loop-claude", "mint"]` as T13 asks, although that subcommand returns `failed` until the mint handler (plan D2) lands. The other stages keep the handler's defaults. Why: the plan asks for a handler for each stage including `mint`; the README says to leave `mint` out for now.
- **D8** [gap] Plan T13: the sample plan is `plans/example/`, a two-task plan that documents the layout. Its `plan.md` says it is an example. Why: the CI step must pass "on a sample plan under `plans/`". Materialise runs only on a merged plan proposal (FR-17 AC1), so a plan added by an ordinary PR is never materialised.
- **D9** [gap] Plan T13: the CI step and `.wf.toml` both run `scripts/validate-plans.sh`, after `build` and before `tests`. It runs `go run ./cmd/agent-loop plan validate --profile .agent-loop.toml` on each `plans/*/` directory and prints `no plans under plans/` and exits 0 when there are none. Why: one script keeps CI and `.wf.toml` the same; after `build` because it needs the binary to build.
- **D10** [open] FR-9, plan T13: how the minter treats a hand-written plan directory under `plans/` (such as `plans/example/`) when it reads the plans directory as context isn't specified. T4/T5 should settle it.

## Verification

`wf.py verify --plan` (gofmt, vet, build, plan check, race tests). `GOOS=linux go build ./...`. Run `scripts/validate-plans.sh` with and without plan directories.

## Risks and open questions

- T7 (#160) builds `plan abandon` in the same `runPlan`; a merge conflict there is likely but small.
- D10: the minter might treat `plans/example/` as an existing plan.

## Outcome

_Filled in before shipping._
