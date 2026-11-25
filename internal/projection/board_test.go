package projection

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/core"
	"github.com/mayusuk/agent-loop/internal/ports"
)

func board(t *testing.T, p *DB, now time.Time) []BoardRow {
	t.Helper()
	b, err := p.Board(context.Background(), "", now)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func record(t *testing.T, p *DB, d Decision) {
	t.Helper()
	if err := p.RecordDecision(context.Background(), d); err != nil {
		t.Fatal(err)
	}
}

func itemDecision(project, item string, claim, seq int, at time.Time, row ItemRow) Decision {
	row.Project, row.Item = project, item
	return Decision{
		WorkflowID: "item/" + project + "/" + item, Claim: claim,
		Record: core.DecisionRecord{Seq: seq, At: at, Event: "head_moved", To: row.State},
		Item:   row,
	}
}

func TestFR11AC1_BoardShowsEachClaimedItem(t *testing.T) {
	p := openTest(t)
	// Item 42 enters in_review, then a second decision keeps it there with
	// new counts: the time in state runs from the first.
	record(t, p, itemDecision("p", "42", 1, 1, t0, ItemRow{State: core.StateInReview, Round: 1, RoundsAllowed: 3, Host: "mac", NextAction: "review"}))
	record(t, p, itemDecision("p", "42", 1, 2, t0.Add(10*time.Minute), ItemRow{State: core.StateInReview, Round: 2, RoundsAllowed: 3, Blocking: 2, Host: "mac", NextAction: "review"}))
	// Item 7 waits on the daily limit.
	record(t, p, itemDecision("p", "7", 1, 1, t0.Add(20*time.Minute), ItemRow{State: core.StateWaitingLimit, RoundsAllowed: 3, NextAction: "implement", WaitReason: "daily_limit"}))
	// Item 9 merged and item 10 closed: off the board.
	record(t, p, itemDecision("p", "9", 1, 1, t0, ItemRow{State: core.StateMerged}))
	record(t, p, itemDecision("p", "10", 1, 1, t0, ItemRow{State: core.StateClosed}))
	// Another project.
	record(t, p, itemDecision("q", "1", 1, 1, t0, ItemRow{State: core.StateEscalated, NextAction: "owner", WaitReason: "round_limit"}))

	now := t0.Add(time.Hour)
	got, err := p.Board(context.Background(), "p", now)
	if err != nil {
		t.Fatal(err)
	}
	want := []BoardRow{
		{ItemRow: ItemRow{Project: "p", Item: "42", State: core.StateInReview, Round: 2, RoundsAllowed: 3, Blocking: 2, Host: "mac", NextAction: "review", EnteredAt: t0}, TimeInState: time.Hour},
		{ItemRow: ItemRow{Project: "p", Item: "7", State: core.StateWaitingLimit, RoundsAllowed: 3, NextAction: "implement", WaitReason: "daily_limit", EnteredAt: t0.Add(20 * time.Minute)}, TimeInState: 40 * time.Minute},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("board:\n got %+v\nwant %+v", got, want)
	}

	all := board(t, p, now)
	if len(all) != 3 || all[0].Project != "p" || all[2].Project != "q" {
		t.Fatalf("all projects: %+v", all)
	}
}

func TestFR11AC1_StateChangeResetsTimeInState(t *testing.T) {
	p := openTest(t)
	record(t, p, itemDecision("p", "42", 1, 1, t0, ItemRow{State: core.StateInReview}))
	record(t, p, itemDecision("p", "42", 1, 2, t0.Add(time.Minute), ItemRow{State: core.StateAddressing}))
	b := board(t, p, t0.Add(3*time.Minute))
	if b[0].State != core.StateAddressing || b[0].TimeInState != 2*time.Minute {
		t.Fatalf("got %s for %v, want addressing for 2m", b[0].State, b[0].TimeInState)
	}
	// A board time before the entry never gives a negative duration.
	if b := board(t, p, t0); b[0].TimeInState != 0 {
		t.Fatalf("time in state %v, want 0", b[0].TimeInState)
	}
	// A merge takes the item off the board.
	record(t, p, itemDecision("p", "42", 1, 3, t0.Add(4*time.Minute), ItemRow{State: core.StateMerged}))
	if b := board(t, p, t0.Add(5*time.Minute)); len(b) != 0 {
		t.Fatalf("merged item still on the board: %+v", b)
	}
}

func TestFR11AC2_ItemLogRecordsStartsResultsRetriesEscalationsAndMerge(t *testing.T) {
	p := openTest(t)
	ctx := context.Background()
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	run := func(id string, attempt, start, end int, result string) {
		t.Helper()
		if err := p.StartRun(ctx, StageRun{RunID: id, Project: "p", Item: "42", Stage: contract.StageImplement, Attempt: attempt, Host: "mac", Started: at(start)}); err != nil {
			t.Fatal(err)
		}
		if err := p.FinishRun(ctx, id, at(end), result, "/logs/"+id+".log"); err != nil {
			t.Fatal(err)
		}
	}
	dec := func(seq, m int, event string, to core.State, actions ...string) {
		t.Helper()
		d := itemDecision("p", "42", 1, seq, at(m), ItemRow{State: to})
		d.Record.Event, d.Record.Actions = event, actions
		if event == "stage_result" {
			d.Record.Result = &core.ResultRef{Stage: contract.StageImplement, RunID: "r" + event, Outcome: contract.OutcomeFailed}
		}
		record(t, p, d)
	}
	run("r1", 1, 0, 5, "failed")
	dec(1, 5, "stage_result", core.StateImplementing, "run_stage") // retry
	run("r2", 2, 5, 9, "timed_out")
	dec(2, 9, "stage_timed_out", core.StateEscalated, "post_status", "notify") // escalation
	dec(3, 20, "merge_result", core.StateMerged, "notify")                     // merge

	log, err := p.ItemLog(ctx, "p", "42")
	if err != nil {
		t.Fatal(err)
	}
	type line struct {
		at   time.Time
		what string
	}
	var got []line
	for _, e := range log {
		switch {
		case e.Decision != nil:
			got = append(got, line{e.At, "decision " + e.Decision.Record.Event + " -> " + string(e.Decision.Record.To)})
		case e.RunEnd:
			got = append(got, line{e.At, "end " + e.Run.RunID + " " + e.Run.Result})
		default:
			got = append(got, line{e.At, "start " + e.Run.RunID})
		}
	}
	want := []line{
		{at(0), "start r1"},
		{at(5), "start r2"},
		{at(5), "decision stage_result -> implementing"},
		{at(5), "end r1 failed"},
		{at(9), "decision stage_timed_out -> escalated"},
		{at(9), "end r2 timed_out"},
		{at(20), "decision merge_result -> merged"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("log:\n got %v\nwant %v", got, want)
	}
	runs, _ := p.Runs(ctx, "p", "42")
	if runs[1].Attempt != 2 || runs[1].LogPath != "/logs/r2.log" || runs[1].Host != "mac" {
		t.Fatalf("run 2: %+v", runs[1])
	}
}

func TestStageRuns_RetryAndErrors(t *testing.T) {
	p := openTest(t)
	ctx := context.Background()
	r := StageRun{RunID: "r1", Project: "p", Item: "42", Stage: contract.StageReview, Kind: "round", Attempt: 1, Started: t0, LogPath: "/a"}
	for range 2 {
		if err := p.StartRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	runs, _ := p.Runs(ctx, "p", "42")
	if len(runs) != 1 || !runs[0].Ended.IsZero() || runs[0].Result != "" || runs[0].Kind != "round" {
		t.Fatalf("running run: %+v", runs)
	}
	// An empty log path keeps the one from the start.
	if err := p.FinishRun(ctx, "r1", t0.Add(time.Minute), "done", ""); err != nil {
		t.Fatal(err)
	}
	if runs, _ := p.Runs(ctx, "p", "42"); runs[0].LogPath != "/a" || runs[0].Result != "done" {
		t.Fatalf("finished run: %+v", runs[0])
	}
	if err := p.FinishRun(ctx, "nope", t0, "done", ""); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("unknown run: %v", err)
	}
	if err := p.FinishRun(ctx, "r1", time.Time{}, "done", ""); err == nil {
		t.Fatal("zero end: want an error")
	}
	if err := p.FinishRun(ctx, "r1", t0, "", ""); err == nil {
		t.Fatal("no result: want an error")
	}
	for name, bad := range map[string]StageRun{
		"no id":    {Project: "p", Item: "1", Stage: "review", Started: t0},
		"no stage": {RunID: "x", Project: "p", Item: "1", Started: t0},
		"no start": {RunID: "x", Project: "p", Item: "1", Stage: "review"},
	} {
		if err := p.StartRun(ctx, bad); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestFR16AC3_DailyCountCountsHandlerRuns(t *testing.T) {
	p := openTest(t)
	ctx := context.Background()
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no time zone data:", err)
	}
	day := time.Date(2026, 11, 16, 12, 0, 0, 0, ny)
	runs := []struct {
		id      string
		project string
		stage   contract.Stage
		kind    string
		at      time.Time
	}{
		{"before", "p", contract.StageImplement, "", time.Date(2026, 11, 15, 23, 59, 59, 0, ny)},
		{"midnight", "p", contract.StageImplement, "", time.Date(2026, 11, 16, 0, 0, 0, 0, ny)},
		{"review", "p", contract.StageReview, "", time.Date(2026, 11, 16, 8, 0, 0, 0, ny)},
		{"address", "p", contract.StageAddress, "", time.Date(2026, 11, 16, 23, 59, 59, 0, ny)},
		{"merge", "p", core.StageMerge, "", time.Date(2026, 11, 16, 9, 0, 0, 0, ny)},
		// A mint run and a conflicted base update (an address run, FR-5
		// AC6) are handler runs and count.
		{"mint", "p", contract.StageMint, "", time.Date(2026, 11, 16, 12, 0, 0, 0, ny)},
		{"base update", "p", contract.StageAddress, string(contract.ModeBaseUpdate), time.Date(2026, 11, 16, 13, 0, 0, 0, ny)},
		{"other project", "q", contract.StageReview, "", time.Date(2026, 11, 16, 9, 0, 0, 0, ny)},
		{"next day", "p", contract.StageReview, "", time.Date(2026, 11, 17, 0, 0, 0, 0, ny)},
	}
	for _, r := range runs {
		if err := p.StartRun(ctx, StageRun{RunID: r.id, Project: r.project, Item: "1", Stage: r.stage, Kind: r.kind, Started: r.at.UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	// midnight, review, address, mint and base update.
	if n, err := p.DailyCount(ctx, "p", day, ny); err != nil || n != 5 {
		t.Fatalf("count %d, %v; want 5", n, err)
	}
	// In UTC, 16 Nov holds "before" (04:59:59Z), "midnight", "review",
	// "mint" and "base update", and "address" (04:59:59Z on 17 Nov) moves
	// to the next day.
	if n, _ := p.DailyCount(ctx, "p", time.Date(2026, 11, 16, 3, 0, 0, 0, time.UTC), time.UTC); n != 5 {
		t.Fatalf("UTC count %d, want 5", n)
	}
	if n, _ := p.DailyCount(ctx, "p", time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC), time.UTC); n != 2 {
		t.Fatalf("UTC 17 Nov count %d, want 2 (address, next day)", n)
	}
}

func TestFR16AC4_DayBoundaryInProfileZone(t *testing.T) {
	p := openTest(t)
	ctx := context.Background()
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no time zone data:", err)
	}
	// 2026-11-01 is the end of daylight saving time in New York: 25 hours.
	late := time.Date(2026, 11, 1, 23, 30, 0, 0, ny)
	if err := p.StartRun(ctx, StageRun{RunID: "late", Project: "p", Item: "1", Stage: contract.StageReview, Started: late}); err != nil {
		t.Fatal(err)
	}
	if n, _ := p.DailyCount(ctx, "p", late, ny); n != 1 {
		t.Fatalf("count %d, want 1", n)
	}
	if n, _ := p.DailyCount(ctx, "p", late.AddDate(0, 0, 1), ny); n != 0 {
		t.Fatalf("next day count %d, want 0", n)
	}
	if n, _ := p.DailyCount(ctx, "p", late, nil); n > 1 {
		t.Fatalf("local count %d", n)
	}
}

func TestHosts_UpsertAndList(t *testing.T) {
	p := openTest(t)
	ctx := context.Background()
	if err := p.UpsertHost(ctx, Host{Name: "mac", Capabilities: []string{"agent"}, LastSeen: t0, LastCheck: "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := p.UpsertHost(ctx, Host{Name: "mac", Capabilities: []string{"agent", "docker"}, LastSeen: t0.Add(time.Minute), LastCheck: "docker missing"}); err != nil {
		t.Fatal(err)
	}
	if err := p.UpsertHost(ctx, Host{Name: "box"}); err != nil {
		t.Fatal(err)
	}
	if err := p.UpsertHost(ctx, Host{}); err == nil {
		t.Fatal("no name: want an error")
	}
	got, err := p.Hosts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []Host{
		{Name: "box"},
		{Name: "mac", Capabilities: []string{"agent", "docker"}, LastSeen: t0.Add(time.Minute), LastCheck: "docker missing"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hosts:\n got %+v\nwant %+v", got, want)
	}
}

func TestConcurrentWriters(t *testing.T) {
	p := openTest(t)
	ctx := context.Background()
	errs := make(chan error, 20)
	for i := range 20 {
		go func() {
			d := itemDecision("p", "x", 1, i+1, t0.Add(time.Duration(i)*time.Second), ItemRow{State: core.StateInReview})
			errs <- p.RecordDecision(ctx, d)
		}()
	}
	for range 20 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if ds, _ := p.Decisions(ctx, "p", "x"); len(ds) != 20 {
		t.Fatalf("got %d decisions, want 20", len(ds))
	}
}

func TestFR11AC1_ReleasedClaimLeavesTheBoard(t *testing.T) {
	p := openTest(t)
	ctx := context.Background()
	// Item 42 is aborted, and the owner removes aborted: the claim is
	// released and the item leaves the board.
	record(t, p, itemDecision("p", "42", 1, 1, t0, ItemRow{State: core.StateAborted, NextAction: "owner"}))
	// Item 7 stays claimed.
	record(t, p, itemDecision("p", "7", 1, 1, t0, ItemRow{State: core.StateInReview, NextAction: "review"}))
	if err := p.RecordRelease(ctx, Release{Project: "p", Item: "42", Claim: 1, At: t0.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	// A retried release changes nothing.
	if err := p.RecordRelease(ctx, Release{Project: "p", Item: "42", Claim: 1, At: t0.Add(2 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if b := board(t, p, t0.Add(time.Hour)); len(b) != 1 || b[0].Item != "7" {
		t.Fatalf("board after release: %+v, want only item 7", b)
	}
	// A late decision of the released claim doesn't bring it back.
	record(t, p, itemDecision("p", "42", 1, 2, t0.Add(3*time.Minute), ItemRow{State: core.StateAborted}))
	if b := board(t, p, t0.Add(time.Hour)); len(b) != 1 {
		t.Fatalf("late decision of a released claim is on the board: %+v", b)
	}
	// The item is picked again: the new claim is on the board.
	record(t, p, itemDecision("p", "42", 2, 1, t0.Add(10*time.Minute), ItemRow{State: core.StateImplementing, NextAction: "implement"}))
	b := board(t, p, t0.Add(time.Hour))
	if len(b) != 2 || b[0].Item != "7" || b[1].Item != "42" || b[1].State != core.StateImplementing {
		t.Fatalf("board after the new claim: %+v", b)
	}
	// A stale release of the old claim leaves the new claim on the board.
	if err := p.RecordRelease(ctx, Release{Project: "p", Item: "42", Claim: 1, At: t0.Add(11 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if b := board(t, p, t0.Add(time.Hour)); len(b) != 2 {
		t.Fatalf("stale release removed the new claim: %+v", b)
	}
}

func TestRecordRelease_UnknownItemAndInvalidInput(t *testing.T) {
	p := openTest(t)
	ctx := context.Background()
	// An item with no row (no decision was recorded) has nothing to remove.
	if err := p.RecordRelease(ctx, Release{Project: "p", Item: "1", Claim: 1, At: t0}); err != nil {
		t.Fatalf("unknown item: %v", err)
	}
	for _, r := range []Release{
		{Item: "1", Claim: 1, At: t0},
		{Project: "p", Claim: 1, At: t0},
		{Project: "p", Item: "1", At: t0},
		{Project: "p", Item: "1", Claim: 1},
	} {
		if err := p.RecordRelease(ctx, r); err == nil {
			t.Errorf("%+v: want an error", r)
		}
	}
}

func TestFR11AC1_AbortedPausedAndBlockedItemsStayOnTheBoard(t *testing.T) {
	p := openTest(t)
	record(t, p, itemDecision("p", "3", 1, 1, t0.Add(2*time.Minute), ItemRow{State: core.StateBlocked, RoundsAllowed: 3, NextAction: "wait", WaitReason: "dependency #5"}))
	record(t, p, itemDecision("p", "1", 1, 1, t0, ItemRow{State: core.StateAborted, RoundsAllowed: 3, NextAction: "owner", WaitReason: "aborted"}))
	record(t, p, itemDecision("p", "2", 1, 1, t0.Add(time.Minute), ItemRow{State: core.StatePaused, RoundsAllowed: 3, NextAction: "owner", WaitReason: "paused"}))
	now := t0.Add(time.Hour)
	got, err := p.Board(context.Background(), "p", now)
	if err != nil {
		t.Fatal(err)
	}
	want := []BoardRow{
		{ItemRow: ItemRow{Project: "p", Item: "1", State: core.StateAborted, RoundsAllowed: 3, NextAction: "owner", WaitReason: "aborted", EnteredAt: t0}, TimeInState: time.Hour},
		{ItemRow: ItemRow{Project: "p", Item: "2", State: core.StatePaused, RoundsAllowed: 3, NextAction: "owner", WaitReason: "paused", EnteredAt: t0.Add(time.Minute)}, TimeInState: 59 * time.Minute},
		{ItemRow: ItemRow{Project: "p", Item: "3", State: core.StateBlocked, RoundsAllowed: 3, NextAction: "wait", WaitReason: "dependency #5", EnteredAt: t0.Add(2 * time.Minute)}, TimeInState: 58 * time.Minute},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("board:\n got %+v\nwant %+v", got, want)
	}
}

// An item command finds the workflow of the item's claim on the board
// (work plan #22 D3): merged, closed and released items aren't found.
func TestFR10AC3_FindOnBoard(t *testing.T) {
	ctx := context.Background()
	p := openTest(t)
	record(t, p, itemDecision("p", "42", 2, 1, t0, ItemRow{State: core.StateInReview}))
	record(t, p, itemDecision("q", "42", 1, 1, t0, ItemRow{State: core.StateImplementing}))
	record(t, p, itemDecision("p", "9", 1, 1, t0, ItemRow{State: core.StateMerged}))
	record(t, p, itemDecision("p", "5", 1, 1, t0, ItemRow{State: core.StateImplementing}))
	if err := p.RecordRelease(ctx, Release{Project: "p", Item: "5", Claim: 1, At: t0.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	find := func(project, item string) []BoardItem {
		t.Helper()
		got, err := p.FindOnBoard(ctx, project, item)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got, want := find("", "42"), []BoardItem{{"p", "42", "item/p/42"}, {"q", "42", "item/q/42"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("every project: %v, want %v", got, want)
	}
	if got, want := find("q", "42"), []BoardItem{{"q", "42", "item/q/42"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("project q: %v, want %v", got, want)
	}
	for _, item := range []string{"9", "5", "nope"} {
		if got := find("", item); len(got) != 0 {
			t.Fatalf("item %s: %v, want none", item, got)
		}
	}
}

// TestFR11AC1_BoardShowsTheRunningStagesHost: while a stage runs, the
// board's host is the host that runs it; once it ends, no host is shown
// (work plan #105 D3).
func TestFR11AC1_BoardShowsTheRunningStagesHost(t *testing.T) {
	p := openTest(t)
	ctx := context.Background()
	record(t, p, itemDecision("p", "42", 1, 1, t0, ItemRow{State: core.StateImplementing, RoundsAllowed: 3, NextAction: "run_stage"}))
	record(t, p, itemDecision("p", "7", 1, 1, t0, ItemRow{State: core.StateImplementing, RoundsAllowed: 3, NextAction: "run_stage"}))
	host := func(item string) string {
		t.Helper()
		for _, r := range board(t, p, t0.Add(time.Hour)) {
			if r.Item == item {
				return r.Host
			}
		}
		t.Fatalf("item %s isn't on the board", item)
		return ""
	}

	// An earlier run on another host ended; the current one runs on mac.
	old := StageRun{RunID: "r0", Project: "p", Item: "42", Stage: contract.StageImplement, Attempt: 1, Started: t0}
	run := StageRun{RunID: "r1", Project: "p", Item: "42", Stage: contract.StageImplement, Attempt: 2, Started: t0.Add(time.Minute)}
	for _, r := range []StageRun{old, run} {
		if err := p.StartRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.SetRunHost(ctx, "r0", "linux"); err != nil {
		t.Fatal(err)
	}
	if err := p.FinishRun(ctx, "r0", t0.Add(time.Minute), "worker_lost", ""); err != nil {
		t.Fatal(err)
	}
	if got := host("42"); got != "" {
		t.Errorf("run started, no host yet: host %q, want none", got)
	}
	if err := p.SetRunHost(ctx, "r1", "mac"); err != nil {
		t.Fatal(err)
	}
	if got := host("42"); got != "mac" {
		t.Errorf("running on mac: host %q", got)
	}
	if got := host("7"); got != "" {
		t.Errorf("another item's run leaked: host %q", got)
	}
	if runs, _ := p.Runs(ctx, "p", "42"); runs[1].Host != "mac" || runs[0].Host != "linux" {
		t.Errorf("stage runs: %+v", runs)
	}
	if err := p.FinishRun(ctx, "r1", t0.Add(2*time.Minute), "done", ""); err != nil {
		t.Fatal(err)
	}
	if got := host("42"); got != "" {
		t.Errorf("run ended: host %q, want none", got)
	}

	if err := p.SetRunHost(ctx, "nope", "mac"); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("unknown run: %v", err)
	}
	if err := p.SetRunHost(ctx, "r1", ""); err == nil {
		t.Error("no host: want an error")
	}
}
