package fake

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/ports/untrusted"
)

// Host is the fake's CodeHost.
type Host Fake

var _ ports.CodeHost = (*Host)(nil)

// Review states, as the code host names them.
const (
	ReviewApprove          = "APPROVE"
	ReviewComment          = "COMMENT"
	ReviewChangesRequested = "CHANGES_REQUESTED"
)

// change is a change (pull request).
type change struct {
	ref         ports.ChangeRef
	item        ports.ItemRef // the item its record or closing reference names
	byRecord    bool          // linked by a coder change record, not a closing reference
	branch      string        // a branch "<login>:<name>" is in a fork
	author      ports.Actor
	title, body untrusted.Text
	rec         *ports.Record
	state       ports.ChangeState
	mergedBy    *ports.Actor
	mergedAt    time.Time
	mergedHead  ports.Commit
	openedAt    time.Time
	labels      map[ports.Marker]bool
	threads     []ports.ThreadRef
	reviews     []*review
	comments    []*comment // conversation comments, not review comments
}

func (c *change) fork() bool { return strings.Contains(c.branch, ":") }

// linkedTo reports whether c belongs to item (§6.2): a change record from
// the coder on a branch named for the item, or the code host's closing
// reference on a change a human opened.
func (c *change) linkedTo(f *Fake, item ports.ItemRef) bool {
	if c.item != item || item == "" {
		return false
	}
	if !c.byRecord {
		return true
	}
	return f.branchFor(c.branch, item)
}

// branchFor: <prefix><item> or <prefix><item>-<n> with n ≥ 2 (§3.3, D44).
func (f *Fake) branchFor(branch string, item ports.ItemRef) bool {
	rest, ok := strings.CutPrefix(branch, f.cfg.BranchPrefix+string(item))
	return ok && (rest == "" || regexp.MustCompile(`^-([2-9]|[1-9][0-9]+)$`).MatchString(rest))
}

type thread struct {
	ref        ports.ThreadRef
	change     ports.ChangeRef
	path       string
	line       int
	comments   []*comment
	resolved   bool
	resolvedBy ports.Actor
	resolvedAt time.Time
}

type review struct {
	id          string
	author      ports.Actor
	state       string
	body        untrusted.Text
	rec         *ports.Record
	commit      ports.Commit
	at          time.Time
	dismissed   bool
	dismissedBy ports.Actor
	findings    map[ports.FindingID]ports.ThreadRef // "" for a finding with no thread
}

// hostEvent is a change event. A HeadMoved's Pusher is filled in when the
// event is read (D4).
type hostEvent struct {
	ev     ports.ChangeEvent
	commit ports.Commit
}

func (f *Fake) change(ref ports.ChangeRef) (*change, error) {
	c, ok := f.changes[ref]
	if !ok {
		return nil, fmt.Errorf("fake: change %s: %w", ref, ports.ErrNotFound)
	}
	return c, nil
}

func (f *Fake) head(c *change) ports.Commit {
	if c.state == ports.ChangeStateMerged {
		return c.mergedHead
	}
	return f.branches[c.branch]
}

func (f *Fake) hostEvent(ref ports.ChangeRef, d ports.ChangeEventDetail) {
	f.hostLog = append(f.hostLog, hostEvent{ev: ports.ChangeEvent{Change: ref, At: f.now, Detail: d}})
}

func (f *Fake) view(c *change) ports.Change {
	v := ports.Change{
		Ref:      c.ref,
		URL:      "fake://pulls/" + string(c.ref),
		Item:     c.item,
		Branch:   c.branch,
		Base:     f.cfg.BaseBranch,
		Head:     f.head(c),
		State:    c.state,
		Fork:     c.fork(),
		Author:   c.author,
		MergedAt: c.mergedAt,
		Markers:  sortedMarkers(c.labels),
	}
	if c.mergedBy != nil {
		a := *c.mergedBy
		v.MergedBy = &a
	}
	return v
}

// Capabilities: like GitHub, a push rule and file-level comments.
func (h *Host) Capabilities() ports.HostCaps {
	return ports.HostCaps{PushRule: true, FileLevelComments: true, MaxBody: 65536}
}

// Validate checks that the profile names this adapter.
func (h *Host) Validate(_ context.Context, p ports.ProfileCodeHost) []ports.Problem {
	return validateAdapter(p.Adapter)
}

// ChangeForItem returns the item's open change, or nil.
func (h *Host) ChangeForItem(ctx context.Context, item ports.ItemRef) (*ports.Change, error) {
	f := (*Fake)(h)
	if err := f.begin(ctx); err != nil {
		return nil, err
	}
	defer f.mu.Unlock()
	for _, ref := range f.changeOrder {
		c := f.changes[ref]
		if c.state == ports.ChangeStateOpen && c.linkedTo(f, item) {
			v := f.view(c)
			return &v, nil
		}
	}
	return nil, nil
}

func (h *Host) Change(ctx context.Context, ref ports.ChangeRef) (ports.Change, error) {
	f := (*Fake)(h)
	if err := f.begin(ctx); err != nil {
		return ports.Change{}, err
	}
	defer f.mu.Unlock()
	c, err := f.change(ref)
	if err != nil {
		return ports.Change{}, err
	}
	return f.view(c), nil
}

// Changes returns change events after since. Each head move gets its Pusher
// the first time it is observed (D4).
func (h *Host) Changes(ctx context.Context, since ports.Cursor) ([]ports.ChangeEvent, ports.Cursor, error) {
	f := (*Fake)(h)
	if err := f.begin(ctx); err != nil {
		return nil, since, err
	}
	defer f.mu.Unlock()
	evs, next, err := page(f.hostLog, since, f.cfg.PageSize)
	if err != nil {
		return nil, since, err
	}
	out := make([]ports.ChangeEvent, len(evs))
	for i, e := range evs {
		out[i] = e.ev
		if hm, ok := e.ev.Detail.(ports.HeadMoved); ok {
			hm.Pusher = f.attribute(f.commits[e.commit])
			out[i].Detail = hm
		}
	}
	if len(evs) == 0 {
		// A call that returns nothing ends the poll (D2); the next poll's
		// window starts after it.
		f.lastHostPoll = f.tick()
	}
	return out, next, nil
}

// OpenChange opens a change as the identity. A change on the same branch
// from that identity with the same key is returned instead.
func (h *Host) OpenChange(ctx context.Context, n ports.NewChange, as ports.Identity, key ports.IdemKey) (ports.ChangeRef, error) {
	f := (*Fake)(h)
	if err := f.begin(ctx); err != nil {
		return "", err
	}
	defer f.mu.Unlock()
	if f.killed("OpenChange", BeforeWrite) {
		return "", ErrKilled
	}
	actor, err := f.identity(as)
	if err != nil {
		return "", err
	}
	for _, ref := range f.changeOrder {
		c := f.changes[ref]
		if c.branch == n.Branch && c.author.ID == actor.ID && c.rec != nil && c.rec.Key == key {
			return ref, nil
		}
	}
	c, err := f.openChange(actor, n.Item, n.Branch, n.Title, n.Body, withKey(n.Record, key))
	if err != nil {
		return "", err
	}
	if f.killed("OpenChange", AfterWrite) {
		return "", ErrKilled
	}
	return c.ref, nil
}

// openChange opens a change. A change with rec is linked to item by its
// record; one without by a closing reference (§6.2).
func (f *Fake) openChange(author ports.Actor, item ports.ItemRef, branch string, title, body untrusted.Text, rec *ports.Record) (*change, error) {
	if _, ok := f.branches[branch]; !ok {
		return nil, fmt.Errorf("fake: branch %s: %w", branch, ports.ErrNotFound)
	}
	if branch == f.cfg.BaseBranch {
		return nil, fmt.Errorf("fake: a change from the base branch: %w", ports.ErrConflict)
	}
	for _, c := range f.changes {
		if c.branch == branch && c.state == ports.ChangeStateOpen {
			return nil, fmt.Errorf("fake: change %s is already open on %s: %w", c.ref, branch, ports.ErrConflict)
		}
	}
	f.tick()
	c := &change{
		ref: ports.ChangeRef(f.nextID()), item: item, byRecord: rec != nil, branch: branch, author: author,
		title: title, body: body, rec: rec, state: ports.ChangeStateOpen, openedAt: f.now,
		labels: map[ports.Marker]bool{},
	}
	f.changes[c.ref] = c
	f.changeOrder = append(f.changeOrder, c.ref)
	f.hostEvent(c.ref, ports.Opened{Actor: author, Item: item})
	return c, nil
}

// SetLabel sets or clears a change label as the loop identity.
func (h *Host) SetLabel(ctx context.Context, ref ports.ChangeRef, m ports.Marker, on bool, key ports.IdemKey) error {
	f := (*Fake)(h)
	if err := f.begin(ctx); err != nil {
		return err
	}
	defer f.mu.Unlock()
	if f.killed("SetLabel", BeforeWrite) {
		return ErrKilled
	}
	c, err := f.change(ref)
	if err != nil {
		return err
	}
	if c.labels[m] != on {
		f.tick()
		if on {
			c.labels[m] = true
		} else {
			delete(c.labels, m)
		}
	}
	if f.killed("SetLabel", AfterWrite) {
		return ErrKilled
	}
	return nil
}

// ContainsBase reports whether the change's head contains the base tip.
func (h *Host) ContainsBase(ctx context.Context, ref ports.ChangeRef) (bool, error) {
	f := (*Fake)(h)
	if err := f.begin(ctx); err != nil {
		return false, err
	}
	defer f.mu.Unlock()
	c, err := f.change(ref)
	if err != nil {
		return false, err
	}
	return f.reachable(f.head(c))[f.branches[f.cfg.BaseBranch]], nil
}

// Gates reports each required gate's latest result on commit cm (D10).
func (h *Host) Gates(ctx context.Context, ref ports.ChangeRef, cm ports.Commit) ([]ports.GateResult, error) {
	f := (*Fake)(h)
	if err := f.begin(ctx); err != nil {
		return nil, err
	}
	defer f.mu.Unlock()
	if _, err := f.change(ref); err != nil {
		return nil, err
	}
	return f.gateResults(cm), nil
}

func (f *Fake) gateResults(cm ports.Commit) []ports.GateResult {
	var out []ports.GateResult
	for _, name := range f.cfg.Gates {
		s, ok := f.gates[cm][name]
		if !ok {
			s = ports.GateMissing
		}
		out = append(out, ports.GateResult{Name: name, State: s, Commit: cm})
	}
	return out
}

// ReviewRecords returns the reviews the reviewer identity posted with a
// review record (C3).
func (h *Host) ReviewRecords(ctx context.Context, ref ports.ChangeRef) ([]ports.ReviewRecord, error) {
	f := (*Fake)(h)
	if err := f.begin(ctx); err != nil {
		return nil, err
	}
	defer f.mu.Unlock()
	c, err := f.change(ref)
	if err != nil {
		return nil, err
	}
	var out []ports.ReviewRecord
	for _, r := range c.reviews {
		if r.rec == nil || r.rec.Type != ports.RecordReview || !f.counts(r.rec, r.author) {
			continue
		}
		rec := *r.rec
		rec.Author, rec.Ref, rec.At = r.author, ports.CommentRef(r.id), r.at
		threads := map[ports.FindingID]ports.ThreadRef{}
		for id, t := range r.findings {
			if t != "" {
				threads[id] = t
			}
		}
		out = append(out, ports.ReviewRecord{Ref: ports.ReviewRef(r.id), Author: r.author, SubmittedAt: r.at, Record: rec, Threads: threads})
	}
	return out, nil
}

// PostReview posts a review as the reviewer identity, with one thread for
// each finding that has a path (I8). A reviewer review with the same key is
// returned instead.
func (h *Host) PostReview(ctx context.Context, ref ports.ChangeRef, p ports.ReviewPost, key ports.IdemKey) (ports.ReviewRef, error) {
	f := (*Fake)(h)
	if err := f.begin(ctx); err != nil {
		return "", err
	}
	defer f.mu.Unlock()
	if f.killed("PostReview", BeforeWrite) {
		return "", ErrKilled
	}
	c, err := f.change(ref)
	if err != nil {
		return "", err
	}
	for _, r := range c.reviews {
		if r.author.ID == f.cfg.Reviewer.ID && r.rec != nil && r.rec.Key == key {
			return ports.ReviewRef(r.id), nil
		}
	}
	state := ReviewComment
	if p.Approve {
		state = ReviewApprove
	}
	r := f.submitReview(c, f.cfg.Reviewer, state, p.Summary, withKey(p.Record, key), p.Commit)
	for _, fd := range p.Findings {
		if fd.Path == "" {
			r.findings[fd.ID] = ""
			continue
		}
		rec := fd.Record
		t := f.newThread(c, f.cfg.Reviewer, fd.Path, fd.Line, fd.Body, &rec)
		r.findings[fd.ID] = t.ref
	}
	if f.killed("PostReview", AfterWrite) {
		return "", ErrKilled
	}
	return ports.ReviewRef(r.id), nil
}

func (f *Fake) submitReview(c *change, author ports.Actor, state string, body untrusted.Text, rec *ports.Record, cm ports.Commit) *review {
	f.tick()
	if cm == "" {
		cm = f.head(c)
	}
	r := &review{id: f.nextID(), author: author, state: state, body: body, rec: rec, commit: cm, at: f.now, findings: map[ports.FindingID]ports.ThreadRef{}}
	c.reviews = append(c.reviews, r)
	f.hostEvent(c.ref, ports.ReviewSubmitted{Actor: author})
	return r
}

func (f *Fake) newThread(c *change, author ports.Actor, path string, line int, body untrusted.Text, rec *ports.Record) *thread {
	f.tick()
	t := &thread{ref: ports.ThreadRef("T" + f.nextID()), change: c.ref, path: path, line: line}
	t.comments = append(t.comments, &comment{id: f.nextID(), author: author, body: body, rec: rec, at: f.now})
	f.threads[t.ref] = t
	c.threads = append(c.threads, t.ref)
	return t
}

// ResolveFinding resolves the finding's thread as the loop identity (owner
// decision on #100). A
// finding with no thread has nothing to resolve (D8).
func (h *Host) ResolveFinding(ctx context.Context, ref ports.ChangeRef, id ports.FindingID, key ports.IdemKey) error {
	f := (*Fake)(h)
	if err := f.begin(ctx); err != nil {
		return err
	}
	defer f.mu.Unlock()
	if f.killed("ResolveFinding", BeforeWrite) {
		return ErrKilled
	}
	c, err := f.change(ref)
	if err != nil {
		return err
	}
	tref, found := ports.ThreadRef(""), false
	for _, r := range c.reviews {
		if r.author.ID != f.cfg.Reviewer.ID {
			continue
		}
		if t, ok := r.findings[id]; ok {
			tref, found = t, true
		}
	}
	if !found {
		return fmt.Errorf("fake: finding %s on change %s: %w", id, ref, ports.ErrNotFound)
	}
	if tref != "" {
		f.resolve(f.threads[tref], f.cfg.Loop, true)
	}
	if f.killed("ResolveFinding", AfterWrite) {
		return ErrKilled
	}
	return nil
}

func (f *Fake) resolve(t *thread, by ports.Actor, on bool) {
	if t.resolved == on {
		return
	}
	f.tick()
	t.resolved, t.resolvedBy, t.resolvedAt = on, by, f.now
	if on {
		f.hostEvent(t.change, ports.ThreadResolved{Actor: by})
	} else {
		f.hostEvent(t.change, ports.ThreadUnresolved{Actor: by})
	}
}

// ManualComments returns the manual comments on the change and whether a
// human cleared each one (§9.6).
func (h *Host) ManualComments(ctx context.Context, ref ports.ChangeRef) ([]ports.ManualComment, error) {
	f := (*Fake)(h)
	if err := f.begin(ctx); err != nil {
		return nil, err
	}
	defer f.mu.Unlock()
	c, err := f.change(ref)
	if err != nil {
		return nil, err
	}
	var out []ports.ManualComment
	for _, tref := range c.threads {
		t := f.threads[tref]
		for _, cm := range t.comments {
			if !f.isManualCommenter(cm.author) {
				continue
			}
			mc := ports.ManualComment{ID: "MC-" + cm.id, Author: cm.author, Kind: ports.ManualCommentThread, Thread: t.ref, Body: cm.body, CreatedAt: cm.at}
			// Cleared by a human's resolve that came after the comment.
			if t.resolved && f.isHuman(t.resolvedBy) && cm.at.Before(t.resolvedAt) {
				mc.Cleared, mc.ClearedBy = true, ports.ClearedByHumanResolved
			}
			out = append(out, mc)
		}
	}
	for _, r := range c.reviews {
		if !f.isManualCommenter(r.author) {
			continue
		}
		kind := ports.ManualCommentReviewBody
		if untrusted.Read(r.body) == "" {
			if r.state != ReviewChangesRequested {
				continue // an empty comment or approval is not a manual comment
			}
			kind = ports.ManualCommentEmptyChangesRequested
		}
		mc := ports.ManualComment{ID: "MC-" + r.id, Author: r.author, Kind: kind, Body: r.body, CreatedAt: r.at}
		if r.dismissed && f.isHuman(r.dismissedBy) {
			mc.Cleared, mc.ClearedBy = true, ports.ClearedByHumanDismissed
		}
		out = append(out, mc)
	}
	return out, nil
}

// PostReply posts a reply with rec as the coder identity in the thread, or
// as a change comment when to is the change's own ref (D3). A coder reply
// with the same key in the same place is not posted again.
func (h *Host) PostReply(ctx context.Context, ref ports.ChangeRef, to ports.ThreadRef, rec ports.Record, body ports.UntrustedText, key ports.IdemKey) error {
	f := (*Fake)(h)
	if err := f.begin(ctx); err != nil {
		return err
	}
	defer f.mu.Unlock()
	if f.killed("PostReply", BeforeWrite) {
		return ErrKilled
	}
	c, err := f.change(ref)
	if err != nil {
		return err
	}
	var place *[]*comment
	if to == ports.ThreadRef(ref) {
		place = &c.comments
	} else if t, ok := f.threads[to]; ok && t.change == ref {
		place = &t.comments
	} else {
		return fmt.Errorf("fake: thread %s on change %s: %w", to, ref, ports.ErrNotFound)
	}
	for _, cm := range *place {
		if cm.author.ID == f.cfg.Coder.ID && cm.rec != nil && cm.rec.Key == key {
			return nil
		}
	}
	f.tick()
	*place = append(*place, &comment{id: f.nextID(), author: f.cfg.Coder, body: body, rec: withKey(rec, key), at: f.now})
	if to != ports.ThreadRef(ref) {
		f.hostEvent(ref, ports.ReviewSubmitted{Actor: f.cfg.Coder})
	}
	if f.killed("PostReply", AfterWrite) {
		return ErrKilled
	}
	return nil
}

// Merge merges the change as the loop identity if its head is still pinned
// and the base branch's rules allow it (D9).
func (h *Host) Merge(ctx context.Context, ref ports.ChangeRef, pinned ports.Commit, m ports.MergeMethod, key ports.IdemKey) (ports.MergeOutcome, error) {
	f := (*Fake)(h)
	if err := f.begin(ctx); err != nil {
		return "", err
	}
	defer f.mu.Unlock()
	if f.killed("Merge", BeforeWrite) {
		return "", ErrKilled
	}
	c, err := f.change(ref)
	if err != nil {
		return "", err
	}
	switch {
	case c.state == ports.ChangeStateMerged && c.mergedHead == pinned:
		return ports.MergeMerged, nil
	case c.state != ports.ChangeStateOpen:
		return ports.MergeNotMergeable, nil
	case f.head(c) != pinned:
		return ports.MergeHeadMoved, nil
	case !f.mergeable(c):
		return ports.MergeNotMergeable, nil
	}
	f.merge(c, f.cfg.Loop, m)
	if f.killed("Merge", AfterWrite) {
		return "", ErrKilled
	}
	return ports.MergeMerged, nil
}

// mergeable applies the base branch's rules: required gates pass on the
// head, enough approvals, and the head contains the base tip.
func (f *Fake) mergeable(c *change) bool {
	head := f.head(c)
	for _, g := range f.gateResults(head) {
		if g.State != ports.GatePass {
			return false
		}
	}
	approvals := 0
	for _, r := range c.reviews {
		if r.state == ReviewApprove && !r.dismissed && r.author.ID != c.author.ID {
			approvals++
		}
	}
	if approvals < f.cfg.RequiredApprovals {
		return false
	}
	return !f.cfg.RequireUpToDate || f.reachable(head)[f.branches[f.cfg.BaseBranch]]
}

func (f *Fake) merge(c *change, by ports.Actor, m ports.MergeMethod) {
	f.tick()
	head := f.head(c)
	parents := []ports.Commit{f.branches[f.cfg.BaseBranch]}
	if m == ports.MergeMethodMerge {
		parents = append(parents, head)
	}
	mc := f.newCommit(parents, by, f.cfg.BaseBranch)
	f.branches[f.cfg.BaseBranch] = mc.sha
	a := by
	c.state, c.mergedBy, c.mergedAt, c.mergedHead = ports.ChangeStateMerged, &a, f.now, head
	f.hostEvent(c.ref, ports.Merged{Actor: by})
}
