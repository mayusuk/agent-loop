# Work plan: Implement the local API, server and CLI

**Issue:** [#22](https://github.com/mayusuk/agent-loop/issues/22) (feature)
**Branch:** `issue/22-implement-the-local-api-server-and-cli`
**Base:** `origin/main @ 955a19f` (checked before branching)
**Status:** In progress

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

- [ ] Proto, generated code, and the API service with socket permissions, gateway and client.
- [ ] CLI commands over the API, with a test per command against a test server.
- [ ] `server init` with the Compose file.
- [ ] Composition of one project (`wire.go`), with the coder-run wiring test; `server start` and `worker`.

## Acceptance criteria

- The socket and its directory are created with modes `0600` and `0700`, and a test checks this.
- Each CLI command sends the right signal, which an API test checks against a test server.
- `agent-loop board` shows the FR-11 AC1 columns and the waits that have no label.
- `server init` writes a valid deployment config, and its Compose file passes `docker compose config` (skipped when Docker is missing).
- `projection.CoderRuns` is wired into the GitHub host as `HostConfig.Runs`, and a test shows a coder push is attributed to the coder, not `pusher_unknown`.

## Decisions

None.

## Verification

`wf.py verify`, plus `GOOS=linux go build ./...` and `CGO_ENABLED=0 GOOS=darwin go build ./...`.

## Risks and open questions

- Running against a real Temporal server (worker versioning, Compose images) isn't covered by CI; the Compose file is checked only with `docker compose config`.

## Outcome

_Filled in before shipping._
