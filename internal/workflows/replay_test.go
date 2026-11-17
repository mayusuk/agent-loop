package workflows

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/ports"
)

// The recorded histories (TECHNICAL-SPEC §14): an item from claim to the
// loop's merge, and the project run that claimed it, up to its first
// continue-as-new.
var histories = []string{"item_claim_to_merge.json", "project_first_run.json"}

// TestT16_ReplayRecordedHistories replays each recorded history against
// the current workflow code. A workflow change that would break a running
// workflow fails here; such a change needs a new worker version.
func TestT16_ReplayRecordedHistories(t *testing.T) {
	for _, name := range histories {
		t.Run(name, func(t *testing.T) {
			r := worker.NewWorkflowReplayer()
			RegisterWorkflows(r)
			if err := r.ReplayWorkflowHistoryFromJSONFile(nil, filepath.Join("testdata", "histories", name)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestT16_RecordHistories writes the histories above from a run on a local
// Temporal dev server. It runs only with AGENT_LOOP_LIVE=1 and
// AGENT_LOOP_RECORD=1, after a change that needs new histories.
func TestT16_RecordHistories(t *testing.T) {
	if os.Getenv("AGENT_LOOP_LIVE") != "1" || os.Getenv("AGENT_LOOP_RECORD") != "1" {
		t.Skip("set AGENT_LOOP_LIVE=1 AGENT_LOOP_RECORD=1 to record histories on a dev server")
	}
	ctx := context.Background()
	srv, err := testsuite.StartDevServer(ctx, testsuite.DevServerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	c := srv.Client()

	w := newWF(t)
	w.a.Now = time.Now
	item := w.item()
	w.handle(contract.StageImplement, passing("agent/"+string(item)))
	w.handle(contract.StageReview, approving())
	for _, q := range []string{CoreQueue, CapabilityQueue(nil)} {
		wk := worker.New(c, q, worker.Options{})
		if q == CoreQueue {
			RegisterWorkflows(wk)
		}
		RegisterActivities(wk, w.a)
		if err := wk.Start(); err != nil {
			t.Fatal(err)
		}
		defer wk.Stop()
	}
	cfg := w.cfg
	cfg.PollInterval = time.Second
	cfg.Item.PollInterval = time.Second
	cfg.SupervisorInterval = 20 * time.Second
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: ProjectID("p"), TaskQueue: CoreQueue}, ProjectWorkflow, ProjectInput{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	itemID := core.ClaimFor("p", string(item), 0).WorkflowID
	deadline := time.Now().Add(time.Minute)
	for {
		it, err := w.f.Tracker().Item(ctx, item)
		if err != nil {
			t.Fatal(err)
		}
		if it.CloseReason == ports.CloseCompleted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the item did not merge within a minute")
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := c.GetWorkflow(ctx, itemID, "").Get(ctx, nil); err != nil {
		t.Fatal(err)
	}
	// The project's first run ends with continue-as-new at its first
	// reconcile.
	time.Sleep(25 * time.Second)
	save := func(id, runID, name string) {
		var events []*history.HistoryEvent
		it := c.GetWorkflowHistory(ctx, id, runID, false, enums.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
		for it.HasNext() {
			e, err := it.Next()
			if err != nil {
				t.Fatal(err)
			}
			events = append(events, e)
		}
		b, err := protojson.MarshalOptions{Indent: "  "}.Marshal(&history.History{Events: events})
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join("testdata", "histories")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	save(itemID, "", histories[0])
	save(ProjectID("p"), run.GetRunID(), histories[1])
	c.TerminateWorkflow(ctx, ProjectID("p"), "", "recorded")
}
