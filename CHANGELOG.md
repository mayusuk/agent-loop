# Changelog

All notable changes to `agent-loop`. Versions follow semantic versioning; each release's git tag
is the version with a leading `v`. A release's GitHub notes are its section of this file.

## 0.1.0

The first release: one project on GitHub, run locally on the owner's machine (PRODUCT-SPEC
§12 v0.1, TECHNICAL-SPEC §15). It ships with the starter handlers, `agent-loop-claude` 0.1.0
(`mayusuk/agent-loop-handlers`).

Contract versions: 1.0

Platforms: macOS (arm64, amd64) and Linux (amd64, arm64).

### Install

```bash
brew install mayusuk/tap/agent-loop
agent-loop sign       # macOS: after installing and after every brew upgrade
agent-loop version    # prints: agent-loop 0.1.0
```

Or build from source with Go 1.25 or later: `go install ./cmd/agent-loop` in a checkout, then
`agent-loop sign` on macOS. Install `agent-loop-claude` from source too
(`go install ./cmd/agent-loop-claude` in its checkout) until its Homebrew formula,
`brew install mayusuk/tap/agent-loop-claude`, is released
([mayusuk/agent-loop-handlers#58](https://github.com/mayusuk/agent-loop-handlers/issues/58)).
`docs/GETTING-STARTED.md` takes a repository to its first merged pull request.

### Added

- The loop: picks GitHub issues marked ready, has a handler implement each one and another review
  it, answers the review, and merges once the review is clean and the required checks pass. It
  escalates to the owner when it can't go on, and stays within its limits (review rounds,
  attempts, time limits and the daily agent-run limit).
- Durable workflows on Temporal (`ItemWorkflow`, `ProjectWorkflow` for one project,
  `DeploymentWorkflow` for the deployment pause), driven by a pure, deterministic core that
  records every decision.
- The GitHub adapter, with three GitHub App identities (coder, reviewer, loop) and short-lived,
  narrowly scoped tokens per run. Every write is idempotent, so a retry after a crash never writes
  twice.
- The handler contract 1.0: `input.json` in, `result.json` out, with JSON Schemas in
  `contract/v1/`. Handlers never write to GitHub; the worker posts their results.
- The local runner: each handler runs in a fresh tree under the Claude Code sandbox, with no
  secrets in its environment, and is stopped with everything it started at its time limit.
- Secrets in the macOS Keychain (each item trusts only agent-loop) or an env file, with log
  redaction of live tokens. `agent-loop sign` signs agent-loop with a self-signed certificate so
  the Keychain keeps trusting it across rebuilds and upgrades.
- The project profile (`.agent-loop.toml`) and deployment config, validated against JSON Schemas.
- The SQLite read model and decision log, the local API, and desktop and tracker notifications.
- The CLI: `init [--yes|--check]`, `project validate|pause|resume`, `run --dry-run`,
  `server init|start`, `worker`, `board`, `item pause|resume|abort`, `pause`, `resume`, `log`,
  `sign` and `version`.
- The adapter conformance suite (C1–C18) and an in-memory fake adapter.
- Releases with GoReleaser and the Homebrew formula in `mayusuk/homebrew-tap`.

### Not in this release

- v0.2: minting new work (`agent-loop mint`), materialising plans (`agent-loop plan abandon`),
  several hosts with capabilities, agent-run slots and the Docker runner.
- v0.3: several projects on one deployment, host bootstrap (`agent-loop host check`), remote
  notifications and a remote server.

The commands for these are listed in `agent-loop help` and print "not implemented yet".

### Known issues

- [#138](https://github.com/mayusuk/agent-loop/issues/138): the macOS binaries are not signed
  with an Apple Developer ID or notarized. Each Mac signs its own copy with `agent-loop sign`,
  which you must run again after every `brew upgrade`. Until you do, `server start` and `worker`
  stop on the upgraded build once your keys are in the Keychain, and tell you to run it.
