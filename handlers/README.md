# agent-loop-claude: the starter handlers

Official starter handlers for [agent-loop](https://github.com/mayusuk/agent-loop)
(agent-loop PRODUCT-SPEC D38, INTEGRATION-SPEC §12.4).

agent-loop's core ships no stage handlers (D34). `handlers/` provides one program,
`agent-loop-claude`, with one subcommand per stage. Each subcommand runs Claude Code headless
in the run's workspace with a stage prompt, and turns the agent's output into a `result.json`
that meets the handler contract (INTEGRATION-SPEC §3–§5). A project can use, fork or ignore
these handlers: they meet the stage contracts like any other handler.

```toml
[stages.implement]
command  = ["agent-loop-claude", "implement"]
contract = "1.0"

[stages.review]
command  = ["agent-loop-claude", "review"]
contract = "1.0"
```

Release: **0.1.0** (see [CHANGELOG.md](CHANGELOG.md)), for contract version **1.0**. It ships
with agent-loop v0.1 and covers `implement`, `review` and `address`. `mint` follows with agent-loop
v0.2.

## Install

You need Claude Code (`claude`) installed and logged in. Install `agent-loop-claude` with Homebrew,
on macOS or Linux:

```sh
brew install mayusuk/tap/agent-loop-claude
agent-loop-claude version
```

Homebrew puts it on your `PATH`. It must be on the `PATH` of the agent-loop worker, since
`agent-loop project validate` and the runner look the command up there. `brew upgrade
agent-loop-claude` installs a newer release. `version` prints:

```text
agent-loop-claude 0.1.0
contract versions: 1.0
```

`agent-loop-claude version --json` prints `{"version":"0.1.0","contracts":["1.0"]}`.

To build from source instead, with Go 1.25:

```sh
git clone https://github.com/mayusuk/agent-loop.git
cd agent-loop
go install ./handlers/cmd/agent-loop-claude
```

`go install` puts the binary in `$(go env GOPATH)/bin`, usually `~/go/bin`, which must then be on
the worker's `PATH`. A build from a commit between releases reports the last release.

## Profile

Point each stage of your agent-loop profile (`agent-loop.toml`) at a subcommand. agent-loop
requires `implement`, `review` and `address`:

```toml
[stages.implement]
command      = ["agent-loop-claude", "implement"]
contract     = "1.0"
capabilities = ["agent"]
runner       = "local"
time_limit   = "2h"

[stages.review]
command      = ["agent-loop-claude", "review"]
contract     = "1.0"
capabilities = ["agent"]
runner       = "local"
time_limit   = "2h"

[stages.address]
command      = ["agent-loop-claude", "address"]
contract     = "1.0"
capabilities = ["agent"]
runner       = "local"
time_limit   = "2h"
```

Only `command` and `contract` are required (INTEGRATION-SPEC §12.4). `capabilities`, `runner` and
`time_limit` show agent-loop's defaults (TECHNICAL-SPEC §7.1); leave them out to get the same.
`contract = "1.0"` is the only version this release supports.

Leave `mint` out for now. `agent-loop-claude mint` exists so a profile that names it is valid, but
in this release it always returns `failed` with the reason `mint is not supported in this release
(agent-loop v0.2)`.

### Sandbox

With `runner = "local"`, agent-loop writes Claude Code sandbox settings for each run and passes
their path in `AGENT_LOOP_CLAUDE_SETTINGS`. `agent-loop-claude` hands them to `claude` with
`--settings`, runs it with `--permission-mode acceptEdits`, and never with
`--dangerously-skip-permissions` (plan D12, TECHNICAL-SPEC §7.2). It refuses to start, with a `failed`
result, when the settings are missing (`no sandbox settings: AGENT_LOOP_CLAUDE_SETTINGS is not
set`), don't set both `sandbox.enabled` and `sandbox.failIfUnavailable` to true (`sandbox
settings <path>: sandbox.enabled is not true`, or the same for `sandbox.failIfUnavailable`), or
don't set `sandbox.allowUnsandboxedCommands` to false (`sandbox settings <path>:
sandbox.allowUnsandboxedCommands is not false`), so no command can run outside the sandbox.

To run without the sandbox, set `runner = "local-unsandboxed"` and add `--unsandboxed` to the
command. Claude then has your full user access: your files, any network address and agent-loop's
local API socket. `agent-loop project validate` warns about it.

### Flags

Every stage subcommand takes the same flags, after the subcommand name in `command`:

| Flag | Meaning |
|---|---|
| `--skill <name>` | Run the named Claude Code skill instead of the stage prompt (see below) |
| `--claude <path>` | The `claude` binary to run |
| `--deadline-margin <duration>` | Stop `claude` this long before the run's deadline, so a result is still written. A Go duration; default `2m` |
| `--unsandboxed` | Allow running `claude` when `AGENT_LOOP_CLAUDE_SETTINGS` is not set. If it is set, the settings are still checked and passed. Only for `runner = "local-unsandboxed"` |

## Prompts

Each stage has a built-in prompt (`internal/prompt/builtin/`). A project can replace it in two
ways (INTEGRATION-SPEC §12.4):

- **A prompt file.** Add `.agent-loop/prompts/<stage>.md` (`implement.md`, `review.md` or
  `address.md`) to your repository. It is read from the base branch (`origin/<base>`) at the start
  of the run, never from the working branch, so a change under review cannot rewrite its own
  prompt. It must be a regular file of at most 64 KiB.
- **A skill.** Add `--skill <name>` to the command, for example
  `command = ["agent-loop-claude", "implement", "--skill", "work-issue"]`. The agent is told to run
  that skill in result-only mode: it writes the draft and never posts to GitHub or pushes itself.
  `--skill` wins over a prompt file, and the run log says so.

Whatever the source, every prompt is followed by the same rules, the run's facts and the draft
format for the stage. The run log names the prompt used (`prompt: built-in implement prompt`, `prompt:
.agent-loop/prompts/implement.md from origin/main`, `prompt: skill work-issue`).

The agent ends by writing a **draft**, one JSON file that the handler turns into `result.json`.
Its format is in [docs/agent-draft.md](docs/agent-draft.md). A prompt file or skill must write
that format.

## Environment

agent-loop's worker sets these for each run (INTEGRATION-SPEC §3.1): `AGENT_LOOP_INPUT`,
`AGENT_LOOP_RESULT`, `AGENT_LOOP_OUT` and `AGENT_LOOP_CONTRACT`. `agent-loop-claude` also reads:

| Variable | Meaning |
|---|---|
| `AGENT_LOOP_CLAUDE_SETTINGS` | Path of the Claude Code settings file for the run. Set by agent-loop's `local` runner. Required unless `--unsandboxed` is given |
| `AGENT_LOOP_CLAUDE_BIN` | The `claude` binary to run when `--claude` is not given. Otherwise `claude` on `PATH` (plan D13) |

## Contract versions

This release supports handler contract **1.0** (INTEGRATION-SPEC §2), as `version` reports. It
runs when the worker's `AGENT_LOOP_CONTRACT` is any 1.x, since it ignores the fields a later minor
adds, and writes `failed` for any other major. Releases follow the contract's
major version: a contract 2.x needs a new major release of `agent-loop-claude`.

## Releasing

`agent-loop-claude` is released with agent-loop: one `vX.Y.Z` tag on this repository releases both
programs at the same version (agent-loop TECHNICAL-SPEC §15, work plan #179 D1). The owner cuts
a release; nothing here pushes tags. For a release `X.Y.Z`:

1. Set `Release` in `handlers/cmd/agent-loop-claude/main.go` to `X.Y.Z`, and add its `## X.Y.Z`
   section to `handlers/CHANGELOG.md` as well as to the root `CHANGELOG.md`, in a PR.
   `TestI12_4_` tests check that the CHANGELOG and this README name the same release and contract
   versions.
2. After the PR merges, tag the merge commit and push the tag, as agent-loop's release does.

Pushing the tag runs `.github/workflows/release.yml`. It runs the tests, then
`scripts/release-notes.sh vX.Y.Z`, which prints both CHANGELOG sections and fails if `Release`
isn't `X.Y.Z` or either section is missing or empty, so a mismatched tag publishes nothing. Then
GoReleaser (`.goreleaser.yaml`, v2.20.3) builds both programs for macOS and Linux (arm64 and
amd64) with `main.version` set to `X.Y.Z`, publishes one GitHub release with both programs'
archives, and commits `Formula/agent-loop.rb` and `Formula/agent-loop-claude.rb` to
[mayusuk/homebrew-tap](https://github.com/mayusuk/homebrew-tap).

Check the release config locally, from the repository root, without publishing:

```sh
go run github.com/goreleaser/goreleaser/v2@v2.20.3 check   # reports the deprecated brews section
go run github.com/goreleaser/goreleaser/v2@v2.20.3 release --snapshot --clean
./dist/agent-loop-claude_darwin_arm64_v8.0/agent-loop-claude version
```

`check` exits non-zero only because GoReleaser marks the `brews` (formula) section deprecated, as
in agent-loop (work plan #58 D5).

## Specs

agent-loop's specs, in `specs/` at the repository root, are the source of truth, and they win
over anything in `handlers/`. `docs/specs/agent-loop-excerpts.md` is an older copy of the
sections these handlers implement, from before they moved into agent-loop; if it disagrees with
`specs/`, `specs/` is right.

## Development

The handlers were the `mayusuk/agent-loop-handlers` repository until they moved here, with their
history, as `handlers/` (agent-loop #179); that repository is archived. They are packages of the
agent-loop module, `github.com/mayusuk/agent-loop/handlers/...`, and import only
`github.com/mayusuk/agent-loop/contract` from it, for the contract types, schemas and
`ValidateResult`. Nothing else in the module imports them. `handlers/imports_test.go` enforces
both (D34). The repository's checks cover them; from the root:

```sh
go test -race -count=1 ./handlers/...
```

Tests use a fake `claude` binary, so they need no API key and no network.
Work is planned and tracked in agent-loop with the plan-work / work-issue workflow (the root
`.wf.toml`).

### Live smoke test

`e2e/live_test.go` runs `implement`, then `review` on the pushed head, then `address` on the
findings, with the real Claude Code. It builds `agent-loop-claude`, seeds a small Go module in a
bare local remote, and plays the worker: a fresh clone per stage, `input.json` written as the
worker writes it, and each `result.json` checked with `contract.ValidateResult`. Every stage must
end `done`, and each pushed head must be on the bare remote. It never calls GitHub: a fake `gh` on
`PATH` serves the item and the change, and refuses every other call. The agent gets none of your
GitHub credentials: no `GH_*`/`GITHUB_*` tokens, no real `gh` on its `PATH`, a fresh `HOME` that
links only `~/.claude`, `~/.claude.json` and (on macOS) `~/Library/Keychains` for claude's login,
no ssh agent, and a git that speaks only to local paths.

It is skipped unless `AGENT_LOOP_LIVE=1`, so CI and the normal checks never run it. To run it, have
`claude` installed and logged in, then:

```sh
AGENT_LOOP_LIVE=1 go test -count=1 -timeout 90m -run TestPlanT9_LiveSmoke -v ./e2e/
```

It spends real Claude usage (three headless sessions). Each stage gets a 20-minute deadline.

- `AGENT_LOOP_CLAUDE_BIN`: the `claude` binary to use; otherwise `claude` on `PATH`.
- By default claude runs in Claude Code's sandbox, with the settings TECHNICAL-SPEC §7.2 gives the
  `local` runner, written for each stage to `run-<stage>/claude-settings.json`: the sandbox on and
  required, no command outside it, writes only to the stage's workspace, out directory, result and
  temp directory, reads of `~/.netrc`, `~/.ssh`, `~/.config/gh`, `~/Library/Keychains` and the
  other credential stores denied, and network access only to the Anthropic API and the package
  registries (no GitHub host, no loopback). `AGENT_LOOP_CLAUDE_SETTINGS=<file>` uses your own
  settings file instead; it then is your job to make it as strict.
  The settings generator in `e2e/sandbox_test.go` is a copy of agent-loop's
  `internal/runners/sandbox.go`; `TestT7_2_SandboxCopyInStep` fails when an agent-loop bump changes
  that file, so update the copy and its pinned checksum together.
- `AGENT_LOOP_LIVE_UNSANDBOXED=1` passes `--unsandboxed` instead, for a machine without the
  sandbox. Claude then has your full user access.

With `-v` the test logs each stage's time and, at the end, the `gh` calls the agent made (the fake
`gh` logs them to `run-<stage>/tmp/gh-calls.log`, inside the stage's sandbox). A failing
stage prints the end of its run log.

`TestPlanT9_HarnessDryRun` runs the same flow with the fake `claude` in every `go test`, so the
harness itself stays tested.
