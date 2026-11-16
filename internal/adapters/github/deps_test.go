package github_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/mayusuk/agent-loop/internal/adapters/github/githubtest"
	"github.com/mayusuk/agent-loop/internal/ports"
)

func changeBody(item ports.ItemRef, n int) string {
	data, _ := json.Marshal(map[string]any{"type": "change", "v": 1, "item": item, "n": n, "key": "k"})
	return "Implements it.\n\n" + githubtest.Marker(ports.Record{Data: data})
}

func TestAddDependency(t *testing.T) {
	p := newStub(t, 100)
	owner := actorOf(p.owner)
	it, a, b := p.item(t, owner), p.item(t, owner), p.item(t, owner)
	must(t, p.tracker.AddDependency(ctx, it, a, "k1"))
	must(t, p.tracker.AddDependency(ctx, it, a, "k1"))
	must(t, p.tracker.AddDependency(ctx, it, b, "k2"))
	deps, err := p.tracker.Dependencies(ctx, it)
	must(t, err)
	if len(deps) != 2 || deps[0].Ref != a || deps[1].Ref != b {
		t.Errorf("dependencies %+v", deps)
	}
	if err := p.tracker.AddDependency(ctx, it, it, "k3"); !errors.Is(err, ports.ErrConflict) {
		t.Errorf("self: %v", err)
	}
	if err := p.tracker.AddDependency(ctx, it, "999", "k4"); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("missing blocker: %v", err)
	}
	// A 422 because another call added it first is success.
	c := p.item(t, owner)
	p.srv.After("GET", `/issues/`+string(it)+`/dependencies/blocked_by$`, func() {
		must(t, p.tracker.AddDependency(ctx, it, c, "race"))
	})
	must(t, p.tracker.AddDependency(ctx, it, c, "k5"))
	// A 422 for another reason is ErrConflict.
	d := p.item(t, owner)
	p.srv.Fail("POST", `/dependencies/blocked_by$`, 422, nil, `{"message":"Validation Failed"}`, 1)
	if err := p.tracker.AddDependency(ctx, it, d, "k6"); !errors.Is(err, ports.ErrConflict) {
		t.Errorf("refused: %v", err)
	}
}

// TestDependenciesDoneSignals: close reason and closer for each dependency,
// and ChangeMerged from each link of §6.2 and §9.5 (work plan D7).
func TestDependenciesDoneSignals(t *testing.T) {
	p := newStub(t, 100)
	owner, human := actorOf(p.owner), actorOf(p.human)
	blocked := p.item(t, owner)
	byOwner, byHuman, notDone := p.item(t, owner), p.item(t, owner), p.item(t, owner)
	must(t, p.driver.CloseItem(owner, byOwner, ports.CloseCompleted))
	must(t, p.driver.CloseItem(human, byHuman, ports.CloseCompleted))
	must(t, p.driver.CloseItem(owner, notDone, ports.CloseNotDone))

	// Linked by branch name and the coder's change record.
	viaBranch := p.item(t, owner)
	p.srv.MergePull(p.owner, p.srv.AddPull(p.coder, "agent/"+string(viaBranch), changeBody(viaBranch, 1)), false)
	// The third branch, after two closed changes.
	viaThird := p.item(t, owner)
	p.srv.AddPull(p.coder, "agent/"+string(viaThird), changeBody(viaThird, 1))
	p.srv.AddPull(p.coder, "agent/"+string(viaThird)+"-2", changeBody(viaThird, 2))
	p.srv.MergePull(p.owner, p.srv.AddPull(p.coder, "agent/"+string(viaThird)+"-3", changeBody(viaThird, 3)), false)
	// A merged pull request on the branch whose record isn't the coder's.
	forged := p.item(t, owner)
	p.srv.MergePull(p.owner, p.srv.AddPull(p.human, "agent/"+string(forged), changeBody(forged, 1)), false)
	// A record naming another item.
	other := p.item(t, owner)
	p.srv.MergePull(p.owner, p.srv.AddPull(p.coder, "agent/"+string(other), changeBody("1", 1)), false)
	// Linked by the owner's closing reference.
	viaRef := p.item(t, owner)
	refNum, _ := itoaRef(viaRef)
	p.srv.MergePull(p.owner, p.srv.AddPull(p.owner, "feature-x", "Fixes it", refNum), false)
	// Closed by a pull request that merged (ClosedEvent.closer): the issue
	// is closed and done through its merged change.
	viaCloser := p.item(t, owner)
	closerNum, _ := itoaRef(viaCloser)
	p.srv.MergePull(p.owner, p.srv.AddPull(p.owner, "feature-y", "Fixes it", closerNum), true)
	// An open pull request on the branch.
	open := p.item(t, owner)
	p.srv.AddPull(p.coder, "agent/"+string(open), changeBody(open, 1))

	type want struct {
		reason ports.CloseReason
		by     string
		merged bool
	}
	wants := map[ports.ItemRef]want{
		byOwner:   {ports.CloseCompleted, owner.ID, false},
		byHuman:   {ports.CloseCompleted, human.ID, false},
		notDone:   {ports.CloseNotDone, owner.ID, false},
		viaBranch: {ports.CloseOpen, "", true},
		viaThird:  {ports.CloseOpen, "", true},
		forged:    {ports.CloseOpen, "", false},
		other:     {ports.CloseOpen, "", false},
		viaRef:    {ports.CloseOpen, "", true},
		viaCloser: {ports.CloseCompleted, owner.ID, true},
		open:      {ports.CloseOpen, "", false},
	}
	for ref := range wants {
		must(t, p.tracker.AddDependency(ctx, blocked, ref, ports.IdemKey("dep:"+ref)))
	}
	deps, err := p.tracker.Dependencies(ctx, blocked)
	must(t, err)
	if len(deps) != len(wants) {
		t.Fatalf("got %d dependencies, want %d", len(deps), len(wants))
	}
	for _, d := range deps {
		w := wants[d.Ref]
		by := ""
		if d.ClosedBy != nil {
			by = d.ClosedBy.ID
		}
		if d.CloseReason != w.reason || by != w.by || d.ChangeMerged != w.merged {
			t.Errorf("dependency %s: %s by %q merged=%v, want %s by %q merged=%v", d.Ref, d.CloseReason, by, d.ChangeMerged, w.reason, w.by, w.merged)
		}
	}
	if _, err := p.tracker.Dependencies(ctx, "999"); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("missing item: %v", err)
	}
	p.srv.Fail("POST", `^/graphql$`, 502, nil, "", 1)
	if _, err := p.tracker.Dependencies(ctx, blocked); !errors.Is(err, ports.ErrTransient) {
		t.Errorf("graphql failure: %v", err)
	}
}
