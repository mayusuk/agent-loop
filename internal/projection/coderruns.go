package projection

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mayusuk/agent-loop/internal/adapters/github"
	"github.com/mayusuk/agent-loop/internal/ports"
)

// CoderRuns records one project's coder runs in the coder_runs table: an
// implement or address handler run, or the worker's own base merge
// (INTEGRATION-SPEC §9.8 step 6, I11). It implements workflows.CoderRuns,
// which writes the records, and github.RunLog, which the GitHub host reads
// them through to work out who pushed a head (work plan #21 D3).
type CoderRuns struct {
	DB      *DB
	Project string
}

var _ github.RunLog = CoderRuns{}

// Started records a coder run's start. A retry with the same run ID
// changes nothing.
func (c CoderRuns) Started(ctx context.Context, item ports.ItemRef, runID string, at time.Time) error {
	switch {
	case c.Project == "" || item == "" || runID == "":
		return errors.New("projection: coder run: needs a project, item and run ID")
	case at.IsZero():
		return errors.New("projection: coder run: no start time")
	}
	_, err := c.DB.db.ExecContext(ctx, `INSERT INTO coder_runs (run_id, project, item, started_at, ended_at, head)
		VALUES (?, ?, ?, ?, 0, '') ON CONFLICT (run_id) DO NOTHING`, runID, c.Project, string(item), nanos(at))
	if err != nil {
		return fmt.Errorf("projection: coder run: %w", err)
	}
	return nil
}

// Ended records a coder run's end and the head it left. A run that was
// never started is ports.ErrNotFound.
func (c CoderRuns) Ended(ctx context.Context, _ ports.ItemRef, runID string, at time.Time, head ports.Commit) error {
	if at.IsZero() {
		return errors.New("projection: coder run: no end time")
	}
	res, err := c.DB.db.ExecContext(ctx, `UPDATE coder_runs SET ended_at = ?, head = ? WHERE run_id = ? AND project = ?`,
		nanos(at), string(head), runID, c.Project)
	if err != nil {
		return fmt.Errorf("projection: coder run: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("projection: coder run %s: %w", runID, ports.ErrNotFound)
	}
	return nil
}

// CoderRuns returns the project's coder runs live at any time in
// [from, to]: started at or before to, and not ended before from. A run
// with no end is live. Oldest first.
func (c CoderRuns) CoderRuns(ctx context.Context, from, to time.Time) ([]github.CoderRun, error) {
	rows, err := c.DB.db.QueryContext(ctx, `SELECT item, started_at, ended_at, head FROM coder_runs
		WHERE project = ? AND started_at <= ? AND (ended_at = 0 OR ended_at >= ?)
		ORDER BY started_at, run_id`, c.Project, nanos(to), nanos(from))
	if err != nil {
		return nil, fmt.Errorf("projection: coder runs: %w", err)
	}
	defer rows.Close()
	var out []github.CoderRun
	for rows.Next() {
		var item, head string
		var start, end int64
		if err := rows.Scan(&item, &start, &end, &head); err != nil {
			return nil, fmt.Errorf("projection: coder runs: %w", err)
		}
		out = append(out, github.CoderRun{Item: ports.ItemRef(item), Start: fromNanos(start), End: fromNanos(end), Head: ports.Commit(head)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("projection: coder runs: %w", err)
	}
	return out, nil
}
