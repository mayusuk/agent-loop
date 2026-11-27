# Live checks

PRODUCT-SPEC §11.1 runs twice: against the fakes in CI (`TestScenario11_1_Step1_...` to `Step9_...` in `internal/workflows`), and on a real GitHub test repository. This page is the setup for the real runs. Every live test is skipped unless `AGENT_LOOP_LIVE=1`, so CI never needs credentials. None of them may point at a repository anyone uses.

There are three live tiers. Run them in this order, since each needs what the one before proves.

| Tier | Test | Needs |
| --- | --- | --- |
| Temporal | `TestLiveTemporal_DevServer`, `TestLiveTemporal_ComposeStack` (`internal/workflows`) | Network for the dev server download, or Docker for Compose |
| GitHub adapter | `TestLiveConformance` (`internal/adapters/github`), C1–C21 | A sandbox repository, three apps, three accounts |
| Scenario | `TestScenario11_1_Live`, `TestScenario11_1_Live_Step9` (`cmd/agent-loop`) | A sandbox set up with `agent-loop init`, a running server, the scripted handler |

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

The live run takes every §11.1 step, with the same scripts as the fake run (work plan #25 D9). The test builds `agent-loop` and runs `agent-loop worker` itself, so that step 4 can kill it. It acts as the owner and as HUMAN, and reads the repository with the loop's own adapters and the board through the server's API socket. Three things make each step's events happen when the step says, rather than when an agent or CI happens to produce them:

- **Scripted handlers.** Every stage runs `scripthandler` (`internal/scripthandler`), which hands each run to the test over a Unix socket. The test answers with what the step needs: a commit pushed with the run's own credentials, the review's findings and rulings, the address run's replies. So B's round 1 has its score-4 finding and C reaches the round limit every time.
- **The gate.** The test reports the profile's one gate (a commit status) on every scenario head: `pending` while a step holds the item, `success` otherwise. That is how "before it merges" happens: the owner acts while the gate is pending, then the test passes it. No GitHub Actions workflow may report that status.
- **The owner's and HUMAN's actions.** The test makes them with their tokens: review comments, thread resolutions, pushes, label changes, closes, and on step 6 the loop-branch ruleset change. In step 1 the address run's own attempt to resolve a thread is made with a coder-app token the test issues.

Setup:

1. Use the sandbox from tier 2. Its `main` requires the status check named by the profile's gate (for example `ci`). Remove any workflow that reports it.
2. Run `agent-loop init` on it and keep the default labels, `wip`, `max_rounds`, `resume_rounds` and `blocking_threshold` (2, 3, 1, 4). Make sure no other ready issue is open.
3. Point every stage at the scripted handler, with paths short enough for a Unix socket (under about 100 characters). The test builds `scripthandler` at the first path and listens on the second:

   ```toml
   [stages.implement]
   command = ["/tmp/al/scripthandler", "/tmp/al/h.sock"]
   runner = "local-unsandboxed"
   # the same for [stages.review] and [stages.address]
   ```

   `local-unsandboxed` is needed because the sandbox wraps only `agent-loop-claude`; the scripted handler runs no agent.
4. For `TestScenario11_1_Live` (steps 1–8), leave `limits.daily_agent_runs` unset or at least 100.
5. Run `agent-loop server init` and `agent-loop server start`. Don't start a worker: the test runs its own.
6. Optional, for NFR-4 AC2: turn on "Block all incoming connections" in the macOS firewall (System Settings → Network → Firewall → Options). agent-loop polls GitHub, its API is a Unix socket, and Temporal listens on 127.0.0.1, so the scenario must pass this way.

| Variable | Value |
| --- | --- |
| `AGENT_LOOP_LIVE_CONFIG` | The deployment config's path |
| `AGENT_LOOP_LIVE_HANDLER`, `AGENT_LOOP_LIVE_HANDLER_SOCKET` | The two paths in the stage commands |
| `AGENT_LOOP_GITHUB_OWNER_TOKEN` | A token of the profile's first owner account |
| `AGENT_LOOP_GITHUB_HUMAN_TOKEN`, `_LOGIN`, `_ID` | The other human's token, login and numeric account ID |
| `AGENT_LOOP_GITHUB_API` | As in tier 2 |
| `AGENT_LOOP_LIVE_STEP_TIMEOUT` | How long one wait in a step may take, default `1h` |
| `AGENT_LOOP_LIVE_SERVER_LOG` | The server's log file, if you redirect it, so the credential check reads it too |

```sh
AGENT_LOOP_LIVE=1 go test -count=1 -timeout 12h -run 'TestScenario11_1_Live$' -v ./cmd/agent-loop/
```

The steps run in order, 1 to 8, each with its own items, as each fake step is its own run. A step closes whatever it leaves open, so the next one starts with both WIP slots. A step that can't complete fails; none is skipped. What differs from the fake run:

- **Step 4.** The worker is killed while B's review is in its handler (on the fake, after the review was posted), and restarted 30 seconds later. The test checks for one round-1 review, the rerun, and a `worker_lost` run.
- **Step 6.** The loop-branch ruleset refuses HUMAN's push, and the test checks that it does. It then disables and re-enables the ruleset, which makes D's commits untrusted (D40), and does it again for the second escalation. This is the one difference §11.1 itself names.
- **Notifications.** Step 3's desktop notification is checked as the escalation decision's `notify` action in the decision log; the deployment's notifiers deliver it.
- **Step 8.** Every board read of the whole run must show a state and a next action or a wait for each item. Every log of the test's worker, the server's log if given, every run log under the workspace root's `results/` (but not the handlers' own `out/` files), and every text the worker posted on the scenario's issues and pull requests must hold no token of the test accounts, no run's `GH_TOKEN` (which the handler echoes into its output), and nothing shaped like a GitHub token or a private key.

Step 9 needs a small daily limit, which would stop steps 1–8, so it is its own test with its own profile settings:

1. Set `limits.daily_agent_runs = 4` and a `timezone` in which 00:00 comes between 20 minutes and the step timeout after you start (for example, at 22:30 UTC, `timezone = "Etc/GMT-1"` puts 00:00 at 23:00 UTC). Restart the server so it loads the profile.
2. Run:

   ```sh
   AGENT_LOOP_LIVE=1 go test -count=1 -timeout 4h -run 'TestScenario11_1_Live_Step9' -v ./cmd/agent-loop/
   ```

The test checks, as the fake run does, that the limit is reached after four agent runs with one of X and Y waiting for it, that Z isn't picked, and that after 00:00 all three continue on their own and merge, Z's implement starting after 00:00. It then runs the credential check.

Afterwards, close any items and pull requests a failed run left, delete the `agent/` branches, and put the profile's stages back.

### Polling budget (TECHNICAL-SPEC Q4)

`TestQ4_PollingBudget` (`internal/adapters/github`) measures the calls against the GitHub stub. With two claimed items, both merging on a pending gate at the 60-second poll, an hour costs about 120 counted REST calls and 180 GraphQL queries. On the live run, read each installation's `GET /rate_limit` (`core` and `graphql`) before and after an idle hour with two items claimed, and record the GraphQL points used. The test fails if the measured cost passes half of GitHub's 5,000 an hour.
