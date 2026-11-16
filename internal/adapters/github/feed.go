package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mayusuk/agent-loop/internal/ports"
)

// cursor is the tracker feed's position (work plan D5): the last issue event
// and the last issue comment returned.
type cursor struct {
	event     int64
	comment   int64
	commentAt int64 // unix seconds of the last comment's created_at
}

const cursorVersion = "gh1"

func (c cursor) String() string {
	return fmt.Sprintf("%s:%d:%d:%d", cursorVersion, c.event, c.comment, c.commentAt)
}

func parseCursor(s ports.Cursor) (cursor, error) {
	parts := strings.Split(string(s), ":")
	bad := fmt.Errorf("github: bad tracker cursor %q: %w", s, ports.ErrConflict)
	if len(parts) != 4 || parts[0] != cursorVersion {
		return cursor{}, bad
	}
	var n [3]int64
	for i, p := range parts[1:] {
		v, err := strconv.ParseInt(p, 10, 64)
		if err != nil || v < 0 {
			return cursor{}, bad
		}
		n[i] = v
	}
	return cursor{event: n[0], comment: n[1], commentAt: n[2]}, nil
}

// issueEvent is one repository issue event.
type issueEvent struct {
	ID          int64     `json:"id"`
	Event       string    `json:"event"`
	Actor       *user     `json:"actor"`
	CreatedAt   time.Time `json:"created_at"`
	Label       *label    `json:"label"`
	StateReason string    `json:"state_reason"`
	Issue       *issue    `json:"issue"`
}

// Changes returns the item events after since, oldest first, at most one
// page (fake D2, work plan D4, D5). An empty since returns no events and the
// current end of the feed.
func (t *Tracker) Changes(ctx context.Context, since ports.Cursor) ([]ports.ItemEvent, ports.Cursor, error) {
	if since == "" {
		end, err := t.feedEnd(ctx)
		if err != nil {
			return nil, since, err
		}
		return nil, ports.Cursor(end.String()), nil
	}
	cur, err := parseCursor(since)
	if err != nil {
		return nil, since, err
	}
	evs, err := t.eventsAfter(ctx, cur.event)
	if err != nil {
		return nil, since, err
	}
	cms, err := t.commentsAfter(ctx, cur)
	if err != nil {
		return nil, since, err
	}

	// Merge the two sources by time, events first at equal times.
	type entry struct {
		at time.Time
		ev *issueEvent
		cm *comment
	}
	var all []entry
	for i := range evs {
		all = append(all, entry{at: evs[i].CreatedAt, ev: &evs[i]})
	}
	for i := range cms {
		all = append(all, entry{at: cms[i].CreatedAt, cm: &cms[i]})
	}
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].at.Equal(all[j].at) {
			return all[i].at.Before(all[j].at)
		}
		return all[i].ev != nil && all[j].ev == nil
	})

	issues := map[int]*issue{} // read once per call
	read := func(n int) (*issue, error) {
		if it, ok := issues[n]; ok {
			return it, nil
		}
		it, err := t.issue(ctx, ports.ItemRef(strconv.Itoa(n)))
		if err != nil {
			return nil, err
		}
		issues[n] = &it
		return &it, nil
	}

	var out []ports.ItemEvent
	for _, e := range all {
		if len(out) == t.pageSize {
			break
		}
		var ev *ports.ItemEvent
		var err error
		if e.ev != nil {
			ev, err = t.convertEvent(e.ev, read)
		} else {
			ev, err = t.convertComment(e.cm, read)
		}
		if err != nil {
			return nil, since, err
		}
		if e.ev != nil {
			cur.event = e.ev.ID
		} else {
			cur.comment, cur.commentAt = e.cm.ID, e.cm.CreatedAt.Unix()
		}
		if ev != nil {
			out = append(out, *ev)
		}
	}
	return out, ports.Cursor(cur.String()), nil
}

// feedEnd returns the cursor at the newest event and comment.
func (t *Tracker) feedEnd(ctx context.Context) (cursor, error) {
	var c cursor
	var evs []issueEvent
	if err := t.c.Get(ctx, asLoop, t.c.RepoPath("/issues/events"), url.Values{"per_page": {"1"}}, &evs); err != nil {
		return c, err
	}
	if len(evs) > 0 {
		c.event = evs[0].ID
	}
	var cms []comment
	q := url.Values{"per_page": {"1"}, "sort": {"created"}, "direction": {"desc"}}
	if err := t.c.Get(ctx, asLoop, t.c.RepoPath("/issues/comments"), q, &cms); err != nil {
		return c, err
	}
	if len(cms) > 0 {
		c.comment, c.commentAt = cms[0].ID, cms[0].CreatedAt.Unix()
	}
	return c, nil
}

// eventsAfter reads the repository's issue events, newest first, until the
// event after; it returns the newer ones oldest first. If after is set and
// no longer in the feed, events were lost: ErrConflict (work plan D5).
func (t *Tracker) eventsAfter(ctx context.Context, after int64) ([]issueEvent, error) {
	var newer []issueEvent
	found := after == 0
	err := t.c.Pages(ctx, asLoop, t.c.RepoPath("/issues/events"), nil, func(page json.RawMessage) (bool, error) {
		var evs []issueEvent
		if err := decode(page, &evs, "/issues/events"); err != nil {
			return false, err
		}
		for _, e := range evs {
			if e.ID <= after {
				found = true
				return false, nil
			}
			newer = append(newer, e)
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("github: tracker cursor event %d is no longer in the events feed: %w", after, ports.ErrConflict)
	}
	sort.Slice(newer, func(i, j int) bool { return newer[i].ID < newer[j].ID })
	return newer, nil
}

// commentsAfter returns the issue comments created after the cursor's,
// oldest first.
func (t *Tracker) commentsAfter(ctx context.Context, cur cursor) ([]comment, error) {
	q := url.Values{"sort": {"created"}, "direction": {"asc"}}
	if cur.commentAt > 0 {
		q.Set("since", time.Unix(cur.commentAt, 0).UTC().Format(time.RFC3339))
	}
	cms, err := List[comment](ctx, t.c, asLoop, t.c.RepoPath("/issues/comments"), q)
	if err != nil {
		return nil, err
	}
	var out []comment
	for _, c := range cms {
		if c.ID > cur.comment {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// convertEvent maps an issue event to an item event, or nil for an event
// the tracker doesn't report (pull requests, labels that aren't markers,
// other kinds).
func (t *Tracker) convertEvent(e *issueEvent, read func(int) (*issue, error)) (*ports.ItemEvent, error) {
	if e.Issue == nil || e.Issue.PullRequest != nil {
		return nil, nil
	}
	var actor ports.Actor
	if e.Actor != nil {
		actor = e.Actor.actor()
	}
	var d ports.ItemEventDetail
	switch e.Event {
	case "labeled", "unlabeled":
		if e.Label == nil {
			return nil, nil
		}
		m, ok := t.markers[strings.ToLower(e.Label.Name)]
		if !ok {
			return nil, nil
		}
		d = ports.MarkerChanged{Actor: actor, Marker: m, On: e.Event == "labeled"}
	case "closed":
		d = ports.ItemClosed{Actor: actor, Reason: issue{State: "closed", StateReason: e.StateReason}.closeReason()}
	case "reopened":
		d = ports.ItemReopened{}
	case "assigned", "unassigned":
		d = ports.AssigneeChanged{Actor: actor} // Human is set below
	case "blocked_by_added", "blocked_by_removed":
		d = ports.DependencyChanged{}
	default:
		return nil, nil
	}
	it, err := read(e.Issue.Number)
	if errors.Is(err, ports.ErrNotFound) {
		return nil, nil // deleted, or transferred away
	}
	if err != nil {
		return nil, err
	}
	if ac, ok := d.(ports.AssigneeChanged); ok {
		for _, u := range it.Assignees {
			if u.Type == "User" {
				ac.Human = true
			}
		}
		d = ac
	}
	return &ports.ItemEvent{Item: ports.ItemRef(strconv.Itoa(it.Number)), At: e.CreatedAt, Version: it.version(), Detail: d}, nil
}

// convertComment maps an issue comment to a Commented event, or nil for a
// pull request comment.
func (t *Tracker) convertComment(c *comment, read func(int) (*issue, error)) (*ports.ItemEvent, error) {
	if strings.Contains(c.HTMLURL, "/pull/") {
		return nil, nil
	}
	i := strings.LastIndex(c.IssueURL, "/")
	n, err := strconv.Atoi(c.IssueURL[i+1:])
	if err != nil {
		return nil, nil
	}
	it, err := read(n)
	if errors.Is(err, ports.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ports.ItemEvent{Item: ports.ItemRef(strconv.Itoa(n)), At: c.CreatedAt, Version: it.version(), Detail: ports.Commented{Actor: c.User.actor()}}, nil
}
