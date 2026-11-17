package workflows

import (
	"context"
	"testing"

	"github.com/mayusuk/agent-loop/internal/adapters/fake"
	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/ports"
)

// FR-13 AC5, AC6, #11 D6, work plan D12: an unknown pusher's head move
// reaches the item with every untrusted commit of the push, so it is
// escalated and approved as a whole.
func TestFR13AC6_UnknownPushCarriesItsCommits(t *testing.T) {
	w := newWF(t)
	ctx := context.Background()
	item := w.item()
	ch := w.implemented(item)
	other := w.item()
	rep, err := w.a.Poll(ctx, PollRequest{Items: []string{string(item)}})
	if err != nil {
		t.Fatal(err)
	}
	end, err := w.f.StartCoderRun(other) // another item's coder token is live
	if err != nil {
		t.Fatal(err)
	}
	c1, _ := w.f.Push(fake.Owner, ch.Branch)
	c2, _ := w.f.Push(fake.Owner, ch.Branch)
	end()
	rep, err = w.a.Poll(ctx, PollRequest{Cursors: rep.Cursors, Items: []string{string(item)}})
	if err != nil {
		t.Fatal(err)
	}
	var moved *core.HeadMoved
	for _, r := range rep.Events {
		if r.Signal.HeadMoved != nil && r.Signal.HeadMoved.Commit == string(c2) {
			moved = r.Signal.HeadMoved
		}
	}
	if moved == nil {
		t.Fatalf("no head move to %s in %+v", c2, rep.Events)
	}
	if moved.Pusher != core.PusherUnknown {
		t.Fatalf("pusher %s, want unknown", moved.Pusher)
	}
	has := func(c ports.Commit) bool {
		for _, x := range moved.Commits {
			if x == string(c) {
				return true
			}
		}
		return false
	}
	if !has(c1) || !has(c2) {
		t.Fatalf("commits %v, want %s and %s", moved.Commits, c1, c2)
	}
}

// FR-12 AC4, FR-13 AC7: tracker events reach only running items, with the
// actor's role; a closed dependency is routed to the item it blocks.
func TestFR12AC4_PollRoutesEventsWithRoles(t *testing.T) {
	w := newWF(t)
	ctx := context.Background()
	item, dep, idle := w.item(), w.item(), w.item()
	if err := w.f.Tracker().AddDependency(ctx, item, dep, "k"); err != nil {
		t.Fatal(err)
	}
	rep, err := w.a.Poll(ctx, PollRequest{Items: []string{string(item)}})
	if err != nil {
		t.Fatal(err)
	}
	w.f.SetMarker(fake.Owner, item, ports.MarkerPaused, true)
	w.f.SetMarker(fake.Owner, idle, ports.MarkerPaused, true)
	w.f.CloseItem(fake.Owner, dep, ports.CloseCompleted)
	rep, err = w.a.Poll(ctx, PollRequest{Cursors: rep.Cursors, Items: []string{string(item)}})
	if err != nil {
		t.Fatal(err)
	}
	var marker, closed bool
	for _, r := range rep.Events {
		if r.Item != string(item) {
			t.Fatalf("event for %s, which has no workflow: %+v", r.Item, r.Signal)
		}
		if m := r.Signal.MarkerChanged; m != nil && m.Marker == core.MarkerPaused && m.By == core.RoleOwner && m.Added {
			marker = true
		}
		if d := r.Signal.DependencyClosed; d != nil && d.Dependency == string(dep) && d.Done {
			closed = true
		}
	}
	if !marker || !closed {
		t.Fatalf("events %+v: owner pause %v, dependency done %v", rep.Events, marker, closed)
	}
}

// D44, work plan D9: a new claim takes the next branch number from the
// change's branch, or from the loop branches already on the remote.
func TestD44_LoadItemCountsClosedChanges(t *testing.T) {
	w := newWF(t)
	ctx := context.Background()
	item := w.item()
	st, err := w.a.LoadItem(ctx, LoadRequest{WorkflowID: "item/p/" + string(item), Item: string(item), Claim: 1})
	if err != nil || st.ClosedChanges != 0 || st.Change != nil {
		t.Fatalf("fresh item: %+v %v", st, err)
	}
	end, err := w.f.StartCoderRun(item)
	if err != nil {
		t.Fatal(err)
	}
	w.push("agent/" + string(item))
	w.push("agent/" + string(item) + "-2")
	end()
	st, _ = w.a.LoadItem(ctx, LoadRequest{WorkflowID: "w", Item: string(item), Claim: 2})
	if st.ClosedChanges != 2 {
		t.Fatalf("closed changes %d, want 2", st.ClosedChanges)
	}
	for b, n := range map[string]int{"agent/7": 1, "agent/7-3": 3, "agent/7-x": 0, "agent/70": 0, "feature-x": 0, "agent/7-1": 0} {
		if got := branchNumber("agent/", "7", b); got != n {
			t.Errorf("branchNumber(%s) = %d, want %d", b, got, n)
		}
	}
}
