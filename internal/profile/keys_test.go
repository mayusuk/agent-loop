package profile

import (
	"strings"
	"testing"
)

// TestD3_FloatOwnerIDIsRejected is review R1: a whole-number float id used to
// pass the schema and load with an empty ID.
func TestD3_FloatOwnerIDIsRejected(t *testing.T) {
	for _, owners := range []string{
		`[{ login = "a", id = 1234567.0 }]`,
		`[{ login = "a", id = 1.5 }]`,
	} {
		t.Run(owners, func(t *testing.T) {
			src := strings.Replace(minimalTables, `owners   = [{ login = "mayur-github-login", id = 1234567 }]`, "owners = "+owners, 1)
			_, err := parseProfile(testFile, []byte(src), nil)
			wantProblem(t, err, testFile, "identities.owners[0].id", "whole number")
			if n := len(problemsOf(t, err)); n != 1 {
				t.Errorf("got %d problems, want 1:\n%v", n, err)
			}
		})
	}
}

// TestD17_FloatInIntegerKeyNamesTheKey is review R2: a float where a whole
// number belongs is a problem at its own key, in the file: key: message form.
func TestD17_FloatInIntegerKeyNamesTheKey(t *testing.T) {
	cases := []struct{ top, extra, key, msg string }{
		{"wip = 2.0", "", "wip", "must be a whole number"},
		{"wip = 2.5", "", "wip", "must be a whole number"},
		{"release_apps = [1.0]", "", "release_apps[0]", "must be a whole number"},
		{"", "[stages.merge]\nattempts = 2.0", "stages.merge.attempts", "must be a whole number"},
		{"", "[limits]\ndaily_agent_runs = 20.0", "limits.daily_agent_runs", "FR-16 AC3"},
	}
	for _, c := range cases {
		t.Run(c.top+c.extra, func(t *testing.T) {
			_, err := load(t, c.top, c.extra+"\n")
			wantProblem(t, err, testFile, c.key, c.msg)
			if n := len(problemsOf(t, err)); n != 1 {
				t.Errorf("got %d problems, want 1:\n%v", n, err)
			}
		})
	}
	// daily_spend is the one number that may have a fraction.
	if p := mustLoad(t, "", "[limits]\ndaily_spend = 2.0\n"); p.Limits.DailySpend != 2 {
		t.Errorf("daily_spend = %v", p.Limits.DailySpend)
	}
}

// TestD17_NumericProjectNameIsAKeyNotAnIndex is review R5.
func TestD17_NumericProjectNameIsAKeyNotAnIndex(t *testing.T) {
	path := writeDeployment(t, "[projects.\"2024\"]\n")
	_, err := LoadDeployment(path)
	wantProblem(t, err, path, "projects.2024.profile", "required")
}

// A float in an array of tables ([[identities.owners]]) is caught too.
func TestD3_FloatOwnerIDInArrayOfTables(t *testing.T) {
	src := strings.Replace(minimalTables, `owners   = [{ login = "mayur-github-login", id = 1234567 }]`, "", 1) +
		"\n[[identities.owners]]\nlogin = \"a\"\nid = 7.0\n"
	_, err := parseProfile(testFile, []byte(src), nil)
	wantProblem(t, err, testFile, "identities.owners[0].id", "whole number")
}

// An id of a type the schema already rejects is still never stored empty.
func TestD3_OwnerIDOfAnotherTypeIsAProblem(t *testing.T) {
	ps := &problems{file: testFile}
	identities(rawIdentities{Coder: "a", Reviewer: "b", Loop: "c", Owners: []rawOwner{{Login: "x", ID: 1.0}}}, ps)
	wantProblem(t, ps.err(), testFile, "identities.owners[0].id", "positive whole number or a non-empty string")
}

func TestKeyPath(t *testing.T) {
	inst := map[string]any{
		"projects": map[string]any{"2024": map[string]any{}},
		"types":    []any{"a"},
		"wip":      2,
	}
	cases := []struct {
		tokens []string
		want   string
	}{
		{[]string{"projects", "2024", "profile"}, "projects.2024.profile"},
		{[]string{"types", "0"}, "types[0]"},
		{[]string{"types", "5", "x"}, "types[5].x"},
		{[]string{"wip", "x"}, "wip.x"},
		{nil, ""},
	}
	for _, c := range cases {
		if got := keyPath(inst, c.tokens); got != c.want {
			t.Errorf("keyPath(%v) = %q, want %q", c.tokens, got, c.want)
		}
	}
}

// TestD17_FloatKeepsTheSchemaTypeError is review R6 (#36): only a key that
// wants an integer says "must be a whole number"; any other key keeps the
// schema's type error.
func TestD17_FloatKeepsTheSchemaTypeError(t *testing.T) {
	cases := []struct{ top, extra, key, msg string }{
		{`timezone = 1.5`, "", "timezone", "got number, want string"},
		{`timezone = 2.0`, "", "timezone", "got number, want string"},
		{"", "[follow_up]\nready = 1.0", "follow_up.ready", "got number, want boolean"},
		{`merge_method = 1.0`, "", "merge_method", "must be one of"},
		{`release_apps = [1.5]`, "", "release_apps[0]", "must be a whole number"},
	}
	for _, c := range cases {
		t.Run(c.top+c.extra, func(t *testing.T) {
			_, err := load(t, c.top, c.extra+"\n")
			wantProblem(t, err, testFile, c.key, c.msg)
			ps := problemsOf(t, err)
			if len(ps) != 1 {
				t.Errorf("got %d problems, want 1:\n%v", len(ps), err)
			}
			if c.msg != "must be a whole number" && strings.Contains(err.Error(), "whole number") {
				t.Errorf("non-integer key reported as whole number:\n%v", err)
			}
		})
	}
}

// TestD17_OneProblemPerBadFloat is review R7 (#36): a rejected float adds no
// problem at its parent, and none under a key that is already a problem.
func TestD17_OneProblemPerBadFloat(t *testing.T) {
	cases := []struct {
		top, extra string
		want       []Problem
	}{
		{`release_apps = [1.0, 2.0]`, "", []Problem{
			{testFile, "release_apps[0]", "must be a whole number"},
			{testFile, "release_apps[1]", "must be a whole number"},
		}},
		{`release_apps = [1.0, 1.5]`, "", []Problem{
			{testFile, "release_apps[0]", "must be a whole number"},
			{testFile, "release_apps[1]", "must be a whole number"},
		}},
		{`release_apps = [inf, inf]`, "", []Problem{
			{testFile, "release_apps[0]", "must be a finite number"},
			{testFile, "release_apps[1]", "must be a finite number"},
		}},
		{`bogus = 1.5`, "", []Problem{{testFile, "bogus", "unknown key"}}},
		{`bogus = { a = 1.0 }`, "", []Problem{{testFile, "bogus", "unknown key"}}},
		{`bogus = nan`, "", []Problem{{testFile, "bogus", "unknown key"}}},
		{"", "[follow_up]\nready = inf", []Problem{{testFile, "follow_up.ready", "must be a finite number"}}},
		{"", "[limits]\ndaily_spend = inf", []Problem{{testFile, "limits.daily_spend", "must be a finite number"}}},
	}
	for _, c := range cases {
		t.Run(c.top+c.extra, func(t *testing.T) {
			_, err := load(t, c.top, c.extra+"\n")
			got := problemsOf(t, err)
			if len(got) != len(c.want) {
				t.Fatalf("got %d problems, want %d:\n%v", len(got), len(c.want), err)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("problem %d = %v, want %v", i, got[i], c.want[i])
				}
			}
		})
	}
}

// A float in the deployment file, which has no overrides, follows the same
// rules.
func TestD17_DeploymentFloatProblems(t *testing.T) {
	path := writeDeployment(t, "bogus = 1.0\n")
	_, err := LoadDeployment(path)
	if ps := problemsOf(t, err); len(ps) != 1 || ps[0].Message != "unknown key" {
		t.Errorf("got:\n%v", err)
	}
}
