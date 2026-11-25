# Work plan: GitHub App keys are read from the secret store on every token, so Keychain prompts on every call

**Issue:** [#94](https://github.com/mayusuk/agent-loop/issues/94) (bug)
**Branch:** `issue/94-github-app-keys-are-read-from-the`
**Base:** `origin/main @ f4771cf` (checked before branching)
**Status:** In progress

## Summary

`GitHubApps` now reads each identity's app ID, private key and stored installation ID from the secret backend once per process and keeps them in memory, so minting a token no longer touches the Keychain. A restart reads them again.

## Context

Root cause: `GitHubApps.appJWT` (`internal/secrets/githubapp.go`) calls `Backend.Get` for `app_id` and `private_key` on every call, and `installation` calls it for `installation_id` on every call. Every `Token`, `Installed`, `App` and `Account` call goes through `appJWT`, so each installation token costs three backend reads. With the Keychain backend and items that do not trust the binary, each read is a password prompt.

## Approach

- Add a per-identity cache of the parsed app ID and `*rsa.PrivateKey`, and of the stored installation ID (or its absence), on `GitHubApps`, guarded by the existing `mu`.
- The lock is held across the backend read, so concurrent first calls read once.
- Only successful reads (and "not found" for the optional installation ID) are cached; a failed or invalid read is retried on the next call.

## Steps

- [ ] Test: a counting backend sees one read per field per identity across many `Token` calls (fails before the fix).
- [ ] Cache the app ID, key and stored installation ID per identity under `mu`.

## Acceptance criteria

- Each identity's app ID and key are read and parsed once per process and kept in memory behind the existing lock.
- A backend that counts `Get` calls sees one read per field per identity across many `Token` calls.

## Decisions

- **D1** [gap] INTEGRATION-SPEC §9.1 / TECHNICAL-SPEC secrets: the optional `installation_id` field is also read once per process (its value, or that it is not stored), not only `app_id` and `private_key`. Why: it is read on every token too, so leaving it would keep one Keychain prompt per token.
- **D2** [gap] INTEGRATION-SPEC §9.1: a failed or invalid secret read is not cached; the next call reads again. Changing a stored secret takes effect only after a restart. Why: the owner can fix a missing or malformed secret without restarting, and the issue says a restart re-reads.

## Verification

`go test -race` covers concurrent `Token` calls in the new test.

## Risks and open questions

Holding `mu` across a Keychain read blocks other identities' cached lookups while a prompt is open. That is accepted: it happens once per identity per process.

## Outcome

_Filled in before shipping._
