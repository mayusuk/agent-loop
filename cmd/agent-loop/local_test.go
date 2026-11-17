package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mayusuk/agent-loop/internal/adapters/fake"
	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/profile"
)

// testProject is a project registered in a deployment config in a temp
// directory, run against the fake adapter.
type testProject struct {
	dir, cfg string
	f        *fake.Fake
	// trackerProblems and hostProblems are what the adapters' Validate
	// reports, so a test can seed adapter problems.
	trackerProblems, hostProblems []ports.Problem
	ids                           profile.IdentityIDs
}

// cleanProfile is a valid profile whose handlers are on PATH (see
// newTestProject). extra is appended.
const cleanProfile = `
[tracker]
repo = "acme/widgets"

[code_host]
repo = "acme/widgets"

[identities]
coder    = "secret://github-app/coder"
reviewer = "secret://github-app/reviewer"
loop     = "secret://github-app/loop"
owners   = [{ login = "owner", id = 1001 }]

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

// newTestProject writes the deployment config and the profile, puts an
// agent-loop-claude on PATH that records any start in its directory, and
// replaces openAdapters with the fake.
func newTestProject(t *testing.T, prof string) *testProject {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	handler := "#!/bin/sh\ntouch \"" + filepath.Join(dir, "handler-started") + "\"\n"
	if err := os.WriteFile(filepath.Join(bin, "agent-loop-claude"), []byte(handler), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	repo := filepath.Join(dir, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent-loop.toml"), []byte(prof), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "deployment.toml")
	dep := "[secrets]\nbackend = \"envfile\"\npath = \"" + filepath.Join(dir, "secrets.env") + "\"\n\n" +
		"[projects.widgets]\nprofile = \"" + filepath.Join(repo, ".agent-loop.toml") + "\"\n"
	if err := os.WriteFile(cfg, []byte(dep), 0o600); err != nil {
		t.Fatal(err)
	}
	tp := &testProject{dir: dir, cfg: cfg, f: fake.New(fake.DefaultConfig()),
		ids: profile.IdentityIDs{Coder: fake.Coder.ID, Reviewer: fake.Reviewer.ID, Loop: fake.Loop.ID}}
	old := openAdapters
	t.Cleanup(func() { openAdapters = old })
	openAdapters = func(context.Context, *localProject) (*projectAdapters, []ports.Problem, error) {
		return &projectAdapters{
			tracker: seededTracker{tp.f.Tracker(), tp.trackerProblems},
			host:    seededHost{tp.f.Host(), tp.hostProblems},
			ids:     tp.ids, close: func() {},
		}, nil, nil
	}
	return tp
}

// handlerStarted reports whether any handler was started.
func (tp *testProject) handlerStarted() bool {
	_, err := os.Stat(filepath.Join(tp.dir, "handler-started"))
	return err == nil
}

// seededTracker and seededHost replace the fake's Validate, which checks
// that the profile names the fake adapter, with seeded problems.
type seededTracker struct {
	ports.WorkTracker
	probs []ports.Problem
}

func (s seededTracker) Validate(context.Context, ports.ProfileTracker) []ports.Problem { return s.probs }

type seededHost struct {
	ports.CodeHost
	probs []ports.Problem
}

func (s seededHost) Validate(context.Context, ports.ProfileCodeHost) []ports.Problem { return s.probs }
