package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayusuk/agent-loop/internal/profile"
)

// T2, TECHNICAL-SPEC §11: server init writes a valid deployment config and
// the Temporal Compose files, then starts Compose; it keeps an existing
// config and password.
func TestT2_ServerInitWritesConfigAndCompose(t *testing.T) {
	realHome := os.Getenv("HOME") // docker finds its compose plugin there
	home := t.TempDir()
	t.Setenv("HOME", home)
	var calls [][]string
	old := runDocker
	runDocker = func(_, _ io.Writer, args ...string) error { calls = append(calls, args); return nil }
	t.Cleanup(func() { runDocker = old })

	code, out, errs := cli(t, "server", "init")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	cfg := filepath.Join(home, ".config", "agent-loop", "deployment.toml")
	dep, err := profile.LoadDeployment(cfg)
	if err != nil {
		t.Fatalf("written config: %v", err)
	}
	if dep.API.Socket != filepath.Join(home, ".config", "agent-loop", "api.sock") || dep.API.Gateway.Enabled ||
		dep.Temporal.Address != profile.DefaultTemporalAddress || len(dep.Projects) != 0 {
		t.Fatalf("config %+v", dep)
	}
	if fi, _ := os.Stat(cfg); fi.Mode().Perm() != 0o600 {
		t.Fatalf("config mode %o", fi.Mode().Perm())
	}
	compose := filepath.Join(home, ".config", "agent-loop", "temporal", "compose.yaml")
	if len(calls) != 1 || strings.Join(calls[0], " ") != "compose -f "+compose+" up -d" {
		t.Fatalf("docker calls %v", calls)
	}
	if !strings.Contains(out, "Wrote "+cfg) {
		t.Fatalf("output %q", out)
	}
	env := filepath.Join(filepath.Dir(compose), ".env")
	pw, err := os.ReadFile(env)
	if err != nil || !strings.HasPrefix(string(pw), "POSTGRES_PASSWORD=") || len(pw) < 40 {
		t.Fatalf(".env %q, %v", pw, err)
	}

	// Again with --no-start: the config and password are kept.
	os.WriteFile(cfg, []byte("[temporal]\naddress = \"localhost:9999\"\n"), 0o600)
	code, out, errs = cli(t, "server", "init", "--no-start")
	if code != 0 || !strings.Contains(out, "Kept "+cfg) || len(calls) != 1 {
		t.Fatalf("second init: exit %d %q %q, calls %v", code, out, errs, calls)
	}
	if again, _ := os.ReadFile(env); string(again) != string(pw) {
		t.Fatal("the Postgres password changed")
	}

	// The Compose file passes docker compose config.
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker isn't installed")
	}
	docker := func(args ...string) *exec.Cmd {
		c := exec.Command("docker", args...)
		c.Env = append(os.Environ(), "HOME="+realHome)
		return c
	}
	if err := docker("compose", "version").Run(); err != nil {
		t.Skip("docker compose isn't installed")
	}
	cmd := docker("compose", "-f", compose, "config", "--quiet")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("docker compose config: %v\n%s", err, b)
	}
}

// A bad existing config is reported, not replaced.
func TestServerInitRefusesInvalidConfig(t *testing.T) {
	home := t.TempDir()
	cfg := filepath.Join(home, "deployment.toml")
	os.WriteFile(cfg, []byte("nonsense = 1\n"), 0o600)
	code, _, errs := cli(t, "server", "init", "--config", cfg, "--no-start")
	if code != 1 || errs == "" {
		t.Fatalf("exit %d %q", code, errs)
	}
	if b, _ := os.ReadFile(cfg); string(b) != "nonsense = 1\n" {
		t.Fatal("config replaced")
	}
}
