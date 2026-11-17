# Work plan: Implement the local API, server and CLI

**Issue:** [#22](https://github.com/mayusuk/agent-loop/issues/22) (feature)
**Branch:** `issue/22-implement-the-local-api-server-and-cli`
**Base:** `origin/main @ 955a19f` (checked before branching)
**Status:** Done

## Summary

Adds the owner's local API (gRPC on a Unix socket, with an optional token-protected HTTP/JSON gateway that is off by default) and the CLI commands that use it: `board`, `item pause|resume|abort`, `project pause|resume`, `pause|resume` and `log`. Adds `server init` (deployment config and a bundled Temporal + Postgres Compose file), `server start` (workflows and API for the registered project) and `worker --capabilities` (activities). This is the composition root that #21 left open: the worker passes one `projection.CoderRuns` as both `Activities.CoderRuns` and the GitHub host's `HostConfig.Runs`.

## Context

- TECHNICAL-SPEC §11 (API and CLI), T2 (Temporal with Docker Compose and Postgres), T13 (gRPC on a Unix socket, gateway off by default), §3 (server runs workflows and API, workers run activities), §6.1 (workflow IDs), §6.4 (queues), §7.2 (sandboxes deny the socket directory), §13 (deployment config).
- PRODUCT-SPEC FR-10 AC3 (pause), AC7 (abort), FR-11 AC1 (board columns and unlabelled waits), AC2 (log); release table: v0.1 is one project, FR-15 (multiple projects) is v0.3.
- #21 D3: T17 wires `projection.CoderRuns` into `HostConfig.Runs`. #21 built `DeploymentWorkflow`, `ProjectWorkflow`, `ItemWorkflow`, `RegisterWorkflows`, `RegisterActivities`, `WorkerOptions`, `WorkerQueues`.
- `projection.DB` already has `Board`, `ItemLog`; `profile` loads the deployment config and profiles; `secrets.GitHubApps` issues tokens.

## Approach

- `internal/api/v1/api.proto` and generated Go (`protoc-gen-go`, `protoc-gen-go-grpc`), committed with a `go:generate` line.
- `internal/api`: `Service` implements the gRPC service over two narrow ports, `Workflows` (signal and query, implemented over a Temporal client) and the projection. `Listen` makes the socket directory `0700` and the socket `0600`. `Gateway` is a hand-written HTTP/JSON handler over the same service with a bearer token. `Dial` is the CLI's client.
- `cmd/agent-loop`: the thin CLI commands; `server init` (writes config, Compose file and `.env`, runs `docker compose up -d`); `server start`; `worker`; `wire.go`, the composition of one project's adapters and activities, with `HostConfig.Runs` and `Activities.CoderRuns` set to the same `projection.CoderRuns`.
- `internal/secrets`: `GitHubApps.App` reads an app's ID and slug, so the worker can resolve the identities' bot accounts.
- `internal/adapters/github`: `Host.RequiredApprovals` (base-branch approvals, for `Config.RequireApproval`).
- `internal/projection`: `ItemWorkflows` finds an item's workflow ID for an item command.

## Steps

- [x] Proto, generated code, and the API service with socket permissions, gateway and client.
- [x] CLI commands over the API, with a test per command against a test server.
- [x] `server init` with the Compose file.
- [x] Composition of one project (`wire.go`), with the coder-run wiring test; `server start` and `worker`.

## Acceptance criteria

- The socket and its directory are created with modes `0600` and `0700`, and a test checks this.
- Each CLI command sends the right signal, which an API test checks against a test server.
- `agent-loop board` shows the FR-11 AC1 columns and the waits that have no label.
- `server init` writes a valid deployment config, and its Compose file passes `docker compose config` (skipped when Docker is missing).
- The board API exposes the WIP, pause and daily-limit state #23's dry run needs when the server is running.
- `projection.CoderRuns` is wired into the GitHub host as `HostConfig.Runs`, and a test shows a coder push is attributed to the coder, not `pusher_unknown`.

## Decisions

- **D1** [gap] TECHNICAL-SPEC §11, T13: the API is gRPC service `agentloop.api.v1.AgentLoop` (`internal/api/v1/api.proto`) with `Board`, `ItemCommand`, `ProjectCommand`, `DeploymentCommand` and `ItemLog`. `Mint` and `PlanAbandon` are v0.2 and not in v1 yet; adding them is backward compatible. `board --watch` streaming isn't built.
- **D2** [gap] TECHNICAL-SPEC §11 gateway: the HTTP/JSON gateway is hand-written over the same service: `POST /v1/<Method>`, protobuf JSON bodies, `Authorization: Bearer <token>` compared in constant time, 401 otherwise. It listens only on a loopback address. `api.gateway.token` is a secret reference whose whole name is read from the secret backend. Why: no grpc-gateway codegen is needed for five calls.
- **D3** [gap] TECHNICAL-SPEC §11 `item pause|resume|abort <item>`: the item is found on the board (the projection's unreleased, not merged or closed row) and the command is signalled to that row's workflow as an `OwnerCommand` event stamped with the server's time. `--project` picks one when the key is on several boards; an item not on the board is refused. `log <item>` takes `--project` the same way.
- **D4** [gap] TECHNICAL-SPEC §11 socket: an existing socket directory is narrowed to `0700`; a stale socket is replaced, one a server answers on is refused, and a path that isn't a socket is never removed.
- **D5** [gap] TECHNICAL-SPEC §11 CLI: every command that uses the server takes `--config <path>`; when the default config doesn't exist the defaults apply, so the CLI looks for the default socket.
- **D6** [gap] FR-11 AC1: the board prints the deployment pause, then per project its pause and "daily limit reached", then a table: item, state, round as `used/allowed`, blocking count, host, time in state, next action, and wait reason. A workflow that can't be queried shows "state unknown" with the reason instead of guessing.
- **D7** [open] FR-11 AC1, FR-2 AC6: unclaimed items with `needs-owner` aren't on the board, since the projection has no row for an item no workflow runs. Needs a projection write from the supervisor's dependency escalation.
- **D10** [gap] TECHNICAL-SPEC §11 `server init`: it writes the deployment config (mode `0600`) with every default spelled out, the gateway off and no projects, only when none exists; an existing config is kept and must load, or init fails. `--config` names another path and `--no-start` skips starting Temporal.
- **D11** [gap] T2: the Compose files go in `temporal/` next to the deployment config: `compose.yaml` (project `agent-loop`, `postgres:16.6` and `temporalio/auto-setup:1.28.1`), a dynamic config turning on worker versioning (§6.3), and `.env` (mode `0600`) with a random Postgres password made once and kept on re-runs. Temporal is published only on `127.0.0.1:7233`; Postgres isn't published. Init runs `docker compose -f <file> up -d`.
- **D8** [gap] TECHNICAL-SPEC §11 `server start` "loads every project", PRODUCT-SPEC §12 (v0.1 is one project, FR-15 is v0.3): `server start` and `worker` load every registered project and refuse to run when more or fewer than one is registered. Why: activities are registered by method name on the shared `core` queue, so a second project's activities would act on the first one's repository.
- **D12** [gap] INTEGRATION-SPEC §9.1, #21 D7: the worker finds each identity's accounts at start: the app's ID and slug from `GET /app` with the app's JWT, then its bot user from `GET /users/<slug>[bot]`. The worker's base-merge signature is the coder bot's login and `<id>+<login>@users.noreply.github.com`.
- **D13** [gap] FR-4 AC7: `Config.RequireApproval` is read once at worker start: the base branch requires approval when its rulesets or classic protection require at least one approving review (`Host.RequiredApprovals`). A change to the rule takes effect on the next worker start.
- **D14** [gap] TECHNICAL-SPEC §7.3, I11: the GitHub client acts with one renewing token lease per identity, held for the worker's life and revoked when it stops: the loop's §9.1 row, the "Reviewer, worker" row, and the "Coder, worker" row narrowed to `contents: read`, so no coder token that can push is live outside a recorded coder run. Base merges still issue their own push token per activity.
- **D15** [gap] TECHNICAL-SPEC §3, §6.3: `server start` runs the workflows on the `core` queue with no activity polling, makes this build the worker deployment's current version (build ID = the binary's version), starts `deployment` and `project/<name>` with the ID conflict policy *use existing*, then serves the API (and the gateway when on) until SIGINT or SIGTERM. `worker` runs only activities, on `core` and each covered stage queue, and records its host, capabilities and last-seen time in `hosts` every minute.
- **D16** [gap] TECHNICAL-SPEC §7.3: the worker's clone fetches authenticate with the client's coder token (contents read only), not the loop token. Run trees, results and logs go under the user cache directory's `agent-loop/workspaces`.
- **D17** [open] TECHNICAL-SPEC §6.2, §13: a running `ProjectWorkflow` keeps the settings it started with, so a profile change needs the workflow restarted; no command does that yet.
- **D9** [gap] FR-1 AC6, #23 (PR #65) review R3: the board's project entries also carry `running_items` (the WIP in use), `daily_runs`, `day` and today's `daily_spend`, read from the `ProjectWorkflow` query. Wiring: `run --dry-run`/`check` from #23 dials the socket from the deployment config; when `Board(project)` answers it uses `len(running_items)` as the WIP in use, `paused` or `deployment_paused` as the pause, and `daily_limit_reached`/`daily_runs` as the daily limit state; when it doesn't answer, the output says the server isn't running and assumes none, as #23 does now. Whichever of #22 and #65 merges second makes that change.

## Verification

`wf.py verify`, plus `GOOS=linux go build ./...` and `CGO_ENABLED=0 GOOS=darwin go build ./...`.

## Risks and open questions

- Running against a real Temporal server (worker versioning, Compose images, the dynamic-config keys that turn on worker deployments) isn't covered by CI; the Compose file is checked only with `docker compose config`. Scenario 11.1 live (T20, #25) is where it gets exercised.

## Outcome

Done as planned: the gRPC API and gateway (`internal/api`), the CLI commands `board`, `item pause|resume|abort`, `project pause|resume`, `pause`, `resume`, `log`, `server init`, `server start` and `worker`, and the composition root in `cmd/agent-loop/wire.go`. Tests: `TestT13_SocketAndDirectoryModes`, `TestT13_GatewayNeedsBearerToken`, `TestFR10AC3_CLICommandsSendSignals`, `TestFR11AC1_BoardShowsColumnsAndUnlabelledWaits`, `TestFR11AC2_LogShowsRunsAndDecisions`, `TestT2_ServerInitWritesConfigAndCompose` (runs `docker compose config`, skipped without Docker), `TestI11_WorkerHostAttributesCoderPushToCoder`, `TestFR1AC6_BoardCarriesSupervisorState`, `TestFR4AC7_RequiredApprovals`.

Deviations: `Host.RequiredApprovals` (D13) and `GitHubApps.App` (D12) were added because the worker needs them; `validateBaseRules` now shares the rule reading with `RequiredApprovals`. The board API also carries the supervisor state #23's dry run needs (D9, added to the issue during the work). `server start` and `worker` against a real Temporal server aren't exercised in CI (see Risks). No follow-up issues filed: D7 and D17 go to the spec issue.
