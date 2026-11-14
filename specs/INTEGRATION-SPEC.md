# Integration Spec: agent-loop

| | |
|---|---|
| **Status** | Draft, 2026-11-14. Not yet reviewed |
| **Implements** | `PRODUCT-SPEC.md` (approved), `TECHNICAL-SPEC.md` (merged draft) |
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
| `GH_TOKEN` (GitHub) | A **read-only** token for the stage's identity, so tools like `gh` can read issues, pull requests and comments |
| stdin | Closed |
| stdout, stderr | Captured, redacted (TECHNICAL-SPEC §7.3) and stored as the run log. Never parsed |
| Exit code | Informational. The result file decides the outcome. A missing, unreadable or invalid result is `failed` (FR-18 AC3), whatever the exit code |
| Time limit | The stage's `time_limit`. When it is reached the whole process group or container is stopped (FR-18 AC4) |

The handler must run headless: it must never wait for terminal input. A handler that needs a human returns `needs_owner` (FR-10 AC4).

### 3.2 Sessions

Every run is a new process and a new agent session (NFR-8). The input never holds a session reference, a transcript, or anything from another stage's session. A handler that needs to keep something for a later stage writes it to the branch (for example `.agent-loop/notes/<item>.md`) or returns it in its result as text that the worker posts.

### 3.3 Workspace and branches

- The worker prepares a worktree from a cached clone, fetched just before the run.
- The item's branch is `<branch_prefix><item-key>`, for example `agent/42` or `agent/PROJ-42`. `branch_prefix` comes from the profile (default `agent/`). The input names the branch.
- **implement:** if the branch doesn't exist, the worktree is created on a new branch from the base tip. Otherwise it checks out the branch head.
- **review:** the worktree is checked out at `review.commit`, detached, so the handler can't push.
- **address, base-update mode:** the handler brings the branch up to date by **merging** the base into it. It never rebases, and never force-pushes (§8.3 of the product spec).
- **mint:** the worktree is on the plan branch `<branch_prefix>plan/<plan-key>`, from the base tip.
- After the run, the worker deletes the worktree. The branch stays.

### 3.4 Credentials by stage

| Stage | Identity | Push token (credential helper) | Read token (`GH_TOKEN`) | Writes to the tracker |
|---|---|---|---|---|
| implement | coder | Yes: contents write on the project repository only | Yes | None. The worker opens the change |
| address | coder | Yes | Yes | None. The worker posts replies |
| review | reviewer | **None** | Yes | None. The worker posts the review |
| mint | coder | Yes, for the plan branch | Yes | None. The worker opens the proposal |

The loop identity's credentials never reach a handler (product spec §8.8).

## 4. Input (`input.json`)

### 4.1 Common fields

```json
{
  "contract_version": "1.0",
  "run_id": "item/eagain/42:implement:1",
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

- `run_id` is unique for each attempt, and stable if the same attempt is restarted after a worker crash.
- `item` and `change` are **references only** (NFR-8 AC1). The handler reads the title, body and comments itself, and treats them as data (product spec §8.5). The input never copies tracker text.
- `change` (when there is one): `{ "ref": "57", "url": "…", "head": "…", "base_tip": "…" }`.

### 4.2 Stage fields

| Stage | Extra fields |
|---|---|
| implement | None. An existing open change is adopted, and implement isn't run (FR-3 AC3) |
| review | `review`: `{ "kind": "round" \| "base_update" \| "manual_reply" \| "owner_push", "commit": "<sha>", "round": 2 \| null, "blocking_threshold": 4, "rule_findings": ["F3","F5"], "rule_manual": ["MC-981"] }` |
| address | `address`: `{ "mode": "findings" \| "manual" \| "base_update", "answer_findings": ["F3"], "answer_manual": ["MC-981"] }` |
| mint | `mint`: `{ "plan_key": "003-auth", "source_of_truth": ["specs/001.md","specs/002.md","specs/003.md"], "plans_dir": "plans/" }` |

- `rule_findings` lists every finding that is unresolved before this review (FR-4 AC4). `rule_manual` lists every manual comment that an address run replied to and no review has ruled on yet (FR-4 AC8).
- `answer_findings` lists every unresolved finding. `answer_manual` lists every manual comment that isn't cleared (FR-5 AC1). Both are empty in `base_update` mode (FR-5 AC6).
- IDs refer to records the handler can read: finding IDs in reviewer records (§6.3), and manual comment IDs in the code host (`MC-<id>` is the code host's comment or review ID).

## 5. Result (`result.json`)

### 5.1 Shape

```json
{ "contract_version": "1.0", "run_id": "…", "result": "done", "done": { … } }
```

`result` is one of `done`, `complete`, `blocked`, `failed` or `needs_owner`. Only the object named by `result` may be present. The results allowed for each stage are in FR-18 AC1.

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
- `score` is an integer from 1 to 5. A `path` must exist at `commit`, and a `line` must be in the diff or the file. `line` needs a `path`.
- How each new finding is posted (I8): with `path` and `line`, as an inline comment. With `path` only, as a file-level comment. With neither, as a section of the review text. Handlers should give a `path` whenever the finding is about a file, so it gets a thread that can be resolved.

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
- In `base_update` mode, `replies` and `manual_replies` must be empty, `head` must contain the base tip, and the diff from the input head may hold only the merge of the base (FR-5 AC6).
- The worker posts each reply as the coder identity in the finding's or comment's thread (§6.4).

**mint**

```json
"done": {
  "plan_key": "003-auth",
  "head": "<sha on the plan branch>",
  "proposal": { "title": "Plan: 003 auth", "body_file": "proposal.md" }
}
```

The worker checks that the plan branch holds `plans/<plan_key>/plan.md` and a valid `plans/<plan_key>/tasks.yaml` (§7). It then opens the proposal as the coder identity, and the loop identity labels it `plan-proposal`. An invalid task list makes the result `failed`, so a bad plan is caught before the owner sees it (materialise still validates again, FR-17 AC6).

## 6. Tracker records

### 6.1 Record marker

Every record the worker writes ends with a hidden marker:

```
<!-- agent-loop:v1 {"type":"review","item":"42","change":"57","key":"item/eagain/42:review:r2:1", ...} -->
```

- The marker is one line of compact JSON in an HTML comment. It is invisible in rendered markdown.
- `key` is the idempotency key (TECHNICAL-SPEC §9). Before writing, the adapter searches for a record with the same `key` from the same author, and returns it if it exists.
- **A record counts only if its author is the identity allowed to write that type** (table below). A marker in text from anyone else is ignored and logged. This is what keeps copied or forged markers from acting as records (§8.5 of the product spec).

| `type` | Written as | Where (GitHub) | Purpose |
|---|---|---|---|
| `change` | coder | Change body | Links the change to its item (§6.2) |
| `review` | reviewer | Review body | Structured review result (§6.3) |
| `finding` | reviewer | Inline review comment, file-level review comment, or a review-text section for findings with no file (I8) | One finding |
| `reply` | coder | Reply in the finding's thread | Coder reply to a finding (§6.4) |
| `manual_reply` | coder | Reply in the manual comment's thread, or a change comment that links the review | Coder reply to a manual comment |
| `status` | loop | Item comment | Claim, escalation, resume, abort and manual-action records |
| `approved_commits` | loop | Item comment | Commits the owner approved on resume (FR-13 AC6) |
| `followup` | loop | Follow-up item body | FR-8 |
| `task` | loop | Created item body | Plan and task key (FR-17 AC3) |
| `plan` | loop | Tracking item body | Plan key and task item refs (FR-17 AC2) |
| `proposal` | coder | Plan proposal body | Plan key |

### 6.2 Change link

- A change belongs to an item when its body has a `change` record from the coder identity naming the item. The branch name `<branch_prefix><item-key>` is a second check.
- A change the owner opened (FR-12 AC4, "owner opens a change") is linked when the code host's own structured link names the item. On GitHub that is the pull request's closing-issue references. Body text is not parsed.

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
 "key":"item/eagain/42:review:r2:1"}
```

- `findings` lists **every** finding on the change, with its current status. The latest review record is therefore the complete state, and a restart never needs to replay earlier reviews.
- `round` is set only for kind `round`. It counts the earlier `round` review records on the change that weren't stale (FR-6 AC5). The worker sets it, not the handler.
- **Stale reviews** (FR-4 AC3): if the head moved before the worker posts, the review is still posted, and its marker carries `"stale": true`. Stale records are ignored for merges and for counting rounds.
- When a finding becomes `addressed` or `withdrawn`, the worker resolves its thread as the reviewer identity (§4.1 of the product spec). Inline and file-level findings have a thread. A finding with no file has none, so its status is shown only in the status table of each later review (I8). The coder replies to such a finding in a change comment that carries a `reply` record.

### 6.4 Replies

A reply record from the coder identity carries `{"type":"reply","finding":"F3","kind":"fixed","deferred_to":null,"head":"<sha>"}`. A manual reply carries `{"type":"manual_reply","comment":"MC-981","kind":"explained"}`. Replies never change a status. Only the next review record does (FR-5 AC4).

### 6.5 Status comments

Loop status comments use plain language for the owner, followed by a `status` marker with `event` (`claim`, `escalate`, `resume`, `pause`, `abort`, `release`, `blocked`, `manual_action`), `reason` (a fixed code from `records.schema.json`, for example `round_limit`, `untrusted_commit`, `gate_failed`), and any finding, comment or commit IDs. An escalation comment mentions the owner accounts (TECHNICAL-SPEC T14) and says what they need to do (FR-10 AC1).

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
type Actor struct { ID, Login string; Kind ActorKind } // ActorKind: Human, Bot
type Marker string       // "ready", "in_progress", "blocked", "needs_owner", "paused", "aborted", "follow_up", "plan", "plan_proposal", "type:feature", "type:bug"
type Version string      // opaque concurrency token for an item (etag, updated-at, revision)
type Cursor string       // opaque position in an adapter's change feed
```

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

- `Item.CloseReason` is `Done`, `NotDone` or `Open`. "Closed as done" (FR-2 AC6) is `Done`.
- `ItemEvent` is one of `MarkerChanged{Actor, Marker, On}`, `Closed{Actor, Reason}`, `Reopened`, `AssigneeChanged{Actor, Human bool}`, `Commented{Actor}` or `DependencyChanged`. Each carries the item ref, the time and the item's new `Version`.
- `Claim` sets `in_progress`, clears `ready` and, when `Capabilities().Assign` is true, assigns the coder identity, all only if the item's version still equals `expect`.
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

- `ChangeEvent` is one of `HeadMoved{Commit, Actor}`, `Merged{Actor}`, `Closed{Actor}`, `Reopened`, `Opened{Actor, Item}`, `ReviewSubmitted{Actor}`, `ThreadResolved{Actor}`, `ThreadUnresolved{Actor}`, `ReviewDismissed{Actor}` or `GateChanged{Commit}`.
- `ManualComment` has `ID`, `Author` (always a human, §9.6), `Kind` (`Thread` or `ReviewBody`), `Cleared` with `ClearedBy` (`HumanResolved`, `HumanDismissed`, or none), and `Body UntrustedText`. Ruling-based clearing (FR-7 AC9, first bullet) is worked out by the core from review records, not by the adapter.
- `MergeOutcome` is `Merged`, `HeadMoved`, `NotMergeable` (conflict, or protection refused) or `Failed`, which match FR-7 AC6 causes 2, 3 and 5.
- `GateResult` has `Name`, `State` (`Pass`, `Fail`, `Pending`, `Missing`) and `Commit`.

### 8.4 Other ports

`Workspace`, `Runner`, `SecretStore` and `Notifier` are internal to agent-loop and are defined in TECHNICAL-SPEC §5. Their contract for handlers is §3 of this spec.

## 9. GitHub adapter

### 9.1 Identities and permissions

Each identity is a GitHub App installed on the project repository. Tokens are installation tokens, created for each run and **narrowed** with the `permissions` and `repositories` parameters, so each one only gets what its use needs (FR-13 AC3, AC4).

| Token | Used by | Permissions |
|---|---|---|
| Coder, handler | implement, address and mint handlers | contents: write, issues: read, pull requests: read, metadata: read |
| Coder, worker | Opening changes and proposals, posting replies | pull requests: write, issues: read, contents: read |
| Reviewer, handler | Review handler | contents: read, issues: read, pull requests: read |
| Reviewer, worker | Posting reviews, approving, resolving finding threads | pull requests: write, contents: read |
| Loop | Adapter activities only | issues: write, pull requests: write, contents: write (needed to merge, §8.8 of the product spec), checks: read, statuses: read, administration: read (rulesets) |

`project validate` checks each app's installed permissions against this table, and reports any extra permission (NFR-4 AC1).

### 9.2 Markers

Markers map to labels by default (`[markers]` in the profile, TECHNICAL-SPEC §13). Type markers map to the `feature` and `bug` labels. GitHub has no workflow statuses, so `StatusMarkers` is false.

### 9.3 Claim

GitHub has no compare-and-set on issues. Only the `ProjectWorkflow` claims items, so claims are already serialised. `Claim` therefore:

1. reads the issue, and compares `updated_at` and the labels with `expect`. On a mismatch it returns `ErrConflict`;
2. adds `in-progress` and removes `ready`, and posts a `claim` status record;
3. reads the issue again, and fails with `ErrConflict` if a human changed it in between. In that case it rolls back its own label changes.

GitHub Apps can't be assigned to issues, so `Assign` is false. A human assignee is still read for FR-2 AC1.

### 9.4 Change feed and polling

`Changes` uses these endpoints, each with its own cursor in `Cursor`:

| Source | Endpoint | Gives |
|---|---|---|
| Issue events | `GET /repos/{o}/{r}/issues/events` (newest first, read until the last seen ID) | Label changes with actor, closes with `state_reason`, reopens, assignments |
| Issues and pulls updated | `GET /repos/{o}/{r}/issues?state=all&since=<t>&sort=updated` | Catch-all for anything the events feed missed |
| Pull request detail | GraphQL `pullRequest { headRefOid, merged, mergedBy, closed, reviewThreads, reviews, timelineItems }` for each changed PR | Head moves, reviews, thread resolves, dismissals, merges |
| Gates | GraphQL `commit.statusCheckRollup` for the head | Gate changes |

Requests use conditional `If-None-Match`, so unchanged results don't count against the rate limit. The full reconcile (TECHNICAL-SPEC §6.2) reads every open item that carries a marker. The budget is measured in v0.1 (TECHNICAL-SPEC Q4).

### 9.5 Dependencies (I3)

- Read with `GET /repos/{o}/{r}/issues/{n}/dependencies/blocked_by`, and written with `POST …/dependencies/blocked_by`.
- A dependency is done when the blocking issue is closed with `state_reason: completed`, or when its linked change was merged. `not_planned` and `duplicate` count as closed but not done (FR-2 AC6).
- Body text such as "Blocked by #n" is not read. `project validate` warns if open ready issues contain such lines with no native dependency, so a repository that used the text convention notices.

### 9.6 Manual review comments

- A **manual commenter** is a `User` (not a `Bot`) who isn't one of the three identities, or a bot listed in the profile's `manual_comment_bots` (I7, product spec D37, for example a security scanner). A **manual comment** is a review thread whose first comment is by a manual commenter, or a non-empty review body from one. Comments from other bots (for example dependabot) are not manual comments, and are ignored. A listed bot's comment blocks and is cleared exactly like a human's. Its approval never allows a merge (FR-7 AC9).
- A thread is **cleared by a human** when `isResolved` is true and the `resolvedBy` user isn't one of the three identities. A resolve by a listed bot doesn't clear anything. Only a human's resolve, or an `addressed` ruling, clears a bot's comment. A review body is cleared when the review is `DISMISSED` and the timeline's `ReviewDismissedEvent.actor` is a human. Unresolving the thread makes it block again (FR-7 AC9).
- Plain conversation comments on the pull request are not review comments. They are data only (FR-12 AC4).

### 9.7 Gates and merge

- The required gates are the required status checks of the rules that apply to the base branch (`GET /repos/{o}/{r}/rules/branches/{base}`), plus classic branch protection if set. A profile `gates = [...]` list replaces them.
- A gate passes when its check run's conclusion is `success`, `neutral` or `skipped`, or its commit status is `success`. That is the same rule GitHub uses for required checks. `Pending` covers queued and in-progress runs, and a gate with no run at all is `Missing`. Both keep the item in `merging` (FR-7 AC6, cause 6).
- `ContainsBase` uses `GET /repos/{o}/{r}/compare/{base}...{head}`, and is true when `behind_by == 0`.
- `Merge` calls `PUT /repos/{o}/{r}/pulls/{n}/merge` with `sha = pinned` and `merge_method` from the profile (FR-7 AC5, AC7). The responses map as follows:
  - 200 is `Merged`;
  - 409 "Head branch was modified" is `HeadMoved`;
  - 405 is `NotMergeable`;
  - anything else is `Failed`.
- A repeat finds the pull request already merged at `pinned` and returns `Merged`.

### 9.8 Trust (I6, FR-13 AC5)

GitHub records no pusher for each pull-request commit, so the push ruleset acts as the push record:

1. **Ruleset.** An active repository ruleset targets `refs/heads/<branch_prefix>**`, with the rules *restrict creations*, *restrict updates*, *restrict deletions* and *block force pushes*. Its bypass list may contain only the coder app and the **Repository admin** role. Only the people who can bypass the rules can then push to the loop's branches.
2. **Admins.** Every user with admin permission on the repository must be an owner account, so "Repository admin" bypass means owners only. `project validate` lists collaborators with admin permission, and fails on any that isn't an owner account.
3. **Before every implement, address and merge**, `Trust` checks:
   - the pull request isn't from a fork (`head.repo` equals the base repository);
   - the ruleset still exists, unchanged, and is active (results are cached for one poll interval);
   - every commit not reachable from the base tip arrived while the ruleset was active. In practice: the branch was created after the ruleset's `created_at`, and the ruleset hasn't changed since the last check.

   If any check fails, the change is untrusted. The report names the cause and the commits since the last trusted head.
4. **Owner approvals** (FR-13 AC6): when the owner resumes an item that was escalated for an untrusted change, the loop writes an `approved_commits` record with the commits the report named. Those commits are trusted from then on. A fork is never approved this way.
5. **Rulesets on private repositories** need a plan that supports them. If `GET /repos/{o}/{r}/rulesets` is refused, `project validate` fails and says why. No weaker fallback is used on GitHub.

The commit's verified author isn't used on GitHub, because agent commits made with local git aren't signed. FR-13 AC5's verified-author fallback is for code hosts that have neither push records nor push rules.

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
| C10 | `Trust` reports a fork, a removed ruleset, and a push by an account outside the bypass list (on GitHub: the push is refused) |
| C11 | Text fields from humans come back only as `UntrustedText` |
| C12 | `Changes` from a cursor never skips an event when the feed has more events than one page |

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
| 3. Profile | Writes `.agent-loop.toml` with the defaults: tracker and code host `github`, the base branch, `owners = [<login>]`, the default markers and limits, and the stages (step 6). It asks only about values that have no safe default |
| 4. Labels | Creates the labels in the profile's marker mapping (product spec §5.1) that don't exist yet. Existing labels are left unchanged |
| 5. Push rule | Creates or repairs the ruleset on `<branch_prefix>**` (§9.8 step 1). Warns about any admin who isn't an owner account (§9.8 step 2), and offers to add them to `owners` |
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
- The first adopter's skills (`work-issue`, `review-pr`, `address-pr`, `plan-work`) can be used through `--skill`, once they return their results instead of posting them (I1).

## 13. Profile reference (contract-relevant keys)

| Key | Type | Default | Used by |
|---|---|---|---|
| `tracker.adapter`, `code_host.adapter` | string | `github` | §8 |
| `branch_prefix` | string | `agent/` | §3.3, §9.8 |
| `plans_dir` | string | `plans/` | §7 |
| `markers.<marker>` | `{label}` or `{status}` | labels with the §5.1 names | §9.2, §10 |
| `identities.coder`, `.reviewer`, `.loop` | secret reference | required | §9.1 |
| `identities.owners` | list of logins or account IDs | required | §9.8, FR-13 |
| `stages.<stage>.command` | list of strings | required for each stage used | §3.1 |
| `stages.<stage>.contract` | `MAJOR.MINOR` | required | §2 |
| `stages.<stage>.capabilities`, `.runner`, `.time_limit`, `.attempts` | | `["agent"]`, `local`, `2h`, `2` | TECHNICAL-SPEC §6–§7 |
| `gates` | list of strings | the base branch's required checks | §9.7 |
| `manual_comment_bots` | list of bot logins | `[]` | §9.6 (I7) |
| `merge_method` | `squash` \| `merge` \| `rebase` | `squash` | §9.7 |
| `egress.allow` | list of domains | the defaults in TECHNICAL-SPEC T9 | TECHNICAL-SPEC §7.2 |

## 14. Open questions

Resolved (2026-11-14, owner):

- **IQ1 Other review bots → yes (I7).** A profile can list bots in `manual_comment_bots`. Their comments count as manual comments (§9.6).
- **IQ2 Line-less findings → file-level threads plus a status table (I8).** A finding with a file but no line gets a resolvable file-level thread. Only findings with no file go in the review text, and their status is shown in every review's status table (§6.3).
- **IQ3 Ruleset changes → strict (I9).** A ruleset that was disabled and re-enabled counts as changed, and open items escalate until the owner resumes them (§9.8).

None open.
