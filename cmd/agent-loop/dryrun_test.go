package main

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/mayusuk/agent-loop/internal/adapters/fake"
	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/ports"
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
			Items: []claimedItem{{Key: "77", State: "in_review", Next: "merge", Rule: "TECHNICAL-SPEC §8.2 rule 6"}}}, nil
	}
	code, out, _ := runCLI("run", "--dry-run", "--config", tp.cfg, "widgets")
	if code != 0 {
		t.Fatalf("code %d\n%s", code, out)
	}
	for _, want := range []string{
		"Limits: WIP 1 of 2, daily limit not reached, project paused, deployment running.",
		"  77  in_review, next: merge (TECHNICAL-SPEC §8.2 rule 6)",
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
