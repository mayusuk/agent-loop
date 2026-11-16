package profile

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"testing"
)

func home(t *testing.T) string {
	t.Helper()
	h, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	return h
}

func writeDeployment(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "deployment.toml")
	writeFile(t, path, content)
	return path
}

func TestD14_DeploymentDefaults(t *testing.T) {
	h := home(t)
	d, err := LoadDeployment(writeDeployment(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	backend := SecretsEnvFile
	secretsPath := filepath.Join(h, ".config/agent-loop/secrets.env")
	if runtime.GOOS == "darwin" {
		backend, secretsPath = SecretsKeychain, ""
	}
	checks := []struct {
		key       string
		got, want any
	}{
		{"temporal.address", d.Temporal.Address, "localhost:7233"},
		{"temporal.namespace", d.Temporal.Namespace, "default"},
		{"projection.path", d.Projection.Path, filepath.Join(h, ".local/state/agent-loop/projection.db")},
		{"api.socket", d.API.Socket, filepath.Join(h, ".config/agent-loop/api.sock")},
		{"api.gateway.enabled", d.API.Gateway.Enabled, false},
		{"api.gateway.address", d.API.Gateway.Address, "127.0.0.1:7480"},
		{"notifiers", d.Notifiers, []string{"desktop", "tracker"}},
		{"secrets.backend", d.Secrets.Backend, backend},
		{"secrets.path", d.Secrets.Path, secretsPath},
		{"limits.agent_runs", d.Limits.AgentRuns, 4},
		{"projects", len(d.Projects), 0},
	}
	for _, c := range checks {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s = %#v, want %#v", c.key, c.got, c.want)
		}
	}
	if dd := DefaultDeployment(); dd.Limits.AgentRuns != 4 || dd.Temporal != d.Temporal {
		t.Errorf("DefaultDeployment = %+v", dd)
	}
	p, err := DefaultDeploymentPath()
	if err != nil || p != filepath.Join(h, ".config/agent-loop/deployment.toml") {
		t.Errorf("DefaultDeploymentPath = %q, %v", p, err)
	}
}

func TestD14_DeploymentOverrides(t *testing.T) {
	h := home(t)
	path := writeDeployment(t, `
notifiers = ["desktop"]

[temporal]
address = "temporal.internal:7233"
namespace = "agent-loop"

[projection]
path = "state/projection.db"

[api]
socket = "~/run/agent-loop.sock"

[api.gateway]
enabled = true
address = "[::1]:9000"
token = "secret://agent-loop/gateway"

[secrets]
backend = "envfile"
path = "/etc/agent-loop/secrets.env"

[limits]
agent_runs = 8
`)
	d, err := LoadDeployment(path)
	if err != nil {
		t.Fatal(err)
	}
	want := &Deployment{
		Path:       path,
		Temporal:   Temporal{Address: "temporal.internal:7233", Namespace: "agent-loop"},
		Projection: Projection{Path: filepath.Join(filepath.Dir(path), "state/projection.db")},
		API: API{
			Socket:  filepath.Join(h, "run/agent-loop.sock"),
			Gateway: Gateway{Enabled: true, Address: "[::1]:9000", Token: "secret://agent-loop/gateway"},
		},
		Notifiers: []string{"desktop"},
		Secrets:   Secrets{Backend: SecretsEnvFile, Path: "/etc/agent-loop/secrets.env"},
		Limits:    DeploymentLimits{AgentRuns: 8},
		Projects:  map[string]Project{},
	}
	if !reflect.DeepEqual(d, want) {
		t.Errorf("got  %+v\nwant %+v", d, want)
	}
}

func TestD14_DeploymentProblems(t *testing.T) {
	cases := []struct{ name, content, key, msg string }{
		{"unknown top-level key", "temporal_address = \"x:1\"", "temporal_address", "unknown key"},
		{"unknown nested key", "[temporal]\nhost = \"x\"", "temporal.host", "unknown key"},
		{"bad temporal address", "[temporal]\naddress = \"localhost\"", "temporal.address", "address such as"},
		{"gateway not loopback", "[api.gateway]\naddress = \"0.0.0.0:7480\"", "api.gateway.address", "not a loopback address"},
		{"gateway bad port", "[api.gateway]\naddress = \"127.0.0.1:99999\"", "api.gateway.address", "not a loopback address"},
		{"gateway without token", "[api.gateway]\nenabled = true", "api.gateway.token", "required when the gateway is enabled"},
		{"gateway bad token", "[api.gateway]\ntoken = \"hunter2\"", "api.gateway.token", "secret reference"},
		{"unknown notifier", "notifiers = [\"slack\"]", "notifiers[0]", "one of"},
		{"no notifiers", "notifiers = []", "notifiers", "minItems"},
		{"unknown secrets backend", "[secrets]\nbackend = \"vault\"", "secrets.backend", "one of"},
		{"secrets path with keychain", "[secrets]\nbackend = \"keychain\"\npath = \"/x\"", "secrets.path", "only used by the \"envfile\" backend"},
		{"agent_runs 0", "[limits]\nagent_runs = 0", "limits.agent_runs", "minimum"},
		{"agent_runs 101", "[limits]\nagent_runs = 101", "limits.agent_runs", "maximum"},
		{"project without profile", "[projects.web]", "projects.web.profile", "required"},
		{"project unknown key", "[projects.web]\nprofile = \"p\"\nrepo = \"x\"", "projects.web.repo", "unknown key"},
		{"bad project name", "[projects.\"-web\"]\nprofile = \"p\"", "projects.-web", "unknown key"},
		{"invalid TOML", "[temporal", "", "invalid TOML"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeDeployment(t, c.content+"\n")
			_, err := LoadDeployment(path)
			wantProblem(t, err, path, c.key, c.msg)
		})
	}
	for _, ok := range []string{"[api.gateway]\naddress = \"localhost:1\"", "[limits]\nagent_runs = 1", "[limits]\nagent_runs = 100"} {
		if _, err := LoadDeployment(writeDeployment(t, ok+"\n")); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
}

// TestFR1AC1_ProjectsAreRegisteredInTheDeploymentConfig registers projects
// with [projects.<name>] profile = "<path>" and loads them.
func TestFR1AC1_ProjectsAreRegisteredInTheDeploymentConfig(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repos", "web")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	webProfile := filepath.Join(repo, ".agent-loop.toml")
	writeFile(t, webProfile, minimalTables+"\n[limits]\nagent_runs = 3\n")
	absProfile := filepath.Join(dir, "api.toml")
	writeFile(t, absProfile, minimalTables)

	depPath := filepath.Join(dir, "deployment.toml")
	writeFile(t, depPath, `
[limits]
agent_runs = 3

[projects.web]
profile = "repos/web/.agent-loop.toml"

[projects."api.v2"]
profile = "`+absProfile+`"
`)
	d, err := LoadDeployment(depPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.ProjectNames(); !slices.Equal(got, []string{"api.v2", "web"}) {
		t.Errorf("ProjectNames = %v", got)
	}
	if got := d.Projects["web"]; got != (Project{Name: "web", Profile: webProfile}) {
		t.Errorf("projects.web = %+v", got)
	}
	ps, err := d.LoadProjects()
	if err != nil {
		t.Fatal(err)
	}
	if ps["web"].Limits.AgentRuns != 3 || ps["api.v2"].Limits.AgentRuns != 2 {
		t.Errorf("agent_runs: web %d, api.v2 %d", ps["web"].Limits.AgentRuns, ps["api.v2"].Limits.AgentRuns)
	}

	// A profile over the deployment limit, and one that is missing, are
	// both reported, each naming its own file.
	writeFile(t, webProfile, minimalTables+"\n[limits]\nagent_runs = 4\n")
	if err := os.Remove(absProfile); err != nil {
		t.Fatal(err)
	}
	ps, err = d.LoadProjects()
	wantProblem(t, err, webProfile, "limits.agent_runs", "more than the deployment limit of 3")
	wantProblem(t, err, absProfile, "")
	if len(ps) != 0 {
		t.Errorf("loaded %d profiles despite errors", len(ps))
	}
}

func TestD15_TildeProfilePath(t *testing.T) {
	h := home(t)
	d, err := LoadDeployment(writeDeployment(t, "[projects.web]\nprofile = \"~/code/web/.agent-loop.toml\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Projects["web"].Profile; got != filepath.Join(h, "code/web/.agent-loop.toml") {
		t.Errorf("profile = %q", got)
	}
}

func TestErrorText(t *testing.T) {
	e := &Error{Problems: []Problem{{File: "a.toml", Key: "wip", Message: "bad"}, {File: "a.toml", Message: "worse"}}}
	if got := e.Error(); got != "a.toml: wip: bad\na.toml: worse" {
		t.Errorf("Error() = %q", got)
	}
}
