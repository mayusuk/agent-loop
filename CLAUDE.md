# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

agent-loop is a local service that takes GitHub issues labelled ready through implement → review → address → merge, using AI coding agents (handlers) that it runs in a sandbox. Go 1.25, one module: `github.com/mayusuk/agent-loop`. The starter handlers (`agent-loop-claude`) live in `handlers/`, in the same module but not part of the core (PRODUCT-SPEC D34, D38). They moved here from `mayusuk/agent-loop-handlers`, now archived.

**The specs in `specs/` are the source of truth.** If they conflict, PRODUCT-SPEC wins, then INTEGRATION-SPEC, then TECHNICAL-SPEC. Never change product behaviour without a spec decision. Read `docs/GETTING-STARTED.md` for the owner's view.

## Test locally before every push (required)

Never push a branch, open a PR or push review fixes without first running the full check suite locally and seeing it pass. CI is a second check, not the first. Run the same steps CI runs, in the same order (`.wf.toml` `[[verify]]` mirrors `.github/workflows/ci.yml`):

```sh
test -z "$(gofmt -l .)" || gofmt -l .   # must print nothing
go vet ./...
go build ./...
sh scripts/validate-plans.sh            # every plans/*/ passes plan validate
go test -race -count=1 ./...
```

Or run them all through the workflow script:

```sh
WF=$HOME/.claude/skills/work-issue/scripts/wf.py
$WF verify                 # all checks from .wf.toml
$WF verify --plan <plan>   # also validates the work plan's Decisions section
```

- `$WF ship` runs the checks again before it pushes. That doesn't cover pushes made outside `ship`, such as review fixes from `address-pr` or a merge of `origin/main`. Run `$WF verify` (or the five commands) before each of those pushes too.
- After merging or rebasing onto `origin/main`, run the checks again before pushing.
- Never skip tests, loosen a check, or edit `.wf.toml` or the CI workflow to get a pass. A step that reports `NOT AVAILABLE` is not a pass.
- CI runs on Ubuntu and macOS. Code with platform build tags (such as `internal/secrets/keychain_*`) must also build for the other platform and without cgo: `GOOS=linux go build ./...`, `CGO_ENABLED=0 GOOS=darwin go build ./...`.
- In the PR or reply, say which checks you ran locally and what they printed.

Running one package or test:

```sh
go test -race -count=1 ./internal/core/
go test -race -count=1 -run 'TestFR7AC5_' ./internal/core/
go test -race -count=1 -run 'TestC15_' ./internal/adapters/fake/   # one conformance case against the fake
go test -race -count=1 ./handlers/...                     # the starter handlers, their e2e tests and release-script tests
```

## Testing rules

- Name tests after the requirement they prove: `TestFR7AC5_...`, `TestNFR2AC1_...`, `TestC7_...` (a conformance case).
- Tests must not call GitHub, Claude or a real Temporal server. Use `internal/adapters/fake`, an in-memory GitHub stand-in for the GitHub adapter, fake handler executables, and Temporal's testsuite. Tests that need real services are skipped unless `AGENT_LOOP_LIVE=1`.
- `conformance/` is the adapter conformance suite (C1–C18, INTEGRATION-SPEC §11). Every adapter must pass it. When a bug escapes the suite, add the missing case there.

## Architecture

The layout follows TECHNICAL-SPEC §3.1. The big picture:

- **`internal/core`**: a pure, deterministic `Decide(ItemState, Event, Policy) (ItemState, []Action)`. Every call also returns a decision record. It may import **only the standard library and `contract/`**, which `internal/core/imports_test.go` enforces. This keeps it safe to call from Temporal workflow code. Tracker text reaches the core only as `UntrustedText`, which it never reads.
- **`internal/ports`**: the `WorkTracker` and `CodeHost` interfaces (defined in INTEGRATION-SPEC §8) and the internal ports (`Workspace`, `Runner`). Owner checks live in the core: adapters report actors by stable account ID, with no "is owner" method.
- **`internal/adapters/{github,fake}`**: implementations of the ports. Every write takes an idempotency key (TECHNICAL-SPEC §9), so a retry after a crash never writes twice.
- **`contract/`**: the stable, versioned handler contract: `input.json` in, `result.json` out, JSON Schemas in `contract/v1/`, and `ValidateResult`. Handlers never write to GitHub. The worker posts their results under the right identity: coder, reviewer or loop.
- **`internal/runners`, `internal/workspace`**: run a handler in a fresh per-run tree with the Claude Code sandbox settings, passed in `AGENT_LOOP_CLAUDE_SETTINGS`. Secrets never reach a handler's environment.
- **`internal/secrets`**: the env-file and macOS Keychain backends, short-lived GitHub App tokens scoped per identity (INTEGRATION-SPEC §9.1), and log redaction.
- **`internal/profile`**: TOML profile and deployment config with JSON Schema validation and defaults.
- **`internal/workflows`, `internal/projection`, `internal/api`, `internal/notify`, `cmd/agent-loop`**: the Temporal workflows and activities, the SQLite read model and decision log, the local API and CLI, and notifications.
- **`handlers/`**: the starter handlers, one program `handlers/cmd/agent-loop-claude` with private packages in `handlers/internal/`. They may import only `contract/` from this module, and nothing outside `handlers/` may import them; `handlers/imports_test.go` enforces both (D34). They are a handler like any other: they never write to GitHub, and their tests use a fake `claude` binary, never the real one (the live test needs `AGENT_LOOP_LIVE=1`). Their own docs, plans and work plans are in `handlers/docs/`. One `v*` tag releases both binaries (`.goreleaser.yaml`); every release needs a `## <version>` section in both `CHANGELOG.md` and `handlers/CHANGELOG.md`, and `Release` in `handlers/cmd/agent-loop-claude/main.go` must match.

## How work is organised

- New milestones are minted into `plans/` (`plans_dir` in `.agent-loop.toml`): `plans/<key>/plan.md`, `tasks.yaml` and `tasks/<task>.md` (INTEGRATION-SPEC §7, `plans/README.md`). Check a plan before merging it with `go run ./cmd/agent-loop plan validate --profile .agent-loop.toml plans/<key>/`; CI runs `scripts/validate-plans.sh` on every `plans/*/`. `.agent-loop.toml` is this repository's own profile, with the specs as `source_of_truth`.
- The plans made before that layout are in `docs/plans/` and are not converted (v0.1: `2026-11-15-v0-1-mvp.md`, tasks T1–T20 = issues #6–#25; v0.2: `2026-11-25-v0-2-minter-materialise-and-multiple.md`). Each issue gets a work plan in `docs/work/<n>-*.md`, committed with the change. Its **Decisions** section records every spec gap or reading; `ship` files them as a `spec` follow-up issue.
- Issues are worked with the `work-issue`, `review-pr` and `address-pr` skills via `wf.py`. Use one git worktree per issue under `.worktrees/` (gitignored). Never commit to `main`.
- If the base has moved, merge `origin/main` into the branch rather than force-pushing a branch that's under review, then run the local checks again.
- Run `gofmt`: CI fails on unformatted files.
