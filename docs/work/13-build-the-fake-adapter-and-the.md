# Work plan: Build the fake adapter and the conformance suite

**Issue:** [#13](https://github.com/mayusuk/agent-loop/issues/13) (feature)
**Branch:** `issue/13-build-the-fake-adapter-and-the`
**Base:** `origin/main @ f2266b4` (checked before branching)
**Status:** Draft

## Summary

Adds `internal/adapters/fake`, an in-memory, scriptable `WorkTracker` and `CodeHost` that tests can drive as the owner, another human, a listed review bot or any of the loop's identities, with kill points for crash tests. Adds `conformance.Run(t, factory)`, the adapter suite C1–C16 of INTEGRATION-SPEC §11, and runs it against the fake on every commit.

## Context

- INTEGRATION-SPEC §11 (C1–C16) lists the behaviours. §6.1 (record authors and lookup places), §6.2 (change link), §8 (ports), §9.3–§9.8 (GitHub behaviour the fake mirrors: claim, feed, dependencies, manual comments, gates, merge, trust) give the rules.
- TECHNICAL-SPEC §14 and T16: the fake is scriptable so every FR-12 AC4 row can be tested, and passes the same suite as GitHub. Plan task T8.
- `internal/ports` (#8) is on main. Its work plan fixes the names (D5), event shapes (D6), `Record` (D7), `TrustReport` (D12) and the `UntrustedText` rules (D2, D3): only adapters, workflows and `conformance/` may import `internal/ports/untrusted`.
- PRODUCT-SPEC FR-12 AC4: the manual actions the fake must be able to script (v0.1 rows only; the plan-proposal rows are v0.2).
- Tests must not call GitHub (`.wf.toml` notes). The GitHub run of the suite is T9/T10 and is skipped unless `AGENT_LOOP_LIVE=1`.

## Approach

**`conformance/`**
- `Env` is one fresh sandbox project: the two ports, a `Driver`, the actors (owner, other human, listed bot, coder, reviewer, loop), base branch, branch prefix, required gates and feed page size. `Factory func(*testing.T) Env`. `Run(t, f)` runs one subtest per case, `TestC1_...` to `TestC16_...`, each on a fresh `Env`.
- `Driver` performs sandbox actions as a given actor: create, label, assign, comment, close and reopen items; push commits, merge the base into a branch, open, merge, close and reopen changes (also from a fork); review comments, replies, resolve, unresolve, reviews and dismissals; tags, gate results, required approvals, ruleset changes, admins; and coder runs (the worker's run records, I11). Refusals come back as `ports.ErrPermission`. The GitHub adapter supplies its own driver later.
- Files: `conformance.go` (Env, Driver, Run, helpers), `tracker.go` (C1 tracker half, C2–C5, C12), `codehost.go` (C1 host half, C6–C9, C14, C16), `trust.go` (C10, C13, C15), `untrusted.go` (C11, a reflective scan that no plain string carries human text).

**`internal/adapters/fake/`**
- `Fake` holds the state behind one mutex: items, comments, commits and branches, changes, threads, reviews, gate results, rulesets, admins, coder runs, two event logs, a logical clock and kill points. `f.Tracker()` and `f.Host()` return the two ports (they can't be one type: both have `Capabilities`, `Validate` and `Changes`). Scripting methods on `*Fake` implement `conformance.Driver` structurally, so the fake package doesn't import `conformance`.
- Files: `fake.go` (config, state, clock, ids, kill points), `tracker.go`, `codehost.go`, `trust.go`, `script.go` (the driver), plus tests: `conformance_test.go`, `fake_test.go` (kill points, FR-12 AC4 rows, edge cases).

## Steps

- [x] Fake core state, clock, kill points and the `WorkTracker`.
- [x] The `CodeHost` (changes, reviews, manual comments, gates, merge) and the driver.
- [x] Trust, rulesets, admins and pusher attribution.
- [x] `conformance.Run` with C1–C16, run against the fake.
- [ ] Fake unit tests: the FR-12 AC4 rows, kill-point crash tests, and edge cases the suite doesn't reach.

## Acceptance criteria

- `conformance.Run` covers every case C1–C16, each as a named subtest (`TestC1_...`).
- The fake adapter passes the whole suite in CI.
- The fake can be scripted to produce each manual action in PRODUCT-SPEC FR-12 AC4 for v0.1.

## Decisions

- **D1** [gap] INTEGRATION-SPEC §11: the suite's API is `conformance.Run(t, Factory)` with `Factory func(*testing.T) Env`. `Env` carries the ports, a `Driver` that acts in the sandbox as a given `ports.Actor`, the six actors (owner, another human with write access, a listed `manual_comment_bots` bot, coder, reviewer, loop), the base branch, the branch prefix, at least one required gate, and the feed page size. Driver refusals are `ports.ErrPermission`. A branch named `<login>:<name>` is in that account's fork, and every `Driver.Push` changes `conformance.ChangedFile`, so findings can point at a line in the diff. Why: the suite must script humans and identities the same way against every adapter; the GitHub driver will use real accounts.
- **D2** [gap] INTEGRATION-SPEC §8.2, §8.3 `Changes`: an empty `Cursor` starts at the feed's current end and returns no events. A call may return only part of what is pending (one page). The caller repeats with the returned cursor until a call returns no events. Why: C12 needs a paging contract that every adapter and the core share; on first start the full reconcile covers current state, so replaying history isn't needed.
- **D3** [gap] INTEGRATION-SPEC §8.3, §6.3: a reply to a finding with no thread is posted with `ThreadRef(<change ref>)`, the change itself. Why: `ThreadRef`'s doc names "the change itself" but not its value.
- **D4** [gap] INTEGRATION-SPEC §9.8 step 6, I11, C15: coder runs reach a code-host adapter as a run log given when it is built (in the conformance driver, `StartCoderRun(item)` returns a function that ends it), not through a port method. A head move's `Pusher` is decided when it is first observed (by `Changes` or `Trust`) and kept: `Coder` if a coder run for the same item was live at the push, `Owner` if no coder run overlapped [previous `Changes` call, now], `Unknown` otherwise. A coder push with no live coder run is refused, because its token is revoked (TECHNICAL-SPEC §7.3). Why: the ports have no run-record method, and the window is the spec's [t − Δ, t].
- **D5** [ambiguity] INTEGRATION-SPEC §9.8 step 3: `Trust` reports, over the head's commits not reachable from the base tip: `fork` and `branch_outside_prefix` for all of them; `ruleset_missing` for all of them while a required ruleset is removed or disabled; `ruleset_changed` for those pushed before any later ruleset change, disable, enable, removal or restore (I9; "unchanged since the last check" is read as "unchanged since the commit arrived"); `pushed_before_ruleset` for those pushed while the loop-branch ruleset wasn't active; `admin_not_owner` for all of them while an admin isn't an owner account, and for those pushed while one was; `pusher_unknown` for pushes whose `Pusher` is `Unknown`. Causes are in that order, commits in push order. Owner approvals are the core's (#8 D12). Why: the adapter doesn't know the core's last trusted head, and per-commit times let an owner approval end the escalation.
- **D6** [gap] INTEGRATION-SPEC §8, NFR-2: writes that leave no record are idempotent by key: a repeated `Claim` with a key that already claimed returns the item's current version, a repeated `Merge` of a change merged at `pinned` returns `Merged`, and markers, labels, dependencies, closes and resolves are set-to-value. Writes that leave a record (`Comment`, `CreateItem`, `OpenChange`, `PostReview`, `PostReply`) look for the key from the same author in the §6.1 place.
- **D7** [open] INTEGRATION-SPEC §9.3 step 4 says GitHub's `Claim` posts the `claim` status record, but `Claim` takes no `Record`, so it can't. The fake's `Claim` posts nothing; the core posts the claim record with `Comment`. T9 should settle this in the spec.
- **D8** [gap] INTEGRATION-SPEC §8 error classes the spec leaves open: `OpenChange` on a branch that doesn't exist is `ErrNotFound`, and with an open change already on the branch (another key) is `ErrConflict`; `Close` with `CloseOpen` is `ErrConflict`; `ResolveFinding` for an unknown finding is `ErrNotFound`, and for a finding with no thread does nothing; `Merge` of a closed change is `NotMergeable`.
- **D9** [gap] INTEGRATION-SPEC §9.7, C14: the fake's `Merge` is `NotMergeable` while a required gate on the head isn't `Pass`, while fewer approvals than required exist (a non-dismissed `APPROVE` by anyone but the change's author counts), or while the head lacks the base tip ("require branches to be up to date", which `init` turns on). Why: these are the GitHub refusals the core must re-check (I15).
- **D10** [gap] INTEGRATION-SPEC §9.7, C9: `Gates(ref, c)` reports each required gate's latest result on `c` itself. A result on another commit never counts, a required gate with no result on `c` is `Missing`, and checks that aren't required aren't reported.
- **D11** [gap] TECHNICAL-SPEC §14, plan T8 "kill points": `f.KillAt(method, BeforeWrite|AfterWrite)` arms a one-shot crash in the named port write method. The call returns `fake.ErrKilled`, which is `ports.ErrTransient`, either before or after its write is applied, so a retry with the same key can be tested. Why: a crash between a write and its reply is the case NFR-2 exists for.

## Verification

Standard checks. The FR-12 AC4 test names each v0.1 row.

## Risks and open questions

- The suite is written before the GitHub adapter; a GitHub behaviour that doesn't fit a case is a `spec` issue for T9/T10, not a change to the fake alone.

## Outcome

_Filled in before shipping._
