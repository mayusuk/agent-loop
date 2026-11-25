package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/api"
	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/projection"
	"github.com/mayusuk/agent-loop/internal/workflows"
)

// signalled is one signal the test server's workflows received.
type signalled struct {
	ID, Name string
	Arg      any
}

// fakeWorkflows records signals and answers queries from fixed states.
type fakeWorkflows struct {
	mu         sync.Mutex
	signals    []signalled
	projects   map[string]workflows.ProjectView
	deployment *workflows.DeploymentState
	// starting names projects whose workflow runs but can't answer yet.
	starting map[string]bool
}

func (f *fakeWorkflows) Signal(_ context.Context, id, name string, arg any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signals = append(f.signals, signalled{id, name, arg})
	return nil
}

func (f *fakeWorkflows) QueryProject(_ context.Context, p string) (workflows.ProjectView, error) {
	if f.starting[p] {
		return workflows.ProjectView{}, api.ErrStarting
	}
	v, ok := f.projects[p]
	if !ok {
		return v, api.ErrNotRunning
	}
	return v, nil
}

func (f *fakeWorkflows) QueryDeployment(context.Context) (workflows.DeploymentState, error) {
	if f.deployment == nil {
		return workflows.DeploymentState{}, api.ErrNotRunning
	}
	return *f.deployment, nil
}

func (f *fakeWorkflows) take() []signalled {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.signals
	f.signals = nil
	return s
}

var testNow = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

// testServer serves the API on a socket in a short temp directory (Unix
// socket paths are limited to about 100 bytes) and returns the deployment
// config that names it, the fake workflows and the projection.
func testServer(t *testing.T, projects ...string) (string, *fakeWorkflows, *projection.DB) {
	t.Helper()
	dir, err := os.MkdirTemp("", "al")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ctx := context.Background()
	db, err := projection.Open(ctx, filepath.Join(dir, "state", "projection.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	wf := &fakeWorkflows{projects: map[string]workflows.ProjectView{}}
	sock := filepath.Join(dir, "cfg", "api.sock")
	l, err := api.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := api.NewServer(&api.Service{Workflows: wf, Projection: db, Projects: projects, Now: func() time.Time { return testNow }})
	go srv.Serve(l)
	t.Cleanup(srv.Stop)
	cfg := filepath.Join(dir, "deployment.toml")
	if err := os.WriteFile(cfg, []byte("[api]\nsocket = \""+sock+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg, wf, db
}

func cli(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func onBoard(t *testing.T, db *projection.DB, project, item string, at time.Time, row projection.ItemRow) {
	t.Helper()
	row.Project, row.Item = project, item
	err := db.RecordDecision(context.Background(), projection.Decision{
		WorkflowID: "item/" + project + "/" + item, Claim: 1,
		Record: core.DecisionRecord{Seq: 1, At: at, Event: "started", To: row.State},
		Item:   row,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TECHNICAL-SPEC §11: each CLI command sends its signal to the right
// workflow through the API (FR-10 AC3, AC7).
func TestFR10AC3_CLICommandsSendSignals(t *testing.T) {
	cfg, wf, db := testServer(t, "p", "q")
	onBoard(t, db, "p", "42", testNow, projection.ItemRow{State: core.StateInReview})
	onBoard(t, db, "p", "7", testNow, projection.ItemRow{State: core.StateImplementing})
	onBoard(t, db, "q", "7", testNow, projection.ItemRow{State: core.StateImplementing})

	owner := func(c core.Command) workflows.ItemSignal {
		return workflows.MustSignal(core.OwnerCommand{At: core.At{Time: testNow}, Command: c})
	}
	cases := []struct {
		args []string
		want signalled
	}{
		{[]string{"item", "pause", "--config", cfg, "42"}, signalled{"item/p/42", workflows.SignalEvent, owner(core.CommandPause)}},
		{[]string{"item", "resume", "--config", cfg, "42"}, signalled{"item/p/42", workflows.SignalEvent, owner(core.CommandResume)}},
		{[]string{"item", "abort", "--config", cfg, "--project", "q", "7"}, signalled{"item/q/7", workflows.SignalEvent, owner(core.CommandAbort)}},
		{[]string{"project", "pause", "--config", cfg, "p"}, signalled{"project/p", workflows.SignalPause, workflows.PauseSignal{Paused: true}}},
		{[]string{"project", "resume", "--config", cfg, "q"}, signalled{"project/q", workflows.SignalPause, workflows.PauseSignal{Paused: false}}},
		{[]string{"pause", "--config", cfg}, signalled{"deployment", workflows.SignalPause, workflows.PauseSignal{Paused: true}}},
		{[]string{"resume", "--config", cfg}, signalled{"deployment", workflows.SignalPause, workflows.PauseSignal{Paused: false}}},
	}
	for _, c := range cases {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			code, out, errs := cli(t, c.args...)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errs)
			}
			if !strings.Contains(out, c.want.ID) {
				t.Errorf("output %q doesn't name %s", out, c.want.ID)
			}
			if got := wf.take(); !reflect.DeepEqual(got, []signalled{c.want}) {
				t.Fatalf("signals %+v, want %+v", got, c.want)
			}
		})
	}

	refused := []struct {
		args []string
		want string
	}{
		{[]string{"item", "pause", "--config", cfg, "7"}, "name the project"},
		{[]string{"item", "pause", "--config", cfg, "99"}, "isn't on the board"},
		{[]string{"project", "pause", "--config", cfg, "nope"}, "isn't registered"},
	}
	for _, c := range refused {
		code, _, errs := cli(t, c.args...)
		if code != 1 || !strings.Contains(errs, c.want) {
			t.Errorf("%v: exit %d, stderr %q, want 1 and %q", c.args, code, errs, c.want)
		}
	}
	if got := wf.take(); len(got) != 0 {
		t.Fatalf("refused commands sent %+v", got)
	}
}

// FR-11 AC1: the board shows each claimed item's state, round, blocking
// count, host, time in state and next action, and the waits with no label:
// a paused deployment or project and the daily limit.
func TestFR11AC1_BoardShowsColumnsAndUnlabelledWaits(t *testing.T) {
	cfg, wf, db := testServer(t, "p", "q", "r")
	wf.deployment = &workflows.DeploymentState{Paused: true}
	wf.projects["p"] = workflows.ProjectView{State: workflows.ProjectState{Paused: true}}
	wf.projects["q"] = workflows.ProjectView{DailyLimitReached: true}
	onBoard(t, db, "p", "42", testNow.Add(-90*time.Minute), projection.ItemRow{State: core.StateInReview, Round: 2, RoundsAllowed: 3, Blocking: 1, Host: "mac", NextAction: "review"})
	onBoard(t, db, "q", "7", testNow.Add(-time.Minute), projection.ItemRow{State: core.StateWaitingLimit, RoundsAllowed: 3, NextAction: "implement", WaitReason: "daily_limit"})

	code, out, errs := cli(t, "board", "--config", cfg)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	for _, want := range []string{
		"Deployment: paused",
		"Project p: paused",
		"ITEM", "STATE", "ROUND", "BLOCKING", "HOST", "IN STATE", "NEXT ACTION",
		"42", "in_review", "2/3", "mac", "1h30m0s", "review",
		"Project q: daily limit reached",
		"7", "waiting_limit", "daily_limit",
		"Project r: state unknown: project r isn't running",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("board lacks %q:\n%s", want, out)
		}
	}

	code, out, _ = cli(t, "board", "--config", cfg, "q")
	if code != 0 || strings.Contains(out, "Project p") || !strings.Contains(out, "Project q") {
		t.Fatalf("board q: exit %d\n%s", code, out)
	}
}

// FR-11 AC2: agent-loop log shows an item's runs and decisions in time
// order.
func TestFR11AC2_LogShowsRunsAndDecisions(t *testing.T) {
	cfg, _, db := testServer(t, "p")
	ctx := context.Background()
	onBoard(t, db, "p", "42", testNow, projection.ItemRow{State: core.StateImplementing})
	if err := db.StartRun(ctx, projection.StageRun{RunID: "run-1", Project: "p", Item: "42", Stage: contract.StageImplement, Attempt: 1, Host: "mac", Started: testNow.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishRun(ctx, "run-1", testNow.Add(time.Minute), "done", "/logs/run-1.log"); err != nil {
		t.Fatal(err)
	}
	code, out, errs := cli(t, "log", "--config", cfg, "42")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], "decision  started") ||
		!strings.Contains(lines[1], "start     implement attempt 1 on mac (run run-1)") ||
		!strings.Contains(lines[2], "end       implement attempt 1: done, log /logs/run-1.log") {
		t.Fatalf("log:\n%s", out)
	}
	if code, _, errs := cli(t, "log", "--config", cfg, "9"); code != 1 || !strings.Contains(errs, "has no log") {
		t.Fatalf("unknown item: exit %d, %q", code, errs)
	}
}

func TestServerNotRunning(t *testing.T) {
	dir, err := os.MkdirTemp("", "al")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	cfg := filepath.Join(dir, "deployment.toml")
	os.WriteFile(cfg, []byte("[api]\nsocket = \""+filepath.Join(dir, "api.sock")+"\"\n"), 0o600)
	if code, _, errs := cli(t, "pause", "--config", cfg); code != 1 || errs == "" {
		t.Fatalf("exit %d, %q", code, errs)
	}
}
