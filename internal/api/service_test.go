package api

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"go.temporal.io/api/serviceerror"

	apiv1 "github.com/mayusuk/agent-loop/internal/api/v1"
	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/projection"
	"github.com/mayusuk/agent-loop/internal/workflows"
)

type viewWorkflows struct {
	stubWorkflows
	view workflows.ProjectView
}

func (v *viewWorkflows) QueryProject(context.Context, string) (workflows.ProjectView, error) {
	return v.view, nil
}

// FR-1 AC6, work plan D9: the board carries the supervisor's WIP in use,
// pause and daily count, so a dry run against a running server reads them.
func TestFR1AC6_BoardCarriesSupervisorState(t *testing.T) {
	db, err := projection.Open(context.Background(), filepath.Join(shortDir(t), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	wf := &viewWorkflows{view: workflows.ProjectView{
		DailyLimitReached: true,
		State: workflows.ProjectState{
			Paused: true,
			Items:  map[string]workflows.RunningItem{"9": {WorkflowID: "item/p/9"}, "4": {WorkflowID: "item/p/4"}},
			Daily:  core.DailyCount{Day: "2026-11-17", Runs: 20},
			Spend:  workflows.Spend{Day: "2026-11-17", Amount: 3.5},
		},
	}}
	s := &Service{Workflows: wf, Projection: db, Projects: []string{"p"}, Now: time.Now}
	b, err := s.Board(context.Background(), &apiv1.BoardRequest{})
	if err != nil {
		t.Fatal(err)
	}
	p := b.GetProjects()[0]
	if !b.GetDeploymentPaused() || !p.GetPaused() || !p.GetDailyLimitReached() ||
		!reflect.DeepEqual(p.GetRunningItems(), []string{"4", "9"}) ||
		p.GetDailyRuns() != 20 || p.GetDay() != "2026-11-17" || p.GetDailySpend() != 3.5 {
		t.Fatalf("board %v", b)
	}
	// Spend from an earlier day isn't today's.
	wf.view.State.Spend.Day = "2026-11-16"
	b, _ = s.Board(context.Background(), &apiv1.BoardRequest{})
	if got := b.GetProjects()[0].GetDailySpend(); got != 0 {
		t.Fatalf("stale spend %v", got)
	}
}

type startingWorkflows struct{ stubWorkflows }

func (*startingWorkflows) QueryProject(context.Context, string) (workflows.ProjectView, error) {
	return workflows.ProjectView{}, queryErr(serviceerror.NewQueryFailed("unknown queryType state. KnownQueryTypes=[__stack_trace]"))
}

// TestFR11AC1_BoardSaysAProjectIsStarting: a project workflow that runs
// but hasn't registered its state query yet is "starting", not Temporal's
// raw error (work plan #105 D2).
func TestFR11AC1_BoardSaysAProjectIsStarting(t *testing.T) {
	db, err := projection.Open(context.Background(), filepath.Join(shortDir(t), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &Service{Workflows: &startingWorkflows{}, Projection: db, Projects: []string{"p"}, Now: time.Now}
	b, err := s.Board(context.Background(), &apiv1.BoardRequest{})
	if err != nil {
		t.Fatal(err)
	}
	p := b.GetProjects()[0]
	if want := "project p is starting: try again in a moment"; !p.GetUnknown() || p.GetError() != want {
		t.Fatalf("unknown %v, error %q, want %q", p.GetUnknown(), p.GetError(), want)
	}
}

// TestQueryErr maps Temporal's query errors: an unknown query type is
// ErrStarting, a missing workflow ErrNotRunning, anything else is kept.
func TestQueryErr(t *testing.T) {
	if err := queryErr(serviceerror.NewQueryFailed("unknown queryType state. KnownQueryTypes=[]")); !errors.Is(err, ErrStarting) {
		t.Errorf("unknown query type: %v", err)
	}
	if err := queryErr(serviceerror.NewNotFound("workflow not found")); !errors.Is(err, ErrNotRunning) || errors.Is(err, ErrStarting) {
		t.Errorf("not found: %v", err)
	}
	other := serviceerror.NewQueryFailed("panic in query handler")
	if err := queryErr(other); errors.Is(err, ErrStarting) || errors.Is(err, ErrNotRunning) || !errors.Is(err, other) {
		t.Errorf("other query failure: %v", err)
	}
}
