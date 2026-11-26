package workflows

import (
	"context"
	"fmt"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/ports"
)

// TestLiveTemporal_CapableHostWait runs on a real Temporal server (skipped
// unless AGENT_LOOP_LIVE=1). The review needs agent and x. With only an
// agent worker ("mac") running, the implement runs and the review waits,
// scheduled on cap:agent+x with no attempt started, and the item stays in
// review (FR-14 AC2). A second worker ("h2", agent and x) joins with no
// workflow change (FR-14 AC1) while mac stops, so the project's work moves
// to h2 with no state migration (FR-14 AC3): the review runs once and the
// item merges.
func TestLiveTemporal_CapableHostWait(t *testing.T) {
	c := liveDevClient(t)
	ctx := context.Background()
	w := newWF(t)
	w.a.Now = time.Now
	name := fmt.Sprintf("caps%d", time.Now().UnixNano())
	w.a.Project = name
	item := w.item()
	implemented := make(chan struct{})
	impl := passing("agent/" + string(item))
	w.handle(contract.StageImplement, func(h *harness, in contract.Input, out string) string {
		defer close(implemented)
		return impl(h, in, out)
	})
	w.handle(contract.StageReview, approving())

	build := "live-" + name
	xQueue := CapabilityQueue([]string{"x", "agent"})
	stopMac := liveWorkersOn(t, c, w.a, build, CoreQueue, CapabilityQueue(nil))
	h := c.WorkerDeploymentClient().GetHandle(DeploymentName)
	var err error
	for range 30 {
		if _, err = h.SetCurrentVersion(ctx, client.WorkerDeploymentSetCurrentVersionOptions{BuildID: build}); err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		stopMac()
		t.Fatalf("setting the current worker version: %v", err)
	}
	cfg := w.cfg
	cfg.Name, cfg.Item.Project = name, name
	cfg.PollInterval, cfg.Item.PollInterval = time.Second, time.Second
	cfg.SupervisorInterval = 10 * time.Minute
	cfg.Item.StageCaps = map[contract.Stage][]string{contract.StageReview: {"x", "agent"}}
	pid := ProjectID(name)
	if _, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: pid, TaskQueue: CoreQueue}, ProjectWorkflow, ProjectInput{Config: cfg}); err != nil {
		stopMac()
		t.Fatal(err)
	}
	defer c.TerminateWorkflow(context.Background(), pid, "", "live test done")

	select {
	case <-implemented:
	case <-time.After(time.Minute):
		stopMac()
		t.Fatal("implement didn't run within a minute")
	}
	itemID := core.ClaimFor(name, string(item), 0).WorkflowID
	// The review waits on cap:agent+x: scheduled, never started.
	deadline := time.Now().Add(time.Minute)
	for {
		desc, err := c.DescribeWorkflowExecution(ctx, itemID, "")
		if err != nil {
			stopMac()
			t.Fatal(err)
		}
		// The implement's RunStage may still be finishing (started); the
		// review's is the one that stays scheduled.
		var pending bool
		for _, pa := range desc.GetPendingActivities() {
			if pa.GetActivityType().GetName() == "RunStage" && pa.GetState() == enumspb.PENDING_ACTIVITY_STATE_SCHEDULED {
				if pa.GetAttempt() != 1 {
					stopMac()
					t.Fatalf("review on %s with no worker: attempt %d", xQueue, pa.GetAttempt())
				}
				pending = true
			}
		}
		if pending && w.runs(contract.StageImplement) == 1 {
			break
		}
		if time.Now().After(deadline) {
			stopMac()
			t.Fatalf("the review wasn't scheduled within a minute: states %v", w.rec.states())
		}
		time.Sleep(500 * time.Millisecond)
	}
	time.Sleep(3 * time.Second)
	if n := w.runs(contract.StageReview); n != 0 {
		stopMac()
		t.Fatalf("the review ran %d times with no worker on %s", n, xQueue)
	}
	if st := w.rec.states(); len(st) == 0 || st[len(st)-1] != core.StateInReview {
		stopMac()
		t.Fatalf("waiting item's states %v, want it in review", st)
	}

	// h2 joins with agent and x; mac stops.
	stopH2 := liveWorkersOn(t, c, w.a, build, CoreQueue, CapabilityQueue(nil), xQueue)
	defer stopH2()
	stopMac()

	deadline = time.Now().Add(2 * time.Minute)
	for {
		it, err := w.f.Tracker().Item(ctx, item)
		if err != nil {
			t.Fatal(err)
		}
		if it.CloseReason == ports.CloseCompleted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the item didn't merge within two minutes of h2 joining: states %v", w.rec.states())
		}
		time.Sleep(500 * time.Millisecond)
	}
	if w.runs(contract.StageImplement) != 1 || w.runs(contract.StageReview) != 1 {
		t.Fatalf("handler runs: implement %d, review %d; want 1 each", w.runs(contract.StageImplement), w.runs(contract.StageReview))
	}
}
