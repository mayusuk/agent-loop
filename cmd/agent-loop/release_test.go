package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// pinnedUse matches a workflow step that uses an action pinned to a full
// commit SHA, with the version it stands for in a trailing comment.
var pinnedUse = regexp.MustCompile(`^\s*-?\s*uses:\s*[\w.-]+/[\w./-]+@[0-9a-f]{40}\s+#\s*v\d+\.\d+\.\d+\s*$`)

// unpinnedUses returns the uses: lines of a workflow that are not pinned to
// a full commit SHA with a version comment.
func unpinnedUses(workflow string) []string {
	var bad []string
	for _, l := range strings.Split(workflow, "\n") {
		t := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "-"))
		if !strings.HasPrefix(t, "uses:") {
			continue
		}
		if !pinnedUse.MatchString(l) {
			bad = append(bad, strings.TrimSpace(l))
		}
	}
	return bad
}

// TestIssue124_ReleaseWorkflowPinsActionsToCommitSHAs: the release job holds
// a token that writes the Homebrew tap, so a moved upstream tag must not
// reach it (PR #137 review R2).
func TestIssue124_ReleaseWorkflowPinsActionsToCommitSHAs(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	wf := string(b)
	if n := strings.Count(wf, "uses:"); n < 3 {
		t.Fatalf("release.yml has %d uses: steps, want at least 3 (checkout, setup-go, goreleaser)", n)
	}
	if bad := unpinnedUses(wf); len(bad) > 0 {
		t.Fatalf("actions not pinned to a commit SHA with a version comment: %q", bad)
	}
}

func TestIssue124_UnpinnedUsesRejectsTagsAndShortSHAs(t *testing.T) {
	cases := map[string]bool{
		"      - uses: actions/checkout@v4":                                                            false,
		"      - uses: goreleaser/goreleaser-action@v6.4.0":                                            false,
		"      - uses: actions/checkout@11d5960":                                                       false,
		"      - uses: actions/checkout@11d5960a326750d5838078e36cf38b85af677262":                      false, // no version comment
		"      - uses: actions/checkout@11d5960a326750d5838078e36cf38b85af677262 # v4.4.0":             true,
		"        uses: goreleaser/goreleaser-action@e435ccd777264be153ace6237001ef4d979d3a7a # v6.4.0": true,
	}
	for line, ok := range cases {
		bad := unpinnedUses(line)
		if ok && len(bad) != 0 {
			t.Errorf("%q: reported as unpinned", line)
		}
		if !ok && len(bad) != 1 {
			t.Errorf("%q: not reported as unpinned", line)
		}
	}
}
