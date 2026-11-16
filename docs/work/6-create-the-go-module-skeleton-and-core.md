# Work plan: Create the Go module skeleton and core import guard

**Issue:** [#6](https://github.com/mayusuk/agent-loop/issues/6) (feature)
**Branch:** `issue/6-create-the-go-module-skeleton-and-core`
**Base:** `origin/main @ 9282b91` (checked before branching)
**Status:** Done

## Summary

Creates the Go module `github.com/mayusuk/agent-loop` with the package layout of TECHNICAL-SPEC §3.1, a `doc.go` per package, and an `agent-loop` binary whose dispatcher prints help (listing the TECHNICAL-SPEC §11 commands) and the version. Adds a Go test that keeps `internal/core` limited to the standard library and `contract/`, proven against fixtures.

## Context

- Plan `docs/plans/2026-11-15-v0-1-mvp.md`, task T1, and plan decision D1: the core import rule (TECHNICAL-SPEC §3.1, which names depguard) is enforced by `internal/core/imports_test.go` instead.
- TECHNICAL-SPEC §3.1 (layout), §11 (CLI commands), §15 (release mapping), T1 (Go, one module).
- `.wf.toml` notes: Go 1.25, gofmt required, tests named after requirements where one applies.
- CI (`.github/workflows/ci.yml`) already runs gofmt, vet, build and `go test -race` on Linux and macOS once `go.mod` exists (NFR-3 AC1). No CI change is needed.
- The repository has no Go code yet.

## Approach

- `go.mod`: `module github.com/mayusuk/agent-loop`, `go 1.25`. No dependencies.
- One `doc.go` per package of §3.1 that this release needs: `internal/core`, `internal/ports`, `internal/adapters/github`, `internal/adapters/fake`, `internal/runners`, `internal/workspace`, `internal/secrets`, `internal/notify`, `internal/workflows`, `internal/projection`, `internal/api`, `internal/profile`, `contract`, `conformance`.
- `cmd/agent-loop/main.go`: `main` calls `run(args, stdout, stderr) int`, which dispatches on the first argument. `help`, `-h`, `--help` print the help; `version` prints `agent-loop <version>`. Each §11 command is known and reports "not implemented yet". The command table lives in one slice that drives both dispatch and help, so they can't drift. `version.go` holds `var version = "dev"`, set by `-ldflags "-X main.version=..."`, falling back to the module version from `runtime/debug.ReadBuildInfo` for `go install`ed builds.
- `internal/core/imports_test.go`: parses every `.go` file under a directory with `go/parser` (imports only), skipping `testdata`, and reports each import that is neither standard library nor `github.com/mayusuk/agent-loop/contract[/...]`. One test runs it on `internal/core` itself; fixture tests in `internal/core/testdata/` prove it flags forbidden imports and accepts allowed ones.

## Steps

- [x] Add `go.mod` and a `doc.go` for each package of TECHNICAL-SPEC §3.1.
- [x] Add the `agent-loop` command dispatcher with help and version, and its tests.
- [x] Add the `internal/core` import guard with fixtures and tests.

## Acceptance criteria

- `go build ./...`, `go vet ./...` and `go test -race ./...` pass on Linux and macOS in CI.
- `agent-loop version` prints the version, and `agent-loop help` lists the planned subcommands of TECHNICAL-SPEC §11.
- A test proves that the import guard fails on a forbidden import, using a fixture.

## Decisions

- **D1** [gap] TECHNICAL-SPEC §11: `agent-loop version` prints one line, `agent-loop <version>`. The version is `dev` unless set at link time with `-ldflags "-X main.version=<v>"`; a `go install`ed build uses its module version. Why: the spec names no version format or source.
- **D2** [gap] TECHNICAL-SPEC §11: `agent-loop help`, `-h` and `--help` print the help to stdout and exit 0. No arguments prints the help to stderr and exits 2. An unknown command prints `agent-loop: unknown command "<x>"` and the help to stderr, and exits 2. Why: the spec defines no CLI usage errors; this follows the `go` tool's convention.
- **D3** [gap] TECHNICAL-SPEC §11, §15: help lists every command of the §11 table, including `mint` and `plan abandon`, which are v0.2 (§15), and leaves out `notify-agent` (§12, v0.3), which is not in the §11 table. A listed command that is not built yet exits 1 with `agent-loop <cmd>: not implemented yet`. Why: the issue asks for the planned §11 subcommands; later tasks replace the stubs.
- **D4** [gap] TECHNICAL-SPEC §3.1: packages marked "later" or v0.2 get no directory now (`internal/adapters/jira`). `internal/runners` is one package for now; whether `local` and `docker` become subpackages is left to T13. Why: empty future packages add nothing, and the runner split is an implementation choice.
- **D5** [gap] TECHNICAL-SPEC §3.1, plan D1: the import guard treats an import as standard library when its first path element has no dot (the rule `go` itself uses for module paths), except `C` (cgo), which is forbidden, and allows `github.com/mayusuk/agent-loop/contract` and its subpackages only (not, for example, `.../contractx`). It checks every `.go` file under `internal/core`, test files included, except `testdata/`. Why: the spec states the rule, not how to detect the standard library or whether test files count.

## Verification

- Build the binary and run `agent-loop version` and `agent-loop help` by hand.
- Temporarily add a forbidden import to `internal/core` and see the guard fail (not committed).

## Risks and open questions

None. The skeleton changes no product behaviour.

## Outcome

- Added `go.mod` (Go 1.25, no dependencies) and a `doc.go` for each v0.1 package of TECHNICAL-SPEC §3.1.
- Added `cmd/agent-loop` with a table-driven dispatcher: `help` lists every §11 command, `version` prints the version, and the other commands report "not implemented yet" (D1–D3). Tests cover help, no arguments, an unknown command, each stub, and each version source.
- Added `internal/core/imports_test.go`, which checks `internal/core` itself and proves against `testdata/forbidden` and `testdata/allowed` fixtures that it flags module, third-party, look-alike (`contractx`), cgo, nested and test-file imports, and ignores `testdata` (D5). A forbidden import added by hand made it fail, as expected.
- Deviation: the help command is attached to the command table in `init()`, because a direct reference made a Go initialization cycle. No behaviour change.
- No follow-up issues.
