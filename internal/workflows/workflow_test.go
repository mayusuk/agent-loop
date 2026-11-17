package workflows

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/adapters/fake"
	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/ports"
)

// TechnicalSpec §6.2–§6.3, FR-2 AC5, FR-7: the project claims a ready
// item and its workflow takes it through implement and review to the
// loop's merge, then ends and frees its WIP slot.
func TestFR2AC5_ClaimThroughMerge(t *testing.T) {
	w := newWF(t)
	item := w.item()
	w.handle(contract.StageImplement, passing("agent/"+string(item)))
	w.handle(contract.StageReview, approving())

	st := w.runProject()

	started, results := w.rec.stages()
	if !slices.Equal(started, []contract.Stage{contract.StageImplement, contract.StageReview}) {
		t.Fatalf("stages started %v", started)
	}
	if !slices.Equal(results, []string{"done", "done"}) {
		t.Fatalf("results %v", results)
	}
	states := w.rec.states()
	want := []core.State{core.StateImplementing, core.StateInReview, core.StateMerging, core.StateMerged}
	if !slices.Equal(states, want) {
		t.Fatalf("states %v, want %v", states, want)
	}
	ch, err := w.f.Host().ChangeForItem(context.Background(), item)
	if err != nil || ch != nil {
		t.Fatalf("an open change is left: %+v %v", ch, err)
	}
	it := w.itemState(item)
	if it.CloseReason != ports.CloseCompleted || hasMarker(it, ports.MarkerInProgress) {
		t.Fatalf("item after merge: %+v", it)
	}
	if len(st.Items) != 0 {
		t.Fatalf("WIP still holds %v", st.Items)
	}
	if st.Daily.Runs != 2 {
		t.Fatalf("daily runs = %d, want 2", st.Daily.Runs)
	}
}

// FR-10 AC1, AC2, FR-12 AC4: a needs_owner result escalates the item; the
// owner removing needs-owner resumes it, and it goes on to merge.
func TestFR10AC2_EscalationAndResume(t *testing.T) {
	w := newWF(t)
	item := w.item()
	asked := false
	impl := passing("agent/" + string(item))
	w.handle(contract.StageImplement, func(h *harness, in contract.Input, out string) string {
		if !asked {
			asked = true
			write(h.t, out, "q.md", "Which API?")
			return resultJSON(in, "needs_owner", map[string]any{"question_file": "q.md"})
		}
		return impl(h, in, out)
	})
	w.handle(contract.StageReview, approving())
	w.env.RegisterDelayedCallback(func() {
		it := w.itemState(item)
		if !hasMarker(it, ports.MarkerNeedsOwner) {
			t.Errorf("not escalated after 10 minutes: %v", it.Markers)
		}
		if err := w.f.SetMarker(fake.Owner, item, ports.MarkerNeedsOwner, false); err != nil {
			t.Error(err)
		}
	}, 10*time.Minute)

	w.runProject()

	states := w.rec.states()
	want := []core.State{core.StateImplementing, core.StateEscalated, core.StateImplementing, core.StateInReview, core.StateMerging, core.StateMerged}
	if !slices.Equal(states, want) {
		t.Fatalf("states %v, want %v", states, want)
	}
	if got := w.runs(contract.StageImplement); got != 2 {
		t.Fatalf("implement ran %d times, want 2", got)
	}
	if !slices.Contains(w.notes.reasons(), core.ReasonNeedsOwner) {
		t.Fatalf("owner not notified: %v", w.notes.reasons())
	}
}

func dumpDecisions(t *testing.T, r *recorder) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range r.decisions {
		t.Logf("%d %s %s->%s %s %v", d.Record.Seq, d.Record.Event, d.Record.From, d.Record.To, d.Record.Rule, d.Record.Actions)
	}
}
