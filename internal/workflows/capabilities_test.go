package workflows

import (
	"context"
	"slices"
	"sync"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"

	"github.com/mayusuk/agent-loop/contract"
)

// TestFR14AC2_StageRunsOnItsCapabilityQueue: each stage's RunStage is
// scheduled on the queue of the capabilities it requires, so a stage that
// needs x runs only where a worker with x polls cap:agent+x, and it starts
// with its first attempt (TECHNICAL-SPEC §6.4). RunStage sets no
// schedule-to-start timeout, so it waits there for such a worker without
// using an attempt or its time limit (work plan #162 D3).
func TestFR14AC2_StageRunsOnItsCapabilityQueue(t *testing.T) {
	w := newWF(t)
	item := w.item()
	w.handle(contract.StageImplement, passing("agent/"+string(item)))
	w.handle(contract.StageReview, approving())
	w.cfg.Item.StageCaps = map[contract.Stage][]string{contract.StageReview: {"x", "agent"}}

	type run struct {
		queue   string
		attempt int32
	}
	var mu sync.Mutex
	var runs []run
	w.env.SetOnActivityStartedListener(func(info *activity.Info, ctx context.Context, _ converter.EncodedValues) {
		if info.ActivityType.Name != "RunStage" {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		runs = append(runs, run{info.TaskQueue, info.Attempt})
	})
	w.runProject()

	mu.Lock()
	defer mu.Unlock()
	want := []run{{"cap:agent", 1}, {"cap:agent+x", 1}}
	if !slices.Equal(runs, want) {
		t.Fatalf("RunStage calls %+v, want %+v", runs, want)
	}
}
