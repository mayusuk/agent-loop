package runners

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mayusuk/agent-loop/internal/profile"
)

// TestLiveGhVerifiesTLSInTheSandbox runs gh inside Claude Code's sandbox
// with the settings this runner generates, and checks that gh verifies
// GitHub's certificate (#99, work plan D1). It is skipped unless
// AGENT_LOOP_LIVE=1, and needs claude (signed in) and gh on PATH. gh gets a
// dummy token, so GitHub answering "HTTP 401" proves the TLS handshake
// passed; a sandbox that blocks the macOS trust service fails it with an
// x509 error instead.
func TestLiveGhVerifiesTLSInTheSandbox(t *testing.T) {
	if os.Getenv("AGENT_LOOP_LIVE") != "1" {
		t.Skip("set AGENT_LOOP_LIVE=1 to run gh inside the Claude Code sandbox")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("the Claude Code sandbox runs on macOS and Linux")
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude is not on PATH")
	}
	if _, err := exec.LookPath("gh"); err != nil {
		t.Skip("gh is not on PATH")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := SandboxPaths{
		Worktree:   filepath.Join(root, "tree"),
		OutDir:     filepath.Join(root, "out"),
		ResultPath: filepath.Join(root, "result.json"),
		TempDir:    filepath.Join(root, "tmp"),
		Socket:     filepath.Join(root, "tmp", "git.sock"),
		DenyRead:   DeniedReads(nil, home),
		Egress:     append(slices.Clone(profile.DefaultEgress), profile.CodeHostEgress["github"]...),
	}
	for _, d := range []string{p.Worktree, p.OutDir, p.TempDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	settings, err := Settings(p)
	if err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(root, "settings.json")
	if err := os.WriteFile(settingsPath, settings, 0o600); err != nil {
		t.Fatal(err)
	}
	outFile := filepath.Join(p.OutDir, "gh.txt")
	script := "#!/bin/sh\n" +
		"export GH_TOKEN=agent-loop-dummy-token GH_CONFIG_DIR=" + filepath.Join(p.TempDir, "gh") + "\n" +
		"gh api /zen > " + outFile + " 2>&1\n" +
		"echo \"exit $?\" >> " + outFile + "\n"
	if err := os.WriteFile(filepath.Join(p.Worktree, "check-gh.sh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, claude, "-p",
		"--settings", settingsPath, "--setting-sources", "",
		"--model", "haiku", "--max-turns", "4",
		"Use the Bash tool to run exactly this command once: ./check-gh.sh  Then reply done.")
	cmd.Dir = p.Worktree
	cmd.Env = append(os.Environ(), "TMPDIR="+p.TempDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("claude: %v\n%s", err, out)
	}
	got, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("the sandboxed command wrote no output (did claude run it?): %v", err)
	}
	s := string(got)
	if strings.Contains(s, "x509") || strings.Contains(s, "certificate") {
		t.Fatalf("gh failed TLS verification inside the sandbox:\n%s", s)
	}
	if !strings.Contains(s, "HTTP 401") {
		t.Fatalf("gh did not get GitHub's answer for a dummy token:\n%s", s)
	}
}
