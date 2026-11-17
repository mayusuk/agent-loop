# Work plan: Build GitHub trust, rulesets and permission checks

**Issue:** [#16](https://github.com/mayusuk/agent-loop/issues/16) (feature)
**Branch:** `issue/16-build-github-trust-rulesets-and`
**Base:** `origin/main @ 107d790` (checked before branching)
**Status:** Done

## Summary

Replaces the T10 placeholder `CodeHost.Trust` of the GitHub adapter with the §9.8 checks (fork, branch outside the prefix, the three rulesets, admins, pusher) and attributes every `HeadMoved` to the coder, an owner or `Unknown` from the worker's coder-run records (I11). `Validate` gains the app-permission, ruleset, admin and base-branch-rule checks. The stub GitHub learns rulesets, collaborators, forks, tags and ruleset enforcement, so the GitHub adapter now passes the whole conformance suite, C10, C13 and C15 included.

## Context

- INTEGRATION-SPEC §9.1 (identities and permissions), §9.7 (base-branch rules the loop can't meet, "up to date"), §9.8 (trust, steps 1–7), I6, I9, I10, I11, I14, §11 (C10, C13, C15). PRODUCT-SPEC FR-13 AC4–AC6, NFR-4 AC1, D40. TECHNICAL-SPEC §7.3 (tokens per run, revoked at the end), §8.3.
- Plan task T11. T10 (`docs/work/15-*.md`) D13 (placeholder `Trust`, `Pusher` always `unknown`), D17 (`RunWithoutTrust`), D19 (driver trust actions `ErrNotYet`).
- The fake (`docs/work/13-*.md`) D4 (coder run log given at construction; pusher decided at first observation, window [previous poll, now], never starting after the push) and D5 (which commits each cause names, in a fixed order). The GitHub adapter follows the same readings.
- Core #11 D3–D5: approvals are applied by the core (`TrustReport.Check`), never to a fork; the adapter reports causes and commits only. `approved_commits` records are tracker comments, already written and read by the GitHub tracker (T9).

## Approach

**`internal/adapters/github`**
- `trust.go`: `RunLog` (the worker's coder-run records) and `CoderRun`; `AppPermissions` (installed permissions of each app); the trust state (three rulesets matched by shape, the admin list) read with `GET /rulesets`, `GET /rulesets/{id}` and `GET /collaborators?permission=admin&affiliation=all`, cached for one poll interval; `Trust`; pusher attribution memoised per commit.
- `hostfeed.go`: `HeadMoved.Pusher` from the attribution; the time of the last `Changes` call that returned no events is the previous poll.
- `host.go`: `HostConfig` gains `Owners`, `Apps` (app IDs), `Runs`, `Permissions`, `ReleaseApps` and `PollInterval`.
- `hostvalidate.go`: the code-host `Validate` with the new checks.

**`internal/secrets`**: `GitHubApps.InstalledPermissions` reads an installation's permissions with the app JWT (`GET /repos/{o}/{r}/installation`), so it satisfies `github.AppPermissions`.

**`internal/adapters/github/githubtest`**: rulesets (CRUD, enforcement on pushes, ref creation, merges and tags), collaborators with permissions, forks (`POST /forks`, repository paths of a fork), app IDs for bot users, per-user contents permission, compare commits with dates. The `Driver` implements the trust actions and holds a `RunLog` (`StartCoderRun`).

**`conformance/`**: no new cases; the GitHub stub moves from `RunWithoutTrust` to `Run`.

## Steps

- [x] Stub GitHub: rulesets, collaborators, forks, tags, enforcement, app IDs; driver trust actions and run log.
- [x] Pusher attribution in `Changes` and `Trust`, and the `Trust` checks with the cached ruleset and admin state; fixture tests for each cause; conformance `Run` against the stub.
- [x] `Validate`: app permissions, rulesets, admins, base-branch rules; `secrets.GitHubApps.InstalledPermissions`; tests.
- [x] Owner approvals: an approved commit is trusted afterwards, a fork never (TestFR13AC6_).

## Acceptance criteria

- Fixture tests cover each cause in a `TrustReport`: fork, removed or changed ruleset, branch outside the prefix, a new admin, and an unknown pusher (C10, C13–C15).
- `Validate` reports each problem listed above with a fix hint (TestFR13AC4_..., TestNFR4AC1_...).
- An owner-approved commit is trusted afterwards, and a fork is never approved (TestFR13AC6_...).

## Decisions

- **D1** [gap] INTEGRATION-SPEC §9.8 step 6, I11: coder runs reach the GitHub host as a `RunLog` in `HostConfig` (`CoderRuns(ctx, from, to)` returns each run of the project, with its item, start and end, zero while live, and its head at the end, `done.head` or the branch head after `failed`, for step 6), as in fake D4. Without a `RunLog` every push is `Unknown`. The worker implements it from its run records; the conformance driver records them with `StartCoderRun`. Why: the ports have no run-record method.
- **D2** [ambiguity] INTEGRATION-SPEC §9.8 step 6: GitHub gives no push time, so a commit's push lies in a window dated by the read that first sees it, as step 6 dates a head move by the poll that first sees it. Commit data (committer or author dates) is never used, since the pusher sets it (review R1). The window starts at the previous poll (the last `Changes` call that returned no events), open at that end; before this `Host` has polled, at its first cursor's window start; before it has read the feed at all, at the zero time, so every ruleset change counts. It ends at that read: a head move at the `Changes` call; an open pull request's head that no head move attributed (its first head, or a head already there when the host started) at the feed read that saw it; a commit first seen by `Trust` at the `Trust` call. A commit no read saw takes the window of the nearest later commit one did, the push it came with. `Pusher` is `Coder` when a run of the change's item recorded the commit as its head (`CoderRun.Head`: its `done.head`, or the branch head after `failed`), or when a run of the item overlaps the window and no other item's run was live in any part of the window that the item's runs don't cover; `Owner` when no coder run of the project overlaps the window; else `Unknown` (review R3). It is decided once and kept for the life of the `Host`. Why: the spec's [t − Δ, t] and "Coder when it happened during a coder run for the same item"; fake D4. Reading taken: a push in the part of the window outside the item's run, while no other run's token was live, can only be an owner's (step 1) and is counted as the run's, the same owner-for-coder gap step 6 accepts; another item's live token is never counted as this item's.
- **D3** [gap] INTEGRATION-SPEC §9.8 step 1, 3: a required ruleset is a repository ruleset (`source_type` `Repository`) with `enforcement: active` whose target, `ref_name` include list, rules and bypass list have the shape step 1 gives: loop branches include `refs/heads/<prefix>**`, rules `creation`, `update`, `deletion`, `non_fast_forward`, bypass only the coder app (`Integration`, by app ID) and `RepositoryRole` 5 (admin); base branch include `refs/heads/<base>`, or `~DEFAULT_BRANCH` only when the base is the repository's `default_branch` (`GET /repos/{o}/{r}`, read only when a ruleset names `~DEFAULT_BRANCH`; review R2), rules `update`, `deletion`, `non_fast_forward`, bypass only the loop app and the admin role; tags (`target: tag`) include `refs/tags/**` or `~ALL`, rules `creation`, `update`, `deletion`, bypass only the admin role and `release_apps`. Extra rules are allowed, an `exclude` list is not. No matching ruleset is `ruleset_missing` for every commit. `OrganizationAdmin` and other bypass actors don't match. Why: "may contain only" lists the allowed bypass actors; matching by shape, not name, lets `init` and the owner name them.
- **D4** [ambiguity] INTEGRATION-SPEC §9.8 step 3, I9, fake D5: a ruleset's last change is its `updated_at`. `ruleset_changed` names the commits whose push window starts before any required ruleset's last change; `pushed_before_ruleset` those whose window starts before the loop-branch ruleset's `created_at`. A ruleset restored after removal is a new ruleset, so both apply to the commits before it. Why: GitHub keeps no ruleset history; `updated_at` moves on every edit, disable and enable, which is the strict reading of I9.
- **D5** [gap] INTEGRATION-SPEC §9.8 step 2, 3: `Validate` compares admins with the profile's `owners`, and `Trust` with `HostConfig.Owners` (the same IDs, given when the host is built). `admin_not_owner` names every commit while a user who isn't an owner account (by ID, `HostConfig.Owners`) is in `GET /collaborators?permission=admin&affiliation=all` (organisation owners included), and, after that admin is gone, the commits whose window starts before the last time the host saw one. Why: "like a ruleset change"; GitHub doesn't say when an admin was added.
- **D6** [gap] INTEGRATION-SPEC §9.8 step 3 "cached for one poll interval": the ruleset and admin state is read at most once per `HostConfig.PollInterval` (60 s when zero) and shared by every `Trust` call. A failed read is returned as the error, never cached. `GET /rulesets` refused with 403 or 404 counts as no rulesets (`ruleset_missing`), not an error, since a retry can't help.
- **D7** [gap] INTEGRATION-SPEC §9.8 step 3: causes are reported in fake D5's order (fork, branch outside prefix, ruleset missing, ruleset changed, pushed before ruleset, admin not owner, pusher unknown), each naming its commits, and `Commits` lists every named commit once in compare order (oldest first). A fork reports only `fork`. Commits are those `GET /compare/{base}...{head}` lists as ahead.
- **D8** [gap] INTEGRATION-SPEC §9.1, FR-13 AC4, NFR-4 AC1: each app's installed permissions come from `HostConfig.Permissions` (`secrets.GitHubApps.InstalledPermissions`, `GET /repos/{o}/{r}/installation` with the app JWT). The expected installation permissions are the union of each app's rows in §9.1: coder `contents: write, issues: read, pull_requests: write, metadata: read`; reviewer `contents: read, issues: read, pull_requests: write, metadata: read`; loop `issues: write, pull_requests: write, contents: write, checks: read, statuses: read, administration: read, metadata: read`. A permission not listed, or at a higher level than listed, is `app_permission_extra` (error); one missing or lower is `app_permission_missing` (error). `metadata: read` is never extra. With no `Permissions` source, or a failed read, `Validate` reports `app_permissions_unreadable` (error).
- **D9** [gap] INTEGRATION-SPEC §9.7, §9.8: the new `Validate` codes, all with a hint: `rulesets_unsupported` (error, `GET /rulesets` refused with 403 or 404), `ruleset_missing` (error, per required ruleset, naming it), `ruleset_bypass_missing` (error: the coder app isn't in the loop-branch bypass list, or the loop app isn't in the base-branch one, so the loop can't push or merge), `ruleset_other_branches_missing` (warning, D10), `admin_not_owner` (error, per admin), `conversation_resolution_required` (error, N4a), `up_to_date_not_required` (warning), `approvals_required` (warning), `code_owner_review_required` (warning). The base-branch rules come from `GET /rules/branches/{base}` and classic protection (`GET /branches/{base}/protection`, 404 meaning none). A read that fails is `rulesets_unreadable`, `admins_unreadable` or `branch_rules_unreadable` (error). When the base branch is missing or unreadable, `Validate` stops there, since every later check reads it.
- **D10** [gap] INTEGRATION-SPEC §9.8 step 1 "optional fourth ruleset": it counts as present when an active repository branch ruleset other than the two required ones includes `~ALL` or `refs/heads/**`, has an `update` rule, and doesn't list the coder app in its bypass list. Why: the spec doesn't give its shape.
- **D11** [gap] stub GitHub, live run to confirm: a ruleset refusal of a contents write, ref creation, branch merge or pull-request merge answers 403 "Repository rule violations found" (the adapter's `Merge` maps it to `Failed`); a fork's branches are served under `/repos/<login>/<name>` once `POST /forks` made the fork; `Integration` bypass actors are matched by app ID, not by the bot user's ID. The stub doesn't refuse a coder push outside a coder run (token revocation is the secrets store's, T12), unlike the fake.
- **D12** [open] plan T11, INTEGRATION-SPEC §11: the live conformance run stays on `RunWithoutTrust`. C10, C13 and C15 need a second admin account, a fork and app IDs in the sandbox, which the live setup doesn't have yet; they should join the live run with T20.

## Verification

- `go test -race ./internal/adapters/github/... ./conformance/... ./internal/secrets/...`, then the full `wf.py verify`.
- `GOOS=linux go build ./...` and `CGO_ENABLED=0 GOOS=darwin go build ./...` (CLAUDE.md).

## Risks and open questions

- The stub encodes my reading of GitHub's rulesets and collaborators APIs; D11 lists what the live run must confirm.
- Pusher attribution after a restart is less precise (D2): the window falls back to the cursor's window start, or the zero time before the feed is read, which can only widen it. A wider window errs towards `ruleset_changed` and `pushed_before_ruleset`, and towards `Unknown` whenever another item's run falls outside this item's runs in it.

## Outcome

- `Trust` replaces the T10 placeholder: fork, branch outside the prefix, the three rulesets matched by shape and cached for one poll interval, admins, and pusher attribution from the worker's run records (`RunLog`). `HeadMoved.Pusher` is no longer always `unknown`.
- `Validate` checks app permissions (`AppPermissions`, implemented by `secrets.GitHubApps.InstalledPermissions`), rulesets, admins and the base-branch rules of §9.7. The code-host `Validate` moved to `hostvalidate.go`.
- The stub GitHub models rulesets, collaborators, forks, tags and ruleset enforcement; the driver's trust actions are real REST calls, so the stub now runs the whole conformance suite (`conformance.Run`).
- Deviation: the live run stays on `RunWithoutTrust` (D12).
- No follow-up issues filed; D12 belongs to T20.
