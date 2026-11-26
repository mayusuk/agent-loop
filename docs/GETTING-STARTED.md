# Getting started with agent-loop

> **Status: draft.** This guide describes how onboarding is designed to work (`specs/INTEGRATION-SPEC.md` §12). The commands don't exist yet. It will be checked against the real tools before v0.1 ships.

agent-loop runs your project's development loop with coding agents. It picks issues labelled `ready`, has one agent implement them and another review them, answers the review, and merges once the review is clean and your checks pass. When it needs you, it stops and asks.

This guide takes a GitHub repository to its first merged pull request in about 30 minutes, without writing any code.

## 1. What you need

- macOS or Linux.
- Admin rights on the GitHub repository. Private repositories need a GitHub plan that supports rulesets.
- Docker, to run Temporal, the workflow engine agent-loop uses.
- The [Claude Code](https://docs.anthropic.com/claude-code) CLI, logged in (`claude` works in your terminal). This is only needed for the starter handlers.
- At least one required check on your default branch, for example your CI tests. agent-loop never merges past a failing check.

## 2. Install

With [Homebrew](https://brew.sh), on macOS or Linux:

```bash
brew install mayusuk/tap/agent-loop
agent-loop sign                  # macOS: after installing and after every brew upgrade; see "Keychain trust" below
agent-loop version               # checks that your shell finds it
```

The starter handlers don't have a release yet. Install them from [`mayusuk/agent-loop-handlers`](https://github.com/mayusuk/agent-loop-handlers): run `go install ./cmd/agent-loop-claude` in its checkout (you need Go 1.25 or later), and make sure `$(go env GOPATH)/bin` is on your `PATH`, so `agent-loop-claude` is found. Then:

```bash
agent-loop server init           # writes ~/.config/agent-loop/deployment.toml and starts Temporal in Docker
```

### From source

Instead of Homebrew, you can build from a checkout. You need Go 1.25 or later.

```bash
git clone https://github.com/mayusuk/agent-loop.git
cd agent-loop
go install ./cmd/agent-loop                # installs into $(go env GOPATH)/bin
export PATH="$(go env GOPATH)/bin:$PATH"   # add this line to ~/.zshrc or ~/.bashrc too
agent-loop version                         # checks that your shell finds it
agent-loop sign                            # macOS: do this after every build
```

Install the starter handlers the same way, as above. Then run `agent-loop server init`.

### Keychain trust (macOS)

agent-loop keeps the GitHub Apps' keys in your macOS Keychain. Each item lets only agent-loop read it, so another program that asks gets a password prompt, not the key. macOS recognises agent-loop by its code signature. `go build` signs each build with its own hash, so a rebuild or an upgrade would be a different program, and every read would ask for your password. An unattended loop can't click **Allow**.

`agent-loop sign` fixes this. It signs the agent-loop binary with a certificate called `agent-loop code signing` and the identifier `dev.agent-loop`. The Keychain trusts every build signed that way, so an upgrade keeps working without a prompt.

- **The first time**, `agent-loop sign` creates the certificate in your login keychain. It's self-signed and stays on your Mac. macOS may ask once whether `codesign` may use its key: click **Always Allow**.
- **After every build or upgrade**, run `agent-loop sign` again, including after `brew upgrade` (Homebrew reminds you; it can't sign for you, because its install steps can't reach your login keychain). An unsigned build still runs, but `agent-loop init`, `server start` and `worker` warn about it, and once your keys are stored, `server start` and `worker` stop on an unsigned upgrade and tell you to run `agent-loop sign`.
- **If items trust a different build** (for example, keys stored before you first signed, or a build you forgot to sign), `server start` and `worker` stop and name the items, rather than wait for a prompt. `agent-loop init --check` reports them too. Run `agent-loop sign`, then `agent-loop init` in your project. It asks once, then macOS asks once for each item: enter your login password and click **Allow**. After that, init rewrites the items for the new build, and you won't be asked again.
- **If your keychain is locked** (for example, after a restart with no one logged in), `server start` and `worker` stop with "the keychain is locked". Unlock it with `security unlock-keychain` or by logging in, then start them again. `agent-loop init` lets macOS ask you to unlock it.

If you'd rather not use the Keychain, set `secrets.backend = "envfile"` in the deployment config.

## 3. Set up your project

In your repository, run:

```bash
cd ~/code/my-project
agent-loop init
```

`init` asks before each change. Here's what it does:

1. **Checks** that you're an admin on the repository and that it supports rulesets.
2. **Creates three GitHub Apps**: `my-project-coder` (writes code), `my-project-reviewer` (reviews, can't push) and `my-project-loop` (labels and merges). Your browser opens once for each app, and you click **Create**. Their keys go into your macOS Keychain.
3. **Writes `.agent-loop.toml`** with sensible defaults, and you as the owner, and registers the project in `~/.config/agent-loop/deployment.toml`.
4. **Creates labels**: `ready`, `in-progress`, `needs-owner`, `blocked`, `paused`, `aborted`, `follow-up`, `feature` and `bug`.
5. **Protects branches**: only the coder app and you can push to `agent/*`, only the loop app and you can update your default branch (so the agents can't push to it or merge into it), and only you can create tags.
6. **Chooses handlers.** Accept the starter handlers (Claude Code) for now.
7. **Shows your required checks**, and warns if there are none.
8. **Validates** everything and lists anything left to fix.

`init` doesn't touch your issues or pull requests, and doesn't commit anything. Review the profile, then commit it:

```bash
git add .agent-loop.toml && git commit -m "Add agent-loop profile" && git push
```

### Issues created by other tools

Some tools, such as the wf `plan-issues` skill, mark ready issues with the label `status:ready` and write dependencies as "Blocked by #n" lines in the issue text. agent-loop reads neither by default:

- **The ready label.** If your repository has a `status:ready` label and no `ready` label, `init` offers to map the ready marker to `status:ready` (`--yes` takes the offer). With an existing profile, set it yourself in `.agent-loop.toml`: `[markers]` `ready = { label = "status:ready" }`.
- **Dependencies.** agent-loop reads only GitHub's native "blocked by" relationship, never the text. `agent-loop project validate` warns about each ready issue whose text says "Blocked by #n" without one, and prints the command that adds it:

  ```bash
  gh api -X POST repos/OWNER/REPO/issues/5/dependencies/blocked_by \
    -F issue_id=$(gh api repos/OWNER/REPO/issues/3 --jq .id)   # #5 is blocked by #3
  ```

  Or open the issue on GitHub and add the blocker under **Relationships**, **Mark as blocked by**.

## 4. See what it would do

```bash
agent-loop run --dry-run my-project
```

This changes nothing, and doesn't need the server running. It lists the issues the loop would pick and in what order, why it would skip the others, and what it would write to GitHub.

## 5. Start the loop

```bash
agent-loop server start
agent-loop worker --capabilities agent
```

Pick a small, well-described issue, and give it the labels `ready` and `feature` (or `bug`). Within a minute it gets `in-progress`. Then watch:

```bash
agent-loop board
```

You'll see the item move from `implementing` to `in_review`, possibly to `addressing` and back, then to `merging` and `merged`. The pull request shows the reviewer's findings, the coder's replies, and a status table in every review.

## 6. When it needs you

The loop never waits silently. When it can't decide something, it adds `needs-owner` to the issue, comments with what it needs, and sends you a desktop notification. Common reasons:

| Reason | What to do |
|---|---|
| The round limit was reached with a blocking finding | Read the findings. Push a fix or explain, then remove `needs-owner` to allow one more round |
| A check failed on the reviewed commit | Fix the check or the code, then remove `needs-owner` |
| The agent asked a question | Answer it in the issue, then remove `needs-owner` |
| A commit came from outside the loop | Check the commit. Removing `needs-owner` approves it |

You can also leave review comments on the pull request yourself. Each one blocks the merge until it's answered and the reviewer accepts the answer, or until you resolve it.

Other controls: `agent-loop item pause|resume|abort <issue>`, `agent-loop project pause|resume`, or the `paused` and `aborted` labels.

## 7. Make it yours

You don't have to do any of this. Each step is optional:

| Level | Change | How |
|---|---|---|
| 1. Your prompts | How the agents implement and review | Add `.agent-loop/prompts/implement.md` (or `review.md`, `address.md`, `mint.md`) to your repository, or point a stage at a skill: `command = ["agent-loop-claude", "review", "--skill", "review-pr"]` |
| 1. Your limits | Rounds, WIP, daily runs, merge method | Edit `.agent-loop.toml` (`specs/INTEGRATION-SPEC.md` §13) |
| 2. Your own agent | Any program, any language, any model | A command that reads `input.json` and writes `result.json` (`specs/INTEGRATION-SPEC.md` §3–§5) |
| 3. Another tracker | Jira, Linear, GitLab | A Go adapter in agent-loop that passes the conformance suite (`specs/INTEGRATION-SPEC.md` §8, §11) |

## 8. Troubleshooting

| Symptom | Check |
|---|---|
| Nothing gets picked | Run `agent-loop run --dry-run`: it gives the rule that excludes each issue |
| `init` stops at rulesets | The repository's plan doesn't support rulesets on private repositories. agent-loop needs them to keep outside commits off the agent branches |
| A stage keeps failing | `agent-loop log <issue>` shows each run's log and result |
| Stuck in `merging` | A required check hasn't reported on the head commit. `agent-loop board` shows which one |
| Every change escalates as untrusted, or `project validate` reports `ruleset_bypass_unverified` | A ruleset changed since your login last read its bypass list, which the loop app can't see. Run `agent-loop project validate --owner-login <project>` (or `agent-loop init`) to check and record it, then resume the escalated issues |
| macOS asks for your password to let agent-loop use a key, or `server start` says the Keychain items trust a different build | The build isn't signed, or the items trust an older one. Run `agent-loop sign`, then `agent-loop init` (see "Keychain trust" in §2) |
| Something drifted (labels, apps, ruleset) | `agent-loop init --check` reports it, and `agent-loop init` repairs it |
