package main

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scenarioSide answers one handler exchange as the live scenario does.
type scenarioSide struct {
	plan   Plan
	finish func(p Pushed) Finish
	hello  chan Hello
	pushed chan Pushed
}

func serve(t *testing.T, s *scenarioSide) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s.hello, s.pushed = make(chan Hello, 1), make(chan Pushed, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		enc, dec := json.NewEncoder(c), json.NewDecoder(bufio.NewReader(c))
		var h Hello
		if dec.Decode(&h) != nil {
			return
		}
		s.hello <- h
		enc.Encode(s.plan)
		if s.plan.Error != "" {
			return
		}
		var p Pushed
		if dec.Decode(&p) != nil {
			return
		}
		s.pushed <- p
		enc.Encode(s.finish(p))
	}()
	return sock
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// workspace is a clone of a bare remote with one commit on main, checked
// out on branch.
func workspace(t *testing.T, branch string) (dir, remote, head string) {
	t.Helper()
	remote = filepath.Join(t.TempDir(), "remote.git")
	gitIn(t, ".", "init", "-q", "--bare", "-b", "main", remote)
	dir = filepath.Join(t.TempDir(), "ws")
	gitIn(t, ".", "clone", "-q", remote, dir)
	os.WriteFile(filepath.Join(dir, "README"), []byte("x\n"), 0o644)
	gitIn(t, dir, "add", "README")
	gitIn(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "-m", "base")
	gitIn(t, dir, "push", "-q", "origin", "HEAD:refs/heads/main")
	gitIn(t, dir, "checkout", "-q", "-b", branch)
	return dir, remote, gitIn(t, dir, "rev-parse", "HEAD")
}

type env map[string]string

func (e env) get(k string) string { return e[k] }

func setup(t *testing.T, stage, branch, wsDir, head string) (env, runFiles) {
	t.Helper()
	run := t.TempDir()
	in := map[string]any{"contract_version": "1.0", "run_id": "r1", "stage": stage, "attempt": 1,
		"workspace": map[string]any{"path": wsDir, "branch": branch, "head": head},
		"item":      map[string]any{"ref": "7"}}
	b, _ := json.Marshal(in)
	inPath := filepath.Join(run, "input.json")
	os.WriteFile(inPath, b, 0o600)
	out := filepath.Join(run, "out")
	os.Mkdir(out, 0o700)
	e := env{"AGENT_LOOP_INPUT": inPath, "AGENT_LOOP_OUT": out, "AGENT_LOOP_RESULT": filepath.Join(run, "result.json"), "GH_TOKEN": "run-read-token"}
	return e, runFiles{out: out, result: e["AGENT_LOOP_RESULT"]}
}

type runFiles struct{ out, result string }

// TestINT4_ScriptHandlerCommitsPushesAndWritesTheResult: asked to commit,
// the handler pushes one new commit to the run's branch, reports the new
// head and the run's GH_TOKEN, and writes the files (the token placeholder
// filled in) and result.json the scenario sends back.
func TestINT4_ScriptHandlerCommitsPushesAndWritesTheResult(t *testing.T) {
	ws, remote, base := workspace(t, "agent/7-x")
	e, c := setup(t, "implement", "agent/7-x", ws, base)
	s := &scenarioSide{plan: Plan{Commit: true}, finish: func(p Pushed) Finish {
		return Finish{Files: map[string]string{"change.md": "body " + TokenPlaceholder}, Result: json.RawMessage(`{"head":"` + p.Head + `"}`)}
	}}
	sock := serve(t, s)
	if err := run([]string{sock}, e.get); err != nil {
		t.Fatal(err)
	}
	if h := <-s.hello; !strings.Contains(string(h.Input), `"run_id":"r1"`) {
		t.Fatalf("hello = %s, want input.json", h.Input)
	}
	p := <-s.pushed
	if p.Head == base || p.GHToken != e["GH_TOKEN"] {
		t.Fatalf("pushed = %+v, want a new head and the run's GH_TOKEN", p)
	}
	if got := gitIn(t, remote, "rev-parse", "refs/heads/agent/7-x"); got != p.Head {
		t.Fatalf("remote branch at %s, want %s", got, p.Head)
	}
	if got := gitIn(t, remote, "show", p.Head+":"+changedFile("7")); !strings.Contains(got, "implement run r1") {
		t.Fatalf("commit content %q", got)
	}
	if b, _ := os.ReadFile(filepath.Join(c.out, "change.md")); string(b) != "body "+e["GH_TOKEN"] {
		t.Fatalf("change.md = %q", b)
	}
	if b, _ := os.ReadFile(c.result); string(b) != `{"head":"`+p.Head+`"}` {
		t.Fatalf("result.json = %s", b)
	}
}

// TestINT4_ScriptHandlerWithoutCommitKeepsTheHead: a review run makes no
// commit and reports the workspace head.
func TestINT4_ScriptHandlerWithoutCommitKeepsTheHead(t *testing.T) {
	ws, remote, base := workspace(t, "agent/7-x")
	e, c := setup(t, "review", "", ws, base)
	s := &scenarioSide{finish: func(p Pushed) Finish { return Finish{Result: json.RawMessage(`{}`)} }}
	if err := run([]string{serve(t, s)}, e.get); err != nil {
		t.Fatal(err)
	}
	<-s.hello
	if p := <-s.pushed; p.Head != base {
		t.Fatalf("head = %s, want the workspace head %s", p.Head, base)
	}
	if out, err := exec.Command("git", "-C", remote, "rev-parse", "--verify", "-q", "refs/heads/agent/7-x").CombinedOutput(); err == nil {
		t.Fatalf("a review run pushed a branch: %s", out)
	}
	if _, err := os.Stat(c.result); err != nil {
		t.Fatal(err)
	}
}

// TestINT4_ScriptHandlerFailsOnTheScenarioError: an error from the
// scenario, a bad out-file name, a review asked to commit, or missing
// contract variables make the handler fail without a result.
func TestINT4_ScriptHandlerFailsOnTheScenarioError(t *testing.T) {
	cases := map[string]struct {
		stage, branch string
		side          *scenarioSide
	}{
		"plan error":    {"implement", "agent/7-x", &scenarioSide{plan: Plan{Error: "worker killed"}}},
		"finish error":  {"implement", "agent/7-x", &scenarioSide{finish: func(Pushed) Finish { return Finish{Error: "no script"} }}},
		"bad file name": {"implement", "agent/7-x", &scenarioSide{finish: func(Pushed) Finish { return Finish{Files: map[string]string{"../x": "y"}} }}},
		"review commit": {"review", "", &scenarioSide{plan: Plan{Commit: true}}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ws, _, base := workspace(t, "agent/7-x")
			e, c := setup(t, tc.stage, tc.branch, ws, base)
			if err := run([]string{serve(t, tc.side)}, e.get); err == nil {
				t.Fatal("the handler succeeded")
			}
			if _, err := os.Stat(c.result); err == nil {
				t.Fatal("the handler wrote a result")
			}
		})
	}
	if err := run([]string{"/nonexistent.sock"}, env{}.get); err == nil {
		t.Fatal("ran without the contract variables")
	}
	if err := run(nil, env{}.get); err == nil {
		t.Fatal("ran without a socket")
	}
}
