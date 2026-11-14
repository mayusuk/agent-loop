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
| T2 | Orchestration | Temporal, self-hosted with Docker Compose (Temporal + Postgres) in v0.1 | Durable workflows, timers and retries (NFR-1). Moving to a remote server or Temporal Cloud is a change to the connection config (NFR-3 AC2) |
| T3 | Tracker vs code host | Two separate ports: `WorkTracker` (items) and `CodeHost` (changes, reviews, gates, merge) | Jira holds issues but not code, so "Jira items + GitHub changes" has to work without core changes |
| T4 | Tracker and code-host adapters | Go interfaces, compiled into the binary, selected in the profile. GitHub first, Jira and GitLab later | They are few, written by us, and need tight correctness (claims, idempotency) |
| T5 | Loop state in the tracker | Abstract **markers** (§5.1 labels become markers). Each adapter maps a marker to a label, a status or a field, as the profile says | GitHub uses labels. Jira teams expect workflow statuses |
| T6 | Agent stage handlers | A subprocess with a JSON contract: the core starts a command from the profile, passes `input.json`, reads `result.json` | Any language and any agent. New agents need no Go code (D34) |
| T7 | First handlers (outside the core) | Claude Code headless (`claude -p` with the adopter's skills) and a Claude Agent SDK handler | The v0.1 adopter's skills already exist |
| T8 | Sandboxing | A `Runner` port. v0.1: `LocalRunner`, a git worktree per run inside Claude Code's OS sandbox (Seatbelt on macOS, bubblewrap on Linux). v0.2: `DockerRunner`, a container per run | Unattended agents must not act with the owner's full access |
| T9 | Network egress | An allowlist per project (defaults: Anthropic API, the code host, package registries, plus profile additions). Enforced by the Claude sandbox in v0.1, and by a container network in v0.2 | Limits what a prompt-injected agent can send out (§8.5) |
| T10 | Container images (v0.2) | agent-loop publishes a base image (git, Node, the agent CLIs). Each project extends it with its toolchain and names the image in the profile | Projects own their toolchains (§9) |
| T11 | Read model and decision log | A projection database: SQLite in v0.1, Postgres from v0.3 (remote server). Temporal stays the source of truth | The board (FR-11) needs fast cross-item queries that Temporal visibility doesn't give |
| T12 | Secrets | A `SecretStore` port: macOS Keychain and env/file backends in v0.1. 1Password or Vault later. Tokens are short-lived and issued per run, per identity | FR-13 AC3, §8.7 |
| T13 | Owner interface | The server exposes a local API (gRPC, with HTTP/JSON via a gateway) on localhost. The `agent-loop` CLI is one client. Owner actions through tracker markers keep working | A web board or chat bot can be added later as another client |
| T14 | Notifications | A `Notifier` port: macOS desktop and tracker mention in v0.1. Slack and push later | FR-11 AC3, AC5 |
| T15 | Change detection | Polling with incremental queries (GitHub: ETags and `since`. Jira: JQL `updated >= cursor`). No webhooks by default | NFR-4: no inbound access to hosts |
| T16 | Testing | In-memory fake adapters plus a shared **conformance suite** that every adapter must pass (fake, GitHub, later Jira) | Scenario 11.1 runs fast and without GitHub. New adapters prove their behaviour |
| T17 | Configuration format | TOML for the project profile and the deployment config, with a JSON Schema for validation | Matches the first adopter (`.wf.toml`), and humans edit it |
| T18 | Who pushes | The handler pushes as the coder identity, with a scoped short-lived token (Q1) | The handler already acts as the coder. Limits on the token make misuse small |
| T19 | Untrusted commits | Prevented by a branch ruleset that lets only the coder identity and owner accounts push to the loop's branches. Pusher history is not read (Q2) | Meets FR-13 AC5 by construction, with less code and no dependence on event retention |
| T20 | Review results | A PR review by the reviewer identity, with a fenced JSON block and inline finding comments. Only reviewer-authored reviews count (Q3) | Machine-readable, and can't be forged by other accounts |
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
                Project-supplied handlers: Claude Code headless,
                Claude Agent SDK, any other command          (* = later)
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

The sketches show the shape. The exact methods are in `INTEGRATION-SPEC.md`. Every method that writes takes an idempotency key (§9).

```go
type WorkTracker interface {
    ListCandidates(ctx, ProjectRef) ([]Item, error)
    Changes(ctx, ProjectRef, Cursor) ([]ItemEvent, Cursor, error)   // incremental (T15)
    Claim(ctx, ItemRef, expect Version, as Identity) error           // fails if the item changed (FR-2 AC5)
    SetMarker(ctx, ItemRef, Marker, on bool, key IdemKey) error      // marker → label/status/field mapping inside
    Comment(ctx, ItemRef, Body, key IdemKey) (CommentRef, error)
    CreateItem(ctx, NewItem, key IdemKey) (ItemRef, error)           // materialise, follow-ups
    Close(ctx, ItemRef, key IdemKey) error
    Dependencies(ctx, ItemRef) ([]Dependency, error)
    ActorIsOwner(ctx, Actor) bool
}

type CodeHost interface {
    FindChange(ctx, ItemRef) (*Change, error)                         // adoption (FR-3 AC3)
    Changes(ctx, RepoRef, Cursor) ([]ChangeEvent, Cursor, error)
    Head(ctx, ChangeRef) (Commit, error)
    Trust(ctx, ChangeRef) (TrustReport, error)                        // fork, push rule in force (FR-13 AC5, T19)
    Reviews(ctx, ChangeRef) ([]Review, error)                         // structured results + manual reviews
    ManualComments(ctx, ChangeRef) ([]ManualComment, error)
    Gates(ctx, ChangeRef, Commit) ([]GateResult, error)
    ContainsBase(ctx, ChangeRef) (bool, error)
    Merge(ctx, ChangeRef, pinned Commit, Method, key IdemKey) error   // FR-7 AC5
}

type Workspace interface {
    Prepare(ctx, RepoRef, Branch) (Dir, error)  // cached clone + fresh worktree per run
    Release(ctx, Dir) error
}

type Runner interface {
    Run(ctx, RunSpec) (RunOutcome, error)   // command, workspace, env, limits, egress policy
}

type SecretStore interface {
    Token(ctx, Identity, Scope) (Token, error)  // short-lived where the tracker allows
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
| `DeploymentWorkflow` | `deployment` | Forever (continue-as-new) | Deployment pause, deployment-wide agent-run slots (v0.2), host health view |
| `ProjectWorkflow` | `project/<name>` | Forever (continue-as-new) | Supervisor: polls, picks, claims, holds WIP, the daily counter, project agent-run slots and project pause. Routes events to items |
| `ItemWorkflow` | `item/<project>/<item-key>` | From claim to close or release | Runs one item's lifecycle by calling `core.Decide` |
| `MintWorkflow` | `mint/<project>` | One per mint run | FR-9. Its ID guarantees one minter per project (FR-9 AC6) |
| `MaterialiseWorkflow` | `plan/<project>/<plan-key>` | From approval to done or abandon | FR-17 |

The workflow ID is the item's lock. Temporal refuses a second running workflow with the same ID, so an item is never worked by two runs at once, even after a duplicate start (FR-2 AC4).

### 6.2 Supervisor loop

`ProjectWorkflow` runs a timer every `supervisor_interval` (default 10 minutes, FR-12 AC1), plus a shorter poll interval for changes (default 60 s, which meets NFR-6). Each pass:

1. A `PollChanges` activity calls `WorkTracker.Changes` and `CodeHost.Changes` from the stored cursors, and returns normalised events.
2. It signals each event to the matching `ItemWorkflow`, or handles it as an unclaimed-item event (FR-2 AC6, reopen, owner-opened change).
3. At the full supervisor interval, it reconciles: it compares the full tracker state with its running workflows, starts missing ones, and stops runs for items closed or merged by hand (FR-12 AC1).
4. It picks: while WIP and the daily limit allow, and the project isn't paused, it claims the next candidate in priority order (FR-2 AC2) with `Claim` (compare-and-set), then starts its `ItemWorkflow`.

Cursors, WIP and the daily count live in the workflow state and survive continue-as-new.

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

- **Time limit (FR-16 AC1, NFR-7 AC1):** a `RunStage` activity has `StartToCloseTimeout` = stage limit and heartbeats every 30 s. On timeout or cancellation, the runner kills the handler's whole process group (local) or the container (docker), and the workflow escalates.
- **Retries (FR-16 AC1, FR-18):** Temporal activity retries are off for `RunStage` (`MaximumAttempts: 1`). The core counts attempts, because each attempt must be a new agent session and an owner resume resets the count (FR-10 AC8). Adapter activities use Temporal retries with backoff, because they are idempotent (§9).
- **Stopping a running handler** (item closed, change closed or merged by hand): the workflow cancels the activity. Every other owner action lets it finish (§6 of the product spec).
- **Daily limit reset (FR-16 AC4):** `ProjectWorkflow` sets a timer for the next 00:00 in the profile's `timezone`, resets the count, signals every item in `waiting_limit`, and notifies the owner.

## 7. Stage execution

### 7.1 Handler contract (summary)

A handler is a command in the profile:

```toml
[stages.implement]
command      = ["claude", "-p", "/work-issue", "--output-format", "json"]
capabilities = ["agent"]
runner       = "local"          # "docker" from v0.2
time_limit   = "2h"
```

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

The v0.1 sandbox only covers Claude-based handlers. `project validate` warns when an agent stage uses `runner = "local"` with a handler that the Claude sandbox doesn't wrap.

A host with Docker advertises the capability `docker`. A stage with `runner = "docker"` requires it automatically.

### 7.3 Credentials in a run

- The worker gets a short-lived token from `SecretStore` for the stage's identity only: coder for implement, address and mint, and reviewer for review. Handlers get a narrowed token: read access for every stage, plus push for the coder stages. The review handler gets no push token at all (FR-13 AC2). Write access to issues and pull requests stays with the worker (T21, INTEGRATION-SPEC §3.4, §9.1).
- The loop identity's credentials never reach a runner (§8.8). Only adapter activities in the worker use them.
- Git gets the token through a credential helper served by the worker over a local socket, not an environment variable, so it doesn't show in process listings or handler output.
- The coder token is limited to the project's repository with `contents: write` (plus what opening changes and replying need), and the branch ruleset lets it push only to the loop's branch prefix (T18, T19).
- Every log line and stored handler output passes through a redactor that knows the run's tokens (§8.7, FR-13 AC3).

## 8. Ordered rules

These are the rule orders that D17 and D27 moved here from the product spec. Each rule's outcome is in the product spec. Only the order is defined here.

### 8.1 Merge-condition failures (FR-7 AC6, D17)

The merger checks the conditions right before merging. When one fails, it acts on the **first** cause in this list that holds. Each run handles one cause, then the conditions are checked again from the top. It never force-pushes.

1. **Untrusted commit** (FR-13 AC5): escalate.
2. **Head moved after the review** (FR-7 AC1, AC5): back to `in_review`. The kind follows FR-6 AC6 (`owner_push` after an owner push, otherwise `round`). Not escalated.
3. **Head lacks the base tip**, with or without conflicts (FR-7 AC4, AC8): go to `addressing` for a base update only (FR-5 AC6), then a `base_update` review. Open manual comments wait for the next check. If `max_base_updates` is used up, escalate instead (FR-6 AC7).
4. **A manual review comment isn't cleared** (FR-7 AC9): go to `addressing` to answer it, then a `manual_reply` review (FR-7 AC10).
5. **A gate failed, branch protection refused, or the merge call failed**: escalate.
6. **A required gate has no result yet**: stay in `merging` and check again each poll interval, until the merge stage's time limit escalates the item (FR-16 AC1).

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

`CodeHost.Trust` makes these checks (INTEGRATION-SPEC §9.8). An untrusted change escalates the item, naming the cause and the commits. The verified author isn't used on GitHub, because agent commits made with local git aren't signed. A code host without an equivalent push rule must check pushers or verified authors in its adapter instead, and declares which it uses through `Capabilities()`.

## 9. Idempotency and durability

- **Every write has an idempotency key** derived from the workflow ID and the decision that made it, for example `item/p/42:escalate:7`. Adapters check before writing:
  - Comments and created items carry a hidden marker with the key (on GitHub, an HTML comment `<!-- agent-loop:key -->`). A retry finds the marker and returns the existing object.
  - Markers are set-to-value, so applying one twice is a no-op.
  - Merge is pinned to a commit (FR-7 AC5). A retry finds the change already merged at that commit and succeeds.
- **Materialise** records the plan key and task key in each created item (FR-17 AC3). It validates the whole task list before the first write (FR-17 AC6).
- **Follow-ups** use the key `followup:<change>` (FR-8 AC3).
- **Stage results** are recorded in workflow history the moment the activity returns. A crash after that never reruns the stage (NFR-1). A crash during a run reruns it as a new attempt, and the implement stage first adopts an existing change (FR-3 AC3).
- **Review rounds** are counted from the structured review results in the code host (FR-6 AC5), and checked against the workflow's own count on reconcile. On a mismatch, the code host wins and the decision log records it.

## 10. Read model and decision log

The projection is written by a `RecordDecision` activity after each `Decide` call, and is rebuilt from Temporal history if lost.

| Table | Contents |
|---|---|
| `items` | project, item key, state, round, rounds allowed, blocking count, host, entered-state time, next action, wait reason |
| `decisions` | item, time, event, tracker state seen, stage result reference, actions (NFR-5) |
| `stage_runs` | item, stage, kind, attempt, host, start, end, result, redacted log path (FR-11 AC2) |
| `hosts` | name, capabilities, last seen, last check result |
| `plans` | project, plan key, state, proposal reference, created items |

SQLite in v0.1 (one file next to the server config). The same SQL runs on Postgres from v0.3.

## 11. Local API and CLI

The server serves gRPC on `127.0.0.1` (port in the deployment config), with an HTTP/JSON gateway. The CLI is a thin client:

| Command | API call | Effect |
|---|---|---|
| `agent-loop board [project]` | `Board` | Reads the projection and live workflow queries (FR-11 AC1, AC4) |
| `agent-loop item pause\|resume\|abort <item>` | `ItemCommand` | Signal to the `ItemWorkflow` |
| `agent-loop project pause\|resume <project>` | `ProjectCommand` | Signal to the `ProjectWorkflow` |
| `agent-loop pause\|resume` | `DeploymentCommand` | Signal to the `DeploymentWorkflow` |
| `agent-loop mint <project>` | `Mint` | Starts the `MintWorkflow`, or says why it can't (FR-9 AC1) |
| `agent-loop plan abandon <plan>` | `PlanAbandon` | FR-17 AC9 |
| `agent-loop project validate <project>` | `Validate` | FR-1 AC2, FR-13 AC1, AC4, NFR-4 AC1 |
| `agent-loop host check` | local | FR-14 AC4 |
| `agent-loop log <item>` | `ItemLog` | FR-11 AC2 |

API commands act as the owner. In v0.1 the API is reachable only on localhost. Remote access (v0.3) needs authentication, and is an open question (Q5).

## 12. Notifications

`Notify` actions go through the `Notifier` port to every channel in the deployment config. In v0.1: macOS desktop (through `osascript` or `terminal-notifier`), and a mention of the owner in the escalation comment.

For a remote server (v0.3, FR-11 AC5), the owner's machine runs `agent-loop notify-agent`, which polls the API over an outbound connection and shows desktop notifications. No inbound access is needed.

## 13. Configuration

- **Deployment config** (`~/.config/agent-loop/deployment.toml`): Temporal address and namespace, projection DB, API port, notifiers, secret-store backend, deployment limits.
- **Project profile** (`.agent-loop.toml` in the repository, or registered by path): the tracker and code-host adapters and their settings, the marker mapping, identities and owner accounts, the base branch, merge method, limits, stages (command, capabilities, runner, time limit), egress additions, image (v0.2), source of truth (v0.2) and timezone.

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
owners   = ["mayur-github-login"]
```

Both files are checked against JSON Schemas in `internal/profile`. `project validate` also calls each adapter's `Validate` and `Capabilities` methods, so adapter-specific settings are checked by the adapter that owns them.

## 14. Testing

| Level | What | Where it runs |
|---|---|---|
| Core unit tests | Table tests over `core.Decide` for every lifecycle edge, the §8 orders, and every FR-12 AC4 row | Every commit, fast |
| Workflow tests | Temporal test environment with fake adapters and a fake runner. Includes replay tests against recorded histories | Every commit |
| Scenario tests | Sections 11.1–11.3 scripted against the fake adapters, including worker kills (NFR-1 AC1) and blocked inbound network (NFR-4 AC2) | Every commit |
| Conformance suite | One suite of behaviours every `WorkTracker` and `CodeHost` must show: compare-and-set claim, idempotent writes, pusher records, manual-comment clearing, pinned merge. Runs against `fake`, and against `github` on a sandbox repository | Fake: every commit. GitHub: nightly and before a release |
| Platform | The suites on macOS and Linux runners (NFR-3 AC1) | CI |

The fake adapters are scriptable: a test can make "the owner" or "another human" push, label, comment, resolve and merge, so every manual-action row can be tested.

## 15. Release mapping

| Release | Technical scope |
|---|---|
| v0.1 | Core, `ItemWorkflow`, `ProjectWorkflow` (single project), GitHub and fake adapters, `LocalRunner` with the Claude sandbox, SQLite projection, Keychain and env secrets, desktop and tracker notifiers, CLI and local API, conformance suite |
| v0.2 | `MintWorkflow`, `MaterialiseWorkflow`, capability task queues across several hosts, agent-run slots, `DockerRunner`, base image |
| v0.3 | Several projects on one deployment, `DeploymentWorkflow` limits, Postgres projection, remote server, `notify-agent`, host bootstrap, API authentication |
| Later | Jira and GitLab adapters (when asked for, D3), Slack and push notifiers, remote runner, Vault or 1Password secrets |

## 16. Open questions

Resolved (2026-11-14, owner):

- **Q1 Who pushes code → the handler (T18).** The handler pushes, opens the change and posts replies as the coder identity. Option B (the worker pushes) was dropped. The risk is a prompt-injected agent misusing its push credential, so the credential is limited: a short-lived token for one repository with `contents: write`, given through the credential helper (§7.3), and a branch ruleset that lets the coder identity push only to the loop's branch prefix (default `agent/`), never to the base branch.
- **Q2 Untrusted commits → prevented by a branch ruleset, not detected (T19).** The check exists because implement and address run the branch's code (builds, tests) on the host before any review, and because an outside commit could carry text aimed at the LLM reviewer. FR-13 AC5 is met by construction: a GitHub ruleset on the loop's branch prefix lets only the coder identity and the owner accounts push, so no one else can add a commit. `project validate` fails if the ruleset is missing or allows other actors. The pre-stage check (§8.3) still rejects changes from forks, and checks that the ruleset is still in force. Pusher history is not read. (Amended by INTEGRATION-SPEC I6: the verified-author fallback was dropped on GitHub, because local agent commits aren't signed.) To confirm: rulesets on private repositories need a GitHub plan that supports them. If the plan doesn't, `project validate` fails and names the gap.
- **Q3 Structured review results → a PR review by the reviewer identity (T20).** The reviewer posts a PR review whose body holds a fenced JSON block (the reviewed commit, the kind, and each finding with its ID, score and status), plus one inline comment per finding. The loop reads reviews through the normal API, and counts only reviews authored by the reviewer identity, so text anyone else posts can never act as a review result. Rounds are counted from these after a restart (FR-6 AC5). The JSON schema is in `INTEGRATION-SPEC.md`.

Still open:

- **Q4 Polling budget.** GitHub's rate limit is per app installation. Several items and a 60 s poll may need GraphQL batching. To be measured in v0.1. 
- **Q5 API authentication** for the remote server (v0.3): mTLS, or a token in the deployment config.
- **Q6 Jira item ↔ change link.** A branch naming rule, a change body field, or Jira's development panel. Needed before the Jira adapter.
