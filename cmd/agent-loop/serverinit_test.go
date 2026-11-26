package main

import (
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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
	t.Setenv("XDG_STATE_HOME", "")
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
	t.Setenv("XDG_STATE_HOME", "")
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
	t.Setenv("XDG_STATE_HOME", "")
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
	t.Setenv("XDG_STATE_HOME", "")
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
	t.Setenv("XDG_STATE_HOME", "")
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

// #113 D3 (owner ruling): with XDG_STATE_HOME set, a new config's projection
// database defaults to $XDG_STATE_HOME/agent-loop/projection.db, written as
// an absolute path, and ~/.local/state is left alone.
func TestServerInitHonoursXDGStateHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	xdg := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", xdg)

	code, out, errs := cli(t, "server", "init", "--no-start")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	cfg := filepath.Join(home, ".config", "agent-loop", "deployment.toml")
	dep, err := profile.LoadDeployment(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(xdg, "agent-loop", "projection.db")
	if dep.Projection.Path != want {
		t.Fatalf("projection path %q, want %q", dep.Projection.Path, want)
	}
	if b, _ := os.ReadFile(cfg); !strings.Contains(string(b), want) {
		t.Fatalf("config doesn't name %s:\n%s", want, b)
	}
	if fi, err := os.Stat(filepath.Dir(want)); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("projection directory: %v %v", fi, err)
	}
	if !strings.Contains(out, "Projection database: "+want) {
		t.Fatalf("output %q", out)
	}
	if _, err := os.Stat(filepath.Join(home, ".local")); !os.IsNotExist(err) {
		t.Fatalf("~/.local touched: %v", err)
	}
}

// #113 D2: an empty or relative XDG_STATE_HOME is ignored, so the default
// stays ~/.local/state/agent-loop.
func TestServerInitIgnoresAnEmptyOrRelativeXDGStateHome(t *testing.T) {
	for _, v := range []string{"", "relative/state"} {
		t.Run(v, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_STATE_HOME", v)
			t.Chdir(t.TempDir())
			code, _, errs := cli(t, "server", "init", "--no-start")
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errs)
			}
			dep, err := profile.LoadDeployment(filepath.Join(home, ".config", "agent-loop", "deployment.toml"))
			if err != nil {
				t.Fatal(err)
			}
			want := filepath.Join(home, ".local", "state", "agent-loop", "projection.db")
			if dep.Projection.Path != want {
				t.Fatalf("projection path %q, want %q", dep.Projection.Path, want)
			}
		})
	}
}

// #113 D1: an unwritable $XDG_STATE_HOME/agent-loop is skipped and reported;
// the existing fallbacks apply, and ~/.local/state is not tried.
func TestServerInitSkipsAnUnwritableXDGStateHome(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	xdg := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(xdg, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(xdg, 0o700) })
	t.Setenv("XDG_STATE_HOME", xdg)

	code, out, errs := cli(t, "server", "init", "--no-start")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	dep, err := profile.LoadDeployment(filepath.Join(home, ".config", "agent-loop", "deployment.toml"))
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
	if !strings.Contains(out, "Can't create or write to "+filepath.Join(xdg, "agent-loop")) {
		t.Fatalf("no skip line for the XDG directory: %q", out)
	}
	if _, err := os.Stat(filepath.Join(home, ".local")); !os.IsNotExist(err) {
		t.Fatalf("~/.local tried: %v", err)
	}
}

// #133 (PR #132 R1): a path holding a byte that Go's %q escapes in a way
// TOML doesn't accept, such as 0x7f, is written with a TOML encoder, so the
// config server init writes parses and names the path exactly.
func TestServerInitWritesAPathWithAControlByte(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	xdg := filepath.Join(t.TempDir(), "st\x7fate")
	t.Setenv("XDG_STATE_HOME", xdg)

	code, _, errs := cli(t, "server", "init", "--no-start")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	dep, err := profile.LoadDeployment(filepath.Join(home, ".config", "agent-loop", "deployment.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(xdg, "agent-loop", "projection.db"); dep.Projection.Path != want {
		t.Fatalf("projection path %q, want %q", dep.Projection.Path, want)
	}
}

// #133 D1: a path that isn't valid UTF-8 can't be written to a TOML file, so
// server init refuses it and writes no config.
func TestServerInitRefusesAPathThatIsNotUTF8(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	xdg := filepath.Join(t.TempDir(), "st\xffate")
	if err := os.Mkdir(xdg, 0o700); err != nil {
		t.Skipf("the file system refuses a name that isn't UTF-8: %v", err)
	}
	t.Setenv("XDG_STATE_HOME", xdg)

	code, _, errs := cli(t, "server", "init", "--no-start")
	if code != 1 || !strings.Contains(errs, "not valid UTF-8") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "agent-loop", "deployment.toml")); !os.IsNotExist(err) {
		t.Fatalf("config written: %v", err)
	}
}

func TestTOMLStringEncodesControlBytesAndRefusesBadUTF8(t *testing.T) {
	got, err := tomlString("a\x7fb\"c\\d")
	if err != nil || got != `"a\u007fb\"c\\d"` {
		t.Fatalf("got %s, %v", got, err)
	}
	if _, err := tomlString("a\xffb"); err == nil || !strings.Contains(err.Error(), "not valid UTF-8") {
		t.Fatalf("err %v", err)
	}
}

// #133 (PR #132 R2): with no home directory, an absolute XDG_STATE_HOME is
// still tried, since it needs no home; without one the config directory is
// used.
func TestServerInitUsesXDGStateHomeWithoutAHomeDirectory(t *testing.T) {
	t.Setenv("HOME", "")
	if _, err := os.UserHomeDir(); err == nil {
		t.Skip("the home directory is found without HOME on this platform")
	}
	cfgDir := t.TempDir()
	xdg := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", xdg)
	if got, want := pickProjectionPath(cfgDir, io.Discard), filepath.Join(xdg, "agent-loop", "projection.db"); got != want {
		t.Fatalf("projection path %q, want %q", got, want)
	}

	t.Setenv("XDG_STATE_HOME", "")
	if got, want := pickProjectionPath(cfgDir, io.Discard), filepath.Join(cfgDir, "projection.db"); got != want {
		t.Fatalf("projection path %q, want %q", got, want)
	}
}

// noHome unsets HOME, skipping the test where the home directory is still
// found without it.
func noHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", "")
	if _, err := os.UserHomeDir(); err == nil {
		t.Skip("the home directory is found without HOME on this platform")
	}
}

// shortTempDir is a temp directory short enough to hold a Unix socket;
// t.TempDir's names, which hold the test name, often aren't.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "al")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// withSecretsBackend makes server init write backend as the secret-store
// backend, so a test covers both backends on either platform.
func withSecretsBackend(t *testing.T, backend string) {
	t.Helper()
	old := serverInitSecretsBackend
	serverInitSecretsBackend = func() string { return backend }
	t.Cleanup(func() { serverInitSecretsBackend = old })
}

// #134 D2 (#139): with no home directory, server init --config writes
// [api] socket as an absolute path to api.sock beside the config, so the
// config it wrote loads and init succeeds. With the env-file backend (the
// Linux default) it writes [secrets] path as secrets.env beside the config
// too (work plan #139 D4). It says which ~/ projection places it skipped
// (work plan #139 D3).
func TestI134D2_ServerInitWritesTheSocketBesideTheConfigWithoutAHome(t *testing.T) {
	for _, backend := range []string{profile.SecretsEnvFile, profile.SecretsKeychain} {
		t.Run(backend, func(t *testing.T) {
			noHome(t)
			t.Setenv("XDG_STATE_HOME", "")
			withSecretsBackend(t, backend)
			dir := shortTempDir(t)
			cfg := filepath.Join(dir, "deployment.toml")
			code, out, errs := cli(t, "server", "init", "--config", cfg, "--no-start")
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errs)
			}
			dep, err := profile.LoadDeployment(cfg)
			if err != nil {
				t.Fatal(err)
			}
			want := filepath.Join(dir, "api.sock")
			if dep.API.Socket != want {
				t.Fatalf("socket %q, want %q", dep.API.Socket, want)
			}
			b, _ := os.ReadFile(cfg)
			if !strings.Contains(string(b), "socket = \""+want+"\"") {
				t.Fatalf("config does not name the socket in full:\n%s", b)
			}
			if dep.Secrets.Backend != backend {
				t.Fatalf("backend %q, want %q", dep.Secrets.Backend, backend)
			}
			wantSecrets := filepath.Join(dir, "secrets.env")
			if backend == profile.SecretsEnvFile {
				if dep.Secrets.Path != wantSecrets {
					t.Fatalf("secrets path %q, want %q", dep.Secrets.Path, wantSecrets)
				}
				if !strings.Contains(string(b), "path = \""+wantSecrets+"\"") {
					t.Fatalf("config does not name the secrets path in full:\n%s", b)
				}
				if !strings.Contains(out, "env-file secrets are in "+wantSecrets) {
					t.Fatalf("secrets path not reported: %q", out)
				}
			} else if strings.Contains(string(b), "secrets.env") || dep.Secrets.Path != "" {
				t.Fatalf("keychain config names a secrets path:\n%s", b)
			}
			if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm()&0o077 != 0 {
				t.Fatalf("config directory mode: %v %v", fi, err)
			}
			if !strings.Contains(out, "API socket is "+want) {
				t.Fatalf("output %q", out)
			}
			if !strings.Contains(out, "skipping ~/.local/state/agent-loop/projection.db") {
				t.Fatalf("skipped ~/ place not reported: %q", out)
			}
			if strings.Contains(string(b), "~") {
				t.Fatalf("config names a ~ path with no home directory:\n%s", b)
			}
		})
	}
}

// #139 D1: a socket path beside the config that doesn't fit a Unix socket
// address with its NUL (exactly sun_path bytes) is refused, naming it, and no
// config is written.
func TestI139D1_ServerInitRefusesASocketPathTooLong(t *testing.T) {
	noHome(t)
	t.Setenv("XDG_STATE_HOME", "")
	// Pad the directory so that <dir>/api.sock is exactly sun_path bytes:
	// one byte past the longest accepted path (the boundary D1 defines).
	base := shortTempDir(t)
	pad := sunPathSize() - len(filepath.Join(base, "x", "api.sock"))
	if pad < 0 {
		t.Skip("temp directory too long")
	}
	dir := filepath.Join(base, "x"+strings.Repeat("a", pad))
	if n := len(filepath.Join(dir, "api.sock")); n != sunPathSize() {
		t.Fatalf("socket path is %d bytes, want exactly %d", n, sunPathSize())
	}
	cfg := filepath.Join(dir, "deployment.toml")
	code, _, errs := cli(t, "server", "init", "--config", cfg, "--no-start")
	sock := filepath.Join(dir, "api.sock")
	if code != 1 || !strings.Contains(errs, sock) || !strings.Contains(errs, "too long for a Unix socket") {
		t.Fatalf("exit %d: %q", code, errs)
	}
	if _, err := os.Stat(cfg); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("config written: %v", err)
	}
}

// #139 D1: the longest path accepted is one byte short of sun_path, and it
// can be listened on.
func TestI139D1_LongestAcceptedSocketPathListens(t *testing.T) {
	noHome(t)
	base := shortTempDir(t)
	pad := sunPathSize() - 1 - len(filepath.Join(base, "x", "api.sock"))
	if pad < 0 {
		t.Skip("temp directory too long")
	}
	dir := filepath.Join(base, "x"+strings.Repeat("a", pad))
	sock, err := pickAPISocket(dir, io.Discard)
	if err != nil || len(sock) != sunPathSize()-1 {
		t.Fatalf("%q (%d bytes), %v", sock, len(sock), err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen on %s: %v", sock, err)
	}
	l.Close()
}

// With a home directory the default ~ socket is written, as before.
func TestI134D2_ServerInitKeepsTheDefaultSocketWithAHome(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if got, err := pickAPISocket(t.TempDir(), io.Discard); err != nil || got != profile.DefaultAPISocket {
		t.Fatalf("%q, %v", got, err)
	}
}

// #139 D4: with a home directory the env-file secrets path is left to its
// ~ default, and the Keychain backend never gets one.
func TestI139D4_SecretsPathLeftToTheDefaultWithAHome(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, backend := range []string{profile.SecretsEnvFile, profile.SecretsKeychain} {
		if got := pickSecretsPath(backend, t.TempDir(), io.Discard); got != "" {
			t.Fatalf("%s: %q", backend, got)
		}
	}
	noHome(t)
	if got := pickSecretsPath(profile.SecretsKeychain, t.TempDir(), io.Discard); got != "" {
		t.Fatalf("keychain without a home: %q", got)
	}
}

// #139 D4 (PR #149 R3): the [secrets] section is followed by one blank line
// before [projects], with and without a secrets path, as the other sections
// are.
func TestI139D4_WrittenConfigKeepsOneBlankLineBeforeProjects(t *testing.T) {
	for _, home := range []bool{true, false} {
		for _, backend := range []string{profile.SecretsEnvFile, profile.SecretsKeychain} {
			t.Run(strconv.FormatBool(home)+"/"+backend, func(t *testing.T) {
				if home {
					t.Setenv("HOME", t.TempDir())
				} else {
					noHome(t)
				}
				t.Setenv("XDG_STATE_HOME", "")
				withSecretsBackend(t, backend)
				cfg := filepath.Join(shortTempDir(t), "deployment.toml")
				if code, _, errs := cli(t, "server", "init", "--config", cfg, "--no-start"); code != 0 {
					t.Fatalf("exit %d: %s", code, errs)
				}
				b, err := os.ReadFile(cfg)
				if err != nil {
					t.Fatal(err)
				}
				body := string(b)
				sec := body[strings.Index(body, "[secrets]"):]
				if !strings.HasSuffix(sec, "\"\n\n[projects]\n") || strings.Contains(sec, "\n\n\n") {
					t.Fatalf("want one blank line before [projects]:\n%q", sec)
				}
			})
		}
	}
}

// sunPathSize is the size of sun_path in struct sockaddr_un: 108 bytes on
// Linux, 104 on macOS and the other BSDs (#139 D1). The check itself lives in
// api.CheckSocketPath (#174 D1).
func sunPathSize() int {
	if runtime.GOOS == "linux" {
		return 108
	}
	return 104
}
