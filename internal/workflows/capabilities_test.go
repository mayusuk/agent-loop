package workflows

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/core"
)

// TestFR14AC2_StageRunsOnItsCapabilityQueue: each stage's RunStage is
// scheduled on the queue of the capabilities it requires, so a stage that
// needs x runs only where a worker with x polls cap:agent+x, and it starts
// with its first attempt (TECHNICAL-SPEC §6.4). That it can wait there
// without a timeout is TestFR14AC2_QueuedStageHasNoScheduleTimeout.
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

// TestFR14AC2_QueuedStageHasNoScheduleTimeout: RunStage, on the stage's
// capability queue, sets no schedule-to-start or schedule-to-close timeout
// and allows one attempt, so a stage that waits for a capable host waits
// without timing out or using an attempt; only its time limit, from the
// start, applies (FR-14 AC2, work plan #162 D3).
func TestFR14AC2_QueuedStageHasNoScheduleTimeout(t *testing.T) {
	s := ItemSettings{
		Policy:    core.Policy{Stages: map[contract.Stage]core.StageLimits{contract.StageReview: {TimeLimit: 7 * time.Minute}}},
		StageCaps: map[contract.Stage][]string{contract.StageReview: {"x", "agent"}},
	}
	for _, tc := range []struct {
		stage contract.Stage
		queue string
		limit time.Duration
	}{
		{contract.StageReview, "cap:agent+x", 7 * time.Minute},
		{contract.StageImplement, "cap:agent", core.DefaultTimeLimit}, // no caps, no limit: the defaults
	} {
		o := stageOptions(s, tc.stage)
		if o.TaskQueue != tc.queue {
			t.Errorf("%s: queue %q, want %q", tc.stage, o.TaskQueue, tc.queue)
		}
		if o.ScheduleToStartTimeout != 0 || o.ScheduleToCloseTimeout != 0 {
			t.Errorf("%s: schedule-to-start %v, schedule-to-close %v, want neither", tc.stage, o.ScheduleToStartTimeout, o.ScheduleToCloseTimeout)
		}
		if o.StartToCloseTimeout != tc.limit {
			t.Errorf("%s: start-to-close %v, want %v", tc.stage, o.StartToCloseTimeout, tc.limit)
		}
		if o.RetryPolicy == nil || o.RetryPolicy.MaximumAttempts != 1 {
			t.Errorf("%s: retry policy %+v, want one attempt", tc.stage, o.RetryPolicy)
		}
	}
}
