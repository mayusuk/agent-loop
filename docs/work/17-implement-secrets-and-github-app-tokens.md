# Work plan: Implement secrets and GitHub App tokens (`internal/secrets`)

**Issue:** [#17](https://github.com/mayusuk/agent-loop/issues/17) (feature)
**Branch:** `issue/17-implement-secrets-and-github-app-tokens`
**Base:** `origin/main @ 5cc778b` (checked before branching)
**Status:** Done

## Summary

Adds `internal/secrets`: two secret backends (macOS Keychain and env/file), a `SecretStore` that issues GitHub App installation tokens narrowed to INTEGRATION-SPEC §9.1 for each identity and use, a per-run token set that refreshes tokens near expiry and revokes them all when the run ends, and a redactor that masks every live token (and anything shaped like a GitHub token) in logs and stored output, also when a token is split across writes.

## Context

- TECHNICAL-SPEC T12 (`SecretStore`, Keychain and env/file in v0.1, short-lived tokens per run and identity), §5 (`Token`, `Revoke`), §7.2 (Keychain access list naming only the agent-loop binary), §7.3 (which identity each stage gets, narrowed handler tokens, the credential helper refreshing near the one-hour expiry, revoke at the end of every run, the redactor), §13 (deployment `secrets.backend`).
- INTEGRATION-SPEC §3.4 (push token and `GH_TOKEN` read token per stage, none for review), §9.1 (the permissions table, tokens narrowed with `permissions` and `repositories`), §9.8 step 6 (tokens issued per run and revoked when it ends, I11).
- PRODUCT-SPEC FR-13 AC2 (reviewer never pushes), AC3 (short-lived, never in logs, tracker or handler output), AC4 (no more permissions than needed), §8 item 7.
- `internal/ports` (T3): `SecretStore`, `Scope{Repos, Permissions}`, `Token` (secret behind `Secret()`, prints redacted, refuses JSON), `Identity`, error classes (`ErrNotFound`, `ErrPermission`, `ErrTransient`, `RateLimited`).
- `internal/profile` (T4): `Identities` holds `secret://` references (D2 there: account IDs come later), `Deployment.Secrets{Backend, Path}` with `keychain` / `envfile` and the default `~/.config/agent-loop/secrets.env` (D14 there).
- Tests never call GitHub: the token endpoint is an `httptest` server. CI runs on Linux and macOS.

## Approach

Files in `internal/secrets/`:

- `backend.go`: `Backend` (`Get(ctx, name)`), `ParseRef` for `secret://<name>`, `Open(profile.Secrets)`.
- `envfile.go`: `EnvFile{Path}`; environment first, then the file.
- `keychain.go`, `keychain_darwin.go` (cgo, Security framework), `keychain_other.go`: `Keychain{Path, TrustedApp}` with `Get`, `Set` and `Delete`. `Set` creates a generic-password item whose access list names only the agent-loop binary. Off macOS every call fails.
- `uses.go`: the §9.1 table as `Use` values (`coder-push`, `coder-read`, `coder-worker`, `reviewer-read`, `reviewer-worker`, `loop`), `ScopeFor(use, repo)`, and `StageUses(stage)` for §3.4.
- `githubapp.go`: `GitHubApps`, the `ports.SecretStore`. It reads each identity's app ID and private key from a `Backend`, signs an RS256 app JWT, finds the installation if it isn't stored, asks for a narrowed installation token, checks the grant, and registers the token with the redactor. `Revoke` deletes the token.
- `run.go`: `Run` (the tokens of one run) and `Lease` (one token that refreshes itself near expiry). `Run.Close` revokes everything the run was issued.
- `redact.go`: `Redactor` with `Add`, `Redact` and a streaming `Writer`.

Tests: env/file everywhere; Keychain in `keychain_darwin_test.go` against a temporary keychain with user interaction off, checking the item's access list; token requests for every use checked against the §9.1 table, which the test parses from `specs/INTEGRATION-SPEC.md` so the code and spec can't drift; refresh and revoke through `Run`; the redactor over a mixed log stream with tokens split at every byte boundary.

## Steps

- [x] Backend interface, secret references and the env/file backend, with tests.
- [x] Keychain backend (macOS, cgo) with the binary-only access list, tested on macOS.
- [x] Redactor and streaming writer, with TestFR13AC3_ tests.
- [x] Token uses from §9.1 and §3.4, with a test against the spec table.
- [x] GitHub App token issuer and revoke against a fake token endpoint.
- [x] Per-run token set with refresh and revoke-at-end.

## Acceptance criteria

- Tests check the token request for each identity and stage against INTEGRATION-SPEC §9.1, using a fake GitHub token endpoint.
- The redactor masks tokens in a mixed stream of logs, including tokens split across writes (TestFR13AC3_...).
- The Keychain backend is tested on macOS only, behind a build tag. The env/file backend is tested everywhere.

## Decisions

- **D1** [gap] TECHNICAL-SPEC T12, INTEGRATION-SPEC §9.1, §13 `identities`: a secret reference is `secret://<name>`, where `<name>` is `/`-separated segments of `A-Z a-z 0-9 . _ -`. A GitHub App identity's secrets are the fields `<name>/app_id`, `<name>/private_key` (an RSA key in PEM, PKCS #1 or PKCS #8) and, optionally, `<name>/installation_id`. Without it, the installation is looked up with `GET /repos/{owner}/{repo}/installation` and cached for the life of the process. Why: the spec names references but not what they hold; an installation ID is something the owner shouldn't have to copy by hand.
- **D2** [gap] TECHNICAL-SPEC T12, §13 `secrets`: the env/file backend reads the variable `AGENT_LOOP_SECRET_<NAME>` (the name in upper case, every character other than a letter or digit as `_`, so `github-app/coder/app_id` is `AGENT_LOOP_SECRET_GITHUB_APP_CODER_APP_ID`), and otherwise the same key in `secrets.path`. The environment wins. The file has `KEY=value` lines, `#` comments, an optional `export `, and double-quoted values with the escapes `\n`, `\"` and `\\` (for PEM keys). A file readable or writable by group or others is refused (`ErrPermission`), a key set twice is an error, and a missing file means the environment alone. Errors name the file, line and key, never a value. An unquoted value is everything after `=` up to the end of the line, trimmed, so `#` there is part of the value, not a comment, and single quotes are not special: a comment must be a line of its own (review R4). A line may be up to 1 MiB; a longer one is an error. Why: the spec names the backend only; taking unquoted values literally keeps a secret containing `#` intact.
- **D3** [gap] TECHNICAL-SPEC §7.2 (Keychain access list), T12: Keychain secrets are generic-password items with the service `agent-loop` and the secret name as the account. `Set` deletes any existing item and creates a new one whose access list trusts only the agent-loop binary (the running executable, with symlinks resolved, unless `TrustedApp` names another path), so the access list is never inherited from an older item. Other processes, including `/usr/bin/security`, get a prompt. Each item's label is `agent-loop <name>`. The backend needs a macOS build made with cgo (it calls the Security framework); off macOS, or in a build with `CGO_ENABLED=0`, every call fails with an error that says so and points at `envfile`. Release builds for macOS must therefore be built with cgo on macOS, not cross-compiled (review R3, R4). Why: the spec says "an access list that names only the agent-loop binary" without the item layout.
- **D4** [open] TECHNICAL-SPEC §7.2: macOS ties a trusted application to the binary's code identity, so a rebuilt or upgraded agent-loop binary is no longer on the access list and the Keychain prompts until the owner allows it or the items are written again. Who writes the items (`agent-loop init`, T19, or a `secrets set` command) and how upgrades re-trust the binary is not specified.
- **D5** [ambiguity] INTEGRATION-SPEC §3.4 and §9.1, TECHNICAL-SPEC §7.3: a coder stage gets two tokens from the coder app. The push token (credential helper) has exactly the §9.1 "Coder, handler" permissions. The read token (`GH_TOKEN`) has the same permissions with `contents: read`. Reading chosen: §9.1 lists one coder handler token, but §7.3 keeps the push token out of the environment, so `GH_TOKEN` can't be the push token. The review stage gets only a read token with the "Reviewer, handler" permissions and no push token (FR-13 AC2). Why: both §3.4 columns need a token, and the environment one must not push.
- **D6** [gap] INTEGRATION-SPEC §9.1: each token request sends exactly the table's permissions (names in GitHub's spelling: `pull requests` is `pull_requests`) and `repositories` with only the project repository's name. A scope with no repository or no permission is refused, so an un-narrowed token is never requested. A grant with any permission beyond the request, except `metadata: read`, which GitHub adds to every token, is revoked at once and refused with `ErrPermission`. Why: "narrowed exactly as §9.1 says" needs a check that the grant matches.
- **D7** [gap] TECHNICAL-SPEC §7.3 (refresh near the one-hour expiry): a lease gets a new token when its current one expires within 10 minutes. The superseded token isn't revoked straight away, because a git operation may still be using it; it is revoked with the rest when the run ends. If getting the new token fails while the current one hasn't expired, the lease returns the current one and tries again on the next call, so a short GitHub outage doesn't fail a push the current token can still make (review R8). Why: the spec says "near" without a margin.
- **D8** [gap] TECHNICAL-SPEC §7.3, INTEGRATION-SPEC §9.8 step 6: `Revoke` calls `DELETE /installation/token` with the token. A 401 (already revoked) and a token past its expiry count as revoked. `Run.Close` revokes every token the run was issued, including superseded ones, reports every failure, and refuses new tokens afterwards. The caller passes a context that outlives the run's own, so a cancelled run still revokes. Why: revoking must be safe to repeat.
- **D9** [gap] FR-13 AC3, TECHNICAL-SPEC §7.3: the redactor replaces each credential with `[redacted]`. It masks every token it was given until that token's expiry, and always masks anything shaped like a GitHub token (`ghp_`, `gho_`, `ghu_`, `ghs_`, `ghr_` plus 36 letters and digits, and `github_pat_` plus 82), so a token from another source, or one already expired, is masked too. Each app JWT the store signs is also masked, for its 10-minute life. Its writer holds back the last *n*−1 bytes of the stream (*n* the longest possible match) until more arrives or it is flushed. Why: the spec names the redactor, not its rules.
- **D10** [gap] INTEGRATION-SPEC §9.1: the app JWT is RS256 with `iss` the app ID, `iat` 60 s in the past (clock drift) and `exp` 9 minutes ahead (GitHub allows at most 10). Why: GitHub's documented limits; the spec doesn't restate them.
- **D11** [gap] INTEGRATION-SPEC §8 error classes: GitHub responses map to 404 `ErrNotFound`, 401 / 403 / 422 `ErrPermission` (422 is GitHub's answer when the app lacks a requested permission or repository), 429 and a 403 with `X-RateLimit-Remaining: 0` or `Retry-After` `RateLimited` (retry at `Retry-After` or `X-RateLimit-Reset`), and 5xx and network errors `ErrTransient`. A rate-limited answer with neither a usable `Retry-After` nor `X-RateLimit-Reset` is retried after 1 minute. Error text is passed through the redactor. Calls use a client with a 30 s timeout unless the caller gives one, and read at most 1 MiB of each answer (review R4). Why: the spec names the classes, not these limits; GitHub's token answers are a few hundred bytes.
- **D12** [open] TECHNICAL-SPEC §7.3, T13: with the env/file backend the secrets are `AGENT_LOOP_SECRET_*` environment variables of the worker, so the runner must not pass them on to a handler. T13 (#18) builds the handler environment.
- **D13** [gap] TECHNICAL-SPEC §7.3: `Run.Close` keeps tokens whose revocation failed, so calling it again retries them; a token issued while the run is closing is revoked at once, with a context the run's cancellation doesn't end, and kept for the next `Close` if that revoke fails (review R1). When GitHub grants more than asked and revoking that token fails, `GitHubApps.Token` returns the token together with the error, and the run keeps it so `Close` revokes it (review R2). Why: a revoke that fails on a network error must not leave a live token behind.

## Verification

Standard checks. On macOS the Keychain tests create and delete a temporary keychain; they never touch the login keychain.

## Risks and open questions

- The Keychain code uses the macOS file-keychain APIs (`SecKeychain*`, `SecAccess*`), which Apple marks deprecated but which are the only way to set a per-application access list. Deprecation warnings are silenced in the cgo flags.

## Outcome

Added `internal/secrets` as planned: `Backend` with `EnvFile` and `Keychain` (cgo on macOS, an error elsewhere), `Open` from the deployment config, the §9.1 token uses and the §3.4 stage mapping, the `GitHubApps` `SecretStore` (app JWT, installation lookup, narrowed token requests, grant check, revoke), `Run` and `Lease` (refresh near expiry, revoke everything at the end), and the `Redactor` with its streaming `Writer`. 92% statement coverage.

- The §9.1 test parses the table from `specs/INTEGRATION-SPEC.md`, so a spec change without the code fails CI.
- The Keychain tests run on macOS (CI's `macos-latest` too) against a temporary keychain with prompts turned off, and check the item's access list through the Security framework.
- No deviations from the approach. No follow-up issues filed: D4 (re-trusting the binary after an upgrade) and D12 (keeping `AGENT_LOOP_SECRET_*` out of handler environments) are for T19 and T13.

Review round 1 (PR #38): all eight findings led to changes, with no pushbacks. R1, R2: tokens that a failed revoke leaves live are kept so `Run.Close` revokes them (D13). R3: the Keychain cgo file is tagged `darwin && cgo`, and the fallback names the cgo requirement (D3). R4: the remaining defaults are recorded in D2, D3 and D11. R5, R6, R7: new tests for masked error text and JWTs, a close racing an issue, and a 403 with only `Retry-After`. R8: a failed refresh keeps the current token while it is valid (D7).
