package projection

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mayusuk/agent-loop/internal/core"
)

// BoardRow is one item on the board (FR-11 AC1).
type BoardRow struct {
	ItemRow
	// TimeInState is how long the item has been in State at the board's
	// time, never negative.
	TimeInState time.Duration
}

// Board returns the items of a project, or of every project when project
// is empty, that are still on the board: every item whose last state is not
// merged or closed and whose claim was not released (work plan D5). Rows are ordered by project, then by the
// time they entered their state, oldest first, then by item key.
func (p *DB) Board(ctx context.Context, project string, now time.Time) ([]BoardRow, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT project, item, state, round, rounds_allowed, blocking,
		host, entered_at, next_action, wait_reason FROM items
		WHERE (? = '' OR project = ?) AND state NOT IN (?, ?) AND released = 0
		ORDER BY project, entered_at, item`,
		project, project, string(core.StateMerged), string(core.StateClosed))
	if err != nil {
		return nil, fmt.Errorf("projection: board: %w", err)
	}
	defer rows.Close()
	var out []BoardRow
	for rows.Next() {
		var (
			r       BoardRow
			state   string
			entered int64
		)
		if err := rows.Scan(&r.Project, &r.Item, &state, &r.Round, &r.RoundsAllowed, &r.Blocking,
			&r.Host, &entered, &r.NextAction, &r.WaitReason); err != nil {
			return nil, fmt.Errorf("projection: board: %w", err)
		}
		r.State = core.State(state)
		r.EnteredAt = fromNanos(entered)
		r.TimeInState = max(now.Sub(r.EnteredAt), 0)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("projection: board: %w", err)
	}
	return out, nil
}

// Release is the owner releasing an item's claim (PRODUCT-SPEC §6, FR-12
// AC4): removing in-progress or aborted, or assigning the item to a human.
type Release struct {
	Project string
	Item    string
	Claim   int // the claim that was released
	At      time.Time
}

// RecordRelease takes a released claim off the board (FR-11 AC1, work plan
// D5). It marks the item's row released if the row belongs to that claim or
// an earlier one; a later claim's row is left alone, so a stale release
// never hides a new claim. A decision of a later claim puts the item back
// on the board. A retry, or an item with no row, changes nothing.
func (p *DB) RecordRelease(ctx context.Context, r Release) error {
	switch {
	case r.Project == "" || r.Item == "":
		return errors.New("projection: record release: no project or item")
	case r.Claim <= 0:
		return errors.New("projection: record release: no claim")
	case r.At.IsZero():
		return errors.New("projection: record release: no time")
	}
	_, err := p.db.ExecContext(ctx, `UPDATE items SET released = 1, updated_at = ?
		WHERE project = ? AND item = ? AND claim <= ? AND released = 0`,
		nanos(r.At), r.Project, r.Item, r.Claim)
	if err != nil {
		return fmt.Errorf("projection: record release: %w", err)
	}
	return nil
}

// BoardItem is an item on the board with the workflow that runs its claim.
type BoardItem struct {
	Project    string
	Item       string
	WorkflowID string
}

// FindOnBoard returns the board items with key item, in project or in
// every project when project is empty, ordered by project. An item
// command uses it to find the workflow to signal (work plan #22 D3).
func (p *DB) FindOnBoard(ctx context.Context, project, item string) ([]BoardItem, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT project, item, workflow_id FROM items
		WHERE (? = '' OR project = ?) AND item = ? AND state NOT IN (?, ?) AND released = 0
		ORDER BY project`,
		project, project, item, string(core.StateMerged), string(core.StateClosed))
	if err != nil {
		return nil, fmt.Errorf("projection: find on board: %w", err)
	}
	defer rows.Close()
	var out []BoardItem
	for rows.Next() {
		var b BoardItem
		if err := rows.Scan(&b.Project, &b.Item, &b.WorkflowID); err != nil {
			return nil, fmt.Errorf("projection: find on board: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("projection: find on board: %w", err)
	}
	return out, nil
}
