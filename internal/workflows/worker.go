package workflows

import (
	"context"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// DeploymentName is the Temporal worker deployment of every agent-loop
// worker (TECHNICAL-SPEC §6.3 worker versioning).
const DeploymentName = "agent-loop"

// WorkerOptions turns on worker versioning with the build's version
// (TECHNICAL-SPEC §6.3): an item workflow is pinned to the build that
// started it, so it is never replayed against incompatible code. The
// project and deployment workflows continue as new often and move to the
// current build (work plan D7).
func WorkerOptions(buildID string) worker.Options {
	return worker.Options{
		DeploymentOptions: worker.DeploymentOptions{
			UseVersioning:             true,
			Version:                   worker.WorkerDeploymentVersion{DeploymentName: DeploymentName, BuildID: buildID},
			DefaultVersioningBehavior: workflow.VersioningBehaviorPinned,
		},
	}
}

// RegisterWorkflows registers the three v0.1 workflows on a core-queue
// worker.
func RegisterWorkflows(r worker.WorkflowRegistry) {
	r.RegisterWorkflowWithOptions(ItemWorkflow, workflow.RegisterOptions{Name: "ItemWorkflow", VersioningBehavior: workflow.VersioningBehaviorPinned})
	r.RegisterWorkflowWithOptions(ProjectWorkflow, workflow.RegisterOptions{Name: "ProjectWorkflow", VersioningBehavior: workflow.VersioningBehaviorAutoUpgrade})
	r.RegisterWorkflowWithOptions(DeploymentWorkflow, workflow.RegisterOptions{Name: "DeploymentWorkflow", VersioningBehavior: workflow.VersioningBehaviorAutoUpgrade})
}

// RegisterActivities registers a's activities, and sets its heartbeat hook
// to Temporal's activity heartbeat (TECHNICAL-SPEC §6.5, #20 D1). Register
// it on the core queue and on every capability queue the worker polls.
func RegisterActivities(r worker.ActivityRegistry, a *Activities) {
	if a.Heartbeat == nil {
		a.Heartbeat = func(ctx context.Context) {
			if activity.IsActivity(ctx) {
				activity.RecordHeartbeat(ctx)
			}
		}
	}
	r.RegisterActivity(a)
}
