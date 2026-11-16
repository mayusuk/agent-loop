package runners

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/profile"
	"github.com/mayusuk/agent-loop/internal/secrets"
)

// The test binary is also the fake handler and the credential-helper
// client, so no test needs claude, GitHub or the network.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case CredentialHelperCommand:
			os.Exit(CredentialHelperMain(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
		case "fake-handler":
			os.Exit(fakeHandler(os.Args[2:]))
		}
	}
	os.Exit(m.Run())
}

// fakeHandler is a scripted handler. args[0] is the mode.
func fakeHandler(args []string) int {
	self, _ := os.Executable()
	spawn := func() int {
		c := exec.Command(self, "fake-handler", "sleep")
		if err := c.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(9)
		}
		return c.Process.Pid
	}
	writePids := func(path string, pids ...int) {
		var b strings.Builder
		for _, p := range pids {
			fmt.Fprintln(&b, p)
		}
		os.WriteFile(path, []byte(b.String()), 0o644)
	}
	switch args[0] {
	case "sleep":
		time.Sleep(time.Hour)
	case "hang": // hang <pidfile>: a child, then never exit
		writePids(args[1], os.Getpid(), spawn())
		time.Sleep(time.Hour)
	case "ignore-term": // ignore-term <pidfile>
		signal.Ignore(syscall.SIGTERM)
		writePids(args[1], os.Getpid(), spawn())
		time.Sleep(time.Hour)
	case "term-exit0": // term-exit0 <pidfile>: exit 0 on SIGTERM
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM)
		writePids(args[1], os.Getpid())
		<-ch
		return 0
	case "background": // background <pidfile>: leave a child behind and exit
		writePids(args[1], spawn())
		return 0
	case "result": // result <exit code> <result.json body, or "-" for none>
		if args[2] != "-" {
			os.WriteFile(os.Getenv("AGENT_LOOP_RESULT"), []byte(args[2]), 0o644)
		}
		code, _ := strconv.Atoi(args[1])
		return code
	case "env": // env <file>: the environment, the working directory, stdin
		in, err := io.ReadAll(os.Stdin)
		wd, _ := os.Getwd()
		var b strings.Builder
		fmt.Fprintf(&b, "WD=%s\nSTDIN=%q %v\n", wd, in, err)
		for _, kv := range os.Environ() {
			fmt.Fprintln(&b, kv)
		}
		os.WriteFile(args[1], []byte(b.String()), 0o644)
	case "cred": // cred <file> <host>: ask git for credentials, print them too
		c := exec.Command("git", "credential", "fill")
		c.Stdin = strings.NewReader("protocol=https\nhost=" + args[2] + "\npath=o/r.git\n\n")
		out, err := c.CombinedOutput()
		os.WriteFile(args[1], []byte(fmt.Sprintf("%s\nerr=%v\n", out, err)), 0o644)
		os.Stdout.Write(out)
	case "print": // print <text>: to stdout and stderr
		fmt.Println(args[1])
		fmt.Fprintln(os.Stderr, args[1])
	}
	return 0
}

// fixture is one run's paths, with a git repository as the run's tree.
type fixture struct {
	t        *testing.T
	dir      string // a scratch directory for the test
	wrapped  string // a symlink named agent-loop-claude to the test binary
	self     string
	worktree string
	spec     ports.RunSpec
	input    contract.Input
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wrapped := filepath.Join(dir, "bin", "agent-loop-claude")
	os.MkdirAll(filepath.Dir(wrapped), 0o755)
	if err := os.Symlink(self, wrapped); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(dir, "wt")
	if out, err := exec.Command("git", "init", "--quiet", wt).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	f := &fixture{t: t, dir: dir, wrapped: wrapped, self: self, worktree: wt}
	f.input = contract.Input{ContractVersion: "1.0", RunID: "item/p/42#1:implement:1:a", Stage: contract.StageImplement, Attempt: 1}
	in, _ := json.Marshal(f.input)
	os.WriteFile(filepath.Join(dir, "input.json"), in, 0o400)
	f.spec = ports.RunSpec{
		RunID:      f.input.RunID,
		Dir:        ports.Dir{Path: wt, Branch: "agent/42"},
		RepoURL:    "https://github.com/o/r",
		InputPath:  filepath.Join(dir, "input.json"),
		ResultPath: filepath.Join(dir, "result.json"),
		OutDir:     filepath.Join(dir, "out"),
		LogPath:    filepath.Join(dir, "run.log"),
		// A race-enabled fake handler otherwise sleeps 1s at exit.
		Env:       map[string]string{"GORACE": "atexit_sleep_ms=0"},
		TimeLimit: time.Minute,
		Egress:    []string{"api.anthropic.com", "github.com"},
	}
	return f
}

// cmd is a sandboxed fake-handler command.
func (f *fixture) cmd(args ...string) []string {
	return append([]string{f.wrapped, "fake-handler"}, args...)
}

func (f *fixture) log() string {
	b, _ := os.ReadFile(f.spec.LogPath)
	return string(b)
}

func newLocal() *Local {
	return &Local{DenyRead: DeniedReads(nil, "/home/owner"), KillGrace: 500 * time.Millisecond}
}

func readPids(t *testing.T, path string, n int) []int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ := os.ReadFile(path)
		var pids []int
		for _, f := range strings.Fields(string(b)) {
			p, _ := strconv.Atoi(f)
			pids = append(pids, p)
		}
		if len(pids) == n {
			return pids
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid file %s: %q", path, b)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// gone waits until no process has pid.
func gone(pid int) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestNFR7AC1_HandlerThatNeverExitsIsStoppedWithItsChildren(t *testing.T) {
	for _, mode := range []string{"hang", "ignore-term"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			pidfile := filepath.Join(f.dir, "pids")
			f.spec.Command = f.cmd(mode, pidfile)
			f.spec.TimeLimit = time.Second
			start := time.Now()
			out, err := newLocal().Run(context.Background(), f.spec)
			if err != nil {
				t.Fatal(err)
			}
			if !out.TimedOut {
				t.Errorf("TimedOut = false")
			}
			if took := time.Since(start); took > 4*time.Second {
				t.Errorf("Run took %v for a 1s limit and 0.5s grace", took)
			}
			for _, pid := range readPids(t, pidfile, 2) {
				if !gone(pid) {
					t.Errorf("process %d still runs", pid)
				}
			}
			if out.ExitCode != -1 {
				t.Errorf("exit code = %d, want -1 (signalled)", out.ExitCode)
			}
		})
	}
}

// TestTimedOutRunHasExitCodeMinusOne: a handler that traps SIGTERM and
// exits 0 at its time limit still reports -1 (work plan D14, review R2).
func TestTimedOutRunHasExitCodeMinusOne(t *testing.T) {
	f := newFixture(t)
	f.spec.Command = f.cmd("term-exit0", filepath.Join(f.dir, "pids"))
	f.spec.TimeLimit = time.Second
	out, err := newLocal().Run(context.Background(), f.spec)
	if err != nil {
		t.Fatal(err)
	}
	if !out.TimedOut || out.ExitCode != -1 {
		t.Errorf("outcome = %+v, want TimedOut and exit code -1", out)
	}
}

func TestProcessesLeftBehindAreStoppedWhenTheHandlerExits(t *testing.T) {
	f := newFixture(t)
	pidfile := filepath.Join(f.dir, "pids")
	f.spec.Command = f.cmd("background", pidfile)
	start := time.Now()
	out, err := newLocal().Run(context.Background(), f.spec)
	if err != nil {
		t.Fatal(err)
	}
	if out.TimedOut || out.ExitCode != 0 {
		t.Errorf("outcome = %+v", out)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("Run waited %v for a process left behind", took)
	}
	if pid := readPids(t, pidfile, 1)[0]; !gone(pid) {
		t.Errorf("process %d left behind still runs", pid)
	}
}

func TestCancelStopsTheGroup(t *testing.T) {
	f := newFixture(t)
	pidfile := filepath.Join(f.dir, "pids")
	f.spec.Command = f.cmd("hang", pidfile)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		readPids(t, pidfile, 2)
		cancel()
	}()
	out, err := newLocal().Run(ctx, f.spec)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if out.TimedOut || out.Started.IsZero() || out.Ended.Before(out.Started) {
		t.Errorf("outcome = %+v", out)
	}
	for _, pid := range readPids(t, pidfile, 2) {
		if !gone(pid) {
			t.Errorf("process %d still runs", pid)
		}
	}
}

func TestFR18AC3_MissingOrInvalidResultIsFailedWhateverTheExitCode(t *testing.T) {
	runID := "item/p/42#1:implement:1:a"
	valid := `{"contract_version":"1.0","run_id":"` + runID + `","result":"blocked","blocked":{"reason_file":"reason.txt"}}`
	cases := []struct {
		name   string
		exit   string
		body   string
		stale  bool   // a valid result.json from an earlier run is there before the run
		want   string // the outcome ReadResult returns
		reason string // in the failed reason
	}{
		{"missing, exit 0", "0", "-", false, "failed", "missing"},
		{"missing, exit 1", "1", "-", false, "failed", "missing"},
		{"stale result from an earlier run", "0", "-", true, "failed", "missing"},
		{"not JSON, exit 0", "0", "{not json", false, "failed", "invalid result"},
		{"not JSON, exit 2", "2", "{not json", false, "failed", "invalid result"},
		{"another run's result", "0", strings.Replace(valid, runID, "other", 1), false, "failed", "run_id"},
		{"not allowed for the stage", "0", `{"contract_version":"1.0","run_id":"` + runID + `","result":"complete","complete":{}}`, false, "failed", "complete"},
		{"empty, exit 0", "0", "", false, "failed", "invalid result"},
		{"valid, exit 0", "0", valid, false, "blocked", ""},
		{"valid, exit 7", "7", valid, false, "blocked", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			os.MkdirAll(f.spec.OutDir, 0o700)
			os.WriteFile(filepath.Join(f.spec.OutDir, "reason.txt"), []byte("needs #7\n"), 0o644)
			if c.stale {
				os.WriteFile(f.spec.ResultPath, []byte(valid), 0o644)
			}
			f.spec.Command = f.cmd("result", c.exit, c.body)
			out, err := newLocal().Run(context.Background(), f.spec)
			if err != nil {
				t.Fatal(err)
			}
			if want, _ := strconv.Atoi(c.exit); out.ExitCode != want {
				t.Errorf("exit code = %d, want %s", out.ExitCode, c.exit)
			}
			r, rerr := ReadResult(f.input, out, f.spec.OutDir)
			if string(r.Result) != c.want {
				t.Fatalf("result = %+v, want %s", r, c.want)
			}
			if c.want != "failed" {
				if rerr != nil {
					t.Errorf("error for a valid result: %v", rerr)
				}
				return
			}
			if rerr == nil || !errors.Is(rerr, contract.ErrInvalidResult) {
				t.Errorf("error = %v, want ErrInvalidResult", rerr)
			}
			if r.RunID != runID || r.ContractVersion != contract.Version || r.Failed == nil || !strings.Contains(r.Failed.Reason, c.reason) || strings.Contains(r.Failed.Reason, "\n") {
				t.Errorf("failed result = %+v, want a one-line reason with %q", r.Failed, c.reason)
			}
		})
	}
}

func TestFR18AC3_ResultThatIsNotARegularFileOrTooLargeIsFailed(t *testing.T) {
	in := contract.Input{RunID: "r", Stage: contract.StageImplement}
	dir := t.TempDir()
	big := filepath.Join(dir, "big.json")
	os.WriteFile(big, bytes.Repeat([]byte(" "), MaxResultFile+1), 0o644)
	link := filepath.Join(dir, "link.json")
	os.Symlink(big, link)
	fifo := filepath.Join(dir, "fifo.json")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{dir, big, link, fifo, ""} {
		r, err := ReadResult(in, ports.RunOutcome{ResultPath: p}, dir)
		if r.Result != contract.OutcomeFailed || err == nil {
			t.Errorf("%q: %+v, %v", p, r, err)
		}
	}
}

func TestHandlerEnvironment(t *testing.T) {
	f := newFixture(t)
	envFile := filepath.Join(f.dir, "env.txt")
	f.spec.Command = f.cmd("env", envFile)
	f.spec.Env["EXTRA"] = "yes"
	read := ports.NewToken("ghs_readtoken", ports.IdentityCoder, ports.Scope{}, time.Now().Add(time.Hour))
	f.spec.Read = ports.StaticToken(read)
	l := newLocal()
	l.PassEnv = []string{"ANTHROPIC_API_KEY"}
	worker := map[string]string{
		"PATH": os.Getenv("PATH"), "HOME": "/home/owner", "ANTHROPIC_API_KEY": "sk-x",
		"AGENT_LOOP_SECRET_GITHUB_APP_CODER_PRIVATE_KEY": "pem", "AWS_SECRET_ACCESS_KEY": "aws", "GIT_DIR": "/elsewhere",
	}
	l.Getenv = func(k string) (string, bool) { v, ok := worker[k]; return v, ok }
	if _, err := l.Run(context.Background(), f.spec); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			got[k] = v
		}
	}
	realWT, _ := filepath.EvalSymlinks(f.worktree)
	for k, want := range map[string]string{
		"WD": realWT, "STDIN": `"" <nil>`,
		"AGENT_LOOP_INPUT": f.spec.InputPath, "AGENT_LOOP_RESULT": f.spec.ResultPath, "AGENT_LOOP_OUT": f.spec.OutDir,
		"AGENT_LOOP_CONTRACT": "1.0", "GH_TOKEN": "ghs_readtoken", "GIT_TERMINAL_PROMPT": "0",
		"EXTRA": "yes", "ANTHROPIC_API_KEY": "sk-x", "HOME": "/home/owner",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	for k := range got {
		if strings.HasPrefix(k, "AGENT_LOOP_SECRET_") || k == "AWS_SECRET_ACCESS_KEY" || k == "GIT_DIR" {
			t.Errorf("the handler got %s", k)
		}
	}
	// The temp directory and the settings file are the run's, and both
	// are gone when the run ends.
	tmp, settings := got["TMPDIR"], got[ClaudeSettingsEnv]
	if filepath.Dir(tmp) != filepath.Dir(settings) || filepath.Base(tmp) != "tmp" {
		t.Errorf("TMPDIR %q and settings %q are not in one run directory", tmp, settings)
	}
	if _, err := os.Stat(filepath.Dir(tmp)); !os.IsNotExist(err) {
		t.Errorf("run directory left behind: %v", err)
	}
}

func TestHandlerEnvironmentRefusesSecretsAndReservedNames(t *testing.T) {
	for _, tc := range []struct {
		env  map[string]string
		pass []string
	}{
		{env: map[string]string{"AGENT_LOOP_SECRET_X": "v"}},
		{env: map[string]string{"GH_TOKEN": "v"}},
		{env: map[string]string{"AGENT_LOOP_RESULT": "/x"}},
		{env: map[string]string{ClaudeSettingsEnv: "/x"}},
		{env: map[string]string{"A=B": "v"}},
		{pass: []string{"AGENT_LOOP_SECRET_GITHUB_APP_CODER_APP_ID"}},
		{pass: []string{"TMPDIR"}},
	} {
		f := newFixture(t)
		marker := filepath.Join(f.dir, "ran")
		f.spec.Command = f.cmd("env", marker)
		for k, v := range tc.env {
			f.spec.Env[k] = v
		}
		l := newLocal()
		l.PassEnv = tc.pass
		if _, err := l.Run(context.Background(), f.spec); err == nil {
			t.Errorf("env %v, pass %v: accepted", tc.env, tc.pass)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Errorf("env %v, pass %v: the handler ran", tc.env, tc.pass)
		}
	}
}

func TestSandboxedRunGetsItsSettings(t *testing.T) {
	f := newFixture(t)
	// A shell script named like the wrapped handler copies its settings.
	script := filepath.Join(f.dir, "bin2", "agent-loop-claude")
	os.MkdirAll(filepath.Dir(script), 0o755)
	os.WriteFile(script, []byte("#!/bin/sh\ncp \"$"+ClaudeSettingsEnv+"\" \"$AGENT_LOOP_OUT/settings.json\"\n"), 0o755)
	f.spec.Command = []string{script, "implement"}
	if _, err := newLocal().Run(context.Background(), f.spec); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(f.spec.OutDir, "settings.json"))
	if err != nil {
		t.Fatalf("no settings were passed: %v\n%s", err, f.log())
	}
	var s struct {
		Sandbox struct {
			Enabled    bool
			Filesystem struct{ AllowWrite, DenyRead []string }
			Network    struct{ AllowedDomains, AllowUnixSockets []string }
		}
	}
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	realWT, _ := filepath.EvalSymlinks(f.worktree)
	realOut, _ := filepath.EvalSymlinks(f.spec.OutDir)
	if !s.Sandbox.Enabled || !slices.Contains(s.Sandbox.Filesystem.AllowWrite, realWT) || !slices.Contains(s.Sandbox.Filesystem.AllowWrite, realOut) {
		t.Errorf("settings = %s", data)
	}
	if !slices.Equal(s.Sandbox.Network.AllowedDomains, f.spec.Egress) || len(s.Sandbox.Network.AllowUnixSockets) != 1 || !strings.HasSuffix(s.Sandbox.Network.AllowUnixSockets[0], "/git.sock") {
		t.Errorf("network = %+v", s.Sandbox.Network)
	}
	if !slices.Contains(s.Sandbox.Filesystem.DenyRead, "/home/owner/.ssh") {
		t.Errorf("denyRead = %q", s.Sandbox.Filesystem.DenyRead)
	}

	// A runner made without NewLocal still denies the default reads.
	l := &Local{}
	os.Remove(filepath.Join(f.spec.OutDir, "settings.json"))
	if _, err := l.Run(context.Background(), f.spec); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(f.spec.OutDir, "settings.json"))
	home, _ := os.UserHomeDir()
	if !strings.Contains(string(data), `"`+filepath.Join(home, ".ssh")+`"`) {
		t.Errorf("a zero Local denies no reads: %s", data)
	}
}

func TestLocalRefusesAHandlerTheSandboxDoesNotWrap(t *testing.T) {
	f := newFixture(t)
	marker := filepath.Join(f.dir, "ran")
	f.spec.Command = []string{f.self, "fake-handler", "env", marker}
	for _, runner := range []string{"", "local"} {
		f.spec.Runner = runner
		if _, err := newLocal().Run(context.Background(), f.spec); !errors.Is(err, ErrNotSandboxed) {
			t.Errorf("runner %q: err = %v, want ErrNotSandboxed", runner, err)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("runner %q: the handler ran", runner)
		}
	}
	f.spec.Runner = "docker"
	if _, err := newLocal().Run(context.Background(), f.spec); err == nil {
		t.Error("the local runner accepted a docker stage")
	}
}

func TestLocalUnsandboxedRunsAndWarns(t *testing.T) {
	f := newFixture(t)
	envFile := filepath.Join(f.dir, "env.txt")
	f.spec.Command = []string{f.self, "fake-handler", "env", envFile}
	f.spec.Runner = "local-unsandboxed"
	var logged bytes.Buffer
	l := newLocal()
	l.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	if _, err := l.Run(context.Background(), f.spec); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal("the handler didn't run")
	}
	if strings.Contains(string(b), ClaudeSettingsEnv+"=") {
		t.Error("an unsandboxed run got sandbox settings")
	}
	if !strings.Contains(f.log(), UnsandboxedWarning) || !strings.Contains(logged.String(), UnsandboxedWarning) {
		t.Errorf("no warning: run log %q, logger %q", f.log(), logged.String())
	}
}

func TestRunSpecChecks(t *testing.T) {
	for name, mut := range map[string]func(*ports.RunSpec){
		"no command":        func(s *ports.RunSpec) { s.Command = nil },
		"relative result":   func(s *ports.RunSpec) { s.ResultPath = "result.json" },
		"relative log":      func(s *ports.RunSpec) { s.LogPath = "" },
		"relative worktree": func(s *ports.RunSpec) { s.Dir.Path = "wt" },
		"no time limit":     func(s *ports.RunSpec) { s.TimeLimit = 0 },
		"missing program":   func(s *ports.RunSpec) { s.Command = []string{"/nonexistent/agent-loop-claude"} },
		"no git repository": func(s *ports.RunSpec) { s.Dir.Path = s.OutDir },
	} {
		f := newFixture(t)
		f.spec.Command = f.cmd("print", "x")
		os.MkdirAll(f.spec.OutDir, 0o700)
		mut(&f.spec)
		if _, err := newLocal().Run(context.Background(), f.spec); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestCredentialHelperServesThePushTokenAndNothingForReview runs git in the
// handler, which asks the run's helper over its socket (INTEGRATION-SPEC
// §3.1, §3.4).
func TestCredentialHelperServesThePushTokenAndNothingForReview(t *testing.T) {
	const secret = "ghs_pushtoken0123456789abcdefghijklmnopq" // GitHub-shaped: 36 characters after ghs_
	push := ports.NewToken(secret, ports.IdentityCoder, ports.Scope{}, time.Now().Add(time.Hour))
	for _, c := range []struct {
		name string
		push ports.TokenSource
		host string
		want bool
	}{
		{"implement", ports.StaticToken(push), "github.com", true},
		{"review", nil, "github.com", false},
		{"another host", ports.StaticToken(push), "evil.example", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			out := filepath.Join(f.dir, "cred.txt")
			f.spec.Command = f.cmd("cred", out, c.host)
			f.spec.Push = c.push
			if _, err := newLocal().Run(context.Background(), f.spec); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(out)
			if err != nil {
				t.Fatalf("no output: %v\n%s", err, f.log())
			}
			got := string(b)
			if c.want != strings.Contains(got, "password="+secret) {
				t.Errorf("git credential fill gave %q", got)
			}
			if c.want && !strings.Contains(got, "username=x-access-token") {
				t.Errorf("username missing: %q", got)
			}
			if !c.want && strings.Contains(got, "password=") {
				t.Errorf("a credential was served: %q", got)
			}
			// The handler printed what it got: the log masks it.
			if strings.Contains(f.log(), secret) {
				t.Errorf("the run log holds the token: %q", f.log())
			}
		})
	}
}

func TestCredentialHelperServerRules(t *testing.T) {
	sock := filepath.Join(shortTemp(t), "s")
	push := ports.StaticToken(ports.NewToken("tok", ports.IdentityCoder, ports.Scope{}, time.Now().Add(time.Hour)))
	red := secrets.NewRedactor()
	srv, err := serveCredentials(context.Background(), sock, "https://GitHub.com/o/r", push, red)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.close()
	if st, err := os.Stat(sock); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("socket mode: %v %v", st, err)
	}
	ask := func(op, attrs string) string {
		var out, errb bytes.Buffer
		if code := CredentialHelperMain([]string{sock, op}, strings.NewReader(attrs), &out, &errb); code != 0 {
			t.Fatalf("exit %d: %s", code, errb.String())
		}
		return out.String()
	}
	if got := ask("get", "protocol=https\nhost=github.com\n\n"); got != "username=x-access-token\npassword=tok\n" {
		t.Errorf("get = %q", got)
	}
	for _, attrs := range []string{"protocol=http\nhost=github.com\n", "protocol=https\nhost=github.com.evil\n", "host=github.com\n", ""} {
		if got := ask("get", attrs); got != "" {
			t.Errorf("get %q = %q, want nothing", attrs, got)
		}
	}
	for _, op := range []string{"store", "erase"} {
		if got := ask(op, "protocol=https\nhost=github.com\npassword=x\n"); got != "" {
			t.Errorf("%s = %q", op, got)
		}
	}
	if red.Redact("tok") != secrets.Mask {
		t.Error("the served token isn't known to the redactor")
	}
	var errb bytes.Buffer
	if CredentialHelperMain([]string{sock}, nil, io.Discard, &errb) != 2 {
		t.Error("missing operation accepted")
	}
	if CredentialHelperMain([]string{filepath.Join(filepath.Dir(sock), "none"), "get"}, strings.NewReader(""), io.Discard, &errb) != 1 {
		t.Error("missing socket gave success")
	}

	// A repository that isn't https serves nothing.
	sock2 := filepath.Join(filepath.Dir(sock), "s2")
	srv2, err := serveCredentials(context.Background(), sock2, "/local/repo", push, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv2.close()
	var out bytes.Buffer
	CredentialHelperMain([]string{sock2, "get"}, strings.NewReader("protocol=https\nhost=github.com\n"), &out, &errb)
	if out.Len() != 0 {
		t.Errorf("served %q for a local repository", out.String())
	}
}

// shortTemp is a temp directory with a path short enough for a socket.
func shortTemp(t *testing.T) string {
	d, err := os.MkdirTemp("", "al-t-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func TestRunLogIsRedacted(t *testing.T) {
	f := newFixture(t)
	const secret = "ghs_readtoken0123456789abcdefghijklmnopq"
	f.spec.Read = ports.StaticToken(ports.NewToken(secret, ports.IdentityCoder, ports.Scope{}, time.Now().Add(time.Hour)))
	f.spec.Command = f.cmd("print", "token is "+secret+" and an unknown one ghp_"+strings.Repeat("a", 36))
	if _, err := newLocal().Run(context.Background(), f.spec); err != nil {
		t.Fatal(err)
	}
	log := f.log()
	if strings.Contains(log, secret) || strings.Contains(log, "ghp_aaaa") || strings.Count(log, "token is "+secrets.Mask) != 2 {
		t.Errorf("log = %q", log)
	}
	if st, _ := os.Stat(f.spec.LogPath); st.Mode().Perm() != 0o600 {
		t.Errorf("log mode %v", st.Mode())
	}
}

func TestNewLocalDeniesTheDeploymentsPaths(t *testing.T) {
	dep := &profile.Deployment{Projection: profile.Projection{Path: "/data/al/p.db"}, API: profile.API{Socket: "/run/al/api.sock"}}
	l, err := NewLocal(dep, nil)
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	for _, want := range []string{"/data/al/p.db", "/run/al", filepath.Join(home, ".ssh")} {
		if !slices.Contains(l.DenyRead, want) {
			t.Errorf("DenyRead lacks %s: %q", want, l.DenyRead)
		}
	}
}
