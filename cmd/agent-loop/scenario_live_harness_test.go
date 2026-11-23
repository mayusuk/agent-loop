package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/mayusuk/agent-loop/internal/api/v1"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/ports"
)

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestScenario11_1_LiveHarnessDrivesScriptHandler runs the live scenario's
// handler side against the real scripthandler, with no GitHub: a scripted
// implement commits and pushes to the run's branch, the result carries the
// pushed head and the change, the run's GH_TOKEN is collected for the
// credential check, and an unscripted run fails and is reported (work plan
// #25 D9). The live test itself needs AGENT_LOOP_LIVE=1.
func TestScenario11_1_LiveHarnessDrivesScriptHandler(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "scripthandler")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/mayusuk/agent-loop/internal/scripthandler").CombinedOutput(); err != nil {
		t.Fatalf("building scripthandler: %v\n%s", err, out)
	}
	sockDir, err := os.MkdirTemp("", "lh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	socket := filepath.Join(sockDir, "h.sock")
	lv := &live{t: t, rows: map[ports.ItemRef]*apiv1.BoardItem{}, scripts: map[ports.ItemRef]map[contract.Stage][]liveHandler{},
		ins: map[ports.ItemRef][]contract.Input{}, at: map[string]time.Time{}, held: map[ports.ItemRef]bool{}}
	lv.serveHandlers(socket)
	lv.on("7", contract.StageImplement, lv.impl())

	remote := filepath.Join(t.TempDir(), "remote.git")
	gitRun(t, ".", "init", "-q", "--bare", "-b", "main", remote)
	ws := filepath.Join(t.TempDir(), "ws")
	gitRun(t, ".", "clone", "-q", remote, ws)
	os.WriteFile(filepath.Join(ws, "README"), []byte("x\n"), 0o644)
	gitRun(t, ws, "add", "README")
	gitRun(t, ws, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "-m", "base")
	gitRun(t, ws, "push", "-q", "origin", "HEAD:refs/heads/main")
	gitRun(t, ws, "checkout", "-q", "-b", "agent/7-x")
	base := gitRun(t, ws, "rev-parse", "HEAD")

	runOnce := func(stage contract.Stage, runID string) (string, string, error) {
		dir := t.TempDir()
		in, _ := json.Marshal(map[string]any{"contract_version": "1.0", "run_id": runID, "stage": stage, "attempt": 1,
			"workspace": map[string]any{"path": ws, "branch": "agent/7-x", "head": base}, "item": map[string]any{"ref": "7"}})
		os.WriteFile(filepath.Join(dir, "input.json"), in, 0o600)
		os.Mkdir(filepath.Join(dir, "out"), 0o700)
		cmd := exec.Command(bin, socket)
		cmd.Env = append(os.Environ(), "AGENT_LOOP_INPUT="+filepath.Join(dir, "input.json"), "AGENT_LOOP_OUT="+filepath.Join(dir, "out"),
			"AGENT_LOOP_RESULT="+filepath.Join(dir, "result.json"), "GH_TOKEN=run-read-token")
		out, err := cmd.CombinedOutput()
		res, _ := os.ReadFile(filepath.Join(dir, "result.json"))
		change, _ := os.ReadFile(filepath.Join(dir, "out", "change.md"))
		if err != nil {
			return "", "", fmt.Errorf("%v: %s", err, out)
		}
		return string(res), string(change), nil
	}

	res, change, err := runOnce(contract.StageImplement, "r1")
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		RunID  string `json:"run_id"`
		Result string `json:"result"`
		Done   struct {
			Head   string `json:"head"`
			Change struct {
				BodyFile string `json:"body_file"`
			} `json:"change"`
		} `json:"done"`
	}
	if err := json.Unmarshal([]byte(res), &r); err != nil {
		t.Fatalf("result.json %q: %v", res, err)
	}
	pushed := gitRun(t, remote, "rev-parse", "refs/heads/agent/7-x")
	if r.RunID != "r1" || r.Result != "done" || r.Done.Head != pushed || pushed == base || r.Done.Change.BodyFile != "change.md" {
		t.Fatalf("result %+v, remote head %s, base %s", r, pushed, base)
	}
	if !strings.Contains(gitRun(t, remote, "show", pushed+":"+itemFile("7")), "implement run r1") {
		t.Fatal("the commit doesn't change the item's file")
	}
	if !strings.Contains(change, "token=run-read-token") {
		t.Fatalf("change.md = %q, want the run's token echoed for the worker to mask", change)
	}
	lv.mu.Lock()
	tokens, ins := lv.tokens, lv.ins["7"]
	lv.mu.Unlock()
	if len(tokens) != 1 || tokens[0] != "run-read-token" || len(ins) != 1 || ins[0].RunID != "r1" {
		t.Fatalf("collected tokens %v, inputs %v", tokens, ins)
	}

	// The script has no second implement.
	if _, _, err := runOnce(contract.StageImplement, "r2"); err == nil {
		t.Fatal("an unscripted run succeeded")
	}
	lv.mu.Lock()
	problems := lv.problems
	lv.mu.Unlock()
	if len(problems) != 1 || !strings.Contains(problems[0], "unscripted implement run r2") {
		t.Fatalf("problems %v", problems)
	}
}
