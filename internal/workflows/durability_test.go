package workflows

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/mock"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/ports"
)

var errKilled = errors.New("worker killed")

// NFR-1 AC1, FR-16 AC1, D42: the worker is killed in every stage after the
// stage did its work but before its result reached the workflow. The rerun
// (same attempt, new run ID) finds what the killed run left: implement
// adopts the change and review finds its posted review, so no handler runs
// twice; the merge retry finds the change merged. Nothing completed is
// lost, and the item still merges once.
func TestNFR1AC1_WorkerKilledInEveryStage(t *testing.T) {
	w := newWF(t)
	item := w.item()
	w.handle(contract.StageImplement, passing("agent/"+string(item)))
	w.handle(contract.StageReview, approving())

	var mu sync.Mutex
	killed := map[string]bool{}
	kill := func(what string) bool {
		mu.Lock()
		defer mu.Unlock()
		if killed[what] {
			return false
		}
		killed[what] = true
		return true
	}
	w.env.OnActivity("RunStage", mock.Anything, mock.Anything).Return(func(ctx context.Context, req StageRequest) (StageReport, error) {
		rep, err := w.a.RunStage(ctx, req)
		if err == nil && kill(string(req.Run.Stage)) {
			return StageReport{}, errKilled
		}
		return rep, err
	})
	w.env.OnActivity("Merge", mock.Anything, mock.Anything).Return(func(ctx context.Context, act core.Merge) (core.MergeResult, error) {
		res, err := w.a.Merge(ctx, act)
		if err == nil && kill("merge") {
			return core.MergeResult{}, errKilled
		}
		return res, err
	})
	w.env.OnActivity("CheckMerge", mock.Anything, mock.Anything).Return(func(ctx context.Context, req MergeCheckRequest) (core.MergeChecked, error) {
		res, err := w.a.CheckMerge(ctx, req)
		if err == nil && kill("check") {
			return core.MergeChecked{}, errKilled
		}
		return res, err
	})

	w.runProject()

	for _, k := range []string{"implement", "review", "merge", "check"} {
		if !killed[k] {
			t.Errorf("never killed during %s", k)
		}
	}
	if w.runs(contract.StageImplement) != 1 || w.runs(contract.StageReview) != 1 {
		t.Fatalf("handler runs: implement %d, review %d; want 1 each", w.runs(contract.StageImplement), w.runs(contract.StageReview))
	}
	_, results := w.rec.stages()
	want := []string{contract.ReasonWorkerLost, "done", contract.ReasonWorkerLost, "done"}
	if !slices.Equal(results, want) {
		t.Fatalf("run results %v, want %v", results, want)
	}
	if !slices.Contains(w.rec.states(), core.StateMerged) {
		t.Fatalf("states %v", w.rec.states())
	}
	it := w.itemState(item)
	if it.CloseReason != ports.CloseCompleted {
		t.Fatalf("item not closed: %+v", it)
	}
	merged := 0
	w.rec.mu.Lock()
	for _, d := range w.rec.decisions {
		if d.Record.Event == "merge_result" {
			merged++
		}
	}
	w.rec.mu.Unlock()
	if merged != 1 {
		t.Fatalf("%d merge results, want 1", merged)
	}
}
