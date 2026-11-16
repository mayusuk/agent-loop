package secrets

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/mayusuk/agent-loop/internal/ports"
)

// specTable parses the INTEGRATION-SPEC §9.1 table into row name ->
// permissions, so a change to the spec without the code fails.
func specTable(t *testing.T) map[string]map[string]string {
	t.Helper()
	b, err := os.ReadFile("../../specs/INTEGRATION-SPEC.md")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	start := strings.Index(s, "### 9.1 ")
	end := strings.Index(s[start:], "\n### ")
	sec := s[start : start+end]
	paren := regexp.MustCompile(`\([^)]*\)`)
	rows := map[string]map[string]string{}
	for _, line := range strings.Split(sec, "\n") {
		cells := strings.Split(line, "|")
		if len(cells) != 5 || strings.HasPrefix(strings.TrimSpace(cells[1]), "---") || strings.TrimSpace(cells[1]) == "Token" {
			continue
		}
		perms := paren.ReplaceAllString(cells[3], "")
		perms, _, _ = strings.Cut(perms, ". ")
		m := map[string]string{}
		for _, p := range strings.Split(perms, ",") {
			k, v, ok := strings.Cut(strings.TrimSpace(p), ":")
			if !ok {
				t.Fatalf("§9.1: can't parse %q", p)
			}
			m[strings.ReplaceAll(strings.TrimSpace(k), " ", "_")] = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "."))
		}
		rows[strings.TrimSpace(cells[1])] = m
	}
	return rows
}

func equalPerms(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestFR13AC4_UsesMatchIntegrationSpec91(t *testing.T) {
	rows := specTable(t)
	coderHandler := rows["Coder, handler"]
	coderRead := map[string]string{}
	for k, v := range coderHandler {
		coderRead[k] = v
	}
	coderRead["contents"] = "read" // D5
	for u, c := range map[Use]struct {
		id    ports.Identity
		perms map[string]string
	}{
		UseCoderPush:      {ports.IdentityCoder, coderHandler},
		UseCoderRead:      {ports.IdentityCoder, coderRead},
		UseCoderWorker:    {ports.IdentityCoder, rows["Coder, worker"]},
		UseReviewerRead:   {ports.IdentityReviewer, rows["Reviewer, handler"]},
		UseReviewerWorker: {ports.IdentityReviewer, rows["Reviewer, worker"]},
		UseLoop:           {ports.IdentityLoop, rows["Loop"]},
	} {
		if len(c.perms) == 0 {
			t.Fatalf("%s: §9.1 row not found (rows: %v)", u, rows)
		}
		id, sc, err := ScopeFor(u, "owner/repo")
		if err != nil {
			t.Fatal(err)
		}
		if id != c.id || !equalPerms(sc.Permissions, c.perms) || len(sc.Repos) != 1 || sc.Repos[0] != "owner/repo" {
			t.Errorf("%s = %s %v %v, want %s %v [owner/repo]", u, id, sc.Permissions, sc.Repos, c.id, c.perms)
		}
	}
	if len(rows) != 5 {
		t.Errorf("§9.1 has %d rows, the code knows 5", len(rows))
	}
	if _, _, err := ScopeFor("nope", "o/r"); err == nil {
		t.Error("unknown use accepted")
	}
	// The returned map is a copy.
	_, sc, _ := ScopeFor(UseLoop, "o/r")
	sc.Permissions["contents"] = "read"
	if _, sc2, _ := ScopeFor(UseLoop, "o/r"); sc2.Permissions["contents"] != "write" {
		t.Error("ScopeFor shares its table")
	}
}

func TestFR13AC2_StageUsesFollowIntegrationSpec34(t *testing.T) {
	for _, st := range []string{"implement", "address", "mint"} {
		push, read, err := StageUses(st)
		if err != nil || push != UseCoderPush || read != UseCoderRead {
			t.Errorf("%s = %q %q %v", st, push, read, err)
		}
	}
	push, read, err := StageUses("review")
	if err != nil || push != "" || read != UseReviewerRead {
		t.Errorf("review = %q %q %v; want no push token", push, read, err)
	}
	for _, u := range []Use{UseReviewerRead, UseReviewerWorker} {
		_, sc, _ := ScopeFor(u, "o/r")
		if sc.Permissions["contents"] != "read" {
			t.Errorf("%s can write contents", u)
		}
	}
	if _, _, err := StageUses("merge"); err == nil {
		t.Error("merge has no handler")
	}
}
