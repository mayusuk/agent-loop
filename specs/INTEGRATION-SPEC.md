# Integration Spec: agent-loop

| | |
|---|---|
| **Status** | Draft, 2026-11-14. Not yet reviewed |
| **Implements** | `PRODUCT-SPEC.md` (approved), `TECHNICAL-SPEC.md` (draft, not yet reviewed) |
| **Contract version** | `1.0` |

## 1. Purpose and scope

This spec defines every contract between the agent-loop core and the things that plug into it:

1. **Stage handler contract** (§3–§5): how a handler is started, what it receives, and what it returns. Project authors implement this.
2. **Tracker records** (§6): the structured records the loop writes to and reads from the tracker and code host, and which identity's records count.
3. **Task list** (§7): the plan file that materialise reads.
4. **Ports** (§8): the exact Go interfaces that tracker and code-host adapters implement, with their required behaviour.
5. **GitHub adapter** (§9): how each port maps onto GitHub. **Jira notes** (§10) keep the ports neutral.
6. **Conformance** (§11): the behaviours every adapter must prove.
7. **Adoption** (§12): integration levels, `agent-loop init`, `run --dry-run` and the starter handlers, so a new project can start in minutes.

If this spec seems to change product behaviour, `PRODUCT-SPEC.md` wins and this spec is wrong.

### 1.1 Decisions in this spec

| # | Decision |
|---|---|
| I1 | **The worker writes every structured record.** Handlers return content in `result.json`. The worker validates it against the schema and posts it as the stage's identity. This covers changes, reviews, findings, replies, rulings and plan proposals. Handlers push code (T18) and do nothing else that writes |
| I2 | Because of I1, the review handler gets **read-only** credentials, and the coder handler's token can push code but can't write to issues or pull requests |
| I3 | GitHub dependencies use the **native issue dependency** relationship ("blocked by"), not body text |
| I4 | A plan's task list is a separate **`tasks.yaml`** next to the plan document, validated by a JSON Schema |
| I5 | Every structured record carries a hidden **record marker** (§6.1) and counts only when its author is the identity that may write it |
| I6 | On GitHub, the **push ruleset is the push record** for FR-13 AC5 (§9.8). The loop checks that the ruleset is in force before every implement, address and merge |
| I7 | Review bots named in the profile (`manual_comment_bots`) count as manual commenters. Their comments block until cleared, like a human's, and never allow a merge (§9.6) |
| I8 | Findings with a file but no line become resolvable **file-level** threads. Only findings with no file go in the review text. Every review shows a **status table** of all findings (§6.3) |
| I9 | Any change to the push ruleset makes the open changes untrusted, and their items escalate. This strict behaviour is intended (§9.8) |
| I10 | The coder handler's `contents: write` token can push to any branch, create tags and releases, and merge pull requests on GitHub. So two more rulesets are required: one on the base branch that refuses updates from everyone except the loop app and owners, and one on tags. `init` creates them and `Trust` checks them (§9.8 step 1) |
| I11 | On GitHub, who pushed a head (coder or owner) is worked out from the loop's own run records: tokens are issued per run and revoked when it ends (§9.8 step 6) |
| I12 | Idempotent writes look for an existing record in one fixed place per record type, never through the code host's search, which lags (§6.1) |
| I13 | Owner accounts and identities are matched by the account's stable numeric ID, not by login, which can be renamed (§8.1, §13) |
| I14 | A change on a branch outside `branch_prefix` (an owner-opened change) is adopted, but the push ruleset doesn't cover it, so its commits are untrusted until the owner approves them (§6.2, §9.8) |
| I15 | A merge call refused as not mergeable is re-checked from the top of the merge-failure order before it escalates (§8.3, TECHNICAL-SPEC §8.1) |
| I16 | A finding on a line that isn't in the diff, or in a file that isn't in the diff, is posted at the nearest level GitHub accepts, with its location in the text (§5.2) |

I1 to I9 are confirmed by the owner. I10 to I16 were added by the review of 2026-11-15 to fix gaps, and were confirmed by the owner on 2026-11-15. The owner decisions from that review are listed in §14.

## 2. Versioning

- `contract_version` is `MAJOR.MINOR`. The core supports exactly one major version and every minor up to its own.
- A minor version only adds optional fields. Handlers must ignore unknown input fields. The core rejects unknown result fields, so mistakes show up early.
- A handler declares the version it was written for in its profile entry (`contract = "1.0"`). `project validate` fails when the core can't support it.
- The JSON Schemas are in `contract/v1/` in the agent-loop repository: `input.schema.json`, `result.schema.json`, `tasks.schema.json` and `records.schema.json`. This spec is the source of truth for them.

## 3. Handler invocation

### 3.1 Starting a handler

The worker starts the stage's `command` from the profile:

| Aspect | Contract |
|---|---|
| Working directory | The run's workspace: a fresh git worktree on the item's branch (§3.3) |
| `AGENT_LOOP_INPUT` | Absolute path of `input.json` (read-only) |
| `AGENT_LOOP_RESULT` | Absolute path where the handler writes `result.json` |
| `AGENT_LOOP_OUT` | A directory for files the result points to (text bodies, §5.1). Only files in this directory are read |
| `AGENT_LOOP_CONTRACT` | The contract version the worker speaks, for example `1.0` |
| Git credentials | A git credential helper is configured in the worktree. It serves the stage's push token, or nothing for read-only stages (§3.4) |
| `GH_TOKEN` (GitHub) | A **read-only** token for the stage's identity, so tools like `gh` can read issues, pull requests and comments. It is valid for at least 55 minutes. For longer runs the handler gets a fresh one from `agent-loop-token read`, which asks the worker over the same socket as the credential helper |
| stdin | Closed |
| stdout, stderr | Captured, redacted (TECHNICAL-SPEC §7.3) and stored as the run log. Never parsed |
| Exit code | Informational. The result file decides the outcome. A missing, unreadable or invalid result is `failed` (FR-18 AC3), whatever the exit code |
| Time limit | The stage's `time_limit`. When it is reached the whole process group or container is stopped (FR-18 AC4) |

The handler must run headless: it must never wait for terminal input. A handler that needs a human returns `needs_owner` (FR-10 AC4).

### 3.2 Sessions

Every run is a new process and a new agent session (NFR-8). The input never holds a session reference, a transcript, or anything from another stage's session. A handler that needs to keep something for a later stage writes it to the branch (for example `.agent-loop/notes/<item>.md`) or returns it in its result as text that the worker posts.

### 3.3 Workspace and branches

- The worker prepares a worktree from a cached clone, fetched just before the run.
- The item's branch is `<branch_prefix><item-key>`, for example `agent/42` or `agent/PROJ-42`. After the owner closes an item's change without merging, its next change gets a new branch, `<branch_prefix><item-key>-<n>`, where *n* is one more than the number of the item's changes that were closed without merging (`agent/42-2`, then `agent/42-3`, product spec D44). `branch_prefix` comes from the profile (default `agent/`). The input names the branch, so a handler never builds the name itself.
- **implement:** if the branch doesn't exist, the worktree is created on a new branch from the base tip. Otherwise it checks out the branch head (for example after a crash, before the change was opened). After the owner closed the change without merging (FR-12 AC4), implement runs on the next branch name, from the base tip, so it never builds on the rejected commits. The old branch stays as it is (product spec §8 item 3, D44).
- **review:** the worktree is checked out at `review.commit`, detached, so the handler can't push.
- **Base update (D41):** the worker first tries the merge itself, with no handler and no agent run (§5.3). Only when the merge has conflicts does it start an address run in base-update mode.
- **address, base-update mode:** the handler resolves the conflicts by **merging** the base into the branch. It never rebases, and never force-pushes (§8.3 of the product spec).
- **mint:** the worktree is on the plan branch `<branch_prefix>plan/<plan-key>`, from the base tip.
- After the run, the worker deletes the worktree. The branch stays.

### 3.4 Credentials by stage

| Stage | Identity | Push token (credential helper) | Read token (`GH_TOKEN`) | Writes to the tracker |
|---|---|---|---|---|
| implement | coder | Yes: contents write on the project repository only | Yes | None. The worker opens the change |
| address | coder | Yes | Yes | None. The worker posts replies |
| review | reviewer | **None** | Yes | None. The worker posts the review |
| mint | coder | Yes. The same coder token: it isn't limited to the plan branch, and the push ruleset covers the plan branch like every branch under the prefix | Yes | None. The worker opens the proposal |

The loop identity's credentials never reach a handler (product spec §8.8).

## 4. Input (`input.json`)

### 4.1 Common fields

```json
{
  "contract_version": "1.0",
  "run_id": "item/eagain/42#1:implement:1:a",
  "stage": "implement",
  "attempt": 1,
  "deadline": "2026-11-14T16:00:00Z",
  "identity": "coder",
  "project": { "name": "eagain", "tracker": "github", "code_host": "github" },
  "repo": { "url": "https://github.com/mayusuk/agentMaster", "base_branch": "main" },
  "workspace": { "path": "/var/agent-loop/ws/eagain-42-1", "branch": "agent/42", "head": "9f2c…" },
  "item": { "ref": "42", "url": "https://github.com/mayusuk/agentMaster/issues/42", "type": "feature" },
  "change": null
}
```

- `run_id` is unique for each handler start: workflow ID, claim number (TECHNICAL-SPEC §6.1), stage, attempt, and a start suffix. A rerun after a worker crash is a new start and gets a new `run_id`, but keeps the same `attempt`, because a crash doesn't use an attempt (FR-16 AC1, D42, TECHNICAL-SPEC §6.5). `attempt` is the counted attempt. A handler can see a rerun only by its new `run_id`, and must expect the branch to hold pushes from the interrupted run.
- `item` and `change` are **references only** (NFR-8 AC1). The handler reads the title, body and comments itself, and treats them as data (product spec §8.5). The input never copies tracker text.
- `change` (when there is one): `{ "ref": "57", "url": "…", "head": "…", "base_tip": "…" }`.

### 4.2 Stage fields

| Stage | Extra fields |
|---|---|
| implement | None. An existing open change is adopted, and implement isn't run (FR-3 AC3) |
| review | `review`: `{ "kind": "round" \| "base_update" \| "manual_reply" \| "owner_push", "commit": "<sha>", "round": 2 \| null, "blocking_threshold": 4, "rule_findings": ["F3","F5"], "rule_manual": ["MC-981"], "resolved_paths": ["api/login.go"] }`. `resolved_paths` is set only for `base_update` (§5.2, address) |
| address | `address`: `{ "mode": "findings" \| "manual" \| "base_update", "answer_findings": ["F3"], "answer_manual": ["MC-981"] }` |
| mint | `mint`: `{ "plan_key": "mint-7", "source_of_truth": ["specs/001.md","specs/002.md","specs/003.md"], "plans_dir": "plans/" }`. `plan_key` is provisional (§5.2, mint) |

- `rule_findings` lists every finding that is unresolved before this review (FR-4 AC4). `rule_manual` lists every manual comment that an address run replied to and no review has ruled on yet (FR-4 AC8).
- `answer_findings` lists every unresolved finding. `answer_manual` lists every manual comment that isn't cleared, except empty "changes requested" reviews, which have nothing to answer (FR-5 AC1, §9.6). Both are empty in `base_update` mode (FR-5 AC6). The address handler sees `base_update` mode only when the base merge has conflicts (§5.3).
- IDs refer to records the handler can read: finding IDs in reviewer records (§6.3), and manual comment IDs in the code host (`MC-<id>` is the code host's comment or review ID).

## 5. Result (`result.json`)

### 5.1 Shape

```json
{ "contract_version": "1.0", "run_id": "…", "result": "done", "done": { … } }
```

`result` is one of `done`, `complete`, `blocked`, `failed` or `needs_owner`. Only the object named by `result` may be present, plus the optional `usage` object. The results allowed for each stage are in FR-18 AC1.

`usage` (optional, any result) reports what the run spent, for FR-16 AC3's spend limit: `{ "cost": 1.84, "currency": "USD", "input_tokens": 120000, "output_tokens": 9000 }`. Every field is optional. The loop adds `cost` to the project's daily spend when the profile sets `limits.daily_spend` (TECHNICAL-SPEC §13). A run with no `usage` adds nothing, so a spend limit only holds for handlers that report it (FR-16 AC3: "when handlers report it").

**Text fields** (bodies, replies, questions) are given as `*_file` paths relative to `AGENT_LOOP_OUT`, never inline. Each file is UTF-8 markdown up to 64 KiB. The worker redacts it, adds the record marker (§6.1) and posts it. A file outside `AGENT_LOOP_OUT`, or one that is too big, makes the result invalid.

| Result | Object | Fields |
|---|---|---|
| `blocked` | `blocked` | `reason_file`, optional `dependency` (an item ref) |
| `failed` | `failed` | `reason` (one line, at most 500 characters, stored in the log only) |
| `needs_owner` | `needs_owner` | `question_file` |
| `complete` | `complete` | none (mint only) |

### 5.2 `done` per stage

**implement**

```json
"done": {
  "head": "<sha pushed to the item's branch>",
  "change": { "title": "Add login endpoint", "body_file": "change.md" }
}
```

The worker checks that `head` is the remote head of the item's branch. It then opens the change as the coder identity, from the branch to the base, linked to the item (§6.2). If a change already exists for the branch, it adopts it instead of opening a second one.

**review**

```json
"done": {
  "commit": "<sha reviewed, must equal input review.commit>",
  "kind": "round",
  "summary_file": "summary.md",
  "rulings": [
    { "finding": "F3", "status": "addressed" },
    { "finding": "F5", "status": "partially_addressed", "note_file": "f5.md" }
  ],
  "manual_rulings": [ { "comment": "MC-981", "ruling": "open", "note_file": "mc981.md" } ],
  "new_findings": [
    { "score": 4, "path": "api/login.go", "line": 88, "title": "Password compared in non-constant time", "body_file": "n1.md" },
    { "score": 2, "path": null, "line": null, "title": "Missing changelog entry", "body_file": "n2.md" }
  ]
}
```

The worker checks these rules. Breaking any of them makes the result `failed`:

- `commit` and `kind` equal the input.
- `rulings` has exactly one entry for each ID in `rule_findings` (FR-4 AC4), and `manual_rulings` has exactly one for each ID in `rule_manual` (FR-4 AC8).
- `status` is one of `open`, `partially_addressed`, `addressed` or `withdrawn`. `ruling` is `addressed` or `open`.
- `score` is an integer from 1 to 5. A `path` must exist at `commit`, and a `line` must be in the file. `line` needs a `path`.
- How each new finding is posted (I8, I16): with `path` and a `line` that is in the change's diff, as an inline comment. With a `path` that is in the diff but no line, or a line outside the diff, as a file-level comment whose text starts with `path:line`. With a `path` that isn't in the diff, or neither, as a section of the review text that starts with the location. GitHub accepts review comments only on files and lines in the diff, so this is the nearest thread it allows. Handlers should give a `path` whenever the finding is about a file.

The worker gives each new finding the next ID on the change (`F1`, `F2`, … in array order). It then posts one review as the reviewer identity (§6.3). The review event is `APPROVE` when branch protection requires an approval and no unresolved finding is at or above the threshold (FR-4 AC7). Otherwise it is `COMMENT`. The handler never chooses the event.

**address**

```json
"done": {
  "head": "<sha, same as input head if nothing was pushed>",
  "replies": [
    { "finding": "F3", "kind": "fixed", "body_file": "r3.md" },
    { "finding": "F5", "kind": "deferred", "deferred_to": "61", "body_file": "r5.md" }
  ],
  "manual_replies": [ { "comment": "MC-981", "kind": "explained", "body_file": "m981.md" } ]
}
```

- `kind` is one of `fixed`, `explained`, `deferred`, `not_valid` or `needs_info` (FR-5 AC1). `deferred` needs `deferred_to`, an item that exists and is open (FR-5 AC5). Otherwise that reply doesn't count as a reply.
- The worker applies FR-5 AC2: the head moved or every finding in `answer_findings` has a valid reply, and every comment in `answer_manual` has a reply. Otherwise the result is `failed`.
- In `base_update` mode, `replies` and `manual_replies` must be empty, and `head` must be **one new merge commit** whose first parent is the input head and whose second parent is a base commit that contains the input `base_tip` (FR-5 AC6). Anything else (several commits, a rebase, a commit with one parent) makes the result `failed`. The worker can't check conflict resolutions mechanically. Instead it merges the two parents itself without a working tree (`git merge-tree`). The paths that conflicted, and any path whose content differs from the clean parts of that merge, are passed to the `base_update` review as `review.resolved_paths`, and that review checks them (FR-6 AC6).
- The worker posts each reply as the coder identity in the finding's or comment's thread (§6.4).

**mint**

```json
"done": {
  "plan_key": "003-auth",
  "head": "<sha on the plan branch>",
  "proposal": { "title": "Plan: 003 auth", "body_file": "proposal.md" }
}
```

The loop doesn't know the next plan's key before the minter decides what to plan. So the input's `plan_key` is a provisional mint ID (`mint-<n>`), and the branch is `<branch_prefix>plan/mint-<n>`. `done.plan_key` is the key the minter chose. It must match `[a-z0-9-]{1,64}`, and no `plans/<plan_key>/` may exist on the base branch. The worker checks that the plan branch holds `plans/<plan_key>/plan.md` and a valid `plans/<plan_key>/tasks.yaml` (§7), and that the branch changes nothing outside `plans/<plan_key>/`. It then opens the proposal as the coder identity, and the loop identity labels it `plan-proposal`. An invalid task list makes the result `failed`, so a bad plan is caught before the owner sees it (materialise still validates again, FR-17 AC6).

### 5.3 Base update without an agent (D41)

When an item needs a base update (TECHNICAL-SPEC §8.1 cause 3), the worker does this first, as a deterministic activity with no handler:

1. It fetches the item's branch and the base, and merges the base tip into the branch head without a working tree (`git merge-tree --write-tree`).
2. If the merge has **no conflicts**, it makes one merge commit (first parent the branch head, second parent the base tip) with the coder identity as author and committer, and pushes it to the item's branch as the coder identity. The push is a plain fast-forward. If the branch head moved in between, the push is refused, nothing is retried, and the merge conditions are checked again (cause 2). It never force-pushes.
3. If the merge has **conflicts**, it pushes nothing and starts an address run in `base_update` mode (§5.2, address). Only this case is an agent run, and only this case counts toward the daily agent-run limit (FR-16 AC3).

Either way a `base_update` review follows. After a clean merge its `resolved_paths` is empty. The push uses the **coder worker token**, which has `contents: write` for this (§9.1). The worker records the merge as a coder run for I11 (§9.8 step 6), with its own start and end, and revokes the token when it ends. The loop identity is never used for it, because the loop identity never pushes code (product spec §8 item 8).

## 6. Tracker records

### 6.1 Record marker

Every record the worker writes ends with a hidden marker:

```
<!-- agent-loop:v1 {"type":"review","item":"42","change":"57","key":"item/eagain/42#1:review:r2:1", ...} -->
```

- The marker is one line of compact JSON in an HTML comment. It is invisible in rendered markdown.
- `key` is the idempotency key (TECHNICAL-SPEC §9). Before writing, the adapter looks for a record with the same `key` from the same author in the place the table's last column names, and returns it if it exists (I12). It never uses the code host's search API, whose index lags behind writes, so a retry right after a crash could miss the first write.
- **A record counts only if its author is the identity allowed to write that type** (table below). A marker in text from anyone else is ignored and logged. This is what keeps copied or forged markers from acting as records (§8.5 of the product spec).

| `type` | Written as | Where (GitHub) | Purpose | Looked up before writing in |
|---|---|---|---|---|
| `change` | coder | Change body | Links the change to its item (§6.2) | Open and closed pull requests whose head is the item's branch |
| `review` | reviewer | Review body | Structured review result (§6.3) | The change's reviews |
| `finding` | reviewer | Inline review comment, file-level review comment, or a review-text section for findings with no file (I8) | One finding | Posted with its review, so found with it |
| `reply` | coder | Reply in the finding's thread | Coder reply to a finding (§6.4) | The thread's comments, or the change's comments for a finding with no thread |
| `manual_reply` | coder | Reply in the manual comment's thread, or a change comment that links the review | Coder reply to a manual comment | The same |
| `status` | loop | Item comment | Claim, escalation, resume, abort and manual-action records | The item's comments |
| `approved_commits` | loop | Item comment | Commits the owner approved on resume (FR-13 AC6) | The item's comments |
| `followup` | loop | Follow-up item body | FR-8 | Issues created by the loop app since the change merged (`GET /issues?creator=<loop app>&state=all&since=<merge time>`), then their bodies |
| `task` | loop | Created item body | Plan and task key (FR-17 AC3) | Issues created by the loop app since the plan was merged, as for `followup` |
| `plan` | loop | Tracking item body | Plan key and task item refs (FR-17 AC2) | As for `task` |
| `proposal` | coder | Plan proposal body | Plan key | Pull requests whose head is the plan branch |

The issue list filtered by `creator` isn't served from the search index, so it shows an issue as soon as it is created (conformance C1 checks this right after a write). An owner can relabel a created item but can't change its creator, so the lookup still finds it. A marker someone deleted from a body is not found, and that item no longer counts as the record.

### 6.2 Change link

- A change belongs to an item when its body has a `change` record from the coder identity naming the item. The branch name is a second check: it must be the name §3.3 gives for that change, `<branch_prefix><item-key>` or `<branch_prefix><item-key>-<n>`. The `change` record carries *n* (1 for the first branch), so an item key that ends in `-<digits>` is never ambiguous.
- `ChangeForItem` returns the item's open change. Changes the owner closed without merging are kept only to count *n* (D44).
- A change the owner opened (FR-12 AC4, "owner opens a change") is linked when the code host's own structured link names the item. On GitHub that is the pull request's closing-issue references. Body text is not parsed.
- An adopted change whose branch isn't under `branch_prefix` (for example an owner's `feature-x`) isn't covered by the push ruleset (§9.8), so the loop can't tell who pushed to it. `Trust` reports each of its commits that the owner hasn't approved as untrusted, with the cause `branch_outside_prefix`, and the item escalates before implement, address or merge (FR-13 AC5). The escalation comment suggests pushing the branch to `<branch_prefix><item-key>` instead. Each later commit on that branch needs the same approval (I14).

### 6.3 Review record

The worker posts one review on the change. Its body is the handler's summary, then a **status table** with every finding on the change (ID, score, status, and a link to its thread or section), then the record:

```markdown
| Finding | Score | Status | Where |
|---|---|---|---|
| F3 | 4 | addressed | api/login.go:88 |
| F5 | 3 | partially_addressed | api/session.go (file) |
| F8 | 2 | open | this review (no file) |
```

The record:

```json
{"type":"review","v":1,"item":"42","change":"57","commit":"<sha>","kind":"round","round":2,
 "findings":[{"id":"F3","score":4,"status":"addressed"},{"id":"F5","score":3,"status":"partially_addressed"},
             {"id":"F7","score":4,"status":"open","comment":"<code-host comment id>"}],
 "manual_rulings":[{"comment":"MC-981","ruling":"open"}],
 "key":"item/eagain/42#1:review:r2:1"}
```

- `findings` lists **every** finding on the change, with its current status. The latest review record is therefore the complete state, and a restart never needs to replay earlier reviews.
- `round` is set only for kind `round`. It counts the earlier `round` review records on the change that weren't stale (FR-6 AC5). The worker sets it, not the handler.
- **Stale reviews** (FR-4 AC3): if the head moved before the worker posts, the review is still posted, and its marker carries `"stale": true`. Stale records are ignored for merges and for counting rounds.
- When a review record rules a finding `addressed` or `withdrawn`, the worker resolves its thread as the **loop identity** (§4.1 and D45 of the product spec). It does so after the review run, in an adapter activity of its own that is retried a few times. A resolve that still fails is dropped and never fails the review run: the status is in the review record either way. The worker resolves only findings that the record newly rules so, not ones an earlier record already ruled. A finding's status comes only from review records. If anyone else resolves or unresolves a finding's thread, nothing changes: the status stays as the latest review record says, and the worker doesn't undo the change. A human's comment in a finding thread is a manual comment, and resolving the thread changes whether it is cleared (§9.6, D43). Inline and file-level findings have a thread. A finding with no file has none, so its status is shown only in the status table of each later review (I8). The coder replies to such a finding in a change comment that carries a `reply` record.

### 6.4 Replies

A reply record from the coder identity carries `{"type":"reply","finding":"F3","kind":"fixed","deferred_to":null,"head":"<sha>"}`. A manual reply carries `{"type":"manual_reply","comment":"MC-981","kind":"explained"}`. Replies never change a status. Only the next review record does (FR-5 AC4).

### 6.5 Status comments

Loop status comments use plain language for the owner, followed by a `status` marker with `event` (`claim`, `escalate`, `resume`, `pause`, `abort`, `release`, `blocked`, `manual_action`), `reason` (a fixed code from `records.schema.json`, for example `round_limit`, `untrusted_commit`, `branch_outside_prefix`, `empty_review_needs_dismissal`, `worker_lost`, `gate_failed`), the claim number (TECHNICAL-SPEC §6.1), and any finding, comment or commit IDs. A `resume` record also carries `rounds_added` (FR-10 AC5), so the round allowance can be rebuilt from the tracker (TECHNICAL-SPEC §10). An escalation comment mentions the owner accounts (TECHNICAL-SPEC T14) and says what they need to do (FR-10 AC1).

### 6.6 Follow-up item

The body lists one row per unresolved finding: `| F5 | 3 | partially_addressed | <link> |`. It copies no titles and no reviewer text (FR-8 AC1). It links the original item and change. The marker is `{"type":"followup","change":"57","key":"followup:57"}`.

## 7. Task list (`tasks.yaml`)

A plan is a directory on the base branch: `plans/<plan-key>/plan.md` (free text for the owner) and `plans/<plan-key>/tasks.yaml` (the contract). `plans_dir` can be overridden in the profile.

```yaml
version: 1
plan: 003-auth                 # must equal the directory name
tasks:
  - key: login-api             # unique in the plan, [a-z0-9-]{1,64}, never reused
    type: feature              # a configured type: feature | bug
    title: Add login endpoint  # at most 200 characters
    body: tasks/login-api.md   # path relative to the plan directory
    depends_on: []             # task keys in this plan, or existing item refs ("#17", "PROJ-12")
  - key: login-ui
    type: feature
    title: Login screen
    body: tasks/login-ui.md
    depends_on: [login-api]
```

Materialise validates the whole file before creating anything (FR-17 AC6). It is invalid when:

- it doesn't match `tasks.schema.json`, or `plan` doesn't match the directory;
- a key is duplicated, a type isn't configured, or a body file is missing or over 64 KiB;
- a `depends_on` entry names an unknown key or an item that doesn't exist;
- the dependencies have a cycle.

Each created item gets the title, the body file's contents, the type marker, the `ready` marker (FR-17 AC2), a `task` record with the plan and task key, and one native dependency for each `depends_on` entry. Items are created in dependency order, so each dependency exists before the item that needs it. The tracking item gets a `plan` record that lists every task item. A retry finds existing items by their `task` record and only creates the missing ones (FR-17 AC3).

## 8. Ports

These are the exact Go interfaces in `internal/ports`. Every method:

- takes a `context.Context` and returns errors classified as `ErrNotFound`, `ErrConflict` (a precondition failed), `ErrRateLimited` (carries a retry time), `ErrPermission` or `ErrTransient`. The core decides on the class, never on message text;
- that writes, takes an `IdemKey` and is safe to repeat with the same key (NFR-2);
- returns text from humans or agents only as `UntrustedText` (TECHNICAL-SPEC §4).

### 8.1 Common types

```go
type ItemRef string      // tracker-native key: "42", "PROJ-42"
type ChangeRef string    // code-host-native key: "57"
type Commit string       // full SHA
type Actor struct { ID, Login string; Kind ActorKind } // ActorKind: Human, Bot. ID is the stable account ID (I13)
type Marker string       // "ready", "in_progress", "blocked", "needs_owner", "paused", "aborted", "follow_up", "plan", "plan_proposal", "type:feature", "type:bug"
type Version string      // opaque concurrency token for an item (etag, updated-at, revision)
type Cursor string       // opaque position in an adapter's change feed
```

Adapters fill `Actor.ID` with the account's stable ID (on GitHub the numeric user or app ID, on Jira the `accountId`). The core decides whether an actor is an owner account or one of the three identities by comparing IDs with the profile (FR-13 AC7, I13). No port has an "is owner" method.

### 8.2 `WorkTracker`

```go
type WorkTracker interface {
    Capabilities() TrackerCaps
    Validate(ctx context.Context, p ProfileTracker) []Problem

    Item(ctx context.Context, ref ItemRef) (Item, error)               // markers, state, close reason, assignees, Version
    Candidates(ctx context.Context) ([]Item, error)                   // open items with the ready marker
    Changes(ctx context.Context, since Cursor) ([]ItemEvent, Cursor, error)

    Claim(ctx context.Context, ref ItemRef, expect Version, key IdemKey) (Version, error) // ErrConflict if the item changed
    SetMarker(ctx context.Context, ref ItemRef, m Marker, on bool, key IdemKey) error
    Comment(ctx context.Context, ref ItemRef, rec Record, body UntrustedText, key IdemKey) (CommentRef, error)
    FindRecords(ctx context.Context, ref ItemRef, t RecordType) ([]Record, error) // author-checked (§6.1)
    CreateItem(ctx context.Context, n NewItem, key IdemKey) (ItemRef, error)
    AddDependency(ctx context.Context, item, blockedBy ItemRef, key IdemKey) error
    Dependencies(ctx context.Context, ref ItemRef) ([]Dependency, error) // each with state and close reason
    Close(ctx context.Context, ref ItemRef, reason CloseReason, key IdemKey) error
}
```

- `Item.CloseReason` is `Completed`, `NotDone` or `Open`, and `Item.ClosedBy` is the `Actor` who closed it. `Dependency` carries the same two fields, plus `ChangeMerged` (its linked change was merged). The core decides "closed as done" (FR-2 AC6): the change merged, or `Completed` by an owner account. A `Completed` close by anyone else counts as closed but not done.
- `ItemEvent` is one of `MarkerChanged{Actor, Marker, On}`, `Closed{Actor, Reason}`, `Reopened`, `AssigneeChanged{Actor, Human bool}`, `Commented{Actor}` or `DependencyChanged`. Each carries the item ref, the time and the item's new `Version`.
- `Claim` sets `in_progress`, clears `ready` and, when `Capabilities().Assign` is true, assigns the coder identity, all only if the item's version still equals `expect`.
- `CreateItem` finds an earlier item with the same key in the place §6.1 names for its record type. `NewItem.Since` gives the time from which to look (the change's merge time for a follow-up, the plan's merge time for task and tracking items).
- `TrackerCaps` has `Assign`, `NativeDependencies`, `StatusMarkers` (markers can map to workflow statuses) and `MaxBody`.

### 8.3 `CodeHost`

```go
type CodeHost interface {
    Capabilities() HostCaps
    Validate(ctx context.Context, p ProfileCodeHost) []Problem

    ChangeForItem(ctx context.Context, item ItemRef) (*Change, error)   // §6.2, nil if none
    Change(ctx context.Context, ref ChangeRef) (Change, error)          // state, head, base, fork?, merged by, labels
    Changes(ctx context.Context, since Cursor) ([]ChangeEvent, Cursor, error)
    OpenChange(ctx context.Context, n NewChange, as Identity, key IdemKey) (ChangeRef, error)
    SetLabel(ctx context.Context, ref ChangeRef, m Marker, on bool, key IdemKey) error

    Trust(ctx context.Context, ref ChangeRef) (TrustReport, error)     // §9.8
    ContainsBase(ctx context.Context, ref ChangeRef) (bool, error)
    Gates(ctx context.Context, ref ChangeRef, c Commit) ([]GateResult, error) // required gates only

    ReviewRecords(ctx context.Context, ref ChangeRef) ([]ReviewRecord, error) // reviewer-authored only
    PostReview(ctx context.Context, ref ChangeRef, r ReviewPost, key IdemKey) (ReviewRef, error)
    ResolveFinding(ctx context.Context, ref ChangeRef, f FindingID, key IdemKey) error
    ManualComments(ctx context.Context, ref ChangeRef) ([]ManualComment, error)
    PostReply(ctx context.Context, ref ChangeRef, to ThreadRef, rec Record, body UntrustedText, key IdemKey) error

    Merge(ctx context.Context, ref ChangeRef, pinned Commit, m MergeMethod, key IdemKey) (MergeOutcome, error)
}
```

- `ChangeEvent` is one of `HeadMoved{Commit, Pusher}`, `Merged{Actor}`, `Closed{Actor}`, `Reopened`, `Opened{Actor, Item}`, `ReviewSubmitted{Actor}`, `ThreadResolved{Actor}`, `ThreadUnresolved{Actor}`, `ReviewDismissed{Actor}` or `GateChanged{Commit}`.
- `HeadMoved.Pusher` is `Coder`, `Owner` or `Unknown`, worked out as §9.8 step 6 says (I11). The review kind after a head move (FR-6 AC6) and trust both use it.
- `ManualComment` has `ID`, `Author` (always a manual commenter: a human, or a bot listed in `manual_comment_bots`, §9.6), `Kind` (`Thread`, `ReviewBody` or `EmptyChangesRequested`), `Cleared` with `ClearedBy` (`HumanResolved`, `HumanDismissed`, or none), and `Body UntrustedText`. Ruling-based clearing (FR-7 AC9, first bullet) is worked out by the core from review records, not by the adapter.
- `MergeOutcome` is `Merged`, `HeadMoved`, `NotMergeable` or `Failed`. `Merged` ends the merge stage. `HeadMoved` is cause 2 of TECHNICAL-SPEC §8.1. `Failed` is cause 5 (the merge call failed). `NotMergeable` means the code host refused the merge, for a conflict, a base that moved, or a protection rule. The adapter can't always tell which, so the core never acts on `NotMergeable` directly: it reads the change again and runs the merge-failure order from the top. Only if no earlier cause holds is it cause 5, "branch protection refused" (I15).
- `GateResult` has `Name`, `State` (`Pass`, `Fail`, `Pending`, `Missing`) and `Commit`.

### 8.4 Other ports

`Workspace`, `Runner`, `SecretStore` and `Notifier` are internal to agent-loop and are defined in TECHNICAL-SPEC §5. Their contract for handlers is §3 of this spec.

## 9. GitHub adapter

### 9.1 Identities and permissions

Each identity is a GitHub App installed on the project repository. Tokens are installation tokens, created for each run and **narrowed** with the `permissions` and `repositories` parameters, so each one only gets what its use needs (FR-13 AC3, AC4).

| Token | Used by | Permissions |
|---|---|---|
| Coder, handler | implement, address and mint handlers | contents: write, issues: read, pull requests: read, metadata: read |
| Coder, worker | Opening changes and proposals, posting replies, conflict-free base merges (§5.3, D41) | pull requests: write, issues: read, contents: write. Never given to a handler |
| Reviewer, handler | Review handler | contents: read, issues: read, pull requests: read |
| Reviewer, worker | Posting reviews, approving | pull requests: write, contents: read |
| Loop | Adapter activities only, including resolving finding threads (§6.3, product D45: GitHub refuses `resolveReviewThread` to an app installed without `contents: write`, whatever its token) | issues: write, pull requests: write, contents: write (needed to merge, §8.8 of the product spec), checks: read, statuses: read, administration: read (rulesets) |

`project validate` checks each app's installed permissions against this table, and reports any extra permission (NFR-4 AC1).

On GitHub, `contents: write` also allows pushing to any branch, creating tags and releases, and merging a pull request through the API. The coder handler token therefore needs the rulesets in §9.8 step 1 to keep it to the loop's branches (I10). Neither coder token has the `workflows` permission, so a push that changes `.github/workflows/` is refused. An item that needs such a change fails its push, and escalates when its attempts are used up.

### 9.2 Markers

Markers map to labels by default (`[markers]` in the profile, TECHNICAL-SPEC §13). Type markers map to the `feature` and `bug` labels. GitHub has no workflow statuses, so `StatusMarkers` is false.

### 9.3 Claim

GitHub has no compare-and-set on issues. Only the `ProjectWorkflow` claims items, so claims are already serialised. `Claim` therefore:

1. reads the issue, and compares `updated_at` and the labels with `expect`. On a mismatch it returns `ErrConflict`;
2. adds `in-progress` and removes `ready`;
3. reads the issue again, and fails with `ErrConflict` if a human changed it in between. In that case it rolls back its own label changes;
4. only then posts the `claim` status record. A claim that was rolled back leaves no claim comment. If the worker crashes between steps 3 and 4, the next reconcile sees `in-progress` with no `claim` record for the current claim and posts it with the same key.

GitHub Apps can't be assigned to issues, so `Assign` is false. A human assignee is still read for FR-2 AC1.

### 9.4 Change feed and polling

`Changes` uses these endpoints, each with its own cursor in `Cursor`:

| Source | Endpoint | Gives |
|---|---|---|
| Issue events | `GET /repos/{o}/{r}/issues/events` (newest first, read until the last seen ID) | Label changes with actor, closes with `state_reason`, reopens, assignments |
| Issues and pulls updated | `GET /repos/{o}/{r}/issues?state=all&since=<t>&sort=updated` | Catch-all for anything the events feed missed |
| Pull request detail | GraphQL `pullRequest { headRefOid, merged, mergedBy, closed, reviewThreads, reviews, timelineItems }` for each changed PR | Head moves, reviews, thread resolves, dismissals, merges |
| Gates | GraphQL `commit.statusCheckRollup` for the head | Gate changes |

REST requests use conditional `If-None-Match`, so unchanged results don't count against the rate limit. GraphQL has no conditional requests and is charged in points, so the GraphQL rows are read only for pull requests that the REST feeds show as changed, and batched (TECHNICAL-SPEC Q4). The full reconcile (TECHNICAL-SPEC §6.2) reads every open item that carries a marker. The budget is measured in v0.1 (TECHNICAL-SPEC Q4).

### 9.5 Dependencies (I3)

- Read with `GET /repos/{o}/{r}/issues/{n}/dependencies/blocked_by`, and written with `POST …/dependencies/blocked_by`.
- A dependency is done when its linked change was merged, or when the blocking issue is closed with `state_reason: completed` by an owner account (the `closed` event's actor, FR-2 AC6). `not_planned` and `duplicate`, and `completed` by anyone else, count as closed but not done. Its linked change is the one §6.2 links to the issue, or the pull request GitHub names as the issue's closer (`ClosedEvent.closer`). An issue the loop closed after its change merged is done through its merged change.
- Body text such as "Blocked by #n" is not read. `project validate` warns if open ready issues contain such lines with no native dependency, so a repository that used the text convention notices.

### 9.6 Manual review comments

- A **manual commenter** is a `User` (not a `Bot`) who isn't one of the three identities, or a bot listed in the profile's `manual_comment_bots` (I7, product spec D37, for example a security scanner). A **manual comment** is each review comment by a manual commenter, in any thread: one the manual commenter started, a reviewer finding thread, or another commenter's thread (product spec §5, D43). A non-empty review body from a manual commenter is one too, and so is a `CHANGES_REQUESTED` review with an empty body. An empty `COMMENTED` or `APPROVED` review body is not. Comments from other bots (for example dependabot) are not manual comments, and are ignored. A listed bot's comment blocks and is cleared exactly like a human's. Its approval never allows a merge (FR-7 AC9).
- A comment in a thread is **cleared by a human** when the thread's `isResolved` is true and the `resolvedBy` user isn't one of the three identities. Resolving a thread clears every manual comment in it. A resolve by a listed bot doesn't clear anything. Only a human's resolve, or an `addressed` ruling, clears a bot's comment. A human's reply in a reviewer finding thread is not cleared when the worker resolves that thread as the loop identity for its finding (§6.3): `resolvedBy` is then the loop identity. It stays a manual comment that blocks until a human resolves the thread again (unresolve, then resolve) or an `addressed` ruling clears it. A comment added to a thread after a human resolved it blocks until the thread is resolved again. A review body is cleared when the review is `DISMISSED` and the timeline's `ReviewDismissedEvent.actor` is a human. Unresolving the thread makes it block again (FR-7 AC9).
- Plain conversation comments on the pull request are not review comments. They are data only (FR-12 AC4).
- An **empty `CHANGES_REQUESTED` review** is cleared only when a human dismisses it (D43). It is never in `answer_manual` or `rule_manual` (§4.2). At the merge check, if the only manual comments that aren't cleared are empty `CHANGES_REQUESTED` reviews, cause 4 (TECHNICAL-SPEC §8.1) doesn't send the item to `addressing`, because an address run can't clear them. Instead the item escalates with the reason `empty_review_needs_dismissal`, naming each review. If other manual comments aren't cleared too, the item goes to `addressing` for those first.

### 9.7 Gates and merge

- The required gates are the required status checks of the rules that apply to the base branch (`GET /repos/{o}/{r}/rules/branches/{base}`), plus classic branch protection if set. A profile `gates = [...]` list replaces them.
- A gate passes when its check run's conclusion is `success`, `neutral` or `skipped`, or its commit status is `success`. That is the same rule GitHub uses for required checks. `Pending` covers queued and in-progress runs, and a gate with no run at all is `Missing`. Both keep the item in `merging` (FR-7 AC6, cause 6).
- `ContainsBase` uses `GET /repos/{o}/{r}/compare/{base}...{head}`, and is true when `behind_by == 0`.
- `Merge` calls `PUT /repos/{o}/{r}/pulls/{n}/merge` with `sha = pinned` and `merge_method` from the profile (FR-7 AC5, AC7). The responses map as follows:
  - 200 is `Merged`;
  - 409 "Head branch was modified" is `HeadMoved`;
  - 405 is `NotMergeable`, which the core re-checks from the top of the merge-failure order (§8.3, I15);
  - anything else is `Failed`.
- A repeat finds the pull request already merged at `pinned` and returns `Merged`.
- **Base moving during the call.** `sha` pins the head, not the base. If the base moves between the merger's check and the call, GitHub merges anyway unless the base branch's rules require branches to be up to date before merging. Then the merged head wouldn't contain the current base tip (FR-7 AC8). `init` turns on "require branches to be up to date" in the base ruleset when the project has required checks, and `project validate` warns when it is off. With it on, the race returns 405 and goes to cause 3.
- **Base-branch rules the loop can't meet.** Some rules make every merge by the loop fail with 405, and the item escalate for cause 5 every time. `project validate` reports each one:
  - *Require conversation resolution* refuses a merge while any review thread is unresolved. FR-8 lets an item merge with unresolved findings below the threshold, and FR-7 AC9 lets a manual comment be cleared by an `addressed` ruling without a resolved thread. With this rule on, those merges are refused, so `validate` fails when the rule is on (owner decision N4a, 2026-11-15).
  - *Required approving reviews* count only approvals from accounts with write access to the repository, and GitHub may not count an app's approval at all. The reviewer app has `contents: read` (FR-13 AC2). Whether its approval satisfies the rule must be verified on GitHub (IQ4, conformance C14). Until then, `validate` warns when approvals are required.
  - *Require review from code owners* can't be met by the reviewer app unless it is a code owner. `validate` warns when the rule is on.

### 9.8 Trust (I6, I10, I11, FR-13 AC5)

GitHub's pull-request API records no pusher for each commit, so the push ruleset acts as the push record. Product spec D40 makes the ruleset FR-13 AC5's push record. The push activity API is not used.

1. **Rulesets.** Three active repository rulesets are required. `init` creates them (§12.2 step 5):
   - **Loop branches:** targets `refs/heads/<branch_prefix>**`, with the rules *restrict creations*, *restrict updates*, *restrict deletions* and *block force pushes*. Its bypass list may contain only the coder app and the **Repository admin** role. Only the people who can bypass the rules can then push to the loop's branches.
   - **Base branch (I10):** targets the base branch, with *restrict updates*, *restrict deletions* and *block force pushes*. Its bypass list may contain only the loop app and the Repository admin role. So the coder app can neither push to the base nor merge a pull request into it, even though `contents: write` allows both. The loop app bypasses only this ruleset: the project's own rules for the base branch (required checks, pull requests) still apply to its merges. Humans who aren't owners can't merge either. That is a side effect of the rule, and FR-12 AC4 still handles a human merge if one happens.
   - **Tags (I10):** targets `refs/tags/**`, with *restrict creations*, *restrict updates* and *restrict deletions*, and only the Repository admin role (plus any release app the profile names) in the bypass list. This also stops the coder app from creating a release with a new tag.

   Other branches are not covered: the coder app can push to them, and the loop never adopts them without the owner's approval (§6.2, I14). `init` offers an optional fourth ruleset for them, and `project validate` warns when it is missing.
2. **Admins.** Every user with admin permission on the repository must be an owner account, so "Repository admin" bypass means owners only. On an organisation repository this includes the organisation's owners, who are admins of every repository. `project validate` lists collaborators with admin permission (`GET /repos/{o}/{r}/collaborators?permission=admin&affiliation=all`), and fails on any that isn't an owner account.
3. **Before every implement, address and merge**, `Trust` checks:
   - the pull request isn't from a fork (`head.repo` equals the base repository);
   - the change's branch is under `branch_prefix`. Otherwise every commit the owner hasn't approved is untrusted (`branch_outside_prefix`, §6.2, I14);
   - the three rulesets still exist, unchanged, and are active, and the admin list still holds only owner accounts (results are cached for one poll interval). A new admin who isn't an owner account makes every open change untrusted, like a ruleset change (I9);
   - every commit not reachable from the base tip arrived while the rulesets were active. In practice: the branch was created after the loop-branch ruleset's `created_at`, and the rulesets haven't changed since the last check;
   - no commit not reachable from the base tip has the pusher `Unknown` (step 6), unless the owner approved it.

   If any check fails, the change is untrusted. The report names the cause and the commits since the last trusted head.
4. **Owner approvals** (FR-13 AC6): when the owner resumes an item that was escalated for an untrusted change, the loop writes an `approved_commits` record with the commits the report named. Those commits are trusted from then on. A fork is never approved this way.
5. **Rulesets on private repositories** need a plan that supports them. If `GET /repos/{o}/{r}/rulesets` is refused, `project validate` fails and says why. No weaker fallback is used on GitHub.
6. **Who pushed (I11).** The review kind after a head move depends on whether the coder or an owner pushed it (FR-6 AC6, D16), and step 1 means no one else can. The worker issues coder push tokens per run, revokes them when the run ends (TECHNICAL-SPEC §7.3), and records each run's item, start and end. A head move first seen at poll time *t*, with the previous poll at *t − Δ*, gets its `Pusher` like this:
   - `Coder` when it happened during a coder run for the same item. At the end of the run, its `done.head` (or the branch head, after `failed`) is that run's head;
   - `Owner` when no coder push token of the project was live at any time in [*t − Δ*, *t*];
   - `Unknown` otherwise. The head moved while another item's coder run held a live token, and the coder token can push to any loop branch, so that run may have pushed to the wrong branch. Trust treats the new commits as untrusted (`pusher_unknown`), and the item escalates. The owner's resume approves them (FR-13 AC6). With a WIP limit of 2 this happens only when an owner pushes during the other item's coder run.

   Owner pushes while this item's own coder run is live are counted as that run's, which matches FR-12 AC4: the run continues on the current branch, and the review after an address run takes its kind from the run (FR-6 AC6). The one gap: a review right after an implement run during which the owner also pushed is a counted `round`, not `owner_push`. This is accepted.
7. **Deleting merged branches.** *Restrict deletions* on the loop branches may also stop GitHub's "automatically delete head branches" setting from deleting them (to verify, IQ4). Merged loop branches then stay until an owner deletes them. The loop never deletes branches (product spec §8 item 3).

The commit's verified author isn't used on GitHub, because agent commits made with local git aren't signed. FR-13 AC5's pusher-record and verified-author fallbacks are for code hosts with no push rule (D40).

## 10. Jira notes (later adapter)

These notes show the ports don't assume GitHub. They are not a design for the Jira adapter.

| Concern | Jira mapping |
|---|---|
| Item | Issue key (`PROJ-42`) |
| Markers | `ready` and `in_progress` as workflow statuses (for example *Selected for Development* and *In Progress*). The others as labels. Set per marker in `[markers]` |
| Claim | Transition plus a compare on the issue's `updated` timestamp. Assigning a technical user is possible (`Assign = true`) |
| Dependencies | Issue links of type "is blocked by". Done means the resolution is in the profile's done set (default `Done`) |
| Changes | JQL `project = X AND updated >= <cursor>` and the changelog for each issue (gives the actor for every field change) |
| Owner check | Atlassian `accountId` in `owners` |
| Records | Same markers, inside a hidden panel or a comment property instead of an HTML comment |
| Change link | Open: TECHNICAL-SPEC Q6. The branch name `agent/PROJ-42` works for the code host side in any case |

## 11. Conformance suite

Every `WorkTracker` and `CodeHost` adapter must pass `conformance/` (TECHNICAL-SPEC T16). The suite drives the adapter against a sandbox project and checks:

| # | Behaviour |
|---|---|
| C1 | Every write method called twice with the same key leaves one result (NFR-2 AC1) |
| C2 | `Claim` returns `ErrConflict` when a human changed the item after it was read |
| C3 | `FindRecords` and `ReviewRecords` ignore records whose author isn't the allowed identity, even with a valid marker |
| C4 | `Changes` reports each label change with the right actor, and a change made by a non-owner human is reported as such |
| C5 | Closed-as-done and closed-not-done are told apart for items and dependencies |
| C6 | Manual comments: human resolve clears, identity resolve doesn't, unresolve blocks again, and dismissal works the same way |
| C7 | `Merge` with an old pinned commit returns `HeadMoved` and doesn't merge |
| C8 | `ContainsBase` is false after the base moves and true after the base is merged into the branch |
| C9 | `Gates` reports `Pending`, `Missing`, `Pass` and `Fail` correctly for the head commit only |
| C10 | `Trust` reports a fork, a removed or changed ruleset, a new admin who isn't an owner, a branch outside the prefix, and a push by an account outside the bypass list (on GitHub: the push is refused) |
| C11 | Text fields from humans come back only as `UntrustedText` |
| C12 | `Changes` from a cursor never skips an event when the feed has more events than one page |
| C13 | With the coder handler token: a push to the base branch, a merge of a pull request, and creating a tag are all refused (I10) |
| C14 | On a base branch that requires one approval, the reviewer app's `APPROVE` review lets the loop app merge (FR-4 AC7, IQ4) |
| C15 | `HeadMoved.Pusher` is `Coder` for a push during the item's coder run or the worker's base merge, `Owner` for a push with no live coder token, and `Unknown` for a push during another item's coder run (I11) |
| C16 | Manual comments (D43): a human's reply in a reviewer finding thread blocks, and still blocks after the worker resolves that thread for its finding. An empty `CHANGES_REQUESTED` review blocks, has `Kind` `EmptyChangesRequested`, and is cleared only by a human dismissal |
| C17 | `ResolveFinding` resolves the finding's thread as the loop identity: `Changes` reports one `ThreadResolved` whose actor is the loop. On GitHub this holds with the reviewer app installed with `contents: read` only, and the reviewer app's own resolve is refused (product D45). In C6, an identity whose resolve the code host refuses counts as not clearing |

The `fake` adapter passes the same suite, so scenario tests against it mean the same as they would against GitHub.

## 12. Adoption

A project should go from nothing to its first change merged by agent-loop in **under 30 minutes**, without writing code (D39). This section defines the tooling that makes that possible. `docs/GETTING-STARTED.md` is the step-by-step guide for users.

### 12.1 Integration levels

| Level | The project | Writes | Typical effort |
|---|---|---|---|
| 0. Starter | Runs `agent-loop init` and picks the starter handlers (§12.4) | Nothing | Minutes |
| 1. Custom prompts | Keeps the starter handlers, and points them at its own prompt files or skills | Markdown prompts | An hour |
| 2. Custom handler | Writes its own handler program that meets §3–§5 | A program in any language | A day |
| 3. New tracker or code host | Writes a Go adapter for §8 that passes the conformance suite (§11) | Go code in agent-loop | Days. Rare, done once per system |

Every level uses the same contracts. Moving up a level never needs a change to the core.

### 12.2 `agent-loop init` (FR-1 AC5)

`init` runs on the owner's machine, in the project's repository. It acts with the **owner's own login** (on GitHub, a device-flow login or the `gh` CLI's token) only while it runs, and never stores that login. The loop never uses it (FR-13 AC1). Every step shows what it will change and asks first. `--yes` accepts every step, and `--check` only reports. Running it again skips what already exists, and repairs anything that drifted.

| Step | What it does (GitHub) |
|---|---|
| 1. Detect | Reads the git remote, the default branch and the owner's login. Checks that the owner has admin rights on the repository, and that the plan allows rulesets (§9.8 step 5). If either is missing, it stops and says why |
| 2. Identities | Creates the three GitHub Apps (`<project>-coder`, `-reviewer`, `-loop`) through GitHub's App Manifest flow, with exactly the permissions in §9.1. Each app needs one browser confirmation. It installs each app on the repository only, and stores each private key in the `SecretStore` (TECHNICAL-SPEC T12). Existing apps can be named instead |
| 3. Profile | Writes `.agent-loop.toml` with the defaults: tracker and code host `github`, the base branch, `owners = [{ login, id }]` for the owner's account (I13), the default markers and limits, and the stages (step 6). It asks only about values that have no safe default. It registers the project in the deployment config (TECHNICAL-SPEC §13), so the server loads it when it starts |
| 4. Labels | Creates the labels in the profile's marker mapping (product spec §5.1) that don't exist yet. Existing labels are left unchanged |
| 5. Push rules | Creates or repairs the three rulesets in §9.8 step 1: loop branches, base branch and tags. Offers the optional ruleset for other branches. On the base branch it also offers "require branches to be up to date" when there are required checks (§9.7). Reports base-branch rules the loop can't meet (§9.7). Warns about any admin who isn't an owner account (§9.8 step 2), and offers to add them to `owners` |
| 6. Handlers | Asks for each stage: the starter handler (§12.4), or a command. For the starter it checks that the `claude` CLI is installed and logged in |
| 7. Gates | Shows the required checks it found on the base branch (§9.7). If there are none, it warns that merges would wait only on review, and offers to name checks in `gates` |
| 8. Validate | Runs `agent-loop project validate` and shows every problem with a fix |
| 9. Next steps | Prints how to start the server, label a first item `ready`, and watch `agent-loop board` |

`init` never creates or changes issues or pull requests, and never pushes to the repository. It doesn't commit the profile either: the owner reviews and commits `.agent-loop.toml` like any other change.

A tracker or code-host adapter provides its own `init` steps through an optional `Initializer` interface next to `Validate`. The steps above are the GitHub adapter's. A Jira adapter adds its own (for example, mapping markers to workflow statuses).

### 12.3 `agent-loop run --dry-run` (FR-1 AC6)

Runs one supervisor pass (TECHNICAL-SPEC §6.2) with **write-recording adapters**: every read goes to the real tracker, and every write is recorded instead of sent. No handler starts. The output lists:

- each candidate in pick order, marked "would pick" or with the FR-2 AC1 rule that excludes it (for example "has human assignee" or "dependency #17 not done");
- the WIP, daily-limit and pause state that limits picking;
- for each claimed item, its state and the next action, with the rule from TECHNICAL-SPEC §8 that chose it;
- the tracker writes the pass would have made.

It needs only the loop identity's read access, and is safe to run at any time, including while the loop is running.

### 12.4 Starter handlers (D38)

Official starter handlers live in a separate repository, `agent-loop-handlers`, not in the core (D34). They are one program, `agent-loop-claude`, with one subcommand per stage:

```toml
[stages.implement]
command  = ["agent-loop-claude", "implement"]
contract = "1.0"

[stages.review]
command  = ["agent-loop-claude", "review"]
contract = "1.0"
```

- Each subcommand runs Claude Code headless in the workspace, with a stage prompt, and turns the agent's output into a `result.json` that meets §5. It handles text files, the result shape and the `failed` cases, so a project never deals with the contract at levels 0 and 1.
- **Prompts** (level 1): the default prompt for each stage is built in. A project overrides one by adding `.agent-loop/prompts/<stage>.md` to its repository, or by naming a skill (`--skill work-issue`). Prompts are read from the base branch at the start of the run. They are project files the owner controls, never tracker text (product spec §8.5).
- The starter handlers follow the contract's major version. A release of `agent-loop-handlers` names the contract versions it supports, and `project validate` checks it like any other handler (§2).
- The first adopter's skills (`work-issue`, `review-pr`, `address-pr`, `plan-work`) run through the starter handlers with `--skill` (owner decision N12, 2026-11-15). Each skill gets a result-only mode, which writes its result for the starter handler to turn into `result.json` and never posts to GitHub itself (I1, I2). `plan-work` in that mode creates no issues (FR-9 AC3).

## 13. Profile reference (contract-relevant keys)

| Key | Type | Default | Used by |
|---|---|---|---|
| `tracker.adapter`, `code_host.adapter` | string | `github` | §8 |
| `branch_prefix` | string | `agent/` | §3.3, §9.8 |
| `plans_dir` | string | `plans/` | §7 |
| `markers.<marker>` | `{label}` or `{status}` | labels with the §5.1 names | §9.2, §10 |
| `identities.coder`, `.reviewer`, `.loop` | secret reference | required | §9.1 |
| `identities.owners` | list of `{ login, id }`. `id` is the stable account ID and is what counts. `login` is for people reading the file, and `validate` warns when it no longer matches the ID | required | §9.8, FR-13, I13 |
| `stages.<stage>.command` | list of strings | required for each stage used | §3.1 |
| `stages.<stage>.contract` | `MAJOR.MINOR` | required | §2 |
| `stages.<stage>.capabilities`, `.runner`, `.time_limit`, `.attempts` | | `["agent"]`, `local`, `2h`, `2` | TECHNICAL-SPEC §6–§7 |
| `gates` | list of strings | the base branch's required checks | §9.7 |
| `manual_comment_bots` | list of bot logins | `[]` | §9.6 (I7) |
| `merge_method` | `squash` \| `merge` \| `rebase` | `squash` | §9.7 |
| `egress.allow`, `egress.replace` | list of domains, bool | the defaults in TECHNICAL-SPEC §7.2, `false` | TECHNICAL-SPEC §7.2 |
| `release_apps` | list of app IDs | `[]` | §9.8 step 1 (tag ruleset bypass) |

The other profile keys (limits, rounds, intervals, follow-up policy, time zone, priority) are in TECHNICAL-SPEC §13.

## 14. Open questions

Resolved (2026-11-14, owner):

- **IQ1 Other review bots → yes (I7).** A profile can list bots in `manual_comment_bots`. Their comments count as manual comments (§9.6).
- **IQ2 Line-less findings → file-level threads plus a status table (I8).** A finding with a file but no line gets a resolvable file-level thread. Only findings with no file go in the review text, and their status is shown in every review's status table (§6.3).
- **IQ3 Ruleset changes → strict (I9).** A ruleset that was disabled and re-enabled counts as changed, and open items escalate until the owner resumes them (§9.8).

Still open:

- **IQ4 GitHub behaviours to verify on a sandbox repository before v0.1.** (a) Whether an approval by the reviewer app, which has `contents: read`, counts toward required approving reviews (C14). If it doesn't, FR-4 AC7 can't be met on GitHub with FR-13 AC2, and `validate` must fail when approvals are required. (b) Whether *restrict deletions* stops "automatically delete head branches" (§9.8 step 7). (c) That *restrict updates* on the base branch refuses a pull-request merge by the coder app and allows one by the bypassing loop app with the other rulesets still enforced (C13).

Owner decisions from the 2026-11-15 review, all applied: X1 as product D40 (§9.8), R5 as D41 (§5.3, §9.1), X6 as D42 (§4.1), X8 as D43 (§9.6), N8 as D44 (§3.3, §6.2), N4a (§9.7), N12 (§12.4), and I10 to I16 confirmed. Only the GitHub checks in IQ4 are still open.
