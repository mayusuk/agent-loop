# Work plan: project validate can't see ruleset bypass actors, and the runtime trust check skips them

**Issue:** [#101](https://github.com/mayusuk/agent-loop/issues/101) (bug)
**Branch:** `issue/101-project-validate-can-t-see-ruleset`
**Base:** `origin/main @ f4771cf` (checked before branching)
**Status:** Draft

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

- [ ] Stub hides `bypass_actors` from non-admin tokens and adds `current_user_can_bypass`, with a failing test showing that validate reports no false `ruleset_bypass_missing` and that `Trust` catches an extra bypass actor (both fail before the fix).
- [ ] Adapter: records port, owner client, filled-in or unverified rulesets, trust and validate changes. Test helpers record the owner's view like init does.
- [ ] Projection `rulesets` table and `projection.Rulesets`.
- [ ] CLI: `validate --owner-login`, init step 8 with the owner token, server wiring.
- [ ] Conformance C17 (`add_bypass`), in the GitHub stub driver and the fake; INTEGRATION-SPEC §11 lists only C1–C16, so C17 is recorded as a Decision.

## Acceptance criteria

- `project validate` and init's validate step report no false `ruleset_bypass_missing` when the rulesets are right.
- The runtime trust check no longer passes the bypass-list check when it can't see the list: an extra bypass actor is caught.
- The stub drops `bypass_actors` for non-admin tokens, as GitHub does.
- A conformance case covers it.

## Decisions

None yet.

## Verification

The four checks from `.wf.toml`, plus `GOOS=linux go build ./...` and `CGO_ENABLED=0 GOOS=darwin go build ./...`.

## Risks and open questions

- That any change to a ruleset's bypass list moves its `updated_at` is assumed, as the existing `ruleset_changed` check already assumes. To confirm on the sandbox repository with IQ4.

## Outcome

_Filled in before shipping._
