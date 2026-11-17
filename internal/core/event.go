package core

import (
	"time"

	"github.com/mayusuk/agent-loop/contract"
)

// Event is a normalised fact from an adapter, a stage run or the owner
// (TECHNICAL-SPEC §4). Every event carries the time it happened, which is
// the only clock Decide reads.
type Event interface {
	// When is the time of the event.
	When() time.Time
	// Name is the event's name in decision records.
	Name() string
	isEvent()
}

// At is embedded in every event.
type At struct{ Time time.Time }

// When returns the event time.
func (a At) When() time.Time { return a.Time }
func (At) isEvent()          {}

// Started: the item was claimed and its workflow begins (FR-2 AC5). Change
// is its open change, if one exists (FR-3 AC3), and Reviews its review
// records in order (FR-6 AC5, work plan D3, D22).
type Started struct {
	At
	WorkflowID  string
	Item        string
	Claim       int
	Change      *Change
	Reviews     []ReviewRecord
	RoundsAdded int
	// ApprovedCommits are the commits of the item's approved_commits
	// records (FR-13 AC6, work plan #11 D5).
	ApprovedCommits []string
	// ClosedChanges is the number of the item's changes closed without
	// merging (D44, INTEGRATION-SPEC §6.2).
	ClosedChanges int
}

// ReviewRecord is one structured review record (INTEGRATION-SPEC §6.3).
type ReviewRecord struct {
	Commit   string
	Kind     contract.ReviewKind
	Stale    bool
	Findings []Finding
}

// StageResult is a handler's result as the worker validated and posted it
// (FR-18). A missing, malformed or disallowed result arrives as failed
// (FR-18 AC3). Only the outcome object named by Outcome is read.
type StageResult struct {
	At
	RunID   string
	Stage   contract.Stage
	Outcome contract.Outcome
	// Reason is the failed reason, for the log only.
	Reason    string
	Implement *ImplementOutcome
	Review    *ReviewOutcome
	Address   *AddressOutcome
	Blocked   *BlockedOutcome
}

// ImplementOutcome is a done implement: the change the worker opened or
// adopted (FR-3 AC1).
type ImplementOutcome struct {
	Change string
	Head   string
}

// ReviewOutcome is a posted review (FR-4 AC2). Findings is every finding on
// the change with its status after this review, new ones with their
// worker-assigned IDs (INTEGRATION-SPEC §5.2, §6.3).
type ReviewOutcome struct {
	Commit        string
	Kind          contract.ReviewKind
	Stale         bool
	Findings      []Finding
	ManualRulings []ManualRuling
}

// ManualRuling is a review's ruling on a reply to a manual comment
// (FR-4 AC8).
type ManualRuling struct {
	Comment string
	Ruling  contract.ManualRulingValue
}

// AddressOutcome is a done address run (FR-5 AC7). Counts on each reply is
// the worker's check that a deferred reply names an open item (FR-5 AC5,
// work plan D15).
type AddressOutcome struct {
	Head          string
	Replies       []ReplyOutcome
	ManualReplies []ReplyOutcome
}

// ReplyOutcome is one reply to a finding or a manual comment.
type ReplyOutcome struct {
	To     string
	Kind   contract.ReplyKind
	Counts bool
}

// DependencyState is the state of a dependency a blocked result names
// (work plan D14).
type DependencyState string

// The dependency states.
const (
	DependencyOpen    DependencyState = "open"
	DependencyDone    DependencyState = "done"
	DependencyNotDone DependencyState = "not_done"
)

// BlockedOutcome is a blocked result (FR-18 AC1).
type BlockedOutcome struct {
	Dependency string
	State      DependencyState
}

// StageCrashed: the run ended because a worker or the server crashed, with
// no result (FR-16 AC1, D42).
type StageCrashed struct {
	At
	RunID string
}

// StageTimedOut: the run was stopped at its time limit (FR-18 AC4).
type StageTimedOut struct {
	At
	RunID string
}

// DailyLimitReached: the project refused the pending agent run because the
// daily limit is reached (FR-16 AC3, work plan D12).
type DailyLimitReached struct {
	At
	RunID string
}

// HeadMoved: the change's head moved (FR-12 AC4 owner-push row). Commits
// are the commits the push added, the head among them (INTEGRATION-SPEC
// §9.8 step 3); a push with several commits names them all, so an untrusted
// push escalates, and is approved, as a whole (work plan #11 D6).
type HeadMoved struct {
	At
	Commit  string
	Pusher  Pusher
	Commits []string
}

// ManualCommentChanged: a manual review comment appeared or its clearing
// changed (FR-7 AC9). Reopened drops an addressed ruling, so the comment
// blocks again.
type ManualCommentChanged struct {
	At
	Comment      string
	Empty        bool
	HumanCleared bool
	Reopened     bool
}

// BaseUpdated: the worker merged the base without conflicts, with no agent
// run (D41, INTEGRATION-SPEC §5.3).
type BaseUpdated struct {
	At
	Head string
}

// BaseConflicts: the base merge has conflicts, so an address run in
// base_update mode resolves them (FR-5 AC6).
type BaseConflicts struct{ At }

// BaseUpdateRefused: the branch head moved before the base merge was
// pushed. Nothing was pushed (INTEGRATION-SPEC §5.3 step 2).
type BaseUpdateRefused struct{ At }

// Resumed: the owner resumed an escalated item (FR-10 AC2).
type Resumed struct{ At }

// DependencyClosed: the dependency a blocked item waits for was closed.
type DependencyClosed struct {
	At
	Dependency string
	Done       bool
}

// BlockRemoved: the owner removed blocked (FR-12 AC4).
type BlockRemoved struct{ At }

// DayBoundary: the daily count was reset (FR-16 AC4).
type DayBoundary struct{ At }

// ProjectResumed: the project or the deployment pause ended (FR-10 AC3,
// AC8). A merging item gets a new merge time limit, because the gate held
// its checks during the pause (work plan #12 D12).
type ProjectResumed struct{ At }

// Name implementations.
func (Started) Name() string              { return "started" }
func (StageResult) Name() string          { return "stage_result" }
func (StageCrashed) Name() string         { return "stage_crashed" }
func (StageTimedOut) Name() string        { return "stage_timed_out" }
func (DailyLimitReached) Name() string    { return "daily_limit_reached" }
func (HeadMoved) Name() string            { return "head_moved" }
func (ManualCommentChanged) Name() string { return "manual_comment_changed" }
func (BaseUpdated) Name() string          { return "base_updated" }
func (BaseConflicts) Name() string        { return "base_conflicts" }
func (BaseUpdateRefused) Name() string    { return "base_update_refused" }
func (Resumed) Name() string              { return "resumed" }
func (DependencyClosed) Name() string     { return "dependency_closed" }
func (BlockRemoved) Name() string         { return "block_removed" }
func (DayBoundary) Name() string          { return "day_boundary" }
func (TrustRefused) Name() string         { return "trust_refused" }
func (ProjectResumed) Name() string       { return "project_resumed" }
func (MergeChecked) Name() string         { return "merge_checked" }
func (MergeResult) Name() string          { return "merge_result" }
