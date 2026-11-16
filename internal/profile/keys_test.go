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
