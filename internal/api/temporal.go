package api

import (
	"context"
	"errors"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"github.com/mayusuk/agent-loop/internal/workflows"
)

// TemporalWorkflows is Workflows over a Temporal client. Signals and
// queries go to each workflow's current run.
type TemporalWorkflows struct {
	Client client.Client
}

var _ Workflows = TemporalWorkflows{}

func notFound(err error) error {
	var nf *serviceerror.NotFound
	if errors.As(err, &nf) {
		return errors.Join(ErrNotRunning, err)
	}
	return err
}

// Signal signals the workflow's current run.
func (t TemporalWorkflows) Signal(ctx context.Context, id, name string, arg any) error {
	return notFound(t.Client.SignalWorkflow(ctx, id, "", name, arg))
}

// QueryProject queries the project workflow's state.
func (t TemporalWorkflows) QueryProject(ctx context.Context, project string) (workflows.ProjectView, error) {
	var v workflows.ProjectView
	enc, err := t.Client.QueryWorkflow(ctx, workflows.ProjectID(project), "", workflows.QueryState)
	if err != nil {
		return v, notFound(err)
	}
	return v, enc.Get(&v)
}

// QueryDeployment queries the deployment workflow's state.
func (t TemporalWorkflows) QueryDeployment(ctx context.Context) (workflows.DeploymentState, error) {
	var v workflows.DeploymentState
	enc, err := t.Client.QueryWorkflow(ctx, workflows.DeploymentID, "", workflows.QueryState)
	if err != nil {
		return v, notFound(err)
	}
	return v, enc.Get(&v)
}
