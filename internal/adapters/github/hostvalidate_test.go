package github_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mayusuk/agent-loop/internal/adapters/github"
	"github.com/mayusuk/agent-loop/internal/adapters/github/githubtest"
	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/secrets"
)

// The secrets store reads the apps' installed permissions for Validate.
var _ github.AppPermissions = (*secrets.GitHubApps)(nil)

// cleanRepo adds what a repository needs beyond newStub's for Validate to
// find nothing: "up to date" and the optional ruleset for other branches.
func (p *stubProject) cleanRepo() {
	p.srv.RequireUpToDate(true)
	p.srv.AddRuleset(githubtest.Ruleset{Name: "other branches", Target: "branch", Include: []string{"~ALL"},
		Rules: []string{"update"}, Bypass: []githubtest.Bypass{{Type: "RepositoryRole", ID: githubtest.AdminRole}}})
}

func goodHostProfile() ports.ProfileCodeHost {
	return ports.ProfileCodeHost{Adapter: "github", Repo: "acme/widgets", BaseBranch: "main", MergeMethod: ports.MergeMethodSquash,
		Owners: []ports.OwnerAccount{{Login: "owner", ID: "1001"}}}
}

// problems lists severity:code, and fails on a problem with no message or
// hint.
func problems(t *testing.T, ps []ports.Problem) string {
	t.Helper()
	var out []string
	for _, x := range ps {
		if x.Message == "" || x.Hint == "" {
			t.Errorf("%s has no message or hint: %+v", x.Code, x)
		}
		out = append(out, string(x.Severity)+":"+x.Code)
	}
	return strings.Join(out, " ")
}

// TestFR13AC4_ValidateReportsEachProblem: each ruleset, admin, permission
// and base-branch problem, with a fix hint (§9.7, §9.8, work plan #16 D9).
func TestFR13AC4_ValidateReportsEachProblem(t *testing.T) {
	cases := []struct {
		name  string
		setup func(p *stubProject)
		want  string
	}{
		{"Clean", func(*stubProject) {}, ""},
		{"RulesetMissing", func(p *stubProject) {
			must(t, p.driver.SetRuleset(githubtest.RulesetTags, "remove"))
		}, "error:ruleset_missing"},
		{"RulesetDisabled", func(p *stubProject) {
			must(t, p.driver.SetRuleset(githubtest.RulesetLoopBranches, "disable"))
		}, "error:ruleset_missing"},
		{"RulesetWrongBypass", func(p *stubProject) {
			rs := p.srv.Rulesets()[1] // base branch
			must(t, p.driverCall("PUT", "/rulesets/"+itoa(rs.ID), map[string]any{"bypass_actors": []any{
				map[string]any{"actor_type": "Integration", "actor_id": coderApp}}}))
		}, "error:ruleset_missing"},
		{"BypassMissing", func(p *stubProject) {
			rs := p.srv.Rulesets()[0] // loop branches
			must(t, p.driverCall("PUT", "/rulesets/"+itoa(rs.ID), map[string]any{"bypass_actors": []any{
				map[string]any{"actor_type": "RepositoryRole", "actor_id": githubtest.AdminRole}}}))
		}, "error:ruleset_bypass_missing"},
		{"RulesetsUnsupported", func(p *stubProject) {
			p.srv.Fail("GET", `/rulesets$`, 403, nil, `{"message":"Upgrade to GitHub Pro or make this repository public to enable this feature."}`, 0)
		}, "error:rulesets_unsupported"},
		{"OtherBranchesRulesetMissing", func(p *stubProject) {
			rs := p.srv.Rulesets()[3]
			must(t, p.driverCall("DELETE", "/rulesets/"+itoa(rs.ID), nil))
		}, "warning:ruleset_other_branches_missing"},
		{"AdminNotOwner", func(p *stubProject) {
			must(t, p.driver.AddAdmin(actorOf(p.human)))
		}, "error:admin_not_owner"},
		{"ConversationResolution", func(p *stubProject) {
			p.srv.RequireConversationResolution(true)
		}, "error:conversation_resolution_required"},
		{"ConversationResolutionRule", func(p *stubProject) {
			p.srv.SetPullRequestRule(map[string]any{"required_review_thread_resolution": true, "required_approving_review_count": 0})
		}, "error:conversation_resolution_required"},
		{"NotUpToDate", func(p *stubProject) {
			p.srv.RequireUpToDate(false)
		}, "warning:up_to_date_not_required"},
		{"ApprovalsAndCodeOwners", func(p *stubProject) {
			must(t, p.driver.RequireApprovals(1))
			p.srv.RequireCodeOwnerReviews(true)
		}, "warning:approvals_required warning:code_owner_review_required"},
		{"MissingPermission", func(p *stubProject) {
			p.perms[ports.IdentityLoop]["administration"] = ""
		}, "error:app_permission_missing"},
		{"PermissionsUnreadable", func(p *stubProject) {
			delete(p.perms, ports.IdentityReviewer)
		}, "error:app_permissions_unreadable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newStub(t, 100)
			p.cleanRepo()
			c.setup(p)
			if got := problems(t, p.host.Validate(ctx, goodHostProfile())); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestNFR4AC1_ValidateReportsExtraPermissions: a permission beyond §9.1,
// or a higher level than it gives, is reported for each app; metadata:
// read, which every app has, isn't (work plan #16 D8).
func TestNFR4AC1_ValidateReportsExtraPermissions(t *testing.T) {
	p := newStub(t, 100)
	p.cleanRepo()
	p.perms[ports.IdentityCoder]["workflows"] = "write"
	p.perms[ports.IdentityReviewer]["contents"] = "write"
	p.perms[ports.IdentityLoop]["administration"] = "write"
	ps := p.host.Validate(ctx, goodHostProfile())
	if got := problems(t, ps); got != "error:app_permission_extra error:app_permission_extra error:app_permission_extra" {
		t.Fatalf("got %q", got)
	}
	for i, want := range []string{"coder app has workflows: write", "reviewer app has contents: write", "loop app has administration: write"} {
		if !strings.Contains(ps[i].Message, want) {
			t.Errorf("problem %d: %q doesn't say %q", i, ps[i].Message, want)
		}
	}
}

// FR-4 AC7, work plan #22 D13: the worker reads the base branch's required
// approvals to decide whether the reviewer approves.
func TestFR4AC7_RequiredApprovals(t *testing.T) {
	p := newStub(t, 100)
	if n, err := p.host.RequiredApprovals(ctx); err != nil || n != 0 {
		t.Fatalf("no approvals required: %d, %v", n, err)
	}
	must(t, p.driver.RequireApprovals(2))
	if n, err := p.host.RequiredApprovals(ctx); err != nil || n != 2 {
		t.Fatalf("two required: %d, %v", n, err)
	}
	p.srv.Fail("GET", `/rules/branches/`, 500, nil, `{"message":"boom"}`, 0)
	if _, err := p.host.RequiredApprovals(ctx); err == nil {
		t.Fatal("unreadable rules gave no error")
	}
}

// TestFR1AC2_ValidateReadsEveryPageOfChecks: a gate whose only check run,
// or only status, is past the first page of 100 on the base tip is known,
// not gate_unknown (#68).
func TestFR1AC2_ValidateReadsEveryPageOfChecks(t *testing.T) {
	p := newStub(t, 100)
	p.cleanRepo()
	tip := p.srv.Branch("main")
	// Check runs list newest first, so the oldest is on the last page.
	p.srv.AddCheckRun(tip, "build", "completed", "success")
	for i := range 120 {
		p.srv.AddCheckRun(tip, fmt.Sprintf("noise-%d", i), "completed", "success")
	}
	// Statuses list oldest first, so the newest is on the last page.
	for i := range 120 {
		must(t, p.driverCall("POST", "/statuses/"+tip, map[string]any{"state": "success", "context": fmt.Sprintf("noise/%d", i)}))
	}
	must(t, p.driverCall("POST", "/statuses/"+tip, map[string]any{"state": "success", "context": "ci/status"}))

	prof := goodHostProfile()
	prof.Gates = []string{"build", "ci/status", "biuld"}
	ps := p.host.Validate(ctx, prof)
	if got := problems(t, ps); got != "warning:gate_unknown" {
		t.Fatalf("got %q", got)
	}
	if !strings.Contains(ps[0].Message, `gate "biuld"`) {
		t.Errorf("message %q doesn't name the gate", ps[0].Message)
	}

	p.srv.Fail("GET", `/commits/[^/]+/status$`, 500, nil, `{"message":"boom"}`, 0)
	if got := problems(t, p.host.Validate(ctx, prof)); got != "error:gates_unreadable" {
		t.Errorf("unreadable statuses: got %q", got)
	}
}

// TestFR1AC2_ValidateWarnsAboutUnknownGates: a profile gate that is
// neither a required check of the base branch nor a check run or status on
// its tip is the warning gate_unknown; known gates pass, and no gates means
// no check (work plan #23 D11).
func TestFR1AC2_ValidateWarnsAboutUnknownGates(t *testing.T) {
	p := newStub(t, 100)
	p.cleanRepo()
	tip := p.srv.Branch("main")
	p.srv.RequireChecks("required-ci")
	p.srv.AddCheckRun(tip, "build", "completed", "success")
	must(t, p.driverCall("POST", "/statuses/"+tip, map[string]any{"state": "success", "context": "ci/status"}))

	prof := goodHostProfile()
	prof.Gates = []string{"required-ci", "build", "ci/status", "biuld"}
	ps := p.host.Validate(ctx, prof)
	if got := problems(t, ps); got != "warning:gate_unknown" {
		t.Fatalf("got %q", got)
	}
	if !strings.Contains(ps[0].Message, `gate "biuld"`) {
		t.Errorf("message %q doesn't name the gate", ps[0].Message)
	}

	prof.Gates = nil
	if got := problems(t, p.host.Validate(ctx, prof)); got != "" {
		t.Errorf("no gates: got %q", got)
	}

	p.srv.Fail("GET", `/commits/[^/]+/check-runs$`, 500, nil, `{"message":"boom"}`, 0)
	prof.Gates = []string{"build"}
	if got := problems(t, p.host.Validate(ctx, prof)); got != "error:gates_unreadable" {
		t.Errorf("unreadable: got %q", got)
	}
}
