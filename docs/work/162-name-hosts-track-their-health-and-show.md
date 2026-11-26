# Work plan: Name hosts, track their health, and show capable-host waits

**Issue:** [#162](https://github.com/mayusuk/agent-loop/issues/162) (feature)
**Branch:** `issue/162-name-hosts-track-their-health-and-show`
**Base:** `origin/main @ 0f37cc6` (checked before branching)
**Status:** Draft

## Summary

`agent-loop worker` gets `--name` (default the OS host name), records that name on its stage runs, and heartbeats its name and capabilities into the `hosts` table. The board gets a hosts section (name, capabilities, health, last seen) and shows a stage that waits for a capable host, naming the capability set it needs (FR-14 AC1–AC3, FR-11 AC4).

## Context

- Plan `docs/plans/2026-11-25-v0-2-minter-materialise-and-multiple.md` T9, decisions D3 (host = `--name`, default OS host name), D4 (v0.2 health is the poll test only), D8 (capable-host wait), and the `DescribeTaskQueue` risk.
- PRODUCT-SPEC §6 paragraph 2 and §5: a stage waiting for a capable host stays in its state; the wait is shown on the board only, never as a label. FR-11 AC4, FR-14 AC1–AC3.
- TECHNICAL-SPEC §6.4 (queues `cap:<sorted set>`, a worker polls every subset queue; healthy = polled within the last minute), §10 (`hosts` table; the board's HOST is the name the `hosts` table uses), §11 (`worker` command).
- Already in place: `CapabilityQueue`, `WorkerQueues`, `--capabilities`, the `hosts` table with `UpsertHost`/`Hosts`, `Activities.HostName` recorded with `SetRunHost` when `RunStage` starts, and a once-a-minute `UpsertHost` in the worker using `os.Hostname()`.
- `RunStage` has only a StartToClose timeout and `MaximumAttempts: 1`, so a task on a queue with no poller stays scheduled without using an attempt or a time limit, and starts when a poller arrives (FR-14 AC2). Work state lives in Temporal and the tracker, not the worker, so a different worker with the same capabilities continues it (FR-14 AC3).
- The board reads items from the projection (`items`, `stage_runs`); a stage run row is written by `RunStarted` before `RunStage` is scheduled, and gets its host only when a worker starts it.

## Approach

- **Worker (`cmd/agent-loop/worker.go`):** `--name` flag, default `os.Hostname()`, trimmed; empty is a usage error. `acts.HostName = name`. A `hostBeat` helper upserts `{name, caps, now}` keeping the previous `LastCheck`, every 15 s, and only after the Temporal client's health check passes. The start line prints the name.
- **Projection:** `Host.Healthy(now)` (last seen within `HostHealthWindow` = 1 minute) and `Host.Covers(caps)`. `BoardRow.QueuedStage`: the stage of the item's newest stage run that hasn't ended and has no host yet (scheduled, not started).
- **API:** `BoardReply.hosts` (name, capabilities, healthy, last seen). `Service.StageCaps` (per project, from the profile). `Board` reads the hosts, and for a row with a queued stage whose capability set no healthy host covers, sets the wait reason to `capable_host <caps>`.
- **CLI board:** a `Hosts` section after the projects: NAME, CAPABILITIES, HEALTH, LAST SEEN.
- **Server start:** passes `StageCaps` from the profile.
- **Tests:** worker queue mapping for a profile whose stage needs `x` (AC1, AC2); projection queued stage and host health; service wait and hosts (AC2, AC4); CLI board output (AC1, AC4); heartbeat helper; a workflow test that the stage runs on `cap:agent+x` with attempt 1; a live-Temporal test (gated on `AGENT_LOOP_LIVE=1`) where the `x` worker is absent, then started, then replaced (AC2, AC3).

## Steps

- [x] Projection: `Host.Healthy`, `Host.Covers`, `BoardRow.QueuedStage`, with tests.
- [x] API: hosts on the board and the capable-host wait; proto regenerated; service tests.
- [x] CLI: hosts section on `agent-loop board`; `--name` and the heartbeat in `worker`; `StageCaps` in server start; tests.
- [ ] Workflows: test that a stage needing `x` runs on `cap:agent+x` with attempt 1, and a live test for the wait, the return and the move to another worker.
- [ ] Specs touched only where this issue's behaviour is named (TECHNICAL-SPEC §10 HOST, §11 `worker` row); outcome.

## Acceptance criteria

- FR-14 AC1: a second worker with `--name h2 --capabilities agent,x` joins with no workflow change and appears on the board.
- FR-14 AC2: a stage that requires `x` runs only on the worker with `x`. While that worker is down, the stage waits in its state and the board names the missing capability. It starts when the worker returns, with no attempt used.
- FR-14 AC3: moving a project's work from one worker to another needs no state migration (stop one worker, start another with the same capabilities, and the item continues).
- FR-11 AC4: the board lists hosts with capabilities and health. A host that hasn't polled for over a minute shows unhealthy.

## Decisions

- **D1** [gap] TECHNICAL-SPEC §11 (`worker`), plan D3: `--name` is trimmed; an empty name, or no `--name` when the OS host name can't be read, is a usage error (exit 2) naming `--name`. The board's HOST and stage runs use this name, so TECHNICAL-SPEC §10's "the worker's OS host name" becomes "the worker's `--name`, by default the OS host name". Why: D3 gives the default but not the error cases.
- **D2** [ambiguity] TECHNICAL-SPEC §6.4, plan D4, D8: "polled within the last minute" is read as the worker's heartbeat in the `hosts` table, not Temporal's `DescribeTaskQueue` poller list. The worker records `last_seen` every 15 seconds, only after the Temporal client's health check passes, and a host is healthy when `last_seen` is at most one minute old. Why: a poller's last access time is refreshed only when a long poll (about a minute) returns, so an idle, healthy worker would flap around the one-minute line; the poller list also forgets a stopped worker only after minutes, Temporal's testsuite can't report pollers (the plan's risk), and the `hosts` table is needed anyway to list a host that is down.
- **D3** [ambiguity] plan D8, FR-14 AC2, FR-11 AC4: the capable-host wait is worked out by the board when it is read, not by the item workflow. A stage waits for a capable host when the item's newest stage run hasn't ended and no worker has started it (no host recorded), and no healthy host's capabilities include every capability the stage requires (default `agent`). A healthy host with those capabilities polls that queue (§6.4), so this is "a queue with no healthy poller". Nothing is written: the workflow, its history and the decision log don't change, and the stage stays scheduled, so no attempt or time limit is used. Why: the wait is board-only (PRODUCT-SPEC §5) and has no state change; detecting it in the workflow would add an activity and history events every poll for a value only the board reads.
- **D4** [gap] FR-11 AC4, FR-14 AC2: the wait reason is `capable_host <capabilities>`, the stage's sorted capability set joined with commas, for example `capable_host agent,x`. It replaces the row's wait reason; the next action is unchanged. Why: the specs say the board names the capability but give no text; this follows the `dependency #5` pattern.
- **D5** [gap] FR-11 AC4: `agent-loop board` prints a `Hosts` section after the projects, also when the board is limited to one project (hosts are deployment-wide): NAME, CAPABILITIES (comma-separated), HEALTH (`healthy` or `unhealthy`), LAST SEEN (time since, in whole seconds, for example `40s ago`). With no hosts it prints `no workers have started`. The API's `BoardReply.hosts` carries name, capabilities, healthy and `last_seen_seconds_ago`. Why: the AC lists the columns but not the layout.
- **D6** [gap] plan D4: in v0.2 the `last_check` column is kept as the worker found it and not shown; it never makes a host unhealthy. Why: `host check` is v0.3 (FR-14 AC4).
- **D7** [open] TECHNICAL-SPEC §10 (`hosts`): two running workers with the same name share one `hosts` row, and the last heartbeat's capabilities win. Not settled here, since D3 makes the name the owner's choice.
- **D8** [open] plan D8: the mint workflow (T4, T5) doesn't exist yet. Its mint runs need the same board rule once they are recorded as stage runs.
- **D9** [open] TECHNICAL-SPEC §10, §13: the projection is a local SQLite file that each worker opens at its own `[projection] path`. A worker on another machine records its heartbeat, and the stage runs it starts, in its own file, which the server doesn't read. Until the projection is shared (Postgres, v0.3), the hosts section and capable-host waits cover only workers that share the server's projection file.

## Verification

- `wf.py verify --plan` (gofmt, vet, build, race tests), plus `GOOS=linux go build ./...`.
- The live test with `AGENT_LOOP_LIVE=1`, if the Temporal dev server can be started here.

## Risks and open questions

- D2 and D3 read the plan's D8 mechanism differently (heartbeat and board-time check instead of `DescribeTaskQueue` in the workflow). The plan's risk anticipated a substitute for `DescribeTaskQueue`; the reviewer should confirm the reading.
- D9: multiple machines need a shared projection.

## Outcome

_Filled in before shipping._
