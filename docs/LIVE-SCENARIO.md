# Live checks

PRODUCT-SPEC §11.1 runs twice: against the fakes in CI (`TestScenario11_1_Step1_...` to `Step9_...` in `internal/workflows`), and on a real GitHub test repository. This page is the setup for the real runs. Every live test is skipped unless `AGENT_LOOP_LIVE=1`, so CI never needs credentials. None of them may point at a repository anyone uses.

There are three live tiers. Run them in this order, since each needs what the one before proves.

| Tier | Test | Needs |
| --- | --- | --- |
| Temporal | `TestLiveTemporal_DevServer`, `TestLiveTemporal_ComposeStack` (`internal/workflows`) | Network for the dev server download, or Docker for Compose |
| GitHub adapter | `TestLiveConformance` (`internal/adapters/github`), C1–C16 | A sandbox repository, three apps, three accounts |
| Scenario | `TestScenario11_1_Live` (`cmd/agent-loop`) | A sandbox set up with `agent-loop init`, a running server, the starter handlers |

## 1. Temporal

```sh
AGENT_LOOP_LIVE=1 go test -count=1 -run TestLiveTemporal_DevServer -v ./internal/workflows/
```

This starts a Temporal dev server (the SDK downloads the `temporal` CLI on first use) with worker versioning turned on, as the Compose stack's `dynamicconfig.yaml` does. It runs one item from claim to merge on the fake adapter, with versioned workers, the deployment's current version set, and the worker stopped and restarted after the implement. It checks that the item merges, that no handler runs twice, and that the item workflow is pinned to the build. Set `AGENT_LOOP_TEMPORAL_ADDRESS` (and `AGENT_LOOP_TEMPORAL_NAMESPACE`, default `default`) to run it against a server you started instead.

```sh
AGENT_LOOP_LIVE=1 AGENT_LOOP_COMPOSE=1 go test -count=1 -run TestLiveTemporal_ComposeStack -v ./internal/workflows/
```

This brings up the Compose file that `agent-loop server init` writes (Temporal on Postgres), under its own Compose project `agent-loop-livetest`, and runs the same check against it. It then takes the stack down and deletes its volume. It needs Docker running and port 7233 free, so stop your own stack first.

## 2. GitHub adapter conformance

The sandbox repository:

- Has a `main` branch that requires a status check named `ci`, with classic branch protection.
- Has the three rulesets `agent-loop init` creates (`agent-loop: loop branches`, `agent-loop: base branch`, `agent-loop: tags`). Run `agent-loop init` on it once.
- Has three GitHub Apps installed (coder, reviewer, loop) with the INTEGRATION-SPEC §9.1 permissions.
- Has three accounts: OWNER (an owner account, a repository admin), HUMAN (another account with write access, which can fork the repository), and BOT (a bot listed in `manual_comment_bots`).

| Variable | Value |
| --- | --- |
| `AGENT_LOOP_GITHUB_REPO` | `owner/name` of the sandbox |
| `AGENT_LOOP_GITHUB_API` | The API base, default `https://api.github.com` |
| `AGENT_LOOP_GITHUB_<ROLE>_TOKEN`, `_LOGIN` | For OWNER, HUMAN, BOT, CODER, REVIEWER and LOOP. For the apps, an installation token and `name[bot]` |
| `AGENT_LOOP_GITHUB_<APP>_APP_ID` | The app IDs of CODER, REVIEWER and LOOP, as the rulesets' bypass lists name them |

```sh
AGENT_LOOP_LIVE=1 go test -count=1 -run TestLiveConformance -v ./internal/adapters/github/
```

The trust cases C10, C13 and C15 (deferred from #16) run here too. C10 briefly makes HUMAN an admin and disables, removes and restores rulesets, so the OWNER token must be able to manage rulesets and collaborators. If a case fails because GitHub behaves differently from INTEGRATION-SPEC, file it as a `spec` issue rather than changing the adapter to match (plan D4).

## 3. The scenario

The test acts as the owner and as HUMAN. It builds `agent-loop` and runs `agent-loop worker` itself, so that step 4 can kill it. It reads the repository with the loop's own adapters and the board through the server's API socket.

Setup:

1. Use the sandbox from tier 2, with a GitHub Actions workflow that reports `ci` on every push and passes. A run of a minute or two gives the owner's actions room.
2. Run `agent-loop init` on it, with the profile's handlers set to the starter handlers (`agent-loop-claude` from `mayusuk/agent-loop-handlers`) and `wip = 2` (the default). Keep the default labels. Make sure no other ready issue is open.
3. Run `agent-loop server init` and `agent-loop server start`. Don't start a worker: the test runs its own.
4. Optional, for NFR-4 AC2: turn on "Block all incoming connections" in the macOS firewall (System Settings → Network → Firewall → Options). agent-loop polls GitHub, its API is a Unix socket, and Temporal listens on 127.0.0.1, so the scenario must pass this way.
5. Optional, for step 9: set `limits.daily_agent_runs` low (for example 6) and pass the same number in `AGENT_LOOP_LIVE_DAILY_LIMIT`.

| Variable | Value |
| --- | --- |
| `AGENT_LOOP_LIVE_CONFIG` | The deployment config's path |
| `AGENT_LOOP_GITHUB_OWNER_TOKEN` | A token of the profile's first owner account |
| `AGENT_LOOP_GITHUB_HUMAN_TOKEN`, `_LOGIN`, `_ID` | The other human's token, login and numeric account ID |
| `AGENT_LOOP_GITHUB_API` | As in tier 2 |
| `AGENT_LOOP_LIVE_STEP_TIMEOUT` | How long a step waits for the loop, default `1h` |
| `AGENT_LOOP_LIVE_SERVER_LOG` | The server's log file, if you redirect it, so step 8 checks it too |
| `AGENT_LOOP_LIVE_DAILY_LIMIT` | The profile's daily limit, to run step 9 |

```sh
AGENT_LOOP_LIVE=1 go test -count=1 -timeout 12h -run TestScenario11_1_Live -v ./cmd/agent-loop/
```

The test creates items A to D and runs the steps in the order the items reach them: 4, 1, 5, 2, 3, 6, 7, 9, 8. What differs from the fake run:

- **The owner's timing.** To make "before it merges" happen on a real repository, the owner adds `paused` to an item, acts, and removes it. A pause holds the merge but changes no other step.
- **Agent-dependent steps.** The starter handlers decide what the reviews find. A step whose start depends on that, such as a blocking finding in B's round 1 or C reaching the round limit, waits for it and is skipped, with the reason, if the agents don't produce it in time. A pass means every step that ran passed. Read the skips: step 2 and step 3 need the agents' help, and the fake run proves them in full.
- **Step 6.** The loop-branch ruleset refuses HUMAN's push, and the test checks that it does. It then disables and re-enables the ruleset, which makes D's commits untrusted (D40).
- **Step 9.** The test checks that the project reaches its limit and counts no more runs than the limit. The waiting items continue after 00:00 in the profile's time zone. Leave the stack running and check the board the next morning.
- **Step 8.** Every board read during the run must show a state and a next action or a wait for each item. The worker's log, the server's log if given, and every run log under the workspace root's `results/` (but not the handlers' own `out/` files) must hold no token of the test accounts and nothing shaped like a GitHub token or a private key.

Afterwards, close the items and pull requests the run left, and delete the `agent/` branches.

### Polling budget (TECHNICAL-SPEC Q4)

`TestQ4_PollingBudget` (`internal/adapters/github`) measures the calls against the GitHub stub. With two claimed items, both merging on a pending gate at the 60-second poll, an hour costs about 120 counted REST calls and 180 GraphQL queries. On the live run, read each installation's `GET /rate_limit` (`core` and `graphql`) before and after an idle hour with two items claimed, and record the GraphQL points used. The test fails if the measured cost passes half of GitHub's 5,000 an hour.
