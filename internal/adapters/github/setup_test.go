package github_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/mayusuk/agent-loop/internal/adapters/github"
	"github.com/mayusuk/agent-loop/internal/adapters/github/githubtest"
	"github.com/mayusuk/agent-loop/internal/ports"
)

// setupStub is a stub repository with an admin owner and an owner client.
func setupStub(t *testing.T) (*githubtest.Server, *githubtest.User, *github.Client) {
	t.Helper()
	srv := githubtest.New("acme", "widgets")
	t.Cleanup(srv.Close)
	owner := srv.AddUser(1001, "owner", "User")
	srv.SetPermission(owner, "admin")
	c, err := github.NewOwnerClient(srv.URL, "acme/widgets", owner.Token)
	if err != nil {
		t.Fatal(err)
	}
	return srv, owner, c
}

func newSetup(t *testing.T, c *github.Client) *github.Setup {
	t.Helper()
	s, err := github.NewSetup(c, github.SetupConfig{BaseBranch: "main", BranchPrefix: "agent/", Apps: github.AppIDs{Coder: coderApp, Reviewer: reviewerApp, Loop: loopApp}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func apply(t *testing.T, cs []github.SetupChange) {
	t.Helper()
	for _, c := range cs {
		if err := c.Apply(context.Background()); err != nil {
			t.Fatalf("%s: %v", c.Summary, err)
		}
	}
}

func TestFR1AC5_DetectReadsOwnerAndRepository(t *testing.T) {
	srv, _, c := setupStub(t)
	srv.SetDefaultBranch("trunk")
	info, err := github.Detect(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if info.Login != "owner" || info.UserID != 1001 || !info.Admin || info.DefaultBranch != "trunk" || !info.RulesetsOK || info.OwnerType != "User" {
		t.Errorf("info %+v", info)
	}
	srv.Fail("GET", `/rulesets$`, 403, nil, `{"message":"Upgrade to GitHub Pro"}`, 0)
	if info, err = github.Detect(context.Background(), c); err != nil || info.RulesetsOK {
		t.Errorf("refused rulesets: %+v, %v", info, err)
	}
}

func TestFR1AC5_DetectReportsNonAdmin(t *testing.T) {
	srv, owner, c := setupStub(t)
	srv.SetPermission(owner, "write")
	info, err := github.Detect(context.Background(), c)
	if err != nil || info.Admin {
		t.Errorf("info %+v, %v", info, err)
	}
}

// TestFR1AC5_AppManifestHasExactPermissions: each app is created with
// exactly its §9.1 permissions, the same table validate checks.
func TestFR1AC5_AppManifestHasExactPermissions(t *testing.T) {
	for _, id := range []ports.Identity{ports.IdentityCoder, ports.IdentityReviewer, ports.IdentityLoop} {
		data, err := github.AppManifest(id, "widgets-"+string(id), "https://github.com/acme/widgets", "http://127.0.0.1:1/cb")
		if err != nil {
			t.Fatal(err)
		}
		var m struct {
			Name   string            `json:"name"`
			Public bool              `json:"public"`
			Perms  map[string]string `json:"default_permissions"`
			Hook   struct {
				Active bool `json:"active"`
			} `json:"hook_attributes"`
		}
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		if m.Name != "widgets-"+string(id) || m.Public || m.Hook.Active {
			t.Errorf("%s: %s", id, data)
		}
		want := goodPerms()[id]
		if len(m.Perms) != len(want) {
			t.Errorf("%s: permissions %v, want %v", id, m.Perms, want)
		}
		for k, v := range want {
			if m.Perms[k] != v {
				t.Errorf("%s: %s = %q, want %q", id, k, m.Perms[k], v)
			}
		}
		if _, ok := m.Perms["workflows"]; ok {
			t.Errorf("%s has workflows", id)
		}
	}
	if _, err := github.AppManifest("owner", "x", "u", "r"); err == nil {
		t.Error("manifest for an unknown identity")
	}
}

func TestFR1AC5_ConvertManifestCreatesApp(t *testing.T) {
	srv, _, _ := setupStub(t)
	data, _ := github.AppManifest(ports.IdentityLoop, "widgets-loop", "https://github.com/acme/widgets", "http://127.0.0.1:1/cb")
	code, err := srv.CreateFromManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	app, err := github.ConvertManifest(context.Background(), srv.URL, code, nil)
	if err != nil {
		t.Fatal(err)
	}
	if app.ID == 0 || app.Slug != "widgets-loop" || !strings.Contains(app.PEM, "PRIVATE KEY") {
		t.Errorf("app %+v", app)
	}
	if _, err := github.ConvertManifest(context.Background(), srv.URL, code, nil); err == nil {
		t.Error("a code converted twice")
	}
}

func TestFR1AC5_LabelsCreatesOnlyMissing(t *testing.T) {
	srv, _, c := setupStub(t)
	srv.CreateLabel("Ready", "bug")
	s := newSetup(t, c)
	cs, err := s.Labels(context.Background(), []string{"ready", "in-progress", "bug", "feature", "feature"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || cs[0].Summary != `create label "in-progress"` || cs[1].Summary != `create label "feature"` {
		t.Fatalf("changes %+v", cs)
	}
	apply(t, cs)
	if got := srv.Labels(); !slices.Equal(got, []string{"Ready", "bug", "feature", "in-progress"}) {
		t.Errorf("labels %v", got)
	}
	if cs, _ := s.Labels(context.Background(), []string{"ready", "in-progress", "bug", "feature"}); len(cs) != 0 {
		t.Errorf("second run: %+v", cs)
	}
}

// rulesetProblems checks the stub's rulesets the way project validate
// does, with the app IDs.
func rulesetProblems(t *testing.T, c *github.Client) []string {
	t.Helper()
	ids := github.Identities{Coder: ports.Actor{ID: "9001"}, Reviewer: ports.Actor{ID: "9002"}, Loop: ports.Actor{ID: "9003"}}
	h, err := github.NewHost(c, github.HostConfig{Identities: ids, BaseBranch: "main", BranchPrefix: "agent/", Owners: []string{"1001"},
		Apps: github.AppIDs{Coder: coderApp, Reviewer: reviewerApp, Loop: loopApp}, Permissions: goodPerms()})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range h.Validate(context.Background(), ports.ProfileCodeHost{Adapter: "github", Repo: "acme/widgets", BaseBranch: "main",
		BranchPrefix: "agent/", Owners: []ports.OwnerAccount{{Login: "owner", ID: "1001"}}}) {
		if strings.HasPrefix(p.Code, "ruleset") {
			out = append(out, p.Code+": "+p.Message)
		}
	}
	return out
}

// TestFR1AC5_RulesetsCreatedPassValidate: on a bare repository the step
// creates the three rulesets and the optional fourth, validate then finds
// nothing wrong with them, and a second run changes nothing.
func TestFR1AC5_RulesetsCreatedPassValidate(t *testing.T) {
	srv, _, c := setupStub(t)
	s := newSetup(t, c)
	req, other, err := s.Rulesets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(req) != 3 || len(other) != 1 {
		t.Fatalf("required %+v other %+v", req, other)
	}
	apply(t, req)
	apply(t, other)
	if ps := rulesetProblems(t, c); len(ps) != 0 {
		t.Errorf("validate: %v", ps)
	}
	req, other, err = s.Rulesets(context.Background())
	if err != nil || len(req) != 0 || len(other) != 0 {
		t.Errorf("second run: %+v %+v %v", req, other, err)
	}
	var names []string
	for _, rs := range srv.Rulesets() {
		names = append(names, rs.Name)
		if rs.Name == github.RulesetNameOther && !slices.Equal(rs.Exclude, []string{"refs/heads/agent/**", "refs/heads/main"}) {
			t.Errorf("other branches exclude %v", rs.Exclude)
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{github.RulesetNameBase, github.RulesetNameLoop, github.RulesetNameOther, github.RulesetNameTags}) {
		t.Errorf("rulesets %v", names)
	}
}

// TestFR1AC5_RulesetsRepairDrift: a loop ruleset that drifted (disabled,
// an exclude, a rule missing, an extra bypass, the coder app missing) is
// repaired in place, keeping its other rules; the loop's own base ruleset
// that lacks the loop app and holds a pull-request rule gets the app and
// loses the rule (work plan #24 D7, D18).
func TestFR1AC5_RulesetsRepairDrift(t *testing.T) {
	srv, _, c := setupStub(t)
	srv.AddRequiredRulesets("agent/", "main", coderApp, loopApp)
	srv.AddRuleset(githubtest.Ruleset{Name: "others", Target: "branch", Include: []string{"~ALL"}, Rules: []string{"update"}})
	var loopID, baseID int64
	for _, rs := range srv.Rulesets() {
		switch rs.Name {
		case githubtest.RulesetLoopBranches:
			loopID = rs.ID
		case githubtest.RulesetBaseBranch:
			baseID = rs.ID
		}
	}
	put := func(id int64, rs githubtest.Ruleset) {
		t.Helper()
		body := map[string]any{"name": rs.Name, "target": rs.Target, "enforcement": rs.Enforcement,
			"conditions": map[string]any{"ref_name": map[string]any{"include": rs.Include, "exclude": rs.Exclude}}}
		rules := []any{}
		for _, r := range rs.Rules {
			rules = append(rules, map[string]any{"type": r})
		}
		by := []any{}
		for _, b := range rs.Bypass {
			by = append(by, map[string]any{"actor_type": b.Type, "actor_id": b.ID})
		}
		body["rules"], body["bypass_actors"] = rules, by
		if err := c.Send(context.Background(), github.AsOwner, "PUT", c.RepoPath("/rulesets/%d", id), body, nil); err != nil {
			t.Fatal(err)
		}
	}
	put(loopID, githubtest.Ruleset{Name: "mine", Target: "branch", Enforcement: "disabled", Include: []string{"refs/heads/agent/**"},
		Exclude: []string{"refs/heads/agent/x"}, Rules: []string{"creation", "update", "required_signatures"},
		Bypass: []githubtest.Bypass{{Type: "Integration", ID: 777}}})
	put(baseID, githubtest.Ruleset{Name: github.RulesetNameBase, Target: "branch", Enforcement: "active", Include: []string{"refs/heads/main"},
		Rules: []string{"update", "pull_request", "deletion", "non_fast_forward"}, Bypass: []githubtest.Bypass{{Type: "RepositoryRole", ID: githubtest.AdminRole}}})
	s := newSetup(t, c)
	req, other, err := s.Rulesets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(req) != 2 || len(other) != 0 || !strings.HasPrefix(req[0].Summary, `repair ruleset "mine"`) ||
		!strings.Contains(req[1].Summary, `repair ruleset "agent-loop: base branch" for the base branch: ruleset "agent-loop: base branch" also holds the rule pull_request`) {
		t.Fatalf("required %+v other %+v", req, other)
	}
	apply(t, req)
	if ps := rulesetProblems(t, c); len(ps) != 0 {
		t.Errorf("validate: %v", ps)
	}
	for _, rs := range srv.Rulesets() {
		if rs.ID == loopID {
			if rs.Name != "mine" || rs.Enforcement != "active" || len(rs.Exclude) != 0 || !slices.Contains(rs.Rules, "required_signatures") ||
				!slices.Contains(rs.Rules, "non_fast_forward") || len(rs.Bypass) != 2 {
				t.Errorf("repaired %+v", rs)
			}
		}
		if rs.ID == baseID && (!slices.Equal(rs.Rules, []string{"update", "deletion", "non_fast_forward"}) || len(rs.Bypass) != 2) {
			t.Errorf("repaired base %+v", rs)
		}
	}
	if req, _, _ := s.Rulesets(context.Background()); len(req) != 0 {
		t.Errorf("second run: %+v", req)
	}
}

// TestFR1AC5_RulesetsLeaveOwnerBaseRulesets: init never repairs a ruleset
// of the owner's on the base branch, so the loop app never bypasses the
// owner's required checks or pull-request rules there (INTEGRATION-SPEC
// §9.8 step 1, work plan #24 D18). It creates its own base ruleset, and
// "up to date" still goes on the owner's checks ruleset (D6), which the
// loop app doesn't bypass.
func TestFR1AC5_RulesetsLeaveOwnerBaseRulesets(t *testing.T) {
	ciRuleset := githubtest.Ruleset{Name: "ci", Target: "branch", Include: []string{"~DEFAULT_BRANCH"}, Rules: []string{"required_status_checks", "pull_request"},
		Params: map[string]map[string]any{"required_status_checks": {"strict_required_status_checks_policy": false,
			"required_status_checks": []any{map[string]any{"context": "test"}}}}}
	cases := []struct {
		name  string
		owner githubtest.Ruleset
	}{
		{"CIRuleset", ciRuleset},
		{"CIRulesetWithPushRules", func() githubtest.Ruleset {
			rs := ciRuleset
			rs.Rules = []string{"required_status_checks", "pull_request", "update", "deletion", "non_fast_forward"}
			return rs
		}()},
		{"CIRulesetBypassedByLoopApp", func() githubtest.Ruleset {
			rs := ciRuleset
			rs.Rules = []string{"required_status_checks", "pull_request", "update", "deletion", "non_fast_forward"}
			rs.Bypass = []githubtest.Bypass{{Type: "Integration", ID: loopApp}}
			return rs
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, c := setupStub(t)
			ownerID := srv.AddRuleset(tc.owner)
			var before githubtest.Ruleset
			for _, rs := range srv.Rulesets() {
				if rs.ID == ownerID {
					before = rs
				}
			}
			s := newSetup(t, c)
			req, _, err := s.Rulesets(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			var summaries []string
			for _, ch := range req {
				summaries = append(summaries, ch.Summary)
				if strings.Contains(ch.Summary, `"ci"`) {
					t.Errorf("init plans to change the owner's ruleset: %s", ch.Summary)
				}
			}
			if !slices.ContainsFunc(summaries, func(s string) bool {
				return strings.HasPrefix(s, `create ruleset "agent-loop: base branch"`)
			}) {
				t.Errorf("no base ruleset created: %v", summaries)
			}
			apply(t, req)
			bc, err := s.UpToDate(context.Background())
			if err != nil || bc.Change == nil || !strings.Contains(bc.Change.Summary, `ruleset "ci"`) {
				t.Fatalf("up to date: %+v %v", bc, err)
			}
			apply(t, []github.SetupChange{*bc.Change})
			for _, rs := range srv.Rulesets() {
				switch rs.Name {
				case "ci":
					if !slices.Equal(rs.Rules, before.Rules) || !slices.EqualFunc(rs.Bypass, before.Bypass, func(a, b githubtest.Bypass) bool { return a == b }) {
						t.Errorf("owner ruleset changed: %+v, was %+v", rs, before)
					}
					if rs.Params["required_status_checks"]["strict_required_status_checks_policy"] != true {
						t.Errorf("up to date not set on the checks ruleset: %v", rs.Params)
					}
				case github.RulesetNameBase:
					if !slices.Equal(rs.Rules, []string{"update", "deletion", "non_fast_forward"}) {
						t.Errorf("loop base ruleset %+v", rs)
					}
				}
			}
			if req, _, _ := s.Rulesets(context.Background()); len(req) != 0 {
				t.Errorf("second run: %+v", req)
			}
		})
	}
}

// TestFR1AC5_RulesetsOwnerPushRulesetNotRepaired: an owner's ruleset that
// fits the base ruleset's shape but doesn't let the loop app bypass it is
// left alone too; init creates its own (work plan #24 D18).
func TestFR1AC5_RulesetsOwnerPushRulesetNotRepaired(t *testing.T) {
	srv, _, c := setupStub(t)
	id := srv.AddRuleset(githubtest.Ruleset{Name: "protect main", Target: "branch", Include: []string{"refs/heads/main"},
		Rules: []string{"update", "deletion", "non_fast_forward"}, Bypass: []githubtest.Bypass{{Type: "RepositoryRole", ID: githubtest.AdminRole}}})
	req, _, err := newSetup(t, c).Rulesets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(req, func(ch github.SetupChange) bool {
		return strings.HasPrefix(ch.Summary, `create ruleset "agent-loop: base branch"`)
	}) || slices.ContainsFunc(req, func(ch github.SetupChange) bool { return strings.Contains(ch.Summary, `"protect main"`) }) {
		t.Fatalf("required %+v", req)
	}
	apply(t, req)
	for _, rs := range srv.Rulesets() {
		if rs.ID == id && len(rs.Bypass) != 1 {
			t.Errorf("owner ruleset changed: %+v", rs)
		}
	}
}

func TestFR1AC5_RulesetsNeedAppIDs(t *testing.T) {
	_, _, c := setupStub(t)
	s, _ := github.NewSetup(c, github.SetupConfig{BaseBranch: "main"})
	if _, _, err := s.Rulesets(context.Background()); err == nil {
		t.Error("rulesets planned without app IDs")
	}
}

// TestFR1AC5_UpToDateWhereChecksAreDefined: the setting goes on the rule
// that requires the checks (work plan #24 D6).
func TestFR1AC5_UpToDateWhereChecksAreDefined(t *testing.T) {
	t.Run("Ruleset", func(t *testing.T) {
		srv, _, c := setupStub(t)
		id := srv.AddRuleset(githubtest.Ruleset{Name: "ci", Target: "branch", Include: []string{"~DEFAULT_BRANCH"}, Rules: []string{"required_status_checks"},
			Params: map[string]map[string]any{"required_status_checks": {"strict_required_status_checks_policy": false,
				"required_status_checks": []any{map[string]any{"context": "test"}}}}})
		s := newSetup(t, c)
		bc, err := s.UpToDate(context.Background())
		if err != nil || bc.Change == nil || !slices.Equal(bc.Checks, []string{"test"}) || bc.UpToDate {
			t.Fatalf("%+v %v", bc, err)
		}
		apply(t, []github.SetupChange{*bc.Change})
		for _, rs := range srv.Rulesets() {
			if rs.ID == id && rs.Params["required_status_checks"]["strict_required_status_checks_policy"] != true {
				t.Errorf("params %v", rs.Params)
			}
		}
		if bc, err = s.UpToDate(context.Background()); err != nil || bc.Change != nil || !bc.UpToDate {
			t.Errorf("second run: %+v %v", bc, err)
		}
	})
	t.Run("Classic", func(t *testing.T) {
		srv, _, c := setupStub(t)
		srv.RequireClassicChecks("build")
		s := newSetup(t, c)
		bc, err := s.UpToDate(context.Background())
		if err != nil || bc.Change == nil || !strings.Contains(bc.Change.Summary, "classic protection") {
			t.Fatalf("%+v %v", bc, err)
		}
		apply(t, []github.SetupChange{*bc.Change})
		if !srv.UpToDate() {
			t.Error("classic strict not set")
		}
	})
	t.Run("NoChecks", func(t *testing.T) {
		_, _, c := setupStub(t)
		bc, err := newSetup(t, c).UpToDate(context.Background())
		if err != nil || bc.Change != nil || len(bc.Checks) != 0 || bc.Note != "" {
			t.Errorf("%+v %v", bc, err)
		}
	})
	t.Run("OrganisationRuleset", func(t *testing.T) {
		srv, _, c := setupStub(t)
		srv.RequireChecks("lint") // a ruleset the repository API can't read
		bc, err := newSetup(t, c).UpToDate(context.Background())
		if err != nil || bc.Change != nil || !strings.Contains(bc.Note, "organisation ruleset") {
			t.Errorf("%+v %v", bc, err)
		}
	})
}

func TestFR1AC5_AdminsAndBaseRules(t *testing.T) {
	srv, _, c := setupStub(t)
	other := srv.AddUser(1002, "alice", "User")
	srv.SetPermission(other, "admin")
	srv.RequireConversationResolution(true)
	s := newSetup(t, c)
	admins, err := s.Admins(context.Background())
	if err != nil || len(admins) != 2 || admins[1].Login != "alice" || admins[1].ID != "1002" {
		t.Errorf("admins %+v %v", admins, err)
	}
	var codes []string
	for _, p := range s.BaseRuleProblems(context.Background()) {
		codes = append(codes, p.Code)
	}
	if !slices.Equal(codes, []string{"conversation_resolution_required"}) {
		t.Errorf("codes %v", codes)
	}
}

// TestFR1AC5_ValidateRejectsMixedBaseRuleset: a base ruleset that the loop
// app bypasses but that also holds required checks or a pull-request rule
// doesn't count as the base ruleset, since the loop's merges would skip
// those rules (INTEGRATION-SPEC §9.8 step 1).
func TestFR1AC5_ValidateRejectsMixedBaseRuleset(t *testing.T) {
	for _, extra := range []string{"required_status_checks", "pull_request"} {
		t.Run(extra, func(t *testing.T) {
			srv, _, c := setupStub(t)
			srv.AddRequiredRulesets("agent/", "main", coderApp, loopApp)
			for _, rs := range srv.Rulesets() {
				if rs.Name == githubtest.RulesetBaseBranch {
					if err := c.Send(context.Background(), github.AsOwner, "DELETE", c.RepoPath("/rulesets/%d", rs.ID), nil, nil); err != nil {
						t.Fatal(err)
					}
				}
			}
			srv.AddRuleset(githubtest.Ruleset{Name: "main", Target: "branch", Include: []string{"refs/heads/main"},
				Rules:  []string{"update", "deletion", "non_fast_forward", extra},
				Bypass: []githubtest.Bypass{{Type: "Integration", ID: loopApp}}})
			ps := rulesetProblems(t, c)
			if !slices.ContainsFunc(ps, func(p string) bool {
				return strings.HasPrefix(p, "ruleset_missing: no active ruleset for the base branch") &&
					strings.Contains(p, `ruleset "main" also holds the rule `+extra+", which the loop app would bypass")
			}) {
				t.Errorf("validate: %v", ps)
			}
		})
	}
}
