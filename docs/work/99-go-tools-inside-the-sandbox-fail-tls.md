# Work plan: Go tools inside the sandbox fail TLS verification, so gh can't read the item

**Issue:** [#99](https://github.com/mayusuk/agent-loop/issues/99) (bug)
**Branch:** `issue/99-go-tools-inside-the-sandbox-fail-tls`
**Base:** `origin/main @ f4771cf` (checked before branching)
**Status:** Done

## Summary

Go programs that do HTTPS (`gh`, `go mod download`) fail certificate verification inside the Claude Code sandbox the local runner generates, so the implement agent can't read its issue. The generated settings now let sandboxed commands look up the macOS trust service, `com.apple.trustd.agent`, through `sandbox.network.allowMachLookup`. Nothing else is widened.

## Context

TECHNICAL-SPEC §7.2 sets what the `local` runner's sandbox allows; `internal/runners/sandbox.go` writes it.

**Root cause (reproduced).** With the settings the runner writes (same shape, scratch paths), inside `claude -p --settings ...` a tiny Go HTTPS client and `gh api /zen` both fail: `tls: failed to verify certificate: x509: OSStatus -26276`. `curl` to the same host succeeds, so egress is fine. Go's `crypto/x509` on macOS verifies chains with `SecTrustEvaluateWithError`, which asks the trust daemon over the mach service `com.apple.trustd.agent`; Seatbelt denies that lookup. Adding `"allowMachLookup": ["com.apple.trustd.agent"]` under `sandbox.network` and nothing else makes both succeed (`gh` reaches GitHub and gets `HTTP 401` for a dummy token; the Go client gets `200 OK`).

Claude Code's documented options (settings reference, sandboxing page): `excludedCommands` (runs `gh` unsandboxed, with full access: ruled out by §7.2 and the required sandbox), `enableWeakerNetworkIsolation` (documented for MITM proxies with a custom CA; broader than needed), and `network.allowMachLookup` (names the exact XPC services allowed). The last is the narrowest.

## Approach

- `sandbox.go`: add `AllowMachLookup []string` to `networkSettings` and set it to a new exported `MachServices = []string{"com.apple.trustd.agent"}`. Same settings on every platform (the key only applies to Seatbelt).
- Golden file and §7.2 test: assert the allowed mach services are exactly the trust service, so a future widening fails a test.
- Live test (`AGENT_LOOP_LIVE=1`, needs `claude` and `gh` on PATH, macOS): write the generated settings for a scratch run, have `claude -p` run a script that calls `gh api /zen` with a dummy token and writes the output to the run's tree, and assert the output has no certificate error and shows GitHub answered.

## Steps

- [ ] _Each step small enough to commit on its own._
- [ ] _For a bug, the first step is a test that fails because of it._

## Acceptance criteria

- `gh api` and other Go HTTPS clients verify TLS inside the generated sandbox on macOS.
- Reads denied by TECHNICAL-SPEC §7.2 are unchanged; no new writes, sockets or domains.
- Unit test for the generated settings; live test behind `AGENT_LOOP_LIVE=1` that runs `gh` in the sandbox.

## Decisions

- **D1** [gap] TECHNICAL-SPEC §7.2 Network: the generated sandbox settings allow the macOS mach service `com.apple.trustd.agent` (`sandbox.network.allowMachLookup`) and no other, so Go tools can verify TLS certificates. Why: §7.2 lists only domains and sockets; without the trust service every Go HTTPS client fails, and `excludedCommands` or `enableWeakerNetworkIsolation` would widen more than needed.
- **D2** [gap] TECHNICAL-SPEC §7.2: the key is written on every platform, not only macOS. Why: the settings file stays one shape, and the key is a Seatbelt setting that the Linux sandbox does not use.

## Verification

Reproduced and fixed by hand with `claude -p --settings` (Claude Code 2.1.316, macOS). The live test `TestLiveGhVerifiesTLSInTheSandbox` repeats it with `AGENT_LOOP_LIVE=1`.

## Risks and open questions

The trust daemon is a system service outside the sandbox; Claude Code's docs say it doesn't confine what a listed service does. trustd evaluates certificate chains (reading the system and user trust settings itself) and may fetch revocation data; it gives the sandboxed process a trust verdict, not file contents, so the denied reads are unchanged.

## Outcome

The generated settings now carry `sandbox.network.allowMachLookup = ["com.apple.trustd.agent"]` (`runners.MachServices`); the golden file and the §7.2 test pin it as the only mach service. `TestLiveGhVerifiesTLSInTheSandbox` runs `gh api /zen` through `claude -p` with the generated settings: it failed with `x509: OSStatus -26276` before the fix and passes after. No deviation from the approach. No follow-up issues. PR #98 also edits `internal/runners/local.go` (`GH_CONFIG_DIR`); this branch does not touch that file.
