# Work plan: Implement the handler contract package (`contract/v1`)

**Issue:** [#7](https://github.com/mayusuk/agent-loop/issues/7) (feature)
**Branch:** `issue/7-implement-the-handler-contract-package`
**Base:** `origin/main @ 399150e` (checked before branching)
**Status:** Done

## Summary

Adds the versioned handler contract to `contract/`: Go types for `input.json`, `result.json`, tracker record markers and `tasks.yaml`, the four JSON Schemas in `contract/v1/` (embedded with `go:embed`), `ValidateResult`, marker encoding and decoding, and contract version negotiation. Every later stage, worker and adapter task builds on these types.

## Context

- INTEGRATION-SPEC §2 (versioning), §3.1 (`AGENT_LOOP_OUT`), §4 (input), §5 (result and per-stage rules), §6.1–§6.6 (records), §7 (task list). PRODUCT-SPEC FR-18 AC1 (results per stage) and AC3 (invalid result is `failed`), FR-4, FR-5 AC1–AC6.
- Plan `docs/plans/2026-11-15-v0-1-mvp.md`, task T2.
- `internal/core` may import only the standard library and `contract/` (TECHNICAL-SPEC §3.1, plan D1). So `contract/` itself stays standard-library only: core must not pull in a third-party package through it.
- Existing code: `contract/doc.go` only.

## Approach

Package `contract` (standard library only):

- `version.go`: `Version = "1.0"`, `ParseVersion`, `CheckVersion` (major must be 1, minor at most the core's).
- `schemas.go`: `//go:embed v1/*.schema.json`, exposed as `Schemas` (an `fs.FS`) and `Schema(name)`.
- `input.go`: `Input` and its stage objects; `ParseInput` ignores unknown fields (§2).
- `result.go`: `Result` and its result objects, per-stage `done` types; `ParseResult(stage, data)` decodes strictly (unknown fields rejected, §2).
- `validate.go`: `ValidateResult(in, data, outDir) (Result, error)`. On any violation it returns a `failed` result whose reason names the rule, plus the error. It checks everything that can be decided from the input, the result and the out directory. Rules that need the repository or the tracker are the worker's (D3), and `MissingReplies` is exported so the worker can re-apply FR-5 AC2 once it knows which `deferred_to` items are open.
- `marker.go`: `Record`, `EncodeMarker`, `DecodeMarker`, `FindMarker`.
- `tasks.go`: `TaskList` and `Task` types with `yaml` and `json` tags. No YAML parser is added (D12).
- `v1/{input,result,records,tasks}.schema.json` (draft 2020-12).

Tests use `github.com/santhosh-tekuri/jsonschema/v5` (no dependencies of its own) to validate every spec example against its schema, and to cross-check that the schema and `ValidateResult` agree on the shape cases. It is a test-only import, so it never reaches core.

## Steps

- [x] Version negotiation, embedded schemas and the input types, with tests for unknown input fields and unsupported versions.
- [x] Result types, `ParseResult` and `ValidateResult` with table tests for every §5 rule (passing and failing case each).
- [x] Record types and the marker codec, with round-trip and malformed-marker tests.
- [x] Task list types and schema; schema tests for every spec example (§4–§7).

## Acceptance criteria

- Table tests cover every rule in INTEGRATION-SPEC §5 with a passing and a failing case (FR-18 AC3).
- Each example JSON in INTEGRATION-SPEC §4–§6 validates against its schema.
- A marker round-trips, and malformed markers are rejected.
- A result for contract `1.1` given to a `1.0` core is reported as unsupported (§2).

## Decisions

- **D1** [gap] INTEGRATION-SPEC §5.1: a result's `run_id` must equal the input's `run_id`. Why: a result left behind by an interrupted run must not be taken for the rerun's (§4.1 gives each start its own `run_id`).
- **D2** [gap] §2: a result's `contract_version` is supported when its major is 1 and its minor is at most the core's (`1.0`). A result may name a lower minor than the input. `ParseInput` accepts any `1.x` input and rejects another major. Why: §2 says what a minor may add, not how the two sides compare.
- **D3** [ambiguity] §5.2, §5.3: `ValidateResult(input, result, outDir)` checks every rule that can be decided from those three. The rules that need the repository or the tracker stay with the worker's stage activity: implement `head` is the branch's remote head; a finding's `path` exists at `commit` and its `line` is in the file; `deferred_to` names an open item; the `base_update` merge commit's parents; mint's plan branch contents and that `plans/<plan_key>/` is new on the base. `MissingReplies` is exported so the worker can re-apply FR-5 AC2 with a `deferred_to` check. Reading taken: the issue's signature has no repository access.
- **D4** [gap] §5.1: the object named by `result` must be present for every result, including `"complete": {}`.
- **D5** [gap] §5.1: a `*_file` path must be relative, contain no `..` segment, resolve (through symlinks) to a regular file inside `AGENT_LOOP_OUT`, be at most 65,536 bytes and be valid UTF-8. An empty file is allowed.
- **D6** [gap] §5.1: `failed.reason` is required, non-empty, has no line break, and is at most 500 Unicode code points.
- **D7** [gap] §5.2: required fields are implement `change.title` (non-empty) and `change.body_file`; review `summary_file`; each new finding's `title` (non-empty) and `body_file`; each reply's and manual reply's `body_file`. `note_file` is optional. `rulings`, `manual_rulings`, `new_findings`, `replies` and `manual_replies` may be omitted or `null`, meaning empty.
- **D8** [ambiguity] §5.2: "exactly one entry for each ID" is read as one-to-one: a ruling for an ID not in `rule_findings` (or `rule_manual`), or a second ruling for an ID, makes the result invalid. The same holds for address replies against `answer_findings` and `answer_manual`.
- **D9** [gap] §5.2 review: a new finding's `path` is a repository-relative slash path: not empty, not absolute, with no `.` or `..` segment. `line` is at least 1.
- **D10** [gap] §5.2 address: `deferred_to` may be non-null only on a `deferred` reply, for findings and manual comments alike.
- **D11** [ambiguity] §5.2 address: "the input head" is the input's `workspace.head`. In `base_update` mode `done.head` must differ from it (the parent checks are the worker's, D3).
- **D12** [gap] §7: this task adds the `tasks.yaml` types and `tasks.schema.json` only. Parsing YAML and the materialise checks (duplicate keys, cycles, body files) come with materialise in v0.2. Why: no v0.1 consumer, and `contract/` stays standard-library only.
- **D13** [gap] §5.2, §6: a commit SHA is 40 (SHA-1) or 64 (SHA-256) lowercase hex characters.
- **D14** [gap] §6.1: a marker is exactly `<!-- agent-loop:v1 ` + compact JSON + ` -->` on one line. The JSON is written with HTML escaping, so `-->` can't occur inside it. `FindMarker` reads only the body's last non-blank line, since every record ends with its marker. A marker anywhere else in the text is ignored.
- **D15** [gap] §6: every record needs `type` (one of the eleven in §6.1) and a non-empty `key`. Fields per type: `change` {item, n}; `review` {item, change, commit, kind, round, findings[{id, score, status, comment}], manual_rulings[{comment, ruling}], stale}; `finding` {change, finding, score}; `reply` {change, finding, kind, deferred_to, head}; `manual_reply` {change, comment, kind}; `status` {item, event, reason, claim, finding_ids, comment_ids, commits, rounds_added}; `approved_commits` {item, claim, commits}; `followup` {item, change}; `task` {plan, task}; `plan` {plan, tasks[{task, item}]}; `proposal` {plan}. `v` is optional and, when present, 1. Unknown fields are allowed on read so a minor version can add fields. The §6.4 reply examples show no `key`, but §6.1 makes `key` the lookup for every record, so the test examples add one.
- **D16** [gap] §6.5: `status.reason` is a lowercase snake-case code (schema pattern `^[a-z][a-z0-9_]*$`). Go constants cover the codes the specs name (`round_limit`, `untrusted_commit`, `branch_outside_prefix`, `empty_review_needs_dismissal`, `worker_lost`, `gate_failed`, `pusher_unknown`). The core tasks add the rest. Why: the full list is settled by the escalation rules in T5–T7, not here.
- **D17** [gap] §5.1 `usage`: `cost` is a number at least 0, `input_tokens` and `output_tokens` are integers at least 0, `currency` is a non-empty string.
- **D18** [gap] §5.1 `blocked.dependency`: when present, a non-empty item ref.
- **D19** [ambiguity] FR-18 AC1 lists `materialise`, but INTEGRATION-SPEC §4.2 gives it no handler input. `ValidateResult` accepts the four handler stages (implement, review, address, mint) and rejects any other.
- **D20** [open] §2, §5: a result with duplicate JSON keys is decoded with the last value winning, as Go and JSON Schema validators do. The spec doesn't say whether duplicates should be rejected.
- **D21** [gap] §4: `input.schema.json` requires every §4.1 field (`change` may be `null`), requires the `review`, `address` or `mint` object for its stage, limits `identity` to `coder` or `reviewer`, `attempt` to at least 1 and `blocking_threshold` to 1–5, and allows additional properties everywhere (§2).
- **D22** [gap] §7: in `tasks.schema.json` `depends_on` is optional and has no duplicates, `body` is a relative path with no `..` segment, `type` is any non-empty string (the profile decides which are configured), and unknown fields are rejected.

## Verification

Standard checks. Schema tests run offline against the embedded files.

## Risks and open questions

- Record fields beyond those the spec names are a gap; each choice is recorded in Decisions so the owner can fold it into §6.

## Outcome

Done as planned. `contract/` stays standard-library only; the one new module requirement, `github.com/santhosh-tekuri/jsonschema/v5` (no dependencies of its own), is imported by tests only. `ValidateResult` has about 150 table cases, each failing case also checked to come back as a `failed` result with a one-line reason, and each shape-only failing case also checked to be rejected by `result.schema.json`; every valid case must pass the schema too. The spec examples (§4–§7, placeholders filled) are in `contract/testdata/examples`. The four steps landed in one commit because the shared test helpers and examples span them. No follow-up issues filed: the repository and tracker checks of D3 belong to the worker stage-activity task already in the plan.
