# Work plan: Implement `agent-loop init` for GitHub

**Issue:** [#24](https://github.com/mayusuk/agent-loop/issues/24) (feature)
**Branch:** `issue/24-implement-agent-loop-init-for-github`
**Base:** `origin/main @ 5d30a15` (checked before branching)
**Status:** In progress

## Summary

Adds `agent-loop init` (FR-1 AC5, INTEGRATION-SPEC §12.2 steps 1–9). It runs with the owner's own GitHub login, held in memory only, and asks before every change. It creates the three GitHub Apps through the manifest flow and stores their keys in the `SecretStore`, writes `.agent-loop.toml`, registers the project in the deployment config, creates the labels, the three rulesets (plus the optional fourth) and "require branches to be up to date", and ends with `project validate`. `--yes` accepts every step, `--check` only reports drift, and a second run changes nothing.

## Context

- PRODUCT-SPEC FR-1 AC5, FR-13 AC1. INTEGRATION-SPEC §9.1 (app permissions), §9.7 (gates, "up to date"), §9.8 (rulesets, admins), §12.2 (the steps). TECHNICAL-SPEC §11 (`init` runs locally), §13 (deployment `[projects.<name>]`).
- Builds on `internal/adapters/github` (Client, Host's ruleset fit `misfit`, `admins`, `validateBaseRules`, `wantPermissions`), `internal/secrets` (`GitHubApps`, `Keychain.Set`, `EnvFile`), `internal/profile`, and `cmd/agent-loop` (`validateProject`, `loadProject`).
- Tests run against `githubtest`, the stateful GitHub stub. It needs: `GET /user`, `permissions` and `owner` on `GET /repos/{o}/{r}`, `POST /labels`, the manifest conversion `POST /app-manifests/{code}/conversions`, apps that exist before they are installed, rule parameters on stored rulesets, and `PATCH .../protection/required_status_checks`.

## Approach

- **`internal/adapters/github/setup.go`** (GitHub's init steps, the `Initializer` of §12.2): each step reads the repository with the owner's token and returns `SetupChange`s (a summary and an apply function), so `init` can show, ask, apply, or only report for `--check`. Functions: `Detect` (login, ID, admin right, default branch, owner type, rulesets supported), `PlanLabels`, `PlanRulesets` (create or repair the three rulesets and the optional fourth, using the Host's `misfit` so `init` and `validate` agree), `PlanUpToDate`, `BaseRuleNotes` (base-branch rules the loop can't meet), `NonOwnerAdmins`, `RequiredChecks`, `AppManifest` and `ConvertManifest`.
- **`internal/secrets`**: `WritableBackend` interface (`Set`) and `EnvFile.Set`, which rewrites the file with mode 0600 and quotes values.
- **`cmd/agent-loop/init.go`**: the CLI. Flags `--yes`, `--check`, `--config`, `--repo-dir`, `--name`, `--oauth-client-id`. The owner token comes from `gh auth token`, or from the device flow when `--oauth-client-id` is given, and lives only in memory. The browser parts (manifest form, install page) sit behind an `appBrowser` interface that tests replace with the stub. Writes the profile text, appends the deployment registration, runs `validateProject`, prints next steps.

## Steps

- [x] Extend `githubtest` with the endpoints above.
- [x] Add `secrets.WritableBackend` and `EnvFile.Set`.
- [x] Add the GitHub setup steps in `internal/adapters/github/setup.go`, with tests.
- [ ] Add `agent-loop init` in `cmd/agent-loop/init.go`: owner login, the manifest browser flow, profile, deployment registration, prompts, `--yes`, `--check`, validate, next steps.
- [ ] Tests: first run makes every §12.2 change and a second makes none; `--check` makes no change and reports drift; the owner token is never written to disk; declining a step skips it.

## Acceptance criteria

- Against a fake GitHub API, a first run makes every change in INTEGRATION-SPEC §12.2, and a second run makes none (TestFR1AC5_...).
- `--check` makes no changes and reports drift.
- A test proves that the owner's login token is never written to disk.

## Decisions

- **D1** [gap] INTEGRATION-SPEC §12.2: The owner's login is the `gh` CLI's token (`gh auth token`) by default. The device flow runs only when `--oauth-client-id <id>` names an OAuth App, because the device flow needs a client ID and the specs name no agent-loop OAuth App. Why: §12.2 allows either, and only the `gh` path works without a registered app.
- **D2** [gap] §12.2 step 3: The project name is the repository's name, or `--name`. If the deployment config already registers that name for a different profile path, `init` stops and asks for `--name`, rather than rewriting the entry. Why: two repositories can share a name, and rewriting would point another project's entry at this repository.
- **D3** [gap] §12.2 step 2: The identity references are `secret://github-app/<project>/coder`, `/reviewer` and `/loop`, so two projects in one secret store don't collide. An existing app is "named" by storing its `app_id` and `private_key` under that reference before running `init`, which then reuses it.
- **D4** [ambiguity] §12.2 steps 3, 5, 6, 7: Every question whose answer goes into the profile (each stage's handler, gate names when the base branch has no required checks, and adding non-owner admins to `owners`) is asked in step 3, since step 3 writes the profile. Steps 5–7 then only check and report. A profile that already exists is never rewritten: it is loaded and its values drive the other steps.
- **D5** [gap] §12.2 step 3: The written profile spells out the defaults: adapters, repos, `base_branch` (the default branch), `branch_prefix`, `wip`, `max_rounds`, `merge_method`, `limits.daily_agent_runs`, the full `[markers]` mapping, identities with the owner `{ login, id }`, and the implement, review and address stages. `--yes` picks the starter handler for every stage.
- **D6** [ambiguity] INTEGRATION-SPEC §9.7: "require branches to be up to date" is turned on where the base branch's required checks are defined (the ruleset whose `required_status_checks` rule names them, or classic protection), not in the loop's base-branch ruleset. Why: the loop app is in that ruleset's bypass list, so a rule there would not apply to its merges, and §9.7 needs it to.
- **D7** [gap] §9.8 step 1: The rulesets are named `agent-loop: loop branches`, `agent-loop: base branch`, `agent-loop: tags` and `agent-loop: other branches`. A ruleset that aims at a kind but doesn't fit (`project validate`'s rule) is repaired in place: the loop's rules are added, other rules are kept, excludes are removed, it is made active, and the bypass list is set to exactly the allowed actors.
- **D8** [gap] §9.8 step 1: The optional fourth ruleset targets `~ALL`, excludes the loop branches and the base branch, restricts creations, updates, deletions and force pushes, and lets only the Repository admin role bypass it. `--yes` creates it too, since `--yes` accepts every step.
- **D9** [gap] §12.2 step 4: New labels get the colour `ededed` and no description. Existing labels, matched case-insensitively, are left unchanged.
- **D10** [gap] §12.2 step 2: An app that exists but isn't installed on the repository opens the install page and waits for the installation (polling `GET /repos/{o}/{r}/installation` with the app's JWT, up to 10 minutes). Permission drift on an existing app, and an installation on all repositories, are reported with a fix, not repaired: GitHub has no API to change them with the owner's token.
- **D11** [gap] FR-1 AC5: `init --check` exits 1 when anything would change and 0 otherwise. It writes nothing, opens no browser, and runs `project validate` only when the project is registered.
- **D12** [gap] §12.2 step 6: For the starter handlers `init` checks that `claude` is on `PATH` and that `claude auth status` succeeds. Either failing is a warning, not a stop.
- **D13** [gap] TECHNICAL-SPEC §13: When the deployment config doesn't exist, `init` creates it with only the `[projects.<name>]` entry, so every other key takes its default.
- **D14** [open] INTEGRATION-SPEC §12.2: The `Initializer` interface for other adapters is not defined yet. v0.1 has only the GitHub steps, called directly by the CLI.

## Verification

- `$WF verify --plan` (gofmt, vet, build, race tests). `GOOS=linux go build ./...` and `CGO_ENABLED=0 GOOS=darwin go build ./...`.

## Risks and open questions

- The manifest and install steps can't run in CI; the browser is behind an interface the tests replace with the stub. The live flow is checked by hand.

## Outcome

_Filled in before shipping._
