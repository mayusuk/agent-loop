package ports

import (
	"context"
	"time"
)

// WorkTracker is the tracker side of a project: items, markers, comments,
// records and dependencies (INTEGRATION-SPEC §8.2).
type WorkTracker interface {
	Capabilities() TrackerCaps
	Validate(ctx context.Context, p ProfileTracker) []Problem

	Item(ctx context.Context, ref ItemRef) (Item, error) // markers, state, close reason, assignees, Version
	Candidates(ctx context.Context) ([]Item, error)      // open items with the ready marker
	Changes(ctx context.Context, since Cursor) ([]ItemEvent, Cursor, error)

	Claim(ctx context.Context, ref ItemRef, expect Version, key IdemKey) (Version, error) // ErrConflict if the item changed
	SetMarker(ctx context.Context, ref ItemRef, m Marker, on bool, key IdemKey) error
	Comment(ctx context.Context, ref ItemRef, rec Record, body UntrustedText, key IdemKey) (CommentRef, error)
	FindRecords(ctx context.Context, ref ItemRef, t RecordType) ([]Record, error) // author-checked (§6.1)
	CreateItem(ctx context.Context, n NewItem, key IdemKey) (ItemRef, error)
	AddDependency(ctx context.Context, item, blockedBy ItemRef, key IdemKey) error
	Dependencies(ctx context.Context, ref ItemRef) ([]Dependency, error) // each with state and close reason
	Close(ctx context.Context, ref ItemRef, reason CloseReason, key IdemKey) error
}

// TrackerCaps are the features a tracker supports.
type TrackerCaps struct {
	Assign             bool // Claim can assign the coder identity
	NativeDependencies bool
	StatusMarkers      bool // markers can map to workflow statuses
	MaxBody            int  // longest body or comment, in bytes
}

// CloseReason is whether and how an item is closed.
type CloseReason string

const (
	CloseOpen      CloseReason = "open"      // not closed
	CloseCompleted CloseReason = "completed" // closed as completed
	CloseNotDone   CloseReason = "not_done"  // closed as not planned, duplicate or similar
)

// Item is a tracker item. It holds no title or body: those are untrusted text
// the core never reads.
type Item struct {
	Ref         ItemRef
	URL         string
	Markers     []Marker
	CloseReason CloseReason
	ClosedBy    *Actor // who closed it, nil while open
	Assignees   []Actor
	CreatedAt   time.Time
	Version     Version
}

// Dependency is an item that blocks another. The core decides whether it is
// done (FR-2 AC6): ChangeMerged, or CloseCompleted by an owner account.
type Dependency struct {
	Ref          ItemRef
	CloseReason  CloseReason
	ClosedBy     *Actor
	ChangeMerged bool // its linked change was merged
}

// NewItem is an item the loop creates: a follow-up, task or tracking item.
type NewItem struct {
	Markers []Marker
	Title   UntrustedText
	Body    UntrustedText
	Record  Record // the followup, task or plan record (§6.1)
	// Since is when to start looking for an earlier item with the same key:
	// the change's merge time for a follow-up, the plan's merge time for task
	// and tracking items.
	Since time.Time
}

// ItemEvent is one change to an item, from the tracker's change feed.
type ItemEvent struct {
	Item    ItemRef
	At      time.Time
	Version Version // the item's version after the event
	Detail  ItemEventDetail
}

// ItemEventDetail is one of MarkerChanged, ItemClosed, ItemReopened,
// AssigneeChanged, Commented or DependencyChanged.
type ItemEventDetail interface{ itemEvent() }

// MarkerChanged: Actor set (On) or cleared a marker.
type MarkerChanged struct {
	Actor  Actor
	Marker Marker
	On     bool
}

// ItemClosed: Actor closed the item.
type ItemClosed struct {
	Actor  Actor
	Reason CloseReason
}

// ItemReopened: the item was reopened.
type ItemReopened struct{}

// AssigneeChanged: Actor changed the assignees. Human is whether a human is
// assigned afterwards (FR-2 AC1).
type AssigneeChanged struct {
	Actor Actor
	Human bool
}

// Commented: Actor commented on the item.
type Commented struct{ Actor Actor }

// DependencyChanged: the item's dependencies changed.
type DependencyChanged struct{}

func (MarkerChanged) itemEvent()     {}
func (ItemClosed) itemEvent()        {}
func (ItemReopened) itemEvent()      {}
func (AssigneeChanged) itemEvent()   {}
func (Commented) itemEvent()         {}
func (DependencyChanged) itemEvent() {}
