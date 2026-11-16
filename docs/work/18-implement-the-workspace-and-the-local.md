# Work plan: Implement the workspace and the local runner with the Claude sandbox

**Issue:** [#18](https://github.com/mayusuk/agent-loop/issues/18) (feature)
**Branch:** `issue/18-implement-the-workspace-and-the-local`
**Base:** `origin/main @ 6ec9fe5` (checked before branching)
**Status:** In progress

## Summary

Adds `internal/workspace`, which keeps a cached clone of each repository and prepares a fresh working tree per run on the branch INTEGRATION-SPEC §3.3 says, then deletes it. Adds the `local` `Runner` in `internal/runners`: it starts a handler with a built environment, stdin closed and redacted logs, in its own process group stopped at the time limit, serves git credentials over a per-run socket, writes the run's Claude Code sandbox settings (TECHNICAL-SPEC §7.2) and hands them to the handler in `AGENT_LOOP_CLAUDE_SETTINGS`, and refuses a handler the sandbox doesn't wrap unless the stage is `local-unsandboxed`.

## Context

- TECHNICAL-SPEC §5 (`Workspace`, `Runner`), §6.5 (time limit kills the whole process group), §7.1 (environment the runner gives), §7.2 (the `local` runner, the sandbox rule, write paths, denied reads, denied loopback, default egress allowlist), §7.3 (credential helper over a per-run socket in the run's temp directory, narrowed tokens, the redactor), §11 (the API socket directory a sandbox must not open).
- INTEGRATION-SPEC §3.1 (handler invocation table), §3.3 (workspace and branch rules), §3.4 (credentials by stage: no push token for review), §13 (`stages.<stage>.runner`, `egress`).
- PRODUCT-SPEC FR-18 AC3 (missing or invalid result is `failed`), AC4 and NFR-7 (a handler that never exits is stopped), FR-13 AC2–AC3.
- Plan `docs/plans/2026-11-15-v0-1-mvp.md`, task T13. Coordinator note on #18: the runner writes the settings file and passes its path in `AGENT_LOOP_CLAUDE_SETTINGS`; the starter handlers run `claude --settings "$AGENT_LOOP_CLAUDE_SETTINGS"` (agent-loop-handlers D12, T8).
- On main: `contract` (#7: `Input`, `ValidateResult`, `Version`), `internal/ports` (#8: `Workspace`, `Runner`, `RunSpec`, `RunOutcome`, `Token`; work plan D14 lets this task own and extend their argument types), `internal/profile` (#9: `Stage.Runner`, `Egress.Effective`, `Deployment` paths), `internal/secrets` (#17: `Lease` that renews near expiry, `Redactor`; its D12 says the runner must not pass `AGENT_LOOP_SECRET_*` to a handler).
- Claude Code's settings format (`sandbox.enabled`, `sandbox.filesystem.allowWrite|denyRead`, `sandbox.network.allowedDomains|deniedDomains|allowUnixSockets|allowLocalBinding`, `sandbox.failIfUnavailable`, `sandbox.allowUnsandboxedCommands`, `permissions`), checked against the published settings schema (json.schemastore.org/claude-code-settings.json) on 2026-11-16, Claude Code 2.1.316. In that format a path starting `//` is absolute and one starting `/` is relative to the settings file.

## Approach

**`internal/workspace`** (`Git`, implements `ports.Workspace`):
- Root defaults to `<user cache dir>/agent-loop/workspaces`: `repos/<key>.git` (a bare cached clone per repository URL) and `runs/<slug>-<random>` (one per run).
- `Prepare` locks the cached clone (a `flock` on `repos/<key>.lock`, so two workers on one host don't race), clones it if missing, fetches every branch (`+refs/heads/*:refs/heads/*`, `--prune`), then makes the run's tree with `git clone --shared --no-checkout` from the cache, points `origin` at the real URL, and checks out: a detached commit for review; the remote branch head when the branch exists; otherwise a new branch from the base tip with its upstream set to the same name. Returns the real path (symlinks resolved), the branch and the head.
- The fetch authenticates with an optional token callback, passed to git as an `http.extraHeader` in `GIT_CONFIG_*` environment variables, never in arguments. Git runs with no system or global config and never prompts.
- `Release` removes the run's tree; it refuses a path outside `runs/`, and an already deleted tree is not an error. The branch stays.

**`internal/runners`** (`Local`, implements `ports.Runner`):
- `sandbox.go`: `Settings(SandboxPaths) ([]byte, error)` builds the settings JSON; `DeniedReads(dep, home)` lists the §7.2 reads; `Wrapped(command)` tells whether the sandbox wraps a handler.
- `credhelper.go`: the per-run socket server (`get` answers the push token for `https` on the repository's host only, nothing otherwise) and the client `CredentialHelperMain`, which `agent-loop credential-helper <socket> <op>` runs.
- `local.go`: `Run` checks the spec, makes the run directory (`<tmp>/al-XXXX/` with `git.sock`, `claude-settings.json` and the writable `tmp/`), configures the helper in the run's tree, builds the environment from scratch, starts the handler in its own process group with stdin on `/dev/null` and stdout and stderr through the redactor into the log file, stops the group at the time limit (SIGTERM, then SIGKILL after a grace period) and kills what is left of the group when the handler exits.
- `result.go`: `ReadResult(input, outcome)` reads and validates `result.json`; anything missing or invalid is a `failed` result, whatever the exit code.
- `internal/ports`: `RunSpec` gains `RepoURL`, `Runner` and `LogPath`; `PushToken`/`ReadToken` become `Push`/`Read` of a new `TokenSource` interface (which `secrets.Lease` already implements), so the helper serves a renewed token.
- `cmd/agent-loop`: a hidden `credential-helper` subcommand that calls `runners.CredentialHelperMain`.

Tests use the test binary itself as the fake handler and the helper client (dispatched from `TestMain`), a symlink named `agent-loop-claude` for the sandboxed path, local bare repositories as remotes, and no network or `claude`.

## Steps

- [x] Ports: `TokenSource`, `RunSpec` fields; update the ports tests.
- [x] Workspace: cached clone, fetch, per-run tree with the §3.3 branch rules, release; tests against local repositories.
- [x] Sandbox settings: generator, denied reads, `Wrapped`; golden test against §7.2.
- [ ] Credential helper: server, client, hidden CLI subcommand; tests for push token served and nothing for review.
- [ ] Local runner: environment, stdin, redacted log, process group and time limit, sandbox refusal and warning; `ReadResult`; tests NFR-7 AC1 and FR-18 AC3.

## Acceptance criteria

- A handler that never exits is stopped at its time limit, together with every child process (TestNFR7AC1_...).
- A missing or invalid `result.json` gives `failed`, whatever the exit code (TestFR18AC3_...).
- A golden test checks the generated sandbox settings against TECHNICAL-SPEC §7.2.
- The credential helper serves the stage's push token, and nothing for review (INTEGRATION-SPEC §3.4).

## Decisions

- **D1** [gap] TECHNICAL-SPEC §7.2, coordinator note on #18: the `local` runner writes the run's Claude Code settings file and passes its absolute path to the handler in `AGENT_LOOP_CLAUDE_SETTINGS`; the starter handlers run `claude --settings "$AGENT_LOOP_CLAUDE_SETTINGS"` (agent-loop-handlers D12). The variable is not set for `local-unsandboxed`. Why: §7.2 says the runner generates the settings but not how the handler gets them.
- **D2** [ambiguity] TECHNICAL-SPEC §5, §7.2, INTEGRATION-SPEC §3.3: the "fresh worktree per run" is a separate clone made with `git clone --shared` from the cached clone, not a `git worktree`. Why: a `git worktree` keeps its index, refs and new objects in the cached clone, so a handler committing would need write access outside its tree, which §7.2 forbids ("Nothing else"). A shared clone borrows the cache's objects read-only and writes only inside the run's directory. The cached clone has `gc.auto = 0`, so objects a run borrows are never pruned under it.
- **D3** [gap] TECHNICAL-SPEC §5, §7.3: `RunSpec` names the handler's tokens as `Push` and `Read` of type `ports.TokenSource` (`Token(ctx)`), not fixed tokens, so the credential helper can hand out a renewed token near the one-hour expiry (`secrets.Lease` implements it; `ports.StaticToken` wraps a fixed one). It also gains `RepoURL` (the helper serves only that host), `Runner` (`local`, the default, or `local-unsandboxed`) and `LogPath`. Why: §7.3 has the helper renew tokens; the sketch's argument types are owned by this task (ports work plan D14).
- **D4** [gap] INTEGRATION-SPEC §3.3: the cached clone is a bare repository per repository URL whose `refs/heads/*` mirror the remote's branches (`git fetch --prune --no-tags origin +refs/heads/*:refs/heads/*`), fetched before every `Prepare` under an exclusive `flock`, so runs on one host never fetch the same clone at once. A run on an existing branch gets a local branch at the remote head, tracking it; a new branch starts at the base tip with its upstream set to `refs/heads/<name>` on `origin`, so a plain `git push` creates it; review gets the commit detached. `origin` in the run's tree is the repository URL, not the cache. A commit or base branch the remote doesn't have is `ErrNotFound`; a failed fetch is `ErrTransient`. Branch names must pass `git check-ref-format --branch` and not start with `-`; a detach must be a full SHA.
- **D5** [gap] TECHNICAL-SPEC §13: the workspace root defaults to `<user cache dir>/agent-loop/workspaces` (`~/Library/Caches/agent-loop/workspaces` on macOS, `$XDG_CACHE_HOME` or `~/.cache` on Linux), with `repos/` and `runs/` (mode `0700`) inside. It is stored with symlinks resolved, so the paths in the sandbox settings are real ones. Why: it must be readable by the sandbox, so it can't sit in the state directory the sandbox denies (D9).
- **D6** [gap] TECHNICAL-SPEC §7.3: the worker's own git commands (the fetch and the run's clone) read no system or global git config (`GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL=/dev/null`), never prompt, and drop every inherited `GIT_*` variable. The fetch of an `https` URL authenticates with a token from an optional callback, passed as an `http.extraHeader` (`Authorization: Basic` of `x-access-token:<token>`) through `GIT_CONFIG_*` environment variables, never in arguments. Which identity's token it uses is the caller's choice (T15). Why: the owner's global config may name credential helpers or URL rewrites the loop must not use.
- **D7** [gap] INTEGRATION-SPEC §3.3: `Release` deletes only a direct child of `runs/`, refuses any other path, and treats a tree that is already gone as released. Branches stay, on the remote and in the cache.
- **D8** [gap] TECHNICAL-SPEC §7.2: the settings file sets `sandbox.enabled`, `sandbox.failIfUnavailable` (Claude Code refuses to start rather than run unsandboxed), `autoAllowBashIfSandboxed`, `allowUnsandboxedCommands = false` and no `excludedCommands`; `sandbox.filesystem.allowWrite` is exactly the run's tree, `AGENT_LOOP_OUT`, the result path and the run's temp directory; `sandbox.filesystem.denyRead` is D9; `sandbox.network.allowedDomains` is the stage's egress list; `allowUnixSockets` is only the helper's socket, with `allowAllUnixSockets` and `allowLocalBinding` false. Because the sandbox wraps only Claude's shell commands, `permissions` repeat the limits for Claude's own tools: `Read(...)` deny rules for every denied path, `WebFetch(domain:...)` allow rules for the allowlist only, `additionalDirectories` for the out and temp directories, and `disableBypassPermissionsMode = "disable"` (skip-permissions is for the Docker runner only). Absolute paths are written with a leading `//`, as Claude Code requires. Why: §7.2 lists what the settings must do, not Claude Code's keys.
- **D9** [gap] TECHNICAL-SPEC §7.2: the denied reads are `~/.config/agent-loop`, `~/.local/state/agent-loop` (agent-loop's state directory, which holds the default projection database, and where the server's Temporal data must live: T17 should put it there), the deployment file's directory, the API socket's directory, the projection database and its `-wal`, `-shm` and `-journal` files, the env/file secrets file, `~/.ssh`, `~/.aws`, `~/.config/gh`, `~/.netrc`, `~/.docker`, `~/.kube`, `~/.git-credentials`, `~/.config/git/credentials` and `~/Library/Keychains`. A path that is a symlink is denied under both names. Why: §7.2 names "the server's Temporal data" and "the git credential stores" without paths.
- **D10** [gap] TECHNICAL-SPEC §7.2 "Loopback and the local network are denied": `sandbox.network.deniedDomains` always holds `localhost`, `*.localhost`, `127.0.0.1`, `::1`, `0.0.0.0`, `*.local` and `host.docker.internal`, which win over any allow, and those names are dropped from the allowlist even when the profile's `egress.allow` names them. Addresses outside the allowlist are denied by the sandbox anyway.
- **D11** [gap] TECHNICAL-SPEC §7.2 "a handler that the Claude sandbox doesn't wrap": a handler is wrapped when its command's program base name is `agent-loop-claude` (the starter handlers, which run Claude Code with `--settings "$AGENT_LOOP_CLAUDE_SETTINGS"` and refuse to run without it). `runners.Wrapped` is exported for `project validate` (T18). Every handler stage is an agent stage in v0.1, so the rule applies to every run of the `local` runner. Why: the spec gives the rule but no way to tell which handlers it covers; another Claude-based handler can be added to the list when one exists.

## Verification

Standard checks. Process-group tests check with `kill(pid, 0)` that every child is gone. No test needs `claude`, GitHub or the network.

## Risks and open questions

- A child that leaves the process group (`setsid`) escapes the group kill. The Claude sandbox and token revocation at the end of the run (§7.3) limit what it can do.
- The real Claude sandbox is not exercised here; the settings follow the published schema, and a live check belongs to the handlers' live test.

## Outcome

_Filled in before shipping._
