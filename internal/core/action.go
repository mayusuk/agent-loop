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
	// Trust, when set, asks the workflow for the pre-stage trust check
	// before the start (FR-13 AC5, TECHNICAL-SPEC §8.3, work plan #11 D2).
	// An untrusted change comes back as TrustRefused, and nothing starts.
	Trust *TrustCheck
}

// TrustCheck is what the workflow needs for the pre-stage trust check: the
// commits the owner approved (FR-13 AC6). The workflow starts the run only
// when TrustReport.Check(Approved) is trusted.
type TrustCheck struct {
	Approved []string
}

// UpdateBase makes the agent-free base merge as the coder identity (D41,
// INTEGRATION-SPEC §5.3). Its outcome is BaseUpdated, BaseConflicts or
// BaseUpdateRefused.
type UpdateBase struct {
	Key    string
	Change string
	Head   string
}

// CheckMerge asks the workflow to read the merge facts for the change and
// answer with MergeChecked (FR-7, work plan #11 D1). Recheck says to wait
// one poll interval first, because a required gate has no result yet
// (TECHNICAL-SPEC §8.1 cause 6).
type CheckMerge struct {
	Change  string
	Head    string
	Recheck bool
}

// Merge merges the change as the loop identity, pinned to the reviewed
// commit, with the profile's merge method (FR-7 AC5, AC7). Its outcome
// comes back as MergeResult.
type Merge struct {
	Key    string
	Change string
	Pinned string
}

// FileFollowUp files the follow-up item for findings left open after the
// loop's merge (FR-8). The workflow takes its type and status from the
// profile, writes the row links from the comment IDs, links the item and
// change, and comments on the item with a link to it (FR-8 AC2, AC5).
type FileFollowUp struct {
	Key    string // followup:<change> (INTEGRATION-SPEC §6.6, FR-8 AC3)
	Item   string
	Change string
	Rows   []FollowUpRow
}

// FollowUpRow is one unresolved finding in a follow-up. It has references
// only, never a title or reviewer text (FR-8 AC1, §8.5).
type FollowUpRow struct {
	ID      string
	Score   int
	Status  contract.FindingStatus
	Comment string // code-host comment ID, for the link
}

// ApproveCommits writes an approved_commits record: the owner approved these
// commits by resuming (FR-13 AC6, INTEGRATION-SPEC §9.8 step 4).
type ApproveCommits struct {
	Key     string
	Commits []string
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
	Commits     []string
	Gates       []string
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
func (Merge) Kind() string          { return "merge" }
func (FileFollowUp) Kind() string   { return "file_follow_up" }
func (ApproveCommits) Kind() string { return "approve_commits" }
func (SetMarker) Kind() string      { return "set_marker" }
func (PostStatus) Kind() string     { return "post_status" }
func (Notify) Kind() string         { return "notify" }
func (Wait) Kind() string           { return "wait" }
func (RecordDecision) Kind() string { return "record_decision" }

func (RunStage) isAction()       {}
func (UpdateBase) isAction()     {}
func (CheckMerge) isAction()     {}
func (Merge) isAction()          {}
func (FileFollowUp) isAction()   {}
func (ApproveCommits) isAction() {}
func (SetMarker) isAction()      {}
func (PostStatus) isAction()     {}
func (Notify) isAction()         {}
func (Wait) isAction()           {}
func (RecordDecision) isAction() {}
