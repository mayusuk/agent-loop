package projection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mayusuk/agent-loop/internal/adapters/github"
)

// Rulesets keeps one project's record of its repository's rulesets as the
// owner's login last read them, in the rulesets table: each ruleset's
// updated_at and bypass list. The loop app's token can't see bypass lists,
// so the GitHub host compares its own view with this record (work plan
// #101 D1, D4).
type Rulesets struct {
	DB      *DB
	Project string
}

var _ github.RulesetRecords = Rulesets{}

// RulesetRecords returns the project's records, by ruleset ID.
func (r Rulesets) RulesetRecords(ctx context.Context) ([]github.RulesetRecord, error) {
	rows, err := r.DB.db.QueryContext(ctx, `SELECT ruleset_id, updated_at, bypass FROM rulesets
		WHERE project = ? ORDER BY ruleset_id`, r.Project)
	if err != nil {
		return nil, fmt.Errorf("projection: rulesets: %w", err)
	}
	defer rows.Close()
	var out []github.RulesetRecord
	for rows.Next() {
		var rec github.RulesetRecord
		var at int64
		var bypass string
		if err := rows.Scan(&rec.ID, &at, &bypass); err != nil {
			return nil, fmt.Errorf("projection: rulesets: %w", err)
		}
		if err := json.Unmarshal([]byte(bypass), &rec.Bypass); err != nil {
			return nil, fmt.Errorf("projection: ruleset %d: %w", rec.ID, err)
		}
		rec.UpdatedAt = fromNanos(at)
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("projection: rulesets: %w", err)
	}
	return out, nil
}

// SaveRulesetRecords replaces the project's records with recs, in one
// transaction.
func (r Rulesets) SaveRulesetRecords(ctx context.Context, recs []github.RulesetRecord) error {
	if r.Project == "" {
		return errors.New("projection: rulesets: needs a project")
	}
	tx, err := r.DB.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("projection: rulesets: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM rulesets WHERE project = ?`, r.Project); err != nil {
		return fmt.Errorf("projection: rulesets: %w", err)
	}
	for _, rec := range recs {
		bypass := rec.Bypass
		if bypass == nil {
			bypass = []github.BypassActor{}
		}
		b, err := json.Marshal(bypass)
		if err != nil {
			return fmt.Errorf("projection: ruleset %d: %w", rec.ID, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO rulesets (project, ruleset_id, updated_at, bypass) VALUES (?, ?, ?, ?)`,
			r.Project, rec.ID, nanos(rec.UpdatedAt), string(b)); err != nil {
			return fmt.Errorf("projection: rulesets: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("projection: rulesets: %w", err)
	}
	return nil
}
