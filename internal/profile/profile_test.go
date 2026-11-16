package profile

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestTechSpec13_Defaults checks every default in the TECHNICAL-SPEC §13 key
// table and the INTEGRATION-SPEC §13 table.
func TestTechSpec13_Defaults(t *testing.T) {
	p, err := LoadProfile("testdata/minimal.agent-loop.toml", nil)
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		key       string
		got, want any
	}{
		{"tracker.adapter", p.Tracker.Adapter, "github"},
		{"code_host.adapter", p.CodeHost.Adapter, "github"},
		{"base_branch (repository default)", p.BaseBranch, ""},
		{"branch_prefix", p.BranchPrefix, "agent/"},
		{"plans_dir", p.PlansDir, "plans/"},
		{"types", p.Types, []string{"feature", "bug"}},
		{"priority", p.Priority, []string{"type:bug", "type:feature", "oldest"}},
		{"wip", p.WIP, 2},
		{"max_rounds", p.MaxRounds, 3},
		{"blocking_threshold", p.BlockingThreshold, 4},
		{"resume_rounds", p.ResumeRounds, 1},
		{"max_base_updates", p.MaxBaseUpdates, 3},
		{"merge_method", p.MergeMethod, "squash"},
		{"follow_up.type", p.FollowUp.Type, "feature"},
		{"follow_up.ready", p.FollowUp.Ready, false},
		{"limits.daily_agent_runs", p.Limits.DailyAgentRuns, 20},
		{"limits.daily_spend (none)", p.Limits.DailySpend, 0.0},
		{"limits.agent_runs", p.Limits.AgentRuns, 2},
		{"timezone (server local)", p.TimezoneName, ""},
		{"timezone location", p.Location, time.Local},
		{"intervals.supervisor", p.Intervals.Supervisor, 10 * time.Minute},
		{"intervals.poll", p.Intervals.Poll, 60 * time.Second},
		{"source_of_truth (none)", len(p.SourceOfTruth), 0},
		{"gates (base branch's required checks)", p.GatesFromBase, true},
		{"gates list", len(p.Gates), 0},
		{"manual_comment_bots", p.ManualCommentBots, []string{}},
		{"egress.allow", p.Egress.Allow, []string{}},
		{"egress.replace", p.Egress.Replace, false},
		{"release_apps", p.ReleaseApps, []int64{}},
	}
	for _, c := range checks {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s = %#v, want %#v", c.key, c.got, c.want)
		}
	}

	for _, s := range []string{StageImplement, StageReview, StageAddress} {
		st := p.Stages[s]
		if !slices.Equal(st.Capabilities, []string{"agent"}) || st.Runner != "local" || st.TimeLimit != 2*time.Hour || st.Attempts != 2 {
			t.Errorf("stages.%s defaults = %+v", s, st)
		}
	}
	if m := p.Stages[StageMerge]; m.TimeLimit != 2*time.Hour || m.Attempts != 2 {
		t.Errorf("stages.merge defaults = %+v, want 2h and 2 attempts (FR-16 AC1)", m)
	}
	if _, ok := p.Stages[StageMint]; ok {
		t.Error("stages.mint present though the profile doesn't name it")
	}

	wantEgress := append(slices.Clone(DefaultEgress), "github.com", "api.github.com", "codeload.github.com", "objects.githubusercontent.com")
	if !slices.Equal(p.Egress.Effective, wantEgress) {
		t.Errorf("effective egress = %v, want %v", p.Egress.Effective, wantEgress)
	}
}

// TestPS51_DefaultLabels checks that each marker maps to its PRODUCT-SPEC
// §5.1 default label.
func TestPS51_DefaultLabels(t *testing.T) {
	p := mustLoad(t, "", "")
	want := map[string]string{
		"type:feature": "feature", "type:bug": "bug", "ready": "ready",
		"in_progress": "in-progress", "blocked": "blocked", "needs_owner": "needs-owner",
		"paused": "paused", "aborted": "aborted", "follow_up": "follow-up",
		"plan": "plan", "plan_proposal": "plan-proposal",
	}
	if len(p.Markers) != len(want) {
		t.Errorf("got %d markers, want %d: %v", len(p.Markers), len(want), p.Markers)
	}
	for m, label := range want {
		if got := p.Markers[m]; got != (MarkerMapping{Label: label}) {
			t.Errorf("markers.%s = %+v, want label %q", m, got, label)
		}
	}
}

// TestFR1AC4_EveryLabelNameCanBeOverridden overrides every §5.1 label.
func TestFR1AC4_EveryLabelNameCanBeOverridden(t *testing.T) {
	var b strings.Builder
	b.WriteString("[markers]\n")
	want := map[string]MarkerMapping{}
	all := []string{"type:feature", "type:bug"}
	for m := range LoopMarkers {
		all = append(all, m)
	}
	for _, m := range all {
		label := "agent:" + strings.ReplaceAll(m, ":", "-")
		fmt.Fprintf(&b, "%q = { label = %q }\n", m, label)
		want[m] = MarkerMapping{Label: label}
	}
	p := mustLoad(t, "", b.String())
	if !reflect.DeepEqual(p.Markers, want) {
		t.Errorf("markers = %v, want %v", p.Markers, want)
	}
	if got := p.Policy().Markers; !reflect.DeepEqual(got, want) {
		t.Errorf("policy markers = %v, want %v", got, want)
	}
}

func TestFR1AC4_MarkerCanMapToStatus(t *testing.T) {
	p := mustLoad(t, "", "[markers]\nready = { status = \"Selected for Development\" }\n")
	if got := p.Markers["ready"]; got != (MarkerMapping{Status: "Selected for Development"}) {
		t.Errorf("markers.ready = %+v", got)
	}
	if got := p.Markers["in_progress"]; got.Label != "in-progress" {
		t.Errorf("other markers keep their defaults, got %+v", got)
	}
}

func TestFR1AC4_TypeMarkersFollowTypes(t *testing.T) {
	p := mustLoad(t, `types = ["feature", "bug", "chore"]`, "[markers]\n\"type:chore\" = { label = \"kind/chore\" }\n")
	if got := p.Markers["type:chore"]; got.Label != "kind/chore" {
		t.Errorf("type:chore = %+v", got)
	}
	_, err := load(t, "", "[markers]\n\"type:chore\" = { label = \"chore\" }\n")
	wantProblem(t, err, testFile, "markers.type:chore", "not in types")
}

func TestD4_MarkerMappings(t *testing.T) {
	cases := []struct {
		name, markers, key, msg string
	}{
		{"label and status", `ready = { label = "a", status = "b" }`, "markers.ready", "exactly one of label or status"},
		{"neither", `ready = {}`, "markers.ready", "exactly one of label or status"},
		{"unknown marker", `ready_now = { label = "x" }`, "markers.ready_now", "unknown key"},
		{"unknown field", `ready = { name = "x" }`, "markers.ready.name", "unknown key"},
		{"duplicate label", `ready = { label = "blocked" }`, "markers.ready", `maps to label "blocked", which marker blocked also uses`},
		{"empty label", `ready = { label = "" }`, "markers.ready.label", "minLength"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := load(t, "", "[markers]\n"+c.markers+"\n")
			wantProblem(t, err, testFile, c.key, c.msg)
		})
	}
}

// TestTechSpec13_RangeBoundaries checks each range end, accepted, and one
// past it, rejected.
func TestTechSpec13_RangeBoundaries(t *testing.T) {
	type tc struct {
		top, extra string
		key        string // empty when accepted
	}
	var cases []tc
	ints := []struct {
		key    string
		lo, hi int
	}{
		{"wip", 1, 20},
		{"max_rounds", 1, 10},
		{"blocking_threshold", 1, 5},
		{"resume_rounds", 1, 5},
		{"max_base_updates", 1, 10},
	}
	for _, r := range ints {
		cases = append(cases,
			tc{top: fmt.Sprintf("%s = %d", r.key, r.lo)},
			tc{top: fmt.Sprintf("%s = %d", r.key, r.hi)},
			tc{top: fmt.Sprintf("%s = %d", r.key, r.lo-1), key: r.key},
			tc{top: fmt.Sprintf("%s = %d", r.key, r.hi+1), key: r.key},
		)
	}
	cases = append(cases,
		tc{extra: "[limits]\ndaily_agent_runs = 1"},
		tc{extra: "[limits]\ndaily_agent_runs = 1000"},
		tc{extra: "[limits]\ndaily_agent_runs = 1001", key: "limits.daily_agent_runs"},
		tc{extra: "[limits]\ndaily_spend = 0.01"},
		tc{extra: "[limits]\ndaily_spend = 25"},
		tc{extra: "[limits]\ndaily_spend = 0", key: "limits.daily_spend"},
		tc{extra: "[limits]\ndaily_spend = -5.0", key: "limits.daily_spend"},
		tc{extra: "[limits]\ndaily_spend = inf", key: "limits.daily_spend"},
		tc{extra: "[limits]\nagent_runs = 1"},
		tc{extra: "[limits]\nagent_runs = 4"},
		tc{extra: "[limits]\nagent_runs = 0", key: "limits.agent_runs"},
		tc{extra: "[limits]\nagent_runs = 5", key: "limits.agent_runs"},
		tc{top: `merge_method = "squash"`},
		tc{top: `merge_method = "merge"`},
		tc{top: `merge_method = "rebase"`},
		tc{top: `merge_method = "fast-forward"`, key: "merge_method"},
		tc{extra: "[follow_up]\nready = true"},
		tc{extra: "[follow_up]\nready = \"yes\"", key: "follow_up.ready"},
		tc{extra: "[follow_up]\ntype = \"bug\""},
		tc{extra: "[follow_up]\ntype = \"chore\"", key: "follow_up.type"},
		tc{top: `timezone = "Europe/Berlin"`},
		tc{top: `timezone = "UTC"`},
		tc{top: `timezone = "Mars/Olympus"`, key: "timezone"},
		tc{top: `timezone = "Local"`, key: "timezone"},
		tc{top: `timezone = ""`, key: "timezone"},
		tc{extra: "[intervals]\nsupervisor = \"1m\"\npoll = \"15s\""},
		tc{extra: "[intervals]\nsupervisor = \"60m\"\npoll = \"10m\""},
		tc{extra: "[intervals]\nsupervisor = \"59s\"\npoll = \"15s\"", key: "intervals.supervisor"},
		tc{extra: "[intervals]\nsupervisor = \"61m\"", key: "intervals.supervisor"},
		tc{extra: "[intervals]\npoll = \"14s\"", key: "intervals.poll"},
		tc{extra: "[intervals]\npoll = \"11m\"\nsupervisor = \"60m\"", key: "intervals.poll"},
		tc{extra: "[intervals]\nsupervisor = \"5m\"\npoll = \"5m\""},
		tc{extra: "[intervals]\nsupervisor = \"5m\"\npoll = \"6m\"", key: "intervals.poll"},
		tc{extra: "[intervals]\npoll = \"soon\"", key: "intervals.poll"},
		tc{extra: "[intervals]\npoll = 60", key: "intervals.poll"},
		tc{extra: "[stages.merge]\ntime_limit = \"1m\"\nattempts = 1"},
		tc{extra: "[stages.merge]\ntime_limit = \"24h\"\nattempts = 10"},
		tc{extra: "[stages.merge]\ntime_limit = \"59s\"", key: "stages.merge.time_limit"},
		tc{extra: "[stages.merge]\ntime_limit = \"25h\"", key: "stages.merge.time_limit"},
		tc{extra: "[stages.merge]\nattempts = 0", key: "stages.merge.attempts"},
		tc{extra: "[stages.merge]\nattempts = 11", key: "stages.merge.attempts"},
		tc{extra: "[stages.merge]\ncommand = [\"x\"]", key: "stages.merge.command"},
		tc{top: `branch_prefix = "bots/"`},
		tc{top: `branch_prefix = ""`, key: "branch_prefix"},
		tc{top: `branch_prefix = "/agent"`, key: "branch_prefix"},
		tc{top: `branch_prefix = "-agent/"`, key: "branch_prefix"},
		tc{top: `branch_prefix = "a..b/"`, key: "branch_prefix"},
		tc{top: `branch_prefix = "agent bots/"`, key: "branch_prefix"},
		tc{top: `base_branch = "main"`},
		tc{top: `base_branch = "release/1.x"`},
		tc{top: `base_branch = ""`, key: "base_branch"},
		tc{top: `plans_dir = "docs/plans/"`},
		tc{top: `plans_dir = "/plans"`, key: "plans_dir"},
		tc{top: `plans_dir = "../plans"`, key: "plans_dir"},
		tc{top: `plans_dir = "~/plans"`, key: "plans_dir"},
		tc{top: `types = []`, key: "types"},
		tc{top: `types = ["Feature"]`, key: "types[0]"},
		tc{top: `types = ["feature", "feature"]`, key: "types"},
		tc{top: `source_of_truth = ["specs/PRODUCT-SPEC.md", "specs/TECHNICAL-SPEC.md"]`},
		tc{top: `source_of_truth = []`, key: "source_of_truth"},
		tc{top: `release_apps = [1, 42]`},
		tc{top: `release_apps = [0]`, key: "release_apps[0]"},
		tc{top: `release_apps = [7, 7]`, key: "release_apps"},
		tc{top: `manual_comment_bots = ["coderabbitai[bot]"]`},
		tc{top: `manual_comment_bots = [""]`, key: "manual_comment_bots[0]"},
		tc{top: `manual_comment_bots = ["a", "a"]`, key: "manual_comment_bots"},
		tc{top: `gates = ["ci / test"]`},
		tc{top: `gates = [""]`, key: "gates[0]"},
	)
	for _, c := range cases {
		name := strings.ReplaceAll(c.top+c.extra, "\n", " ")
		t.Run(name, func(t *testing.T) {
			_, err := load(t, c.top, c.extra)
			if c.key == "" {
				if err != nil {
					t.Fatalf("rejected a valid value:\n%v", err)
				}
				return
			}
			wantProblem(t, err, testFile, c.key)
		})
	}
}

func TestTechSpec13_OverridesAreKept(t *testing.T) {
	p := mustLoad(t, `
base_branch = "develop"
branch_prefix = "bots/"
plans_dir = "docs/plans/"
types = ["bug", "feature", "chore"]
priority = ["type:chore", "newest"]
wip = 5
max_rounds = 4
blocking_threshold = 3
resume_rounds = 2
max_base_updates = 6
merge_method = "rebase"
timezone = "Asia/Tokyo"
source_of_truth = ["specs/PRODUCT-SPEC.md"]
gates = ["ci"]
manual_comment_bots = ["coderabbitai[bot]"]
release_apps = [99]
`, `
[follow_up]
type = "chore"
ready = true

[limits]
daily_agent_runs = 50
daily_spend = 12.5
agent_runs = 3

[intervals]
supervisor = "30m"
poll = "2m"

[stages.mint]
command = ["my-minter"]
contract = "1.2"
capabilities = ["agent", "gpu"]
runner = "local-unsandboxed"
time_limit = "90m"
attempts = 3

[stages.merge]
time_limit = "30m"
attempts = 1
`)
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	checks := []struct {
		key       string
		got, want any
	}{
		{"base_branch", p.BaseBranch, "develop"},
		{"branch_prefix", p.BranchPrefix, "bots/"},
		{"plans_dir", p.PlansDir, "docs/plans/"},
		{"types", p.Types, []string{"bug", "feature", "chore"}},
		{"priority", p.Priority, []string{"type:chore", "newest"}},
		{"wip", p.WIP, 5},
		{"max_rounds", p.MaxRounds, 4},
		{"blocking_threshold", p.BlockingThreshold, 3},
		{"resume_rounds", p.ResumeRounds, 2},
		{"max_base_updates", p.MaxBaseUpdates, 6},
		{"merge_method", p.MergeMethod, "rebase"},
		{"follow_up", p.FollowUp, FollowUp{Type: "chore", Ready: true}},
		{"limits", p.Limits, Limits{DailyAgentRuns: 50, DailySpend: 12.5, AgentRuns: 3}},
		{"timezone", p.TimezoneName, "Asia/Tokyo"},
		{"location", p.Location.String(), tokyo.String()},
		{"intervals", p.Intervals, Intervals{Supervisor: 30 * time.Minute, Poll: 2 * time.Minute}},
		{"source_of_truth", p.SourceOfTruth, []string{"specs/PRODUCT-SPEC.md"}},
		{"gates", p.Gates, []string{"ci"}},
		{"gates from base", p.GatesFromBase, false},
		{"manual_comment_bots", p.ManualCommentBots, []string{"coderabbitai[bot]"}},
		{"release_apps", p.ReleaseApps, []int64{99}},
		{"stages.mint", p.Stages[StageMint], Stage{Command: []string{"my-minter"}, Contract: "1.2", Capabilities: []string{"agent", "gpu"}, Runner: "local-unsandboxed", TimeLimit: 90 * time.Minute, Attempts: 3}},
		{"stages.merge", p.Stages[StageMerge], Stage{TimeLimit: 30 * time.Minute, Attempts: 1}},
	}
	for _, c := range checks {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s = %#v, want %#v", c.key, c.got, c.want)
		}
	}
}

func TestFR16AC3_DailyAgentRunLimitCantBeTurnedOff(t *testing.T) {
	for _, v := range []string{`0`, `"off"`, `-1`, `false`, `"20"`} {
		t.Run(v, func(t *testing.T) {
			_, err := load(t, "", "[limits]\ndaily_agent_runs = "+v+"\n")
			wantProblem(t, err, testFile, "limits.daily_agent_runs", "always in force", "FR-16 AC3")
		})
	}
}

func TestD7_AgentRunsDefaultsToDeploymentLimitWhenLower(t *testing.T) {
	dep := DefaultDeployment()
	dep.Limits.AgentRuns = 1
	p, err := parseProfile(testFile, []byte(minimalTables), dep)
	if err != nil {
		t.Fatal(err)
	}
	if p.Limits.AgentRuns != 1 {
		t.Errorf("limits.agent_runs = %d, want 1", p.Limits.AgentRuns)
	}
	_, err = parseProfile(testFile, []byte(minimalTables+"[limits]\nagent_runs = 2\n"), dep)
	wantProblem(t, err, testFile, "limits.agent_runs", "more than the deployment limit of 1")
}

func TestD5_Priority(t *testing.T) {
	t.Run("default follows types", func(t *testing.T) {
		p := mustLoad(t, `types = ["chore", "feature"]`, "")
		want := []string{"type:feature", "type:chore", "oldest"}
		if !slices.Equal(p.Priority, want) {
			t.Errorf("priority = %v, want %v", p.Priority, want)
		}
	})
	cases := []struct{ priority, key, msg string }{
		{`["type:bug", "type:feature"]`, "priority[1]", `"oldest" or "newest"`},
		{`["bug", "oldest"]`, "priority[0]", "not a type marker"},
		{`["type:chore", "oldest"]`, "priority[0]", "not in types"},
		{`["type:bug", "type:bug", "oldest"]`, "priority[1]", "listed twice"},
		{`[]`, "priority", "minItems"},
	}
	for _, c := range cases {
		t.Run(c.priority, func(t *testing.T) {
			_, err := load(t, "priority = "+c.priority, "")
			wantProblem(t, err, testFile, c.key, c.msg)
		})
	}
	p := mustLoad(t, `priority = ["oldest"]`, "")
	if !slices.Equal(p.Priority, []string{"oldest"}) {
		t.Errorf("priority = %v", p.Priority)
	}
}

func TestD6_FollowUpTypeMustBeConfigured(t *testing.T) {
	_, err := load(t, `types = ["bug"]`, "")
	wantProblem(t, err, testFile, "follow_up.type", `default type "feature" is not in types`)
	p := mustLoad(t, `types = ["bug"]`, "[follow_up]\ntype = \"bug\"\n")
	if p.FollowUp.Type != "bug" {
		t.Errorf("follow_up.type = %q", p.FollowUp.Type)
	}
}

func TestD9_Stages(t *testing.T) {
	t.Run("docker runner adds the docker capability", func(t *testing.T) {
		p := mustLoad(t, "", "[stages.mint]\ncommand = [\"m\"]\ncontract = \"1.0\"\nrunner = \"docker\"\n")
		if got := p.Stages[StageMint].Capabilities; !slices.Equal(got, []string{"agent", "docker"}) {
			t.Errorf("capabilities = %v", got)
		}
	})
	cases := []struct{ name, extra, key, msg string }{
		{"unknown stage", "[stages.deploy]\ncommand = [\"d\"]\ncontract = \"1.0\"", "stages.deploy", "unknown key"},
		{"missing command", "[stages.mint]\ncontract = \"1.0\"", "stages.mint.command", "required"},
		{"missing contract", "[stages.mint]\ncommand = [\"m\"]", "stages.mint.contract", "required"},
		{"empty command", "[stages.mint]\ncommand = []\ncontract = \"1.0\"", "stages.mint.command", "minItems"},
		{"bad contract", "[stages.mint]\ncommand = [\"m\"]\ncontract = \"1\"", "stages.mint.contract", "contract version"},
		{"bad runner", "[stages.mint]\ncommand = [\"m\"]\ncontract = \"1.0\"\nrunner = \"vm\"", "stages.mint.runner", "one of"},
		{"bad capability", "[stages.mint]\ncommand = [\"m\"]\ncontract = \"1.0\"\ncapabilities = [\"Agent\"]", "stages.mint.capabilities[0]", "lower-case"},
		{"duplicate capability", "[stages.mint]\ncommand = [\"m\"]\ncontract = \"1.0\"\ncapabilities = [\"agent\", \"agent\"]", "stages.mint.capabilities", "equal"},
		{"bad time limit", "[stages.mint]\ncommand = [\"m\"]\ncontract = \"1.0\"\ntime_limit = \"2 hours\"", "stages.mint.time_limit", "duration"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := load(t, "", c.extra+"\n")
			wantProblem(t, err, testFile, c.key, c.msg)
		})
	}
	t.Run("implement, review and address are required", func(t *testing.T) {
		src := strings.Replace(minimalTables, "[stages.address]\ncommand  = [\"agent-loop-claude\", \"address\"]\ncontract = \"1.0\"\n", "", 1)
		_, err := parseProfile(testFile, []byte(src), nil)
		wantProblem(t, err, testFile, "stages.address", "required")
	})
}

func TestFR13AC1_IdentitiesMustBeDifferent(t *testing.T) {
	src := strings.Replace(minimalTables, `reviewer = "secret://github-app/reviewer"`, `reviewer = "secret://github-app/coder"`, 1)
	_, err := parseProfile(testFile, []byte(src), nil)
	wantProblem(t, err, testFile, "identities.reviewer", "same as identities.coder", "FR-13 AC1")
}

func TestD3_Owners(t *testing.T) {
	sub := func(owners string) string {
		return strings.Replace(minimalTables, `owners   = [{ login = "mayur-github-login", id = 1234567 }]`, "owners = "+owners, 1)
	}
	p, err := parseProfile(testFile, []byte(sub(`[{ login = "a", id = 1 }, { login = "jira-user", id = "5b10ac8d82e05b22cc7d4ef5" }]`)), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []Owner{{Login: "a", ID: "1"}, {Login: "jira-user", ID: "5b10ac8d82e05b22cc7d4ef5"}}
	if !reflect.DeepEqual(p.Identities.Owners, want) {
		t.Errorf("owners = %+v", p.Identities.Owners)
	}
	if got := p.Policy().OwnerIDs; !slices.Equal(got, []string{"1", "5b10ac8d82e05b22cc7d4ef5"}) {
		t.Errorf("policy owner IDs = %v", got)
	}
	cases := []struct{ owners, key, msg string }{
		{`[]`, "identities.owners", "minItems"},
		{`[{ login = "a", id = 1 }, { login = "b", id = 1 }]`, "identities.owners[1].id", "also the id of identities.owners[0]"},
		{`[{ login = "a" }]`, "identities.owners[0].id", "required"},
		{`[{ id = 1 }]`, "identities.owners[0].login", "required"},
		{`[{ login = "a", id = 0 }]`, "identities.owners[0].id", "minimum"},
		{`[{ login = "a", id = true }]`, "identities.owners[0].id", ""},
		{`[{ login = "a", id = 1, name = "x" }]`, "identities.owners[0].name", "unknown key"},
	}
	for _, c := range cases {
		t.Run(c.owners, func(t *testing.T) {
			_, err := parseProfile(testFile, []byte(sub(c.owners)), nil)
			wantProblem(t, err, testFile, c.key, c.msg)
		})
	}
}

func TestD10_AdapterSettings(t *testing.T) {
	cases := []struct{ from, to, key, msg string }{
		{"[tracker]\nrepo = \"owner/agentMaster\"", "[tracker]\nrepo = \"agentMaster\"", "tracker.repo", "owner/name"},
		{"[tracker]\nrepo = \"owner/agentMaster\"", "[tracker]\nadapter = \"jira\"\nrepo = \"owner/agentMaster\"", "tracker.adapter", "github"},
		{"[code_host]\nrepo = \"owner/agentMaster\"", "[code_host]\nrepository = \"owner/agentMaster\"", "code_host.repository", "unknown key"},
		{"[code_host]\nrepo = \"owner/agentMaster\"", "[code_host]", "code_host.repo", "required"},
	}
	for _, c := range cases {
		t.Run(c.to, func(t *testing.T) {
			_, err := parseProfile(testFile, []byte(strings.Replace(minimalTables, c.from, c.to, 1)), nil)
			wantProblem(t, err, testFile, c.key, c.msg)
		})
	}
}

func TestD11_GatesEmptyListMeansNoGates(t *testing.T) {
	p := mustLoad(t, "gates = []", "")
	if p.GatesFromBase || p.Gates == nil || len(p.Gates) != 0 {
		t.Errorf("gates = %#v, from base %v", p.Gates, p.GatesFromBase)
	}
}

func TestD12_Egress(t *testing.T) {
	p := mustLoad(t, "", "[egress]\nallow = [\"*.example.com\", \"github.com\"]\n")
	if !slices.Contains(p.Egress.Effective, "*.example.com") || !slices.Contains(p.Egress.Effective, "api.anthropic.com") {
		t.Errorf("effective = %v", p.Egress.Effective)
	}
	n := 0
	for _, h := range p.Egress.Effective {
		if h == "github.com" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("github.com listed %d times", n)
	}
	p = mustLoad(t, "", "[egress]\nallow = [\"example.com\"]\nreplace = true\n")
	if !slices.Equal(p.Egress.Effective, []string{"example.com"}) {
		t.Errorf("replace: effective = %v", p.Egress.Effective)
	}
	for _, bad := range []string{"Example.com", "https://example.com", "example.com/path", "*example.com", ""} {
		_, err := load(t, "", fmt.Sprintf("[egress]\nallow = [%q]\n", bad))
		wantProblem(t, err, testFile, "egress.allow[0]", "host name")
	}
}

func TestUnknownKeyNamesKeyAndFile(t *testing.T) {
	const file = "testdata/unknown-key.agent-loop.toml"
	_, err := LoadProfile(file, nil)
	wantProblem(t, err, file, "wip_limit", "unknown key")
	wantProblem(t, err, file, "stages.implement.timeout", "unknown key")
	for _, line := range []string{
		file + ": wip_limit: unknown key",
		file + ": stages.implement.timeout: unknown key",
	} {
		if !strings.Contains(err.Error(), line) {
			t.Errorf("error text lacks %q:\n%v", line, err)
		}
	}
	for _, c := range []struct{ extra, key string }{
		{"[limits]\nmax_runs = 3", "limits.max_runs"},
		{"[intervals]\nreconcile = \"1m\"", "intervals.reconcile"},
		{"[follow_up]\nlabel = \"x\"", "follow_up.label"},
		{"[egress]\ndeny = []", "egress.deny"},
		{"[identities.extra]\nx = 1", "identities.extra"},
	} {
		_, err := load(t, "", c.extra+"\n")
		wantProblem(t, err, testFile, c.key, "unknown key")
	}
}

func TestD17_EveryProblemIsReported(t *testing.T) {
	_, err := load(t, "wip = 0\nmax_rounds = 11\nbogus = 1", "[limits]\ndaily_agent_runs = \"off\"\n")
	ps := problemsOf(t, err)
	keys := map[string]bool{}
	for _, p := range ps {
		keys[p.Key] = true
	}
	for _, k := range []string{"wip", "max_rounds", "bogus", "limits.daily_agent_runs"} {
		if !keys[k] {
			t.Errorf("no problem for %s in:\n%v", k, err)
		}
	}
	// Cross-field problems are all reported too.
	_, err = load(t, `types = ["bug"]`+"\npriority = [\"type:feature\", \"first\"]", "[intervals]\nsupervisor = \"2m\"\npoll = \"3m\"\n")
	for _, k := range []string{"follow_up.type", "priority[0]", "priority[1]", "intervals.poll"} {
		wantProblem(t, err, testFile, k)
	}
}

func TestLoadProfileFileErrors(t *testing.T) {
	_, err := LoadProfile("testdata/does-not-exist.toml", nil)
	wantProblem(t, err, "testdata/does-not-exist.toml", "")
	_, err = parseProfile(testFile, []byte("wip = \n"), nil)
	wantProblem(t, err, testFile, "", "invalid TOML")
	_, err = parseProfile(testFile, []byte(minimalTables+"\nwhen = 1979-05-27\n"), nil)
	if err == nil {
		t.Fatal("a TOML date in a profile was accepted")
	}
}

func TestPolicyCopiesProfile(t *testing.T) {
	p := mustLoad(t, `timezone = "Europe/Berlin"`, "[limits]\ndaily_spend = 3\n")
	pol := p.Policy()
	if pol.WIP != 2 || pol.MaxRounds != 3 || pol.BlockingThreshold != 4 || pol.ResumeRounds != 1 ||
		pol.MaxBaseUpdates != 3 || pol.MergeMethod != "squash" || pol.FollowUpType != "feature" ||
		pol.DailyAgentRuns != 20 || pol.DailySpend != 3 || pol.AgentRuns != 2 ||
		pol.SupervisorInterval != 10*time.Minute || pol.PollInterval != time.Minute ||
		pol.BranchPrefix != "agent/" || pol.PlansDir != "plans/" || pol.Location.String() != "Europe/Berlin" {
		t.Errorf("policy = %+v", pol)
	}
	if pol.Stages[StageReview] != (StageLimits{TimeLimit: 2 * time.Hour, Attempts: 2}) || pol.Stages[StageMerge].Attempts != 2 {
		t.Errorf("stage limits = %+v", pol.Stages)
	}
	pol.Types[0] = "changed"
	pol.Markers["ready"] = MarkerMapping{Label: "changed"}
	if p.Types[0] == "changed" || p.Markers["ready"].Label == "changed" {
		t.Error("Policy shares memory with the profile")
	}
}

func TestFR13AC1_OwnerCantBeAnIdentity(t *testing.T) {
	pol := mustLoad(t, "", "").Policy() // owner ID 1234567
	ok, err := pol.WithIdentityIDs(IdentityIDs{Coder: "11", Reviewer: "12", Loop: "13"})
	if err != nil {
		t.Fatal(err)
	}
	if ok.Identities.Loop != "13" || pol.Identities.Loop != "" {
		t.Errorf("WithIdentityIDs: got %+v, original %+v", ok.Identities, pol.Identities)
	}
	cases := []struct {
		ids IdentityIDs
		msg string
	}{
		{IdentityIDs{Coder: "1234567", Reviewer: "12", Loop: "13"}, "owner account can't be one of the three identities"},
		{IdentityIDs{Coder: "11", Reviewer: "12", Loop: "1234567"}, "is the loop identity"},
		{IdentityIDs{Coder: "11", Reviewer: "11", Loop: "13"}, "three identities must be different"},
		{IdentityIDs{Coder: "11", Reviewer: "", Loop: "13"}, "identities.reviewer: account ID is empty"},
	}
	for _, c := range cases {
		if _, err := pol.WithIdentityIDs(c.ids); err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("WithIdentityIDs(%+v) = %v, want error containing %q", c.ids, err, c.msg)
		}
	}
}

func TestSchemasAreExposed(t *testing.T) {
	for name, b := range map[string][]byte{"profile": ProfileSchema(), "deployment": DeploymentSchema()} {
		if !strings.Contains(string(b), `"$schema"`) {
			t.Errorf("%s schema looks wrong", name)
		}
	}
	if got := DefaultProfilePath("/repo"); got != "/repo/.agent-loop.toml" {
		t.Errorf("DefaultProfilePath = %q", got)
	}
}
