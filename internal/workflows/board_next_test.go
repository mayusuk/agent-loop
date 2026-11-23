package workflows

import (
	"testing"
	"time"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/adapters/fake"
	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/projection"
)

// boardRow is the item's board row.
func (s *scen) boardRow(item ports.ItemRef) (projection.BoardRow, bool) {
	s.t.Helper()
	for _, r := range s.board() {
		if r.Item == string(item) {
			return r, true
		}
	}
	return projection.BoardRow{}, false
}

// TestFR11AC1_IgnoredEventKeepsNextAction: an escalated item shows a next
// action and a wait on the board. A label change by a human who isn't an
// owner is recorded and has no effect, so the board keeps both as they
// were; the owner's resume, which changes the state, replaces them (work
// plan #25 D7, review R2).
func TestFR11AC1_IgnoredEventKeepsNextAction(t *testing.T) {
	s := newScenario(t)
	b := s.newItem("B")
	s.hold(b)
	s.on(b, contract.StageImplement,
		func(h *harness, in contract.Input, out string) string {
			write(h.t, out, "q.md", "Which API?")
			return resultJSON(in, "needs_owner", map[string]any{"question_file": "q.md"})
		},
		s.impl())
	s.on(b, contract.StageReview, s.clean())
	var escalated projection.BoardRow
	s.at(10*time.Minute, func() {
		r, ok := s.boardRow(b)
		if !ok || r.State != core.StateEscalated || r.NextAction == "" || r.WaitReason == "" {
			t.Errorf("escalated B on the board: %+v (on board %v), want a next action and a wait", r, ok)
		}
		escalated = r
		// A human who isn't an owner adds and removes labels: recorded,
		// no effect (FR-13 AC7).
		s.must(s.f.SetMarker(fake.Human, b, ports.MarkerPaused, true))
		s.must(s.f.SetMarker(fake.Human, b, ports.MarkerNeedsOwner, false))
	})
	decisionsAt := 0
	s.at(20*time.Minute, func() {
		decisionsAt = len(s.itemStates(b))
		r, _ := s.boardRow(b)
		if r.State != core.StateEscalated || r.NextAction != escalated.NextAction || r.WaitReason != escalated.WaitReason {
			t.Errorf("after a non-owner's label changes the board shows %s %q %q, want %s %q %q",
				r.State, r.NextAction, r.WaitReason, escalated.State, escalated.NextAction, escalated.WaitReason)
		}
		// The owner resumes (the label is gone, so they set it and
		// remove it; FR-13 AC7 leaves it as the non-owner left it).
		s.must(s.f.SetMarker(fake.Owner, b, ports.MarkerNeedsOwner, true))
		s.must(s.f.SetMarker(fake.Owner, b, ports.MarkerNeedsOwner, false))
	})
	s.at(40*time.Minute, func() {
		r, _ := s.boardRow(b)
		if r.State != core.StateMerging || r.WaitReason == escalated.WaitReason || (r.NextAction == "" && r.WaitReason == "") {
			t.Errorf("after the owner's resume the board shows %s %q %q, want merging with its own next action or wait", r.State, r.NextAction, r.WaitReason)
		}
		s.release(b)
	})
	s.runProject()
	s.noCredentials()
	if decisionsAt == 0 {
		t.Fatal("the board was never read after the ignored events")
	}
}
