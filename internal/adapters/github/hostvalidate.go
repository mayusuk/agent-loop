package github

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/mayusuk/agent-loop/internal/ports"
)

// permLevel orders GitHub App permission levels.
var permLevel = map[string]int{"": 0, "none": 0, "read": 1, "write": 2, "admin": 3}

// wantPermissions are the installation permissions each app needs: the
// union of its rows in INTEGRATION-SPEC §9.1 (work plan #16 D8).
var wantPermissions = map[ports.Identity]map[string]string{
	ports.IdentityCoder:    {"contents": "write", "issues": "read", "pull_requests": "write", "metadata": "read"},
	ports.IdentityReviewer: {"contents": "read", "issues": "read", "pull_requests": "write", "metadata": "read"},
	ports.IdentityLoop: {"issues": "write", "pull_requests": "write", "contents": "write", "checks": "read",
		"statuses": "read", "administration": "read", "metadata": "read"},
}

// Validate checks the code-host part of the profile against the
// repository: the profile itself (#15 D13), the apps' permissions, the
// rulesets, the admins and the base-branch rules (#16 D8–D10).
func (h *Host) Validate(ctx context.Context, p ports.ProfileCodeHost) []ports.Problem {
	var out []ports.Problem
	add := func(sev ports.Severity, code, msg, hint string) {
		out = append(out, ports.Problem{Severity: sev, Code: code, Message: msg, Hint: hint})
	}
	if p.Adapter != "github" {
		add(ports.SeverityError, "wrong_adapter", fmt.Sprintf("the profile names code-host adapter %q, not github", p.Adapter), `set code_host.adapter = "github"`)
		return out
	}
	if !strings.EqualFold(p.Repo, h.c.Repo()) {
		add(ports.SeverityError, "wrong_repo", fmt.Sprintf("the profile names repository %q, but the adapter is for %q", p.Repo, h.c.Repo()), "use the repository the GitHub Apps are installed on")
	}
	switch p.MergeMethod {
	case "", ports.MergeMethodSquash, ports.MergeMethodMerge, ports.MergeMethodRebase:
	default:
		add(ports.SeverityError, "merge_method_unsupported", fmt.Sprintf("merge_method %q isn't squash, merge or rebase", p.MergeMethod), `set merge_method = "squash"`)
	}
	base := p.BaseBranch
	if base == "" {
		base = h.base
	}
	var branch struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	err := h.c.Get(ctx, asLoop, h.c.RepoPath("/branches/%s", url.PathEscape(base)), nil, &branch)
	switch {
	case errors.Is(err, ports.ErrNotFound):
		add(ports.SeverityError, "base_branch_missing", fmt.Sprintf("base branch %q doesn't exist in %s", base, h.c.Repo()), "set base_branch to the repository's default branch")
		return out
	case err != nil:
		add(ports.SeverityError, "branches_unreadable", fmt.Sprintf("can't read branch %q: %v", base, err), "check the loop app is installed with contents: write")
		return out
	}
	out = append(out, h.validatePermissions(ctx)...)
	out = append(out, h.validateRulesets(ctx, base)...)
	out = append(out, h.validateAdmins(ctx, p.Owners)...)
	out = append(out, h.validateBaseRules(ctx, base)...)
	out = append(out, h.validateGates(ctx, p.Gates, base, branch.Commit.SHA)...)
	return out
}

// validateGates warns about each profile gate that the repository has
// never reported: no check run or status of that name on the base tip, and
// not a required check of the base branch (FR-1 AC2, work plan #23 D11).
// A misspelt gate would otherwise hold an item in merging until the merge
// stage's time limit.
func (h *Host) validateGates(ctx context.Context, gates []string, base, tip string) []ports.Problem {
	if len(gates) == 0 {
		return nil
	}
	unreadable := func(err error) []ports.Problem {
		return []ports.Problem{{Severity: ports.SeverityError, Code: "gates_unreadable",
			Message: fmt.Sprintf("can't read the checks reported on branch %q: %v", base, err),
			Hint:    "check the loop app has checks: read and statuses: read"}}
	}
	known := map[string]bool{}
	required, err := h.branchRequiredChecks(ctx, base)
	if err != nil {
		return unreadable(err)
	}
	for _, n := range required {
		known[n] = true
	}
	if tip != "" {
		q := url.Values{"per_page": {"100"}}
		var runs struct {
			CheckRuns []struct {
				Name string `json:"name"`
			} `json:"check_runs"`
		}
		if err := h.c.Get(ctx, asLoop, h.c.RepoPath("/commits/%s/check-runs", url.PathEscape(tip)), q, &runs); err != nil {
			return unreadable(err)
		}
		for _, r := range runs.CheckRuns {
			known[r.Name] = true
		}
		var combined struct {
			Statuses []struct {
				Context string `json:"context"`
			} `json:"statuses"`
		}
		if err := h.c.Get(ctx, asLoop, h.c.RepoPath("/commits/%s/status", url.PathEscape(tip)), q, &combined); err != nil {
			return unreadable(err)
		}
		for _, s := range combined.Statuses {
			known[s.Context] = true
		}
	}
	var out []ports.Problem
	for _, g := range gates {
		if !known[g] {
			out = append(out, ports.Problem{Severity: ports.SeverityWarning, Code: "gate_unknown",
				Message: fmt.Sprintf("gate %q isn't a required check of branch %q and has no check run or status on its tip, so it may be misspelt; an item waits in merging for a gate that never reports, until the merge stage's time limit", g, base),
				Hint:    "check the name against the checks a pull request shows, or remove it from gates"})
		}
	}
	return out
}

// validatePermissions compares each app's installed permissions with §9.1
// (FR-13 AC4, NFR-4 AC1).
func (h *Host) validatePermissions(ctx context.Context) []ports.Problem {
	var out []ports.Problem
	for _, id := range []ports.Identity{ports.IdentityCoder, ports.IdentityReviewer, ports.IdentityLoop} {
		if h.perms == nil {
			out = append(out, ports.Problem{Severity: ports.SeverityError, Code: "app_permissions_unreadable",
				Message: fmt.Sprintf("can't read the %s app's installed permissions: no app credentials", id),
				Hint:    "store the app's ID and private key, as agent-loop init does"})
			continue
		}
		got, err := h.perms.InstalledPermissions(ctx, id, h.c.Repo())
		if err != nil {
			out = append(out, ports.Problem{Severity: ports.SeverityError, Code: "app_permissions_unreadable",
				Message: fmt.Sprintf("can't read the %s app's installed permissions: %v", id, err),
				Hint:    fmt.Sprintf("check the %s app is installed on %s and its key is stored", id, h.c.Repo())})
			continue
		}
		want := wantPermissions[id]
		names := map[string]bool{}
		for k := range got {
			names[k] = true
		}
		for k := range want {
			names[k] = true
		}
		keys := make([]string, 0, len(names))
		for k := range names {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			g, w := permLevel[got[k]], permLevel[want[k]]
			switch {
			case g > w:
				out = append(out, ports.Problem{Severity: ports.SeverityError, Code: "app_permission_extra",
					Message: fmt.Sprintf("the %s app has %s: %s, beyond the %s it needs", id, k, got[k], levelName(want[k])),
					Hint:    fmt.Sprintf("set %s to %s in the %s app's permissions and accept the change on the installation", k, levelName(want[k]), id)})
			case g < w:
				out = append(out, ports.Problem{Severity: ports.SeverityError, Code: "app_permission_missing",
					Message: fmt.Sprintf("the %s app has %s: %s, but needs %s", id, k, levelName(got[k]), want[k]),
					Hint:    fmt.Sprintf("grant %s: %s to the %s app and accept the change on the installation", k, want[k], id)})
			}
		}
	}
	return out
}

func levelName(l string) string {
	if l == "" {
		return "none"
	}
	return l
}

// validateRulesets checks the three required rulesets and the optional
// fourth (§9.8 steps 1 and 5, #16 D3, D9, D10).
func (h *Host) validateRulesets(ctx context.Context, base string) []ports.Problem {
	var out []ports.Problem
	add := func(sev ports.Severity, code, msg, hint string) {
		out = append(out, ports.Problem{Severity: sev, Code: code, Message: msg, Hint: hint})
	}
	ds, refused, err := h.rulesets(ctx)
	switch {
	case refused:
		add(ports.SeverityError, "rulesets_unsupported", fmt.Sprintf("GitHub refuses to list the rulesets of %s: rulesets need a public repository or a paid plan, and the loop app needs administration: read", h.c.Repo()),
			"make the repository public or move it to a plan with rulesets; agent-loop has no weaker fallback on GitHub")
		return out
	case err != nil:
		add(ports.SeverityError, "rulesets_unreadable", fmt.Sprintf("can't read the rulesets of %s: %v", h.c.Repo(), err), "check the loop app has administration: read")
		return out
	}
	fit := [3]*rulesetDetail{}
	for k := rulesetLoop; k <= rulesetTags; k++ {
		var why []string
		for i := range ds {
			if m := h.misfit(ds[i], k, base); m == "" {
				fit[k] = &ds[i]
				break
			} else if h.aimsAt(ds[i], k, base) {
				why = append(why, m)
			}
		}
		if fit[k] != nil {
			continue
		}
		msg := fmt.Sprintf("no active ruleset for the %s (%s)", k, strings.Join(h.includes(k, base), " or "))
		if len(why) > 0 {
			msg += ": " + strings.Join(why, "; ")
		}
		add(ports.SeverityError, "ruleset_missing", msg, "run agent-loop init to create or repair it (INTEGRATION-SPEC §9.8 step 1)")
	}
	if d := fit[rulesetLoop]; d != nil && !d.bypasses("Integration", h.apps.Coder) {
		add(ports.SeverityError, "ruleset_bypass_missing", fmt.Sprintf("the coder app isn't in the bypass list of ruleset %q, so it can't push to the loop's branches", d.Name),
			fmt.Sprintf("add the coder app (app ID %d) to the ruleset's bypass list", h.apps.Coder))
	}
	if d := fit[rulesetBase]; d != nil && !d.bypasses("Integration", h.apps.Loop) {
		add(ports.SeverityError, "ruleset_bypass_missing", fmt.Sprintf("the loop app isn't in the bypass list of ruleset %q, so it can't merge", d.Name),
			fmt.Sprintf("add the loop app (app ID %d) to the ruleset's bypass list", h.apps.Loop))
	}
	if !h.othersCovered(ds, fit) {
		add(ports.SeverityWarning, "ruleset_other_branches_missing", "no ruleset keeps the coder app off branches outside the loop's prefix, so it can push to them (they stay untrusted until the owner approves them)",
			"let agent-loop init create the optional ruleset for other branches")
	}
	return out
}

// validateAdmins fails on each admin who isn't an owner account (§9.8 step
// 2).
func (h *Host) validateAdmins(ctx context.Context, owners []ports.OwnerAccount) []ports.Problem {
	ids := map[string]bool{}
	for _, o := range owners {
		ids[o.ID] = true
	}
	if len(owners) == 0 {
		ids = h.owners
	}
	admins, err := h.admins(ctx)
	if err != nil {
		return []ports.Problem{{Severity: ports.SeverityError, Code: "admins_unreadable",
			Message: fmt.Sprintf("can't list the admins of %s: %v", h.c.Repo(), err), Hint: "check the loop app is installed on the repository"}}
	}
	var out []ports.Problem
	for _, a := range admins {
		if !ids[strconv.FormatInt(a.ID, 10)] {
			out = append(out, ports.Problem{Severity: ports.SeverityError, Code: "admin_not_owner",
				Message: fmt.Sprintf("%s (ID %d) is an admin of %s but not an owner account, and admins bypass the loop's rulesets", a.Login, a.ID, h.c.Repo()),
				Hint:    fmt.Sprintf("lower %s to write access, or add them to owners in the profile", a.Login)})
		}
	}
	return out
}

// baseRules are the base branch's merge rules, from its rulesets and
// classic protection together.
type baseRules struct {
	strict, threads, codeOwners bool
	approvals                   int
}

// readBaseRules reads the base branch's rules. A rule set it can't read is
// the problem it returns.
func (h *Host) readBaseRules(ctx context.Context, base string) (baseRules, *ports.Problem) {
	var br baseRules
	type rule struct {
		Type       string `json:"type"`
		Parameters struct {
			Strict     bool `json:"strict_required_status_checks_policy"`
			Approvals  int  `json:"required_approving_review_count"`
			CodeOwners bool `json:"require_code_owner_review"`
			Threads    bool `json:"required_review_thread_resolution"`
		} `json:"parameters"`
	}
	rules, err := List[rule](ctx, h.c, asLoop, h.c.RepoPath("/rules/branches/%s", url.PathEscape(base)), nil)
	if err != nil {
		return br, &ports.Problem{Severity: ports.SeverityError, Code: "branch_rules_unreadable",
			Message: fmt.Sprintf("can't read the rules of branch %q: %v", base, err), Hint: "check the loop app is installed on the repository"}
	}
	for _, r := range rules {
		switch r.Type {
		case "required_status_checks":
			br.strict = br.strict || r.Parameters.Strict
		case "pull_request":
			br.threads = br.threads || r.Parameters.Threads
			br.codeOwners = br.codeOwners || r.Parameters.CodeOwners
			br.approvals = max(br.approvals, r.Parameters.Approvals)
		}
	}
	var classic struct {
		Checks *struct {
			Strict bool `json:"strict"`
		} `json:"required_status_checks"`
		Reviews *struct {
			Approvals  int  `json:"required_approving_review_count"`
			CodeOwners bool `json:"require_code_owner_reviews"`
		} `json:"required_pull_request_reviews"`
		Conversation *struct {
			Enabled bool `json:"enabled"`
		} `json:"required_conversation_resolution"`
	}
	err = h.c.Get(ctx, asLoop, h.c.RepoPath("/branches/%s/protection", url.PathEscape(base)), nil, &classic)
	switch {
	case errors.Is(err, ports.ErrNotFound): // no classic protection
	case err != nil:
		return br, &ports.Problem{Severity: ports.SeverityError, Code: "branch_rules_unreadable",
			Message: fmt.Sprintf("can't read the protection of branch %q: %v", base, err), Hint: "check the loop app has administration: read"}
	default:
		if classic.Checks != nil {
			br.strict = br.strict || classic.Checks.Strict
		}
		if classic.Reviews != nil {
			br.approvals = max(br.approvals, classic.Reviews.Approvals)
			br.codeOwners = br.codeOwners || classic.Reviews.CodeOwners
		}
		if classic.Conversation != nil {
			br.threads = br.threads || classic.Conversation.Enabled
		}
	}
	return br, nil
}

// RequiredApprovals is how many approving reviews the base branch's rules
// and protection require (FR-4 AC7): the worker approves with the reviewer
// only when it is more than zero (work plan #22 D13).
func (h *Host) RequiredApprovals(ctx context.Context) (int, error) {
	br, p := h.readBaseRules(ctx, h.base)
	if p != nil {
		return 0, errors.New("github: " + p.Message)
	}
	return br.approvals, nil
}

// validateBaseRules reports the base-branch rules the loop can't meet, and
// a missing "up to date" rule (§9.7).
func (h *Host) validateBaseRules(ctx context.Context, base string) []ports.Problem {
	var out []ports.Problem
	add := func(sev ports.Severity, code, msg, hint string) {
		out = append(out, ports.Problem{Severity: sev, Code: code, Message: msg, Hint: hint})
	}
	br, p := h.readBaseRules(ctx, base)
	if p != nil {
		return []ports.Problem{*p}
	}
	strict, threads, codeOwners, approvals := br.strict, br.threads, br.codeOwners, br.approvals
	if threads {
		add(ports.SeverityError, "conversation_resolution_required", fmt.Sprintf("branch %q requires conversation resolution, which refuses merges the loop makes with open findings below the threshold or manual comments cleared by a ruling", base),
			"turn off \"Require conversation resolution before merging\" on the base branch (owner decision N4a)")
	}
	if !strict {
		add(ports.SeverityWarning, "up_to_date_not_required", fmt.Sprintf("branch %q doesn't require branches to be up to date before merging, so a base that moves during a merge can merge a head without the base tip", base),
			"turn on \"Require branches to be up to date before merging\" in the base branch's required status checks")
	}
	if approvals > 0 {
		add(ports.SeverityWarning, "approvals_required", fmt.Sprintf("branch %q requires %d approving review(s); whether the reviewer app's approval counts is unconfirmed (IQ4)", base, approvals),
			"turn off required approvals, or check that the reviewer app's approval satisfies them (conformance C14)")
	}
	if codeOwners {
		add(ports.SeverityWarning, "code_owner_review_required", fmt.Sprintf("branch %q requires review from code owners, which the reviewer app can't give unless it is one", base),
			"turn off \"Require review from Code Owners\", or make the reviewer app a code owner")
	}
	return out
}

// othersCovered reports whether a ruleset other than the loop and base
// ones keeps the coder app off every branch (§9.8 step 1, #16 D10).
func (h *Host) othersCovered(ds []rulesetDetail, fit [3]*rulesetDetail) bool {
	for i := range ds {
		d := &ds[i]
		if d == fit[rulesetLoop] || d == fit[rulesetBase] || d.Target != "branch" || d.Enforcement != "active" || !d.hasRule("update") || d.bypasses("Integration", h.apps.Coder) {
			continue
		}
		for _, p := range d.Conditions.RefName.Include {
			if p == "~ALL" || p == "refs/heads/**" {
				return true
			}
		}
	}
	return false
}
