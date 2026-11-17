package workflows

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/log"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/adapters/fake"
	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/projection"
)

// recorder is a Projection that keeps what the workflows wrote.
type recorder struct {
	mu        sync.Mutex
	decisions []projection.Decision
	starts    []projection.StageRun
	ends      []RunEnd
	releases  []projection.Release
	daily     int
}

func (r *recorder) RecordDecision(_ context.Context, d projection.Decision) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.decisions = append(r.decisions, d)
	return nil
}

func (r *recorder) StartRun(_ context.Context, s projection.StageRun) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts = append(r.starts, s)
	return nil
}

func (r *recorder) FinishRun(_ context.Context, id string, ended time.Time, result, log string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ends = append(r.ends, RunEnd{RunID: id, Ended: ended, Result: result, LogPath: log})
	return nil
}

func (r *recorder) RecordRelease(_ context.Context, rel projection.Release) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.releases = append(r.releases, rel)
	return nil
}

func (r *recorder) DailyCount(context.Context, string, time.Time, *time.Location) (int, error) {
	return r.daily, nil
}

// stages returns the stages started, in order, and the results recorded.
func (r *recorder) stages() (started []contract.Stage, results []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.starts {
		started = append(started, s.Stage)
	}
	for _, e := range r.ends {
		results = append(results, e.Result)
	}
	return
}

func (r *recorder) states() []core.State {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []core.State
	for _, d := range r.decisions {
		if n := len(out); n == 0 || out[n-1] != d.Item.State {
			out = append(out, d.Item.State)
		}
	}
	return out
}

// notices records project and item notifications.
type notices struct {
	mu  sync.Mutex
	got []ports.Notification
}

func (n *notices) Notify(_ context.Context, x ports.Notification) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.got = append(n.got, x)
	return nil
}

func (n *notices) reasons() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for _, x := range n.got {
		out = append(out, x.Reason)
	}
	return out
}

// wfHarness runs the workflows in Temporal's test environment against the
// fake adapter, with real activities.
type wfHarness struct {
	*harness
	suite testsuite.WorkflowTestSuite
	env   *testsuite.TestWorkflowEnvironment
	rec   *recorder
	notes *notices
	cfg   ProjectConfig
	// handlers by stage; each may be replaced by a test.
	mu       sync.Mutex
	handlers map[contract.Stage]handler
	ran      map[contract.Stage]int
}

func newWF(t *testing.T) *wfHarness {
	h := newHarness(t)
	w := &wfHarness{harness: h, rec: &recorder{}, notes: &notices{}, handlers: map[contract.Stage]handler{}, ran: map[contract.Stage]int{}}
	h.a.Projection = w.rec
	h.a.Notifier = w.notes
	h.a.Identities = core.Identities{Owners: []string{fake.Owner.ID}, Coder: fake.Coder.ID, Reviewer: fake.Reviewer.ID, Loop: fake.Loop.ID}
	h.next = func(h *harness, in contract.Input, out string) string {
		w.mu.Lock()
		hd := w.handlers[in.Stage]
		w.ran[in.Stage]++
		w.mu.Unlock()
		if hd == nil {
			t.Errorf("no handler for %s", in.Stage)
			return ""
		}
		return hd(h, in, out)
	}
	w.cfg = ProjectConfig{
		Name:               "p",
		Item:               ItemSettings{PollInterval: time.Minute},
		Timezone:           "UTC",
		PollInterval:       time.Minute,
		SupervisorInterval: 2 * time.Hour,
	}
	w.suite.SetLogger(log.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	w.env = w.suite.NewTestWorkflowEnvironment()
	w.env.SetStartTime(h.f.Now())
	w.env.SetTestTimeout(time.Minute)
	w.env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: ProjectID("p")})
	RegisterWorkflows(w.env)
	RegisterActivities(w.env, h.a)
	w.env.OnSignalExternalWorkflow(mock.Anything, DeploymentID, "", SignalRegister, mock.Anything).Return(nil).Maybe()
	// The fake's clock follows the workflow clock, so records and
	// events carry the test environment's time.
	h.a.Now = func() time.Time { return w.env.Now() }
	return w
}

func (w *wfHarness) handle(st contract.Stage, hd handler) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.handlers[st] = hd
}

func (w *wfHarness) runs(st contract.Stage) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ran[st]
}

// passing is an implementer that also sets the required gate to pass on
// the head it pushes.
func passing(branch string) handler {
	impl := implementer(branch)
	return func(h *harness, in contract.Input, out string) string {
		res := impl(h, in, out)
		if err := h.f.SetGate(h.head(branch), "ci", ports.GatePass); err != nil {
			h.t.Errorf("gate: %v", err)
		}
		return res
	}
}

// approving is a review with no findings.
func approving() handler { return reviewer(nil, nil) }

// runProject runs ProjectWorkflow until its first supervisor interval
// ends it with continue-as-new, and returns the state it carried.
func (w *wfHarness) runProject() ProjectState {
	w.t.Helper()
	w.env.ExecuteWorkflow(ProjectWorkflow, ProjectInput{Config: w.cfg})
	if !w.env.IsWorkflowCompleted() {
		w.t.Fatal("project workflow did not finish")
	}
	err := w.env.GetWorkflowError()
	var can *workflow.ContinueAsNewError
	if !errors.As(err, &can) {
		w.t.Fatalf("project workflow ended with %v, want continue-as-new", err)
	}
	var in ProjectInput
	if err := converter.GetDefaultDataConverter().FromPayloads(can.Input, &in); err != nil {
		w.t.Fatal(err)
	}
	if in.State == nil {
		w.t.Fatal("continue-as-new carried no state")
	}
	return *in.State
}

// itemState reads the item's tracker view.
func (w *wfHarness) itemState(ref ports.ItemRef) ports.Item {
	w.t.Helper()
	it, err := w.f.Tracker().Item(context.Background(), ref)
	if err != nil {
		w.t.Fatal(err)
	}
	return it
}

func hasMarker(it ports.Item, m ports.Marker) bool {
	for _, x := range it.Markers {
		if x == m {
			return true
		}
	}
	return false
}
