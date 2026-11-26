# Work plan: Extend the handler contract for mint and validate task lists

**Issue:** [#154](https://github.com/mayusuk/agent-loop/issues/154) (feature)
**Branch:** `issue/154-extend-the-handler-contract-for-mint`
**Base:** `origin/main @ 0f37cc6` (checked before branching)
**Status:** Draft

## Summary

Adds the full `tasks.yaml` validator, `tasklist.ValidateTasks`, which applies every rule in INTEGRATION-SPEC §7 and returns every problem it finds, so materialise (T6), the mint stage (T4) and `agent-loop plan validate` (plan D11) share one check. It also completes the mint stage's result checks: a mint run's input must carry its `mint` object, and tests named after FR-18 AC1 and AC3 prove the mint `done` and `complete` shapes.

## Context

- Plan `docs/plans/2026-11-25-v0-2-minter-materialise-and-multiple.md`, T1. INTEGRATION-SPEC §4.2 (mint input), §5.1, §5.2 (mint done), §7 (task list); PRODUCT-SPEC FR-17 AC6, FR-18 AC1–AC3.
- Already in place from v0.1 (#7): `StageMint`, `MintInput`, `MintDone`, `Complete`, the mint rows of `allowedOutcomes`, `checkMint` (plan key pattern, head SHA, proposal title and body file), the mint parts of `input.schema.json` and `result.schema.json`, and `tasks.schema.json` with the `TaskList` and `Task` types. #7's work plan D12 kept `contract/` standard-library only and left YAML parsing and the directory checks to v0.2.
- TECHNICAL-SPEC §3: `internal/core` imports only the standard library and `contract/` (and its subpackages, per `imports_test.go`). `handlers/` may import only `contract/` from this module.
- `internal/profile` already validates TOML against a JSON Schema with `santhosh-tekuri/jsonschema/v6` and reports every leaf error; the task list check follows the same pattern.

## Approach

- New package `contract/tasklist` (D1), holding `ValidateTasks(plan string, file []byte, bodies fs.FS, types []string, itemExists func(string) bool) (contract.TaskList, []Problem)`.
  - Parse the YAML into a `yaml.Node` (gopkg.in/yaml.v3) and convert it to a JSON-like value. Duplicate mapping keys, several documents, an empty file, anchors other than plain aliases and non-scalar keys are schema problems (D3).
  - Validate that value against the embedded `contract/v1/tasks.schema.json` and report every leaf error with its location (`tasks[1].key`).
  - Then, using every field that is well typed even when other fields aren't, check: `plan` equals the directory; duplicate keys; types not configured; body file missing, not a regular file, or over 64 KiB; `depends_on` entries naming an unknown key or an external item that doesn't exist (D4, D5); cycles (one problem per strongly connected component, D6).
  - A `Problem` has a `Rule` (stable code per §7 rule), the task key, the location and a message. Problems come in a fixed order. The `TaskList` is returned only when there are no problems.
- `contract.ValidateResult`: a mint run whose input has no `mint` object is invalid, like review and address.
- `tasks.schema.json` description updated to name `ValidateTasks`. `contract/tasks.go` comment updated.
- Tests: `TestFR17AC6_*` (one per §7 rule and a valid list), `TestFR18AC1_*` (mint done carries plan key, head, proposal; complete only from mint), `TestFR18AC3_*` (mint input without `mint`).

## Steps

- [x] ValidateResult: mint input must carry `mint`; FR-18 AC1 mint done/complete tests.
- [ ] `contract/tasklist`: YAML to value, schema check, problems type, and FR-17 AC6 schema tests.
- [ ] Directory and profile checks: plan name, duplicate keys, types, body files, dependencies, cycles, with FR-17 AC6 tests.
- [ ] Schema and doc comment updates; go.mod.

## Acceptance criteria

- FR-18 AC1: `complete` is accepted only for the mint stage, and `done` for mint carries `plan_key`, `head` and `proposal`.
- FR-18 AC3: a mint result that is malformed or not allowed is reported invalid.
- FR-17 AC6: `ValidateTasks` rejects each case in INTEGRATION-SPEC §7: schema mismatch, `plan` not equal to the directory, duplicate key, unconfigured type, missing body file, body over 64 KiB, unknown `depends_on` key, missing external item, and a cycle. A valid list passes.
- `plan_key` must match `[a-z0-9-]{1,64}`.
- Tests are named after the requirements (`TestFR17AC6_...`, `TestFR18AC1_...`).

## Decisions

- **D1** [ambiguity] Plan T1, TECHNICAL-SPEC §3: `ValidateTasks` lives in a subpackage, `contract/tasklist`, not in `contract` itself. Reading: "in `contract/`" means the contract tree. Why: it needs a YAML parser and a JSON Schema validator; keeping them out of package `contract` keeps the core and handlers, which import `contract`, on the standard library (as #7 D12 intended).
- **D2** [gap] FR-18 AC1, AC2, plan T1: `ValidateResult` doesn't accept a materialise stage. Materialise has no handler (PRODUCT-SPEC §4, #7 D19), so its `done` (the created item references) is produced by the worker's deterministic activity (T6), never by a `result.json`. Why: there is no handler result to validate.
- **D3** [gap] INTEGRATION-SPEC §7: YAML specifics. A file that isn't one YAML document, a mapping with a repeated key, a non-string mapping key, or a merge key (`<<`) doesn't match the schema. Timestamps and other tagged scalars are read as strings; `!!int`, `!!float`, `!!bool` and `!!null` keep their types. Why: §7 defines the file only by its JSON Schema.
- **D4** [gap] INTEGRATION-SPEC §7 (`depends_on`): an entry that matches the task key pattern `[a-z0-9-]{1,64}` is a task key in the plan, and is an "unknown key" if no task has it. Any other entry (`#17`, `PROJ-12`) is an external item ref, passed to `itemExists` as written. Why: §7 allows both kinds in one list but doesn't say how to tell them apart.
- **D5** [gap] Plan D11, INTEGRATION-SPEC §7: a nil `itemExists` skips the external item check, for `agent-loop plan validate` when no tracker is reachable. Materialise always passes one. Why: D11 checks external refs only "when a tracker is reachable".
- **D6** [gap] INTEGRATION-SPEC §7: each dependency cycle is one problem per strongly connected component, naming its keys in task-list order; a task that depends on itself is a cycle. Why: "returns every problem" without listing overlapping cycles many times.
- **D7** [gap] INTEGRATION-SPEC §7: a body path is cleaned (`./tasks/a.md` is `tasks/a.md`) before it is looked up; a path that names a directory or anything but a regular file counts as missing. Why: §7 says only "missing".
- **D8** [gap] INTEGRATION-SPEC §5.2 (mint): a mint run whose input has no `mint` object makes the result invalid. Why: same rule as review and address; the worker builds the input, so this only catches a worker bug.
- **D9** [open] INTEGRATION-SPEC §7: a key is "never reused", but reuse across plans can't be decided from one file; materialise's `task` records (T6) would have to check it. A body file that isn't UTF-8, and a list with no tasks, are not rejected by §7 and are accepted here.

## Verification

`wf.py verify --plan` (gofmt, vet, build, race tests). `GOOS=linux go build ./...`.

## Risks and open questions

- `os.DirFS` follows symlinks, so a caller reading bodies from a checkout could read outside the plan directory. Materialise reads from a commit's tree (T3), where this doesn't arise; noted for T13's `plan validate`.

## Outcome

_Filled in before shipping._
