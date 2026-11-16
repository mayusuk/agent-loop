package github_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/mayusuk/agent-loop/internal/ports"
)

// feed reads the feed from cur until a call returns no events (fake D2).
func feed(t *testing.T, p *stubProject, cur ports.Cursor) ([]ports.ItemEvent, ports.Cursor) {
	t.Helper()
	var all []ports.ItemEvent
	for {
		evs, next, err := p.tracker.Changes(ctx, cur)
		must(t, err)
		cur = next
		if len(evs) == 0 {
			return all, cur
		}
		all = append(all, evs...)
	}
}

// TestChangesMapsEvents: every reported kind, with actors, close reasons,
// the item's current version; pull requests and other labels are left out
// (§9.4, work plan D4, D5).
func TestChangesMapsEvents(t *testing.T) {
	p := newStub(t, 100)
	owner, human := actorOf(p.owner), actorOf(p.human)
	it := p.item(t, owner)
	blocker := p.item(t, owner)
	_, cur := feed(t, p, "")

	p.srv.CreateLabel("wontfix")
	must(t, p.client.Send(ctx, ports.IdentityLoop, "POST", p.client.RepoPath("/issues/%s/labels", it), map[string]any{"labels": []string{"wontfix"}}, nil))
	must(t, p.driver.SetMarker(human, it, ports.MarkerReady, true))
	must(t, p.driver.CommentItem(human, it, "hello", nil))
	must(t, p.driver.Assign(owner, it, &human))
	must(t, p.driver.Assign(owner, it, nil))
	must(t, p.tracker.AddDependency(ctx, it, blocker, "d"))
	must(t, p.driver.CloseItem(owner, it, ports.CloseNotDone))
	must(t, p.driver.ReopenItem(human, it))
	pr := p.srv.AddPull(p.coder, "agent/"+string(it), "")
	p.srv.MergePull(p.owner, pr, false)
	must(t, p.driver.CommentItem(owner, ports.ItemRef(itoa(int64(pr))), "on the pull request", nil))

	evs, _ := feed(t, p, cur)
	now := mustItem(t, p, it).Version
	var got []ports.ItemEventDetail
	for _, ev := range evs {
		if ev.Item == blocker {
			continue
		}
		if ev.Item != it || ev.Version != now || ev.At.IsZero() {
			t.Errorf("event %+v: want item %s at version %s", ev, it, now)
		}
		got = append(got, ev.Detail)
	}
	want := []ports.ItemEventDetail{
		ports.MarkerChanged{Actor: human, Marker: ports.MarkerReady, On: true},
		ports.Commented{Actor: human},
		ports.AssigneeChanged{Actor: owner, Human: false}, // the state when read
		ports.AssigneeChanged{Actor: owner, Human: false},
		ports.DependencyChanged{},
		ports.ItemClosed{Actor: owner, Reason: ports.CloseNotDone},
		ports.ItemReopened{},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d events %+v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestChangesAssigneeHuman(t *testing.T) {
	p := newStub(t, 100)
	owner, human := actorOf(p.owner), actorOf(p.human)
	it := p.item(t, owner)
	_, cur := feed(t, p, "")
	must(t, p.driver.Assign(owner, it, &human))
	evs, _ := feed(t, p, cur)
	if len(evs) != 1 || evs[0].Detail != (ports.AssigneeChanged{Actor: owner, Human: true}) {
		t.Errorf("events %+v", evs)
	}
}

// TestChangesCursor: an empty cursor is the current end; a bad cursor or one
// whose event is gone is ErrConflict (fake D2, D8, work plan D5).
func TestChangesCursor(t *testing.T) {
	p := newStub(t, 100)
	// An empty repository: the cursor still works once events arrive.
	evs, cur, err := p.tracker.Changes(ctx, "")
	must(t, err)
	if len(evs) != 0 || cur == "" {
		t.Fatalf("empty cursor: %v, %q", evs, cur)
	}
	it := p.item(t, actorOf(p.owner), ports.MarkerReady)
	must(t, p.driver.CommentItem(actorOf(p.owner), it, "c", nil))
	got, end := feed(t, p, cur)
	if len(got) != 2 {
		t.Errorf("from an empty repository: %d events, want 2", len(got))
	}
	if evs, _, _ := p.tracker.Changes(ctx, ""); len(evs) != 0 {
		t.Error("an empty cursor returned events")
	}
	for _, bad := range []ports.Cursor{"x", "gh1:1:2", "gh2:1:2:3", "gh1:a:0:0", "gh1:-1:0:0"} {
		if _, _, err := p.tracker.Changes(ctx, bad); !errors.Is(err, ports.ErrConflict) {
			t.Errorf("cursor %q: %v, want ErrConflict", bad, err)
		}
	}
	if _, _, err := p.tracker.Changes(ctx, "gh1:5:0:0"); !errors.Is(err, ports.ErrConflict) {
		t.Errorf("cursor before the feed's oldest event: %v, want ErrConflict", err)
	}
	// Nothing new: the polls are answered 304.
	before := len(p.srv.Requests())
	if evs, _ := feed(t, p, end); len(evs) != 0 {
		t.Errorf("events after the end: %+v", evs)
	}
	for _, r := range p.srv.Requests()[before:] {
		if r.Status != http.StatusNotModified {
			t.Errorf("%s %s?%s answered %d, want 304", r.Method, r.Path, r.Query, r.Status)
		}
	}
}

// TestChangesPagesWithComments: events and comments interleave in time
// order across pages, with none skipped or repeated (C12).
func TestChangesPagesWithComments(t *testing.T) {
	p := newStub(t, 3)
	owner := actorOf(p.owner)
	it := p.item(t, owner)
	_, cur := feed(t, p, "")
	var want []string
	for i := 0; i < 5; i++ {
		must(t, p.driver.SetMarker(owner, it, ports.MarkerPaused, i%2 == 0))
		want = append(want, "marker")
		must(t, p.driver.CommentItem(owner, it, "c", nil))
		want = append(want, "comment")
	}
	first, next, err := p.tracker.Changes(ctx, cur)
	must(t, err)
	if len(first) != 3 {
		t.Fatalf("first page: %d events, want 3", len(first))
	}
	must(t, p.driver.CommentItem(owner, it, "late", nil))
	want = append(want, "comment")
	rest, _ := feed(t, p, next)
	all := append(first, rest...)
	if len(all) != len(want) {
		t.Fatalf("got %d events, want %d", len(all), len(want))
	}
	for i, ev := range all {
		kind := "marker"
		if _, ok := ev.Detail.(ports.Commented); ok {
			kind = "comment"
		}
		if kind != want[i] {
			t.Errorf("event %d is a %s, want a %s", i, kind, want[i])
		}
	}
}

func TestChangesSkipsDeletedIssues(t *testing.T) {
	p := newStub(t, 100)
	it := p.item(t, actorOf(p.owner))
	_, cur := feed(t, p, "")
	must(t, p.driver.SetMarker(actorOf(p.owner), it, ports.MarkerReady, true))
	p.srv.Fail("GET", `/issues/\d+$`, 404, nil, `{"message":"Not Found"}`, 1)
	evs, _ := feed(t, p, cur)
	if len(evs) != 0 {
		t.Errorf("events for a deleted issue: %+v", evs)
	}
}
