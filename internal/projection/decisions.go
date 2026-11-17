package projection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/core"
)

// Decision is what the RecordDecision activity writes after each
// core.Decide call: the decision record and the item's board row after it
// (TECHNICAL-SPEC §10).
type Decision struct {
	WorkflowID string
	Claim      int
	Record     core.DecisionRecord
	Item       ItemRow
}

// ItemRow is an item's row in the items table, as the board shows it.
type ItemRow struct {
	Project       string
	Item          string
	State         core.State
	Round         int // rounds used (FR-6 AC5)
	RoundsAllowed int
	Blocking      int // blocking finding count
	Host          string
	NextAction    string
	WaitReason    string
	// EnteredAt is when the item entered State. RecordDecision sets it
	// from the record's time when the state changes, and keeps the stored
	// one otherwise.
	EnteredAt time.Time
}

// View is what the workflow knows about an item that ItemState doesn't
// hold: the host running its stage, the next action and the wait reason, as
// the board shows them (FR-11 AC1).
type View struct {
	Host       string
	NextAction string
	WaitReason string
}

// NewDecision builds the projection input for one decision from the item
// state after it, the policy and the decision record.
func NewDecision(project string, s core.ItemState, p core.Policy, rec core.DecisionRecord, v View) Decision {
	return Decision{
		WorkflowID: s.WorkflowID,
		Claim:      s.Claim,
		Record:     rec,
		Item: ItemRow{
			Project:       project,
			Item:          s.Item,
			State:         s.State,
			Round:         s.RoundsUsed,
			RoundsAllowed: s.Allowance(p),
			Blocking:      len(s.Blocking(p)),
			Host:          v.Host,
			NextAction:    v.NextAction,
			WaitReason:    v.WaitReason,
		},
	}
}

// eventStageResult is the event name of core.StageResult.
var eventStageResult = core.StageResult{}.Name()

func (d Decision) validate() error {
	switch {
	case d.WorkflowID == "":
		return errors.New("no workflow ID")
	case d.Item.Project == "" || d.Item.Item == "":
		return errors.New("no project or item")
	case d.Record.Event == "":
		return errors.New("no event")
	case d.Record.At.IsZero():
		return errors.New("no time")
	case d.Record.Seq <= 0:
		return errors.New("no decision number")
	}
	// NFR-5 AC1: a decision on a stage result names that result.
	r := d.Record.Result
	if d.Record.Event == eventStageResult && r == nil {
		return errors.New("a stage_result decision must name the stage result")
	}
	if r != nil && (r.Stage == "" || r.RunID == "" || r.Outcome == "") {
		return errors.New("the stage result needs a stage, run ID and outcome")
	}
	return nil
}

// RecordDecision writes one decision and the item's row in one
// transaction (TECHNICAL-SPEC §10, NFR-5 AC1). A retry of the same decision
// (workflow ID, claim and decision number) changes nothing. The item row
// is updated only by a decision of a later claim, or a later decision of
// the same claim, so a late retry never moves the board back.
func (p *DB) RecordDecision(ctx context.Context, d Decision) error {
	if err := d.validate(); err != nil {
		return fmt.Errorf("projection: record decision: %w", err)
	}
	basis, err := json.Marshal(toBasisJSON(d.Record.Basis))
	if err != nil {
		return fmt.Errorf("projection: record decision: %w", err)
	}
	actions := d.Record.Actions
	if actions == nil {
		actions = []string{}
	}
	acts, err := json.Marshal(actions)
	if err != nil {
		return fmt.Errorf("projection: record decision: %w", err)
	}
	var rs, rid, ro string
	if r := d.Record.Result; r != nil {
		rs, rid, ro = string(r.Stage), r.RunID, string(r.Outcome)
	}
	at := nanos(d.Record.At)

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("projection: record decision: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO decisions
		(workflow_id, claim, seq, project, item, at, event, from_state, to_state, rule,
		 tracker_state, result_stage, result_run_id, result_outcome, actions)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (workflow_id, claim, seq) DO NOTHING`,
		d.WorkflowID, d.Claim, d.Record.Seq, d.Item.Project, d.Item.Item, at,
		d.Record.Event, string(d.Record.From), string(d.Record.To), d.Record.Rule,
		string(basis), rs, rid, ro, string(acts))
	if err != nil {
		return fmt.Errorf("projection: record decision: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil // already recorded
	}
	it := d.Item
	_, err = tx.ExecContext(ctx, `INSERT INTO items
		(project, item, workflow_id, claim, seq, state, round, rounds_allowed, blocking,
		 host, entered_at, next_action, wait_reason, released, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)
		ON CONFLICT (project, item) DO UPDATE SET
			released = CASE WHEN excluded.claim > items.claim THEN 0 ELSE items.released END,
			entered_at = CASE WHEN items.state <> excluded.state THEN excluded.entered_at ELSE items.entered_at END,
			workflow_id = excluded.workflow_id, claim = excluded.claim, seq = excluded.seq,
			state = excluded.state, round = excluded.round, rounds_allowed = excluded.rounds_allowed,
			blocking = excluded.blocking, host = excluded.host, next_action = excluded.next_action,
			wait_reason = excluded.wait_reason, updated_at = excluded.updated_at
		WHERE excluded.claim > items.claim OR (excluded.claim = items.claim AND excluded.seq > items.seq)`,
		it.Project, it.Item, d.WorkflowID, d.Claim, d.Record.Seq, string(it.State), it.Round,
		it.RoundsAllowed, it.Blocking, it.Host, at, it.NextAction, it.WaitReason, at)
	if err != nil {
		return fmt.Errorf("projection: record decision: %w", err)
	}
	return tx.Commit()
}

// DecisionRow is a recorded decision as the log shows it.
type DecisionRow struct {
	Project    string
	Item       string
	WorkflowID string
	Claim      int
	Record     core.DecisionRecord
}

// Decisions returns an item's decisions, oldest first (FR-11 AC2, NFR-5).
func (p *DB) Decisions(ctx context.Context, project, item string) ([]DecisionRow, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT workflow_id, claim, seq, at, event, from_state,
		to_state, rule, tracker_state, result_stage, result_run_id, result_outcome, actions
		FROM decisions WHERE project = ? AND item = ? ORDER BY at, claim, seq`, project, item)
	if err != nil {
		return nil, fmt.Errorf("projection: decisions: %w", err)
	}
	defer rows.Close()
	var out []DecisionRow
	for rows.Next() {
		var (
			r                                 = DecisionRow{Project: project, Item: item}
			at                                int64
			from, to, basis, rs, rid, ro, act string
		)
		if err := rows.Scan(&r.WorkflowID, &r.Claim, &r.Record.Seq, &at, &r.Record.Event, &from, &to,
			&r.Record.Rule, &basis, &rs, &rid, &ro, &act); err != nil {
			return nil, fmt.Errorf("projection: decisions: %w", err)
		}
		r.Record.At = fromNanos(at)
		r.Record.From, r.Record.To = core.State(from), core.State(to)
		var b basisJSON
		if err := json.Unmarshal([]byte(basis), &b); err != nil {
			return nil, fmt.Errorf("projection: decisions: tracker state: %w", err)
		}
		r.Record.Basis = b.basis()
		if err := json.Unmarshal([]byte(act), &r.Record.Actions); err != nil {
			return nil, fmt.Errorf("projection: decisions: actions: %w", err)
		}
		if len(r.Record.Actions) == 0 {
			r.Record.Actions = nil
		}
		if rid != "" {
			r.Record.Result = &core.ResultRef{Stage: contract.Stage(rs), RunID: rid, Outcome: contract.Outcome(ro)}
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("projection: decisions: %w", err)
	}
	return out, nil
}

// basisJSON is the stored form of the tracker state a decision saw
// (decisions.tracker_state, work plan D4).
type basisJSON struct {
	Head             string   `json:"head"`
	ReviewCommit     string   `json:"review_commit"`
	ReviewKind       string   `json:"review_kind"`
	RoundsUsed       int      `json:"rounds_used"`
	RoundAllowance   int      `json:"round_allowance"`
	BaseUpdates      int      `json:"base_updates"`
	BlockingFindings []string `json:"blocking_findings"`
	UnclearedManual  []string `json:"uncleared_manual"`
}

func toBasisJSON(b core.Basis) basisJSON {
	return basisJSON{
		Head: b.Head, ReviewCommit: b.ReviewCommit, ReviewKind: string(b.ReviewKind),
		RoundsUsed: b.RoundsUsed, RoundAllowance: b.RoundAllowance, BaseUpdates: b.BaseUpdates,
		BlockingFindings: orEmpty(b.BlockingFindings), UnclearedManual: orEmpty(b.UnclearedManual),
	}
}

func (b basisJSON) basis() core.Basis {
	return core.Basis{
		Head: b.Head, ReviewCommit: b.ReviewCommit, ReviewKind: contract.ReviewKind(b.ReviewKind),
		RoundsUsed: b.RoundsUsed, RoundAllowance: b.RoundAllowance, BaseUpdates: b.BaseUpdates,
		BlockingFindings: orNil(b.BlockingFindings), UnclearedManual: orNil(b.UnclearedManual),
	}
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func orNil(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}
