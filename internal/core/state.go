package core

import (
	"slices"
	"time"

	"github.com/mayusuk/agent-loop/contract"
)

// State is an item's lifecycle state (PRODUCT-SPEC §6).
type State string

// The item states. StateNone is an item whose workflow hasn't started.
const (
	StateNone         State = ""
	StateImplementing State = "implementing"
	StateInReview     State = "in_review"
	StateAddressing   State = "addressing"
	StateMerging      State = "merging"
	StateMerged       State = "merged"
	StateBlocked      State = "blocked"
	StateEscalated    State = "escalated"
	StateWaitingLimit State = "waiting_limit"
	StatePaused       State = "paused"
	StateAborted      State = "aborted"
	StateClosed       State = "closed"
	// StateReleased: the owner released the claim; the item left the loop
	// (§6 owner-actions table, FR-12 AC4).
	StateReleased State = "released"
)

// StageMerge is the deterministic merge stage. It has no handler, so it is
// not one of the contract's stages (FR-18 AC5).
const StageMerge contract.Stage = "merge"

// Pusher says who moved a change's head (FR-6 AC6, TECHNICAL-SPEC §8.3).
type Pusher string

// The pushers. PusherUnknown is an untrusted push (FR-13 AC5, trust.go).
const (
	PusherCoder   Pusher = "coder"
	PusherOwner   Pusher = "owner"
	PusherUnknown Pusher = "unknown"
)

// Escalation reason codes (INTEGRATION-SPEC §6.5, work plan D13).
const (
	ReasonRoundLimit          = contract.ReasonRoundLimit
	ReasonWorkerLost          = contract.ReasonWorkerLost
	ReasonRetriesExhausted    = "retries_exhausted"
	ReasonTimeLimit           = "time_limit"
	ReasonNeedsOwner          = "needs_owner"
	ReasonBlockedNoDependency = "blocked_no_dependency"
	ReasonDependencyNotDone   = "dependency_not_done"
	ReasonBaseUpdateLimit     = "base_update_limit"

	// Merge and trust escalations (FR-7 AC6, FR-13 AC5, work plan #11 D4,
	// D7, D10).
	ReasonUntrustedCommit           = contract.ReasonUntrustedCommit
	ReasonBranchOutsidePrefix       = contract.ReasonBranchOutsidePrefix
	ReasonPusherUnknown             = contract.ReasonPusherUnknown
	ReasonForkChange                = "fork_change"
	ReasonEmptyReviewNeedsDismissal = contract.ReasonEmptyReviewNeedsDismissal
	ReasonGateFailed                = contract.ReasonGateFailed
	ReasonMergeRefused              = "merge_refused"
	ReasonMergeFailed               = "merge_failed"

	// Owner actions (FR-12 AC4, work plan #12 D4, D9).
	ReasonOwnerEscalated = "owner_escalated"
	ReasonChangeClosed   = "change_closed"
	ReasonPaused         = "paused"
	ReasonItemClosed     = "item_closed"
	ReasonMergedByHand   = "merged_by_hand"
	ReasonReleased       = "released"
	ReasonChangeReopened = "change_reopened"
)

// MaxCrashReruns is how many times one attempt runs again after a crash
// before the item escalates (FR-16 AC1, D42).
const MaxCrashReruns = 3

// ItemState is everything the core knows about one claimed item
// (TECHNICAL-SPEC §4). It holds references and facts only, never tracker
// text. Decide never changes the value it is given.
type ItemState struct {
	State State

	// WorkflowID and Claim make idempotency keys and run IDs
	// (TECHNICAL-SPEC §6.1, §9).
	WorkflowID string
	Item       string
	Claim      int

	// Change is the item's open change, nil before implement.
	Change *Change

	// Run is the current or pending stage run. In waiting_limit it is the
	// run the daily limit refused (work plan D12).
	Run *Run

	// RoundsUsed counts non-stale round reviews (FR-6 AC5). RoundsAdded is
	// the allowance added by resumes (FR-10 AC5).
	RoundsUsed  int
	RoundsAdded int
	// BaseUpdates counts non-stale base_update reviews (FR-6 AC7, D8).
	BaseUpdates int
	// UncountedReviews counts every non-stale review that isn't a round
	// (base_update, manual_reply, owner_push) under this claim. It tells
	// apart two such reviews of one commit, such as two manual-reply
	// reviews after two address runs that left the head (work plan #25
	// D4).
	UncountedReviews int

	// Findings is every finding on the change, as the latest non-stale
	// review left it (INTEGRATION-SPEC §6.3).
	Findings []Finding
	// LatestReview is the latest non-stale review, nil before the first.
	LatestReview *Review

	ManualComments []ManualComment
	// PendingRulings are manual comments replied to that no review has
	// ruled on yet (FR-4 AC8).
	PendingRulings []string
	// NextKind is the kind the next review must have because an address
	// run or base update came before it (FR-6 AC6, D5, D6). Empty when
	// nothing is waiting.
	NextKind contract.ReviewKind
	// OwnerPushed is set when an owner push moved the head since the
	// latest review (FR-6 AC6).
	OwnerPushed bool

	Escalation *Escalation
	// BlockedOn is the dependency a blocked item waits for.
	BlockedOn string
	// UpdatingBase is set while the agent-free base merge runs (D41).
	UpdatingBase bool

	// ApprovedCommits are the commits the owner approved after an
	// untrusted-change escalation (FR-13 AC6).
	ApprovedCommits []string
	// UnknownPushes are head moves with pusher Unknown seen while a review
	// runs; they escalate the item when the review ends (FR-13 AC5, work
	// plan #11 D6).
	UnknownPushes []string
	// Merge is the merge stage while the item is merging, nil otherwise.
	Merge *MergeStage

	// Running is the stage state whose work (a handler run, the agent-free
	// base merge or a merge call) is still in flight while an owner action
	// holds the item in paused, aborted, escalated or released (work plan
	// #12 D1). Empty when nothing is in flight under a hold.
	Running State
	// PendingEscalation: Escalation is recorded but not announced yet,
	// because the item is paused; it is escalated when it resumes, or when
	// its in-flight work ends under an owner escalation (work plan #12 D3).
	PendingEscalation bool
	// ResumeAddress: the owner resumed a round-limit escalation with a
	// blocking finding while the item was paused, so leaving the pause
	// starts with an address run (FR-10 AC5, work plan #12 D5).
	ResumeAddress bool
	// ClosedChanges counts the item's changes closed without merging; the
	// next implement runs on branch number ClosedChanges+1 (D44,
	// INTEGRATION-SPEC §3.3).
	ClosedChanges int

	// Seq counts decisions; it is part of every idempotency key.
	Seq int
	// Starts counts handler starts in this claim; it makes each run_id
	// unique (work plan D11).
	Starts int
}

// Change is the item's change as the core sees it.
type Change struct {
	Ref    string
	Head   string
	Pusher Pusher
}

// Run is one stage run.
type Run struct {
	Stage    contract.Stage
	Attempt  int
	Crashes  int
	RunID    string
	Deadline time.Time

	// Review and Address are the stage inputs the core chooses.
	Review  *ReviewSpec
	Address *AddressSpec
	// StartHead is the head when an address run started (FR-5 AC2).
	StartHead string
}

// ReviewSpec is what the core tells a review run (INTEGRATION-SPEC §4.2).
type ReviewSpec struct {
	Kind   contract.ReviewKind
	Commit string
	Round  int // 0 unless Kind is round
	// Seq is ItemState.UncountedReviews when the review was asked for, for
	// a kind other than round; the review's record key holds it, so a
	// rerun finds its own review and a later review of the same commit
	// doesn't (work plan #25 D4).
	Seq          int
	RuleFindings []string
	RuleManual   []string
}

// AddressSpec is what the core tells an address run.
type AddressSpec struct {
	Mode           contract.AddressMode
	AnswerFindings []string
	AnswerManual   []string
}

// Finding is one finding's current state. Comment is the code-host ID of
// its thread, a reference only, empty for a finding with no file
// (INTEGRATION-SPEC §6.3).
type Finding struct {
	ID      string
	Score   int
	Status  contract.FindingStatus
	Comment string
}

// Unresolved reports whether the finding is open or partially addressed
// (§5).
func (f Finding) Unresolved() bool {
	return f.Status == contract.StatusOpen || f.Status == contract.StatusPartiallyAddressed
}

// Review is a non-stale review the core used.
type Review struct {
	Commit string
	Kind   contract.ReviewKind
}

// ManualComment is one manual review comment (§5, FR-7 AC9).
type ManualComment struct {
	ID string
	// Empty is an empty "changes requested" review, which only a human
	// dismissal clears (D43).
	Empty bool
	// HumanCleared: a human resolved its thread or dismissed its review.
	HumanCleared bool
	// RuledAddressed: a review ruled a reply to it addressed (FR-4 AC8).
	RuledAddressed bool
}

// Cleared reports whether the comment no longer blocks (FR-7 AC9).
func (m ManualComment) Cleared() bool { return m.HumanCleared || m.RuledAddressed }

// Escalation is why an item waits for the owner.
type Escalation struct {
	Reason string
	Stage  contract.Stage
	// AfterBlockingRound is a round-limit escalation with a blocking finding
	// (FR-6 AC3); a resume then starts with an address run (FR-10 AC5).
	AfterBlockingRound bool
	FindingIDs         []string
	CommentIDs         []string
	Dependency         string
	// Commits are the untrusted commits a trust escalation names, and Gates
	// the failed gates a gate escalation names.
	Commits []string
	Gates   []string
	// Trust marks an untrusted-change escalation, which a resume approves
	// unless the change is from a fork (FR-13 AC6).
	Trust bool
	Fork  bool
}

// Allowance is the item's round allowance (§5, FR-6 AC8).
func (s ItemState) Allowance(p Policy) int { return p.maxRounds() + s.RoundsAdded }

// Blocking returns the IDs of unresolved findings at or above the threshold.
func (s ItemState) Blocking(p Policy) []string {
	var ids []string
	for _, f := range s.Findings {
		if f.Unresolved() && f.Score >= p.blockingThreshold() {
			ids = append(ids, f.ID)
		}
	}
	return ids
}

// UnresolvedFindings returns the IDs of every unresolved finding.
func (s ItemState) UnresolvedFindings() []string {
	var ids []string
	for _, f := range s.Findings {
		if f.Unresolved() {
			ids = append(ids, f.ID)
		}
	}
	return ids
}

// UnclearedManual returns the IDs of manual comments that aren't cleared.
func (s ItemState) UnclearedManual() []string {
	var ids []string
	for _, m := range s.ManualComments {
		if !m.Cleared() {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

// answerableManual is answer_manual: uncleared comments except empty
// "changes requested" reviews (FR-5 AC1, D16).
func (s ItemState) answerableManual() []string {
	var ids []string
	for _, m := range s.ManualComments {
		if !m.Cleared() && !m.Empty {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

// Clone returns a deep copy, so Decide never aliases its input.
func (s ItemState) Clone() ItemState {
	c := s
	if s.Change != nil {
		ch := *s.Change
		c.Change = &ch
	}
	if s.Run != nil {
		r := *s.Run
		if s.Run.Review != nil {
			rv := *s.Run.Review
			rv.RuleFindings = slices.Clone(rv.RuleFindings)
			rv.RuleManual = slices.Clone(rv.RuleManual)
			r.Review = &rv
		}
		if s.Run.Address != nil {
			a := *s.Run.Address
			a.AnswerFindings = slices.Clone(a.AnswerFindings)
			a.AnswerManual = slices.Clone(a.AnswerManual)
			r.Address = &a
		}
		c.Run = &r
	}
	c.Findings = slices.Clone(s.Findings)
	if s.LatestReview != nil {
		lr := *s.LatestReview
		c.LatestReview = &lr
	}
	c.ManualComments = slices.Clone(s.ManualComments)
	c.PendingRulings = slices.Clone(s.PendingRulings)
	if s.Escalation != nil {
		e := *s.Escalation
		e.FindingIDs = slices.Clone(e.FindingIDs)
		e.CommentIDs = slices.Clone(e.CommentIDs)
		e.Commits = slices.Clone(e.Commits)
		e.Gates = slices.Clone(e.Gates)
		c.Escalation = &e
	}
	c.ApprovedCommits = slices.Clone(s.ApprovedCommits)
	c.UnknownPushes = slices.Clone(s.UnknownPushes)
	if s.Merge != nil {
		m := *s.Merge
		c.Merge = &m
	}
	return c
}
