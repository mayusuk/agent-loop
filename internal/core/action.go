package core

import (
	"time"

	"github.com/mayusuk/agent-loop/contract"
)

// Action is something the workflow does for a decision (TECHNICAL-SPEC §4).
// Every write carries an idempotency key (TECHNICAL-SPEC §9).
type Action interface {
	// Kind is the action's name in decision records.
	Kind() string
	isAction()
}

// RunStage starts an agent stage run. Before starting, the workflow asks
// the project for a daily-limit unit; a refusal comes back as
// DailyLimitReached (FR-16 AC3, work plan D12).
type RunStage struct {
	RunID    string
	Stage    contract.Stage
	Attempt  int
	Deadline time.Time
	Change   string
	Head     string
	Review   *ReviewSpec
	Address  *AddressSpec
}

// UpdateBase makes the agent-free base merge as the coder identity (D41,
// INTEGRATION-SPEC §5.3). Its outcome is BaseUpdated, BaseConflicts or
// BaseUpdateRefused.
type UpdateBase struct {
	Key    string
	Change string
	Head   string
}

// CheckMerge enters merging: the merge conditions are checked for Head
// (FR-7). The merge rules are T6's (work plan D21).
type CheckMerge struct {
	Change string
	Head   string
}

// SetMarker sets or clears a loop marker on the item.
type SetMarker struct {
	Key    string
	Marker string
	On     bool
}

// The markers this task sets (INTEGRATION-SPEC §8.1 spellings).
const (
	MarkerNeedsOwner = "needs_owner"
	MarkerBlocked    = "blocked"
)

// PostStatus posts a loop status comment with a status record
// (INTEGRATION-SPEC §6.5). The workflow writes the plain-language text and
// mentions the owner accounts on an escalation.
type PostStatus struct {
	Key         string
	Event       contract.StatusEvent
	Reason      string
	Stage       contract.Stage
	FindingIDs  []string
	CommentIDs  []string
	Dependency  string
	RoundsAdded *int
}

// Notify notifies the owner (FR-11 AC3).
type Notify struct {
	Key    string
	Event  contract.StatusEvent
	Reason string
}

// Wait says the item waits; nothing runs until another event.
type Wait struct{ Reason string }

// RecordDecision is the decision record of this call (NFR-5 AC1, work plan
// D1). It is always the last action.
type RecordDecision struct{ Record DecisionRecord }

// DecisionRecord names what a decision was based on and what it did.
type DecisionRecord struct {
	Seq     int
	At      time.Time
	Event   string
	From    State
	To      State
	Basis   Basis
	Result  *ResultRef
	Rule    string
	Actions []string
}

// Basis is the tracker state a decision saw.
type Basis struct {
	Head             string
	ReviewCommit     string
	ReviewKind       contract.ReviewKind
	RoundsUsed       int
	RoundAllowance   int
	BaseUpdates      int
	BlockingFindings []string
	UnclearedManual  []string
}

// ResultRef names the stage result a decision was based on.
type ResultRef struct {
	Stage   contract.Stage
	RunID   string
	Outcome contract.Outcome
}

// Kind implementations.
func (RunStage) Kind() string       { return "run_stage" }
func (UpdateBase) Kind() string     { return "update_base" }
func (CheckMerge) Kind() string     { return "check_merge" }
func (SetMarker) Kind() string      { return "set_marker" }
func (PostStatus) Kind() string     { return "post_status" }
func (Notify) Kind() string         { return "notify" }
func (Wait) Kind() string           { return "wait" }
func (RecordDecision) Kind() string { return "record_decision" }

func (RunStage) isAction()       {}
func (UpdateBase) isAction()     {}
func (CheckMerge) isAction()     {}
func (SetMarker) isAction()      {}
func (PostStatus) isAction()     {}
func (Notify) isAction()         {}
func (Wait) isAction()           {}
func (RecordDecision) isAction() {}
