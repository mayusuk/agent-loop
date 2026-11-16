package ports

import (
	"encoding/json"
	"time"
)

// ItemRef is the tracker-native key of an item: "42", "PROJ-42".
type ItemRef string

// ChangeRef is the code-host-native key of a change: "57".
type ChangeRef string

// Commit is a full commit SHA.
type Commit string

// Version is an opaque concurrency token for an item (etag, updated-at,
// revision).
type Version string

// Cursor is an opaque position in an adapter's change feed.
type Cursor string

// IdemKey is the idempotency key of a write (TECHNICAL-SPEC §9), for example
// "item/eagain/42#1:escalate:7". A write repeated with the same key leaves one
// result (NFR-2).
type IdemKey string

// CommentRef is the tracker-native ID of a comment on an item.
type CommentRef string

// ActorKind tells a human account from a bot or app account.
type ActorKind string

const (
	ActorHuman ActorKind = "human"
	ActorBot   ActorKind = "bot"
)

// Actor is an account that acted in the tracker or code host. ID is the
// account's stable ID (on GitHub the numeric user or app ID, on Jira the
// accountId), and is what the core compares with the profile (I13). Login is
// for people reading logs.
type Actor struct {
	ID, Login string
	Kind      ActorKind
}

// Marker is a tracker-neutral state or type marker (INTEGRATION-SPEC §8.1).
// The profile maps each one to a label or workflow status.
type Marker string

const (
	MarkerReady        Marker = "ready"
	MarkerInProgress   Marker = "in_progress"
	MarkerBlocked      Marker = "blocked"
	MarkerNeedsOwner   Marker = "needs_owner"
	MarkerPaused       Marker = "paused"
	MarkerAborted      Marker = "aborted"
	MarkerFollowUp     Marker = "follow_up"
	MarkerPlan         Marker = "plan"
	MarkerPlanProposal Marker = "plan_proposal"
	MarkerTypeFeature  Marker = "type:feature"
	MarkerTypeBug      Marker = "type:bug"
)

// Identity is one of the loop's three identities (PRODUCT-SPEC §4.1).
type Identity string

const (
	IdentityCoder    Identity = "coder"
	IdentityReviewer Identity = "reviewer"
	IdentityLoop     Identity = "loop"
)

// RecordType is the type of a structured record (INTEGRATION-SPEC §6.1).
type RecordType string

const (
	RecordChange          RecordType = "change"
	RecordReview          RecordType = "review"
	RecordFinding         RecordType = "finding"
	RecordReply           RecordType = "reply"
	RecordManualReply     RecordType = "manual_reply"
	RecordStatus          RecordType = "status"
	RecordApprovedCommits RecordType = "approved_commits"
	RecordFollowUp        RecordType = "followup"
	RecordTask            RecordType = "task"
	RecordPlan            RecordType = "plan"
	RecordProposal        RecordType = "proposal"
)

// Record is a structured record: the hidden marker the worker writes at the
// end of a body or comment (INTEGRATION-SPEC §6.1). Data is the marker's
// whole JSON object, encoded and decoded by contract/. Adapters write Data
// into the marker, look records up in the fixed place for their Type (I12),
// and return only records whose author may write that Type.
type Record struct {
	Type RecordType
	Key  IdemKey
	Data json.RawMessage

	// Set by adapters on records they read.
	Author Actor
	Ref    CommentRef // the comment or body that holds the record, if any
	At     time.Time
}

// Severity is how serious a validation problem is.
type Severity string

const (
	SeverityError   Severity = "error"   // project validate fails
	SeverityWarning Severity = "warning" // project validate warns
)

// Problem is one finding of an adapter's Validate (project validate).
type Problem struct {
	Severity Severity
	Code     string // stable, for example "ruleset_missing"
	Message  string // what is wrong
	Hint     string // how to fix it
}

// OwnerAccount is an owner account from the profile. ID is the stable account
// ID and is what counts. Login is for people reading the file (I13).
type OwnerAccount struct {
	Login, ID string
}

// MarkerMapping is how one marker is represented in the tracker: a label or
// a workflow status (INTEGRATION-SPEC §13).
type MarkerMapping struct {
	Label  string
	Status string
}

// ProfileIdentities are the secret references of the three identities.
type ProfileIdentities struct {
	Coder, Reviewer, Loop string // for example "secret://github-app/coder"
}

// ProfileTracker is the part of the project profile a WorkTracker validates.
type ProfileTracker struct {
	Adapter    string // "github"
	Repo       string // "owner/name" on GitHub
	Markers    map[Marker]MarkerMapping
	Identities ProfileIdentities
	Owners     []OwnerAccount
	Settings   map[string]any // adapter-specific keys, checked by the adapter
}

// ProfileCodeHost is the part of the project profile a CodeHost validates.
type ProfileCodeHost struct {
	Adapter           string // "github"
	Repo              string
	BaseBranch        string
	BranchPrefix      string // "agent/"
	Markers           map[Marker]MarkerMapping
	Identities        ProfileIdentities
	Owners            []OwnerAccount
	Gates             []string // replaces the base branch's required checks when set
	ManualCommentBots []string // bot logins whose comments count as manual (I7)
	MergeMethod       MergeMethod
	ReleaseApps       []string // app IDs allowed to bypass the tag ruleset
	Settings          map[string]any
}
