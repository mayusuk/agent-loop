package profile

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// minimalTables is the table part of the smallest valid profile. Tests put
// top-level keys before it and more tables after it.
const minimalTables = `
[tracker]
repo = "owner/agentMaster"

[code_host]
repo = "owner/agentMaster"

[identities]
coder    = "secret://github-app/coder"
reviewer = "secret://github-app/reviewer"
loop     = "secret://github-app/loop"
owners   = [{ login = "mayur-github-login", id = 1234567 }]

[stages.implement]
command  = ["agent-loop-claude", "implement"]
contract = "1.0"

[stages.review]
command  = ["agent-loop-claude", "review"]
contract = "1.0"

[stages.address]
command  = ["agent-loop-claude", "address"]
contract = "1.0"
`

const testFile = "/repo/.agent-loop.toml"

// load parses a profile made of top-level keys, the minimal tables, and
// extra tables.
func load(t *testing.T, top, extra string) (*Profile, error) {
	t.Helper()
	return parseProfile(testFile, []byte(top+"\n"+minimalTables+"\n"+extra), nil)
}

func mustLoad(t *testing.T, top, extra string) *Profile {
	t.Helper()
	p, err := load(t, top, extra)
	if err != nil {
		t.Fatalf("unexpected error:\n%v", err)
	}
	return p
}

// problemsOf returns the problems in err, failing the test if err is not an
// *Error.
func problemsOf(t *testing.T, err error) []Problem {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got none")
	}
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("error is %T, not *Error: %v", err, err)
	}
	return pe.Problems
}

// wantProblem fails unless err has a problem for file and key whose message
// contains every fragment.
func wantProblem(t *testing.T, err error, file, key string, fragments ...string) {
	t.Helper()
	for _, p := range problemsOf(t, err) {
		if p.File != file || p.Key != key {
			continue
		}
		ok := true
		for _, f := range fragments {
			ok = ok && strings.Contains(p.Message, f)
		}
		if ok {
			return
		}
	}
	t.Fatalf("no problem for %s key %q containing %q in:\n%v", file, key, fragments, err)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
