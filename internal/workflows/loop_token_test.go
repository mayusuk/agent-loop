package workflows

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/runners"
)

// The test binary is also the credential-helper client, the supervisor and
// a fake handler, so the loop-token test runs the real local runner.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case runners.CredentialHelperCommand:
			os.Exit(runners.CredentialHelperMain(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
		case runners.SuperviseCommand:
			os.Exit(runners.SuperviseMain(os.Args[2:], os.Stderr))
		case "dump-handler":
			os.Exit(dumpHandler())
		}
	}
	os.Exit(m.Run())
}

// dumpHandler writes its environment and what the credential helper serves
// for github.com to $AGENT_LOOP_OUT, then a failed result.
func dumpHandler() int {
	out := os.Getenv("AGENT_LOOP_OUT")
	os.WriteFile(filepath.Join(out, "env.txt"), []byte(strings.Join(os.Environ(), "\n")), 0o600)
	c := exec.Command("git", "credential", "fill")
	c.Stdin = strings.NewReader("protocol=https\nhost=github.com\npath=o/r.git\n\n")
	b, err := c.CombinedOutput()
	os.WriteFile(filepath.Join(out, "cred.txt"), []byte(fmt.Sprintf("%s\nerr=%v\n", b, err)), 0o600)
	in, _ := os.ReadFile(os.Getenv("AGENT_LOOP_INPUT"))
	var input struct {
		RunID string `json:"run_id"`
	}
	json.Unmarshal(in, &input)
	res, _ := json.Marshal(map[string]any{"contract_version": "1.0", "run_id": input.RunID, "result": "failed", "failed": map[string]any{"reason": "dumped"}})
	os.WriteFile(os.Getenv("AGENT_LOOP_RESULT"), res, 0o600)
	return 0
}

// initWS prepares a git-initialised directory as the run's tree, so the
// real runner can configure its credential helper in it.
type initWS struct {
	t    *testing.T
	head ports.Commit
}

func (w initWS) Prepare(_ context.Context, _ ports.RepoRef, b ports.Branch) (ports.Dir, error) {
	dir := filepath.Join(w.t.TempDir(), "wt")
	if out, err := exec.Command("git", "init", "--quiet", dir).CombinedOutput(); err != nil {
		w.t.Fatalf("git init: %v %s", err, out)
	}
	return ports.Dir{Path: dir, Branch: b.Name, Head: w.head}, nil
}

func (initWS) Release(context.Context, ports.Dir) error { return nil }

// TestPRODUCT8Item8_LoopTokenNeverReachesAHandler runs implement, address
// and review handlers through the real local runner while the worker holds
// a loop token. No handler's environment or credential helper ever sees
// it: the helper serves the coder's push token, or nothing for review, and
// GH_TOKEN is the stage identity's read token (INTEGRATION-SPEC §3.4,
// TECHNICAL-SPEC §7.3).
func TestPRODUCT8Item8_LoopTokenNeverReachesAHandler(t *testing.T) {
	const loopSecret = "ghs_LOOPloopLOOPloopLOOPloopLOOPloop0001"
	t.Setenv("AGENT_LOOP_SECRET_LOOP", loopSecret)
	t.Setenv("GITHUB_TOKEN", loopSecret)
	t.Setenv("GH_TOKEN", loopSecret)

	h := newHarness(t)
	item := h.item()
	ch := h.implemented(item)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	local := &runners.Local{DenyRead: runners.DeniedReads(nil, t.TempDir()), KillGrace: 500 * time.Millisecond}
	h.a.Runner = local
	h.a.Workspace = initWS{t: t, head: ch.Head}
	for s := range h.a.Stages {
		h.a.Stages[s] = StageConfig{Command: []string{self, "dump-handler"}, Runner: "local-unsandboxed", TimeLimit: time.Minute}
	}
	issuedBefore := len(h.sec.issued)

	for _, c := range []struct {
		rs       core.RunStage
		pushes   bool
		identity ports.Identity
	}{
		{core.RunStage{Stage: contract.StageAddress, Change: string(ch.Ref), Head: string(ch.Head),
			Address: &core.AddressSpec{Mode: contract.ModeFindings}}, true, ports.IdentityCoder},
		{core.RunStage{Stage: contract.StageReview, Change: string(ch.Ref), Head: string(ch.Head),
			Review: &core.ReviewSpec{Kind: contract.KindRound, Commit: string(ch.Head), Round: 1}}, false, ports.IdentityReviewer},
	} {
		t.Run(string(c.rs.Stage), func(t *testing.T) {
			req := h.req(item, c.rs)
			rep, err := h.a.RunStage(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if rep.Result == nil || rep.Result.Reason != "dumped" {
				log, _ := os.ReadFile(rep.LogPath)
				t.Fatalf("result = %+v\n%s", rep.Result, log)
			}
			dir, _ := h.a.runDir(req.Run.RunID)
			env, _ := os.ReadFile(filepath.Join(dir, "out", "env.txt"))
			cred, _ := os.ReadFile(filepath.Join(dir, "out", "cred.txt"))
			if strings.Contains(string(env), loopSecret) || strings.Contains(string(cred), loopSecret) {
				t.Fatalf("the loop token reached the handler:\nenv:\n%s\ncred:\n%s", env, cred)
			}
			if strings.Contains(string(env), "AGENT_LOOP_SECRET_") || strings.Contains(string(env), "GITHUB_TOKEN=") {
				t.Fatalf("a worker secret variable reached the handler:\n%s", env)
			}
			var ghToken string
			for _, kv := range strings.Split(string(env), "\n") {
				if v, ok := strings.CutPrefix(kv, "GH_TOKEN="); ok {
					ghToken = v
				}
			}
			var read, push *ports.Token
			for _, tok := range h.sec.issued[issuedBefore:] {
				tok := tok
				switch {
				case tok.Secret() == ghToken:
					read = &tok
				case strings.Contains(string(cred), "password="+tok.Secret()):
					push = &tok
				}
			}
			if read == nil || read.Identity != c.identity || read.Scope.Permissions["contents"] != "read" {
				t.Fatalf("GH_TOKEN is not the %s read token: %q", c.identity, ghToken)
			}
			if c.pushes != (push != nil) || (push != nil && (push.Identity != ports.IdentityCoder || push.Scope.Permissions["contents"] != "write")) {
				t.Fatalf("helper served %q, pushes = %v", cred, c.pushes)
			}
			if !c.pushes && strings.Contains(string(cred), "password=") {
				t.Fatalf("the review helper served a credential: %q", cred)
			}
			if live := h.sec.live(); len(live) != 0 {
				t.Fatalf("tokens not revoked after the run: %v", live)
			}
		})
	}
	if n := h.sec.identities()[ports.IdentityLoop]; n != 0 {
		t.Fatalf("loop tokens issued for runs: %d", n)
	}
}
