package runners

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mayusuk/agent-loop/internal/profile"
)

var update = flag.Bool("update", false, "rewrite golden files")

// goldenPaths is one run's paths, as a macOS worker would have them.
func goldenPaths() SandboxPaths {
	home := "/Users/owner"
	dep := &profile.Deployment{
		Path:       home + "/.config/agent-loop/deployment.toml",
		Projection: profile.Projection{Path: home + "/.local/state/agent-loop/projection.db"},
		API:        profile.API{Socket: home + "/.config/agent-loop/api.sock"},
		Secrets:    profile.Secrets{Backend: profile.SecretsEnvFile, Path: home + "/.config/agent-loop/secrets.env"},
	}
	run := "/private/var/folders/xy/T/al-run-1"
	egress := append(slices.Clone(profile.DefaultEgress), profile.CodeHostEgress["github"]...)
	egress = append(egress, "example.org", "localhost") // a profile addition, and loopback, which stays denied
	return SandboxPaths{
		Worktree:   home + "/Library/Caches/agent-loop/workspaces/runs/agent-42-abc",
		OutDir:     run + "/out",
		ResultPath: run + "/result.json",
		TempDir:    run + "/tmp",
		Socket:     run + "/git.sock",
		DenyRead:   DeniedReads(dep, home),
		Egress:     egress,
	}
}

// TestSandboxSettingsGolden compares the generated settings with the
// checked-in golden file. Run with -update to rewrite it after a reviewed
// change.
func TestSandboxSettingsGolden(t *testing.T) {
	got, err := Settings(goldenPaths())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "claude-settings.golden.json")
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("settings differ from %s:\n%s", path, got)
	}
}

// TestSandboxSettingsMeetTechnicalSpec72 checks the golden settings item by
// item against TECHNICAL-SPEC §7.2.
func TestSandboxSettingsMeetTechnicalSpec72(t *testing.T) {
	p := goldenPaths()
	data, err := os.ReadFile(filepath.Join("testdata", "claude-settings.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Sandbox struct {
			Enabled, FailIfUnavailable, AllowUnsandboxedCommands bool
			ExcludedCommands                                     []string
			Filesystem                                           struct{ AllowWrite, DenyRead []string }
			Network                                              struct {
				AllowedDomains, DeniedDomains, AllowUnixSockets []string
				AllowAllUnixSockets, AllowLocalBinding          bool
			}
		}
		Permissions struct {
			DisableBypassPermissionsMode string
			Allow, Deny                  []string
		}
	}
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	sb := s.Sandbox
	if !sb.Enabled || !sb.FailIfUnavailable || sb.AllowUnsandboxedCommands || len(sb.ExcludedCommands) != 0 {
		t.Errorf("the sandbox must be on, required, and with no way out: %+v", sb)
	}
	if s.Permissions.DisableBypassPermissionsMode != "disable" {
		t.Error("skip-permissions must be disabled under the local runner")
	}

	// Writes: the worktree, AGENT_LOOP_OUT, the result path and a per-run
	// temp directory. Nothing else.
	wantWrites := []string{p.Worktree, p.OutDir, p.ResultPath, p.TempDir}
	if !slices.Equal(sb.Filesystem.AllowWrite, wantWrites) {
		t.Errorf("allowWrite = %q, want %q", sb.Filesystem.AllowWrite, wantWrites)
	}

	// Reads denied.
	home := "/Users/owner"
	for _, rel := range []string{".config/agent-loop", ".local/state/agent-loop/projection.db", ".local/state/agent-loop",
		".ssh", ".aws", ".config/gh", ".netrc", ".docker", ".kube", ".git-credentials", ".config/git/credentials", "Library/Keychains"} {
		path := filepath.Join(home, rel)
		if !slices.Contains(sb.Filesystem.DenyRead, path) {
			t.Errorf("denyRead lacks %s", path)
		}
		// Only a permission rule needs "//" for an absolute path.
		if !slices.Contains(s.Permissions.Deny, "Read(/"+path+"/**)") {
			t.Errorf("the Read tool may read %s", path)
		}
	}

	// Network: only the allowlist; loopback and the local network denied;
	// the credential helper's socket is the only socket.
	for _, d := range []string{"api.anthropic.com", "github.com", "api.github.com", "codeload.github.com", "objects.githubusercontent.com",
		"registry.npmjs.org", "proxy.golang.org", "sum.golang.org", "pypi.org", "files.pythonhosted.org", "crates.io", "static.crates.io", "example.org"} {
		if !slices.Contains(sb.Network.AllowedDomains, d) {
			t.Errorf("allowedDomains lacks %s", d)
		}
		if !slices.Contains(s.Permissions.Allow, "WebFetch(domain:"+d+")") {
			t.Errorf("WebFetch may not reach %s", d)
		}
	}
	if len(sb.Network.AllowedDomains) != 13 {
		t.Errorf("allowedDomains = %q, want exactly the defaults and the addition", sb.Network.AllowedDomains)
	}
	for _, d := range []string{"localhost", "127.0.0.1", "::1"} {
		if slices.Contains(sb.Network.AllowedDomains, d) || !slices.Contains(sb.Network.DeniedDomains, d) {
			t.Errorf("loopback %s is not denied", d)
		}
	}
	if sb.Network.AllowLocalBinding || sb.Network.AllowAllUnixSockets || !slices.Equal(sb.Network.AllowUnixSockets, []string{p.Socket}) {
		t.Errorf("network = %+v", sb.Network)
	}
}

func TestSandboxSettingsRejectsBadPaths(t *testing.T) {
	p := goldenPaths()
	p.OutDir = "out"
	if _, err := Settings(p); err == nil {
		t.Error("a relative path was accepted")
	}
	p = goldenPaths()
	p.Egress = nil
	if _, err := Settings(p); err == nil {
		t.Error("an empty allowlist was accepted")
	}
}

func TestDeniedReadsWithoutDeployment(t *testing.T) {
	got := DeniedReads(nil, "/home/o")
	for _, want := range []string{"/home/o/.config/agent-loop", "/home/o/.local/state/agent-loop", "/home/o/.ssh", "/home/o/Library/Keychains"} {
		if !slices.Contains(got, want) {
			t.Errorf("DeniedReads lacks %s: %q", want, got)
		}
	}
	// A deployment elsewhere adds its own paths, once each.
	dep := &profile.Deployment{Path: "/etc/al/deployment.toml", API: profile.API{Socket: "/run/al/api.sock"}, Projection: profile.Projection{Path: "/data/p.db"}}
	got = DeniedReads(dep, "/home/o")
	for _, want := range []string{"/etc/al", "/run/al", "/data/p.db", "/data/p.db-wal"} {
		if !slices.Contains(got, want) {
			t.Errorf("DeniedReads lacks %s", want)
		}
	}
	seen := map[string]bool{}
	for _, p := range got {
		if seen[p] {
			t.Errorf("%s listed twice", p)
		}
		seen[p] = true
	}
}

func TestWrapped(t *testing.T) {
	for cmd, want := range map[string]bool{
		"agent-loop-claude implement":             true,
		"/usr/local/bin/agent-loop-claude review": true,
		"claude -p x":                      false,
		"my-handler":                       false,
		"agent-loop-claude-fork implement": false,
	} {
		if got := Wrapped(strings.Fields(cmd)); got != want {
			t.Errorf("Wrapped(%q) = %v", cmd, got)
		}
	}
	if Wrapped(nil) {
		t.Error("an empty command is wrapped")
	}
}
