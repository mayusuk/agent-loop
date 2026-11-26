package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/mayusuk/agent-loop/internal/api"
	"github.com/mayusuk/agent-loop/internal/profile"
	"github.com/mayusuk/agent-loop/internal/projection"
	"github.com/mayusuk/agent-loop/internal/secrets"
	"github.com/mayusuk/agent-loop/internal/workflows"
)

// dialTemporal connects to the deployment's Temporal server.
func dialTemporal(dep *profile.Deployment) (client.Client, error) {
	c, err := client.Dial(client.Options{HostPort: dep.Temporal.Address, Namespace: dep.Temporal.Namespace})
	if err != nil {
		return nil, fmt.Errorf("connecting to Temporal at %s (is it running? agent-loop server init starts it): %w", dep.Temporal.Address, err)
	}
	return c, nil
}

// untilSignal returns a context that ends on SIGINT or SIGTERM.
func untilSignal() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// setCurrentVersion makes this build the worker deployment's current
// version (TECHNICAL-SPEC §6.3), so new workflows run on it. The version
// exists once this process's worker has polled, so it is retried for a
// while.
func setCurrentVersion(ctx context.Context, c client.Client, buildID string) error {
	h := c.WorkerDeploymentClient().GetHandle(workflows.DeploymentName)
	var err error
	for range 30 {
		if _, err = h.SetCurrentVersion(ctx, client.WorkerDeploymentSetCurrentVersionOptions{BuildID: buildID}); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("making build %s the current worker version: %w", buildID, err)
}

// runServerStart runs the server (TECHNICAL-SPEC §3, §11): the workflows on
// the core queue, the deployment and project workflows, and the local API.
// It loads every registered project, of which v0.1 allows one (work plan
// D8).
func runServerStart(args []string, stdout, stderr io.Writer) int {
	fl, cfg := newFlags("server start", stderr)
	if fl.Parse(args) != nil || fl.NArg() != 0 {
		return usageErr(stderr, "server start [--config <path>]")
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "agent-loop server start: %v\n", err)
		return 1
	}
	dep, err := loadDeployment(*cfg)
	if err != nil {
		return fail(err)
	}
	name, prof, err := onlyProject(dep)
	if err != nil {
		return fail(err)
	}
	ctx, stop := untilSignal()
	defer stop()
	// The server checks every item the deployment's processes read, the
	// workers' included, so a stale one shows up here, not as a stalled
	// run (work plan #95 D5).
	names := identitySecretNames(prof.Identities)
	if dep.API.Gateway.Enabled {
		if gw, err := secrets.ParseRef(dep.API.Gateway.Token); err == nil {
			names = append(names, gw)
		}
	}
	backend, err := openBackend(dep.Secrets)
	if err != nil {
		return fail(err)
	}
	if err := checkKeychainTrust(ctx, backend, names, func(w string) {
		fmt.Fprintf(stderr, "agent-loop server start: warning: %s\n", w)
	}); err != nil {
		return fail(err)
	}
	db, err := projection.Open(ctx, dep.Projection.Path)
	if err != nil {
		return fail(err)
	}
	defer db.Close()
	var gatewayToken string
	if dep.API.Gateway.Enabled {
		if gatewayToken, err = readSecret(ctx, dep, dep.API.Gateway.Token); err != nil {
			return fail(fmt.Errorf("api.gateway.token: %w", err))
		}
	}
	l, err := api.Listen(dep.API.Socket)
	if err != nil {
		return fail(err)
	}
	c, err := dialTemporal(dep)
	if err != nil {
		l.Close()
		return fail(err)
	}
	defer c.Close()

	buildID := resolvedVersion()
	opts := workflows.WorkerOptions(buildID)
	opts.LocalActivityWorkerOnly = true // activities run on workers
	w := worker.New(c, workflows.CoreQueue, opts)
	workflows.RegisterWorkflows(w)
	if err := w.Start(); err != nil {
		l.Close()
		return fail(err)
	}
	defer w.Stop()
	if err := setCurrentVersion(ctx, c, buildID); err != nil {
		l.Close()
		return fail(err)
	}
	start := func(id string, wf string, arg any) error {
		_, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
			ID: id, TaskQueue: workflows.CoreQueue,
			WorkflowIDConflictPolicy: enumspb.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
		}, wf, arg)
		return err
	}
	if err := start(workflows.DeploymentID, "DeploymentWorkflow", workflows.DeploymentState{}); err != nil {
		l.Close()
		return fail(err)
	}
	pcfg := projectConfig(name, prof)
	if err := start(workflows.ProjectID(name), "ProjectWorkflow", workflows.ProjectInput{Config: pcfg}); err != nil {
		l.Close()
		return fail(err)
	}

	svc := &api.Service{Workflows: api.TemporalWorkflows{Client: c}, Projection: db, Projects: []string{name}, StageCaps: boardStageCaps(pcfg)}
	g := api.NewServer(svc)
	errs := make(chan error, 2)
	go func() { errs <- g.Serve(l) }()
	defer g.GracefulStop()
	if dep.API.Gateway.Enabled {
		gl, err := api.ListenGateway(dep.API.Gateway.Address)
		if err != nil {
			return fail(err)
		}
		hs := &http.Server{Handler: api.Gateway(svc, gatewayToken), ReadHeaderTimeout: 10 * time.Second}
		go func() { errs <- hs.Serve(gl) }()
		defer hs.Close()
		fmt.Fprintf(stdout, "Gateway on http://%s\n", dep.API.Gateway.Address)
	}
	fmt.Fprintf(stdout, "agent-loop server %s: project %s, API on %s. Start a worker with agent-loop worker --capabilities agent.\n", buildID, name, dep.API.Socket)
	select {
	case <-ctx.Done():
		return 0
	case err := <-errs:
		if errors.Is(err, http.ErrServerClosed) {
			return 0
		}
		return fail(err)
	}
}

// readSecret reads the secret a reference names from the deployment's
// backend (work plan D2).
func readSecret(ctx context.Context, dep *profile.Deployment, ref string) (string, error) {
	name, err := secrets.ParseRef(ref)
	if err != nil {
		return "", err
	}
	b, err := secrets.Open(dep.Secrets)
	if err != nil {
		return "", err
	}
	v, err := b.Get(ctx, name)
	if err != nil {
		return "", err
	}
	if v = strings.TrimSpace(v); v == "" {
		return "", fmt.Errorf("secret %s is empty", name)
	}
	return v, nil
}
