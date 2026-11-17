package workflows

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/workspace"
)

// gitRun runs git in dir with a fixed identity and no user config.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitRemote is a bare repository with main and agent/42, and a clone to
// push from.
type gitRemote struct{ bare, work string }

func newGitRemote(t *testing.T) *gitRemote {
	t.Helper()
	root := t.TempDir()
	r := &gitRemote{bare: filepath.Join(root, "origin.git"), work: filepath.Join(root, "work")}
	gitRun(t, root, "init", "--quiet", "--bare", "--initial-branch=main", r.bare)
	gitRun(t, root, "clone", "--quiet", r.bare, r.work)
	gitRun(t, r.work, "checkout", "--quiet", "-b", "main")
	r.commit(t, "a.txt", "base")
	gitRun(t, r.work, "push", "--quiet", "origin", "main")
	gitRun(t, r.work, "checkout", "--quiet", "-b", "agent/42")
	r.commit(t, "b.txt", "agent work")
	gitRun(t, r.work, "push", "--quiet", "origin", "agent/42")
	return r
}

func (r *gitRemote) commit(t *testing.T, file, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.work, file), []byte(content+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, r.work, "add", file)
	gitRun(t, r.work, "commit", "--quiet", "-m", content)
	return gitRun(t, r.work, "rev-parse", "HEAD")
}

// on commits content to file on branch and pushes it.
func (r *gitRemote) on(t *testing.T, branch, file, content string) string {
	t.Helper()
	gitRun(t, r.work, "checkout", "--quiet", branch)
	c := r.commit(t, file, content)
	gitRun(t, r.work, "push", "--quiet", "origin", branch)
	return c
}

func (r *gitRemote) head(t *testing.T, branch string) string {
	return gitRun(t, r.bare, "rev-parse", "refs/heads/"+branch)
}

// countingRunner fails the test if any handler runs.
type countingRunner struct{ n int }

func (c *countingRunner) Run(context.Context, ports.RunSpec) (ports.RunOutcome, error) {
	c.n++
	return ports.RunOutcome{}, nil
}

type runLog struct {
	started, ended []string
	heads          []ports.Commit
}

func (l *runLog) Started(_ context.Context, _ ports.ItemRef, id string, _ time.Time) error {
	l.started = append(l.started, id)
	return nil
}

func (l *runLog) Ended(_ context.Context, _ ports.ItemRef, id string, _ time.Time, head ports.Commit) error {
	l.ended = append(l.ended, id)
	l.heads = append(l.heads, head)
	return nil
}

func baseActivities(t *testing.T, r *gitRemote) (*Activities, *store, *countingRunner, *runLog) {
	t.Helper()
	g, err := workspace.New(filepath.Join(t.TempDir(), "ws"))
	if err != nil {
		t.Fatal(err)
	}
	sec, run, log := &store{}, &countingRunner{}, &runLog{}
	return &Activities{
		Config: Config{
			Project: "p", Repo: ports.RepoRef{URL: r.bare, BaseBranch: "main"}, RepoName: "o/r", BranchPrefix: "agent/",
			CoderSignature: workspace.Signature{Name: "project-coder[bot]", Email: "9001+project-coder[bot]@users.noreply.github.com"},
			RunsDir:        t.TempDir(),
		},
		Workspace: g, Git: g, Runner: run, Secrets: sec, CoderRuns: log,
	}, sec, run, log
}

// TestFR5AC6_CleanBaseUpdateIsOneMergeCommitAndNoHandler: a base update
// without conflicts makes exactly one merge commit (first parent the head,
// second the base tip) as the coder, pushes it, runs no handler, records
// a coder run and revokes its coder worker token (INTEGRATION-SPEC §5.3).
func TestFR5AC6_CleanBaseUpdateIsOneMergeCommitAndNoHandler(t *testing.T) {
	r := newGitRemote(t)
	base := r.on(t, "main", "c.txt", "base moved")
	head := r.head(t, "agent/42")
	a, sec, run, log := baseActivities(t, r)
	req := BaseUpdateRequest{Item: "42", Action: core.UpdateBase{Key: "item/p/42#1:base_update:5", Change: "7", Head: head}}
	rep, err := a.UpdateBase(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Updated == nil {
		t.Fatalf("report = %+v", rep)
	}
	newHead := r.head(t, "agent/42")
	if rep.Updated.Head != newHead {
		t.Fatalf("head = %s, remote = %s", rep.Updated.Head, newHead)
	}
	if got := gitRun(t, r.bare, "rev-list", "--parents", "-n", "1", newHead); got != newHead+" "+head+" "+base {
		t.Fatalf("parents = %q, want %s %s", got, head, base)
	}
	if n := gitRun(t, r.bare, "rev-list", "--count", head+".."+newHead, "^"+base); n != "1" {
		t.Fatalf("new commits on the branch = %s, want 1", n)
	}
	if got := gitRun(t, r.bare, "log", "-1", "--format=%an|%cn", newHead); got != "project-coder[bot]|project-coder[bot]" {
		t.Fatalf("author|committer = %s", got)
	}
	if run.n != 0 {
		t.Fatalf("handlers run = %d", run.n)
	}
	if ids := sec.identities(); ids[ports.IdentityCoder] != 1 || len(ids) != 1 || len(sec.live()) != 0 {
		t.Fatalf("tokens: issued %v, live %v", ids, sec.live())
	}
	if len(log.started) != 1 || len(log.ended) != 1 || log.heads[0] != ports.Commit(newHead) {
		t.Fatalf("coder runs = %+v", log)
	}
	// A retry after the push finds the merge and reports it, pushing nothing.
	again, err := a.UpdateBase(context.Background(), req)
	if err != nil || again.Updated == nil || again.Updated.Head != newHead || r.head(t, "agent/42") != newHead {
		t.Fatalf("retry = %+v, %v", again, err)
	}
}

// TestFR5AC6_BaseUpdateWithConflictsRunsAddressInBaseUpdateMode: with
// conflicts nothing is pushed, and the core's next stage is an address run
// in base_update mode, which RunStage starts with that mode.
func TestFR5AC6_BaseUpdateWithConflictsRunsAddressInBaseUpdateMode(t *testing.T) {
	r := newGitRemote(t)
	r.on(t, "main", "b.txt", "base side")
	head := r.head(t, "agent/42")
	a, _, run, _ := baseActivities(t, r)
	rep, err := a.UpdateBase(context.Background(), BaseUpdateRequest{Item: "42", Action: core.UpdateBase{Key: "k", Change: "7", Head: head}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Conflicts == nil || r.head(t, "agent/42") != head || run.n != 0 {
		t.Fatalf("report = %+v, head %s, runs %d", rep, r.head(t, "agent/42"), run.n)
	}

	// The core turns the conflicts into an address run in base_update mode.
	s := core.ItemState{State: core.StateAddressing, WorkflowID: "item/p/42", Item: "42", Claim: 1,
		Change: &core.Change{Ref: "7", Head: head, Pusher: core.PusherCoder}, UpdatingBase: true,
		LatestReview: &core.Review{Commit: head, Kind: contract.KindRound}, RoundsUsed: 1}
	_, acts := core.Decide(s, rep.Event(), core.Policy{})
	var rs *core.RunStage
	for _, x := range acts {
		if v, ok := x.(core.RunStage); ok {
			rs = &v
		}
	}
	if rs == nil || rs.Stage != contract.StageAddress || rs.Address == nil || rs.Address.Mode != contract.ModeBaseUpdate {
		t.Fatalf("actions = %+v", acts)
	}

	// RunStage starts the address handler in that mode.
	h := newHarness(t)
	item := h.item()
	ch := h.implemented(item)
	rs.Change, rs.Head = string(ch.Ref), string(ch.Head)
	h.run(h.req(item, *rs), func(h *harness, in contract.Input, out string) string { return "" })
	if in := h.ins[len(h.ins)-1]; in.Stage != contract.StageAddress || in.Address.Mode != contract.ModeBaseUpdate {
		t.Fatalf("address input = %+v", in.Address)
	}
}

// TestFR5AC6_BaseUpdateRefusedWhenHeadMoved: a head that moved since the
// core read it is refused, and nothing is pushed.
func TestFR5AC6_BaseUpdateRefusedWhenHeadMoved(t *testing.T) {
	r := newGitRemote(t)
	r.on(t, "main", "c.txt", "base moved")
	old := r.head(t, "agent/42")
	moved := r.on(t, "agent/42", "d.txt", "owner push")
	a, _, _, _ := baseActivities(t, r)
	rep, err := a.UpdateBase(context.Background(), BaseUpdateRequest{Item: "42", Action: core.UpdateBase{Key: "k", Change: "7", Head: old}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Refused == nil || r.head(t, "agent/42") != moved {
		t.Fatalf("report = %+v", rep)
	}
}
