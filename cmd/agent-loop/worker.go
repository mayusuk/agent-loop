package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/mayusuk/agent-loop/internal/projection"
	"github.com/mayusuk/agent-loop/internal/secrets"
	"github.com/mayusuk/agent-loop/internal/workflows"
)

// parseCapabilities splits a comma-separated capability list, dropping
// empty entries and duplicates, sorted.
func parseCapabilities(s string) []string {
	var out []string
	for _, c := range strings.Split(s, ",") {
		if c = strings.TrimSpace(c); c != "" && !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	slices.Sort(out)
	return out
}

// workerName is the worker's host name: --name, trimmed, or the OS host
// name when --name isn't given (plan D3). An empty name is an error (work
// plan #162 D1).
func workerName(flag string, given bool, osHost func() (string, error)) (string, error) {
	if given {
		if n := strings.TrimSpace(flag); n != "" {
			return n, nil
		}
		return "", errors.New("--name is empty")
	}
	n, err := osHost()
	if n = strings.TrimSpace(n); err != nil || n == "" {
		return "", fmt.Errorf("can't read the OS host name (%v): name this host with --name", err)
	}
	return n, nil
}

// heartbeatInterval is how often a worker records itself in the hosts
// table; a host is unhealthy after projection.HostHealthWindow without one
// (work plan #162 D2).
const heartbeatInterval = 15 * time.Second

// hostStore is the projection's hosts table.
type hostStore interface {
	Hosts(ctx context.Context) ([]projection.Host, error)
	UpsertHost(ctx context.Context, h projection.Host) error
}

// recordHost records the worker's name, capabilities and heartbeat at
// now, keeping the host's last check (FR-11 AC4, FR-14 AC1).
func recordHost(ctx context.Context, db hostStore, name string, caps []string, now time.Time) error {
	h := projection.Host{Name: name, Capabilities: caps, LastSeen: now}
	all, err := db.Hosts(ctx)
	if err != nil {
		return err
	}
	for _, old := range all {
		if old.Name == name {
			h.LastCheck = old.LastCheck // kept for agent-loop host check (v0.3)
		}
	}
	return db.UpsertHost(ctx, h)
}

// runWorker runs the project's activities on the core queue and on every
// stage queue the capabilities cover (TECHNICAL-SPEC §3, §6.4).
func runWorker(args []string, stdout, stderr io.Writer) int {
	const usage = "worker --capabilities <list> [--name <name>] [--config <path>]"
	fl, cfg := newFlags("worker", stderr)
	capList := fl.String("capabilities", "", "comma-separated capabilities of this host, for example agent,docker")
	nameFlag := fl.String("name", "", "this host's name on the board and its stage runs (default the OS host name)")
	if fl.Parse(args) != nil || fl.NArg() != 0 || len(parseCapabilities(*capList)) == 0 {
		return usageErr(stderr, usage)
	}
	caps := parseCapabilities(*capList)
	given := false
	fl.Visit(func(f *flag.Flag) { given = given || f.Name == "name" })
	host, err := workerName(*nameFlag, given, os.Hostname)
	if err != nil {
		fmt.Fprintf(stderr, "agent-loop worker: %v\n", err)
		return usageErr(stderr, usage)
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "agent-loop worker: %v\n", err)
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
	db, err := projection.Open(ctx, dep.Projection.Path)
	if err != nil {
		return fail(err)
	}
	defer db.Close()

	backend, err := openBackend(dep.Secrets)
	if err != nil {
		return fail(err)
	}
	if err := checkKeychainTrust(ctx, backend, identitySecretNames(prof.Identities), func(w string) {
		fmt.Fprintf(stderr, "agent-loop worker: warning: %s\n", w)
	}); err != nil {
		return fail(err)
	}
	red := secrets.NewRedactor()
	store, err := secrets.NewGitHubApps(backend, prof.Identities, red)
	if err != nil {
		return fail(err)
	}
	setup, tokens, err := resolveGitHub(ctx, store, prof.CodeHost.Repo)
	defer tokens.Close(context.WithoutCancel(ctx))
	if err != nil {
		return fail(err)
	}
	gh, err := newGitHubAdapters(name, prof, db, setup)
	if err != nil {
		return fail(err)
	}
	acts, err := newActivities(ctx, dep, name, prof, db, gh, setup, store, red)
	if err != nil {
		return fail(err)
	}
	acts.HostName = host // recorded on the stage runs it starts, shown on the board

	c, err := dialTemporal(dep)
	if err != nil {
		return fail(err)
	}
	defer c.Close()
	if err := claimName(ctx, db, host, caps, time.Now()); err != nil {
		return fail(err)
	}
	onFatal, fatal := fatalSink()
	opts := workflows.WorkerOptions(resolvedVersion())
	opts.OnFatalError = onFatal // a worker that stops polling stops the heartbeat (work plan #162 D2)
	opts.DisableWorkflowWorker = true // the server runs the workflows
	opts.Identity = fmt.Sprintf("%d@%s", os.Getpid(), host)
	queues := workflows.WorkerQueues(caps, stageCaps(prof))
	for _, q := range queues {
		w := worker.New(c, q, opts)
		workflows.RegisterActivities(w, acts)
		if err := w.Start(); err != nil {
			return fail(fmt.Errorf("queue %s: %w", q, err))
		}
		defer w.Stop()
	}
	// The heartbeat stands for polling: it is recorded only while the
	// Temporal server answers and every queue's worker runs (work plan #162
	// D2).
	check := func(ctx context.Context) error {
		_, err := c.CheckHealth(ctx, &client.CheckHealthRequest{})
		return err
	}
	beat := func() { heartbeat(ctx, check, db, host, caps, time.Now(), stderr) }
	beat()
	fmt.Fprintf(stdout, "agent-loop worker %s: project %s, capabilities %s, queues %s\n", host, name, strings.Join(caps, ","), strings.Join(queues, ", "))
	tick := time.NewTicker(heartbeatInterval)
	defer tick.Stop()
	return heartbeatLoop(ctx, tick.C, fatal, beat, stderr)
}

// claimName refuses a worker name that a healthy host already uses with
// other capabilities: one name is one host (plan D3), so two workers with
// it must poll the same queues, or the board would switch between their
// capability sets (work plan #162 D7).
func claimName(ctx context.Context, db hostStore, name string, caps []string, now time.Time) error {
	all, err := db.Hosts(ctx)
	if err != nil {
		return err
	}
	for _, h := range all {
		if h.Name == name && h.Healthy(now) && !slices.Equal(h.Capabilities, caps) {
			return fmt.Errorf("host %s is already running with capabilities %s: give this worker another --name, or stop that worker and wait until the board shows it unhealthy",
				name, strings.Join(h.Capabilities, ","))
		}
	}
	return nil
}

// fatalSink is a Temporal worker's OnFatalError and the channel that
// reports the first fatal error: the SDK has then stopped that worker.
func fatalSink() (func(error), <-chan error) {
	ch := make(chan error, 1)
	return func(err error) {
		select {
		case ch <- err:
		default:
		}
	}, ch
}

// heartbeat records the host as seen at now, only if check (the Temporal
// client's health check) passes (work plan #162 D2).
func heartbeat(ctx context.Context, check func(context.Context) error, db hostStore, host string, caps []string, now time.Time, stderr io.Writer) {
	hctx, cancel := context.WithTimeout(ctx, heartbeatInterval)
	defer cancel()
	if err := check(hctx); err != nil {
		fmt.Fprintf(stderr, "agent-loop worker: Temporal isn't answering, so the host isn't marked seen: %v\n", err)
		return
	}
	if err := recordHost(ctx, db, host, caps, now); err != nil {
		fmt.Fprintf(stderr, "agent-loop worker: recording the host: %v\n", err)
	}
}

// heartbeatLoop beats on every tick until ctx ends (exit 0) or a Temporal
// worker stops with a fatal error (exit 1). Then no more heartbeats are
// recorded, so the host turns unhealthy within a minute and the board
// shows the stages that wait for it (FR-14 AC2, FR-11 AC4).
func heartbeatLoop(ctx context.Context, tick <-chan time.Time, fatal <-chan error, beat func(), stderr io.Writer) int {
	for {
		select {
		case <-ctx.Done():
			return 0
		case err := <-fatal:
			fmt.Fprintf(stderr, "agent-loop worker: a Temporal worker stopped polling: %v\n", err)
			return 1
		case <-tick:
			beat()
		}
	}
}
