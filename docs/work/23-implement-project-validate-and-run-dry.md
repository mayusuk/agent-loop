# Work plan: Implement `project validate` and `run --dry-run`

**Issue:** [#23](https://github.com/mayusuk/agent-loop/issues/23) (feature)
**Branch:** `issue/23-implement-project-validate-and-run-dry`
**Base:** `origin/main @ 0c424ae` (checked before branching)
**Status:** In progress

## Summary

Adds `agent-loop project validate <project>` and `agent-loop run --dry-run <project>`. Both run in the CLI process, without the server. `validate` checks the profile, the handlers, the sandbox rule, the identities and the owner accounts, and calls the adapters' `Validate` and `Capabilities`. It names every problem and exits non-zero on any error. The dry run makes one picking pass through write-recording adapters, prints each candidate's verdict and the writes it would make, and starts no handler.

## Context

- FR-1 AC2, AC6; FR-2 AC1, AC6; FR-13 AC1, AC4; NFR-4 AC1; INTEGRATION-SPEC §2, §9.1, §9.5, §9.7, §9.8, §12.3; TECHNICAL-SPEC §7.2 (sandbox rule), §11 (local commands), §13.
- `profile.LoadProfile` already does the schema, ranges, and the "three identities differ" check on references. `Policy.WithIdentityIDs` checks account IDs.
- The GitHub `Tracker.Validate` covers markers, labels and text dependencies. `Host.Validate` covers app permissions, rulesets, admins and base-branch rules (#16).
- `core.Pick` gives verdicts in pick order with the FR-2 AC1 exclusion. `core.ClaimFor` plans a claim.
- `runners.Wrapped` says whether the Claude sandbox wraps a command.
- No production wiring from profile and secrets to the GitHub adapters exists yet (T17 #22 also needs it).

## Approach

- `internal/adapters/recording`: `Tracker` and `Host` wrap a `ports.WorkTracker` and a `ports.CodeHost`. Reads pass through. Every write is appended to a shared `Log` and never sent.
- `internal/secrets`: `GitHubApps.Account(ctx, id)` reads the app ID from the backend, `GET /app` with the app JWT (slug), and `GET /users/<slug>[bot]` (bot account ID).
- `internal/adapters/github`: `Tracker.Validate` checks each owner account with `GET /user/{id}`. `githubtest` serves that route.
- `cmd/agent-loop/project.go`: loads the deployment config (`--config`) and the project's profile, and opens the adapters through a replaceable `openAdapters` (GitHub in production, with a read-only loop token).
- `cmd/agent-loop/validate.go`: the profile, handler, sandbox, identity, capability and gate checks, and the adapters' `Validate`; prints the report.
- `cmd/agent-loop/dryrun.go`: builds `core.Candidate`s, runs `core.Pick`, and sends each pick's claim and each FR-2 AC6 escalation through the recording adapters; prints the report. The server's state comes through a `serverBoard` hook, which reports "not running" until the API exists (#22).

## Steps

- [x] Recording adapters, with a test that no write reaches the fake.
- [x] `GitHubApps.Account`, and the owner-account check in the GitHub tracker's `Validate`.
- [ ] `project validate` with GitHub wiring, and TestFR1AC2_ tests against the fake.
- [ ] `run --dry-run`, and TestFR1AC6_ tests against the fake.
- [ ] Outcome, verify, ship.

## Acceptance criteria

- Against the fake adapter, a dry run makes no write and starts no handler, and a test checks both (TestFR1AC6_...).
- The dry run gives the FR-2 AC1 exclusion rule for each candidate it skips.
- `validate` exits non-zero and lists each seeded problem (TestFR1AC2_...).

## Decisions

- **D1** [gap] FR-1 AC2, AC6, TECHNICAL-SPEC §11: `<project>` is the name registered in the deployment config. `--config <path>` picks the deployment config (default `~/.config/agent-loop/deployment.toml`). Exit codes: 0 when there is no error (warnings allowed), 1 when there is an error or the project can't be loaded, 2 for a usage error. `run` without `--dry-run` is a usage error. Why: the specs name the commands but not their flags or exit codes.
- **D2** [gap] FR-1 AC2: `validate` prints one line per problem, `error <code>: <message>` or `warning <code>: <message>`, then `  fix: <hint>`, and ends with `<n> error(s), <m> warning(s)`. Profile load problems use the code `profile` and name the key. When the profile doesn't load, the other checks are skipped. Why: the spec says "naming each problem" without a format.
- **D3** [gap] FR-1 AC2 ("handlers can be reached"): a stage's `command[0]` must resolve. An absolute path must be an executable file. A path that contains `/` is resolved against the profile's directory, which is the repository root and the run's working directory. A bare name is looked up on `PATH`. Otherwise the error is `handler_not_found`. Why: the run starts the command in the worktree, so this is the check that matches what the worker does.
- **D4** [gap] INTEGRATION-SPEC §2: each stage's `contract` is checked with `contract.CheckVersion`. A failure is the error `handler_contract_unsupported`. Why: the spec says validate fails and doesn't name the check.
- **D5** [gap] TECHNICAL-SPEC §7.2: `runner = "local"` with a command the Claude sandbox doesn't wrap (`runners.Wrapped`) is the error `handler_not_sandboxed`. `local-unsandboxed` is the warning `handler_unsandboxed`, which quotes `runners.UnsandboxedWarning`. `docker` is the error `runner_unsupported`, because v0.1 has no docker runner. Why: the spec gives the first two; the worker refuses docker in v0.1.
- **D6** [gap] FR-13 AC1, INTEGRATION-SPEC §9.1: on GitHub an identity's account ID is its app's bot user: the app's slug from `GET /app` (app JWT), then `GET /users/<slug>[bot]`. The app ID is the secret field `app_id`. A failure is the error `identity_unreadable`. Two identities on one account, or an owner account that is an identity, is the error `identity_conflict` (`Policy.WithIdentityIDs`). Why: the profile holds only secret references, so the IDs have to come from GitHub.
- **D7** [gap] FR-13 AC1, INTEGRATION-SPEC §13 `identities.owners`: the GitHub tracker checks each owner with `GET /user/{id}`. A 404 is the error `owner_missing`. An account whose type isn't `User` is the error `owner_not_human`. A login that differs from the profile's is the warning `owner_login_changed`. Why: the spec asks for these checks but not how they are made.
- **D8** [gap] TECHNICAL-SPEC §11, INTEGRATION-SPEC §12.3: `validate` and the dry run act with a loop token narrowed to read access (`issues`, `pull_requests`, `contents`, `checks`, `statuses`, `administration`, all `read`). A bug can't write to GitHub through them. The coder and reviewer apps are used only through their app JWTs (account lookup and installed permissions). Why: §12.3 says the dry run needs only the loop's read access, and validate only reads.
- **D9** [gap] TECHNICAL-SPEC §5 (`Capabilities`): a tracker without native dependencies is the warning `dependencies_unsupported`. A marker mapped to a status on a tracker without status markers is the error `status_markers_unsupported`. Why: §5 says validate fails or warns, without saying which.
- **D10** [gap] FR-1 AC2 ("gates can be reached"): a profile with `gates = []` gets the warning `no_gates`, since changes then merge with no check. Why: an empty list replaces the base branch's required checks (§9.7).
- **D11** [open] FR-1 AC2: the gate names a profile lists aren't checked against the code host, because `CodeHost` has no read that lists the checks a repository reports. A misspelt gate keeps an item in `merging` until its time limit.
- **D12** [gap] INTEGRATION-SPEC §12.3, TECHNICAL-SPEC §11: when the server's state can't be read, the dry run says so. It then decides as if the project and deployment weren't paused, the daily limit wasn't reached and no item was claimed, and it says that assumption in its output. The server's board isn't wired yet, because the API comes with #22, so the server's state is never read until then. Why: WIP, the daily count and pauses live in the workflow state. The tracker's candidate list doesn't show claimed items.
- **D13** [gap] INTEGRATION-SPEC §12.3: the writes the dry run lists are the ones its picking pass makes: a `Claim` for each item it would pick (with the `core.ClaimFor` key), and for each FR-2 AC6 escalation, `SetMarker needs-owner` and a status comment with reason `dependency_not_done`. Each line gives the port, the method, the item and the idempotency key. Starting the item's workflow is listed as "would start", and is not a tracker write. Why: the spec lists "the writes the pass would have made" without saying which writes those are.
- **D14** [gap] INTEGRATION-SPEC §12.3: each candidate's line is `<item>  would pick` or `<item>  excluded: <code> (<text>)`, for example `excluded: dependency_open (dependency 17 not done)`. The code is `core.Exclusion`. Why: the spec gives examples, not a format.

## Verification

`wf.py verify`, plus `GOOS=linux go build ./...` and `CGO_ENABLED=0 GOOS=darwin go build ./...`.

## Risks and open questions

- PR #63 (#21) adds a `Candidates` builder in `internal/workflows`. The dry run has its own builder in `cmd/agent-loop`, which can switch to the shared one once #63 merges.

## Outcome

_Filled in before shipping._
