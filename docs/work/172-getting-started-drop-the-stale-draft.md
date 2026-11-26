# Work plan: GETTING-STARTED: drop the stale draft banner and check the guide against the v0.1.0 tools (#144 D4)

**Issue:** [#172](https://github.com/mayusuk/agent-loop/issues/172) (feature)
**Branch:** `issue/172-getting-started-drop-the-stale-draft`
**Base:** `origin/main @ 958df78` (checked before branching)
**Status:** Done

## Summary

`docs/GETTING-STARTED.md` still opened with a "Status: draft ... the commands don't exist yet" banner, which is wrong now that v0.1.0 ships the guide in every release archive. This replaces the banner and corrects every command, flag and behaviour the guide shows that no longer matches the real `agent-loop` CLI on main and `agent-loop-claude` from `mayusuk/agent-loop-handlers`.

## Context

- Issue #172 (resolves #144 D4). The guide is the owner's view of INTEGRATION-SPEC §12 onboarding.
- Checked against `agent-loop --help` and each subcommand's `--help`, built from this branch, and `agent-loop-claude` built from `agent-loop-handlers` main, plus the code paths behind each claim (`cmd/agent-loop/init.go`, `dryrun.go`, `client.go`, `serverstart.go`, `keychaintrust.go`, `internal/adapters/github/setup.go`, `internal/profile`). No GitHub or Claude calls.
- Out of scope: the handlers install lines, which PR #147 changes. `brew install mayusuk/tap/agent-loop` stays first.

## Approach

Docs only. Edit `docs/GETTING-STARTED.md` in place: new banner, then fix each drift found:

- Prerequisites: `init` logs in with the `gh` CLI's token (or `--oauth-client-id`), so `gh` must be logged in; the repository needs a GitHub `origin` remote.
- Keys go to the Keychain on macOS and to `~/.config/agent-loop/secrets.env` on Linux (the default backend is per OS).
- `init` steps: nine steps (Detect, Identities, Profile, Labels, Push rules, Handlers, Gates, Validate, Next steps). The browser opens twice per app (create, then install). The handler choice and the gates question are asked while writing the profile. The labels include `plan` and `plan-proposal`. Push rules: admins (you) bypass; optional ruleset for other branches; "up to date before merging".
- `server start` and `worker` both run in the foreground: two terminals.
- Dry run without the server can't show claimed items or limits.
- Troubleshooting: `run --dry-run` needs the project name; `board` shows `required gate pending`, not which check, so point at the PR's checks.
- Make it yours: `mint.md` isn't a prompt the starter handlers read (mint isn't supported in this release).
- The wf skill that marks `status:ready` is `plan-work`, not `plan-issues`.

## Steps

- [x] Replace the banner and fix the drift in `docs/GETTING-STARTED.md`.

## Acceptance criteria

- The draft banner is removed or rewritten for v0.1.0.
- Every command, flag and output the guide shows matches the real CLI on main and `agent-loop-claude`.
- `brew install mayusuk/tap/agent-loop` stays first; the handlers install lines are unchanged.

## Decisions

None.

## Verification

Built both binaries into a scratch dir and ran every subcommand's `--help`, plus `server init --no-start`, `run --dry-run` without a project, `board` and `worker` with no server or project, under a scratch `HOME`.

## Risks and open questions

None.

## Outcome

Done as planned: banner replaced and the drift listed in Approach fixed. No code changed. Code-level observations (not fixed here) are in the PR report.
