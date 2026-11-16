package runners

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/profile"
	"github.com/mayusuk/agent-loop/internal/secrets"
)

// ErrNotSandboxed is returned, before anything starts, for a handler the
// Claude Code sandbox doesn't wrap on a stage whose runner is "local"
// (TECHNICAL-SPEC §7.2).
var ErrNotSandboxed = errors.New("the Claude Code sandbox doesn't wrap this handler; use a sandboxed handler or set runner = \"local-unsandboxed\"")

// UnsandboxedWarning is the warning for a local-unsandboxed run, in the
// words of TECHNICAL-SPEC §7.2.
const UnsandboxedWarning = "This handler runs with the owner's full user access: it can read the owner's files, reach any network address, and open the local API socket."

// DefaultKillGrace is how long a handler's process group has between
// SIGTERM and SIGKILL once its time limit is reached (work plan D14).
const DefaultKillGrace = 10 * time.Second

// InheritedEnv are the worker's environment variables a handler gets
// (work plan D12). Everything else it gets is set by the runner or named in
// RunSpec.Env or Local.PassEnv.
var InheritedEnv = []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "LC_CTYPE", "LC_MESSAGES", "TZ"}

// reservedEnv are the variables the runner sets itself. RunSpec.Env and
// PassEnv may not set them.
var reservedEnv = []string{
	"AGENT_LOOP_INPUT", "AGENT_LOOP_RESULT", "AGENT_LOOP_OUT", "AGENT_LOOP_CONTRACT",
	ClaudeSettingsEnv, "GH_TOKEN", "TMPDIR", "GIT_TERMINAL_PROMPT",
}

// Local is the v0.1 `local` runner (TECHNICAL-SPEC §7.2): it runs a handler
// on the host, in the run's tree, in its own process group, inside Claude
// Code's sandbox.
type Local struct {
	// DenyRead is the sandbox's denied reads (DeniedReads). Empty means
	// DeniedReads(nil, the user's home).
	DenyRead []string
	// Redactor masks the run's tokens in its log. Nil makes one per run.
	Redactor *secrets.Redactor
	// HelperCommand starts the credential-helper client; the socket and
	// git's operation are appended. Empty means this binary's
	// CredentialHelperCommand.
	HelperCommand []string
	// TempRoot holds the run directories. Empty means os.TempDir(). Keep it
	// short: a Unix socket path is limited to about 100 bytes.
	TempRoot string
	// PassEnv names more worker environment variables a handler gets, for
	// example an API key its agent needs.
	PassEnv []string
	// KillGrace is the time between SIGTERM and SIGKILL. Zero means
	// DefaultKillGrace.
	KillGrace time.Duration
	// Logger gets the unsandboxed warning. Nil means slog.Default().
	Logger *slog.Logger
	// Getenv reads the worker's environment. Nil means os.LookupEnv.
	Getenv func(string) (string, bool)
}

// NewLocal returns a local runner whose sandbox denies the reads of
// DeniedReads for dep (nil for the defaults) and the user's home.
func NewLocal(dep *profile.Deployment, r *secrets.Redactor) (*Local, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &Local{DenyRead: DeniedReads(dep, home), Redactor: r}, nil
}

var _ ports.Runner = (*Local)(nil)

// Run starts the handler and waits for it to end or for its time limit
// (TECHNICAL-SPEC §7.1–§7.3, INTEGRATION-SPEC §3.1). The outcome's exit
// code is informational: ReadResult decides what the run returned. When the
// time limit is reached the whole process group is stopped and the outcome
// says TimedOut. When ctx ends first the group is stopped the same way and
// ctx's error is returned with the outcome. An error with no outcome means
// the handler never started.
func (l *Local) Run(ctx context.Context, s ports.RunSpec) (ports.RunOutcome, error) {
	sandboxed, err := l.check(s)
	if err != nil {
		return ports.RunOutcome{}, err
	}
	logf, err := os.OpenFile(s.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return ports.RunOutcome{}, fmt.Errorf("run log: %w", err)
	}
	defer logf.Close()
	red := l.Redactor
	if red == nil {
		red = secrets.NewRedactor()
	}
	log := red.Writer(logf)
	defer log.Flush()

	if err := os.MkdirAll(s.OutDir, 0o700); err != nil {
		return ports.RunOutcome{}, err
	}
	// A result left from an earlier run must never be read as this one's.
	if err := os.Remove(s.ResultPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return ports.RunOutcome{}, err
	}

	runDir, err := os.MkdirTemp(l.TempRoot, "al-")
	if err != nil {
		return ports.RunOutcome{}, err
	}
	defer os.RemoveAll(runDir)
	if runDir, err = filepath.EvalSymlinks(runDir); err != nil {
		return ports.RunOutcome{}, err
	}
	tmp := filepath.Join(runDir, "tmp")
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return ports.RunOutcome{}, err
	}
	socket := filepath.Join(runDir, "git.sock")

	// The credential helper lives as long as the run.
	srvCtx, stopSrv := context.WithCancel(context.WithoutCancel(ctx))
	srv, err := serveCredentials(srvCtx, socket, s.RepoURL, s.Push, red)
	if err != nil {
		stopSrv()
		return ports.RunOutcome{}, err
	}
	defer func() { stopSrv(); srv.close() }()
	if err := l.configureHelper(ctx, s.Dir.Path, socket); err != nil {
		return ports.RunOutcome{}, err
	}

	env, err := l.env(ctx, s, tmp, red)
	if err != nil {
		return ports.RunOutcome{}, err
	}
	if sandboxed {
		path, err := l.writeSettings(s, runDir, tmp, socket)
		if err != nil {
			return ports.RunOutcome{}, err
		}
		env = append(env, ClaudeSettingsEnv+"="+path)
	} else {
		l.logger().Warn(UnsandboxedWarning, "run_id", s.RunID, "runner", profile.RunnerLocalUnsandboxed)
		fmt.Fprintf(log, "agent-loop: warning: %s\n", UnsandboxedWarning)
	}

	return l.start(ctx, s, env, log)
}

// check validates the spec and applies the sandbox rule. It reports
// whether the run is sandboxed.
func (l *Local) check(s ports.RunSpec) (bool, error) {
	if len(s.Command) == 0 {
		return false, errors.New("runner: empty command")
	}
	for name, p := range map[string]string{"workspace": s.Dir.Path, "input": s.InputPath, "result": s.ResultPath, "out dir": s.OutDir, "log": s.LogPath} {
		if !filepath.IsAbs(p) {
			return false, fmt.Errorf("runner: %s path %q is not absolute", name, p)
		}
	}
	if s.TimeLimit <= 0 {
		return false, errors.New("runner: no time limit")
	}
	switch s.Runner {
	case "", profile.RunnerLocal:
		if !Wrapped(s.Command) {
			return false, fmt.Errorf("runner: %q: %w", s.Command[0], ErrNotSandboxed)
		}
		return true, nil
	case profile.RunnerLocalUnsandboxed:
		return false, nil
	}
	return false, fmt.Errorf("runner: the local runner can't run a stage with runner %q", s.Runner)
}

func (l *Local) logger() *slog.Logger {
	if l.Logger != nil {
		return l.Logger
	}
	return slog.Default()
}

func (l *Local) getenv(k string) (string, bool) {
	if l.Getenv != nil {
		return l.Getenv(k)
	}
	return os.LookupEnv(k)
}

// env builds the handler's environment from scratch (work plan D12): the
// inherited names, PassEnv, the spec's Env, and the contract's variables.
// No AGENT_LOOP_SECRET_* variable ever reaches a handler (secrets D12).
func (l *Local) env(ctx context.Context, s ports.RunSpec, tmp string, red *secrets.Redactor) ([]string, error) {
	vars := map[string]string{}
	for _, k := range slices.Concat(InheritedEnv, l.PassEnv) {
		if err := allowedName(k); err != nil {
			return nil, fmt.Errorf("runner: PassEnv: %w", err)
		}
		if v, ok := l.getenv(k); ok {
			vars[k] = v
		}
	}
	for k, v := range s.Env {
		if err := allowedName(k); err != nil {
			return nil, fmt.Errorf("runner: RunSpec.Env: %w", err)
		}
		vars[k] = v
	}
	vars["AGENT_LOOP_INPUT"] = s.InputPath
	vars["AGENT_LOOP_RESULT"] = s.ResultPath
	vars["AGENT_LOOP_OUT"] = s.OutDir
	vars["AGENT_LOOP_CONTRACT"] = contract.Version
	vars["TMPDIR"] = tmp
	vars["GIT_TERMINAL_PROMPT"] = "0"
	if s.Read != nil {
		tok, err := s.Read.Token(ctx)
		if err != nil {
			return nil, fmt.Errorf("runner: read token: %w", err)
		}
		red.Add(tok.Secret(), tok.ExpiresAt)
		vars["GH_TOKEN"] = tok.Secret()
	}
	env := make([]string, 0, len(vars))
	for k, v := range vars {
		env = append(env, k+"="+v)
	}
	slices.Sort(env)
	return env, nil
}

// allowedName refuses a variable the runner sets itself, and any secret of
// the env/file backend.
func allowedName(k string) error {
	if k == "" || strings.ContainsAny(k, "=\x00") {
		return fmt.Errorf("invalid variable name %q", k)
	}
	if strings.HasPrefix(k, "AGENT_LOOP_SECRET_") {
		return fmt.Errorf("%s is a secret and never reaches a handler", k)
	}
	if slices.Contains(reservedEnv, k) {
		return fmt.Errorf("%s is set by the runner", k)
	}
	return nil
}

// configureHelper makes git in the run's tree use only the run's
// credential helper: an empty entry first clears every helper from the
// owner's global or system config.
func (l *Local) configureHelper(ctx context.Context, dir, socket string) error {
	cmd := l.HelperCommand
	if len(cmd) == 0 {
		self, err := os.Executable()
		if err != nil {
			return err
		}
		cmd = []string{self, CredentialHelperCommand}
	}
	quoted := make([]string, 0, len(cmd)+1)
	for _, a := range append(slices.Clone(cmd), socket) {
		quoted = append(quoted, shellQuote(a))
	}
	helper := "!" + strings.Join(quoted, " ")
	git := func(args ...string) error {
		c := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "config", "--local"}, args...)...)
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, "GIT_") {
				c.Env = append(c.Env, kv)
			}
		}
		c.Env = append(c.Env, "GIT_CONFIG_NOSYSTEM=1")
		if out, err := c.CombinedOutput(); err != nil {
			return fmt.Errorf("runner: git config: %w: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if err := git("--replace-all", "credential.helper", ""); err != nil {
		return err
	}
	return git("--add", "credential.helper", helper)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// writeSettings writes the run's Claude Code settings next to the socket,
// outside every path the sandbox may write, and returns its path.
func (l *Local) writeSettings(s ports.RunSpec, runDir, tmp, socket string) (string, error) {
	real := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		// The result file doesn't exist yet: resolve its directory.
		if r, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
			return filepath.Join(r, filepath.Base(p))
		}
		return p
	}
	denyRead := l.DenyRead
	if len(denyRead) == 0 {
		// A runner built without NewLocal still denies the defaults.
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		denyRead = DeniedReads(nil, home)
	}
	var deny []string
	for _, p := range denyRead {
		deny = append(deny, p)
		if r := real(p); r != p {
			deny = append(deny, r)
		}
	}
	data, err := Settings(SandboxPaths{
		Worktree:   real(s.Dir.Path),
		OutDir:     real(s.OutDir),
		ResultPath: real(s.ResultPath),
		TempDir:    tmp,
		Socket:     socket,
		DenyRead:   deny,
		Egress:     s.Egress,
	})
	if err != nil {
		return "", err
	}
	path := filepath.Join(runDir, "claude-settings.json")
	if err := os.WriteFile(path, data, 0o400); err != nil {
		return "", err
	}
	return path, nil
}

// start runs the handler in its own process group and stops the group at
// the time limit or when ctx ends.
func (l *Local) start(ctx context.Context, s ports.RunSpec, env []string, log io.Writer) (ports.RunOutcome, error) {
	grace := l.KillGrace
	if grace <= 0 {
		grace = DefaultKillGrace
	}
	// stdout and stderr go to one pipe the runner copies to the log, so
	// waiting for the handler doesn't wait for processes it left behind.
	pr, pw, err := os.Pipe()
	if err != nil {
		return ports.RunOutcome{}, err
	}
	copied := make(chan struct{})
	go func() {
		io.Copy(log, pr)
		close(copied)
	}()
	defer func() {
		// Processes outside the group (setsid) may still hold the pipe.
		select {
		case <-copied:
		case <-time.After(grace):
		}
		pr.Close()
		<-copied
	}()

	cmd := exec.Command(s.Command[0], s.Command[1:]...)
	cmd.Dir = s.Dir.Path
	cmd.Env = env
	cmd.Stdin = nil // /dev/null: reads see end of input at once
	cmd.Stdout, cmd.Stderr = pw, pw
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	out := ports.RunOutcome{ResultPath: s.ResultPath, Started: time.Now()}
	err = cmd.Start()
	pw.Close()
	if err != nil {
		return ports.RunOutcome{}, fmt.Errorf("runner: start %q: %w", s.Command[0], err)
	}
	pgid := cmd.Process.Pid

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	limit := time.NewTimer(s.TimeLimit)
	defer limit.Stop()

	var werr, ctxErr error
	select {
	case werr = <-done:
	case <-limit.C:
		out.TimedOut = true
		werr = stopGroup(pgid, grace, done)
	case <-ctx.Done():
		ctxErr = ctx.Err()
		werr = stopGroup(pgid, grace, done)
	}
	// Whatever the handler left running in its group goes with it.
	syscall.Kill(-pgid, syscall.SIGKILL)
	out.Ended = time.Now()
	out.ExitCode = exitCode(cmd, werr)
	if out.TimedOut {
		// Whatever the handler did on SIGTERM, it was stopped (D14).
		out.ExitCode = -1
	}
	if ctxErr != nil {
		return out, fmt.Errorf("runner: stopped: %w", ctxErr)
	}
	return out, nil
}

// stopGroup sends SIGTERM to the process group, then SIGKILL after grace,
// and returns the handler's wait error.
func stopGroup(pgid int, grace time.Duration, done <-chan error) error {
	syscall.Kill(-pgid, syscall.SIGTERM)
	t := time.NewTimer(grace)
	defer t.Stop()
	select {
	case err := <-done:
		return err
	case <-t.C:
	}
	syscall.Kill(-pgid, syscall.SIGKILL)
	return <-done
}

// exitCode is the handler's exit code, or -1 when a signal ended it.
func exitCode(cmd *exec.Cmd, err error) int {
	if cmd.ProcessState != nil {
		return cmd.ProcessState.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}
