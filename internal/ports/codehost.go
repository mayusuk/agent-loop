package ports

import (
	"context"
	"time"
)

// CodeHost is the code-host side of a project: changes, reviews, gates,
// trust and merge (INTEGRATION-SPEC §8.3).
type CodeHost interface {
	Capabilities() HostCaps
	Validate(ctx context.Context, p ProfileCodeHost) []Problem

	ChangeForItem(ctx context.Context, item ItemRef) (*Change, error) // §6.2, nil if none
	Change(ctx context.Context, ref ChangeRef) (Change, error)        // state, head, base, fork?, merged by, labels
	Changes(ctx context.Context, since Cursor) ([]ChangeEvent, Cursor, error)
	OpenChange(ctx context.Context, n NewChange, as Identity, key IdemKey) (ChangeRef, error)
	SetLabel(ctx context.Context, ref ChangeRef, m Marker, on bool, key IdemKey) error

	Trust(ctx context.Context, ref ChangeRef) (TrustReport, error) // §9.8
	ContainsBase(ctx context.Context, ref ChangeRef) (bool, error)
	Gates(ctx context.Context, ref ChangeRef, c Commit) ([]GateResult, error) // required gates only

	ReviewRecords(ctx context.Context, ref ChangeRef) ([]ReviewRecord, error) // reviewer-authored only
	PostReview(ctx context.Context, ref ChangeRef, r ReviewPost, key IdemKey) (ReviewRef, error)
	ResolveFinding(ctx context.Context, ref ChangeRef, f FindingID, key IdemKey) error
	ManualComments(ctx context.Context, ref ChangeRef) ([]ManualComment, error)
	PostReply(ctx context.Context, ref ChangeRef, to ThreadRef, rec Record, body UntrustedText, key IdemKey) error

	Merge(ctx context.Context, ref ChangeRef, pinned Commit, m MergeMethod, key IdemKey) (MergeOutcome, error)

	PlanAt(ctx context.Context, dir string) (PlanVersion, error) // latest base commit that changed dir, its change, merger and files
}

// HostCaps are the features a code host supports.
type HostCaps struct {
	PushRule          bool // an enforced push rule is the FR-13 AC5 push record (D40)
	FileLevelComments bool // findings with a file but no line get their own thread (I8)
	MaxBody           int  // longest body or comment, in bytes
}

// ReviewRef is the code-host-native ID of a review.
type ReviewRef string

// ThreadRef is the code-host-native ID of a review thread, or of the change
// itself for replies to findings with no thread.
type ThreadRef string

// FindingID is a finding's ID on its change: "F3".
type FindingID string

// MergeMethod is how a change is merged.
type MergeMethod string

const (
	MergeMethodSquash MergeMethod = "squash"
	MergeMethodMerge  MergeMethod = "merge"
	MergeMethodRebase MergeMethod = "rebase"
)

// ChangeState is whether a change is open, closed without merging, or merged.
type ChangeState string

const (
	ChangeStateOpen   ChangeState = "open"
	ChangeStateClosed ChangeState = "closed"
	ChangeStateMerged ChangeState = "merged"
)

// Change is a change (pull request) on the code host.
type Change struct {
	Ref      ChangeRef
	URL      string
	Item     ItemRef // the item it is linked to (§6.2)
	Branch   string
	Base     string // base branch
	Head     Commit
	State    ChangeState
	Fork     bool // the head is in another repository
	Author   Actor
	MergedBy *Actor
	MergedAt time.Time
	Markers  []Marker // labels mapped back to markers
	// Plan is the plan key of a plan proposal: the coder's proposal record
	// on a branch under <branch_prefix>plan/ (§6.1). Empty otherwise.
	Plan string
}

// NewChange is a change the worker opens for an item. Title and Body come
// from the handler's result.
type NewChange struct {
	Item   ItemRef
	Branch string
	Base   string
	Title  UntrustedText
	Body   UntrustedText
	Record Record // the change record (§6.2)
}

// ChangeEvent is one change to a change, from the code host's change feed.
type ChangeEvent struct {
	Change ChangeRef
	At     time.Time
	Detail ChangeEventDetail
}

// ChangeEventDetail is one of HeadMoved, Merged, ChangeClosed,
// ChangeReopened, Opened, ReviewSubmitted, ThreadResolved, ThreadUnresolved,
// ReviewDismissed or GateChanged.
type ChangeEventDetail interface{ changeEvent() }

// Pusher is who moved a change's head, worked out as §9.8 step 6 says (I11).
type Pusher string

const (
	PusherCoder   Pusher = "coder"
	PusherOwner   Pusher = "owner"
	PusherUnknown Pusher = "unknown"
)

// HeadMoved: the head moved to Commit.
type HeadMoved struct {
	Commit Commit
	Pusher Pusher
}

// Merged: Actor merged the change.
type Merged struct{ Actor Actor }

// ChangeClosed: Actor closed the change without merging.
type ChangeClosed struct{ Actor Actor }

// ChangeReopened: the change was reopened.
type ChangeReopened struct{}

// Opened: Actor opened a change linked to Item.
type Opened struct {
	Actor Actor
	Item  ItemRef
}

// ReviewSubmitted: Actor submitted a review.
type ReviewSubmitted struct{ Actor Actor }

// ThreadResolved: Actor resolved a review thread.
type ThreadResolved struct{ Actor Actor }

// ThreadUnresolved: Actor unresolved a review thread.
type ThreadUnresolved struct{ Actor Actor }

// ReviewDismissed: Actor dismissed a review.
type ReviewDismissed struct{ Actor Actor }

// GateChanged: a gate on Commit changed state.
type GateChanged struct{ Commit Commit }

func (HeadMoved) changeEvent()        {}
func (Merged) changeEvent()           {}
func (ChangeClosed) changeEvent()     {}
func (ChangeReopened) changeEvent()   {}
func (Opened) changeEvent()           {}
func (ReviewSubmitted) changeEvent()  {}
func (ThreadResolved) changeEvent()   {}
func (ThreadUnresolved) changeEvent() {}
func (ReviewDismissed) changeEvent()  {}
func (GateChanged) changeEvent()      {}

// TrustCause is why a change is untrusted (§9.8).
type TrustCause string

const (
	TrustFork                TrustCause = "fork"
	TrustBranchOutsidePrefix TrustCause = "branch_outside_prefix" // I14
	TrustRulesetMissing      TrustCause = "ruleset_missing"
	TrustRulesetChanged      TrustCause = "ruleset_changed" // also disabled and re-enabled (I9)
	TrustAdminNotOwner       TrustCause = "admin_not_owner"
	TrustPushedBeforeRuleset TrustCause = "pushed_before_ruleset"
	TrustPusherUnknown       TrustCause = "pusher_unknown" // I11
)

// TrustReport is the result of the pre-stage trust check. A change with no
// Causes is trusted. Commits are the commits not reachable from the base tip
// that the causes make untrusted. The adapter doesn't apply owner approvals:
// the core drops approved commits (FR-13 AC6) and never approves a fork.
type TrustReport struct {
	Causes  []TrustCause
	Commits []Commit
}

// GateState is the state of one required gate on a commit (§9.7).
type GateState string

const (
	GatePass    GateState = "pass"
	GateFail    GateState = "fail"
	GatePending GateState = "pending" // queued or in progress
	GateMissing GateState = "missing" // no run at all
)

// GateResult is one required gate on a commit.
type GateResult struct {
	Name   string
	State  GateState
	Commit Commit
}

// ReviewRecord is a review the reviewer identity posted, with its record
// (§6.3).
type ReviewRecord struct {
	Ref         ReviewRef
	Author      Actor
	SubmittedAt time.Time
	Record      Record                  // the review record
	Threads     map[FindingID]ThreadRef // the thread of each finding that has one
}

// ReviewPost is a review the worker posts as the reviewer identity. The
// adapter builds the status table from Record (§6.3, I8).
type ReviewPost struct {
	Commit   Commit
	Approve  bool // approve the change (FR-4 AC7); otherwise comment
	Summary  UntrustedText
	Findings []FindingPost // new findings, each posted with this review
	Record   Record        // the review record
}

// FindingPost is one new finding. Line 0 is a file-level finding, and an
// empty Path is a finding with no file, posted in the review text (I8).
type FindingPost struct {
	ID     FindingID
	Score  int
	Status string
	Path   string
	Line   int
	Body   UntrustedText
	Record Record // the finding record
}

// ManualCommentKind is where a manual comment is (§9.6).
type ManualCommentKind string

const (
	ManualCommentThread                ManualCommentKind = "thread"
	ManualCommentReviewBody            ManualCommentKind = "review_body"
	ManualCommentEmptyChangesRequested ManualCommentKind = "empty_changes_requested"
)

// ClearedBy is how a human cleared a manual comment. Ruling-based clearing
// is worked out by the core, not reported here.
type ClearedBy string

const (
	ClearedByNone           ClearedBy = ""
	ClearedByHumanResolved  ClearedBy = "human_resolved"
	ClearedByHumanDismissed ClearedBy = "human_dismissed"
)

// ManualComment is a review comment or review body from a manual commenter
// (§9.6).
type ManualComment struct {
	ID        string // "MC-<code-host id>"
	Author    Actor
	Kind      ManualCommentKind
	Thread    ThreadRef // its thread, for kind ManualCommentThread
	Cleared   bool
	ClearedBy ClearedBy
	Body      UntrustedText
	CreatedAt time.Time
}

// MergeOutcome is the result of a merge call (§8.3, I15).
type MergeOutcome string

const (
	MergeMerged       MergeOutcome = "merged"
	MergeHeadMoved    MergeOutcome = "head_moved"
	MergeNotMergeable MergeOutcome = "not_mergeable"
	MergeFailed       MergeOutcome = "failed"
)

// PlanVersion is a plan directory as of the latest commit on the base
// branch that changed it (FR-17 AC8, #156 D2).
type PlanVersion struct {
	Commit Commit
	// Change is the merged change that brought Commit into the base branch,
	// and MergedBy who merged it. Change is empty and MergedBy nil when no
	// merged change contains Commit, as after a direct push (#156 D3). The
	// adapter reports the merger by stable ID; the core decides whether it
	// is an owner (FR-13 AC7).
	Change   ChangeRef
	MergedBy *Actor
	MergedAt time.Time
	Files    []PlanFile // every file under the directory at Commit, by path
}

// PlanFile is one file of a plan directory. Path is relative to the
// directory ("tasks.yaml", "tasks/login-api.md"). Content is empty for a
// file over MaxPlanFile bytes (#156 D4).
type PlanFile struct {
	Path    string
	Size    int64
	Content UntrustedText
}

// MaxPlanFile is the largest plan file PlanAt returns the content of.
const MaxPlanFile = 1 << 20
