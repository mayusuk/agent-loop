package projection

import (
	"context"
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
// merged or closed (work plan D5). Rows are ordered by project, then by the
// time they entered their state, oldest first, then by item key.
func (p *DB) Board(ctx context.Context, project string, now time.Time) ([]BoardRow, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT project, item, state, round, rounds_allowed, blocking,
		host, entered_at, next_action, wait_reason FROM items
		WHERE (? = '' OR project = ?) AND state NOT IN (?, ?)
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
