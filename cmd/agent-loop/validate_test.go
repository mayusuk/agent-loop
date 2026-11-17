package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/runners"
)

func TestFR1AC2_ValidateCleanProjectExitsZero(t *testing.T) {
	tp := newTestProject(t, cleanProfile)
	code, out, errOut := runCLI("project", "validate", "--config", tp.cfg, "widgets")
	if code != 0 || errOut != "" {
		t.Fatalf("code %d\nstdout %s\nstderr %s", code, out, errOut)
	}
	if !strings.Contains(out, "widgets: 0 error(s), 0 warning(s)") {
		t.Errorf("stdout %s", out)
	}
}

// TestFR1AC2_ValidateNamesEachSeededProblem: validate exits non-zero and
// names every seeded problem: handler, contract, sandbox, gates, identity
// and adapter problems (FR-1 AC2, FR-13 AC1, INTEGRATION-SPEC §2,
// TECHNICAL-SPEC §7.2).
func TestFR1AC2_ValidateNamesEachSeededProblem(t *testing.T) {
	prof := strings.NewReplacer(
		`command  = ["agent-loop-claude", "implement"]`, `command  = ["./bin/no-such-handler"]`+"\nrunner = \"local-unsandboxed\"",
		`command  = ["agent-loop-claude", "review"]`+"\ncontract = \"1.0\"", `command  = ["agent-loop-claude", "review"]`+"\ncontract = \"2.0\"",
		`command  = ["agent-loop-claude", "address"]`, `command  = ["sh", "-c", "true"]`,
	).Replace("gates = []\n" + cleanProfile)
	tp := newTestProject(t, prof)
	tp.ids.Reviewer = "1001" // the owner's account
	tp.trackerProblems = []ports.Problem{{Severity: ports.SeverityError, Code: "label_missing", Message: "label x", Hint: "h"}}
	tp.hostProblems = []ports.Problem{
		{Severity: ports.SeverityError, Code: "app_permission_extra", Message: "the coder app has administration: write", Hint: "h"},
		{Severity: ports.SeverityWarning, Code: "approvals_required", Message: "approvals", Hint: "h"},
	}
	code, out, _ := runCLI("project", "validate", "--config", tp.cfg, "widgets")
	if code != 1 {
		t.Errorf("code %d, want 1", code)
	}
	for _, want := range []string{
		"error   handler_not_found: stages.implement.command: " + filepath.Join(tp.dir, "repo", "bin", "no-such-handler") + " doesn't exist",
		"warning handler_unsandboxed: stages.implement runs with runner = \"local-unsandboxed\". " + runners.UnsandboxedWarning,
		"error   handler_contract_unsupported: stages.review.contract:",
		"error   handler_not_sandboxed: stages.address: \"sh\"",
		"warning no_gates:",
		"error   identity_conflict: identities.owners: account 1001 is the reviewer identity",
		"error   label_missing: label x",
		"error   app_permission_extra: the coder app has administration: write",
		"warning approvals_required:",
		"widgets: 6 error(s), 3 warning(s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Count(out, "        fix: ") != 9 {
		t.Errorf("every problem needs a fix line:\n%s", out)
	}
}

// TestFR1AC2_ValidateReportsEveryProfileProblem: a profile that doesn't
// load names each bad key, and the other checks don't run.
func TestFR1AC2_ValidateReportsEveryProfileProblem(t *testing.T) {
	tp := newTestProject(t, "wip = 99\nnot_a_key = 1\n"+cleanProfile)
	code, out, _ := runCLI("project", "validate", "--config", tp.cfg, "widgets")
	if code != 1 || !strings.Contains(out, "error   profile: ") || !strings.Contains(out, "wip") || !strings.Contains(out, "not_a_key") {
		t.Errorf("code %d\n%s", code, out)
	}
}

// TestFR1AC2_ValidateNamesUnreadableIdentities: with no app secrets
// stored, the real GitHub wiring reports each identity and makes no call.
func TestFR1AC2_ValidateNamesUnreadableIdentities(t *testing.T) {
	tp := newTestProject(t, cleanProfile)
	openAdapters = openGitHub
	githubAPI = "http://127.0.0.1:1" // nothing listens; no call may be made
	t.Cleanup(func() { githubAPI = "" })
	for _, e := range os.Environ() {
		if k, _, _ := strings.Cut(e, "="); strings.HasPrefix(k, "AGENT_LOOP_SECRET_") {
			t.Setenv(k, "")
			os.Unsetenv(k)
		}
	}
	code, out, _ := runCLI("project", "validate", "--config", tp.cfg, "widgets")
	if code != 1 || strings.Count(out, "error   identity_unreadable:") != 3 {
		t.Errorf("code %d\n%s", code, out)
	}
}

func TestFR1AC2_ValidateUsageAndUnknownProject(t *testing.T) {
	tp := newTestProject(t, cleanProfile)
	if code, _, errOut := runCLI("project", "validate", "--config", tp.cfg, "nope"); code != 1 || !strings.Contains(errOut, "registered: widgets") {
		t.Errorf("unknown project: %d %s", code, errOut)
	}
	if code, _, _ := runCLI("project", "validate"); code != 2 {
		t.Errorf("no project: %d", code)
	}
	if code, _, errOut := runCLI("project", "validate", "--config", filepath.Join(tp.dir, "none.toml"), "widgets"); code != 1 || !strings.Contains(errOut, "no deployment config") {
		t.Errorf("no config: %d %s", code, errOut)
	}
}
