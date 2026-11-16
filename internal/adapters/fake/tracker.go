package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/ports/untrusted"
)

// Tracker is the fake's WorkTracker.
type Tracker Fake

var _ ports.WorkTracker = (*Tracker)(nil)

// item is a tracker item.
type item struct {
	ref         ports.ItemRef
	author      ports.Actor
	title, body untrusted.Text
	bodyRec     *ports.Record
	markers     map[ports.Marker]bool
	closeReason ports.CloseReason
	closedBy    *ports.Actor
	assignees   []ports.Actor
	createdAt   time.Time
	version     int
	comments    []*comment
	deps        []ports.ItemRef
}

func (it *item) view(url string) ports.Item {
	i := ports.Item{
		Ref:         it.ref,
		URL:         url,
		Markers:     sortedMarkers(it.markers),
		CloseReason: it.closeReason,
		Assignees:   append([]ports.Actor(nil), it.assignees...),
		CreatedAt:   it.createdAt,
		Version:     ports.Version("v" + strconv.Itoa(it.version)),
	}
	if it.closedBy != nil {
		a := *it.closedBy
		i.ClosedBy = &a
	}
	return i
}

func (f *Fake) itemURL(ref ports.ItemRef) string { return "fake://issues/" + string(ref) }

func (f *Fake) item(ref ports.ItemRef) (*item, error) {
	it, ok := f.items[ref]
	if !ok {
		return nil, fmt.Errorf("fake: item %s: %w", ref, ports.ErrNotFound)
	}
	return it, nil
}

// itemEvent bumps the item's version and logs d.
func (f *Fake) itemEvent(it *item, d ports.ItemEventDetail) {
	it.version++
	f.trackerLog = append(f.trackerLog, ports.ItemEvent{
		Item: it.ref, At: f.now, Version: ports.Version("v" + strconv.Itoa(it.version)), Detail: d,
	})
}

// setMarker sets or clears m as actor. It reports whether anything changed.
func (f *Fake) setMarker(it *item, m ports.Marker, on bool, actor ports.Actor) bool {
	if it.markers[m] == on {
		return false
	}
	f.tick()
	if on {
		it.markers[m] = true
	} else {
		delete(it.markers, m)
	}
	f.itemEvent(it, ports.MarkerChanged{Actor: actor, Marker: m, On: on})
	return true
}

func (f *Fake) newItem(author ports.Actor, title, body untrusted.Text, markers []ports.Marker, rec *ports.Record) *item {
	f.tick()
	it := &item{
		ref:         ports.ItemRef(f.nextID()),
		author:      author,
		title:       title,
		body:        body,
		bodyRec:     rec,
		markers:     map[ports.Marker]bool{},
		closeReason: ports.CloseOpen,
		createdAt:   f.now,
		version:     1,
	}
	for _, m := range markers {
		it.markers[m] = true
	}
	f.items[it.ref] = it
	f.itemOrder = append(f.itemOrder, it.ref)
	return it
}

// Capabilities: like GitHub, no assignment of apps and no workflow statuses.
func (t *Tracker) Capabilities() ports.TrackerCaps {
	return ports.TrackerCaps{Assign: false, NativeDependencies: true, StatusMarkers: false, MaxBody: 65536}
}

// Validate checks that the profile names this adapter.
func (t *Tracker) Validate(_ context.Context, p ports.ProfileTracker) []ports.Problem {
	return validateAdapter(p.Adapter)
}

func validateAdapter(name string) []ports.Problem {
	if name == "fake" {
		return nil
	}
	return []ports.Problem{{
		Severity: ports.SeverityError, Code: "wrong_adapter",
		Message: fmt.Sprintf("the profile names adapter %q, not fake", name),
		Hint:    `set adapter = "fake"`,
	}}
}

func (t *Tracker) Item(ctx context.Context, ref ports.ItemRef) (ports.Item, error) {
	f := (*Fake)(t)
	if err := f.begin(ctx); err != nil {
		return ports.Item{}, err
	}
	defer f.mu.Unlock()
	it, err := f.item(ref)
	if err != nil {
		return ports.Item{}, err
	}
	return it.view(f.itemURL(ref)), nil
}

// Candidates returns the open items with the ready marker, oldest first.
func (t *Tracker) Candidates(ctx context.Context) ([]ports.Item, error) {
	f := (*Fake)(t)
	if err := f.begin(ctx); err != nil {
		return nil, err
	}
	defer f.mu.Unlock()
	var out []ports.Item
	for _, ref := range f.itemOrder {
		it := f.items[ref]
		if it.closeReason == ports.CloseOpen && it.markers[ports.MarkerReady] {
			out = append(out, it.view(f.itemURL(ref)))
		}
	}
	return out, nil
}

func (t *Tracker) Changes(ctx context.Context, since ports.Cursor) ([]ports.ItemEvent, ports.Cursor, error) {
	f := (*Fake)(t)
	if err := f.begin(ctx); err != nil {
		return nil, since, err
	}
	defer f.mu.Unlock()
	return page(f.trackerLog, since, f.cfg.PageSize)
}

// Claim sets in_progress and clears ready if the item's version is still
// expect, then posts the claim status record as the loop (INTEGRATION-SPEC
// §9.3, work plan #14 D1). A repeat with a key that already has a claim
// record returns the current version (D6).
func (t *Tracker) Claim(ctx context.Context, ref ports.ItemRef, expect ports.Version, key ports.IdemKey) (ports.Version, error) {
	f := (*Fake)(t)
	if err := f.begin(ctx); err != nil {
		return "", err
	}
	defer f.mu.Unlock()
	if f.killed("Claim", BeforeWrite) {
		return "", ErrKilled
	}
	it, err := f.item(ref)
	if err != nil {
		return "", err
	}
	claims := 0
	for _, c := range it.comments {
		if c.rec == nil || c.rec.Type != ports.RecordStatus || !f.counts(c.rec, c.author) {
			continue
		}
		var d struct {
			Event string `json:"event"`
		}
		if json.Unmarshal(c.rec.Data, &d) != nil || d.Event != string(contract.EventClaim) {
			continue
		}
		if c.rec.Key == key {
			return it.view("").Version, nil
		}
		claims++
	}
	if it.view("").Version != expect {
		return "", fmt.Errorf("fake: item %s changed since version %s: %w", ref, expect, ports.ErrConflict)
	}
	if it.closeReason != ports.CloseOpen {
		return "", fmt.Errorf("fake: item %s is closed: %w", ref, ports.ErrConflict)
	}
	f.setMarker(it, ports.MarkerInProgress, true, f.cfg.Loop)
	f.setMarker(it, ports.MarkerReady, false, f.cfg.Loop)
	n := claims + 1
	data, err := json.Marshal(contract.Record{
		Type: contract.RecordStatus, V: 1, Key: string(key), Item: string(ref), Event: contract.EventClaim, Claim: n,
	})
	if err != nil {
		return "", err
	}
	rec := ports.Record{Type: ports.RecordStatus, Key: key, Data: data}
	f.commentItem(it, f.cfg.Loop, untrusted.New(fmt.Sprintf("agent-loop claimed this item (claim %d).", n)), &rec)
	if f.killed("Claim", AfterWrite) {
		return "", ErrKilled
	}
	return it.view("").Version, nil
}

// SetMarker sets or clears a marker as the loop identity.
func (t *Tracker) SetMarker(ctx context.Context, ref ports.ItemRef, m ports.Marker, on bool, key ports.IdemKey) error {
	f := (*Fake)(t)
	if err := f.begin(ctx); err != nil {
		return err
	}
	defer f.mu.Unlock()
	if f.killed("SetMarker", BeforeWrite) {
		return ErrKilled
	}
	it, err := f.item(ref)
	if err != nil {
		return err
	}
	f.setMarker(it, m, on, f.cfg.Loop)
	if f.killed("SetMarker", AfterWrite) {
		return ErrKilled
	}
	return nil
}

// Comment posts a comment with rec as the loop identity. A comment from the
// loop identity with the same key is returned instead of a second one.
func (t *Tracker) Comment(ctx context.Context, ref ports.ItemRef, rec ports.Record, body ports.UntrustedText, key ports.IdemKey) (ports.CommentRef, error) {
	f := (*Fake)(t)
	if err := f.begin(ctx); err != nil {
		return "", err
	}
	defer f.mu.Unlock()
	if f.killed("Comment", BeforeWrite) {
		return "", ErrKilled
	}
	it, err := f.item(ref)
	if err != nil {
		return "", err
	}
	for _, c := range it.comments {
		if c.rec != nil && c.rec.Key == key && c.author.ID == f.cfg.Loop.ID {
			return ports.CommentRef(c.id), nil
		}
	}
	c := f.commentItem(it, f.cfg.Loop, body, withKey(rec, key))
	if f.killed("Comment", AfterWrite) {
		return "", ErrKilled
	}
	return ports.CommentRef(c.id), nil
}

func (f *Fake) commentItem(it *item, author ports.Actor, body untrusted.Text, rec *ports.Record) *comment {
	f.tick()
	c := &comment{id: f.nextID(), author: author, body: body, rec: rec, at: f.now}
	it.comments = append(it.comments, c)
	f.itemEvent(it, ports.Commented{Actor: author})
	return c
}

// FindRecords returns the records of type rt on the item (its body and
// comments) whose author may write rt (§6.1, C3).
func (t *Tracker) FindRecords(ctx context.Context, ref ports.ItemRef, rt ports.RecordType) ([]ports.Record, error) {
	f := (*Fake)(t)
	if err := f.begin(ctx); err != nil {
		return nil, err
	}
	defer f.mu.Unlock()
	it, err := f.item(ref)
	if err != nil {
		return nil, err
	}
	var out []ports.Record
	if it.bodyRec != nil && it.bodyRec.Type == rt && f.counts(it.bodyRec, it.author) {
		r := *it.bodyRec
		r.Author, r.Ref, r.At = it.author, ports.CommentRef(it.ref), it.createdAt
		out = append(out, r)
	}
	for _, c := range it.comments {
		if c.rec != nil && c.rec.Type == rt && f.counts(c.rec, c.author) {
			out = append(out, c.record())
		}
	}
	return out, nil
}

// CreateItem creates an item as the loop identity. An item the loop created
// since n.Since whose body record has the same key is returned instead.
func (t *Tracker) CreateItem(ctx context.Context, n ports.NewItem, key ports.IdemKey) (ports.ItemRef, error) {
	f := (*Fake)(t)
	if err := f.begin(ctx); err != nil {
		return "", err
	}
	defer f.mu.Unlock()
	if f.killed("CreateItem", BeforeWrite) {
		return "", ErrKilled
	}
	for _, ref := range f.itemOrder {
		it := f.items[ref]
		if it.author.ID == f.cfg.Loop.ID && !it.createdAt.Before(n.Since) && it.bodyRec != nil && it.bodyRec.Key == key {
			return ref, nil
		}
	}
	it := f.newItem(f.cfg.Loop, n.Title, n.Body, n.Markers, withKey(n.Record, key))
	if f.killed("CreateItem", AfterWrite) {
		return "", ErrKilled
	}
	return it.ref, nil
}

// AddDependency records that item is blocked by blockedBy.
func (t *Tracker) AddDependency(ctx context.Context, ref, blockedBy ports.ItemRef, key ports.IdemKey) error {
	f := (*Fake)(t)
	if err := f.begin(ctx); err != nil {
		return err
	}
	defer f.mu.Unlock()
	if f.killed("AddDependency", BeforeWrite) {
		return ErrKilled
	}
	it, err := f.item(ref)
	if err != nil {
		return err
	}
	if _, err := f.item(blockedBy); err != nil {
		return err
	}
	if ref == blockedBy {
		return fmt.Errorf("fake: item %s can't block itself: %w", ref, ports.ErrConflict)
	}
	for _, d := range it.deps {
		if d == blockedBy {
			return nil
		}
	}
	f.tick()
	it.deps = append(it.deps, blockedBy)
	f.itemEvent(it, ports.DependencyChanged{})
	if f.killed("AddDependency", AfterWrite) {
		return ErrKilled
	}
	return nil
}

// Dependencies returns the items that block ref, each with its close reason,
// who closed it, and whether its linked change merged.
func (t *Tracker) Dependencies(ctx context.Context, ref ports.ItemRef) ([]ports.Dependency, error) {
	f := (*Fake)(t)
	if err := f.begin(ctx); err != nil {
		return nil, err
	}
	defer f.mu.Unlock()
	it, err := f.item(ref)
	if err != nil {
		return nil, err
	}
	var out []ports.Dependency
	for _, d := range it.deps {
		v := f.items[d].view("")
		out = append(out, ports.Dependency{Ref: d, CloseReason: v.CloseReason, ClosedBy: v.ClosedBy, ChangeMerged: f.itemChangeMerged(d)})
	}
	return out, nil
}

// itemChangeMerged reports whether a change linked to the item merged.
func (f *Fake) itemChangeMerged(ref ports.ItemRef) bool {
	for _, c := range f.changes {
		if c.linkedTo(f, ref) && c.state == ports.ChangeStateMerged {
			return true
		}
	}
	return false
}

// Close closes the item as the loop identity.
func (t *Tracker) Close(ctx context.Context, ref ports.ItemRef, reason ports.CloseReason, key ports.IdemKey) error {
	f := (*Fake)(t)
	if err := f.begin(ctx); err != nil {
		return err
	}
	defer f.mu.Unlock()
	if f.killed("Close", BeforeWrite) {
		return ErrKilled
	}
	it, err := f.item(ref)
	if err != nil {
		return err
	}
	if reason == ports.CloseOpen {
		return fmt.Errorf("fake: close item %s with reason open: %w", ref, ports.ErrConflict)
	}
	if it.closeReason == ports.CloseOpen {
		f.closeItem(it, f.cfg.Loop, reason)
	}
	if f.killed("Close", AfterWrite) {
		return ErrKilled
	}
	return nil
}

func (f *Fake) closeItem(it *item, by ports.Actor, reason ports.CloseReason) {
	f.tick()
	it.closeReason = reason
	a := by
	it.closedBy = &a
	f.itemEvent(it, ports.ItemClosed{Actor: by, Reason: reason})
}
