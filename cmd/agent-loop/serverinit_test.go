package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// #103: server init creates the projection directory and reports it. When
// ~/.local can't be written, a new config names a writable fallback (work
// plan D1, D2); a kept config naming an uncreatable path fails init.
func TestServerInitCreatesAWritableProjectionDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	local := filepath.Join(home, ".local")
	if err := os.Mkdir(local, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(local, 0o700) })

	code, out, errs := cli(t, "server", "init", "--no-start")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	cfg := filepath.Join(home, ".config", "agent-loop", "deployment.toml")
	dep, err := profile.LoadDeployment(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".config", "agent-loop", "projection.db")
	if runtime.GOOS == "darwin" {
		want = filepath.Join(home, "Library", "Application Support", "agent-loop", "projection.db")
	}
	if dep.Projection.Path != want {
		t.Fatalf("projection path %q, want %q", dep.Projection.Path, want)
	}
	if fi, err := os.Stat(filepath.Dir(want)); err != nil || !fi.IsDir() {
		t.Fatalf("projection directory not created: %v", err)
	}
	if !strings.Contains(out, "Projection database: "+want) || !strings.Contains(out, filepath.Join(local, "state", "agent-loop")) {
		t.Fatalf("output %q", out)
	}

	// A kept config naming a path under ~/.local fails and names the key.
	if err := os.WriteFile(cfg, []byte("[projection]\npath = \"~/.local/state/agent-loop/projection.db\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errs = cli(t, "server", "init", "--no-start")
	if code != 1 || !strings.Contains(errs, "[projection] path") || !strings.Contains(errs, cfg) {
		t.Fatalf("kept config: exit %d %q", code, errs)
	}
	if b, _ := os.ReadFile(cfg); !strings.Contains(string(b), ".local/state") {
		t.Fatal("kept config was rewritten")
	}
}

// With a writable home, the default path is kept and its directory created.
func TestServerInitCreatesTheDefaultProjectionDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	code, out, errs := cli(t, "server", "init", "--no-start")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	want := filepath.Join(home, ".local", "state", "agent-loop", "projection.db")
	if fi, err := os.Stat(filepath.Dir(want)); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("projection directory: %v %v", fi, err)
	}
	if !strings.Contains(out, "Projection database: "+want) {
		t.Fatalf("output %q", out)
	}
}

// #103 review R1: an existing projection directory the user can't write to
// (left owned by root after a sudo run, say) is skipped for a new config and
// fails init for a kept one. os.MkdirAll alone returns nil for it.
func TestServerInitSkipsAnExistingUnwritableProjectionDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	state := filepath.Join(home, ".local", "state", "agent-loop")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(state, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(state, 0o700) })

	code, out, errs := cli(t, "server", "init", "--no-start")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	cfg := filepath.Join(home, ".config", "agent-loop", "deployment.toml")
	dep, err := profile.LoadDeployment(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".config", "agent-loop", "projection.db")
	if runtime.GOOS == "darwin" {
		want = filepath.Join(home, "Library", "Application Support", "agent-loop", "projection.db")
	}
	if dep.Projection.Path != want {
		t.Fatalf("projection path %q, want %q", dep.Projection.Path, want)
	}
	if !strings.Contains(out, "Can't create or write to "+state) || !strings.Contains(out, "Projection database: "+want) {
		t.Fatalf("output %q", out)
	}
	if probes, _ := filepath.Glob(filepath.Join(filepath.Dir(want), ".probe-*")); len(probes) != 0 {
		t.Fatalf("probe file left behind: %v", probes)
	}

	// A kept config naming the unwritable directory fails and names the key.
	if err := os.WriteFile(cfg, []byte("[projection]\npath = \"~/.local/state/agent-loop/projection.db\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errs = cli(t, "server", "init", "--no-start")
	if code != 1 || !strings.Contains(errs, "[projection] path") || !strings.Contains(errs, cfg) {
		t.Fatalf("kept config: exit %d %q", code, errs)
	}
}

// #103 review R2: on macOS, with both ~/.local and ~/Library unwritable, a
// new config falls back to the config directory as an absolute path (D1),
// and both skipped candidates are reported.
func TestServerInitFallsBackToTheConfigDirOnMacOS(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the Application Support candidate exists only on macOS")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, d := range []string{".local", "Library"} {
		p := filepath.Join(home, d)
		if err := os.Mkdir(p, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(p, 0o700) })
	}

	code, out, errs := cli(t, "server", "init", "--no-start")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	cfg := filepath.Join(home, ".config", "agent-loop", "deployment.toml")
	dep, err := profile.LoadDeployment(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".config", "agent-loop", "projection.db")
	if dep.Projection.Path != want {
		t.Fatalf("projection path %q, want %q", dep.Projection.Path, want)
	}
	if fi, err := os.Stat(filepath.Dir(want)); err != nil || !fi.IsDir() {
		t.Fatalf("projection directory: %v", err)
	}
	if n := strings.Count(out, "Can't create or write to "); n != 2 {
		t.Fatalf("%d skip lines, want 2: %q", n, out)
	}
	for _, skipped := range []string{
		filepath.Join(home, ".local", "state", "agent-loop"),
		filepath.Join(home, "Library", "Application Support", "agent-loop"),
	} {
		if !strings.Contains(out, "Can't create or write to "+skipped) {
			t.Fatalf("no skip line for %s: %q", skipped, out)
		}
	}
	if !strings.Contains(out, "Projection database: "+want) {
		t.Fatalf("output %q", out)
	}
}
