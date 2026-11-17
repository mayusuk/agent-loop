package workflows

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"

	"github.com/mayusuk/agent-loop/contract"
)

// FR-10 AC3, work plan D6: a registered project hears the deployment pause
// at once and every change after it; a repeated pause sends nothing.
func TestFR10AC3_DeploymentPauseReachesProjects(t *testing.T) {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(DeploymentWorkflow)
	var mu sync.Mutex
	got := map[string][]bool{}
	env.OnSignalExternalWorkflow(mock.Anything, mock.Anything, "", SignalDeploymentPause, mock.Anything).Return(
		func(_, id, _, _ string, arg any) error {
			mu.Lock()
			defer mu.Unlock()
			got[id] = append(got[id], arg.(PauseSignal).Paused)
			return nil
		})
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(SignalRegister, RegisterSignal{WorkflowID: "project/a"}) }, time.Second)
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(SignalPause, PauseSignal{Paused: true}) }, 2*time.Second)
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(SignalPause, PauseSignal{Paused: true}) }, 3*time.Second)
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(SignalRegister, RegisterSignal{WorkflowID: "project/b"}) }, 4*time.Second)
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(SignalPause, PauseSignal{Paused: false}) }, 5*time.Second)
	env.RegisterDelayedCallback(func() {
		v, err := env.QueryWorkflow(QueryState)
		if err != nil {
			t.Error(err)
			return
		}
		var st DeploymentState
		if err := v.Get(&st); err != nil || st.Paused || len(st.Projects) != 2 {
			t.Errorf("state %+v %v", st, err)
		}
		env.CancelWorkflow()
	}, 6*time.Second)
	env.ExecuteWorkflow(DeploymentWorkflow, DeploymentState{})

	mu.Lock()
	defer mu.Unlock()
	if a := got["project/a"]; len(a) != 3 || a[0] || !a[1] || a[2] {
		t.Errorf("project/a heard %v, want [false true false]", a)
	}
	if b := got["project/b"]; len(b) != 2 || !b[0] || b[1] {
		t.Errorf("project/b heard %v, want [true false]", b)
	}
}

// TECHNICAL-SPEC §6.4: queue names are the sorted capability set, and a
// worker polls core and every stage queue its capabilities cover.
func TestFR14AC1_TaskQueues(t *testing.T) {
	if q := CapabilityQueue(nil); q != "cap:agent" {
		t.Errorf("default queue %q", q)
	}
	if q := CapabilityQueue([]string{"x", "agent", "x"}); q != "cap:agent+x" {
		t.Errorf("queue %q", q)
	}
	stages := map[contract.Stage][]string{contract.StageImplement: nil, contract.StageReview: {"agent", "gpu"}}
	if q := WorkerQueues([]string{"agent"}, stages); !slices.Equal(q, []string{CoreQueue, "cap:agent"}) {
		t.Errorf("agent worker polls %v", q)
	}
	if q := WorkerQueues([]string{"gpu", "agent"}, stages); !slices.Equal(q, []string{CoreQueue, "cap:agent", "cap:agent+gpu"}) {
		t.Errorf("gpu worker polls %v", q)
	}
	if q := WorkerQueues(nil, stages); !slices.Equal(q, []string{CoreQueue}) {
		t.Errorf("worker with no capabilities polls %v", q)
	}
}
