package main

import (
	"os"
	"os/exec"
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

// releaseNotes runs scripts/release-notes.sh with args and returns its exit
// code, stdout and stderr.
func releaseNotes(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	script := filepath.Join("..", "..", "scripts", "release-notes.sh")
	cmd := exec.Command(sh, append([]string{script}, args...)...)
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, out.String(), errOut.String()
}

func writeChangelog(t *testing.T, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "CHANGELOG.md")
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const sampleChangelog = `# Changelog

Intro.

## 0.2.0


Second release.

### Added

- b
  ` + "```" + `
  # a comment, not a heading
  ` + "```" + `


## 0.1.0

First release.

## 0.0.9

## 0.0.1
Last.
`

// TestIssue141_ReleaseNotesAreTheChangelogSection: a v-tag's release notes
// are the body of its CHANGELOG section, without the heading, the next
// section, or the blank lines at either end (work plan #141, D1).
func TestIssue141_ReleaseNotesAreTheChangelogSection(t *testing.T) {
	p := writeChangelog(t, sampleChangelog)
	cases := []struct{ arg, want string }{
		{"v0.2.0", "Second release.\n\n### Added\n\n- b\n  ```\n  # a comment, not a heading\n  ```\n"},
		{"0.2.0", "Second release.\n\n### Added\n\n- b\n  ```\n  # a comment, not a heading\n  ```\n"},
		{"v0.1.0", "First release.\n"},
		{"v0.0.1", "Last.\n"},
	}
	for _, tc := range cases {
		code, out, errOut := releaseNotes(t, tc.arg, p)
		if code != 0 || out != tc.want || errOut != "" {
			t.Errorf("%s: code %d, stdout %q, stderr %q, want %q", tc.arg, code, out, errOut, tc.want)
		}
	}
}

// TestIssue141_ReleaseNotesFailWithoutASection: a tag with no section, or an
// empty one, fails, so the release stops before it publishes (D1).
func TestIssue141_ReleaseNotesFailWithoutASection(t *testing.T) {
	p := writeChangelog(t, sampleChangelog)
	for _, arg := range []string{"v0.3.0", "v0.1", "v0.0.9", "v0.1.0.1"} {
		code, out, errOut := releaseNotes(t, arg, p)
		if code != 1 || out != "" || !strings.Contains(errOut, "no \"## "+strings.TrimPrefix(arg, "v")+"\" section") {
			t.Errorf("%s: code %d, stdout %q, stderr %q", arg, code, out, errOut)
		}
	}
	if code, out, errOut := releaseNotes(t, "v0.1.0", filepath.Join(t.TempDir(), "missing.md")); code != 1 || out != "" || !strings.Contains(errOut, "can't read") {
		t.Errorf("missing changelog: code %d, stdout %q, stderr %q", code, out, errOut)
	}
	for _, args := range [][]string{{}, {""}, {"v0.1.0", p, "extra"}} {
		if code, out, errOut := releaseNotes(t, args...); code != 2 || out != "" || !strings.Contains(errOut, "usage:") {
			t.Errorf("%q: code %d, stdout %q, stderr %q", args, code, out, errOut)
		}
	}
}

// TestIssue141_ChangelogHas010Section: the v0.1.0 release has notes, and
// they cover what the issue asks for.
func TestIssue141_ChangelogHas010Section(t *testing.T) {
	code, out, errOut := releaseNotes(t, "v0.1.0", filepath.Join("..", "..", "CHANGELOG.md"))
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	for _, want := range []string{
		"brew install mayusuk/tap/agent-loop",
		"agent-loop sign",
		"Contract versions: 1.0",
		"macOS (arm64, amd64) and Linux (amd64, arm64)",
		"### Known issues",
		"#138",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("0.1.0 notes don't mention %q", want)
		}
	}
}

// TestIssue141_ReleaseWorkflowUsesChangelogNotes: the release workflow builds
// the notes from the tag and passes them to GoReleaser, which stays pinned.
func TestIssue141_ReleaseWorkflowUsesChangelogNotes(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	wf := string(b)
	for _, want := range []string{
		`scripts/release-notes.sh "$GITHUB_REF_NAME" > "$RUNNER_TEMP/release-notes.md"`,
		"args: release --clean --release-notes ${{ runner.temp }}/release-notes.md",
		"version: v2.20.3",
	} {
		if !strings.Contains(wf, want) {
			t.Errorf("release.yml lacks %q", want)
		}
	}
	if strings.Index(wf, "scripts/release-notes.sh") > strings.Index(wf, "goreleaser/goreleaser-action@") {
		t.Error("release.yml runs GoReleaser before it writes the release notes")
	}
}

// goreleaserSection returns the top-level key's block of .goreleaser.yaml:
// the "key:" line and the indented or blank lines after it.
func goreleaserSection(cfg, key string) string {
	var b strings.Builder
	in := false
	for _, l := range strings.Split(cfg, "\n") {
		switch {
		case l == key+":":
			in = true
		case in && l != "" && !strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "#"):
			return b.String()
		}
		if in {
			b.WriteString(l + "\n")
		}
	}
	return b.String()
}

// TestIssue141_GoreleaserKeepsBrewsFooterAndArchiveChangelog: the brews
// formula to the tap stays (#141), the release footer links to the tag's
// CHANGELOG (D1), and the archives carry CHANGELOG.md (D3).
func TestIssue141_GoreleaserKeepsBrewsFooterAndArchiveChangelog(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".goreleaser.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(b)
	checks := []struct{ key, want string }{
		{"brews", "  - name: agent-loop\n"},
		{"brews", "    repository:\n      owner: mayusuk\n      name: homebrew-tap\n"},
		{"brews", `token: "{{ .Env.HOMEBREW_TAP_GITHUB_TOKEN }}"`},
		{"archives", "    files:\n      - CHANGELOG.md\n"},
		{"archives", "      - docs/GETTING-STARTED.md\n"},
		{"release", "  footer: |\n"},
		{"release", "(https://github.com/mayusuk/agent-loop/blob/{{ .Tag }}/CHANGELOG.md)"},
	}
	for _, c := range checks {
		sec := goreleaserSection(cfg, c.key)
		if sec == "" {
			t.Errorf(".goreleaser.yaml has no top-level %s:", c.key)
			continue
		}
		if !strings.Contains(sec, c.want) {
			t.Errorf(".goreleaser.yaml %s: lacks %q", c.key, c.want)
		}
	}
}

func TestIssue141_GoreleaserSectionStopsAtTheNextKey(t *testing.T) {
	cfg := "a:\n  x: 1\n\n  # c\nb:\n  y: 2\n"
	if got := goreleaserSection(cfg, "a"); got != "a:\n  x: 1\n\n  # c\n" {
		t.Errorf("a: %q", got)
	}
	if got := goreleaserSection(cfg, "b"); got != "b:\n  y: 2\n\n" {
		t.Errorf("b: %q", got)
	}
	if got := goreleaserSection(cfg, "c"); got != "" {
		t.Errorf("c: %q", got)
	}
}

// TestIssue141_ReleaseNotesIgnoreHeadingsInCodeFences: a "## " line inside a
// ``` or ~~~ fence neither ends a section nor starts one (D6, PR #145 R3).
func TestIssue141_ReleaseNotesIgnoreHeadingsInCodeFences(t *testing.T) {
	fence := "```"
	p := writeChangelog(t, "# Changelog\n\n"+
		fence+"md\n## 0.1.0\nnot this one\n"+fence+"\n\n"+
		"## 0.2.0\n\nShell:\n\n  "+fence+"sh\n## a shell comment\n  "+fence+"\n\n~~~\n## 0.1.0\n~~~\n\nAfter.\n\n"+
		"## 0.1.0\n\nFirst.\n")
	cases := []struct{ arg, want string }{
		{"v0.2.0", "Shell:\n\n  " + fence + "sh\n## a shell comment\n  " + fence + "\n\n~~~\n## 0.1.0\n~~~\n\nAfter.\n"},
		{"v0.1.0", "First.\n"},
	}
	for _, tc := range cases {
		code, out, errOut := releaseNotes(t, tc.arg, p)
		if code != 0 || out != tc.want || errOut != "" {
			t.Errorf("%s: code %d, stdout %q, stderr %q, want %q", tc.arg, code, out, errOut, tc.want)
		}
	}
}
