package runners

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
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
		case SuperviseCommand:
			os.Exit(SuperviseMain(os.Args[2:], os.Stderr))
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
	case "setsid": // setsid <pidfile> hang|linger|exit: a child in its own session
		c := exec.Command(self, "fake-handler", "setsid-child", args[1])
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := c.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 9
		}
		readPidsQuietly(args[1], 2)
		switch args[2] {
		case "hang":
			time.Sleep(time.Hour)
		case "linger":
			time.Sleep(300 * time.Millisecond)
		}
		return 0
	case "setsid-child": // setsid-child <pidfile>: a grandchild, then sleep
		writePids(args[1], os.Getpid(), spawn())
		time.Sleep(time.Hour)
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
	case "commit": // commit <file>: commit in the working directory, print its signature and global config
		c := exec.Command("git", "commit", "--quiet", "--allow-empty", "-m", "agent")
		out, err := c.CombinedOutput()
		sig, _ := exec.Command("git", "log", "-1", "--format=%an|%ae|%cn|%ce|%G?").Output()
		alias, _ := exec.Command("git", "config", "--get", "alias.co").Output()
		os.WriteFile(args[1], []byte(fmt.Sprintf("commit=%v %s\nsig=%s\nalias=%s\n", err, bytes.TrimSpace(out), bytes.TrimSpace(sig), bytes.TrimSpace(alias))), 0o644)
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

// readPidsQuietly waits up to 5s for n pids in path.
func readPidsQuietly(path string, n int) {
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		b, _ := os.ReadFile(path)
		if len(strings.Fields(string(b))) == n {
			return
		}
	}
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

// TestNFR7AC1_ProcessesThatLeftTheGroupAreStopped: a child that calls
// setsid, and its own child, are stopped with the handler (review R1, work
// plan D20): when the handler never exits, when it exits a while after
// starting them, and (Linux only, where the supervisor is the child
// subreaper) when it exits at once.
func TestNFR7AC1_ProcessesThatLeftTheGroupAreStopped(t *testing.T) {
	for _, mode := range []string{"hang", "linger", "exit"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "exit" && runtime.GOOS != "linux" {
				t.Skip("macOS has no child subreaper: an orphan the supervisor never saw escapes (work plan D20)")
			}
			f := newFixture(t)
			pidfile := filepath.Join(f.dir, "pids")
			f.spec.Command = f.cmd("setsid", pidfile, mode)
			f.spec.TimeLimit = 2 * time.Second
			start := time.Now()
			out, err := newLocal().Run(context.Background(), f.spec)
			if err != nil {
				t.Fatal(err)
			}
			if out.TimedOut != (mode == "hang") {
				t.Errorf("outcome = %+v", out)
			}
			if took := time.Since(start); took > 6*time.Second {
				t.Errorf("Run took %v", took)
			}
			for _, pid := range readPids(t, pidfile, 2) {
				if !gone(pid) {
					t.Errorf("process %d, outside the handler's group, still runs", pid)
					syscall.Kill(pid, syscall.SIGKILL)
				}
			}
		})
	}
}

func TestSuperviseUsage(t *testing.T) {
	var b bytes.Buffer
	for _, args := range [][]string{nil, {"10", "/bin/true", "x", "true"}, {"-1", "/bin/true", "--", "true"}, {"x", "/bin/true", "--", "true"}} {
		b.Reset()
		if code := SuperviseMain(args, &b); code != 2 || b.Len() == 0 {
			t.Errorf("SuperviseMain(%q) = %d, %q", args, code, b.String())
		}
	}
}

// TestSuperviseReportsAHandlerThatCannotStart runs the supervisor as its
// own process: in this one it would make the test binary a subreaper.
func TestSuperviseReportsAHandlerThatCannotStart(t *testing.T) {
	self, _ := os.Executable()
	c := exec.Command(self, SuperviseCommand, "10", filepath.Join(t.TempDir(), "missing"), "--", "missing")
	out, _ := c.CombinedOutput()
	if code := c.ProcessState.ExitCode(); code != 127 || !strings.Contains(string(out), "agent-loop supervise:") {
		t.Errorf("supervise = %d, %q; want 127 and a reason", code, out)
	}
}

func TestMissingHandlerNeverStarts(t *testing.T) {
	f := newFixture(t)
	f.spec.Command = []string{filepath.Join(f.dir, "bin", "missing"), "x"}
	f.spec.Runner = profile.RunnerLocalUnsandboxed
	if out, err := newLocal().Run(context.Background(), f.spec); err == nil || !out.Started.IsZero() {
		t.Errorf("Run = %+v, %v; want an error and no outcome", out, err)
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

// TestHandlerCommitsAsTheAuthor: a coder run's commits are the bot's, and
// the owner's global git config (identity, signing, aliases) doesn't apply;
// a review run gets none of the git variables (#102).
func TestHandlerCommitsAsTheAuthor(t *testing.T) {
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\n\tname = Owner\n\temail = owner@example.com\n[commit]\n\tgpgsign = true\n[gpg]\n\tprogram = false\n[alias]\n\tco = checkout\n"), 0o644)
	f := newFixture(t)
	out := filepath.Join(f.dir, "commit.txt")
	f.spec.Command = f.cmd("commit", out)
	f.spec.Author = ports.Signature{Name: "project-coder[bot]", Email: "9001+project-coder[bot]@users.noreply.github.com"}
	l := newLocal()
	worker := map[string]string{"PATH": os.Getenv("PATH"), "HOME": home}
	l.Getenv = func(k string) (string, bool) { v, ok := worker[k]; return v, ok }
	if _, err := l.Run(context.Background(), f.spec); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("%v\n%s", err, f.log())
	}
	a := f.spec.Author
	want := fmt.Sprintf("commit=<nil> \nsig=%s|%s|%s|%s|N\nalias=\n", a.Name, a.Email, a.Name, a.Email)
	if string(b) != want {
		t.Errorf("handler's commit:\n%s\nwant:\n%s", b, want)
	}

	// The coder run's handler is kept off the system git config as well,
	// and its global config is the run's own (work plan #102 D1, #112).
	f = newFixture(t)
	envFile := filepath.Join(f.dir, "env.txt")
	f.spec.Command = f.cmd("env", envFile)
	f.spec.Author = a
	if _, err := l.Run(context.Background(), f.spec); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			env[k] = v
		}
	}
	if env["GIT_CONFIG_NOSYSTEM"] != "1" {
		t.Errorf("coder run GIT_CONFIG_NOSYSTEM = %q, want 1", env["GIT_CONFIG_NOSYSTEM"])
	}
	if g := env["GIT_CONFIG_GLOBAL"]; g == "" || filepath.Dir(g) != filepath.Dir(env["TMPDIR"]) {
		t.Errorf("coder run GIT_CONFIG_GLOBAL = %q, not in the run directory (TMPDIR %q)", g, env["TMPDIR"])
	}

	// A review run gets no author and no git config of the run's own.
	f = newFixture(t)
	envFile = filepath.Join(f.dir, "env.txt")
	f.spec.Command = f.cmd("env", envFile)
	if _, err := l.Run(context.Background(), f.spec); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "GIT_AUTHOR_") || strings.HasPrefix(line, "GIT_COMMITTER_") || strings.HasPrefix(line, "GIT_CONFIG_") {
			t.Errorf("review run got %s", line)
		}
	}
}

// TestHandlerEnvironmentPutsToolCachesInTheRunsTempDir: the Go build and
// module caches, the XDG, npm, pip and Cargo caches, gh's config directory
// and the profile's own caches point into the run's temp directory, the one
// place outside the worktree a run may write (TECHNICAL-SPEC §7.2), unless
// the stage env or PassEnv sets them (#96, #97).
func TestHandlerEnvironmentPutsToolCachesInTheRunsTempDir(t *testing.T) {
	defaults := map[string]string{
		"GOCACHE": "go-build", "XDG_CACHE_HOME": "cache", "GH_CONFIG_DIR": "gh",
		"npm_config_cache": "npm", "PIP_CACHE_DIR": "pip", "GOMODCACHE": "go-mod", "CARGO_HOME": "cargo",
	}
	if !maps.Equal(ToolCaches, defaults) {
		t.Errorf("ToolCaches = %v, want %v", ToolCaches, defaults)
	}
	// caches is the profile's list for every run below.
	var caches []string
	// run starts a handler with the stage env set, and with the worker's
	// environment holding worker, of which pass is passed through.
	run := func(t *testing.T, set, worker map[string]string, pass []string) map[string]string {
		t.Helper()
		f := newFixture(t)
		envFile := filepath.Join(f.dir, "env.txt")
		f.spec.Command = f.cmd("env", envFile)
		f.spec.Caches = caches
		for k, v := range set {
			f.spec.Env[k] = v
		}
		l := newLocal()
		l.PassEnv = pass
		l.Getenv = func(k string) (string, bool) {
			if k == "PATH" {
				return os.Getenv("PATH"), true
			}
			v, ok := worker[k]
			return v, ok
		}
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
		return got
	}
	// check wants each variable in want, and every other tool cache at its
	// default in the run's temp directory.
	check := func(t *testing.T, got, want map[string]string) {
		t.Helper()
		for k, sub := range defaults {
			w, ok := want[k]
			if !ok {
				w = filepath.Join(got["TMPDIR"], sub)
			}
			if got[k] != w {
				t.Errorf("%s = %q, want %q", k, got[k], w)
			}
		}
	}
	// The worker's own values are never inherited unless PassEnv names them.
	worker := map[string]string{
		"GOCACHE": "/home/owner/Library/Caches/go-build", "XDG_CACHE_HOME": "/home/owner/.cache", "GH_CONFIG_DIR": "/home/owner/.config/gh",
		"npm_config_cache": "/home/owner/.npm", "PIP_CACHE_DIR": "/home/owner/Library/Caches/pip", "GOMODCACHE": "/home/owner/go/pkg/mod",
		"CARGO_HOME": "/home/owner/.cargo", "BUNDLE_PATH": "/home/owner/.bundle", "YARN_CACHE_FOLDER": "/home/owner/.yarn",
	}
	t.Run("defaults", func(t *testing.T) {
		check(t, run(t, nil, worker, nil), nil)
	})
	t.Run("stage env wins for every cache", func(t *testing.T) {
		set := map[string]string{"GOCACHE": "/cache/go", "XDG_CACHE_HOME": "/cache/xdg", "GH_CONFIG_DIR": "/cache/gh"}
		check(t, run(t, set, worker, nil), set)
	})
	t.Run("stage env for one, defaults for the rest", func(t *testing.T) {
		set := map[string]string{"XDG_CACHE_HOME": "/cache/xdg"}
		check(t, run(t, set, worker, nil), set)
	})
	t.Run("PassEnv wins", func(t *testing.T) {
		got := run(t, nil, worker, []string{"GH_CONFIG_DIR", "GOMODCACHE"})
		check(t, got, map[string]string{"GH_CONFIG_DIR": "/home/owner/.config/gh", "GOMODCACHE": "/home/owner/go/pkg/mod"})
	})
	t.Run("profile caches", func(t *testing.T) {
		caches = []string{"BUNDLE_PATH", "YARN_CACHE_FOLDER"}
		defer func() { caches = nil }()
		got := run(t, nil, worker, nil)
		check(t, got, nil)
		for _, k := range []string{"BUNDLE_PATH", "YARN_CACHE_FOLDER"} {
			if w := filepath.Join(got["TMPDIR"], "caches", k); got[k] != w {
				t.Errorf("%s = %q, want %q", k, got[k], w)
			}
		}
	})
	t.Run("stage env and PassEnv win over profile caches", func(t *testing.T) {
		caches = []string{"BUNDLE_PATH", "YARN_CACHE_FOLDER"}
		defer func() { caches = nil }()
		got := run(t, map[string]string{"BUNDLE_PATH": "/cache/bundle"}, worker, []string{"YARN_CACHE_FOLDER"})
		if got["BUNDLE_PATH"] != "/cache/bundle" || got["YARN_CACHE_FOLDER"] != "/home/owner/.yarn" {
			t.Errorf("BUNDLE_PATH = %q, YARN_CACHE_FOLDER = %q", got["BUNDLE_PATH"], got["YARN_CACHE_FOLDER"])
		}
	})
	t.Run("not shared between runs", func(t *testing.T) {
		caches = []string{"BUNDLE_PATH"}
		defer func() { caches = nil }()
		a, b := run(t, nil, worker, nil), run(t, nil, worker, nil)
		for k := range defaults {
			if a[k] == b[k] {
				t.Errorf("%s is %q in both runs", k, a[k])
			}
		}
		if a["BUNDLE_PATH"] == b["BUNDLE_PATH"] {
			t.Errorf("BUNDLE_PATH is %q in both runs", a["BUNDLE_PATH"])
		}
	})
}

// TestRunDirIsRemovedWithReadOnlyModuleCache: the Go module cache writes
// read-only directories, and the run's directory is still removed (work
// plan #97 D7).
func TestRunDirIsRemovedWithReadOnlyModuleCache(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "al-x")
	mod := filepath.Join(dir, "tmp", "go-mod", "example.com", "m@v1.0.0")
	if err := os.MkdirAll(mod, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mod, "go.mod"), []byte("module m\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{mod, filepath.Dir(mod)} {
		if err := os.Chmod(d, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	if err := removeRunDir(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("run dir still there: %v", err)
	}
}

// TestCheckCacheName: a profile cache is any variable name the runner would
// set, and never one it sets itself, a secret, an inherited variable or a
// default cache, which the runner would ignore (work plan #97 D5).
func TestCheckCacheName(t *testing.T) {
	for _, k := range []string{"BUNDLE_PATH", "YARN_CACHE_FOLDER", "npm_config_prefix", "_X1"} {
		if err := CheckCacheName(k); err != nil {
			t.Errorf("%s: %v", k, err)
		}
	}
	for k, want := range map[string]string{
		"":                    "invalid variable name",
		"A=B":                 "invalid variable name",
		"MY-CACHE":            "invalid variable name",
		"1X":                  "invalid variable name",
		"AGENT_LOOP_SECRET_X": "is a secret",
		"TMPDIR":              "is set by the runner",
		"HOME":                "is inherited from the worker",
		"PATH":                "is inherited from the worker",
		"LC_ALL":              "is inherited from the worker",
		"GOCACHE":             "is already a default cache",
		"CARGO_HOME":          "is already a default cache",
	} {
		if err := CheckCacheName(k); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", k, err, want)
		}
	}
}

func TestHandlerEnvironmentRefusesSecretsAndReservedNames(t *testing.T) {
	for _, tc := range []struct {
		env    map[string]string
		pass   []string
		caches []string
	}{
		{env: map[string]string{"AGENT_LOOP_SECRET_X": "v"}},
		{env: map[string]string{"GH_TOKEN": "v"}},
		{env: map[string]string{"AGENT_LOOP_RESULT": "/x"}},
		{env: map[string]string{ClaudeSettingsEnv: "/x"}},
		{env: map[string]string{"A=B": "v"}},
		{pass: []string{"AGENT_LOOP_SECRET_GITHUB_APP_CODER_APP_ID"}},
		{pass: []string{"TMPDIR"}},
		{env: map[string]string{"GIT_AUTHOR_NAME": "Owner"}},
		{env: map[string]string{"GIT_COMMITTER_EMAIL": "owner@example.com"}},
		{env: map[string]string{"GIT_CONFIG_GLOBAL": "/x"}},
		{pass: []string{"GIT_CONFIG_NOSYSTEM"}},
		{caches: []string{"TMPDIR"}},
		{caches: []string{"AGENT_LOOP_SECRET_X"}},
		{caches: []string{"GH_TOKEN"}},
		{caches: []string{"HOME"}},
		{caches: []string{"PATH"}},
		{caches: []string{"BUNDLE_PATH", "GOCACHE"}},
		{caches: []string{"MY-CACHE"}},
	} {
		f := newFixture(t)
		marker := filepath.Join(f.dir, "ran")
		f.spec.Command = f.cmd("env", marker)
		for k, v := range tc.env {
			f.spec.Env[k] = v
		}
		f.spec.Caches = tc.caches
		l := newLocal()
		l.PassEnv = tc.pass
		if _, err := l.Run(context.Background(), f.spec); err == nil {
			t.Errorf("env %v, pass %v, caches %v: accepted", tc.env, tc.pass, tc.caches)
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

// TestTrackerFindsAndKillsDescendants exercises the supervisor's process
// scan in this process: a child in its own session and its child are
// found, remembered, and killed; a remembered pid whose start time differs
// (a reused pid) is forgotten, not killed.
func TestTrackerFindsAndKillsDescendants(t *testing.T) {
	f := newFixture(t)
	pidfile := filepath.Join(f.dir, "pids")
	c := exec.Command(f.self, "fake-handler", "setsid-child", pidfile)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	go c.Wait()
	pids := readPids(t, pidfile, 2)
	tr := &tracker{self: os.Getpid(), pgid: syscall.Getpgrp(), seen: map[int]int64{}}
	live := tr.scan()
	for _, pid := range pids {
		if !slices.Contains(live, pid) {
			t.Errorf("scan = %v, lacks %d", live, pid)
		}
	}
	// A process re-parented away (macOS) is still found from what was seen:
	// no process has parent -1, so only the remembered pids are roots.
	orphan := &tracker{self: -1, seen: map[int]int64{pids[0]: tr.seen[pids[0]]}}
	if got := orphan.scan(); !slices.Contains(got, pids[0]) || !slices.Contains(got, pids[1]) {
		t.Errorf("scan from a remembered pid = %v, want %v", got, pids)
	}
	reused := &tracker{self: -1, seen: map[int]int64{pids[0]: tr.seen[pids[0]] + 1}}
	if got := reused.scan(); len(got) != 0 || len(reused.seen) != 0 {
		t.Errorf("a pid with another start time: scan = %v, seen = %v", got, reused.seen)
	}
	tr.signal(syscall.SIGTERM) // outside this group: signalled
	for _, pid := range pids {
		if !gone(pid) {
			t.Errorf("process %d survived SIGTERM", pid)
		}
	}
	tr.killAll()
	if live := tr.scan(); len(live) != 0 {
		t.Errorf("after killAll, scan = %v", live)
	}
}
