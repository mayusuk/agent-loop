package workflows

import (
	"slices"

	"go.temporal.io/sdk/workflow"
)

// DeploymentState is what DeploymentWorkflow carries across
// continue-as-new.
type DeploymentState struct {
	Paused   bool
	Projects []string // registered ProjectWorkflow IDs
}

// DeploymentWorkflow holds the deployment pause (FR-10 AC3; v0.1 has no
// deployment-wide slots). Projects register with it and get every pause
// change as a signal (work plan D6). It continues as new when Temporal
// suggests it (TECHNICAL-SPEC §6.2).
func DeploymentWorkflow(ctx workflow.Context, st DeploymentState) error {
	if err := workflow.SetQueryHandler(ctx, QueryState, func() (DeploymentState, error) { return st, nil }); err != nil {
		return err
	}
	pause := workflow.GetSignalChannel(ctx, SignalPause)
	reg := workflow.GetSignalChannel(ctx, SignalRegister)
	tell := func(id string) {
		err := workflow.SignalExternalWorkflow(ctx, id, "", SignalDeploymentPause, PauseSignal{Paused: st.Paused}).Get(ctx, nil)
		if err != nil {
			workflow.GetLogger(ctx).Warn("project not told of the deployment pause", "project", id, "error", err)
		}
	}
	onPause := func(s PauseSignal) {
		if s.Paused == st.Paused {
			return
		}
		st.Paused = s.Paused
		for _, id := range st.Projects {
			tell(id)
		}
	}
	onRegister := func(r RegisterSignal) {
		if !slices.Contains(st.Projects, r.WorkflowID) {
			st.Projects = append(st.Projects, r.WorkflowID)
			slices.Sort(st.Projects)
		}
		tell(r.WorkflowID)
	}
	for {
		sel := workflow.NewSelector(ctx)
		sel.AddReceive(pause, func(c workflow.ReceiveChannel, _ bool) {
			var s PauseSignal
			c.Receive(ctx, &s)
			onPause(s)
		})
		sel.AddReceive(reg, func(c workflow.ReceiveChannel, _ bool) {
			var r RegisterSignal
			c.Receive(ctx, &r)
			onRegister(r)
		})
		sel.Select(ctx)
		if workflow.GetInfo(ctx).GetContinueAsNewSuggested() {
			for {
				var s PauseSignal
				if !pause.ReceiveAsync(&s) {
					break
				}
				onPause(s)
			}
			for {
				var r RegisterSignal
				if !reg.ReceiveAsync(&r) {
					break
				}
				onRegister(r)
			}
			return workflow.NewContinueAsNewError(ctx, DeploymentWorkflow, st)
		}
	}
}
