package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"

	"github.com/mayusuk/agent-loop/internal/ports"
)

// requiredGates returns the profile's gates, or the required checks of the
// base branch's rules and classic protection (§9.7, work plan D12).
func (h *Host) requiredGates(ctx context.Context, base string) ([]string, error) {
	if len(h.gates) > 0 {
		return h.gates, nil
	}
	return h.branchRequiredChecks(ctx, base)
}

// branchRequiredChecks returns the required checks of the base branch's
// rules and classic protection.
func (h *Host) branchRequiredChecks(ctx context.Context, base string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	type rule struct {
		Type       string `json:"type"`
		Parameters struct {
			Checks []struct {
				Context string `json:"context"`
			} `json:"required_status_checks"`
		} `json:"parameters"`
	}
	rules, err := List[rule](ctx, h.c, asLoop, h.c.RepoPath("/rules/branches/%s", url.PathEscape(base)), nil)
	if err != nil {
		return nil, err
	}
	for _, r := range rules {
		if r.Type == "required_status_checks" {
			for _, c := range r.Parameters.Checks {
				add(c.Context)
			}
		}
	}
	var classic struct {
		Contexts []string `json:"contexts"`
		Checks   []struct {
			Context string `json:"context"`
		} `json:"checks"`
	}
	err = h.c.Get(ctx, asLoop, h.c.RepoPath("/branches/%s/protection/required_status_checks", url.PathEscape(base)), nil, &classic)
	switch {
	case errors.Is(err, ports.ErrNotFound): // not protected
	case err != nil:
		return nil, err
	default:
		for _, c := range classic.Contexts {
			add(c)
		}
		for _, c := range classic.Checks {
			add(c.Context)
		}
	}
	return out, nil
}

// Gates reports each required gate's result on commit c, and only on c
// (§9.7, fake D10, work plan D12).
func (h *Host) Gates(ctx context.Context, ref ports.ChangeRef, c ports.Commit) ([]ports.GateResult, error) {
	p, err := h.pull(ctx, ref)
	if err != nil {
		return nil, err
	}
	names, err := h.requiredGates(ctx, p.Base.Ref)
	if err != nil {
		return nil, err
	}
	runs, statuses, err := h.commitChecks(ctx, string(c))
	if err != nil {
		return nil, err
	}
	byRun := map[string]ports.GateState{}
	latest := map[string]int64{}
	for _, r := range runs {
		if id, ok := latest[r.Name]; ok && id > r.ID {
			continue
		}
		latest[r.Name] = r.ID
		byRun[r.Name] = checkRunState(r.Status, r.Conclusion)
	}
	byStatus := map[string]ports.GateState{}
	for _, s := range statuses {
		byStatus[s.Context] = statusState(s.State)
	}
	out := make([]ports.GateResult, 0, len(names))
	for _, name := range names {
		st, ok := byRun[name]
		if !ok {
			if st, ok = byStatus[name]; !ok {
				st = ports.GateMissing
			}
		}
		out = append(out, ports.GateResult{Name: name, State: st, Commit: c})
	}
	return out, nil
}

// checkRunState maps a check run, case-insensitively, since GraphQL uses
// upper case (work plan D12).
func checkRunState(status, conclusion string) ports.GateState {
	if !equalFold(status, "completed") {
		return ports.GatePending
	}
	switch {
	case equalFold(conclusion, "success"), equalFold(conclusion, "neutral"), equalFold(conclusion, "skipped"):
		return ports.GatePass
	}
	return ports.GateFail
}

func statusState(state string) ports.GateState {
	switch {
	case equalFold(state, "success"):
		return ports.GatePass
	case equalFold(state, "pending"), equalFold(state, "expected"):
		return ports.GatePending
	}
	return ports.GateFail
}

type checkRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

type commitStatus struct {
	Context string `json:"context"`
	State   string `json:"state"`
}

// commitChecks returns every check run and the latest status of each
// context on sha, reading every page of both listings (#68).
func (h *Host) commitChecks(ctx context.Context, sha string) ([]checkRun, []commitStatus, error) {
	var runs []checkRun
	path := h.c.RepoPath("/commits/%s/check-runs", url.PathEscape(sha))
	err := h.c.Pages(ctx, asLoop, path, nil, func(page json.RawMessage) (bool, error) {
		var p struct {
			CheckRuns []checkRun `json:"check_runs"`
		}
		if err := decode(page, &p, path); err != nil {
			return false, err
		}
		runs = append(runs, p.CheckRuns...)
		return len(p.CheckRuns) > 0, nil
	})
	if err != nil {
		return nil, nil, err
	}
	var statuses []commitStatus
	path = h.c.RepoPath("/commits/%s/status", url.PathEscape(sha))
	err = h.c.Pages(ctx, asLoop, path, nil, func(page json.RawMessage) (bool, error) {
		var p struct {
			Statuses []commitStatus `json:"statuses"`
		}
		if err := decode(page, &p, path); err != nil {
			return false, err
		}
		statuses = append(statuses, p.Statuses...)
		return len(p.Statuses) > 0, nil
	})
	if err != nil {
		return nil, nil, err
	}
	return runs, statuses, nil
}
