package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/profile"
	"github.com/mayusuk/agent-loop/internal/runners"
)

// runValidate is `agent-loop project validate <project>` (FR-1 AC2, FR-13
// AC1, AC4, NFR-4 AC1). It exits 1 when any problem is an error (work plan
// #23 D1).
func runValidate(args []string, stdout, stderr io.Writer) int {
	cfg, name, ok := projectFlags("project validate", args, stderr, nil)
	if !ok {
		return 2
	}
	lp, err := loadProject(cfg, name)
	if err != nil {
		fmt.Fprintf(stderr, "agent-loop project validate: %v\n", err)
		return 1
	}
	probs := validateProject(context.Background(), lp)
	return printProblems(stdout, name, probs)
}

// validateProject runs every check on the project (work plan #23 D2–D10).
func validateProject(ctx context.Context, lp *localProject) []ports.Problem {
	if lp.profErr != nil {
		var pe *profile.Error
		if !errors.As(lp.profErr, &pe) {
			return []ports.Problem{{Severity: ports.SeverityError, Code: "profile", Message: lp.profErr.Error(), Hint: "fix the profile"}}
		}
		var out []ports.Problem
		for _, p := range pe.Problems {
			out = append(out, ports.Problem{Severity: ports.SeverityError, Code: "profile", Message: p.String(),
				Hint: "fix the value; the profile keys are in TECHNICAL-SPEC §13"})
		}
		return out
	}
	p := lp.prof
	out := checkStages(p)
	if p.Gates != nil && len(p.Gates) == 0 {
		out = append(out, ports.Problem{Severity: ports.SeverityWarning, Code: "no_gates",
			Message: "gates = [] replaces the base branch's required checks with none, so changes merge without any check",
			Hint:    "remove gates to use the base branch's required checks, or list the checks that must pass"})
	}
	ad, probs, err := openAdapters(ctx, lp)
	out = append(out, probs...)
	if err != nil {
		return append(out, ports.Problem{Severity: ports.SeverityError, Code: "adapters_unavailable",
			Message: fmt.Sprintf("can't open the project's adapters: %v", err), Hint: "fix the problem above, then validate again"})
	}
	if ad == nil {
		return out
	}
	defer ad.close()
	if _, err := p.Policy().WithIdentityIDs(ad.ids); err != nil {
		out = append(out, ports.Problem{Severity: ports.SeverityError, Code: "identity_conflict", Message: err.Error(),
			Hint: "use three different GitHub Apps, and list only human accounts as owners (FR-13 AC1)"})
	}
	out = append(out, checkCapabilities(p, ad.tracker.Capabilities())...)
	out = append(out, ad.tracker.Validate(ctx, trackerProfile(p))...)
	out = append(out, ad.host.Validate(ctx, hostProfile(p))...)
	return out
}

// agentStages are the v0.1 stages that run a handler, in order.
var agentStages = []string{profile.StageImplement, profile.StageReview, profile.StageAddress, profile.StageMint}

// checkStages checks each stage's handler: its command, contract version
// and sandbox rule (INTEGRATION-SPEC §2, TECHNICAL-SPEC §7.2, work plan
// #23 D3–D5).
func checkStages(p *profile.Profile) []ports.Problem {
	var out []ports.Problem
	add := func(sev ports.Severity, code, msg, hint string) {
		out = append(out, ports.Problem{Severity: sev, Code: code, Message: msg, Hint: hint})
	}
	for _, name := range agentStages {
		st, ok := p.Stages[name]
		if !ok {
			continue
		}
		key := "stages." + name
		if len(st.Command) == 0 {
			add(ports.SeverityError, "handler_not_found", key+".command is empty", "set the handler's command, for example [\"agent-loop-claude\", \""+name+"\"]")
		} else if err := findCommand(st.Command[0], filepath.Dir(p.Path)); err != nil {
			add(ports.SeverityError, "handler_not_found", fmt.Sprintf("%s.command: %v", key, err), "install the handler, or fix the command's path")
		}
		if err := contract.CheckVersion(st.Contract); err != nil {
			add(ports.SeverityError, "handler_contract_unsupported", fmt.Sprintf("%s.contract: %v", key, err),
				fmt.Sprintf("use a handler written for contract %d.x, up to %s", contract.Major, contract.Version))
		}
		switch st.Runner {
		case profile.RunnerLocal:
			if len(st.Command) > 0 && !runners.Wrapped(st.Command) {
				add(ports.SeverityError, "handler_not_sandboxed", fmt.Sprintf("%s: %q isn't a handler the Claude Code sandbox wraps (%s), and runner is local", key, st.Command[0], strings.Join(runners.WrappedHandlers, ", ")),
					"use a sandboxed handler, or set runner = \"local-unsandboxed\" to opt out (TECHNICAL-SPEC §7.2)")
			}
		case profile.RunnerLocalUnsandboxed:
			add(ports.SeverityWarning, "handler_unsandboxed", fmt.Sprintf("%s runs with runner = \"local-unsandboxed\". %s", key, runners.UnsandboxedWarning),
				"use a sandboxed handler with runner = \"local\"")
		default:
			add(ports.SeverityError, "runner_unsupported", fmt.Sprintf("%s.runner = %q isn't available in this release", key, st.Runner),
				"use runner = \"local\"; the docker runner comes in v0.2")
		}
	}
	return out
}

// findCommand resolves a handler command as the runner would start it in
// the repository's tree (work plan #23 D3).
func findCommand(cmd, repo string) error {
	path := cmd
	switch {
	case filepath.IsAbs(cmd):
	case strings.ContainsRune(cmd, '/'):
		path = filepath.Join(repo, cmd)
	default:
		if _, err := exec.LookPath(cmd); err != nil {
			return fmt.Errorf("%q isn't on PATH", cmd)
		}
		return nil
	}
	fi, err := os.Stat(path)
	switch {
	case err != nil:
		return fmt.Errorf("%s doesn't exist", path)
	case fi.IsDir() || fi.Mode()&0o111 == 0:
		return fmt.Errorf("%s isn't an executable file", path)
	}
	return nil
}

// checkCapabilities compares what the profile needs with what the tracker
// supports (TECHNICAL-SPEC §5, work plan #23 D9).
func checkCapabilities(p *profile.Profile, caps ports.TrackerCaps) []ports.Problem {
	var out []ports.Problem
	if !caps.NativeDependencies {
		out = append(out, ports.Problem{Severity: ports.SeverityWarning, Code: "dependencies_unsupported",
			Message: "the tracker has no native dependencies, so no item waits for another (FR-2 AC1)",
			Hint:    "use a tracker adapter with native dependencies"})
	}
	if !caps.StatusMarkers {
		var ms []string
		for m, mm := range p.Markers {
			if mm.Status != "" {
				ms = append(ms, m)
			}
		}
		sort.Strings(ms)
		for _, m := range ms {
			out = append(out, ports.Problem{Severity: ports.SeverityError, Code: "status_markers_unsupported",
				Message: fmt.Sprintf("marker %s maps to a workflow status, which the tracker doesn't support", m),
				Hint:    fmt.Sprintf("map markers.%s to a label", m)})
		}
	}
	return out
}

func portMarkers(p *profile.Profile) map[ports.Marker]ports.MarkerMapping {
	out := map[ports.Marker]ports.MarkerMapping{}
	for m, mm := range p.Markers {
		out[ports.Marker(m)] = ports.MarkerMapping{Label: mm.Label, Status: mm.Status}
	}
	return out
}

func portOwners(p *profile.Profile) []ports.OwnerAccount {
	out := make([]ports.OwnerAccount, len(p.Identities.Owners))
	for i, o := range p.Identities.Owners {
		out[i] = ports.OwnerAccount{Login: o.Login, ID: o.ID}
	}
	return out
}

func portIdentities(p *profile.Profile) ports.ProfileIdentities {
	return ports.ProfileIdentities{Coder: p.Identities.Coder, Reviewer: p.Identities.Reviewer, Loop: p.Identities.Loop}
}

func trackerProfile(p *profile.Profile) ports.ProfileTracker {
	return ports.ProfileTracker{Adapter: p.Tracker.Adapter, Repo: p.Tracker.Repo, Markers: portMarkers(p),
		Identities: portIdentities(p), Owners: portOwners(p)}
}

func hostProfile(p *profile.Profile) ports.ProfileCodeHost {
	apps := make([]string, len(p.ReleaseApps))
	for i, a := range p.ReleaseApps {
		apps[i] = fmt.Sprint(a)
	}
	return ports.ProfileCodeHost{Adapter: p.CodeHost.Adapter, Repo: p.CodeHost.Repo, BaseBranch: p.BaseBranch,
		BranchPrefix: p.BranchPrefix, Markers: portMarkers(p), Identities: portIdentities(p), Owners: portOwners(p),
		Gates: slices.Clone(p.Gates), ManualCommentBots: slices.Clone(p.ManualCommentBots),
		MergeMethod: ports.MergeMethod(p.MergeMethod), ReleaseApps: apps}
}

// printProblems prints the report and returns the exit code (work plan #23
// D1, D2).
func printProblems(w io.Writer, name string, probs []ports.Problem) int {
	errs, warns := 0, 0
	for _, p := range probs {
		if p.Severity == ports.SeverityError {
			errs++
		} else {
			warns++
		}
		fmt.Fprintf(w, "%-7s %s: %s\n", p.Severity, p.Code, p.Message)
		if p.Hint != "" {
			fmt.Fprintf(w, "        fix: %s\n", p.Hint)
		}
	}
	fmt.Fprintf(w, "%s: %d error(s), %d warning(s)\n", name, errs, warns)
	if errs > 0 {
		return 1
	}
	return 0
}
