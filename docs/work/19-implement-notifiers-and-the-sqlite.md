# Work plan: Implement notifiers and the SQLite projection

**Issue:** [#19](https://github.com/mayusuk/agent-loop/issues/19) (feature)
**Branch:** `issue/19-implement-notifiers-and-the-sqlite`
**Base:** `origin/main @ 9e11f5f` (checked before branching)
**Status:** Draft

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

- [ ] Projection: driver, schema, `Open`, `RecordDecision`, `Decisions`; NFR-5 AC1 tests.
- [ ] Projection: items board, stage runs, item log, hosts, daily count; FR-11 AC1, AC2 tests.
- [ ] Notifiers: desktop with `Display`, `MacDisplay`, tracker mention, `Multi`, `FromDeployment`; tests with a fake display, macOS live test.

## Acceptance criteria

- Every recorded decision names the tracker state and the stage result it was based on (TestNFR5AC1_...).
- The board query returns state, round, blocking count, host, time in state and next action for each claimed item (TestFR11AC1_...).
- The desktop notifier is behind an interface, and tests use a fake. The real one is tested on macOS only.

## Decisions

- **D1** [gap] TECHNICAL-SPEC T11, §10: the SQLite driver is `modernc.org/sqlite` (pure Go). Why: the spec names no driver; a cgo driver (`mattn/go-sqlite3`) would break `CGO_ENABLED=0` builds and Linux cross-compiles (NFR-3 AC1), and modernc is the most used pure-Go driver with `database/sql` support.

## Verification

`wf.py verify`, and `CGO_ENABLED=0 go build ./...`. The live desktop test runs only on macOS with `AGENT_LOOP_LIVE=1`.

## Risks and open questions

- The board's project-level waits (paused project or deployment) come from live workflow queries (TECHNICAL-SPEC §11), not the projection; the `api` task joins them.

## Outcome

_Filled in before shipping: what was done, deviations from this work plan's approach and why, follow-up issues filed. Details the specs should record go in Decisions, not here._
