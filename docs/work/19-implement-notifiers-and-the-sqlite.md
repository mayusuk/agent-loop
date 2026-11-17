# Work plan: Implement notifiers and the SQLite projection

**Issue:** [#19](https://github.com/mayusuk/agent-loop/issues/19) (feature)
**Branch:** `issue/19-implement-notifiers-and-the-sqlite`
**Base:** `origin/main @ 9e11f5f` (checked before branching)
**Status:** Done

## Summary

Adds the two v0.1 notifiers (macOS desktop and the tracker owner mention) and the SQLite projection: the `items`, `decisions`, `stage_runs` and `hosts` tables, the `RecordDecision` writer, and the board, item-log, host and daily-count queries the CLI and the project workflow need.

## Context

- TECHNICAL-SPEC §10 (tables, who owns which state, daily count rebuilt from `stage_runs`), §12 (notification channels), T11, T14, §3.1 (`internal/notify`, `internal/projection`).
- PRODUCT-SPEC FR-11 AC1–AC3, AC5, NFR-5 AC1, FR-16 AC3–AC4 (daily limit, time zone).
- INTEGRATION-SPEC §6.5: the escalation status comment mentions the owner accounts.
- `internal/core` (`DecisionRecord`, `Basis`, `ResultRef`, `ItemState`), `internal/ports` (`Notifier`, `Notification`), `internal/profile` (`Deployment.Notifiers`, `Projection.Path`, `Owner`).
- The workflows (T16) and activities (T15) call these; this task only provides them.

## Approach

`internal/projection`:
- `Open(ctx, path)` creates the directory (`0700`) and file, sets WAL and a busy timeout, and applies the schema (`CREATE TABLE IF NOT EXISTS`, with a `schema_version` table). Driver: `modernc.org/sqlite` (pure Go, see D1).
- `RecordDecision(ctx, Decision)` inserts the decision and upserts the item row in one transaction. A retried write with the same (workflow, claim, seq) is a no-op. `NewDecision` builds the input from `core.ItemState`, `core.Policy` and the record.
- `StartRun`, `FinishRun` write `stage_runs`. `UpsertHost` writes `hosts`.
- Queries: `Board(project, now)`, `ItemLog(project, item)` (decisions and runs merged by time), `Decisions(project, item)`, `Hosts()`, `DailyCount(project, day, loc)`.
- Portable SQL only (TEXT, BIGINT, `ON CONFLICT`), times as Unix milliseconds UTC, so the same SQL runs on Postgres later.

`internal/notify`:
- `Desktop{Display}` turns a `Notification` into title, subtitle, message, URL and group; `Display` is the interface tests fake. `MacDisplay` uses `terminal-notifier` when on `PATH`, else `osascript` with the text passed as arguments (no script quoting).
- `Tracker`: `Notify` is a no-op; `Mention(owners)` gives the text the escalation comment carries (D6).
- `Multi` sends to every channel and joins errors; `FromDeployment(names, goos, display)` builds it.

## Steps

- [x] Projection: driver, schema, `Open`, `RecordDecision`, `Decisions`; NFR-5 AC1 tests.
- [x] Projection: items board, stage runs, item log, hosts, daily count; FR-11 AC1, AC2 tests.
- [x] Notifiers: desktop with `Display`, `MacDisplay`, tracker mention, `Multi`, `FromDeployment`; tests with a fake display, macOS live test.

## Acceptance criteria

- Every recorded decision names the tracker state and the stage result it was based on (TestNFR5AC1_...).
- The board query returns state, round, blocking count, host, time in state and next action for each claimed item (TestFR11AC1_...).
- The desktop notifier is behind an interface, and tests use a fake. The real one is tested on macOS only.

## Decisions

- **D1** [gap] TECHNICAL-SPEC T11, §10: the SQLite driver is `modernc.org/sqlite`, pinned to v1.59.0. Why: the spec names no driver. A cgo driver (`mattn/go-sqlite3`) would break `CGO_ENABLED=0` builds and Linux cross-compiles (NFR-3 AC1); modernc is the most used pure-Go driver with `database/sql` support. v1.59.0 is the last release whose module needs Go 1.25 (v1.60.0 and later need Go 1.26, and the module is on Go 1.25).
- **D2** [gap] TECHNICAL-SPEC §10, deployment `projection.path`: `Open` creates the database's directory with mode `0700` and the file with mode `0600`, opens it in WAL mode with a 5 s busy timeout and immediate write transactions, and applies the schema with `CREATE ... IF NOT EXISTS`. Why: the spec says only "one file next to the server config"; the log holds item history the owner should keep private, and the server and workers write concurrently.
- **D3** [gap] TECHNICAL-SPEC §10: the database carries a `schema_version` table (version 1). A build refuses a database with a newer version. Times are stored as Unix nanoseconds in UTC in `BIGINT` columns, and the SQL uses only `TEXT`, `BIGINT`, indexes and `ON CONFLICT` upserts, so it runs on Postgres unchanged (v0.3). Why: the spec says the same SQL runs on Postgres but gives no types or migration rule.
- **D4** [gap] TECHNICAL-SPEC §10, NFR-5 AC1: a decision row is keyed by (workflow ID, claim, decision number), and also holds project, item, time, event, from and to state, rule, the tracker state seen (`tracker_state`, JSON with `head`, `review_commit`, `review_kind`, `rounds_used`, `round_allowance`, `base_updates`, `blocking_findings`, `uncleared_manual`), the stage result (`result_stage`, `result_run_id`, `result_outcome`, empty when the decision had none), and the action kinds (JSON list). `RecordDecision` refuses a `stage_result` decision that names no result, or a result without stage, run ID or outcome. A retry with the same key writes nothing. The `items` row is updated in the same transaction, and only by a later claim or a later decision of the same claim, so a late retry never moves the board back. Why: the spec lists the columns' meaning, not their form, key or retry rule.
- **D5** [ambiguity] FR-11 AC1: "every claimed item" is read as every `items` row whose last state is not `merged` or `closed` and whose claim was not released (so `aborted`, `escalated`, `paused`, `blocked` and `waiting_limit` items stay on the board). A release (the owner removes `in-progress` or `aborted`, or assigns the item to a human: PRODUCT-SPEC §6, FR-12 AC4) is written with `RecordRelease(project, item, claim, time)`, which sets the row's `released` flag when the row belongs to that claim or an earlier one; a stale release of an older claim never hides a newer claim, a later decision of the released claim keeps it off, and the first decision of a new claim puts the item back. The core has no release event yet, so the workflow (T16) that sees the claim marker gone calls it. The board is ordered by project, then the time the item entered its state (oldest first), then item key. The time in state runs from the decision that changed the state; a decision that keeps the state keeps the time. Why: the spec doesn't say which states leave the board or the order.
- **D6** [gap] TECHNICAL-SPEC §10 (`stage_runs`): each row is one handler run, keyed by its run ID, with project added to the spec's columns (item keys are per project). `result` is the outcome, or how the run ended without one (for example `timed_out`, `crashed`); a running run has end 0 and an empty result. A repeated start is a no-op; finishing an unknown run is `ErrNotFound`; an empty log path at the finish keeps the one from the start. The item log (`ItemLog`) merges the item's decisions and run starts and ends by time; at equal times the order is run start, decision, run end. Escalations, merges and retries appear as the decisions that made them. Why: FR-11 AC2 says what is recorded, not the form.
- **D7** [gap] TECHNICAL-SPEC §10, FR-16 AC3–AC4: the daily count rebuilt from `stage_runs` counts the project's runs of the handler stages (`implement`, `review`, `address`, `mint`) started from 00:00 to the next 00:00 in the profile time zone (the server's local zone when none is given), so a daylight-saving day is 23 or 25 hours. Rows for `merge` or an agent-free base update, if a caller writes them, don't count. Why: the spec says the count is rebuilt from the day's rows but not which rows count.
- **D8** [open] TECHNICAL-SPEC §10, FR-11 AC1, FR-2 AC6: unclaimed items with `needs-owner` have no workflow, so no decision writes their row; the supervisor (T16) or the board API must add them. The `plans` table is v0.2 and is not created here. Project and deployment pauses come from live workflow queries (TECHNICAL-SPEC §11).

- **D9** [ambiguity] TECHNICAL-SPEC §12 ("`osascript` or `terminal-notifier`"): both are used. `terminal-notifier` is used when it is on `PATH`, because it can open the item's URL on click and replace a repeat by group; otherwise `/usr/bin/osascript` runs a fixed `display notification` script that reads the text from its arguments (after `--`), so no notification text is ever parsed as AppleScript. Why: the spec allows either.
- **D10** [gap] TECHNICAL-SPEC §12, FR-11 AC3: a desktop notification's title is `agent-loop`, its subtitle is `<project> <item>: <reason>`, its message is the notification's message (the reason when there is none), the click opens the item's URL, and the idempotency key is the group, so a retried notify replaces the earlier one with terminal-notifier (osascript can't, so a retry may show twice). Why: the spec doesn't give the layout or say how a retry is handled.
- **D11** [ambiguity] TECHNICAL-SPEC §12, T14, INTEGRATION-SPEC §6.5: the tracker channel is the owner mention in the escalation status comment, which the escalation activity (T15) posts. So the tracker notifier's `Notify` writes nothing, and `notify.Mention(owners)` gives the mention: `@login` for each owner account with a login, in profile order, space-separated. Merges and failed stages are not mentioned on the tracker. Why: a separate tracker comment per notification would duplicate the escalation comment and add noise; the spec places the mention in the escalation comment.
- **D12** [gap] FR-11 AC5, NFR-3 AC1, deployment `notifiers`: off macOS the `desktop` channel is left out with a warning for the server log, and the other channels still run; one channel failing doesn't stop the others, and the errors are joined. Why: the default channel list includes `desktop`, and a Linux server would otherwise fail every notify.

## Verification

`wf.py verify`, and `CGO_ENABLED=0 go build ./...`. The live desktop test runs only on macOS with `AGENT_LOOP_LIVE=1`.

## Risks and open questions

- The board's project-level waits (paused project or deployment) come from live workflow queries (TECHNICAL-SPEC §11), not the projection; the `api` task joins them.

## Outcome

Added `internal/projection` (SQLite via `modernc.org/sqlite`: `Open`, `RecordDecision`, `NewDecision`, `Decisions`, `Board`, `StartRun`, `FinishRun`, `Runs`, `ItemLog`, `DailyCount`, `UpsertHost`, `Hosts`) and `internal/notify` (`Desktop` over a `Display` interface, `MacDisplay`, `Tracker` with `Mention`, `Multi`, `New` from the deployment's channels). The approach was followed. Unclaimed `needs-owner` rows and the `plans` table are left to later tasks (D8). `CGO_ENABLED=0 go build ./...` passes. The live desktop test passed on macOS with `AGENT_LOOP_LIVE=1`.
