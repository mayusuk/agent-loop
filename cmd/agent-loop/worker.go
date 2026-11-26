package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

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

// runWorker runs the project's activities on the core queue and on every
// stage queue the capabilities cover (TECHNICAL-SPEC §3, §6.4).
func runWorker(args []string, stdout, stderr io.Writer) int {
	fl, cfg := newFlags("worker", stderr)
	capList := fl.String("capabilities", "", "comma-separated capabilities of this host, for example agent,docker")
	if fl.Parse(args) != nil || fl.NArg() != 0 || len(parseCapabilities(*capList)) == 0 {
		return usageErr(stderr, "worker --capabilities <list> [--config <path>]")
	}
	caps := parseCapabilities(*capList)
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
	host, _ := os.Hostname()
	acts.HostName = host // shown on the board while a stage runs here

	c, err := dialTemporal(dep)
	if err != nil {
		return fail(err)
	}
	defer c.Close()
	opts := workflows.WorkerOptions(resolvedVersion())
	opts.DisableWorkflowWorker = true // the server runs the workflows
	queues := workflows.WorkerQueues(caps, stageCaps(prof))
	for _, q := range queues {
		w := worker.New(c, q, opts)
		workflows.RegisterActivities(w, acts)
		if err := w.Start(); err != nil {
			return fail(fmt.Errorf("queue %s: %w", q, err))
		}
		defer w.Stop()
	}
	seen := func() {
		h := projection.Host{Name: host, Capabilities: caps, LastSeen: time.Now()}
		if all, err := db.Hosts(ctx); err == nil {
			for _, old := range all {
				if old.Name == host {
					h.LastCheck = old.LastCheck // kept for agent-loop host check
				}
			}
		}
		if err := db.UpsertHost(ctx, h); err != nil {
			fmt.Fprintf(stderr, "agent-loop worker: recording the host: %v\n", err)
		}
	}
	seen()
	fmt.Fprintf(stdout, "agent-loop worker on %s: project %s, capabilities %s, queues %s\n", host, name, strings.Join(caps, ","), strings.Join(queues, ", "))
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return 0
		case <-tick.C:
			seen()
		}
	}
}
