# Technical Spec: agent-loop

| | |
|---|---|
| **Status** | Draft, 2026-11-14. Not yet reviewed |
| **Implements** | `PRODUCT-SPEC.md` (approved 2026-11-14) |
| **Companion** | `INTEGRATION-SPEC.md`: exact port contracts, handler contract, tracker records, task list |

## 1. Purpose and scope

This spec says how agent-loop is built: the technology, the split into components, the Temporal design, and the ordered rules the product spec moved here (D17, D27). It changes no product behaviour. Where it seems to, `PRODUCT-SPEC.md` wins and this spec is wrong.

The main design goal is **extension without core changes** (Goal 4). Trackers, code hosts, agents, sandboxes, secret stores and notification channels all plug in behind ports. The core knows none of them.

Out of scope here, and in `INTEGRATION-SPEC.md`: field-level JSON schemas, the exact tracker records each adapter reads, and the task-list format.

## 2. Technology decisions

| # | Area | Decision | Why |
|---|---|---|---|
| T1 | Core language | Go, one module | Mature Temporal SDK, a single static binary for macOS and Linux (NFR-3), strong typing for the state machine |
| T2 | Orchestration | Temporal, self-hosted with Docker Compose (Temporal + Postgres) in v0.1 | Durable workflows, timers and retries (NFR-1). Moving to a remote server or Temporal Cloud is a change to the connection config (NFR-3 AC2). Temporal gives exactly-once workflow state, durable timers (daily reset, merge time limit), activity timeouts with process kill, and a per-ID lock (FR-2 AC4) without our own code for them. **Considered and rejected by the owner on 2026-11-15 (R1):** `temporal server start-dev` with SQLite (no Docker, but not supported for production use), and a reconciler over the tracker plus SQLite (less to run, but every timer, lock and kill path is our own code). The reason is production parity from day one: v0.1 runs the same Temporal and Postgres that a remote server will. The cost is that Docker is a prerequisite (getting-started guide §1), there are two databases (Temporal's Postgres and the SQLite projection, T11), and every workflow change needs worker versioning (§6.3) |
| T3 | Tracker vs code host | Two separate ports: `WorkTracker` (items) and `CodeHost` (changes, reviews, gates, merge) | Jira holds issues but not code, so "Jira items + GitHub changes" has to work without core changes. D3 builds only GitHub, but product §3 requires the contracts to stay tracker-neutral. The cost now is two interfaces that one GitHub adapter implements with a shared client. A single `Tracker` port was the alternative: less code today, but a core change when the first non-GitHub tracker comes |
| T4 | Tracker and code-host adapters | Go interfaces, compiled into the binary, selected in the profile. GitHub first, Jira and GitLab later | They are few, written by us, and need tight correctness (claims, idempotency) |
| T5 | Loop state in the tracker | Abstract **markers** (§5.1 labels become markers). Each adapter maps a marker to a label, a status or a field, as the profile says | GitHub uses labels. Jira teams expect workflow statuses. FR-1 AC4 already needs a name mapping for every label, so the marker mapping adds only the choice of label or status. Hard-coded labels were the alternative, and would fail FR-1 AC4 |
| T6 | Agent stage handlers | A subprocess with a JSON contract: the core starts a command from the profile, passes `input.json`, reads `result.json` | Any language and any agent. New agents need no Go code (D34) |
| T7 | First handlers (outside the core) | The starter handlers `agent-loop-claude` (D38, INTEGRATION-SPEC §12.4), which run Claude Code headless and can use the adopter's skills through `--skill`. Their first release ships with v0.1 (owner decision X7, §15) | The v0.1 adopter's skills already exist. A Claude Agent SDK handler was considered and is not planned: D38 names Claude Code |
| T8 | Sandboxing | A `Runner` port. v0.1: `LocalRunner`, a git worktree per run inside Claude Code's OS sandbox (Seatbelt on macOS, bubblewrap on Linux). v0.2: `DockerRunner`, a container per run | Unattended agents must not act with the owner's full access. The Claude sandbox is used in v0.1 because it already exists, works without Docker (D39), and covers the starter handlers. Its limits are in §7.2: it wraps only Claude-based handlers, and it must deny reads of agent-loop's own secrets and socket. Alternatives: running every local handler under `sandbox-exec` or bubblewrap ourselves (covers any handler, but is our own sandbox code on two platforms), or Docker from v0.1 (strongest, but Docker becomes a hard requirement for every adopter) |
| T9 | Network egress | An allowlist per project (defaults in §7.2, plus profile additions). Enforced by the Claude sandbox in v0.1, and by a container network in v0.2 | Limits what a prompt-injected agent can send out (§8.5). The code host stays reachable, so the real limit on what an agent can do there is its token's scope (§7.3) |
| T10 | Container images (v0.2) | agent-loop publishes a base image (git, Node, the agent CLIs). Each project extends it with its toolchain and names the image in the profile | Projects own their toolchains (§9) |
| T11 | Read model and decision log | A projection database: SQLite in v0.1, Postgres from v0.3 (remote server). Temporal stays the source of truth | The board (FR-11) needs fast cross-item queries that Temporal visibility doesn't give |
| T12 | Secrets | A `SecretStore` port: macOS Keychain and env/file backends in v0.1. 1Password or Vault later. Tokens are short-lived and issued per run, per identity. On macOS, agent-loop is signed with a stable self-signed certificate and the identifier `dev.agent-loop`, so Keychain items keep trusting it across rebuilds and upgrades (§7.2) | FR-13 AC3, §8.7 |
| T13 | Owner interface | The server exposes a local API (gRPC) on a Unix socket that only the owner's account can open, and that handler sandboxes can't reach (§11). An HTTP/JSON gateway is off by default. The `agent-loop` CLI is one client. Owner actions through tracker markers keep working | A web board or chat bot can be added later as another client. gRPC gives a typed, versioned API with streaming for `board --watch` and logs, from one schema for the CLI and later clients. Plain HTTP/JSON was the alternative: less tooling, but a hand-kept schema. The API acts as the owner (a resume approves untrusted commits, FR-13 AC6), so a loopback TCP port that any local process, including an agent, can call is not used |
| T14 | Notifications | A `Notifier` port: macOS desktop and tracker mention in v0.1. Slack and push later | FR-11 AC3, AC5 |
| T15 | Change detection | Polling with incremental queries (GitHub: ETags on REST and `since`. Jira: JQL `updated >= cursor`). No webhooks by default | NFR-4: no inbound access to hosts. NFR-6 needs only one supervisor interval (10 minutes). The 60 s poll (§6.2) is for the owner: a label or resume takes effect within a minute, which the getting-started guide promises. Its cost is measured in Q4, and the interval is a profile setting (§13) |
| T16 | Testing | In-memory fake adapters plus a shared **conformance suite** that every adapter must pass (fake, GitHub, later Jira) | Scenario 11.1 runs fast and without GitHub. New adapters prove their behaviour |
| T17 | Configuration format | TOML for the project profile and the deployment config, with a JSON Schema for validation | Matches the first adopter (`.wf.toml`), and humans edit it |
| T18 | Who pushes | The handler pushes as the coder identity, with a scoped short-lived token (Q1) | The handler already acts as the coder. Limits on the token make misuse small. The token can do anything `contents: write` allows on the repository, so the limits are rulesets on the code host, not the token alone (INTEGRATION-SPEC §9.8, I10) |
| T19 | Untrusted commits | Prevented by a branch ruleset that lets only the coder identity and owner accounts push to the loop's branches. Pusher history is not read (Q2). The ruleset is FR-13 AC5's push record (product spec D40) | Meets FR-13 AC5 by construction, with less code and no dependence on event retention |
| T20 | Review results | A PR review by the reviewer identity, with a hidden record marker in its body, a status table, and one thread per finding that has a file (INTEGRATION-SPEC §6.1, §6.3, I8). Only reviewer-authored reviews count (Q3) | Machine-readable, and can't be forged by other accounts |
| T21 | Who writes tracker records | The worker. Handlers return content in `result.json`, and the worker validates it and posts it as the stage's identity (INTEGRATION-SPEC I1). Handlers only push code | Records are always valid and idempotent. The review handler gets read-only access |
| T22 | Dependencies on GitHub | Native issue dependencies ("blocked by"). Body text isn't parsed (INTEGRATION-SPEC I3) | Structured data, not untrusted text |
| T23 | Task list | `plans/<plan-key>/tasks.yaml` next to `plan.md`, with a JSON Schema (INTEGRATION-SPEC I4, §7) | Easy to validate and diff |

## 3. Architecture

```
          Owner ── agent-loop CLI ──► Local API (gRPC / HTTP, localhost)
                                            │
┌──────────────────────── agent-loop server (Go) ────────────────────────────┐
│ core/        pure decision logic: lifecycle, next_stage, merge conditions,│
│              round counting, limits. No I/O, no Temporal imports           │
│ workflows/   Temporal workflows that feed events to core and run the       │
│              actions it returns as activities                              │
│ projection/  board rows and decision log (SQLite, later Postgres)          │
│ api/         local API for the CLI                                         │
│                                                                            │
│ ports/  WorkTracker  CodeHost  Workspace  Runner  SecretStore  Notifier    │
└──────┬───────────┬──────────┬──────────┬───────────┬──────────┬────────────┘
       │ github    │ github   │ git      │ local     │ keychain │ desktop
       │ jira*     │ gitlab*  │ worktree │ docker*   │ env/file │ tracker
       │ fake      │ fake     │          │ remote*   │ vault*   │ slack*
                                          │
                         subprocess + JSON stage contract (versioned)
                                          │
                Project-supplied handlers: agent-loop-claude
                (starter, D38), any other command            (* = later)
```

**Workers** are the same binary started with `agent-loop worker --capabilities agent,docker`. A worker runs activities: adapter calls and handler runs. The **server** process runs the workflows and the API. In v0.1 both run on the owner's Mac, and they can also run in one process.

### 3.1 Package layout

```
cmd/agent-loop/          CLI, server, worker entry points
internal/core/           lifecycle, rules, types. Pure, table-tested
internal/ports/          interfaces only
internal/adapters/
    github/              WorkTracker + CodeHost
    fake/                in-memory WorkTracker + CodeHost, scriptable
    jira/                later
internal/runners/        local (worktree + Claude sandbox), docker (v0.2)
internal/workspace/      clone cache, worktrees
internal/secrets/        keychain, envfile
internal/notify/         desktop, tracker
internal/workflows/      Temporal workflows and activities
internal/projection/     read model and decision log
internal/api/            gRPC service and gateway
internal/profile/        TOML loading, JSON Schema validation, defaults
contract/                handler contract types and JSON Schemas (stable, versioned)
conformance/             adapter conformance suite
```

`internal/core` may import only the standard library and `contract/`. A lint rule (depguard) enforces this, so the rules stay deterministic and can be called from workflow code.

## 4. Core model

The core is a pure function:

```go
// Decide is deterministic: the same state and event always give the same result.
func Decide(s ItemState, e Event, p Policy) (ItemState, []Action)
```

- **`ItemState`**: lifecycle state (§6 of the product spec), round allowance and rounds used, base updates used, attempt count and deadline of the current stage, latest review (commit, kind, findings), manual comments and their clearing, approved commits (FR-13 AC6), and the reason for the last escalation.
- **`Event`**: a normalised fact from an adapter or a stage. For example `MarkerChanged{By, Marker, Added}`, `HeadMoved{Commit, PushedBy}`, `StageResult{Stage, Result}`, `GateResult`, `ManualCommentChanged`, `OwnerCommand`, `Tick`, `DayBoundary`.
- **`Action`**: what to do next. For example `RunStage{Stage, Kind}`, `SetMarker`, `Comment{IdempotencyKey}`, `Merge{PinnedHead}`, `Escalate{Reason}`, `Notify`, `FileFollowUp`, `Wait{Reason}`.
- **`Policy`**: the limits and names from the profile.

Every decision produces a **decision record** (NFR-5 AC1): the event, the tracker state it saw, the stage result, and the actions. The workflow writes it to the projection.

Tracker text (bodies, comments, reply text) reaches the core only as the type `UntrustedText`. The core has no operation that reads its content. It can only pass it on by reference (§8.5, NFR-4 AC3).

## 5. Ports

`WorkTracker` and `CodeHost` are defined only in `INTEGRATION-SPEC.md` §8, which is the source of truth for their methods and types. This section doesn't repeat them.

**Owner checks live in the core.** Adapters report every actor as an `Actor` with its stable account ID (INTEGRATION-SPEC §8.1). The core compares that ID with the profile's owner account IDs (FR-13 AC7) and the three identities. No port has an "is owner" method, so every adapter applies the same rule.

The internal ports (INTEGRATION-SPEC §8.4) are sketched here. Every method that writes takes an idempotency key (§9).

```go
type Workspace interface {
    Prepare(ctx, RepoRef, Branch) (Dir, error)  // cached clone + fresh worktree per run
    Release(ctx, Dir) error
}

type Runner interface {
    Run(ctx, RunSpec) (RunOutcome, error)   // command, workspace, env, limits, egress policy
}

type SecretStore interface {
    Token(ctx, Identity, Scope) (Token, error)  // short-lived where the tracker allows
    Revoke(ctx, Token) error                    // at the end of each run (§7.3)
}

type Notifier interface {
    Notify(ctx, Notification) error
}
```

A tracker and a code host can be different systems. They are linked by the item reference that the code host records on the change (for example `PROJ-42` in the branch name or change body). The format is part of the integration spec. Adapters that serve both ports (GitHub) share one client and one rate-limit budget.

An adapter that can't support a capability the core needs (for example assigning an item) says so through a `Capabilities()` method. `project validate` then fails or warns, and the core never assumes a GitHub-only feature.

## 6. Temporal design

### 6.1 Workflows

| Workflow | ID | Lifetime | Job |
|---|---|---|---|
| `DeploymentWorkflow` | `deployment` | Forever (continue-as-new) | Deployment pause (v0.1, FR-10 AC3), deployment-wide agent-run slots (v0.2), host health view |
| `ProjectWorkflow` | `project/<name>` | Forever (continue-as-new) | Supervisor: polls, picks, claims, holds WIP, the daily counter, project agent-run slots and project pause. Routes events to items |
| `ItemWorkflow` | `item/<project>/<item-key>` | From claim to close or release | Runs one item's lifecycle by calling `core.Decide` |
| `MintWorkflow` | `mint/<project>` | One per mint run | FR-9. Its ID guarantees one minter per project (FR-9 AC6) |
| `MaterialiseWorkflow` | `plan/<project>/<plan-key>` | From approval to done or abandon | FR-17 |

The workflow ID is the item's lock. Temporal refuses a second running workflow with the same ID, so an item is never worked by two runs at once, even after a duplicate start (FR-2 AC4). Workflows are started with the ID conflict policy *fail* (a running workflow with the ID refuses the start) and the ID reuse policy *allow duplicate* (a closed one doesn't). So an item that is released or closed and later picked again gets a new `ItemWorkflow` with the same ID. Each such claim has a **claim number**: one more than the number of `claim` records on the item (INTEGRATION-SPEC §6.5). It is part of every idempotency key (§9), so records from an earlier claim are never mistaken for this claim's.

### 6.2 Supervisor loop

`ProjectWorkflow` runs a timer every `supervisor_interval` (default 10 minutes, FR-12 AC1), plus a shorter poll interval for changes (default 60 s, which meets NFR-6). Each pass:

1. A `PollChanges` activity calls `WorkTracker.Changes` and `CodeHost.Changes` from the stored cursors, and returns normalised events.
2. It signals each event to the matching `ItemWorkflow`, or handles it as an unclaimed-item event (FR-2 AC6, reopen, owner-opened change).
3. At the full supervisor interval, it reconciles: it compares the full tracker state with its running workflows, starts missing ones, and stops runs for items closed or merged by hand (FR-12 AC1).
4. It picks: while WIP and the daily limit allow, and the project isn't paused, it claims the next candidate in priority order (FR-2 AC2) with `Claim` (compare-and-set), then starts its `ItemWorkflow`.

Cursors, WIP and the daily count live in the workflow state and survive continue-as-new. `ProjectWorkflow` and `DeploymentWorkflow` continue as new when Temporal suggests it (`GetContinueAsNewSuggested`), and at the latest after each full reconcile, so their histories stay small. An `ItemWorkflow` continues as new after 2,000 events, which a long manual-comment loop can reach.

### 6.3 Item workflow

```
loop:
    actions := core.Decide(state, event, policy)       // pure, deterministic
    for each action: run it as an activity, or wait
    event := next signal | activity result | timer
```

- **Signals in:** tracker and code-host events from the project, owner commands from the API, slot grants.
- **Queries out:** current state and next action, for the board.
- **Stage runs:** a `RunStage` activity on the task queue for the stage's capabilities (§6.4).
- **Slots:** before an agent stage, the item asks `ProjectWorkflow` for an agent-run slot and a daily-limit unit with an Update. The answer is `granted`, `wait_slot` or `waiting_limit` (FR-16 AC2–AC3). The project releases slots when the activity ends.
- **Workflow code changes:** guarded with Temporal worker versioning, so long-running items are never replayed against incompatible code.

### 6.4 Capabilities and task queues

A stage declares the capabilities it requires (default `agent`). The task queue name is the sorted set, for example `cap:agent` or `cap:agent+x`. A worker with capabilities C polls the queue of every stage capability set in the profile that is a subset of C. A worker joining needs no workflow change (FR-14 AC1).

A host is healthy when its worker has polled within the last minute (Temporal `DescribeTaskQueue` pollers) and its last `agent-loop host check` passed. The board shows a stage waiting on a queue with no healthy poller (FR-11 AC4). Adapter-only activities run on a `core` queue that every worker polls.

### 6.5 Time limits, retries and stopping

- **Time limit (FR-16 AC1, NFR-7 AC1):** a `RunStage` activity has `StartToCloseTimeout` = stage limit and heartbeats every 30 s, with a `HeartbeatTimeout` of 2 minutes. The `deadline` in `input.json` is the activity's start plus that limit (cut to the core's deadline when one is set), and the runner gives the handler what is left of it once the workspace is ready (INTEGRATION-SPEC §4.1). On timeout or cancellation, the runner kills the handler's whole process group (local) or the container (docker), and the workflow escalates.
- **Worker crash (FR-16 AC1, NFR-1 AC2, D42):** a lost heartbeat (2 minutes) ends the activity, and the workflow starts the stage again in a new session with a new `run_id` and the **same attempt number** (INTEGRATION-SPEC §4.1), so a crash doesn't use an attempt. The core counts crash reruns for each attempt in `ItemState`. After 3 crash reruns of the same attempt, the item escalates with the reason `worker_lost` instead of a fourth rerun. Each interrupted run is recorded as `worker_lost` in `stage_runs`, separately from `failed`.
- **Retries (FR-16 AC1, FR-18):** Temporal activity retries are off for `RunStage` (`MaximumAttempts: 1`). The core counts attempts, because each attempt must be a new agent session and an owner resume resets the count (FR-10 AC8). Adapter activities use Temporal retries with backoff, because they are idempotent (§9).
- **Stopping a running handler** (item closed, change closed or merged by hand): the workflow cancels the activity. Every other owner action lets it finish (§6 of the product spec).
- **Daily limit reset (FR-16 AC4):** `ProjectWorkflow` sets a timer for the next 00:00 in the profile's `timezone`, resets the count, signals every item in `waiting_limit`, and notifies the owner.

## 7. Stage execution

### 7.1 Handler contract (summary)

A handler is a command in the profile:

```toml
[stages.implement]
command      = ["agent-loop-claude", "implement", "--skill", "work-issue"]
contract     = "1.0"
capabilities = ["agent"]
runner       = "local"          # "docker" from v0.2
time_limit   = "2h"
```

The handler's stdout is only a log and is never parsed (INTEGRATION-SPEC §3.1). Its result goes to `AGENT_LOOP_RESULT`. A command such as `claude -p … --output-format json` on its own therefore always ends as `failed`: it needs a wrapper, such as the starter handlers, that writes `result.json`.

The runner gives the handler:

- `AGENT_LOOP_INPUT`: the path to `input.json`, with `contract_version`, stage, review kind, references to the item, change and plan, the workspace path and the identity it acts as. It holds **references only**, never tracker text and never a session reference (NFR-8 AC1, §8.5). The handler reads the tracker itself, as data.
- `AGENT_LOOP_RESULT`: the path where it writes `result.json`, with one result from FR-18. The result carries the content the loop needs (change title and body, review findings and rulings, replies, plan proposal), and the worker posts it (T21).
- Credentials for its identity only (§7.3).

A missing, malformed or disallowed result, or an unsupported `contract_version`, is `failed` (FR-18 AC3). The full contract is in `INTEGRATION-SPEC.md` §3–§5, and the schemas live in `contract/v1/`.

### 7.2 Runners and sandboxing

| Runner | Release | Isolation | Egress |
|---|---|---|---|
| `local` | v0.1 | Fresh git worktree from a cached clone. The process runs in its own process group. Claude handlers run inside Claude Code's OS sandbox, limited to the worktree | The Claude sandbox's domain allowlist, generated from the profile |
| `docker` | v0.2 | A container per run from the project image (T10). Only the worktree and the input and result directory are mounted. Agents may use skip-permissions only here | A container network that allows only the profile's allowlist |
| `remote` | later | A microVM or cloud sandbox | Provider controls |

The v0.1 sandbox only covers Claude-based handlers. The sandbox is required (owner decision, 2026-11-15): `project validate` fails, and the worker refuses to start the stage, when an agent stage uses `runner = "local"` with a handler that the Claude sandbox doesn't wrap. A profile can opt out for a stage with `runner = "local-unsandboxed"`. Such a handler runs with the owner's full user access: it can read the owner's files, reach any network address, and open the local API socket (§11). `validate` then warns, in those words, on every run.

The `local` runner generates the sandbox settings for each run:

- **Writes:** the worktree, the run's `AGENT_LOOP_OUT` and result path, and a per-run temp directory. Nothing else.
- **Reads denied:** agent-loop's config and state directory (`~/.config/agent-loop`, which holds the API socket and any env/file secrets), the projection database, the server's Temporal data, `~/.ssh`, `~/.aws`, `~/.config/gh`, `~/.netrc`, `~/.docker`, `~/.kube`, the git credential stores, and macOS Keychain files (`~/Library/Keychains`). The Keychain backend (T12) also gives each item an access list that names only the agent-loop binary, so another process gets a prompt, not the key. macOS names an application in that list by its designated requirement. On macOS, agent-loop is signed with a stable self-signed code-signing certificate (`agent-loop code signing`, created in the owner's login keychain) and the fixed identifier `dev.agent-loop`. Its requirement is then that identifier and that certificate, so every build signed with it, including rebuilds and upgrades, reads the items with no prompt after the first approval (owner decision on #95, 2026-11-25; not a Developer ID). `agent-loop sign` signs the binary and creates the certificate if needed. Owners run it after every source build, and after `brew install` and every `brew upgrade`. The Homebrew formula can't run it, because Homebrew's `post_install` sandbox can't reach the login keychain, so the formula's caveat tells the owner to run `agent-loop sign` (owner decision on #124, 2026-11-25). An upgraded build that isn't signed yet doesn't match the items' requirement, so `server start` and `worker` refuse it as below, naming `agent-loop sign` as the fix. An ad-hoc signed build would be a new application at every rebuild. `server start` and `worker` stop, naming the items, when an item trusts a different build, instead of waiting at a prompt, and warn about an ad-hoc build. They also stop on a locked keychain, saying how to unlock it; `init` goes on and lets macOS ask the owner to unlock it. `init --check` reports such items, and `init` re-trusts them: one approval per item, then the item is rewritten for the running binary. An access list that allows any application is never used.
- **Network:** only the egress allowlist. Loopback and the local network are denied, so a handler can't reach the server's gateway or other local services.
- **Certificate checks (macOS):** the sandbox allows one system service, the certificate trust service (`com.apple.trustd.agent`), so Go tools such as `gh` and `go mod download` can verify HTTPS certificates. That service runs outside the sandbox and may fetch intermediate certificates and revocation data from URLs named in a certificate, which are not checked against the egress allowlist. The owner accepted this narrow path (#99, PR #111); no other system service is allowed. The setting (`sandbox.network.allowMachLookup`) is written on every platform, so the settings file has one shape; the Linux sandbox ignores it.
- **Tool caches and config:** the toolchains' default cache and config directories (such as `~/Library/Caches/go-build` and `~/.config/gh`) are outside the writable paths, or denied. So the runner sets a fixed default list to directories in the run's temp directory (`$TMPDIR`): `GOCACHE` (`go-build`), `GOMODCACHE` (`go-mod`), `XDG_CACHE_HOME` (`cache`), `npm_config_cache` (`npm`), `PIP_CACHE_DIR` (`pip`), `CARGO_HOME` (`cargo`) and `GH_CONFIG_DIR` (`gh`) (owner decision, 2026-11-25, #97). The profile's `caches` (§13) names more variables, and each points at `$TMPDIR/caches/<NAME>`. The run's extra variables (below) win over both: a value the run's own env sets, or the worker's value of a variable passed on. A `caches` name that is reserved (below), a secret, one the handler inherits from the worker (such as `PATH` or `HOME`) or one already in the default list is refused by `project validate` and by the runner, since the runner would ignore it or, for an inherited variable the worker lacks, point it into the temp directory. `CARGO_HOME` also holds Cargo's config and installed binaries, so `~/.cargo/config.toml` doesn't apply in a run. `gh` reads its config directory even with `GH_TOKEN` set, so without `GH_CONFIG_DIR` a handler couldn't read its own item. Caches are per run: they start empty on every run and are removed with the run's directory (the runner first makes the Go module cache's read-only directories writable). No cache is shared between runs in v0.1, so one run can't poison the next run's build; builds are slower, but runs stay isolated.
- **Git identity in coder runs:** implement, address and mint runs set `GIT_AUTHOR_NAME`, `GIT_AUTHOR_EMAIL`, `GIT_COMMITTER_NAME` and `GIT_COMMITTER_EMAIL` to the coder identity (as for the worker's own base merges, INTEGRATION-SPEC §5.3), `GIT_CONFIG_GLOBAL` to an empty file `gitconfig` in the run directory (removed with it), and `GIT_CONFIG_NOSYSTEM=1`. So the owner's global and system git config, with its identity, signing, hooks and aliases, doesn't reach the run. Review runs keep the inherited git config.
- **Reserved variables:** these variables are reserved for the runner on every run: `AGENT_LOOP_INPUT`, `AGENT_LOOP_RESULT`, `AGENT_LOOP_OUT`, `AGENT_LOOP_CONTRACT`, `AGENT_LOOP_CLAUDE_SETTINGS`, `GH_TOKEN`, `TMPDIR`, `GIT_TERMINAL_PROMPT` and the six git variables above. Only the runner sets them, and only where this section says: the six git variables only in coder runs, and `GH_TOKEN` only when the run is given a read token. Extra variables given to a run (the run's own env, or the runner's list of worker variables to pass on) may not name one of them, nor an env/file secret (`AGENT_LOOP_SECRET_*`): the run is refused. So no setting can make agent commits look like someone else's.
- **Default egress allowlist (T9):** the Anthropic API (`api.anthropic.com`), the code host (on GitHub: `github.com`, `api.github.com`, `codeload.github.com`, `objects.githubusercontent.com`), and common package registries (`registry.npmjs.org`, `proxy.golang.org`, `sum.golang.org`, `pypi.org`, `files.pythonhosted.org`, `crates.io`, `static.crates.io`). The profile's `egress.allow` adds to it. `egress.replace = true` replaces it.

A host with Docker advertises the capability `docker`. A stage with `runner = "docker"` requires it automatically.

### 7.3 Credentials in a run

- The worker gets a short-lived token from `SecretStore` for the stage's identity only: coder for implement, address and mint, and reviewer for review. Handlers get a narrowed token: read access for every stage, plus push for the coder stages. The review handler gets no push token at all (FR-13 AC2). Write access to issues and pull requests stays with the worker (T21, INTEGRATION-SPEC §3.4, §9.1).
- The loop identity's credentials never reach a runner (§8.8). Only adapter activities in the worker use them.
- Git gets the token through a credential helper served by the worker over a per-run socket in the run's temp directory (the only agent-loop socket the sandbox allows), not an environment variable, so it doesn't show in process listings, the environment, or handler output. This is not a secret from the handler: any process in the run can ask the helper (`git credential fill`) and use the token directly. The helper keeps the token out of logs and lets the worker issue a fresh token when the old one is near its one-hour expiry. What the token can do is limited by its scope and by the code host's rulesets, not by the helper.
- The coder handler token is limited to the project's repository with `contents: write`, `metadata: read` and read access to issues and pull requests (INTEGRATION-SPEC §9.1). It can't open changes or reply: the worker does that with a separate token (T21, I2). Because `contents: write` on GitHub also allows pushing to any branch, creating tags and releases, and merging pull requests, rulesets limit it: the loop's branch prefix accepts pushes only from the coder and owners, and the base branch and tags refuse it (INTEGRATION-SPEC §9.8, I10). It has no `workflows` permission, so it can't change `.github/workflows/` (T18, T19).
- The **coder worker token** (never given to a handler) has `pull requests: write`, `issues: read` and `contents: write`. It opens changes, posts replies, and pushes the worker's own conflict-free base merges (§8.1 cause 3, D41, INTEGRATION-SPEC §5.3, §9.1). It is issued for each of those activities and revoked when the activity ends. The loop identity is never used to push, even for a base merge (§8.8).
- At the end of every run, the worker revokes every token it issued for that run (`SecretStore.Revoke`). So a process the handler left behind can't keep pushing, and a head move seen after the run can't come from that run's token (INTEGRATION-SPEC §9.8, I11).
- Every log line and stored handler output passes through a redactor that knows the run's tokens (§8.7, FR-13 AC3).

## 8. Ordered rules

These are the rule orders that D17 and D27 moved here from the product spec. Each rule's outcome is in the product spec. Only the order is defined here.

### 8.1 Merge-condition failures (FR-7 AC6, D17)

The merger checks the conditions right before merging. When one fails, it acts on the **first** cause in this list that holds. Each run handles one cause, then the conditions are checked again from the top. It never force-pushes.

1. **Untrusted commit** (FR-13 AC5): escalate.
2. **Head moved after the review** (FR-7 AC1, AC5): back to `in_review`. The kind follows FR-6 AC6 (`owner_push` after an owner push, otherwise `round`). Not escalated.
3. **Head lacks the base tip**, with or without conflicts (FR-7 AC4, AC8): go to `addressing` for a base update only (FR-5 AC6, D41), then a `base_update` review. Without conflicts, the worker merges the base itself as the coder identity, in a deterministic activity with no agent run (INTEGRATION-SPEC §5.3). With conflicts, an address run in `base_update` mode resolves them. Open manual comments wait for the next check. If `max_base_updates` is used up, escalate instead (FR-6 AC7). The daily-limit check (§8.2) runs before the address run and before the review, but not before a conflict-free merge, which is not an agent run.

   **Agent-run budget.** A conflict-free base update costs one agent run (the `base_update` review), and one with conflicts costs two. A typical item uses implement (1), one to three counted reviews (1–3), up to two address runs (0–2) and one conflict-free base update (1): about 3 to 7 agent runs. So the default daily limit of 20 (D35) covers about 3 to 6 items a day, against about 2 to 5 when every base update was an agent run.
4. **A manual review comment isn't cleared** (FR-7 AC9): go to `addressing` to answer it, then a `manual_reply` review (FR-7 AC10). If the only manual comments that aren't cleared are empty "changes requested" reviews, which only a human's dismissal clears, escalate naming them instead (D43, INTEGRATION-SPEC §9.6).
5. **A gate failed, branch protection refused, or the merge call failed**: escalate. A merge call refused as not mergeable is first re-checked from the top: the merger reads the change again, and escalates for cause 5 only if causes 1 to 4 still don't hold (INTEGRATION-SPEC §8.3 `MergeOutcome`). So a base that moved between the check and the call goes to cause 3, not to an escalation.
6. **A required gate has no result yet**: stay in `merging` and check again each poll interval, until the merge stage's time limit escalates the item (FR-16 AC1). The poll interval (default 60 s) is shorter than the supervisor interval, so this meets FR-7 AC6's "each supervisor interval" with room to spare.

Why this order. It is the order the owner confirmed as product behaviour (D17), carried over from the product spec. Trust comes first because nothing else may run on an untrusted change. A moved head comes before everything that depends on the reviewed head. The base update comes before manual comments so that the coder answers them on code that already contains the base, and the reviewer's `manual_reply` ruling covers the head that will merge. The other order costs the same number of runs but would rule on replies made against out-of-date code.

### 8.2 Next stage after a wait (FR-10 AC2, D27)

Whenever an item leaves `blocked`, `escalated`, `paused` or `waiting_limit`, the next stage is the **first** rule that holds:

1. Resumed after a round-limit escalation with a blocking finding (FR-6 AC3): an address run (FR-10 AC5), even if the head moved.
2. The item has no change: implement.
3. An address run replied to manual review comments and no review has ruled on those replies yet: a review of kind `manual_reply` (FR-4 AC8, FR-6 AC6).
4. The change's head has no review yet: a review. Its kind follows FR-6 AC6.
5. The latest review left a blocking finding: an address run.
6. Otherwise: merge, which checks §8.1 from the top.

The chosen stage then passes two checks, in this order:

- If it is a counted round (kind `round`) and the round allowance is used up, the item is escalated naming the cause (FR-6 AC8). Resuming it is a round-limit resume (FR-10 AC5). Open manual comments don't suspend this (D31 as amended).
- If it is an agent stage and the daily limit is reached, the item goes to `waiting_limit` (FR-16 AC3).

The same two checks run before every agent stage, not only after a wait.

### 8.3 Pre-stage trust check

Pushes by anyone other than the coder identity and the owner accounts are refused by the code host's branch ruleset (T19), which `project validate` checks. Before implement, address or merge, the loop still checks the change (FR-13 AC5, AC6):

1. A change from a fork is untrusted.
2. The push rule must still be in force and unchanged since the branch was created. Otherwise the commits since the last trusted head are untrusted, unless they are in the item's approved commits.

`CodeHost.Trust` makes these checks (INTEGRATION-SPEC §9.8). An untrusted change escalates the item, naming the cause and the commits. The verified author isn't used on GitHub, because agent commits made with local git aren't signed. A code host without an equivalent push rule must check pushers or verified authors in its adapter instead, and declares which it uses through `Capabilities()`. On GitHub the push rule is the push record (product spec D40).

Who pushed a head (coder or owner) decides the review kind (FR-6 AC6) and is part of trust. On GitHub it is worked out from the loop's own run records, as INTEGRATION-SPEC §9.8 (I11) says.

## 9. Idempotency and durability

- **Every write has an idempotency key** derived from the workflow ID, the claim number (§6.1) and the decision that made it, for example `item/p/42#1:escalate:7`. Adapters check before writing:
  - Comments and created items carry a hidden record marker that holds the key (INTEGRATION-SPEC §6.1: on GitHub, `<!-- agent-loop:v1 {json} -->`). A retry looks for the marker in the fixed place for that record type (INTEGRATION-SPEC §6.1) and returns the existing object.
  - Markers are set-to-value, so applying one twice is a no-op.
  - Merge is pinned to a commit (FR-7 AC5). A retry finds the change already merged at that commit and succeeds.
- **Materialise** records the plan key and task key in each created item (FR-17 AC3). It validates the whole task list before the first write (FR-17 AC6).
- **Follow-ups** use the key `followup:<change>` (FR-8 AC3).
- **Stage results** are recorded in workflow history the moment the activity returns. A crash after that never reruns the stage (NFR-1). A crash during a run reruns it in a new session (§6.5), and the implement stage first adopts an existing change (FR-3 AC3).
- **Review rounds** are counted from the structured review results in the code host (FR-6 AC5), and checked against the workflow's own count on reconcile. On a mismatch, the code host wins and the decision log records it.

## 10. Read model and decision log

The projection is written by a `RecordDecision` activity after each `Decide` call.

**Who owns which state.** Each piece of state has one owner. The others hold copies.

| State | Owner (source of truth) | Copies | If the owner's copy is lost |
|---|---|---|---|
| Claim, labels (markers), item and change open or closed, dependencies | Tracker and code host | Workflow state | Never lost: the tracker is durable |
| Rounds used, findings and their statuses, manual rulings | Review records in the code host (FR-6 AC5, INTEGRATION-SPEC §6.3) | Workflow state | Never lost. On a mismatch the code host wins (§9) |
| Approved commits (FR-13 AC6), escalation reason, claim number | `approved_commits` and `status` records on the item | Workflow state | Never lost |
| Current lifecycle state, current stage, attempt count, stage deadline, pending timers, round allowance added by resumes | Running workflow (Temporal) | Projection `items` | Rebuilt by the full reconcile from the tracker records above. Attempt counts and deadlines restart, as after an owner resume (FR-10 AC8) |
| Cursors, WIP, daily count | `ProjectWorkflow` (Temporal) | none | Cursors: a full reconcile replaces them. Daily count: rebuilt from the day's `stage_runs` rows |
| Decision log, stage-run history, run logs | Projection database and log files | none | Lost. Temporal history isn't a backup: closed workflows are deleted after the namespace retention period, and continue-as-new drops earlier history. The owner backs up the projection database like any other local data |

The round allowance added by resumes is also recorded in each `resume` status record (`rounds_added`), so a lost workflow can rebuild it from the tracker.

| Table | Contents |
|---|---|
| `items` | project, item key, state, round, rounds allowed, blocking count, host, entered-state time, next action, wait reason |
| `decisions` | item, time, event, tracker state seen, stage result reference, actions (NFR-5) |
| `stage_runs` | item, stage, kind, attempt, host, start, end, result, redacted log path (FR-11 AC2) |
| `hosts` | name, capabilities, last seen, last check result |
| `plans` | project, plan key, state, proposal reference, created items |
| `rulesets` | project, ruleset ID, `updated_at`, bypass list (JSON): the owner's last read of the project's rulesets (INTEGRATION-SPEC §9.8 step 8). Each owner read replaces the project's rows |

The `rulesets` table was added without a schema version change (it is additive, so the version stays 1). If the projection is lost, every ruleset whose bypass list the loop app can't see is unverified until the owner reads them again. `project validate` opens the projection to read the record, and reports `projection_unavailable` when it can't.

The board's HOST (FR-11 AC1) is the worker's OS host name, the name the `hosts` table uses. The worker records it on the stage run when the `RunStage` activity starts; a failure to record it doesn't fail the run. The board shows the host of the item's stage run that hasn't ended, and `-` when no stage runs.

SQLite in v0.1 (the path is the deployment config's `[projection] path`, §13). The same SQL runs on Postgres from v0.3.

## 11. Local API and CLI

The server serves gRPC on a Unix socket, `~/.config/agent-loop/api.sock`, in a directory with mode `0700`, and the socket itself is `0600`. With no home directory, `server init` writes the socket as an absolute path to `api.sock` beside the config file instead (§13), and with the env-file backend `[secrets] path` as `secrets.env` there too. It refuses that path, naming it and writing no config, when it doesn't fit a Unix socket address with its terminating NUL: at most 103 bytes on macOS, 107 on Linux (#139). Handler sandboxes deny that directory (§7.2), so a sandboxed agent can't open it, even though it runs as the same user. The HTTP/JSON gateway is off by default. When the owner turns it on, it listens on `127.0.0.1` and needs a bearer token from the deployment config. The CLI is a thin client.

Some commands don't need the server. `init`, `project validate` and `run --dry-run` run in the CLI process with the deployment config, the profile and the `SecretStore`, so they work before `agent-loop server start` (getting-started guide §3–§4).

| Command | API call | Effect |
|---|---|---|
| `agent-loop board [project]` | `Board` | Reads the projection and live workflow queries (FR-11 AC1, AC4). While a project workflow runs but hasn't registered its state query yet, the project line reads "state unknown: project X is starting: try again in a moment" |
| `agent-loop item pause\|resume\|abort <item>` | `ItemCommand` | Signal to the `ItemWorkflow` |
| `agent-loop project pause\|resume <project>` | `ProjectCommand` | Signal to the `ProjectWorkflow` |
| `agent-loop pause\|resume` | `DeploymentCommand` | Signal to the `DeploymentWorkflow` |
| `agent-loop mint <project>` | `Mint` | Starts the `MintWorkflow`, or says why it can't (FR-9 AC1) |
| `agent-loop plan abandon <plan>` | `PlanAbandon` | FR-17 AC9 |
| `agent-loop server init` | local | Writes the deployment config and starts Temporal (T2). Doesn't start the server. It creates the projection database's directory (mode `0700`), checks that it can create a file there (a temporary `.probe-*` file, removed at once), and prints `Projection database: <path>`. For a new config it picks the path as §13 says. With an existing config whose projection directory fails the check, it exits 1 and names `[projection] path` in that file; it never rewrites an existing config |
| `agent-loop server start` | local | Starts the server, which loads every project registered in the deployment config |
| `agent-loop worker --capabilities <list>` | local | Starts a worker (§3, §6.4) |
| `agent-loop init` | local | FR-1 AC5. Runs on the owner's machine with the owner's login, asking before each tracker change (INTEGRATION-SPEC §12.2). Registers the project in the deployment config, then runs `project validate` locally |
| `agent-loop run --dry-run <project>` | local, plus `Board` if the server runs | FR-1 AC6. One supervisor pass through write-recording adapters: nothing is written and no handler starts. Claimed items' states come from the server. When it isn't running, the output says so and lists only the candidates. The candidates are the open items with the ready marker (`WorkTracker.Candidates`), with or without the server, and the heading says so: "Candidates (open items with the ready marker), in pick order:". When the board reports that the project is starting or not running, the Server line gives that reason on its own ("Server: project X is starting: try again in a moment, so ..."); a failed dial or call reads "the server isn't running ..." |
| `agent-loop project validate [--owner-login [--oauth-client-id <id>]] <project>` | local | FR-1 AC2, FR-13 AC1, AC4, NFR-4 AC1. `--owner-login` also reads and records the rulesets' bypass lists with the owner's login (INTEGRATION-SPEC §9.8 step 8) |
| `agent-loop host check` | local | FR-14 AC4 (v0.3). `help` marks it "(v0.3, FR-14 AC4)", as it marks `mint` and `plan` with their release |
| `agent-loop sign [--binary <path>]` | local | macOS only. Signs the binary (default: the running one), with symlinks resolved so the file a link points to is signed and the link kept, with the self-signed certificate and the identifier `dev.agent-loop`, creating the certificate if needed (§7.2) |
| `agent-loop log <item>` | `ItemLog` | FR-11 AC2 |

API commands act as the owner. In v0.1 the API is reachable only through the Unix socket, or the token-protected gateway if the owner turns it on. Remote access (v0.3) needs authentication, and is an open question (Q5).

## 12. Notifications

`Notify` actions go through the `Notifier` port to every channel in the deployment config. In v0.1: macOS desktop (through `osascript` or `terminal-notifier`), and a mention of the owner in the escalation comment.

For a remote server (v0.3, FR-11 AC5), the owner's machine runs `agent-loop notify-agent`, which polls the API over an outbound connection and shows desktop notifications. No inbound access is needed.

## 13. Configuration

- **Deployment config** (`~/.config/agent-loop/deployment.toml`): Temporal address and namespace, projection DB, API socket and optional gateway, notifiers, secret-store backend, deployment limits, and the registered projects (`[projects.<name>] profile = "<path to .agent-loop.toml>"`). `agent-loop init` adds the entry (FR-1 AC1, AC5). The server reads each profile from that path when it starts.
- **Projection path** (`[projection] path`): the default is `~/.local/state/agent-loop/projection.db`. When `server init` writes a new config it starts from `$XDG_STATE_HOME/agent-loop/projection.db` instead if `XDG_STATE_HOME` is set to an absolute path (an empty or relative value is ignored), and writes that path to the config in full; it doesn't also try `~/.local/state` then. When that directory can't be created or written to, it uses `~/Library/Application Support/agent-loop/projection.db` on macOS, then `projection.db` in the deployment config's directory (`~/.config/agent-loop` by default, which server init has just created and the sandbox already denies, §7.2) on any platform, and says which places it skipped (§11).
- **Project profile** (`.agent-loop.toml` in the repository): the tracker and code-host adapters and their settings, the marker mapping, identities and owner accounts, the base branch, merge method, limits, stages (command, capabilities, runner, time limit), egress additions, cache variables, image (v0.2), source of truth (v0.2) and timezone. The full key list is below. INTEGRATION-SPEC §13 repeats the contract-relevant keys.

```toml
[tracker]
adapter = "github"            # later: "jira"
repo    = "owner/agentMaster"

[code_host]
adapter = "github"
repo    = "owner/agentMaster"

[markers]                     # marker → tracker representation
ready       = { label = "ready" }
in_progress = { label = "in-progress" }
needs_owner = { label = "needs-owner" }
# jira example: ready = { status = "Selected for Development" }

[identities]
coder    = "secret://github-app/coder"
reviewer = "secret://github-app/reviewer"
loop     = "secret://github-app/loop"
owners   = [{ login = "mayur-github-login", id = 1234567 }]   # matched by id (INTEGRATION-SPEC §13)
```

**Profile keys.** Every key the product spec makes configurable, with its default. `project validate` rejects unknown keys and values outside the range. Defaults and ranges come from the product spec where it gives them (the Product column). The other ranges are proposals of this spec.

| Key | Default | Range or values | Product |
|---|---|---|---|
| `base_branch` | the repository's default branch | | FR-1 AC4 |
| `branch_prefix`, `plans_dir` | `agent/`, `plans/` | | INTEGRATION-SPEC §3.3, §7 |
| `types` | `["feature", "bug"]` | type markers that exist | FR-2 AC1 |
| `priority` | `["type:bug", "type:feature", "oldest"]` | an ordered list of type markers, then `oldest` or `newest` | FR-2 AC2 |
| `wip` | `2` | 1 to 20 | FR-2 AC3 |
| `max_rounds` | `3` | 1 to 10 | FR-6 AC1 |
| `blocking_threshold` | `4` | 1 to 5 | §5, FR-6 AC2 |
| `resume_rounds` | `1` | 1 to 5 | FR-10 AC5 |
| `max_base_updates` | `3` | 1 to 10 | FR-6 AC7 |
| `merge_method` | `squash` | `squash`, `merge`, `rebase` | FR-7 AC7 |
| `follow_up.type` | `feature` | a configured type | FR-8 AC5 |
| `follow_up.ready` | `false` | `true`, `false` | FR-8 AC2 |
| `limits.daily_agent_runs` | `20` | 1 to 1000. `0` and `off` are rejected | FR-16 AC3, D35 |
| `limits.daily_spend` | none | an amount in the currency handlers report (INTEGRATION-SPEC §5.1 `usage`) | FR-16 AC3 |
| `limits.agent_runs` | `2` | 1 to the deployment limit (default 4) | FR-16 AC2 (v0.2) |
| `timezone` | the server's local time zone | an IANA name | FR-16 AC4 |
| `intervals.supervisor`, `intervals.poll` | `10m`, `60s` | `1m`–`60m`, `15s`–`10m`, poll not longer than supervisor | FR-12 AC1, §6.2 |
| `stages.<stage>.*` | INTEGRATION-SPEC §13 | | FR-16 AC1 |
| `source_of_truth` | none (minter disabled) | an ordered list of paths | FR-9 AC2 (v0.2) |
| `caches` | `[]` | a list of unique environment variable names (`^[A-Za-z_][A-Za-z0-9_]*$`), not one the runner sets itself, a secret, an inherited variable (`PATH`, `HOME`, ...) or a default cache | §7.2 |
| `markers`, `identities`, `gates`, `manual_comment_bots`, `egress` | INTEGRATION-SPEC §13 | | |

Both files are checked against JSON Schemas in `internal/profile`. `project validate` also calls each adapter's `Validate` and `Capabilities` methods, so adapter-specific settings are checked by the adapter that owns them.

## 14. Testing

| Level | What | Where it runs |
|---|---|---|
| Core unit tests | Table tests over `core.Decide` for every lifecycle edge, the §8 orders, and every FR-12 AC4 row | Every commit, fast |
| Workflow tests | Temporal test environment with fake adapters and a fake runner. Includes replay tests against recorded histories | Every commit |
| Scenario tests | Sections 11.1–11.3 scripted against the fake adapters, including worker kills (NFR-1 AC1) and blocked inbound network (NFR-4 AC2) | Every commit |
| Conformance suite | One suite of behaviours every `WorkTracker` and `CodeHost` must show: compare-and-set claim, idempotent writes, trust checks (INTEGRATION-SPEC §11), manual-comment clearing, pinned merge. Runs against `fake`, and against `github` on a sandbox repository | Fake: every commit. GitHub: nightly and before a release |
| Platform | The suites on macOS and Linux runners (NFR-3 AC1) | CI |

The fake adapters are scriptable: a test can make "the owner" or "another human" push, label, comment, resolve and merge, so every manual-action row can be tested.

## 15. Release mapping

| Release | Technical scope |
|---|---|
| v0.1 | Core, `ItemWorkflow`, `ProjectWorkflow` (single project), `DeploymentWorkflow` (deployment pause only, FR-10 AC3), GitHub and fake adapters, the GitHub `init` steps, `LocalRunner` with the Claude sandbox, SQLite projection, Keychain and env secrets, desktop and tracker notifiers, CLI (including `init` and `run --dry-run`, D39) and local API, conformance suite. The first release of the starter handlers, `agent-loop-claude` (`agent-loop-handlers`, D38), ships with v0.1 (owner decision X7, 2026-11-15): §11.1 and the getting-started guide use them |
| v0.2 | `MintWorkflow`, `MaterialiseWorkflow`, capability task queues across several hosts, agent-run slots (including deployment-wide slots in `DeploymentWorkflow`), `DockerRunner`, base image |
| v0.3 | Several projects on one deployment, Postgres projection, remote server, `notify-agent`, host bootstrap, API authentication |
| Later | Jira and GitLab adapters (when asked for, D3), Slack and push notifiers, remote runner, Vault or 1Password secrets |

## 16. Open questions

Resolved (2026-11-14, owner):

- **Q1 Who pushes code → the handler (T18).** The handler pushes as the coder identity. The worker, not the handler, opens the change and posts replies, also as the coder identity (T21, INTEGRATION-SPEC I1, I2). Option B (the worker pushes) was dropped. The risk is a prompt-injected agent misusing its push credential, so the credential is limited: a short-lived token for one repository with `contents: write`, given through the credential helper (§7.3) and revoked at the end of the run. A branch ruleset lets only the coder identity and owners push to the loop's branch prefix (default `agent/`), and base-branch and tag rulesets refuse the coder identity (INTEGRATION-SPEC I10), so it never pushes to or merges into the base branch.
- **Q2 Untrusted commits → prevented by a branch ruleset, not detected (T19).** The check exists because implement and address run the branch's code (builds, tests) on the host before any review, and because an outside commit could carry text aimed at the LLM reviewer. FR-13 AC5 is met by construction: a GitHub ruleset on the loop's branch prefix lets only the coder identity and the owner accounts push, so no one else can add a commit. `project validate` fails if the ruleset is missing or allows other actors. The pre-stage check (§8.3) still rejects changes from forks, and checks that the ruleset is still in force. Pusher history is not read. (Amended by INTEGRATION-SPEC I6: the verified-author fallback was dropped on GitHub, because local agent commits aren't signed.) To confirm: rulesets on private repositories need a GitHub plan that supports them. If the plan doesn't, `project validate` fails and names the gap.
- **Q3 Structured review results → a PR review by the reviewer identity (T20).** The worker posts, as the reviewer identity, a PR review whose body holds the handler's summary, a status table, and a hidden record marker (the reviewed commit, the kind, and each finding with its ID, score and status). Findings with a file get an inline or file-level thread, and findings with no file are sections of the review text (INTEGRATION-SPEC §6.3, I8). The loop reads reviews through the normal API, and counts only reviews authored by the reviewer identity, so text anyone else posts can never act as a review result. Rounds are counted from these after a restart (FR-6 AC5). The JSON schema is in `INTEGRATION-SPEC.md`.

Still open:

- **Q4 Polling budget.** GitHub's rate limit is per app installation. Several items and a 60 s poll may need GraphQL batching. Conditional requests (`If-None-Match`) save REST calls only: GraphQL has no conditional requests and is charged in points per query, so the pull-request detail and gate queries (INTEGRATION-SPEC §9.4) are the cost to measure. To be measured in v0.1.
- **Q5 API authentication** for the remote server (v0.3): mTLS, or a token in the deployment config.
- **Q6 Jira item ↔ change link.** A branch naming rule, a change body field, or Jira's development panel. Needed before the Jira adapter.
- **Q7 Owner decisions from the 2026-11-15 review → all applied.** R1 (T2 kept), X7 (§15), the sandbox requirement (§7.2), and the product decisions D40 (X1, T19, §8.3), D41 (R5, §7.3, §8.1), D42 (X6, §6.5), D43 (X8, §8.1 cause 4) and D44 (N8, INTEGRATION-SPEC §3.3).
