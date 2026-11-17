package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/ports/untrusted"
)

// DefaultLabels are the label names of PRODUCT-SPEC §5.1, which the profile
// can override (INTEGRATION-SPEC §9.2, §13).
var DefaultLabels = map[ports.Marker]string{
	ports.MarkerReady:        "ready",
	ports.MarkerInProgress:   "in-progress",
	ports.MarkerBlocked:      "blocked",
	ports.MarkerNeedsOwner:   "needs-owner",
	ports.MarkerPaused:       "paused",
	ports.MarkerAborted:      "aborted",
	ports.MarkerFollowUp:     "follow-up",
	ports.MarkerPlan:         "plan",
	ports.MarkerPlanProposal: "plan-proposal",
	ports.MarkerTypeFeature:  "feature",
	ports.MarkerTypeBug:      "bug",
}

// MaxBody is GitHub's limit on an issue or comment body, in characters.
const MaxBody = 65536

// TrackerConfig configures the WorkTracker.
type TrackerConfig struct {
	// Labels maps each marker to its label. Markers it leaves out take
	// DefaultLabels.
	Labels       map[ports.Marker]string
	Identities   Identities
	BranchPrefix string // "agent/" when empty
	PageSize     int    // events per Changes call; 100 when zero
}

// Tracker is the GitHub WorkTracker over issues (INTEGRATION-SPEC §8.2,
// §9). It acts as the loop identity.
type Tracker struct {
	c        *Client
	ids      Identities
	labels   map[ports.Marker]string
	markers  map[string]ports.Marker // label to marker
	prefix   string
	pageSize int

	mu       sync.Mutex
	unrolled map[ports.IdemKey]unrolled // claims whose rollback failed (D3)
}

// unrolled is a refused claim's label writes that its rollback couldn't
// undo.
type unrolled struct {
	n              int
	added, removed bool
}

var _ ports.WorkTracker = (*Tracker)(nil)

// NewTracker returns the tracker on c's repository.
func NewTracker(c *Client, cfg TrackerConfig) (*Tracker, error) {
	t := &Tracker{
		c:        c,
		ids:      cfg.Identities,
		prefix:   cfg.BranchPrefix,
		pageSize: cfg.PageSize,
	}
	if t.ids.Loop.ID == "" || t.ids.Coder.ID == "" || t.ids.Reviewer.ID == "" {
		return nil, errors.New("github: tracker needs the IDs of all three identities")
	}
	if t.prefix == "" {
		t.prefix = "agent/"
	}
	if t.pageSize <= 0 {
		t.pageSize = 100
	}
	var err error
	if t.labels, t.markers, err = labelMaps(cfg.Labels); err != nil {
		return nil, err
	}
	return t, nil
}

const asLoop = ports.IdentityLoop

// Capabilities: GitHub Apps can't be assigned to issues and issues have no
// workflow statuses (§9.2, §9.3).
func (t *Tracker) Capabilities() ports.TrackerCaps {
	return ports.TrackerCaps{Assign: false, NativeDependencies: true, StatusMarkers: false, MaxBody: MaxBody}
}

// issue is a GitHub issue as the REST API returns it.
type issue struct {
	ID          int64     `json:"id"`
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	User        user      `json:"user"`
	Labels      []label   `json:"labels"`
	State       string    `json:"state"`
	StateReason string    `json:"state_reason"`
	ClosedBy    *user     `json:"closed_by"`
	Assignees   []user    `json:"assignees"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	HTMLURL     string    `json:"html_url"`
	PullRequest *struct{} `json:"pull_request"`
}

type label struct {
	Name string `json:"name"`
}

type comment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	User      user      `json:"user"`
	CreatedAt time.Time `json:"created_at"`
	IssueURL  string    `json:"issue_url"`
	HTMLURL   string    `json:"html_url"`
}

func (it issue) labelNames() []string {
	out := make([]string, 0, len(it.Labels))
	for _, l := range it.Labels {
		out = append(out, l.Name)
	}
	sort.Strings(out)
	return out
}

func (it issue) hasLabel(name string) bool {
	for _, l := range it.Labels {
		if strings.EqualFold(l.Name, name) {
			return true
		}
	}
	return false
}

// version is the issue's concurrency token (work plan D2):
// updated_at, then the sorted label names, then the sorted assignee IDs.
func (it issue) version() ports.Version {
	return ports.Version(it.UpdatedAt.UTC().Format(time.RFC3339) + "|" + strings.Join(it.labelNames(), ",") + "|" + it.assigneeIDs())
}

func (it issue) assigneeIDs() string {
	ids := make([]int64, 0, len(it.Assignees))
	for _, u := range it.Assignees {
		ids = append(ids, u.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(out, ",")
}

// splitVersion splits a version into its updated_at, label and assignee
// parts. Label names may hold "|", so the first and last "|" split it.
func splitVersion(v ports.Version) (updated time.Time, labels, assignees string, ok bool) {
	s := string(v)
	i, j := strings.Index(s, "|"), strings.LastIndex(s, "|")
	if i < 0 || i == j {
		return time.Time{}, "", "", false
	}
	at, err := time.Parse(time.RFC3339, s[:i])
	if err != nil {
		return time.Time{}, "", "", false
	}
	return at, s[i+1 : j], s[j+1:], true
}

// closeReason maps the issue's state (work plan D6).
func (it issue) closeReason() ports.CloseReason {
	if it.State != "closed" {
		return ports.CloseOpen
	}
	switch it.StateReason {
	case "", "completed":
		return ports.CloseCompleted
	}
	return ports.CloseNotDone
}

func (t *Tracker) view(it issue) ports.Item {
	out := ports.Item{
		Ref:         ports.ItemRef(strconv.Itoa(it.Number)),
		URL:         it.HTMLURL,
		CloseReason: it.closeReason(),
		CreatedAt:   it.CreatedAt,
		Version:     it.version(),
	}
	for _, l := range it.Labels {
		if m, ok := t.markers[strings.ToLower(l.Name)]; ok {
			out.Markers = append(out.Markers, m)
		}
	}
	sort.Slice(out.Markers, func(i, j int) bool { return out.Markers[i] < out.Markers[j] })
	if out.CloseReason != ports.CloseOpen && it.ClosedBy != nil {
		a := it.ClosedBy.actor()
		out.ClosedBy = &a
	}
	for _, u := range it.Assignees {
		out.Assignees = append(out.Assignees, u.actor())
	}
	return out
}

// number parses an item ref.
func number(ref ports.ItemRef) (int, error) {
	n, err := strconv.Atoi(string(ref))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("github: item %q is not an issue number: %w", ref, ports.ErrNotFound)
	}
	return n, nil
}

// issue reads an issue. A pull request is not an item (work plan D6).
func (t *Tracker) issue(ctx context.Context, ref ports.ItemRef) (issue, error) {
	n, err := number(ref)
	if err != nil {
		return issue{}, err
	}
	var it issue
	if err := t.c.Get(ctx, asLoop, t.c.RepoPath("/issues/%d", n), nil, &it); err != nil {
		return issue{}, err
	}
	if it.PullRequest != nil {
		return issue{}, fmt.Errorf("github: #%d is a pull request, not an issue: %w", n, ports.ErrNotFound)
	}
	return it, nil
}

func (t *Tracker) Item(ctx context.Context, ref ports.ItemRef) (ports.Item, error) {
	it, err := t.issue(ctx, ref)
	if err != nil {
		return ports.Item{}, err
	}
	return t.view(it), nil
}

// Candidates returns the open issues with the ready label, oldest first.
func (t *Tracker) Candidates(ctx context.Context) ([]ports.Item, error) {
	q := url.Values{"state": {"open"}, "labels": {t.labels[ports.MarkerReady]}, "sort": {"created"}, "direction": {"asc"}}
	its, err := List[issue](ctx, t.c, asLoop, t.c.RepoPath("/issues"), q)
	if err != nil {
		return nil, err
	}
	var out []ports.Item
	for _, it := range its {
		if it.PullRequest == nil {
			out = append(out, t.view(it))
		}
	}
	return out, nil
}

func (t *Tracker) label(m ports.Marker) (string, error) {
	l, ok := t.labels[m]
	if !ok {
		return "", fmt.Errorf("github: marker %q has no label: %w", m, ports.ErrConflict)
	}
	return l, nil
}

// setLabel adds or removes a label as the loop identity.
func (t *Tracker) setLabel(ctx context.Context, n int, name string, on bool) error {
	return setIssueLabel(ctx, t.c, n, name, on)
}

// SetMarker sets or clears a marker's label. It is set-to-value: an issue
// already in that state is left alone (fake D6).
func (t *Tracker) SetMarker(ctx context.Context, ref ports.ItemRef, m ports.Marker, on bool, _ ports.IdemKey) error {
	name, err := t.label(m)
	if err != nil {
		return err
	}
	it, err := t.issue(ctx, ref)
	if err != nil {
		return err
	}
	if it.hasLabel(name) == on {
		return nil
	}
	return t.setLabel(ctx, it.Number, name, on)
}

// comments lists an issue's comments, oldest first.
func (t *Tracker) comments(ctx context.Context, n int) ([]comment, error) {
	return List[comment](ctx, t.c, asLoop, t.c.RepoPath("/issues/%d/comments", n), nil)
}

// findComment returns the loop's comment on issue n whose record has key.
func (t *Tracker) findComment(ctx context.Context, n int, key ports.IdemKey) (*ports.Record, error) {
	cs, err := t.comments(ctx, n)
	if err != nil {
		return nil, err
	}
	for _, c := range cs {
		if r, ok := t.ids.recordIn(c.Body, c.User, c.CreatedAt, ports.CommentRef(strconv.FormatInt(c.ID, 10)), ""); ok &&
			r.Key == key && r.Author.ID == t.ids.Loop.ID {
			return &r, nil
		}
	}
	return nil, nil
}

// Comment posts a comment with rec as the loop identity. The loop's comment
// on the item with the same key is returned instead of a second one (§6.1:
// status and approved_commits records are looked up in the item's
// comments).
func (t *Tracker) Comment(ctx context.Context, ref ports.ItemRef, rec ports.Record, body ports.UntrustedText, key ports.IdemKey) (ports.CommentRef, error) {
	it, err := t.issue(ctx, ref)
	if err != nil {
		return "", err
	}
	return t.comment(ctx, it.Number, rec, untrusted.Read(body), key)
}

func (t *Tracker) comment(ctx context.Context, n int, rec ports.Record, text string, key ports.IdemKey) (ports.CommentRef, error) {
	marker, err := encodeMarker(rec, key)
	if err != nil {
		return "", err
	}
	if r, err := t.findComment(ctx, n, key); err != nil || r != nil {
		if r != nil {
			return r.Ref, nil
		}
		return "", err
	}
	var c comment
	if err := t.c.Send(ctx, asLoop, http.MethodPost, t.c.RepoPath("/issues/%d/comments", n), map[string]any{"body": withMarker(text, marker)}, &c); err != nil {
		return "", err
	}
	return ports.CommentRef(strconv.FormatInt(c.ID, 10)), nil
}

// FindRecords returns the records of type rt in the issue's body and
// comments whose author may write rt (§6.1, C3, work plan D9).
func (t *Tracker) FindRecords(ctx context.Context, ref ports.ItemRef, rt ports.RecordType) ([]ports.Record, error) {
	it, err := t.issue(ctx, ref)
	if err != nil {
		return nil, err
	}
	var out []ports.Record
	if r, ok := t.ids.recordIn(it.Body, it.User, it.CreatedAt, ports.CommentRef(ref), rt); ok {
		out = append(out, r)
	}
	cs, err := t.comments(ctx, it.Number)
	if err != nil {
		return nil, err
	}
	for _, c := range cs {
		if r, ok := t.ids.recordIn(c.Body, c.User, c.CreatedAt, ports.CommentRef(strconv.FormatInt(c.ID, 10)), rt); ok {
			out = append(out, r)
		}
	}
	return out, nil
}

// CreateItem creates an issue as the loop identity. An issue the loop
// created since n.Since whose body record has the key is returned instead
// (§6.1: GET /issues?creator=<loop app>&state=all&since=<t>, work plan D10).
func (t *Tracker) CreateItem(ctx context.Context, n ports.NewItem, key ports.IdemKey) (ports.ItemRef, error) {
	marker, err := encodeMarker(n.Record, key)
	if err != nil {
		return "", err
	}
	q := url.Values{"creator": {t.ids.Loop.Login}, "state": {"all"}, "sort": {"created"}, "direction": {"asc"}}
	if !n.Since.IsZero() {
		q.Set("since", n.Since.UTC().Format(time.RFC3339))
	}
	its, err := List[issue](ctx, t.c, asLoop, t.c.RepoPath("/issues"), q)
	if err != nil {
		return "", err
	}
	for _, it := range its {
		if it.PullRequest != nil || it.CreatedAt.Before(n.Since.Truncate(time.Second)) {
			continue
		}
		if r, ok := t.ids.recordIn(it.Body, it.User, it.CreatedAt, "", n.Record.Type); ok && r.Key == key {
			return ports.ItemRef(strconv.Itoa(it.Number)), nil
		}
	}
	labels := []string{}
	for _, m := range n.Markers {
		l, err := t.label(m)
		if err != nil {
			return "", err
		}
		labels = append(labels, l)
	}
	title := untrusted.Read(n.Title)
	if strings.TrimSpace(title) == "" {
		title = "agent-loop " + string(n.Record.Type)
	}
	var it issue
	in := map[string]any{"title": title, "body": withMarker(untrusted.Read(n.Body), marker), "labels": labels}
	if err := t.c.Send(ctx, asLoop, http.MethodPost, t.c.RepoPath("/issues"), in, &it); err != nil {
		return "", err
	}
	return ports.ItemRef(strconv.Itoa(it.Number)), nil
}

// Close closes the issue as the loop identity with the matching state
// reason. A closed issue is left alone.
func (t *Tracker) Close(ctx context.Context, ref ports.ItemRef, reason ports.CloseReason, _ ports.IdemKey) error {
	var sr string
	switch reason {
	case ports.CloseCompleted:
		sr = "completed"
	case ports.CloseNotDone:
		sr = "not_planned"
	default:
		return fmt.Errorf("github: close item %s with reason %q: %w", ref, reason, ports.ErrConflict)
	}
	it, err := t.issue(ctx, ref)
	if err != nil {
		return err
	}
	if it.State == "closed" {
		return nil
	}
	return t.c.Send(ctx, asLoop, http.MethodPatch, t.c.RepoPath("/issues/%d", it.Number), map[string]any{"state": "closed", "state_reason": sr}, nil)
}

// Claim is INTEGRATION-SPEC §9.3: read and compare, set the labels, read
// again and roll back on a human change, then post the claim record (work
// plan D1, D2, D3).
func (t *Tracker) Claim(ctx context.Context, ref ports.ItemRef, expect ports.Version, key ports.IdemKey) (ports.Version, error) {
	it, err := t.issue(ctx, ref)
	if err != nil {
		return "", err
	}
	claims, err := t.claimRecords(ctx, it.Number)
	if err != nil {
		return "", err
	}
	for _, r := range claims {
		if r.Key == key {
			return it.version(), nil // this key already claimed (D1)
		}
	}
	t.mu.Lock()
	u, pending := t.unrolled[key]
	t.mu.Unlock()
	if pending {
		// The retry of a refused claim whose rollback failed: roll back
		// again and report the refusal (NFR-2, work plan D3).
		return "", t.rollback(ctx, key, u.n, u.added, u.removed, fmt.Errorf("github: claim %s of item %s was refused: %w", key, ref, ports.ErrConflict))
	}
	inProgress, ready := t.labels[ports.MarkerInProgress], t.labels[ports.MarkerReady]
	want := map[string]bool{}
	for _, l := range it.Labels {
		want[strings.ToLower(l.Name)] = true
	}
	want[strings.ToLower(inProgress)] = true
	delete(want, strings.ToLower(ready))
	var added, removed bool
	if a, r, left, ok := t.interruptedClaim(it, claims, expect); ok {
		// A claim from expect wrote labels and stopped before its record
		// (work plan D17): finish step 2 and go on at step 3.
		added, removed = a, r
		if left {
			if err := t.setLabel(ctx, it.Number, ready, false); err != nil {
				return "", t.rollback(ctx, key, it.Number, added, false, err)
			}
		}
	} else {
		// 1. Compare.
		if it.version() != expect {
			return "", fmt.Errorf("github: item %s changed since version %s: %w", ref, expect, ports.ErrConflict)
		}
		if it.State != "open" {
			return "", fmt.Errorf("github: item %s is closed: %w", ref, ports.ErrConflict)
		}
		// 2. Set the labels.
		added, removed = !it.hasLabel(inProgress), it.hasLabel(ready)
		if added {
			if err := t.setLabel(ctx, it.Number, inProgress, true); err != nil {
				return "", err
			}
		}
		if removed {
			if err := t.setLabel(ctx, it.Number, ready, false); err != nil {
				return "", t.rollback(ctx, key, it.Number, added, false, err)
			}
		}
	}
	rollback := func(cause error) error { return t.rollback(ctx, key, it.Number, added, removed, cause) }
	// 3. Read again.
	now, err := t.issue(ctx, ref)
	if err != nil {
		return "", rollback(err)
	}
	if !sameLabels(now, want) || now.State != "open" || !sameUsers(now.Assignees, it.Assignees) {
		return "", rollback(fmt.Errorf("github: item %s changed during the claim: %w", ref, ports.ErrConflict))
	}
	// 4. Post the claim record.
	n := len(claims) + 1
	data, err := json.Marshal(contract.Record{
		Type: contract.RecordStatus, V: 1, Key: string(key), Item: string(ref), Event: contract.EventClaim, Claim: n,
	})
	if err != nil {
		return "", err
	}
	rec := ports.Record{Type: ports.RecordStatus, Key: key, Data: data}
	if _, err := t.comment(ctx, it.Number, rec, fmt.Sprintf("agent-loop claimed this item (claim %d).", n), key); err != nil {
		return "", err
	}
	after, err := t.issue(ctx, ref)
	if err != nil {
		return "", err
	}
	return after.version(), nil
}

// rollback undoes a claim's label writes and returns cause. When the
// rollback itself fails it returns ErrTransient naming both and remembers
// the writes under key, so a retry of Claim with key rolls back again
// (NFR-2, work plan D3).
func (t *Tracker) rollback(ctx context.Context, key ports.IdemKey, n int, added, removed bool, cause error) error {
	var errs []error
	if added {
		errs = append(errs, t.setLabel(ctx, n, t.labels[ports.MarkerInProgress], false))
	}
	if removed {
		errs = append(errs, t.setLabel(ctx, n, t.labels[ports.MarkerReady], true))
	}
	rerr := errors.Join(errs...)
	t.mu.Lock()
	defer t.mu.Unlock()
	if rerr == nil {
		delete(t.unrolled, key)
		return cause
	}
	if t.unrolled == nil {
		t.unrolled = map[ports.IdemKey]unrolled{}
	}
	t.unrolled[key] = unrolled{n: n, added: added, removed: removed}
	return fmt.Errorf("github: claim of item %d stopped (%v) and the rollback failed (%v): %w", n, cause, rerr, ports.ErrTransient)
}

// interruptedClaim reports whether it is in a state that a claim from
// expect leaves when it stops during or after step 2: open, with expect's
// assignees, no claim record since expect, and expect's labels with the
// claim's in-progress write made and its ready removal made or still to
// make (labels are written in that order). It returns the writes the claim
// makes and whether the ready removal is still to make (work plan D17).
func (t *Tracker) interruptedClaim(it issue, claims []ports.Record, expect ports.Version) (added, removed, removeLeft, ok bool) {
	at, labels, assignees, parsed := splitVersion(expect)
	if !parsed || it.State != "open" || it.assigneeIDs() != assignees || !it.UpdatedAt.After(at) {
		return false, false, false, false
	}
	for _, r := range claims {
		if !r.At.Before(at) {
			return false, false, false, false
		}
	}
	inProgress, ready := t.labels[ports.MarkerInProgress], t.labels[ports.MarkerReady]
	hasIP, hasReady := it.hasLabel(inProgress), it.hasLabel(ready)
	for _, done := range []struct{ add, remove bool }{{true, false}, {false, true}, {true, true}} {
		if (done.add && !hasIP) || (done.remove && hasReady) {
			continue
		}
		// The labels before the claim, if it made the writes in done.
		var before []string
		for _, l := range it.Labels {
			if done.add && strings.EqualFold(l.Name, inProgress) {
				continue
			}
			before = append(before, l.Name)
		}
		if done.remove {
			before = append(before, ready)
		}
		sort.Strings(before)
		if strings.Join(before, ",") != labels {
			continue
		}
		wantAdd := done.add || !hasIP
		wantRemove := done.remove || hasReady
		if wantAdd && !done.add || wantRemove && !done.remove && !done.add {
			continue // the writes are made in order: add, then remove
		}
		return wantAdd, wantRemove, wantRemove && !done.remove, true
	}
	return false, false, false, false
}

// claimRecords returns the loop's claim records on issue n.
func (t *Tracker) claimRecords(ctx context.Context, n int) ([]ports.Record, error) {
	cs, err := t.comments(ctx, n)
	if err != nil {
		return nil, err
	}
	var out []ports.Record
	for _, c := range cs {
		r, ok := t.ids.recordIn(c.Body, c.User, c.CreatedAt, ports.CommentRef(strconv.FormatInt(c.ID, 10)), ports.RecordStatus)
		if !ok {
			continue
		}
		var d struct {
			Event string `json:"event"`
		}
		if json.Unmarshal(r.Data, &d) == nil && d.Event == string(contract.EventClaim) {
			out = append(out, r)
		}
	}
	return out, nil
}

func sameLabels(it issue, want map[string]bool) bool {
	if len(it.Labels) != len(want) {
		return false
	}
	for _, l := range it.Labels {
		if !want[strings.ToLower(l.Name)] {
			return false
		}
	}
	return true
}

func sameUsers(a, b []user) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[int64]bool{}
	for _, u := range a {
		seen[u.ID] = true
	}
	for _, u := range b {
		if !seen[u.ID] {
			return false
		}
	}
	return true
}
