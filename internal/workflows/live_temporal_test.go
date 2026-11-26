package workflows

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/ports"
)

// The workflows on a real Temporal server (work plan #25, deferred from
// #22 review R3): worker versioning with the deployment's current version,
// task queues, and a worker killed and restarted mid-item, against the
// fake adapter and scripted handlers. Skipped unless AGENT_LOOP_LIVE=1
// (plan D5); see docs/LIVE-SCENARIO.md.

// versioningArgs turn on worker versioning on a dev server, as the
// Compose stack's dynamicconfig.yaml does.
var versioningArgs = []string{
	"--dynamic-config-value", "system.enableDeploymentVersions=true",
	"--dynamic-config-value", "frontend.workerVersioningWorkflowAPIs=true",
	"--dynamic-config-value", "frontend.workerVersioningDataAPIs=true",
}

// TestLiveTemporal_DevServer runs the live check on a Temporal dev server
// the test starts (the SDK downloads the CLI), or on the server at
// AGENT_LOOP_TEMPORAL_ADDRESS (namespace AGENT_LOOP_TEMPORAL_NAMESPACE,
// default "default") when it is set.
func TestLiveTemporal_DevServer(t *testing.T) {
	liveItemThroughMerge(t, liveDevClient(t))
}

// liveDevClient skips the test unless AGENT_LOOP_LIVE=1, and returns a
// client of a dev server it starts, or of the server at
// AGENT_LOOP_TEMPORAL_ADDRESS. Both are closed when the test ends.
func liveDevClient(t *testing.T) client.Client {
	t.Helper()
	if os.Getenv("AGENT_LOOP_LIVE") != "1" {
		t.Skip("live Temporal tests run only with AGENT_LOOP_LIVE=1")
	}
	if addr := os.Getenv("AGENT_LOOP_TEMPORAL_ADDRESS"); addr != "" {
		ns := os.Getenv("AGENT_LOOP_TEMPORAL_NAMESPACE")
		if ns == "" {
			ns = "default"
		}
		c, err := client.Dial(client.Options{HostPort: addr, Namespace: ns})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.Close)
		return c
	}
	srv, err := testsuite.StartDevServer(context.Background(), testsuite.DevServerOptions{ExtraArgs: versioningArgs, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Stop() })
	return srv.Client()
}

// TestLiveTemporal_ComposeStack brings up the Compose stack that `agent-loop
// server init` writes (Temporal on Postgres, 127.0.0.1:7233) under its own
// Compose project, runs the live check on it, and takes it down with its
// volume. It needs AGENT_LOOP_LIVE=1, AGENT_LOOP_COMPOSE=1, Docker, and port
// 7233 free.
func TestLiveTemporal_ComposeStack(t *testing.T) {
	if os.Getenv("AGENT_LOOP_LIVE") != "1" || os.Getenv("AGENT_LOOP_COMPOSE") != "1" {
		t.Skip("the Compose check runs only with AGENT_LOOP_LIVE=1 and AGENT_LOOP_COMPOSE=1")
	}
	dir := t.TempDir()
	for _, f := range []string{"compose.yaml", "dynamicconfig.yaml"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "cmd", "agent-loop", "compose", f))
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(dir, f)
		if f == "dynamicconfig.yaml" { // compose.yaml mounts ./dynamicconfig
			os.MkdirAll(filepath.Join(dir, "dynamicconfig"), 0o755)
			dst = filepath.Join(dir, "dynamicconfig", "agent-loop.yaml")
		}
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pw := make([]byte, 16)
	rand.Read(pw)
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("POSTGRES_PASSWORD="+hex.EncodeToString(pw)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	compose := func(args ...string) {
		t.Helper()
		cmd := exec.Command("docker", append([]string{"compose", "-p", "agent-loop-livetest", "-f", filepath.Join(dir, "compose.yaml")}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("docker compose %v: %v\n%s", args, err, out)
		}
	}
	compose("up", "-d")
	t.Cleanup(func() { compose("down", "-v") })

	var c client.Client
	deadline := time.Now().Add(3 * time.Minute)
	for {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:7233", time.Second)
		if err == nil {
			conn.Close()
			if c, err = client.Dial(client.Options{HostPort: "127.0.0.1:7233"}); err == nil {
				if _, err = c.CheckHealth(context.Background(), nil); err == nil {
					break
				}
				c.Close()
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("Temporal on the Compose stack isn't up after 3 minutes: %v", err)
		}
		time.Sleep(2 * time.Second)
	}
	defer c.Close()
	liveItemThroughMerge(t, c)
}

// liveWorkers starts versioned workers for build on the core queue and the
// agent capability queue.
func liveWorkers(t *testing.T, c client.Client, a *Activities, build string) func() {
	t.Helper()
	return liveWorkersOn(t, c, a, build, CoreQueue, CapabilityQueue(nil))
}

// liveWorkersOn starts versioned workers for build on the queues; the
// core queue's also runs the workflows.
func liveWorkersOn(t *testing.T, c client.Client, a *Activities, build string, queues ...string) func() {
	t.Helper()
	var ws []worker.Worker
	for _, q := range queues {
		wk := worker.New(c, q, WorkerOptions(build))
		if q == CoreQueue {
			RegisterWorkflows(wk)
		}
		RegisterActivities(wk, a)
		if err := wk.Start(); err != nil {
			t.Fatal(err)
		}
		ws = append(ws, wk)
	}
	return func() {
		for _, wk := range ws {
			wk.Stop()
		}
	}
}

// liveItemThroughMerge claims one item on a real server and takes it
// through implement, review and merge with the worker killed and
// restarted after the implement: the item still merges, and no handler
// runs twice (NFR-1 AC1, FR-12 AC2).
func liveItemThroughMerge(t *testing.T, c client.Client) {
	ctx := context.Background()
	w := newWF(t)
	w.a.Now = time.Now
	name := fmt.Sprintf("live%d", time.Now().UnixNano())
	w.a.Project = name
	item := w.item()
	implemented := make(chan struct{})
	impl := passing("agent/" + string(item))
	w.handle(contract.StageImplement, func(h *harness, in contract.Input, out string) string {
		defer close(implemented)
		return impl(h, in, out)
	})
	w.handle(contract.StageReview, approving())

	build := "live-" + name
	stop := liveWorkers(t, c, w.a, build)
	h := c.WorkerDeploymentClient().GetHandle(DeploymentName)
	var err error
	for range 30 {
		if _, err = h.SetCurrentVersion(ctx, client.WorkerDeploymentSetCurrentVersionOptions{BuildID: build}); err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		t.Fatalf("setting the current worker version: %v", err)
	}
	cfg := w.cfg
	cfg.Name, cfg.Item.Project = name, name
	cfg.PollInterval, cfg.Item.PollInterval = time.Second, time.Second
	cfg.SupervisorInterval = 10 * time.Minute
	pid := ProjectID(name)
	if _, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: pid, TaskQueue: CoreQueue}, ProjectWorkflow, ProjectInput{Config: cfg}); err != nil {
		t.Fatal(err)
	}
	defer c.TerminateWorkflow(context.Background(), pid, "", "live test done")

	select {
	case <-implemented:
	case <-time.After(time.Minute):
		stop()
		t.Fatal("implement didn't run within a minute")
	}
	// The worker host is killed and restarted with the same build.
	stop()
	time.Sleep(2 * time.Second)
	stop = liveWorkers(t, c, w.a, build)
	defer stop()

	deadline := time.Now().Add(2 * time.Minute)
	for {
		it, err := w.f.Tracker().Item(ctx, item)
		if err != nil {
			t.Fatal(err)
		}
		if it.CloseReason == ports.CloseCompleted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the item didn't merge within two minutes: states %v", w.rec.states())
		}
		time.Sleep(500 * time.Millisecond)
	}
	if w.runs(contract.StageImplement) != 1 || w.runs(contract.StageReview) != 1 {
		t.Fatalf("handler runs: implement %d, review %d; want 1 each", w.runs(contract.StageImplement), w.runs(contract.StageReview))
	}
	itemID := core.ClaimFor(name, string(item), 0).WorkflowID
	desc, err := c.DescribeWorkflowExecution(ctx, itemID, "")
	if err != nil {
		t.Fatal(err)
	}
	if v := desc.GetWorkflowExecutionInfo().GetVersioningInfo(); v == nil || v.GetDeploymentVersion().GetBuildId() != build {
		t.Fatalf("the item workflow isn't pinned to build %s: %v", build, v)
	}
}
