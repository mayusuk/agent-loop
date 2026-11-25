# Work plan: project validate can't see ruleset bypass actors, and the runtime trust check skips them

**Issue:** [#101](https://github.com/mayusuk/agent-loop/issues/101) (bug)
**Branch:** `issue/101-project-validate-can-t-see-ruleset`
**Base:** `origin/main @ f4771cf` (checked before branching)
**Status:** Done

## Summary

GitHub shows a ruleset's `bypass_actors` only to a token with write access to the ruleset, so the loop app's `administration: read` token never sees them. That made `project validate` report false `ruleset_bypass_missing` errors, and it made the runtime trust check pass the bypass-list check without looking at anything. Now the owner's login reads the bypass lists in full during `init` and `project validate --owner-login`, and the projection keeps what it saw. The loop's token accepts a hidden bypass list only when the ruleset hasn't changed since the owner's read. Any other hidden bypass list fails closed.

## Context

- INTEGRATION-SPEC §9.8 step 1: the bypass list of each required ruleset may contain only the named app and the admin role. Step 3: before every implement, address and merge, `Trust` checks the rulesets "still exist, unchanged, and are active". I9 and IQ3 make that check strict. §9.1: the loop app has only `administration: read`, so the loop can't be given more access to fix this.
- GitHub REST docs ("Get a repository ruleset"): "the bypass_actors property is only returned if the user making the API request has write access to the ruleset". `current_user_can_bypass` (`always`, `pull_requests_only`, `never`, `exempt`) is returned on the repository endpoints and describes only the caller. `GET rules/branches/{branch}` returns the rules with no bypass information. Measured in the e2e run (issue body): the owner's token sees the list and the loop token gets no `bypass_actors` field.
- Root cause: `rulesetDetail.BypassActors` decodes a missing field as an empty list. `misfit` (trust.go) then finds no bad actor, and `validateRulesets` (hostvalidate.go) finds neither the coder app nor the loop app.
- `init` (cmd/agent-loop/init.go) already holds the owner's token in memory, and its Setup reads rulesets with it (`AsOwner`). Its step 8 calls `validateProject`, which used the loop token only.
- The projection DB already holds the coder-run records that `Trust` reads (`projection.CoderRuns`, work plan #21 D3), and handlers can't read it (TECHNICAL-SPEC §7.2).

## Approach

- **Read model** (`internal/adapters/github/trust.go`): `bypass_actors` decodes as a pointer, so a missing field differs from an empty list. `current_user_can_bypass` is decoded too. A new port `RulesetRecords` (HostConfig `Rulesets`) keeps, for each ruleset ID, the `updated_at` and the bypass list the owner's token saw. `Host.rulesets` reads with the owner's client when the host has one (HostConfig `OwnerClient`, for init and `validate --owner-login`) and saves every ruleset it read as the record set for the project. With the loop's token, a hidden list is filled in from the record when the record's `updated_at` equals the ruleset's, and when `current_user_can_bypass` agrees with the record about the loop app. Otherwise the ruleset is marked `unverified`.
- **Trust**: `misfit` judges the visible parts as before. A required ruleset that fits but is unverified makes every commit on the change untrusted with the cause `ruleset_changed`, not only the commits pushed before the change. This lasts until the owner verifies again. A loop-branch or tag ruleset whose `current_user_can_bypass` says the loop app bypasses it doesn't fit.
- **Validate** (`hostvalidate.go`): a fitting ruleset that is unverified reports `ruleset_bypass_unverified` (error) with the fix `agent-loop project validate --owner-login`. The `ruleset_bypass_missing` checks run on the filled-in list. The loop app's own bypass of the base ruleset is also read from `current_user_can_bypass`.
- **Projection**: a `rulesets` table (project, ruleset ID, updated_at, bypass actors as JSON), with `projection.Rulesets` implementing the port. Saving replaces the project's rows.
- **CLI**: `project validate --owner-login [--oauth-client-id]` gets the owner's token the way init does, holds it in memory only, and passes an owner client to the host. init step 8 passes the token it already has. Both open the projection DB for the records. The server's host gets `projection.Rulesets` (wire.go).
- **Stub** (`githubtest`): `GET /rulesets/{id}` leaves out `bypass_actors` unless the caller is an admin, and returns `current_user_can_bypass` for the caller on both list and get. The driver gets a ruleset action `add_bypass`.
- **Conformance**: C17. An actor added to a required ruleset's bypass list makes the open change untrusted, and so is a commit pushed after it. The fake implements `add_bypass`.

## Steps

- [x] Stub hides `bypass_actors` from non-admin tokens and adds `current_user_can_bypass`, with a failing test showing that validate reports no false `ruleset_bypass_missing` and that `Trust` catches an extra bypass actor (both fail before the fix).
- [x] Adapter: records port, owner client, filled-in or unverified rulesets, trust and validate changes. Test helpers record the owner's view like init does.
- [x] Projection `rulesets` table and `projection.Rulesets`.
- [x] CLI: `validate --owner-login`, init step 8 with the owner token, server wiring.
- [x] Conformance C17 (`add_bypass`), in the GitHub stub driver and the fake; INTEGRATION-SPEC §11 lists only C1–C16, so C17 is recorded as a Decision.

## Acceptance criteria

- `project validate` and init's validate step report no false `ruleset_bypass_missing` when the rulesets are right.
- The runtime trust check no longer passes the bypass-list check when it can't see the list: an extra bypass actor is caught.
- The stub drops `bypass_actors` for non-admin tokens, as GitHub does.
- A conformance case covers it.

## Decisions

- **D1** [gap] INTEGRATION-SPEC §9.8 steps 1 and 3, §9.1, I9: the loop app's `administration: read` token can't see bypass lists, and the spec gives it no more access. So `Trust` accepts a hidden bypass list only from the owner's last read of that ruleset, and only when the read has the ruleset's current `updated_at` and agrees with the loop app's own `current_user_can_bypass`. Any other hidden list leaves the ruleset unverified. Then every commit on the change is untrusted with `ruleset_changed`, including commits pushed after the change, until the owner reads the rulesets again. This is stricter than I9, which marks only commits pushed before the change. Why: it fails closed, and it is the only way to notice an extra bypass actor without the owner's token. After a ruleset change the owner runs `project validate --owner-login` (or `init`) and then resumes. GETTING-STARTED §8 has a row for this.
- **D2** [gap] INTEGRATION-SPEC §12.2 step 8, FR-1 AC5, FR-13 AC1: init's validate step and `project validate --owner-login` read the rulesets with the owner's login, held in memory only (the gh CLI's token, or the device flow with `--oauth-client-id`). They record each ruleset's ID, `updated_at` and bypass list, never the token. They only read from GitHub. Plain `project validate` and the server use the record.
- **D3** [gap] FR-1 AC2, INTEGRATION-SPEC §9.8: there is a new error `ruleset_bypass_unverified` for a required ruleset whose bypass list is hidden and unrecorded, with the fix `agent-loop project validate --owner-login`. The `--owner-login` and `--oauth-client-id` flags are added to `project validate`. `--oauth-client-id` without `--owner-login` is a usage error (exit 2), so the owner never thinks the rulesets were recorded when they weren't. The base ruleset's `ruleset_bypass_missing` for the loop app is read from the loop app's own `current_user_can_bypass` when its list is hidden. The coder app's check needs the record. The help line for `project` is unchanged.
- **D4** [gap] TECHNICAL-SPEC T11, §10: the record lives in the projection DB, in a new table `rulesets (project, ruleset_id, updated_at, bypass)` with the bypass list as JSON. Each owner read replaces the project's rows. The table is additive, so the schema version stays 1. If the projection is lost, every ruleset becomes unverified until the owner reads them again. `project validate` now opens the projection and reports `projection_unavailable` when it can't.
- **D5** [gap] INTEGRATION-SPEC §11: there is a new conformance case C17. An actor added to a required ruleset's bypass list (driver action `add_bypass`) makes the change's earlier commits and a commit pushed after it untrusted with `ruleset_changed`. It runs for all three rulesets on the GitHub stub and the fake. §11 and CLAUDE.md still list C1–C16.
- **D6** [gap] INTEGRATION-SPEC §9.8 step 1: a loop-branch or tag ruleset doesn't fit when the loop app's `current_user_can_bypass` says the loop app bypasses it. `always`, `pull_requests_only` and `exempt` all count as bypassing.
- **D7** [gap] INTEGRATION-SPEC §9.8 step 1 (optional fourth ruleset): an other-branches ruleset whose bypass list is hidden and unrecorded doesn't count as keeping the coder app off other branches, so `ruleset_other_branches_missing` is still warned.
- **D8** [open] IQ4: this assumes that every edit to a ruleset, its bypass list included, moves `updated_at`, which the existing `ruleset_changed` check also assumes. `updated_at` has one-second precision, so an edit in the same second as both the previous edit and the owner's read would go unnoticed. Both need checking on the sandbox repository.

## Verification

The four checks from `.wf.toml`, plus `GOOS=linux go build ./...` and `CGO_ENABLED=0 GOOS=darwin go build ./...`.

## Risks and open questions

- That any change to a ruleset's bypass list moves its `updated_at` is assumed, as the existing `ruleset_changed` check already assumes. To confirm on the sandbox repository with IQ4.

## Outcome

Done as planned. `githubtest` hides `bypass_actors` from non-admin tokens and serves `current_user_can_bypass`. `github.Host` takes a `RulesetRecords` store and an optional `OwnerClient`: it fills hidden lists in from the owner's record, or marks the ruleset unverified (trust.go, hostvalidate.go). `projection.Rulesets` stores the record. `project validate --owner-login` and init's step 8 read with the owner's token, and the server's host reads the record (wire.go). The conformance suite gets C17. Tests: `TestFR13AC5_HiddenBypassLists`, `TestFR13AC4_ValidateHiddenBypassLists`, `TestFR13AC5_RulesetRecordsRoundTrip`, `TestFR1AC2_ValidateOwnerLoginReadsBypassLists`, and C17. Existing tests now record the owner's read the way init does. No deviations from the approach, and no follow-up issues.

Review round 1 (PR #120): all four findings led to changes, none pushed back. R1 removed the committed `cmd/agent-loop/agent-loop` binary and `.gitignore` now ignores builds under `cmd/`. R2 added `TestFR1AC5_ValidateStepReadsBypassListsAsOwner`. R3 added `TestFR1AC2_ValidateOwnerLoginFailsWithoutLogin` and `TestFR1AC2_ValidateReportsProjectionUnavailable`. R4 made `--oauth-client-id` without `--owner-login` a usage error (D3), tested by `TestFR1AC2_ValidateOAuthClientIDNeedsOwnerLogin`.
