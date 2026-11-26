# Plan: v0.2: minter, materialise and multiple hosts

**Status:** Draft
**Base:** `origin/main @ 5aef6f1`
**Tracking issue:** —

## Goal

Deliver release v0.2 as PRODUCT-SPEC §12 defines it, plus the Docker runner that TECHNICAL-SPEC §15 puts in v0.2 (D1):

- **Planning on demand:** `agent-loop mint <project>` runs the project's mint handler, which opens a plan proposal (FR-9). When the owner merges the proposal, the deterministic materialise stage creates the plan's items as ready, plus one tracking item (FR-17). This covers FR-18's mint and materialise rows, FR-10 AC1's mint and materialise sentence, FR-11 AC1's minting and materialise waits, and FR-12 AC4's plan-proposal rows.
- **Several hosts:** workers join with a name and a capability list. A stage runs only on a host with every capability it requires, and the board shows hosts, their health and stages that wait for a capable host (FR-14 AC1–AC3, FR-11 AC4).
- **Agent-run slots:** per-project and deployment-wide caps on handlers running at once (FR-16 AC2). Stages wait in their state while no slot is free, and the board shows why (§6 paragraph 2, NFR-6 AC1).
- **Docker runner:** `runner = "docker"` runs a stage in a container built from a published base image, with egress limited by a container network (TECHNICAL-SPEC T8–T10, §7.2).

The release is done when scenarios §11.1 and §11.2 pass against the fake adapters.

## Context

- v0.1 is complete (plan `2026-11-15-v0-1-mvp.md`, T1–T20 = #6–#25). The v0.1 release (tags, Homebrew formulas for `agent-loop` and `agent-loop-claude`) and the open spec follow-ups #126, #129, #131, #134 and #136 are being finished separately. Spec text in TECHNICAL-SPEC §7.2, §11 and §13 may change while this plan is worked, so each task re-reads the spec sections it cites.
- **Already in place:**
  - `contract.StageMint` and a `tasks.schema.json` (`contract/v1/`). `contract/tasks.go` holds only the types.
  - `WorkTracker.CreateItem` and `AddDependency` in the fake and GitHub adapters.
  - `CapabilityQueue` and `WorkerQueues` in `internal/workflows/wire.go`, and the `--capabilities` flag on `worker`.
  - A `hosts` table in the projection.
  - The profile keys `source_of_truth`, `plans_dir`, `limits.agent_runs` and `runner = "docker"`, which adds the `docker` capability.
  - The CLI's `mint` and `plan` commands exist but are marked v0.2.
- **Missing:**
  - the core's planning lifecycle;
  - the mint stage in the worker;
  - `MintWorkflow` and `MaterialiseWorkflow`;
  - a code-host read of a plan at a base-branch commit;
  - agent-run slots and `wait_slot`;
  - host naming and health;
  - board rows for hosts and planning;
  - the Docker runner and the base image;
  - the §11.2 scenario.
- **Rules:**
  - `internal/core` stays pure: it imports only the standard library and `contract/`.
  - Every write takes an idempotency key (TECHNICAL-SPEC §9).
  - Tests never call GitHub, Claude, Docker or a real Temporal server unless `AGENT_LOOP_LIVE=1`.
  - Tests are named after the requirements they prove.
- **Sources read:**
  - PRODUCT-SPEC §5, §6, FR-9 to FR-18, §9, §11, §12;
  - INTEGRATION-SPEC §4–§8;
  - TECHNICAL-SPEC §2 (T8–T10), §4–§7, §9–§11, §13–§15;
  - the code listed above.

## Approach

**Planning lifecycle in the core.**
- A second pure decision function, `DecidePlan(PlanState, Event, Policy) (PlanState, []Action)`, sits beside `Decide`. It covers the planning states in PRODUCT-SPEC §6: `minting`, `plan_proposed`, `plan_rejected`, `materialising`, `mint_escalated`, `materialise_escalated`, `plan_abandoned` and `complete`.
- It also covers the mint refusal reasons (FR-9 AC1), the owner-merge rule for proposals and plan fixes (FR-13 AC7, FR-17 AC8) and FR-18's mint and materialise rows.
- Like items, it produces decision records.

**Mint.**
- `MintWorkflow` (`mint/<project>`) asks the project for an agent-run slot and a daily-limit unit, then runs the mint stage on the mint stage's capability queue.
- The worker runs the mint handler on the plan branch `<branch_prefix>plan/mint-<n>`. It then checks that the branch holds only `plans/<plan_key>/` with a valid task list (INTEGRATION-SPEC §5.2), opens the proposal as the coder identity and labels it `plan-proposal` as the loop identity.
- `agent-loop mint` starts the workflow, or prints the FR-9 AC1 reason it can't.

**Materialise.**
- The project supervisor detects an owner merge of a proposal, or of a plan fix while the plan is in `materialise_escalated`. It then starts `MaterialiseWorkflow` (`plan/<project>/<plan-key>`).
- The workflow reads the plan from the latest base-branch commit that changed it, checks that the owner merged that commit and validates the whole task list.
- It then creates items in dependency order through `CreateItem` (with `task` records) and creates the tracking item (with a `plan` record). Restarts find existing items by their records, so no item is created twice (FR-17 AC3).
- `agent-loop plan abandon` and tracking-item close (FR-17 AC9, AC10) complete it.

**Slots and hosts.**
- `ProjectWorkflow`'s admission Update gains `wait_slot`. It caps the project's running agent stages at `limits.agent_runs` and asks `DeploymentWorkflow` for a deployment-wide slot. Mint counts toward slots but not toward WIP.
- A worker gets `--name` (default: the OS host name). Each worker records its name, capabilities and last poll in the `hosts` table.
- A host is healthy when its worker polled within the last minute (TECHNICAL-SPEC §6.4). The board shows hosts and any stage whose queue has no healthy poller.

**Docker.**
- A base image (git, Node, the agent CLIs) is built and published by a workflow. A profile names a project image that extends it.
- `DockerRunner` runs one container per run. It mounts only the worktree and the input and result directory, and uses a network that allows only the egress allowlist. It kills the container on timeout or cancellation.

**Order.** Contract and core first, then adapters, then the workflows. Slots, hosts and Docker are independent of planning and can run in parallel with it. The §11.2 scenario comes last.

## Decisions

- **D1** [conflict] PRODUCT-SPEC §12, TECHNICAL-SPEC §15: TECHNICAL-SPEC §15 puts `DockerRunner` and a base image in v0.2. PRODUCT-SPEC §12 doesn't list them, and §11.2 doesn't need them. Owner: include the Docker runner and the base image in v0.2. They don't add to the §11.2 acceptance (planning session, 2026-11-25).
- **D2** [gap] PRODUCT-SPEC §11.2, INTEGRATION-SPEC §12.4: a live §11.2 run needs a mint handler in the starter handlers (`agent-loop-claude`). Owner: plan that handler separately, in `mayusuk/agent-loop-handlers`. In this repository, the §11.2 scenario tests use a scripted fake mint handler (planning session, 2026-11-25).
- **D3** [gap] FR-14 AC1, TECHNICAL-SPEC §6.4, §10 (`hosts`), §11 (`worker`): a host is identified by `agent-loop worker --name <name>`, which defaults to the OS host name. So two workers on one machine count as two hosts. Owner ruling (planning session, 2026-11-25).
- **D4** [gap] TECHNICAL-SPEC §6.4: a healthy host is defined by "polled within the last minute and its last `agent-loop host check` passed", but `host check` is v0.3 (FR-14 AC4). In v0.2, health is the poll test only. A host with no check result is not counted unhealthy. Why: FR-14 AC2 needs a health test now, and the check arrives in v0.3.
- **D5** [gap] INTEGRATION-SPEC §8.3: materialise needs to read a plan directory at a base-branch commit, find the latest base-branch commit that changed the plan, and know who merged the change that made it (FR-17 AC1, AC8). No `CodeHost` method does this. T3 adds a method to §8.3, with its conformance cases in §11. Why: FR-17 AC8 can't be met through the current port.
- **D6** [gap] INTEGRATION-SPEC §5.2 (mint): the provisional mint ID `mint-<n>` has no defined source for `n`. It is one more than the number of `proposal` records the coder identity has written on the code host (open or closed proposals), so it survives a lost workflow. Why: same pattern as the claim number (TECHNICAL-SPEC §6.1).
- **D7** [gap] TECHNICAL-SPEC §13: the deployment-wide agent-run limit is the deployment config's `[limits] agent_runs` (default 4, from 1 to 32). `limits.agent_runs` in a profile can't exceed it (already checked by `internal/profile`). Why: FR-16 AC2 gives the default but no key or range.
- **D8** [gap] FR-14 AC2, FR-11 AC4: "a stage waits for a capable host" is detected by the item or mint workflow: its `RunStage` activity is scheduled on a capability queue with no healthy poller (`DescribeTaskQueue`). The board's wait reason names the capability set. The activity stays queued, so no attempt is used. Why: TECHNICAL-SPEC §6.4 says what the board shows but not how the wait is found.
- **D9** [gap] TECHNICAL-SPEC T10, §7.2: the base image is `ghcr.io/mayusuk/agent-loop-base:<version>`, built for linux/amd64 and linux/arm64 by a tag-triggered workflow. A profile names the project's image with `image` on a stage that uses `runner = "docker"`. Why: T10 says a base image is published and a project names its image, but gives neither the registry nor the key.
- **D10** [gap] INTEGRATION-SPEC §12.4 (N12): a live §11.2 run, and minting through `--skill plan-work`, depend on work outside this repository:
  - result-only modes for the `work-issue`, `review-pr`, `address-pr` and `plan-work` skills;
  - a mint shape in `agent-loop-claude`'s agent draft;
  - a `plan-work` mode that writes `plans/<key>/plan.md`, `tasks.yaml` and `tasks/<task>.md` and creates no issues.

  Owner: record these as a dependency of the handlers plan (D2), not as tasks here (planning session, 2026-11-25). Why: the skills live with the owner's Claude Code skills and the handler lives in `agent-loop-handlers`. Today the skills post through `wf.py` and ask the owner interactively, and the `--skill` prompt only tells them not to post.
- **D11** [gap] TECHNICAL-SPEC §11, INTEGRATION-SPEC §7: a new local command, `agent-loop plan validate <dir>`, runs `ValidateTasks` on a plan directory with the profile's types, and checks external `depends_on` refs when a tracker is reachable. It exits 1 and lists every problem. Owner: this repository adopts the minting layout (T13). Why: owners and mint handlers need to check a hand-edited task list before merging it, instead of finding out from a materialise escalation (§11.2 step 10).

## Out of scope

- The `agent-loop-claude` mint handler (D2). It is planned in `mayusuk/agent-loop-handlers`.
- Everything in v0.3: several projects (FR-15), host bootstrap and `agent-loop host check` (FR-14 AC4), remote notifications (FR-11 AC5), the remote server (NFR-3 AC2), Postgres and API authentication.
- Milestone gates (FR-9 AC4, reserved).
- macOS Developer ID signing (#138).
- The Jira and GitLab adapters.

## Risks and open questions

- **Spec text is moving.** The v0.1 follow-ups are editing the same spec sections at the same time. Each task re-reads what it cites, and a task that finds a conflict records it in its work plan's Decisions.
- **Docker in CI.** The macOS runners have no Docker. `DockerRunner` tests use a fake `docker` executable, and real-container tests are gated on `AGENT_LOOP_LIVE=1`.
- **GitHub API cost.** Materialise and the plan reads add GraphQL and REST calls (TECHNICAL-SPEC Q4, still open). The plan read uses the contents API at a pinned commit.
- **`DescribeTaskQueue`** must behave the same in Temporal's testsuite and on a real server for D8. If the testsuite can't report pollers, the poller check goes behind a small interface with a fake.

## Tasks

### T1: Extend the handler contract for mint and validate task lists

Complete the v1 contract for the mint stage in `contract/`:

- the `mint` input fields (`plan_key`, `source_of_truth`, `plans_dir`, INTEGRATION-SPEC §4.2);
- the mint `done` result (`plan_key`, `head`, `proposal`) and `complete` (§5.2, FR-18 AC1, AC2);
- a full `tasks.yaml` validator, `ValidateTasks(plan string, file []byte, bodies fs.FS, types []string, itemExists func(string) bool)`, that applies every rule in INTEGRATION-SPEC §7 and returns every problem it finds;
- updates to `contract/v1/*.schema.json` where needed;
- `ValidateResult` accepts `complete` only from mint, and accepts mint and materialise `done` shapes (FR-18 AC3).

**Depends on:** —

**Acceptance criteria:**
- FR-18 AC1: `complete` is accepted only for the mint stage, and `done` for mint carries `plan_key`, `head` and `proposal`.
- FR-18 AC3: a mint result that is malformed or not allowed is reported invalid.
- FR-17 AC6: `ValidateTasks` rejects each case in INTEGRATION-SPEC §7: schema mismatch, `plan` not equal to the directory, duplicate key, unconfigured type, missing body file, body over 64 KiB, unknown `depends_on` key, missing external item, and a cycle. A valid list passes.
- `plan_key` must match `[a-z0-9-]{1,64}`.
- Tests are named after the requirements (`TestFR17AC6_...`, `TestFR18AC1_...`).

### T2: Add the planning lifecycle to the core

Add `DecidePlan(PlanState, Event, Policy) (PlanState, []Action)` to `internal/core`, with a decision record for every call, covering every planning state and edge in PRODUCT-SPEC §6:

- `minting` → `plan_proposed`, `complete` or `mint_escalated`, with retries as in FR-16 AC1;
- `plan_proposed` → `materialising` on an owner merge; → `plan_rejected` when it is closed unmerged; → `materialise_escalated` when a non-owner merges it;
- `materialising` → done, or `materialise_escalated` for an invalid list, a plan not approved, or retries used up;
- `materialise_escalated` → `materialising` on an owner-merged plan fix (with a new attempt count, FR-17 AC8), or `plan_abandoned`.

Add a `MintAllowed(ProjectPlanning) (bool, Reason)` check for FR-9 AC1. It refuses while any of these is true: an open proposal, an unfinished materialise (including `materialise_escalated` that hasn't been abandoned), another minter, a paused project, or the daily limit. Add the FR-12 AC4 plan-proposal rows:
- a non-owner merge is recorded and starts nothing;
- an owner close records the plan as rejected;
- removing `needs-owner` from a proposal is ignored.

Mint and materialise escalations emit actions that mark the proposal when there is one, and that notify (FR-10 AC1). The core still imports only the standard library and `contract/`.

**Depends on:** T1

**Acceptance criteria:**
- Every planning edge in PRODUCT-SPEC §6 has a table test over `DecidePlan`.
- FR-9 AC1: `MintAllowed` refuses with a distinct reason for each condition, and allows minting while items are blocked, escalated or in review.
- FR-9 AC5: `complete` leads to `complete`, and a later mint from `complete` is allowed.
- FR-17 AC8: a plan fix merged by the owner restarts materialise with a new attempt count, a fix merged by a non-owner escalates again, and a merged change to a plan that isn't in `materialise_escalated` starts nothing (D33).
- FR-12 AC4: each plan-proposal row has a test.
- FR-10 AC1: mint and materialise escalations mark the proposal when there is one, and always notify.
- `internal/core/imports_test.go` still passes.

### T3: Read plans from the code host and look up plan records

Add the `CodeHost` support that materialise needs (D5) to `internal/ports`, `internal/adapters/fake` and `internal/adapters/github`. Write the method into INTEGRATION-SPEC §8.3. It must:

- find the latest base-branch commit that changed `plans/<plan-key>/`;
- report the change that brought that commit in and who merged it (`Actor`);
- return the plan files at that commit.

On GitHub, use the commits API filtered by path, the commit's associated pull request, and the contents API pinned to the commit. Also:

- Make `CreateItem` find `task` and `plan` records from `NewItem.Since` (the plan's merge time), as INTEGRATION-SPEC §6.1 says, if it doesn't already.
- Add lookups for `proposal` records on pull requests whose head is a plan branch.
- Add the conformance cases for these to `conformance/` (next free C numbers) and the case list in INTEGRATION-SPEC §11.

**Depends on:** T1

**Acceptance criteria:**
- FR-17 AC8: for a plan changed by two merged PRs, the method returns the later commit, its PR and its merger. The fake and GitHub adapters both pass the new conformance cases.
- FR-17 AC3: `CreateItem` with the same key returns the existing item when it is retried right after a write (as in C1), for `task` and `plan` records.
- FR-13 AC7: the merger is reported by stable account ID, with no "is owner" decision in the adapter.
- The GitHub adapter is tested against the in-memory GitHub stand-in, with no network calls.

### T4: Run the mint stage in the worker

Add the mint stage activity to `internal/workflows` (stage execution and activities). It must:

- work out the provisional `mint-<n>` (D6) and prepare a workspace on `<branch_prefix>plan/mint-<n>` from the base tip;
- build the `mint` input from the profile's `source_of_truth` and `plans_dir`, and run the handler with the coder identity's git setup (TECHNICAL-SPEC §7.2) and the coder's push token (§7.3);
- validate the result. Check that the remote head equals `done.head`, that the branch changes nothing outside `plans/<plan_key>/`, that `plans/<plan_key>/` doesn't exist on the base branch, and that `plan.md` exists and `tasks.yaml` passes `ValidateTasks` (INTEGRATION-SPEC §5.2). Any failure makes the result `failed`;
- open the proposal as the coder identity with a `proposal` record, and label it `plan-proposal` as the loop identity. Both writes are idempotent.

**Depends on:** T1, T3

**Acceptance criteria:**
- FR-9 AC3: a valid mint result opens exactly one proposal, labelled `plan-proposal`, and creates no work items. A retried activity finds the existing proposal.
- FR-9 AC7 and FR-18 AC3: a result whose branch touches a path outside `plans/<plan_key>/`, reuses an existing plan key, or has an invalid task list is `failed`.
- FR-9 AC5: `complete` opens nothing.
- FR-13 AC4: the proposal is opened by the coder and labelled by the loop identity, checked through the fake adapter's actor records.
- Tests use fake handler executables.

### T5: Add MintWorkflow and the mint command

Add `MintWorkflow` (`mint/<project>`, TECHNICAL-SPEC §6.1) driven by `DecidePlan`. It:

- asks `ProjectWorkflow` for admission (daily-limit unit, and agent-run slot once T8 lands; until then `granted` or `waiting_limit`);
- runs the mint stage with retries and time limits as in FR-16 AC1 and the crash rerun rule (D42);
- escalates to `mint_escalated` with notification;
- records the plan in the projection's `plans` table.

`ProjectWorkflow` tracks the project's planning state (open proposal, running materialise) for FR-9 AC1, and a project pause holds new mint runs (§6 paragraph 2). Wire the `Mint` API call and `agent-loop mint <project>` (TECHNICAL-SPEC §11): it starts the workflow or prints the refusal reason, and drops the "(v0.2)" mark from `help`. The board shows minting in progress (FR-11 AC1).

**Depends on:** T2, T4

**Acceptance criteria:**
- FR-9 AC1: `agent-loop mint` refuses, with its reason, during an open proposal, a running or escalated materialise, another mint, a paused project, or a reached daily limit. It never starts on its own.
- FR-9 AC6: a second start while a mint runs is refused by the workflow ID.
- FR-16 AC3: a mint run uses a daily-limit unit and doesn't take a WIP slot.
- FR-10 AC4 and FR-18 AC1: `needs_owner` and exhausted retries go to `mint_escalated` and notify. `mint` again leaves `mint_escalated`.
- FR-11 AC1: `agent-loop board` shows a running mint.
- NFR-1: a worker crash during mint reruns it with the same attempt number (Temporal testsuite).

### T6: Add MaterialiseWorkflow

Add `MaterialiseWorkflow` (`plan/<project>/<plan-key>`, TECHNICAL-SPEC §6.1). The project supervisor starts it when the owner merges a proposal, or merges a plan fix while the plan is in `materialise_escalated` (FR-12 AC4, FR-17 AC1, AC8). It runs as the loop identity, with no handler (FR-17 AC5). It:

- reads the plan through T3 at the latest base-branch commit that changed it, and checks that the owner merged that change;
- validates the whole list with `ValidateTasks` before any write (FR-17 AC6);
- creates the task items in dependency order, each with title, body, type marker, `ready` and a `task` record;
- adds native dependencies, and creates the tracking item with `plan` and a `plan` record listing every task item and no type or ready marker (FR-17 AC2);
- ends `done` with the created refs, or `failed`, which is retried under FR-16 AC1 and then escalated (FR-17 AC7).

Escalations mark the proposal with `needs-owner`, comment why, and notify.

**Depends on:** T2, T3, T5

**Acceptance criteria:**
- FR-17 AC1 and AC4: a proposal that is still open, or that was closed unmerged, creates nothing. Only an owner-merged proposal starts materialise.
- FR-17 AC2: each task becomes one ready item with its type and dependencies. The tracking item has `plan`, no type and no `ready`, and the supervisor never picks it.
- FR-17 AC3 and FR-12 AC2: killing the worker mid-materialise and restarting creates every item exactly once (Temporal testsuite with the fake adapter).
- FR-17 AC6: an invalid list creates nothing and escalates, naming the problem.
- FR-17 AC8: an owner-merged fix restarts materialise with a new attempt count. A fix merged by a non-owner escalates again. Already-created items aren't duplicated.
- FR-17 AC5: no handler runs, and every write is by the loop identity.

### T7: Abandon plans, close tracking items, and show plans on the board

Add:
- the `PlanAbandon` API call and `agent-loop plan abandon <plan>` (FR-17 AC9, TECHNICAL-SPEC §11). It records the plan as abandoned on its tracking item or proposal, creates nothing more, leaves created items alone, and allows minting again;
- closing the tracking item when every task item it lists is closed (FR-17 AC10), checked at each full reconcile;
- board rows for the planning states, including `plan_proposed`, `materialising` and `materialise_escalated` with their next action (FR-11 AC1);
- `plans` table updates in `internal/projection`.

Drop the "(v0.2)" mark from `plan` in `help`.

**Depends on:** T6

**Acceptance criteria:**
- FR-17 AC9: abandoning a plan in `materialise_escalated` records it as abandoned, creates no more items, and `agent-loop mint` is then allowed. Abandoning a plan in any other state is refused with a reason.
- FR-17 AC10: the tracking item closes once all its task items are closed, and not before.
- FR-11 AC1: the board shows minting and materialise in progress, and planning escalations.
- FR-11 AC2: each planning stage start, result, retry and escalation is recorded and viewable.

### T8: Add agent-run slots

Extend `ProjectWorkflow`'s admission Update (`internal/workflows/project.go`, `wire.go`) with `wait_slot`. A project runs at most `limits.agent_runs` agent stages at once (default 2), and the deployment at most `[limits] agent_runs` (default 4, D7), held by `DeploymentWorkflow`. Details:

- Slots are released when the stage activity ends, including on cancellation and crash.
- A waiting stage stays in its state and keeps its WIP slot. Waiting items are granted in pick-priority order.
- Mint runs take a slot but no WIP.
- Add the deployment config key to `internal/profile` and TECHNICAL-SPEC §13, and show slot waits on the board.

**Depends on:** T5

**Acceptance criteria:**
- FR-16 AC2: with a project limit of 2, a third agent stage waits in its state and starts when a slot frees. With a deployment limit of 3 across two projects' workflows, the fourth waits.
- FR-16 AC2: a mint run counts toward the slot limit and not toward WIP.
- A slot is released when a stage is cancelled, times out or loses its worker (no leak after a crash rerun).
- NFR-6 AC1: the board shows "waiting for an agent-run slot" for a waiting stage.
- Survives `ProjectWorkflow` and `DeploymentWorkflow` continue-as-new (replay test).

### T9: Name hosts, track their health, and show capable-host waits

Add `--name` to `agent-loop worker` (default the OS host name, D3) in `cmd/agent-loop/worker.go`. Each worker:

- records its name, capabilities and a heartbeat in the `hosts` table;
- records the name on its stage runs.

Work out host health as in D4. Detect a stage waiting for a capable host as in D8, and show it on the board with the capability set. Add a hosts section to `agent-loop board` with name, capabilities, health and last seen (FR-11 AC4). Check that a profile's stage capabilities map to queues that workers with matching capability lists poll (FR-14 AC1, AC2).

**Depends on:** —

**Acceptance criteria:**
- FR-14 AC1: a second worker with `--name h2 --capabilities agent,x` joins with no workflow change and appears on the board.
- FR-14 AC2: a stage that requires `x` runs only on the worker with `x`. While that worker is down, the stage waits in its state and the board names the missing capability. It starts when the worker returns, with no attempt used.
- FR-14 AC3: moving a project's work from one worker to another needs no state migration (stop one worker, start another with the same capabilities, and the item continues).
- FR-11 AC4: the board lists hosts with capabilities and health. A host that hasn't polled for over a minute shows unhealthy.

### T10: Build and publish the base image

Add a base image (D9) under `images/base/`:
- a Dockerfile with git, Node, the agent CLIs (Claude Code) and a non-root user;
- a GitHub Actions workflow that builds it for linux/amd64 and linux/arm64 on a `v*` tag and pushes `ghcr.io/mayusuk/agent-loop-base:<version>`, with actions pinned to SHAs.

Add an `image` key for stages with `runner = "docker"` to `internal/profile` and TECHNICAL-SPEC §13. It is required for docker stages and rejected for others by `project validate`. Document how a project extends the image in GETTING-STARTED.

**Depends on:** —

**Acceptance criteria:**
- T10: `docker build images/base` succeeds in a live check (`AGENT_LOOP_LIVE=1`). The workflow file passes `actionlint`, or a test that checks pinned SHAs and the tag trigger.
- FR-1 AC2: `project validate` rejects a docker stage with no `image`, and an `image` on a local stage.
- The image runs as a non-root user and holds no credentials (a test reads the Dockerfile).

### T11: Add the Docker runner

Add `internal/runners/docker.go`, implementing `Runner` for `runner = "docker"` (TECHNICAL-SPEC §7.2). It runs one container per run from the stage's `image`:

- **Mounts:** only the worktree and the input and result directory.
- **Environment:** the same reserved variables, coder git identity and cache variables as the local runner, with caches inside the container.
- **Credentials:** delivered as §7.3 says. No secret is put in the container's env or image.
- **Network:** a per-run Docker network that allows only the egress allowlist. Skip-permissions is allowed for agents only here.
- **Ending a run:** on timeout or cancellation, kill and remove the container; remove the container and network after every run.
- **Worker:** advertises `docker` when the Docker daemon is reachable.

Unit tests use a fake `docker` executable. Real-container tests run only with `AGENT_LOOP_LIVE=1`.

**Depends on:** T10

**Acceptance criteria:**
- T8: a docker run mounts only the worktree and the input and result directory, checked from the arguments the fake `docker` receives.
- T9: egress outside the allowlist fails in a live run, and the allowlist hosts are reachable.
- NFR-7 AC1: a handler that never exits is stopped at the time limit, with its container removed, and the item is escalated.
- NFR-4: no secret appears in the container's env, arguments or logs (redaction test).
- FR-14 AC2: a docker stage runs only on a worker that advertises `docker`.

### T12: Prove scenario §11.2 end to end

Extend the scenario tests (`internal/workflows/scenario_test.go` and its harness) with PRODUCT-SPEC §11.2 steps 10 and 11 on the fake adapters. Use two workers, one with capability `x`, and a scripted fake mint handler (D2). Keep all §11.1 steps passing in the same run.

Extend the live harness (`cmd/agent-loop/scenario_live_test.go`, `AGENT_LOOP_LIVE=1`) with the same steps. The live mint step is skipped, with a message, until a starter mint handler exists.

Update GETTING-STARTED (mint, plan abandon, several workers, docker runner), and TECHNICAL-SPEC §15 so the v0.2 row matches what shipped.

**Depends on:** T7, T8, T9, T11

**Acceptance criteria:**
- PRODUCT-SPEC §11.2 step 10 passes on the fake adapters:
  - minting while E is escalated;
  - no items before the owner merges;
  - a worker kill during materialise with no duplicate;
  - an invalid second plan creating nothing and escalating;
  - an owner-merged fix that restarts materialise and creates the items once.
- PRODUCT-SPEC §11.2 step 11 passes: the stage that needs `x` runs only on that worker, and while it is down the stage waits and the board shows why.
- §11.1 steps 1–9 still pass in the same suite.
- GETTING-STARTED and TECHNICAL-SPEC §15 describe the shipped v0.2.

### T13: Adopt the minting layout in this repository

Make agent-loop's own repository ready to mint its next milestone with agent-loop (dogfooding, D11):

- Add `agent-loop plan validate <dir>` (D11) in `cmd/agent-loop`, using T1's `ValidateTasks` and the profile's types, and add it to TECHNICAL-SPEC §11.
- Add `.agent-loop.toml` to this repository with:
  - `source_of_truth = ["specs/PRODUCT-SPEC.md", "specs/INTEGRATION-SPEC.md", "specs/TECHNICAL-SPEC.md"]`;
  - `plans_dir = "plans/"`;
  - the starter handlers for each stage, including `mint`.

  Leave out identity secrets; they come from `agent-loop init`. `project validate` must accept it with a fake tracker in a test.
- Create `plans/README.md` describing the layout (`plans/<key>/plan.md`, `tasks.yaml`, `tasks/<task>.md`, INTEGRATION-SPEC §7), and keep `docs/plans/` for the plans made before this layout, which are not converted.
- Add a CI step (in `.github/workflows/ci.yml` and `.wf.toml` `[[verify]]`, in the same order) that runs `agent-loop plan validate` on every `plans/*/` directory, skipped when there are none.
- Update CLAUDE.md's "How work is organised" to say that new milestones are minted into `plans/`.

**Depends on:** T1

**Acceptance criteria:**
- FR-17 AC6: `agent-loop plan validate` on a valid plan exits 0. On each INTEGRATION-SPEC §7 invalid case it exits 1 and names the problem.
- FR-9 AC2: the repository's profile lists the specs in PRODUCT → INTEGRATION → TECHNICAL order as `source_of_truth`, and `project validate` accepts it (test with fake adapters).
- CI and `.wf.toml` run the plan check in the same position, and it passes on a sample plan under `plans/`.
- CLAUDE.md and `plans/README.md` describe the layout.
