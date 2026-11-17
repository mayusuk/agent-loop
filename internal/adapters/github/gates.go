package github

import (
	"context"
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
	var runs struct {
		CheckRuns []struct {
			ID         int64  `json:"id"`
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	q := url.Values{"per_page": {"100"}}
	if err := h.c.Get(ctx, asLoop, h.c.RepoPath("/commits/%s/check-runs", url.PathEscape(string(c))), q, &runs); err != nil {
		return nil, err
	}
	var combined struct {
		Statuses []struct {
			Context string `json:"context"`
			State   string `json:"state"`
		} `json:"statuses"`
	}
	if err := h.c.Get(ctx, asLoop, h.c.RepoPath("/commits/%s/status", url.PathEscape(string(c))), q, &combined); err != nil {
		return nil, err
	}
	byRun := map[string]ports.GateState{}
	latest := map[string]int64{}
	for _, r := range runs.CheckRuns {
		if id, ok := latest[r.Name]; ok && id > r.ID {
			continue
		}
		latest[r.Name] = r.ID
		byRun[r.Name] = checkRunState(r.Status, r.Conclusion)
	}
	byStatus := map[string]ports.GateState{}
	for _, s := range combined.Statuses {
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
