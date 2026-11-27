package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mayusuk/agent-loop/internal/profile"
)

// repoRoot is this repository's root, from cmd/agent-loop.
const repoRoot = "../.."

// TestFR9AC2_RepositoryProfileNamesTheSpecs: this repository's own profile
// lists the specs in PRODUCT → INTEGRATION → TECHNICAL order as
// source_of_truth, mints into plans/, names a handler for every stage, and
// project validate accepts it with the fake adapters (work plan #166 D6,
// D7).
func TestFR9AC2_RepositoryProfileNamesTheSpecs(t *testing.T) {
	path := filepath.Join(repoRoot, ".agent-loop.toml")
	p, err := profile.LoadProfile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"specs/PRODUCT-SPEC.md", "specs/INTEGRATION-SPEC.md", "specs/TECHNICAL-SPEC.md"}
	if !slices.Equal(p.SourceOfTruth, want) {
		t.Errorf("source_of_truth %q, want %q", p.SourceOfTruth, want)
	}
	for _, f := range p.SourceOfTruth {
		if _, err := os.Stat(filepath.Join(repoRoot, f)); err != nil {
			t.Errorf("source_of_truth names %s: %v", f, err)
		}
	}
	if p.PlansDir != "plans/" {
		t.Errorf("plans_dir %q", p.PlansDir)
	}
	for _, st := range []string{profile.StageImplement, profile.StageReview, profile.StageAddress, profile.StageMint} {
		if got := p.Stages[st].Command; !slices.Equal(got, []string{"agent-loop-claude", st}) {
			t.Errorf("stages.%s.command %q", st, got)
		}
	}

	text, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tp := newTestProject(t, string(text))
	code, out, errOut := runCLI("project", "validate", "--config", tp.cfg, "widgets")
	if code != 0 || errOut != "" || !strings.Contains(out, "widgets: 0 error(s), 0 warning(s)") {
		t.Errorf("project validate: code %d\nstdout %s\nstderr %s", code, out, errOut)
	}
}
