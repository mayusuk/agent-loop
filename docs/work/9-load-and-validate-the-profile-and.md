# Work plan: Load and validate the profile and deployment config (`internal/profile`)

**Issue:** [#9](https://github.com/mayusuk/agent-loop/issues/9) (feature)
**Branch:** `issue/9-load-and-validate-the-profile-and`
**Base:** `origin/main @ 399150e` (checked before branching)
**Status:** In progress

## Summary

Adds `internal/profile`, which loads the project profile (`.agent-loop.toml`) and the deployment config (`~/.config/agent-loop/deployment.toml`), checks each against an embedded JSON Schema plus the cross-field rules a schema can't express, fills in every default from the TECHNICAL-SPEC §13 and INTEGRATION-SPEC §13 key tables, and returns a typed `Profile`, `Deployment` and `Policy`. Every problem is reported, each naming its file and key.

## Context

- TECHNICAL-SPEC §13 (key table, deployment config contents), T17 (TOML plus JSON Schema), §3.1 (`internal/profile` owns loading, schema validation and defaults), §4 (`Policy`: the limits and names from the profile), §6.4 (capabilities), §6.5 (time limits), §7.2 (runners, default egress allowlist), §11 (API socket and gateway).
- INTEGRATION-SPEC §13 (contract-relevant keys and defaults), §8.1 (marker names, `Actor.ID` is a string), §2 (`contract = "MAJOR.MINOR"`), I13 (owners matched by ID).
- PRODUCT-SPEC §5.1 (default label names, all overridable: FR-1 AC4), FR-13 AC1 (three different identities, owners are not identities), FR-16 AC1 (2h, 2 attempts), FR-16 AC3 and D35 (daily run limit can't be turned off), FR-16 AC4 (time zone).
- `internal/core` may import only the standard library and `contract/`, so it can't import `internal/profile`. `Policy` therefore holds only standard-library types.
- Tests must not call GitHub. Nothing here does any I/O beyond reading the two files.

## Approach

Loading a file is three passes, and all problems from all passes are collected into one `*Error` (a list of `Problem{File, Key, Message}`):

1. **Decode** the TOML (`github.com/BurntSushi/toml`) into a generic map, then into JSON values.
2. **Schema**: validate against `schema/profile.schema.json` or `schema/deployment.schema.json` (embedded, `github.com/santhosh-tekuri/jsonschema/v6`). `additionalProperties: false` everywhere gives unknown-key errors. Ranges and enums live in the schema. Schema errors are turned into problems keyed by the dotted TOML path.
3. **Semantics**: decode into typed structs, apply defaults, and check what a schema can't: interval ordering, durations in range, type markers that exist, unique label mappings, distinct identities, `follow_up.type` configured, `limits.agent_runs` against the deployment limit, time zone names.

Public API:

- `LoadDeployment(path) (*Deployment, error)` and `LoadProfile(path, dep *Deployment) (*Profile, error)` (`dep` nil means deployment defaults).
- `(*Deployment).LoadProjects() (map[string]*Profile, error)` loads every registered profile.
- `(*Profile).Policy() Policy` gives the core's view: limits, names, intervals, stage limits, marker mapping, owner IDs.
- `(Policy).WithIdentityIDs(coder, reviewer, loop string) (Policy, error)` adds the identities' account IDs once they are known, and rejects an owner that is one of them (FR-13 AC1, D2).

Files: `internal/profile/{errors,load,profile,deployment,policy}.go`, `internal/profile/schema/*.json`, tests and `testdata/`. `go.mod` gains the two libraries.

## Steps

- [x] Loader core: TOML decode, embedded schemas, schema errors as problems naming file and key.
- [x] Profile: schema, typed struct, defaults, semantic checks, tests for every default and range boundary.
- [ ] Deployment config: schema, defaults, project registration, `LoadProjects`, tests.
- [x] `Policy` and `WithIdentityIDs`, tests (FR-13 AC1, FR-1 AC4, FR-16 AC3).

## Acceptance criteria

- Tests show every default in the TECHNICAL-SPEC §13 key table, and every range boundary (accepted and rejected).
- `limits.daily_agent_runs = 0` and `= "off"` are rejected (TestFR16AC3_...).
- Every §5.1 label name can be overridden (TestFR1AC4_...).
- An unknown key gives an error that names the key and its file.

## Decisions

- **D1** [gap] TECHNICAL-SPEC §4: `profile.Policy` holds only standard-library types, because `internal/core` can't import `internal/profile`. The core's own `Policy` (T5) is filled from it by the workflow layer. Why: keeps the import rule of §3.1.
- **D2** [gap] FR-13 AC1, INTEGRATION-SPEC §13: the profile names identities by secret reference, not account ID, so loading can only check that the three references are different. The owner-is-not-an-identity check runs in `Policy.WithIdentityIDs`, once the identities' account IDs are known (from the secret store or adapter), and `project validate` (T18) calls it. Why: the account IDs aren't in the files.
- **D3** [gap] INTEGRATION-SPEC §13, §8.1: `identities.owners[].id` is a positive integer (GitHub) or a non-empty string (Jira `accountId`), kept as a string to match `Actor.ID`. Owner IDs must be unique, and at least one owner is required. Why: the spec's example is an integer, but the ports are tracker-neutral.
- **D4** [gap] TECHNICAL-SPEC §13 `markers`, INTEGRATION-SPEC §8.1: `[markers]` keys are the loop markers of §8.1 (`ready`, `in_progress`, `blocked`, `needs_owner`, `paused`, `aborted`, `follow_up`, `plan`, `plan_proposal`) plus `"type:<type>"` for each configured type. Defaults are the §5.1 names, and the type name for a type marker. Each value has exactly one of `label` or `status`. Two markers can't map to the same label or status. Why: one label for two markers would make the loop's state ambiguous.
- **D5** [gap] TECHNICAL-SPEC §13 `priority`: each entry before the last is a configured type marker, used at most once; the last is `oldest` or `newest`. When `priority` is omitted, the default drops type markers that aren't configured and appends configured types it doesn't name, in `types` order, before `oldest`. Why: changing `types` alone shouldn't make the default invalid.
- **D6** [gap] TECHNICAL-SPEC §13 `follow_up.type`: if the default `feature` isn't a configured type, loading fails and asks for `follow_up.type`. Why: no safe guess.
- **D7** [gap] TECHNICAL-SPEC §13 `limits`: `limits.daily_spend` is a number greater than 0. `limits.agent_runs` defaults to 2, or to the deployment limit when that is lower. Why: a deployment limit of 1 shouldn't make every profile invalid.
- **D8** [gap] TECHNICAL-SPEC §13 `timezone`: must be an IANA name; `Local` and the empty string are rejected. The IANA database is embedded (`time/tzdata`), so validation doesn't depend on the host. Why: `Local` isn't an IANA name, and omitting the key already means local time.
- **D9** [gap] TECHNICAL-SPEC §13, §6.4, §7.2, INTEGRATION-SPEC §13 `stages`: `implement`, `review` and `address` are required (all used in v0.1); `mint` is optional; `merge` is optional and takes only `time_limit` and `attempts` (FR-16 AC1). `time_limit` is a Go duration from `1m` to `24h`, `attempts` is 1 to 10. Capability names match `^[a-z][a-z0-9_-]*$` and are unique. `runner` is `local`, `local-unsandboxed` or `docker`, and `docker` adds the `docker` capability. `contract` must have the form `MAJOR.MINOR`; whether the core supports it is checked by `project validate` (T18). Why: the spec gives no ranges, and these are permissive enough for real use.
- **D10** [ambiguity] TECHNICAL-SPEC §13 (adapter settings are checked by the adapter): the schema knows the `github` adapter's settings (`repo`, required, `owner/name`), so a typo there is an unknown key at load time. The adapter's `Validate` still checks reachability. Read as: the adapter owns the meaning, the profile schema owns the shape. Why: keeps "unknown keys are rejected" true for the whole file.
- **D11** [gap] INTEGRATION-SPEC §13 `gates`: omitted means "the base branch's required checks" (resolved by the adapter); an explicit `[]` means no gates. Why: the two cases need to be told apart.
- **D12** [gap] INTEGRATION-SPEC §13 `egress`: `egress.allow` entries are lower-case host names, optionally with a leading `*.`. The effective list is the §7.2 default (with the GitHub hosts for the `github` code host) plus `allow`, or only `allow` when `replace = true`.
- **D13** [gap] INTEGRATION-SPEC §13 `branch_prefix`, `plans_dir`: `branch_prefix` is non-empty, made of `A-Z a-z 0-9 . _ / -`, doesn't start with `/` or `-`, and doesn't contain `..`. `plans_dir` is a relative path without `..`. `base_branch` follows the same character rule.
- **D14** [gap] TECHNICAL-SPEC §13 deployment config: key names and defaults are `temporal.address` (`localhost:7233`), `temporal.namespace` (`default`), `projection.path` (`~/.local/state/agent-loop/projection.db`), `api.socket` (`~/.config/agent-loop/api.sock`, §11), `api.gateway.enabled` (`false`), `api.gateway.address` (`127.0.0.1:7480`, must be a loopback address), `api.gateway.token` (a `secret://` reference, required when the gateway is on), `notifiers` (`["desktop", "tracker"]`), `secrets.backend` (`keychain` on macOS, `envfile` elsewhere), `secrets.path` (`~/.config/agent-loop/secrets.env`, `envfile` only), `limits.agent_runs` (4, 1 to 100), and `projects.<name>.profile`. Why: the spec lists the contents but not the names.
- **D15** [gap] TECHNICAL-SPEC §13 projects: project names match `^[A-Za-z0-9][A-Za-z0-9._-]*$`. A leading `~/` in any deployment path is the home directory, and a relative profile path is relative to the deployment file's directory.
- **D16** [open] FR-15: the deployment config accepts several projects. Whether the v0.1 server refuses more than one is left to the server (T17).
- **D17** [gap] FR-1 AC2: loading reports every schema problem, not just the first, each as `<file>: <key>: <message>`. Keys are dotted TOML paths, with list items as `[n]` (`identities.owners[0].id`). The cross-field checks run only when the schema passes, and then all of theirs are reported too. Why: cross-field checks need values of the right types.
- **D18** [gap] TECHNICAL-SPEC §13 `intervals`, `time_limit`: durations are Go duration strings (`"10m"`, `"60s"`, `"2h"`).
- **D19** [gap] INTEGRATION-SPEC §13: `manual_comment_bots` entries are non-empty and unique; `release_apps` entries are positive integers.

## Verification

Standard checks only. All tests are unit tests over files in `testdata/` and temporary directories.

## Risks and open questions

- T2 (`contract/v1`) runs in parallel and may also add a JSON Schema library. Using the same one keeps `go.sum` merges trivial.

## Outcome

_Filled in before shipping._
