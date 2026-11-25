# Work plan: Keychain items must keep trusting agent-loop across rebuilds and upgrades (resolves #17 D4)

**Issue:** [#95](https://github.com/mayusuk/agent-loop/issues/95) (feature)
**Branch:** `issue/95-keychain-items-must-keep-trusting-agent`
**Base:** `origin/main @ bc820ec` (checked before branching)
**Status:** Done

## Summary

macOS ties each agent-loop Keychain item to the code signature of the binary that wrote it. `go build` signs ad hoc, so every rebuild or upgrade is a new app and every read prompts. This change signs agent-loop with a stable self-signed code-signing certificate and a fixed identifier (`agent-loop sign`), detects items that trust a different build (`server start`, `worker`, `init --check`), and re-trusts them in `init` with one approval per item.

## Context

- Owner decision on #95 (2026-11-25): a stable self-signed code-signing certificate, not a Developer ID. Homebrew follows in #124 and calls the signing command; the formula is out of scope.
- TECHNICAL-SPEC §7.2 and T12: each Keychain item has an access list that names only the agent-loop binary, so another process gets a prompt, not the key. #17 D3 implemented it with `SecTrustedApplicationCreateFromPath`, which records the binary's designated requirement (DR). #17 D4 left the rebuild problem open.
- An ad-hoc DR is `cdhash H"..."` (these exact bytes). A certificate-signed DR is `identifier "dev.agent-loop" and certificate leaf = H"<sha1>"`, which every build signed with that certificate meets.
- Secrets are read by `worker` (GitHub App keys) and `server start` (gateway token, when enabled). `init` reads and writes the GitHub App items.

## Approach

**Experiment first (done, see Verification).** On this Mac, against a temporary keychain, an item written by build v1 signed with a self-signed certificate (`-i dev.agent-loop`) is read with user interaction disabled by a different build v2 signed with the same certificate, at the same path and at another path. Controls: v2 ad hoc and v2 signed with a different certificate (same identifier) are refused (`errSecAuthFailed`). An untrusted build can delete an item without approval, so re-trust is one approval (the read) plus a rewrite.

**`internal/secrets`**
- `codesign.go`: `SigningIdentity` (find the certificate by name in a keychain, or create it: an RSA-2048 self-signed certificate with the code-signing extended key usage, imported with `security import` and `-T /usr/bin/codesign`), `Sign` (sign a copy of the binary with `codesign --force --sign <sha1> --identifier dev.agent-loop`, verify it, then rename it over the original so a running binary is never rewritten in place) and `Signature` (read a binary's DR with `codesign -d -r-`, and report ad hoc). Commands go through a seam so tests fake them on any platform.
- `keychain.go`: `(*Keychain).Untrusted(ctx, names)` lists the items that exist but that the running binary can't read without a prompt (read with user interaction off; the value is dropped). `(*Keychain).Retrust(ctx, name)` reads the item (macOS asks once) and rewrites it for the running binary.

**`cmd/agent-loop`**
- `agent-loop sign [--binary <path>] [--identity <name>] [--keychain <path>]`: macOS only. Defaults: the running binary (symlinks resolved, so a Homebrew Cellar path), the certificate "agent-loop code signing", the login keychain. Prints the DR. This is what #124's formula calls after install and upgrade.
- `server start` and `worker`: with the keychain backend, stop before work starts when an item they read trusts a different build, naming the items and the fix, instead of stalling at a password prompt. Both warn when the running build is ad hoc signed.
- `init` step 2: with the keychain backend, warn about an ad-hoc build; list stale items. `--check` counts them as drift; `init` asks, then re-trusts each one.
- GETTING-STARTED: sign after every build or upgrade, what macOS asks and when, and how to recover.
- TECHNICAL-SPEC §7.2 and T12, plan T12: record the decision.

## Steps

- [x] Signing helpers in `internal/secrets` with faked-command tests.
- [x] `Keychain.Untrusted` and `Keychain.Retrust`, tests on a temporary keychain; the live rebuild test behind `AGENT_LOOP_LIVE=1`.
- [x] `agent-loop sign` command and tests.
- [x] Stale-trust checks in `server start`, `worker` and `init` (`--check` and re-trust), with tests.
- [x] GETTING-STARTED, TECHNICAL-SPEC §7.2 / T12, plan T12.

## Acceptance criteria

- Keychain items keep trusting every build signed with the same certificate across rebuilds and upgrades, with no prompt after the first approval (verified against a temporary keychain).
- A signing step with a fixed identifier that works for a source build and a Homebrew-installed binary.
- `server start` and `init --check` report "the Keychain items trust a different agent-loop build"; `agent-loop init` re-trusts them with one approval per item.
- GETTING-STARTED explains the Keychain trust and when macOS asks.
- No access list allows any application (§7.2).

## Decisions

- **D1** [gap] TECHNICAL-SPEC §7.2, T12: the code identifier is `dev.agent-loop` and the certificate's common name is `agent-loop code signing`, in the login keychain. Why: the issue proposed both; one fixed DR per user.
- **D2** [gap] TECHNICAL-SPEC §7.2: `agent-loop sign` creates the certificate when it is missing: RSA-2048, self-signed, code-signing extended key usage, valid 10 years. The certificate is not marked trusted (no admin prompt); codesign signs with it and the Keychain matches its leaf hash. Why: the owner chose a self-signed certificate; trust settings are not needed for the DR to match.
- **D3** [gap] TECHNICAL-SPEC §7.2: each user has their own certificate, so a build signed on one machine is a different app on another. Release binaries (#124) are re-signed locally by `agent-loop sign` after install and after every upgrade. Why: a shared self-signed private key can't be distributed safely.
- **D4** [gap] TECHNICAL-SPEC §7.2: `sign` signs a copy next to the binary and renames it over the original, so a running agent-loop is never rewritten in place. Why: rewriting a running signed binary can get it killed.
- **D5** [gap] TECHNICAL-SPEC §11, §7.2: `server start` and `worker` stop with an error, before any work starts, when an item trusts a different build; they don't wait at a prompt. `worker` checks the GitHub App items (app ID, private key, installation ID of each identity); `server start` checks those too, plus the gateway token when the gateway is on, so a stale item shows up when the server starts rather than as a stalled run. Both warn, without stopping, when the running build is ad hoc signed. `project validate` and `run --dry-run` are interactive and don't check. Why: an unattended loop must not stall on a click (#95).
- **D6** [gap] INTEGRATION-SPEC §12.2 step 2: `init --check` counts stale items as drift; `init` asks once ("Re-trust them?"), then reads each item (macOS asks once per item) and rewrites it for the running binary. `init --check` stops after step 2 when items are stale (exit 1), because the later steps read them and macOS would ask for each. A failed or denied re-trust is a warning and counts as a failed step. An ad-hoc build gets a warning that names `agent-loop sign`, not drift. Why: issue item 3; signing is the owner's step outside init.
- **D7** [gap] TECHNICAL-SPEC §7.2: an item is stale when it exists and the running binary can't read it with user interaction off. Missing items are not stale. Why: this tests what macOS will do, rather than comparing paths or hashes.
- **D8** [gap] TECHNICAL-SPEC §11: a new local command, `agent-loop sign [--binary <path>] [--identity <name>] [--keychain <path>]`, macOS only (exit 1 elsewhere). It signs the running binary by default, with symlinks resolved, so `agent-loop sign` after `brew install` or `brew upgrade` signs the Cellar binary (#124). Why: the owner asked for a signing command #124 can call.

## Verification

Temporary-keychain experiment (2026-11-25, macOS 26.6.2), scratch keychain created with `security create-keychain`, never the login keychain:

| Reader | Result with user interaction off |
|---|---|
| v1 (wrote the item), signed with certificate A, `-i dev.agent-loop` | read |
| v2 (different bytes), certificate A, same path | read |
| v2, certificate A, another path | read |
| v2, ad hoc | refused, errSecAuthFailed (-25293) |
| v2, certificate B, same identifier | refused, errSecAuthFailed |
| v2 ad hoc, delete | deleted, no approval |

`codesign` finds the identity only when its keychain is in the user's search list; the certificate needs no trust setting. The same experiment is `TestT12_KeychainTrustSurvivesResign` (live, `AGENT_LOOP_LIVE=1`).

## Risks and open questions

- The file-keychain ACL APIs are deprecated; Apple could change how legacy ACLs match a DR.
- The first `codesign` use of the new key may ask once to allow codesign to use it.

## Outcome

Done as planned: `internal/secrets` signs (`Signer`, `Signature`) and checks and re-trusts Keychain items (`Keychain.Untrusted`, `Keychain.Retrust`, the `TrustChecker` interface); `agent-loop sign` is new; `server start`, `worker` and `init` check trust. The temporary-keychain experiment is repeatable as `TestT12_KeychainTrustSurvivesResign` (`AGENT_LOOP_LIVE=1`), which passed on this Mac with a certificate made by `Signer.CreateIdentity` itself.

Deviations: `server start` checks the workers' GitHub App items as well as its own gateway token (D5), and `init --check` stops after step 2 when items are stale (D6), since reading them would prompt. No follow-up issues; the Homebrew formula is #124.
