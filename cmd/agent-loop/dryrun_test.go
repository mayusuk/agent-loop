package main

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mayusuk/agent-loop/internal/adapters/fake"
	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/projection"
	"github.com/mayusuk/agent-loop/internal/workflows"
)

// seeded is the fake's items for the dry-run tests, by role.
type seeded struct {
	feature, bug, assigned, waiting, open, orphan, untyped, extra, gone ports.ItemRef
}

func seedCandidates(t *testing.T, f *fake.Fake) seeded {
	t.Helper()
	ctx := context.Background()
	must := func(r ports.ItemRef, err error) ports.ItemRef {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	ok := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var s seeded
	s.feature = must(f.NewItem(fake.Owner, "t", "b", ports.MarkerReady, ports.MarkerTypeFeature))
	s.bug = must(f.NewItem(fake.Owner, "t", "b", ports.MarkerReady, ports.MarkerTypeBug))
	s.assigned = must(f.NewItem(fake.Owner, "t", "b", ports.MarkerReady, ports.MarkerTypeFeature))
	human := fake.Human
	ok(f.Assign(fake.Owner, s.assigned, &human))
	s.open = must(f.NewItem(fake.Owner, "t", "b", ports.MarkerTypeFeature))
	s.waiting = must(f.NewItem(fake.Owner, "t", "b", ports.MarkerReady, ports.MarkerTypeFeature))
	ok(f.Tracker().AddDependency(ctx, s.waiting, s.open, "dep1"))
	s.gone = must(f.NewItem(fake.Owner, "t", "b", ports.MarkerTypeFeature))
	ok(f.CloseItem(fake.Owner, s.gone, ports.CloseNotDone))
	s.orphan = must(f.NewItem(fake.Owner, "t", "b", ports.MarkerReady, ports.MarkerTypeFeature))
	ok(f.Tracker().AddDependency(ctx, s.orphan, s.gone, "dep2"))
	s.untyped = must(f.NewItem(fake.Owner, "t", "b", ports.MarkerReady))
	s.extra = must(f.NewItem(fake.Owner, "t", "b", ports.MarkerReady, ports.MarkerTypeFeature))
	return s
}

// fakeState is everything a write could change in the fake's tracker and
// host.
func fakeState(t *testing.T, f *fake.Fake, refs ...ports.ItemRef) []any {
	t.Helper()
	ctx := context.Background()
	te, _, err := f.Tracker().Changes(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	he, _, err := f.Host().Changes(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	out := []any{len(te), len(he)}
	for _, r := range refs {
		it, err := f.Tracker().Item(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		recs, err := f.Tracker().FindRecords(ctx, r, ports.RecordStatus)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, it, len(recs))
	}
	return out
}

// TestFR1AC6_DryRunMakesNoWriteAndStartsNoHandler: against the fake, a dry
// run lists each candidate in pick order with "would pick" or its FR-2 AC1
// exclusion, lists the writes it would make, makes none of them, and starts
// no handler.
func TestFR1AC6_DryRunMakesNoWriteAndStartsNoHandler(t *testing.T) {
	tp := newTestProject(t, cleanProfile)
	s := seedCandidates(t, tp.f)
	all := []ports.ItemRef{s.feature, s.bug, s.assigned, s.waiting, s.open, s.orphan, s.untyped, s.extra, s.gone}
	before := fakeState(t, tp.f, all...)

	code, out, errOut := runCLI("run", "--dry-run", "--config", tp.cfg, "widgets")
	if code != 0 || errOut != "" {
		t.Fatalf("code %d\nstdout %s\nstderr %s", code, out, errOut)
	}
	if after := fakeState(t, tp.f, all...); !reflect.DeepEqual(before, after) {
		t.Errorf("the dry run changed the fake:\nbefore %v\nafter  %v", before, after)
	}
	if tp.handlerStarted() {
		t.Error("the dry run started a handler")
	}

	verdicts := []string{
		string(s.bug) + "  would pick",
		string(s.feature) + "  would pick",
		string(s.assigned) + "  excluded: human_assignee (has human assignee)",
		string(s.waiting) + "  excluded: dependency_open (dependency " + string(s.open) + " not done)",
		string(s.orphan) + "  excluded: dependency_not_done (dependency " + string(s.gone) + " closed without being done; the loop would add needs-owner)",
		string(s.extra) + "  excluded: wip_full (the WIP limit is full)",
		string(s.untyped) + "  excluded: type_not_configured (not of a configured type)",
	}
	at := -1
	for _, v := range verdicts {
		i := strings.Index(out, "  "+v+"\n")
		if i < 0 {
			t.Errorf("missing %q in\n%s", v, out)
			continue
		}
		if i < at {
			t.Errorf("%q is out of pick order", v)
		}
		at = i
	}
	for _, want := range []string{
		"Candidates (open items with the ready marker), in pick order:\n",
		"the picks below assume none of them applies (WIP 0 of 2)",
		"workflow item/widgets/" + string(s.bug) + "\n",
		"workflow item/widgets/" + string(s.feature) + "\n",
		"tracker.Claim " + string(s.bug) + ": add in_progress, remove ready (key item/widgets/" + string(s.bug) + "#1:claim)",
		"tracker.Claim " + string(s.feature) + ": add in_progress, remove ready (key item/widgets/" + string(s.feature) + "#1:claim)",
		"tracker.SetMarker " + string(s.orphan) + ": set needs_owner (key item/widgets/" + string(s.orphan) + "#0:dependency_not_done:1:marker)",
		"tracker.Comment " + string(s.orphan) + ": status record (key item/widgets/" + string(s.orphan) + "#0:dependency_not_done:1)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if n := strings.Count(out, "  tracker."); n != 4 {
		t.Errorf("%d writes listed, want 4:\n%s", n, out)
	}
}

// TestFR1AC6_DryRunShowsServerState: with the server's view, the dry run
// shows the limits and each claimed item's next action, and a paused
// project picks nothing.
func TestFR1AC6_DryRunShowsServerState(t *testing.T) {
	tp := newTestProject(t, cleanProfile)
	s := seedCandidates(t, tp.f)
	old := serverState
	t.Cleanup(func() { serverState = old })
	serverState = func(context.Context, *localProject) (*serverView, error) {
		return &serverView{Project: core.ProjectView{Paused: true, Claimed: 1},
			Items: []claimedItem{{Key: "77", State: "in_review", Next: "merge"}}}, nil
	}
	code, out, _ := runCLI("run", "--dry-run", "--config", tp.cfg, "widgets")
	if code != 0 {
		t.Fatalf("code %d\n%s", code, out)
	}
	for _, want := range []string{
		"Limits: WIP 1 of 2, daily limit not reached (0 runs today), project paused, deployment running.",
		"  77  in_review, next: merge\n",
		"  " + string(s.bug) + "  excluded: project_paused (the project is paused)",
		"Tracker writes it would have made:\n  tracker.SetMarker " + string(s.orphan),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "would pick") || strings.Contains(out, "tracker.Claim") {
		t.Errorf("a paused project picked:\n%s", out)
	}
}

func TestFR1AC6_RunNeedsDryRun(t *testing.T) {
	tp := newTestProject(t, cleanProfile)
	if code, _, errOut := runCLI("run", "--config", tp.cfg, "widgets"); code != 2 || !strings.Contains(errOut, "only --dry-run") {
		t.Errorf("code %d, stderr %s", code, errOut)
	}
}

// TestFR1AC6_DryRunReadsBoardWhenServerRuns: with the server running, the
// dry run reads the WIP in use, the pauses and the daily limit from the
// board API, lists each claimed item, and a reached daily limit picks
// nothing (work plan #22 D9).
func TestFR1AC6_DryRunReadsBoardWhenServerRuns(t *testing.T) {
	tp := newTestProject(t, cleanProfile)
	s := seedCandidates(t, tp.f)
	cfg, wf, db := testServer(t, "widgets")
	tp.useServer(t, filepath.Join(filepath.Dir(cfg), "cfg", "api.sock"))
	wf.deployment = &workflows.DeploymentState{}
	wf.projects["widgets"] = workflows.ProjectView{DailyLimitReached: true, State: workflows.ProjectState{
		Items: map[string]workflows.RunningItem{"77": {WorkflowID: "item/widgets/77"}},
		Daily: core.DailyCount{Day: "2026-11-17", Runs: 20},
	}}
	onBoard(t, db, "widgets", "77", testNow.Add(-time.Minute), projection.ItemRow{State: core.StateWaitingLimit, RoundsAllowed: 3, NextAction: "implement", WaitReason: "daily_limit"})

	code, out, errOut := runCLI("run", "--dry-run", "--config", tp.cfg, "widgets")
	if code != 0 || errOut != "" {
		t.Fatalf("code %d\nstdout %s\nstderr %s", code, out, errOut)
	}
	for _, want := range []string{
		"Limits: WIP 1 of 2, daily limit reached (20 runs today), project running, deployment running.",
		"Claimed items:\n  77  waiting_limit, next: implement, waiting on daily_limit\n",
		"  " + string(s.bug) + "  excluded: daily_limit (the daily limit is reached)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "would pick") || strings.Contains(out, "tracker.Claim") || strings.Contains(out, "isn't running") {
		t.Errorf("the board's limits weren't used:\n%s", out)
	}

	// A paused deployment, read from the board, excludes every candidate.
	wf.deployment = &workflows.DeploymentState{Paused: true}
	wf.projects["widgets"] = workflows.ProjectView{}
	_, out, _ = runCLI("run", "--dry-run", "--config", tp.cfg, "widgets")
	for _, want := range []string{
		"Limits: WIP 0 of 2, daily limit not reached (0 runs today), project running, deployment paused.",
		"Claimed items:\n  77  waiting_limit",
		"  " + string(s.bug) + "  excluded: deployment_paused (the deployment is paused)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}

// TestFR1AC6_DryRunWithoutServer: when nothing listens on the API socket,
// or the server's project workflow isn't answering, the dry run says the
// server isn't running and picks as if no limit applied (work plan #22 D9,
// #23 D12).
func TestFR1AC6_DryRunWithoutServer(t *testing.T) {
	tp := newTestProject(t, cleanProfile)
	s := seedCandidates(t, tp.f)
	assume := "the picks below assume none of them applies (WIP 0 of 2)"

	code, out, errOut := runCLI("run", "--dry-run", "--config", tp.cfg, "widgets")
	if code != 0 || errOut != "" {
		t.Fatalf("code %d\nstdout %s\nstderr %s", code, out, errOut)
	}
	for _, want := range []string{"Server: the server isn't running", assume, "  " + string(s.bug) + "  would pick"} {
		if !strings.Contains(out, want) {
			t.Errorf("no server: missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "Claimed items:") {
		t.Errorf("no server, yet claimed items were listed:\n%s", out)
	}

	// The server runs, but the project's workflow doesn't answer.
	cfg, wf, _ := testServer(t, "widgets")
	wf.deployment = &workflows.DeploymentState{}
	tp.useServer(t, filepath.Join(filepath.Dir(cfg), "cfg", "api.sock"))
	_, out, _ = runCLI("run", "--dry-run", "--config", tp.cfg, "widgets")
	for _, want := range []string{"Server: the server isn't running: project widgets isn't running", assume, "  " + string(s.bug) + "  would pick"} {
		if !strings.Contains(out, want) {
			t.Errorf("project workflow down: missing %q in\n%s", want, out)
		}
	}

	// The project workflow answers, but the deployment workflow doesn't, so
	// the deployment pause isn't known.
	wf.projects["widgets"] = workflows.ProjectView{}
	wf.deployment = nil
	_, out, _ = runCLI("run", "--dry-run", "--config", tp.cfg, "widgets")
	for _, want := range []string{"Server: the server isn't running: the deployment workflow isn't answering", assume} {
		if !strings.Contains(out, want) {
			t.Errorf("deployment workflow down: missing %q in\n%s", want, out)
		}
	}

	// A server for another deployment doesn't know the project.
	cfg, _, _ = testServer(t, "gadgets")
	tp.useServer(t, filepath.Join(filepath.Dir(cfg), "cfg", "api.sock"))
	_, out, _ = runCLI("run", "--dry-run", "--config", tp.cfg, "widgets")
	for _, want := range []string{`project "widgets" isn't registered`, assume} {
		if !strings.Contains(out, want) {
			t.Errorf("unknown project: missing %q in\n%s", want, out)
		}
	}
}
