package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/mayusuk/agent-loop/internal/adapters/recording"
	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/workflows"
)

// serverView is what the server knows that the tracker doesn't: the
// pause, daily-limit and WIP state, and each claimed item's state and
// next action (TECHNICAL-SPEC §11).
type serverView struct {
	Project core.ProjectView
	Items   []claimedItem
}

// claimedItem is one claimed item as the server's board shows it.
type claimedItem struct {
	Key, State, Next, Rule string
}

// errServerUnavailable is serverState's answer while the server can't be
// asked.
var errServerUnavailable = errors.New("the server isn't running, or this build can't reach its API")

// serverState reads the server's view of the project. The local API that
// serves the board comes with #22; until then the server is never asked
// (work plan #23 D12). Tests replace it.
var serverState = func(context.Context, *localProject) (*serverView, error) {
	return nil, errServerUnavailable
}

// runDryRun is `agent-loop run --dry-run <project>` (FR-1 AC6,
// INTEGRATION-SPEC §12.3).
func runDryRun(args []string, stdout, stderr io.Writer) int {
	var dry bool
	cfg, name, ok := projectFlags("run", args, stderr, func(fs *flag.FlagSet) {
		fs.BoolVar(&dry, "dry-run", false, "show what one supervisor pass would do, writing nothing")
	})
	if !ok {
		return 2
	}
	if !dry {
		fmt.Fprintln(stderr, "agent-loop run: only --dry-run is supported; the loop itself runs in the server (agent-loop server start)")
		return 2
	}
	lp, err := loadProject(cfg, name)
	if err == nil {
		err = lp.profErr
	}
	if err != nil {
		fmt.Fprintf(stderr, "agent-loop run: %v\nRun agent-loop project validate %s for every problem.\n", err, name)
		return 1
	}
	ctx := context.Background()
	ad, probs, err := openAdapters(ctx, lp)
	if err == nil && ad == nil {
		var msgs []string
		for _, p := range probs {
			msgs = append(msgs, p.Message)
		}
		err = errors.New(strings.Join(msgs, "; "))
	}
	if err != nil {
		fmt.Fprintf(stderr, "agent-loop run: %v\n", err)
		return 1
	}
	defer ad.close()
	if err := dryRun(ctx, lp, ad, stdout); err != nil {
		fmt.Fprintf(stderr, "agent-loop run: %v\n", err)
		return 1
	}
	return 0
}

// dryRun makes one picking pass through write-recording adapters and
// prints what it would do (work plan #23 D12–D14). It starts no handler:
// nothing here can run one.
func dryRun(ctx context.Context, lp *localProject, ad *projectAdapters, w io.Writer) error {
	log := &recording.Log{}
	tr := recording.Tracker{WorkTracker: ad.tracker, Log: log}
	p := lp.prof
	fmt.Fprintf(w, "Dry run of %s: nothing is written and no handler starts.\n\n", lp.name)

	view := core.ProjectView{}
	sv, err := serverState(ctx, lp)
	if err != nil {
		fmt.Fprintf(w, "Server: %v, so claimed items and their next actions aren't shown.\n", err)
		fmt.Fprintf(w, "Limits: the pause, daily-limit and WIP state are unknown without the server; the picks below assume none of them applies (WIP 0 of %d).\n\n", p.WIP)
	} else {
		view = sv.Project
		fmt.Fprintf(w, "Limits: WIP %d of %d, daily limit %s, project %s, deployment %s.\n\n", view.Claimed, p.WIP,
			yesNo(view.DailyLimitReached, "reached", "not reached"), yesNo(view.Paused, "paused", "running"), yesNo(view.DeploymentPaused, "paused", "running"))
		fmt.Fprintln(w, "Claimed items:")
		if len(sv.Items) == 0 {
			fmt.Fprintln(w, "  (none)")
		}
		for _, it := range sv.Items {
			fmt.Fprintf(w, "  %s  %s, next: %s (%s)\n", it.Key, it.State, it.Next, it.Rule)
		}
		fmt.Fprintln(w)
	}

	// The supervisor's own activities make the pass, over the recording
	// tracker, so the dry run decides and writes exactly as a real pass
	// would (TECHNICAL-SPEC §6.2 step 4).
	acts := &workflows.Activities{Config: workflows.Config{Project: lp.name, Owners: portOwners(p),
		Identities: core.Identities{Coder: ad.ids.Coder, Reviewer: ad.ids.Reviewer, Loop: ad.ids.Loop}}, Tracker: tr}
	cands, err := acts.Candidates(ctx)
	if err != nil {
		return fmt.Errorf("reading the candidates: %w", err)
	}
	res := core.Pick(lp.name, cands, view, core.PickPolicy{Types: p.Types, Priority: p.Priority, WIP: p.WIP})
	fmt.Fprintln(w, "Candidates, in pick order:")
	if len(res.Verdicts) == 0 {
		fmt.Fprintln(w, "  (none: no open item has the ready marker)")
	}
	for _, v := range res.Verdicts {
		if v.Pick {
			fmt.Fprintf(w, "  %s  would pick\n", v.Key)
		} else {
			fmt.Fprintf(w, "  %s  excluded: %s (%s)\n", v.Key, v.Reason, exclusionText(v))
		}
	}

	for _, e := range res.Escalate {
		if err := acts.EscalateDependency(ctx, e); err != nil {
			return fmt.Errorf("escalating %s: %w", e.Item, err)
		}
	}
	var starts []string
	for _, item := range res.Picks {
		plan, err := acts.PlanClaim(ctx, item)
		if err != nil {
			return fmt.Errorf("planning the claim of %s: %w", item, err)
		}
		if _, err := acts.Claim(ctx, plan); err != nil {
			return err
		}
		starts = append(starts, plan.Plan.WorkflowID)
	}

	fmt.Fprintln(w, "\nWould start:")
	if len(starts) == 0 {
		fmt.Fprintln(w, "  (nothing)")
	}
	for _, s := range starts {
		fmt.Fprintf(w, "  workflow %s\n", s)
	}
	fmt.Fprintln(w, "\nTracker writes it would have made:")
	writes := log.Writes()
	if len(writes) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	for _, wr := range writes {
		fmt.Fprintf(w, "  %s\n", wr)
	}
	return nil
}

func yesNo(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}

// exclusionText is the FR-2 AC1 rule behind an exclusion, in words
// (INTEGRATION-SPEC §12.3, work plan #23 D14).
func exclusionText(v core.Verdict) string {
	switch v.Reason {
	case core.ExcludeClosed:
		return "closed"
	case core.ExcludeClaimed:
		return "already claimed"
	case core.ExcludePlan:
		return "has the plan marker"
	case core.ExcludeNotReady:
		return "not ready"
	case core.ExcludeType:
		return "not of a configured type"
	case core.ExcludeHumanAssignee:
		return "has human assignee"
	case core.ExcludeAborted:
		return "aborted"
	case core.ExcludePaused:
		return "paused"
	case core.ExcludeNeedsOwner:
		if v.Dependency != "" {
			return "needs-owner, dependency " + v.Dependency + " closed without being done"
		}
		return "needs-owner"
	case core.ExcludeDependencyNotDone:
		return "dependency " + v.Dependency + " closed without being done; the loop would add needs-owner"
	case core.ExcludeDependencyOpen:
		return "dependency " + v.Dependency + " not done"
	case core.ExcludeDeploymentPaused:
		return "the deployment is paused"
	case core.ExcludeProjectPaused:
		return "the project is paused"
	case core.ExcludeDailyLimit:
		return "the daily limit is reached"
	case core.ExcludeWIP:
		return "the WIP limit is full"
	}
	return string(v.Reason)
}
