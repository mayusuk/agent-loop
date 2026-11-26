package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mayusuk/agent-loop/internal/adapters/github/githubtest"
	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/profile"
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

// TestValidateRefusesReservedCacheNames: a profile cache variable the runner
// sets itself, or a secret, is an error; any other name is fine
// (TECHNICAL-SPEC §7.2, work plan #97 D5).
func TestValidateRefusesReservedCacheNames(t *testing.T) {
	tp := newTestProject(t, `caches = ["TMPDIR", "BUNDLE_PATH", "AGENT_LOOP_SECRET_X"]`+"\n"+cleanProfile)
	code, out, _ := runCLI("project", "validate", "--config", tp.cfg, "widgets")
	if code != 1 {
		t.Errorf("code %d, want 1", code)
	}
	for _, want := range []string{
		"error   cache_reserved: caches[0]: TMPDIR is set by the runner",
		"error   cache_reserved: caches[2]: AGENT_LOOP_SECRET_X is a secret",
		"widgets: 2 error(s), 0 warning(s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "caches[1]") {
		t.Errorf("BUNDLE_PATH reported:\n%s", out)
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

// TestFR1AC2_ValidateRejectsDockerRunner: v0.1 has no docker runner, so a
// stage with runner = "docker" is the error runner_unsupported (work plan
// #23 D5).
func TestFR1AC2_ValidateRejectsDockerRunner(t *testing.T) {
	prof := strings.Replace(cleanProfile, `command  = ["agent-loop-claude", "review"]`,
		`command  = ["agent-loop-claude", "review"]`+"\nrunner = \"docker\"", 1)
	tp := newTestProject(t, prof)
	code, out, _ := runCLI("project", "validate", "--config", tp.cfg, "widgets")
	if code != 1 || !strings.Contains(out, `error   runner_unsupported: stages.review.runner = "docker"`) ||
		!strings.Contains(out, "widgets: 1 error(s), 0 warning(s)") {
		t.Errorf("code %d\n%s", code, out)
	}
}

// TestFR1AC2_CheckCapabilities: a tracker without native dependencies is
// the warning dependencies_unsupported, and each marker mapped to a status
// on a tracker without status markers is the error
// status_markers_unsupported, in marker order; a tracker with both reports
// nothing (work plan #23 D9).
func TestFR1AC2_CheckCapabilities(t *testing.T) {
	p := &profile.Profile{Markers: map[string]profile.MarkerMapping{
		"ready":   {Status: "Ready"},
		"blocked": {Status: "Blocked"},
		"paused":  {Label: "paused"},
	}}
	var got []string
	for _, x := range checkCapabilities(p, ports.TrackerCaps{}) {
		if x.Message == "" || x.Hint == "" {
			t.Errorf("%s has no message or hint", x.Code)
		}
		got = append(got, string(x.Severity)+":"+x.Code+":"+x.Hint)
	}
	want := []string{
		"warning:dependencies_unsupported:use a tracker adapter with native dependencies",
		"error:status_markers_unsupported:map markers.blocked to a label",
		"error:status_markers_unsupported:map markers.ready to a label",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if ps := checkCapabilities(p, ports.TrackerCaps{NativeDependencies: true, StatusMarkers: true}); len(ps) != 0 {
		t.Errorf("full capabilities: %+v", ps)
	}
}

// githubProject is a test project wired to the real GitHub adapters
// (openGitHub) against a githubtest stub that serves the three apps, their
// bot users and installation tokens.
func githubProject(t *testing.T, prof string) (*testProject, *githubtest.Server) {
	t.Helper()
	tp := newTestProject(t, prof)
	srv := githubtest.New("acme", "widgets")
	t.Cleanup(srv.Close)
	srv.AddUser(1001, "owner", "User")
	perms := map[string]string{"issues": "write", "pull_requests": "write", "contents": "write", "checks": "read",
		"statuses": "read", "administration": "read", "metadata": "read"}
	srv.AddApp(1, "coder", 9001, perms)
	srv.AddApp(2, "reviewer", 9002, perms)
	srv.AddApp(3, "loop", 9003, perms)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	for i, id := range []string{"CODER", "REVIEWER", "LOOP"} {
		t.Setenv("AGENT_LOOP_SECRET_GITHUB_APP_"+id+"_APP_ID", strconv.Itoa(i+1))
		t.Setenv("AGENT_LOOP_SECRET_GITHUB_APP_"+id+"_PRIVATE_KEY", pemKey)
	}
	openAdapters = openGitHub
	githubAPI = srv.URL
	t.Cleanup(func() { githubAPI = "" })
	return tp, srv
}

// TestFR1AC2_ValidateUsesReadOnlyLoopToken: the only installation token the
// local commands ask for is the loop's, narrowed to read access (work plan
// #23 D8, INTEGRATION-SPEC §12.3).
func TestFR1AC2_ValidateUsesReadOnlyLoopToken(t *testing.T) {
	tp, srv := githubProject(t, cleanProfile)
	if code, out, errOut := runCLI("project", "validate", "--config", tp.cfg, "widgets"); strings.Contains(out, "identity_unreadable") ||
		strings.Contains(out, "repo_unreadable") || errOut != "" {
		t.Fatalf("code %d\n%s\n%s", code, out, errOut)
	}
	reqs := srv.TokenRequests()
	if len(reqs) != 1 || reqs[0].AppID != 3 {
		t.Fatalf("token requests %+v, want one for the loop app (3)", reqs)
	}
	if !maps.Equal(reqs[0].Permissions, readOnlyLoop) || !slices.Equal(reqs[0].Repositories, []string{"widgets"}) {
		t.Errorf("token request %+v, want %v on widgets", reqs[0], readOnlyLoop)
	}
	for k, v := range reqs[0].Permissions {
		if v != "read" {
			t.Errorf("permission %s: %s", k, v)
		}
	}
	for _, r := range srv.Requests() {
		if r.Method != "GET" && r.Path != "/installation/token" && !strings.HasSuffix(r.Path, "/access_tokens") {
			t.Errorf("validate wrote: %s %s", r.Method, r.Path)
		}
	}
}

// TestFR1AC4_ValidateReadsDefaultBranch: with base_branch unset, the code
// host uses the repository's default_branch; when that can't be read,
// validate reports repo_unreadable and stops (work plan #23 D15).
func TestFR1AC4_ValidateReadsDefaultBranch(t *testing.T) {
	t.Run("Trunk", func(t *testing.T) {
		tp, srv := githubProject(t, cleanProfile)
		srv.SetDefaultBranch("trunk")
		_, out, _ := runCLI("project", "validate", "--config", tp.cfg, "widgets")
		found := false
		for _, r := range srv.Requests() {
			found = found || r.Path == "/repos/acme/widgets/branches/trunk"
			if strings.Contains(r.Path, "/branches/main") {
				t.Errorf("read the main branch: %s", r.Path)
			}
		}
		if !found {
			t.Errorf("the host didn't check branch trunk:\n%s", out)
		}
	})
	t.Run("NotFound", func(t *testing.T) {
		tp, srv := githubProject(t, cleanProfile)
		srv.Fail("GET", `^/repos/acme/widgets$`, 404, nil, `{"message":"Not Found"}`, 0)
		code, out, _ := runCLI("project", "validate", "--config", tp.cfg, "widgets")
		if code != 1 || !strings.Contains(out, "error   repo_unreadable: can't read acme/widgets's default branch:") ||
			!strings.Contains(out, "widgets: 1 error(s), 0 warning(s)") {
			t.Errorf("code %d\n%s", code, out)
		}
	})
	// R6: a repository that answers without default_branch says so,
	// rather than "<nil>".
	t.Run("Empty", func(t *testing.T) {
		tp, srv := githubProject(t, cleanProfile)
		srv.Fail("GET", `^/repos/acme/widgets$`, 200, nil, `{}`, 0)
		code, out, _ := runCLI("project", "validate", "--config", tp.cfg, "widgets")
		if code != 1 || !strings.Contains(out, "error   repo_unreadable: can't read acme/widgets's default branch: the repository reported no default branch") ||
			strings.Contains(out, "<nil>") {
			t.Errorf("code %d\n%s", code, out)
		}
	})
}

// TestFR1AC2_ValidateOwnerLoginReadsBypassLists: the loop app's token
// can't see the rulesets' bypass lists (#101). Plain validate reports them
// as unverified rather than missing; --owner-login reads them with the
// owner's login, held in memory only, and records them in the projection,
// after which plain validate finds them right (work plan #101 D2–D4).
func TestFR1AC2_ValidateOwnerLoginReadsBypassLists(t *testing.T) {
	tp, srv := githubProject(t, cleanProfile)
	owner := srv.AddUser(1002, "owner2", "User")
	srv.SetPermission(owner, "admin")
	srv.AddRequiredRulesets("agent/", "main", 1, 3)
	oldTok := ownerToken
	t.Cleanup(func() { ownerToken = oldTok })
	asked := 0
	ownerToken = func(context.Context, *initOptions) (string, error) { asked++; return owner.Token, nil }

	_, out, _ := runCLI("project", "validate", "--config", tp.cfg, "widgets")
	if strings.Count(out, "ruleset_bypass_unverified") != 3 || strings.Contains(out, "ruleset_bypass_missing") || asked != 0 {
		t.Fatalf("plain validate before any owner read (asked %d):\n%s", asked, out)
	}
	_, out, errOut := runCLI("project", "validate", "--owner-login", "--config", tp.cfg, "widgets")
	if strings.Contains(out, "ruleset_bypass") || strings.Contains(out, "ruleset_missing") || asked != 1 {
		t.Fatalf("validate --owner-login (asked %d):\n%s\n%s", asked, out, errOut)
	}
	if strings.Contains(out+errOut, owner.Token) {
		t.Error("the owner's token was printed")
	}
	_, out, _ = runCLI("project", "validate", "--config", tp.cfg, "widgets")
	if strings.Contains(out, "ruleset_bypass") || strings.Contains(out, "ruleset_missing") || asked != 1 {
		t.Fatalf("plain validate after the owner's read:\n%s", out)
	}
	// An extra bypass actor, added after the owner's read, is unverified
	// for the loop and found by the owner's next read.
	rs := srv.Rulesets()[0]
	srv.Advance(time.Minute)
	d := &githubtest.Driver{BaseURL: srv.URL, Repo: "acme/widgets", Owner: ports.Actor{ID: "1002", Login: "owner2", Kind: ports.ActorHuman},
		Tokens: map[string]string{"1002": owner.Token}}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(d.Call(d.Owner, "PUT", "/rulesets/"+strconv.FormatInt(rs.ID, 10), map[string]any{"bypass_actors": []any{
		map[string]any{"actor_type": "Integration", "actor_id": 1}, map[string]any{"actor_type": "Integration", "actor_id": 77}}}, nil))
	if _, out, _ = runCLI("project", "validate", "--config", tp.cfg, "widgets"); strings.Count(out, "ruleset_bypass_unverified") != 1 {
		t.Errorf("plain validate after the change:\n%s", out)
	}
	if _, out, _ = runCLI("project", "validate", "--owner-login", "--config", tp.cfg, "widgets"); !strings.Contains(out, "lets Integration 77 bypass it") {
		t.Errorf("validate --owner-login after the change:\n%s", out)
	}
}

// TestFR1AC2_ValidateOwnerLoginFailsWithoutLogin: when the owner's login
// can't be had, project validate --owner-login exits 1 with the reason and
// checks nothing (work plan #101 D2, D3).
func TestFR1AC2_ValidateOwnerLoginFailsWithoutLogin(t *testing.T) {
	tp, srv := githubProject(t, cleanProfile)
	srv.AddRequiredRulesets("agent/", "main", 1, 3)
	oldTok := ownerToken
	t.Cleanup(func() { ownerToken = oldTok })
	ownerToken = func(context.Context, *initOptions) (string, error) {
		return "", errors.New("gh auth token: not logged in")
	}
	code, out, errOut := runCLI("project", "validate", "--owner-login", "--config", tp.cfg, "widgets")
	if code != 1 || !strings.Contains(errOut, "getting your GitHub login: gh auth token: not logged in") || out != "" {
		t.Errorf("code %d\nstdout %s\nstderr %s", code, out, errOut)
	}
	if n := len(srv.TokenRequests()); n != 0 {
		t.Errorf("%d token requests, want none", n)
	}
}

// TestFR1AC2_ValidateOAuthClientIDNeedsOwnerLogin: --oauth-client-id
// without --owner-login is a usage error, not silently ignored (work plan
// #101 D3).
func TestFR1AC2_ValidateOAuthClientIDNeedsOwnerLogin(t *testing.T) {
	tp, _ := githubProject(t, cleanProfile)
	oldTok := ownerToken
	t.Cleanup(func() { ownerToken = oldTok })
	ownerToken = func(context.Context, *initOptions) (string, error) {
		t.Error("asked for the owner's login")
		return "", nil
	}
	code, out, errOut := runCLI("project", "validate", "--oauth-client-id", "Iv1.abc", "--config", tp.cfg, "widgets")
	if code != 2 || !strings.Contains(errOut, "--oauth-client-id needs --owner-login") || out != "" {
		t.Errorf("code %d\nstdout %s\nstderr %s", code, out, errOut)
	}
}

// TestFR1AC2_ValidateReportsProjectionUnavailable: when the projection
// database, which holds the owner's last read of the rulesets, can't be
// opened, validate reports projection_unavailable with a fix naming
// projection.path, and exits 1 (work plan #101 D4).
func TestFR1AC2_ValidateReportsProjectionUnavailable(t *testing.T) {
	tp, _ := githubProject(t, cleanProfile)
	file := filepath.Join(tp.dir, "not-a-dir")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dep, err := os.ReadFile(tp.cfg)
	if err != nil {
		t.Fatal(err)
	}
	old := "path = \"" + filepath.Join(tp.dir, "projection.db") + "\""
	if !strings.Contains(string(dep), old) {
		t.Fatalf("deployment config has no projection path:\n%s", dep)
	}
	bad := strings.Replace(string(dep), old, "path = \""+filepath.Join(file, "state", "projection.db")+"\"", 1)
	if err := os.WriteFile(tp.cfg, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runCLI("project", "validate", "--config", tp.cfg, "widgets")
	if code != 1 || !strings.Contains(out, "error   projection_unavailable: can't open the projection database") ||
		!strings.Contains(out, "fix: check projection.path in the deployment config") {
		t.Errorf("code %d\n%s", code, out)
	}
}
