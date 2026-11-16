package profile

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
)

// Deployment defaults (D14).
const (
	DefaultTemporalAddress    = "localhost:7233"
	DefaultTemporalNamespace  = "default"
	DefaultProjectionPath     = "~/.local/state/agent-loop/projection.db"
	DefaultAPISocket          = "~/.config/agent-loop/api.sock"
	DefaultGatewayAddress     = "127.0.0.1:7480"
	DefaultSecretsPath        = "~/.config/agent-loop/secrets.env"
	DefaultDeploymentAgentRun = 4
	DefaultDeploymentPathRel  = ".config/agent-loop/deployment.toml"
)

var DefaultNotifiers = []string{"desktop", "tracker"}

// Secret-store backends.
const (
	SecretsKeychain = "keychain"
	SecretsEnvFile  = "envfile"
)

// Deployment is a loaded, validated deployment config with defaults filled in.
// Paths are absolute, with ~ expanded.
type Deployment struct {
	// Path is the file the config was loaded from. Empty for DefaultDeployment.
	Path string

	Temporal   Temporal
	Projection Projection
	API        API
	Notifiers  []string
	Secrets    Secrets
	Limits     DeploymentLimits

	// Projects maps each registered project's name to its profile path.
	Projects map[string]Project
}

type Temporal struct {
	Address   string
	Namespace string
}

type Projection struct {
	Path string
}

type API struct {
	Socket  string
	Gateway Gateway
}

type Gateway struct {
	Enabled bool
	Address string
	// Token is a secret reference for the bearer token (TECHNICAL-SPEC §11).
	Token string
}

type Secrets struct {
	Backend string
	// Path is the env file for the envfile backend, empty otherwise.
	Path string
}

type DeploymentLimits struct {
	// AgentRuns caps concurrent agent runs across the deployment (FR-16 AC2).
	AgentRuns int
}

// Project is a project registered in the deployment config.
type Project struct {
	Name string
	// Profile is the absolute path of the project's .agent-loop.toml.
	Profile string
}

type rawDeployment struct {
	Temporal struct {
		Address   *string `toml:"address"`
		Namespace *string `toml:"namespace"`
	} `toml:"temporal"`
	Projection struct {
		Path *string `toml:"path"`
	} `toml:"projection"`
	API struct {
		Socket  *string `toml:"socket"`
		Gateway struct {
			Enabled *bool   `toml:"enabled"`
			Address *string `toml:"address"`
			Token   string  `toml:"token"`
		} `toml:"gateway"`
	} `toml:"api"`
	Notifiers []string `toml:"notifiers"`
	Secrets   struct {
		Backend *string `toml:"backend"`
		Path    *string `toml:"path"`
	} `toml:"secrets"`
	Limits struct {
		AgentRuns *int `toml:"agent_runs"`
	} `toml:"limits"`
	Projects map[string]struct {
		Profile string `toml:"profile"`
	} `toml:"projects"`
}

// DefaultDeploymentPath is ~/.config/agent-loop/deployment.toml.
func DefaultDeploymentPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, DefaultDeploymentPathRel), nil
}

func defaultSecretsBackend() string {
	if runtime.GOOS == "darwin" {
		return SecretsKeychain
	}
	return SecretsEnvFile
}

// DefaultDeployment is the deployment config with every default and no
// projects. Paths are left unexpanded if the home directory is unknown.
func DefaultDeployment() *Deployment {
	ps := &problems{}
	var raw rawDeployment
	return buildDeployment(&raw, "", ps)
}

// LoadDeployment loads and validates the deployment config at path. On
// failure the error is an *Error listing every problem.
func LoadDeployment(path string) (*Deployment, error) {
	ps := &problems{file: path}
	var raw rawDeployment
	if !decodeFile(path, deploymentSchema, &raw, ps, nil) {
		return nil, ps.err()
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	d := buildDeployment(&raw, filepath.Dir(abs), ps)
	d.Path = path
	if err := ps.err(); err != nil {
		return nil, err
	}
	return d, nil
}

func buildDeployment(r *rawDeployment, dir string, ps *problems) *Deployment {
	d := &Deployment{
		Temporal: Temporal{
			Address:   str(r.Temporal.Address, DefaultTemporalAddress),
			Namespace: str(r.Temporal.Namespace, DefaultTemporalNamespace),
		},
		Projection: Projection{Path: expand(str(r.Projection.Path, DefaultProjectionPath), dir, "projection.path", ps)},
		API: API{
			Socket: expand(str(r.API.Socket, DefaultAPISocket), dir, "api.socket", ps),
			Gateway: Gateway{
				Enabled: r.API.Gateway.Enabled != nil && *r.API.Gateway.Enabled,
				Address: str(r.API.Gateway.Address, DefaultGatewayAddress),
				Token:   r.API.Gateway.Token,
			},
		},
		Notifiers: orDefault(r.Notifiers, DefaultNotifiers),
		Secrets:   Secrets{Backend: str(r.Secrets.Backend, defaultSecretsBackend())},
		Limits:    DeploymentLimits{AgentRuns: num(r.Limits.AgentRuns, DefaultDeploymentAgentRun)},
		Projects:  map[string]Project{},
	}

	if r.API.Gateway.Address != nil && !loopback(*r.API.Gateway.Address) {
		ps.add("api.gateway.address", fmt.Sprintf("%q is not a loopback address: the gateway listens only on 127.0.0.1, ::1 or localhost (TECHNICAL-SPEC §11)", *r.API.Gateway.Address))
	}
	if d.API.Gateway.Enabled && d.API.Gateway.Token == "" {
		ps.add("api.gateway.token", "required when the gateway is enabled (TECHNICAL-SPEC §11)")
	}

	switch d.Secrets.Backend {
	case SecretsEnvFile:
		d.Secrets.Path = expand(str(r.Secrets.Path, DefaultSecretsPath), dir, "secrets.path", ps)
	default:
		if r.Secrets.Path != nil {
			ps.add("secrets.path", fmt.Sprintf("only used by the %q backend, not %q", SecretsEnvFile, d.Secrets.Backend))
		}
	}

	for name, rp := range r.Projects {
		d.Projects[name] = Project{Name: name, Profile: expand(rp.Profile, dir, "projects."+name+".profile", ps)}
	}
	return d
}

func expand(p, dir, key string, ps *problems) string {
	out, err := resolvePath(p, dir)
	if err != nil {
		ps.add(key, "can't expand ~: "+err.Error())
		return p
	}
	return out
}

func loopback(addr string) bool {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ProjectNames returns the registered project names in order.
func (d *Deployment) ProjectNames() []string {
	names := make([]string, 0, len(d.Projects))
	for n := range d.Projects {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// LoadProjects loads every registered project's profile, bounded by this
// deployment's limits. It returns the profiles it could load and an *Error
// with every problem from the others.
func (d *Deployment) LoadProjects() (map[string]*Profile, error) {
	out := map[string]*Profile{}
	var all []Problem
	for _, name := range d.ProjectNames() {
		p, err := LoadProfile(d.Projects[name].Profile, d)
		if err != nil {
			var pe *Error
			if errors.As(err, &pe) {
				all = append(all, pe.Problems...)
				continue
			}
			return out, err
		}
		out[name] = p
	}
	if len(all) > 0 {
		return out, &Error{Problems: all}
	}
	return out, nil
}
