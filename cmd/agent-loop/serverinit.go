package main

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
	"github.com/mayusuk/agent-loop/internal/profile"
)

//go:embed compose/compose.yaml
var composeFile []byte

//go:embed compose/dynamicconfig.yaml
var dynamicConfigFile []byte

// runDocker runs docker with args; tests replace it.
var runDocker = func(stdout, stderr io.Writer, args ...string) error {
	cmd := exec.Command("docker", args...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	return cmd.Run()
}

func runServer(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usageErr(stderr, "server init|start [--config <path>]")
	}
	switch args[0] {
	case "init":
		return runServerInit(args[1:], stdout, stderr)
	case "start":
		return runServerStart(args[1:], stdout, stderr)
	}
	return usageErr(stderr, "server init|start [--config <path>]")
}

// deploymentTemplate is the deployment config server init writes: every
// default spelled out, so the owner sees what to change (work plan D10).
const deploymentTemplate = `# agent-loop deployment config (TECHNICAL-SPEC §13), written by
# agent-loop server init. agent-loop init registers projects below.

[temporal]
address   = %s
namespace = %s

[projection]
path = %s

[api]
socket = %s

[api.gateway]
enabled = false

[secrets]
backend = %s

[projects]
`

// runServerInit writes the deployment config, unless one exists, and the
// Temporal Compose files next to it, then starts Temporal with Docker
// Compose (TECHNICAL-SPEC §11, T2). It doesn't start the server.
func runServerInit(args []string, stdout, stderr io.Writer) int {
	fl, cfg := newFlags("server init", stderr)
	noStart := fl.Bool("no-start", false, "write the files but don't start Temporal")
	if fl.Parse(args) != nil || fl.NArg() != 0 {
		return usageErr(stderr, "server init [--config <path>] [--no-start]")
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "agent-loop server init: %v\n", err)
		return 1
	}
	path := *cfg
	if path == "" {
		p, err := profile.DefaultDeploymentPath()
		if err != nil {
			return fail(err)
		}
		path = p
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fail(err)
	}
	switch _, err := os.Stat(path); {
	case errors.Is(err, fs.ErrNotExist):
		d := profile.DefaultDeployment()
		vals := []string{d.Temporal.Address, d.Temporal.Namespace,
			pickProjectionPath(dir, stdout), profile.DefaultAPISocket, d.Secrets.Backend}
		enc := make([]any, len(vals))
		for i, v := range vals {
			q, err := tomlString(v)
			if err != nil {
				return fail(err)
			}
			enc[i] = q
		}
		body := fmt.Sprintf(deploymentTemplate, enc...)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "Wrote %s\n", path)
	case err != nil:
		return fail(err)
	default:
		fmt.Fprintf(stdout, "Kept %s, which already exists\n", path)
	}
	dep, err := profile.LoadDeployment(path)
	if err != nil {
		return fail(err)
	}
	if err := usableDir(filepath.Dir(dep.Projection.Path)); err != nil {
		return fail(fmt.Errorf("can't create or write to the projection database's directory: %w; set [projection] path in %s to a writable place", err, path))
	}
	fmt.Fprintf(stdout, "Projection database: %s\n", dep.Projection.Path)

	tdir := filepath.Join(dir, "temporal")
	if err := os.MkdirAll(filepath.Join(tdir, "dynamicconfig"), 0o700); err != nil {
		return fail(err)
	}
	compose := filepath.Join(tdir, "compose.yaml")
	if err := os.WriteFile(compose, composeFile, 0o600); err != nil {
		return fail(err)
	}
	if err := os.WriteFile(filepath.Join(tdir, "dynamicconfig", "agent-loop.yaml"), dynamicConfigFile, 0o600); err != nil {
		return fail(err)
	}
	env := filepath.Join(tdir, ".env")
	if _, err := os.Stat(env); errors.Is(err, fs.ErrNotExist) {
		pw := make([]byte, 24)
		if _, err := rand.Read(pw); err != nil {
			return fail(err)
		}
		if err := os.WriteFile(env, []byte("POSTGRES_PASSWORD="+hex.EncodeToString(pw)+"\n"), 0o600); err != nil {
			return fail(err)
		}
	} else if err != nil {
		return fail(err)
	}
	fmt.Fprintf(stdout, "Wrote %s\n", compose)
	if *noStart {
		fmt.Fprintf(stdout, "Start Temporal with: docker compose -f %s up -d\n", compose)
		return 0
	}
	if err := runDocker(stdout, stderr, "compose", "-f", compose, "up", "-d"); err != nil {
		return fail(fmt.Errorf("starting Temporal with Docker Compose: %w (is Docker running?)", err))
	}
	fmt.Fprintln(stdout, "Temporal is starting on 127.0.0.1:7233. Next: agent-loop init in your repository, then agent-loop server start.")
	return 0
}

// pickProjectionPath returns the projection path a new deployment config
// names: the first candidate whose directory server init can create and
// write to (work plan #103 D1). The default comes first:
// $XDG_STATE_HOME/agent-loop when XDG_STATE_HOME is an absolute path (work
// plan #113), otherwise ~/.local/state/agent-loop. Then on macOS
// ~/Library/Application Support/agent-loop, then the config directory
// cfgDir, which server init has already created. Each candidate skipped is
// reported on out.
func pickProjectionPath(cfgDir string, out io.Writer) string {
	first := profile.DefaultProjectionPath
	if x := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(x) {
		first = filepath.Join(x, "agent-loop", "projection.db")
	}
	cands := []string{first}
	if runtime.GOOS == "darwin" {
		cands = append(cands, "~/Library/Application Support/agent-loop/projection.db")
	}
	if abs, err := filepath.Abs(cfgDir); err == nil {
		cfgDir = abs
	}
	last := filepath.Join(cfgDir, "projection.db")
	home, herr := os.UserHomeDir()
	for _, c := range cands {
		abs := c
		if strings.HasPrefix(c, "~/") {
			if herr != nil {
				continue // needs the home directory; an absolute candidate doesn't (#133)
			}
			abs = filepath.Join(home, strings.TrimPrefix(c, "~/"))
		}
		err := usableDir(filepath.Dir(abs))
		if err == nil {
			return c
		}
		fmt.Fprintf(out, "Can't create or write to %s (%v); trying another place for the projection database\n", filepath.Dir(abs), err)
	}
	return last
}

// tomlString returns s as a TOML basic string, quoted and escaped by the
// TOML encoder internal/profile's decoder pairs with. Go's %q is not TOML:
// it writes a byte such as 0x7f as \x7f, which TOML rejects (#133). A TOML
// file must be valid UTF-8, so s must be too (work plan #133 D1).
func tomlString(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("%q is not valid UTF-8, so it can't be written to the deployment config; use a path that is", s)
	}
	b, err := toml.Marshal(struct {
		V string `toml:"v"`
	}{s})
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(strings.TrimPrefix(string(b), "v = "), "\n"), nil
}

// usableDir creates dir (mode 0700) if it is missing and checks that a file
// can be created in it (work plan #103 D2). os.MkdirAll alone returns nil for
// an existing directory the user can't write to, such as one left owned by
// root after a sudo run.
func usableDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}
