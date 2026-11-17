package workflows

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"go.temporal.io/sdk/workflow"

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

// startOf returns when each stage run started, in order.
func (r *recorder) startTimes() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []time.Time
	for _, s := range r.starts {
		out = append(out, s.Started)
	}
	return out
}

// limitDay runs one item whose review is refused at the daily limit, and
// checks that it waits in waiting_limit until 00:00, then reviews and
// merges, with the owner told of the limit and of the reset (FR-16 AC3,
// AC4).
func limitDay(t *testing.T, w *wfHarness, impl func(branch string) handler) {
	t.Helper()
	w.cfg.SupervisorInterval = 20 * time.Hour // past 00:00 from 09:00
	w.cfg.PollInterval = 30 * time.Minute
	item := w.item()
	w.handle(contract.StageImplement, impl("agent/"+string(item)))
	w.handle(contract.StageReview, approving())
	start := w.env.Now()
	w.runProject()

	started, _ := w.rec.stages()
	if !slices.Equal(started, []contract.Stage{contract.StageImplement, contract.StageReview}) {
		t.Fatalf("stages %v", started)
	}
	midnight := core.NextDayBoundary(start, time.UTC)
	ts := w.rec.startTimes()
	if ts[0].After(midnight) || ts[1].Before(midnight) {
		t.Fatalf("implement at %v, review at %v; want the review after %v", ts[0], ts[1], midnight)
	}
	states := w.rec.states()
	want := []core.State{core.StateImplementing, core.StateInReview, core.StateWaitingLimit, core.StateInReview, core.StateMerging, core.StateMerged}
	if !slices.Equal(states, want) {
		t.Fatalf("states %v, want %v", states, want)
	}
	rs := w.notes.reasons()
	if !slices.Contains(rs, "daily_limit") || !slices.Contains(rs, "daily_reset") {
		t.Fatalf("notifications %v", rs)
	}
}

// FR-16 AC3, AC4: at the daily agent-run limit the next agent stage waits
// in waiting_limit; the 00:00 reset wakes it.
func TestFR16AC4_DailyLimitWaitAndReset(t *testing.T) {
	w := newWF(t)
	w.cfg.DailyRuns = 1
	limitDay(t, w, passing)
}

// FR-16 AC3, INTEGRATION-SPEC §5.1, #12 D16: reported spend counts toward
// limits.daily_spend; reaching it stops new agent stages until 00:00.
func TestFR16AC3_DailySpendLimitWaitAndReset(t *testing.T) {
	w := newWF(t)
	w.cfg.DailySpend = 1.0
	limitDay(t, w, func(b string) handler { return withUsage(passing(b), `{"cost":1.5,"currency":"USD"}`) })
}

// FR-16 AC3: spend below the limit, or no spend limit, holds nothing.
func TestFR16AC3_SpendBelowLimitDoesNotWait(t *testing.T) {
	w := newWF(t)
	w.cfg.DailySpend = 5
	item := w.item()
	w.handle(contract.StageImplement, withUsage(passing("agent/"+string(item)), `{"cost":1.5}`))
	w.handle(contract.StageReview, withUsage(approving(), `{"cost":2}`))
	st := w.runProject()
	if slices.Contains(w.rec.states(), core.StateWaitingLimit) {
		t.Fatalf("waited below the spend limit: %v", w.rec.states())
	}
	if st.Spend.Amount != 3.5 {
		t.Fatalf("spend = %v, want 3.5", st.Spend.Amount)
	}
}

// FR-2 AC4, TECHNICAL-SPEC §6.1: a second start of a running item's
// workflow is refused; the item is never worked by two runs at once.
func TestFR2AC4_DuplicateStartRefused(t *testing.T) {
	w := newWF(t)
	item := w.item()
	w.handle(contract.StageImplement, passing("agent/"+string(item)))
	var first, second error
	dup := func(ctx workflow.Context) error {
		p := &projectWF{ctx: ctx, cfg: w.cfg, st: ProjectState{Items: map[string]RunningItem{}}}
		plan := ClaimPlanReport{Plan: core.ClaimFor("p", string(item), 0), ItemType: "feature"}
		first = p.startItem(plan)
		second = p.startItem(plan)
		// The owner closes the item, which ends its workflow.
		closed := MustSignal(core.ItemClosed{At: core.At{Time: workflow.Now(ctx)}, By: core.RoleOwner})
		return workflow.SignalExternalWorkflow(ctx, plan.Plan.WorkflowID, "", SignalEvent, closed).Get(ctx, nil)
	}
	w.env.RegisterWorkflowWithOptions(dup, workflow.RegisterOptions{Name: "dup"})
	w.env.ExecuteWorkflow("dup")
	if first != nil {
		t.Fatalf("first start: %v", first)
	}
	if !errors.Is(second, ErrItemRunning) {
		t.Fatalf("second start: %v, want ErrItemRunning", second)
	}
}

// FR-10 AC3: a paused project picks nothing and starts no stage; the
// resume lets the item through.
func TestFR10AC3_ProjectPauseHoldsPicking(t *testing.T) {
	w := newWF(t)
	w.handle(contract.StageReview, approving())
	var item ports.ItemRef
	w.env.RegisterDelayedCallback(func() {
		w.env.SignalWorkflow(SignalPause, PauseSignal{Paused: true})
	}, 10*time.Second)
	w.env.RegisterDelayedCallback(func() {
		item = w.item()
		w.handle(contract.StageImplement, passing("agent/"+string(item)))
	}, 20*time.Second)
	w.env.RegisterDelayedCallback(func() {
		if it := w.itemState(item); hasMarker(it, ports.MarkerInProgress) {
			t.Error("claimed while paused")
		}
		w.env.SignalWorkflow(SignalPause, PauseSignal{Paused: false})
	}, 30*time.Minute)
	start := w.env.Now()
	w.runProject()
	ts := w.rec.startTimes()
	if len(ts) != 2 || ts[0].Before(start.Add(30*time.Minute)) {
		t.Fatalf("stage starts %v", ts)
	}
	if !slices.Contains(w.rec.states(), core.StateMerged) {
		t.Fatalf("states %v", w.rec.states())
	}
}

// FR-10 AC3, work plan D5: a deployment pause reaches the project while a
// stage waits to start; the stage starts only when it ends.
func TestFR10AC3_DeploymentPauseHoldsAdmission(t *testing.T) {
	w := newWF(t)
	item := w.item()
	w.handle(contract.StageImplement, passing("agent/"+string(item)))
	w.handle(contract.StageReview, approving())
	// Pause the deployment while implement runs: the review's admission
	// is held.
	w.handle(contract.StageImplement, func(h *harness, in contract.Input, out string) string {
		w.env.SignalWorkflow(SignalDeploymentPause, PauseSignal{Paused: true})
		return passing("agent/"+string(item))(h, in, out)
	})
	w.env.RegisterDelayedCallback(func() {
		w.env.SignalWorkflow(SignalDeploymentPause, PauseSignal{Paused: false})
	}, time.Hour)
	start := w.env.Now()
	w.runProject()
	ts := w.rec.startTimes()
	if len(ts) != 2 || ts[1].Before(start.Add(time.Hour)) {
		t.Fatalf("stage starts %v, want the review after the pause", ts)
	}
}

// TECHNICAL-SPEC §6.2: an item workflow continues as new after its event
// limit with its state, and still reaches the merge.
func TestT16_ItemContinuesAsNew(t *testing.T) {
	w := newWF(t)
	w.cfg.Item.EventLimit = 2
	item := w.item()
	w.handle(contract.StageImplement, passing("agent/"+string(item)))
	w.handle(contract.StageReview, approving())
	st := w.runProject()
	if !slices.Contains(w.rec.states(), core.StateMerged) || len(st.Items) != 0 {
		t.Fatalf("states %v, items %v", w.rec.states(), st.Items)
	}
	if n := w.runs(contract.StageImplement) + w.runs(contract.StageReview); n != 2 {
		t.Fatalf("%d handler runs, want 2", n)
	}
}
