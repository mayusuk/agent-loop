package runners

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mayusuk/agent-loop/internal/profile"
)

// ClaudeSettingsEnv names the environment variable that holds the path of
// the run's Claude Code settings file. The starter handlers pass it to
// `claude --settings` (work plan D1).
const ClaudeSettingsEnv = "AGENT_LOOP_CLAUDE_SETTINGS"

// WrappedHandlers are the handler programs that run their agent inside the
// Claude Code sandbox with the settings this runner writes (work plan D11).
var WrappedHandlers = []string{"agent-loop-claude"}

// Wrapped reports whether the Claude Code sandbox wraps the handler that
// command starts: its program's base name is one of WrappedHandlers.
func Wrapped(command []string) bool {
	return len(command) > 0 && slices.Contains(WrappedHandlers, filepath.Base(command[0]))
}

// LoopbackDomains are denied even when the egress allowlist names them, so
// a handler can't reach the server's gateway or other local services
// (TECHNICAL-SPEC §7.2, work plan D10).
var LoopbackDomains = []string{
	"localhost",
	"*.localhost",
	"127.0.0.1",
	"::1",
	"0.0.0.0",
	"*.local",
	"host.docker.internal",
}

// MachServices are the macOS mach (XPC) services a sandboxed command may
// look up: only the trust service, which Go's crypto/x509 asks to verify
// certificate chains. Without it every Go program that does HTTPS, gh and
// go mod download among them, fails with "x509: OSStatus -26276" (#99, work
// plan D1). Seatbelt reads this key; the Linux sandbox ignores it (D2).
var MachServices = []string{"com.apple.trustd.agent"}

// SandboxPaths is what one run's sandbox settings are made from. Every path
// is absolute, with symlinks resolved.
type SandboxPaths struct {
	Worktree   string   // the run's tree
	OutDir     string   // AGENT_LOOP_OUT
	ResultPath string   // AGENT_LOOP_RESULT
	TempDir    string   // the run's writable temp directory (TMPDIR)
	Socket     string   // the credential helper's socket
	DenyRead   []string // DeniedReads
	Egress     []string // the stage's egress allowlist
}

// claudeSettings is the part of Claude Code's settings file the runner
// writes (work plan D8).
type claudeSettings struct {
	Sandbox     sandboxSettings     `json:"sandbox"`
	Permissions permissionsSettings `json:"permissions"`
}

type sandboxSettings struct {
	Enabled                  bool               `json:"enabled"`
	FailIfUnavailable        bool               `json:"failIfUnavailable"`
	AutoAllowBashIfSandboxed bool               `json:"autoAllowBashIfSandboxed"`
	AllowUnsandboxedCommands bool               `json:"allowUnsandboxedCommands"`
	ExcludedCommands         []string           `json:"excludedCommands"`
	Filesystem               filesystemSettings `json:"filesystem"`
	Network                  networkSettings    `json:"network"`
}

type filesystemSettings struct {
	AllowWrite []string `json:"allowWrite"`
	DenyRead   []string `json:"denyRead"`
}

type networkSettings struct {
	AllowedDomains      []string `json:"allowedDomains"`
	DeniedDomains       []string `json:"deniedDomains"`
	AllowUnixSockets    []string `json:"allowUnixSockets"`
	AllowAllUnixSockets bool     `json:"allowAllUnixSockets"`
	AllowLocalBinding   bool     `json:"allowLocalBinding"`
	AllowMachLookup     []string `json:"allowMachLookup"`
}

type permissionsSettings struct {
	DisableBypassPermissionsMode string   `json:"disableBypassPermissionsMode"`
	AdditionalDirectories        []string `json:"additionalDirectories"`
	Allow                        []string `json:"allow"`
	Deny                         []string `json:"deny"`
}

// ruleAbs writes an absolute path inside a permission rule such as
// Read(...), where Claude Code reads a single leading "/" as relative to the
// settings file and needs "//" for an absolute path. The sandbox.filesystem
// paths and additionalDirectories take plain absolute paths (work plan D8).
func ruleAbs(p string) string { return "/" + p }

// Settings returns the Claude Code settings for one run (TECHNICAL-SPEC
// §7.2): the sandbox on and required, writes only to the run's tree, out
// directory, result path and temp directory, the denied reads, the egress
// allowlist with loopback denied, the credential helper's socket as the
// only Unix socket, and the macOS trust service as the only mach service.
func Settings(p SandboxPaths) ([]byte, error) {
	for name, v := range map[string]string{"worktree": p.Worktree, "out dir": p.OutDir, "result path": p.ResultPath, "temp dir": p.TempDir, "socket": p.Socket} {
		if !filepath.IsAbs(v) {
			return nil, fmt.Errorf("sandbox settings: %s %q is not absolute", name, v)
		}
	}
	if len(p.Egress) == 0 {
		return nil, errors.New("sandbox settings: the egress allowlist is empty")
	}
	writes := []string{p.Worktree, p.OutDir, p.ResultPath, p.TempDir}
	s := claudeSettings{
		Sandbox: sandboxSettings{
			Enabled:                  true,
			FailIfUnavailable:        true,
			AutoAllowBashIfSandboxed: true,
			AllowUnsandboxedCommands: false,
			ExcludedCommands:         []string{},
			Filesystem: filesystemSettings{
				AllowWrite: slices.Clone(writes),
				DenyRead:   slices.Clone(p.DenyRead),
			},
			Network: networkSettings{
				AllowedDomains:   allowed(p.Egress),
				DeniedDomains:    slices.Clone(LoopbackDomains),
				AllowUnixSockets: []string{p.Socket},
				AllowMachLookup:  slices.Clone(MachServices),
			},
		},
		Permissions: permissionsSettings{
			DisableBypassPermissionsMode: "disable",
			AdditionalDirectories:        []string{p.OutDir, p.TempDir},
			Allow:                        []string{},
			Deny:                         []string{},
		},
	}
	for _, d := range s.Sandbox.Network.AllowedDomains {
		s.Permissions.Allow = append(s.Permissions.Allow, "WebFetch(domain:"+d+")")
	}
	for _, d := range p.DenyRead {
		s.Permissions.Deny = append(s.Permissions.Deny, "Read("+ruleAbs(d)+")", "Read("+ruleAbs(d)+"/**)")
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// allowed is the egress allowlist without the loopback names, which are
// denied whatever the profile says.
func allowed(egress []string) []string {
	out := []string{}
	for _, d := range egress {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" || slices.Contains(LoopbackDomains, d) || slices.Contains(out, d) {
			continue
		}
		out = append(out, d)
	}
	return out
}

// DeniedReads lists the paths a sandboxed handler may not read
// (TECHNICAL-SPEC §7.2, work plan D9): agent-loop's config directory (with
// the API socket and any env/file secrets), its state directory (the
// projection database and the server's Temporal data by default), the API
// socket's directory, the projection database and its journal files, the
// env/file secrets, and the owner's credential stores. dep may be nil for
// the deployment defaults; home is the owner's home directory.
func DeniedReads(dep *profile.Deployment, home string) []string {
	h := func(rel string) string { return filepath.Join(home, rel) }
	out := []string{
		h(".config/agent-loop"),
		h(".local/state/agent-loop"),
	}
	if dep != nil {
		if dep.Path != "" {
			out = append(out, filepath.Dir(dep.Path))
		}
		if dep.API.Socket != "" {
			out = append(out, filepath.Dir(dep.API.Socket))
		}
		if p := dep.Projection.Path; p != "" {
			out = append(out, p, p+"-wal", p+"-shm", p+"-journal")
		}
		if dep.Secrets.Path != "" {
			out = append(out, dep.Secrets.Path)
		}
	}
	out = append(out,
		h(".ssh"),
		h(".aws"),
		h(".config/gh"),
		h(".netrc"),
		h(".docker"),
		h(".kube"),
		h(".git-credentials"),
		h(".config/git/credentials"),
		h("Library/Keychains"),
	)
	var uniq []string
	for _, p := range out {
		p = filepath.Clean(p)
		if !slices.Contains(uniq, p) {
			uniq = append(uniq, p)
		}
	}
	return uniq
}
